// An in-memory, S3-compatible object store for the provider harness (TXE-7326).
//
// Why. The hybrid log store offloads log payloads to S3 or GCS and keeps only a summary row in the
// database. Its bugs live in the order of those uploads: an upload still in flight when the row is
// read or updated, or an upload that fails. No harness gateway had object storage, and no offline
// fixture could hold or fail a PUT at a chosen step, so none of that was reachable from a harness row
// (TXE-7318 shipped under an explicit exemption for that reason).
//
// What. provider-error-fixture.mjs mounts this on its own port. The gateway config
// tests/e2e/api/provider_config/hybrid-logstore-fixture.config.json points logs_store.object_storage at
// it, path-style, with static throwaway credentials (nothing here checks a signature).
//
// The S3 API, path-style under the one bucket S3_BUCKET, as framework/objectstore/s3.go uses it:
//   HEAD /<bucket>                         HeadBucket (the store's Ping)
//   PUT /<bucket>/<key>                    PutObject; rules below may hold or fail it
//   GET | HEAD /<bucket>/<key>             GetObject, 404 NoSuchKey when absent
//   DELETE /<bucket>/<key>                 DeleteObject
//   POST /<bucket>?delete                  DeleteObjects
//   GET /<bucket>?list-type=2&prefix=...   ListObjectsV2 (one page)
//
// The control API, for harness rows, under /__s3/control:
//   POST /reset                 forget every object, rule and PUT, and release held PUTs as failed
//   POST /rules {rules: [{match, nth, action}]}
//                               the nth PUT (1-based, counted from this call) whose key contains
//                               `match` is held until released ("hold") or answered 403 AccessDenied,
//                               which the SDK does not retry ("fail")
//   POST /release {match?}      lets held PUTs whose key contains match (every one when omitted) land
//   GET /state?match=&held=N&settled=N&timeoutMs=T
//                               {objects, puts, held}, filtered to keys containing match. With held or
//                               settled, waits until that many matching PUTs are held, or have finished
//                               (stored or failed), or timeoutMs passes (default 15000)
//   GET /object?key=            the stored body of one object, decompressed when it was gzipped
//
// A held PUT is released as stored after HOLD_LIMIT_MS, inside the gateway's 30 s upload deadline, so a
// row that forgets to release it cannot leave an upload hanging past the run.

import { gunzipSync } from "node:zlib";

export const S3_BUCKET = "harness-objects";
const HOLD_LIMIT_MS = 20000;

const xml = (res, status, body) => {
	res.writeHead(status, { "Content-Type": "application/xml" });
	res.end(`<?xml version="1.0" encoding="UTF-8"?>\n${body}`);
};
const s3Error = (res, status, code, message) => xml(res, status, `<Error><Code>${code}</Code><Message>${message}</Message></Error>`);
const json = (res, status, value) => {
	res.writeHead(status, { "Content-Type": "application/json" });
	res.end(JSON.stringify(value));
};
const escapeXml = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

// decodeAwsChunked strips aws-chunked framing (`<hex size>;chunk-signature=...\r\n<data>\r\n`, ending in a
// zero-size chunk and optional trailers), which the SDK uses for a streaming or trailing-checksum upload.
export function decodeAwsChunked(buf) {
	const parts = [];
	let i = 0;
	while (i < buf.length) {
		const eol = buf.indexOf("\r\n", i);
		if (eol < 0) break;
		const size = parseInt(buf.subarray(i, eol).toString("latin1").split(";")[0], 16);
		if (!Number.isFinite(size) || size === 0) break;
		parts.push(buf.subarray(eol + 2, eol + 2 + size));
		i = eol + 2 + size + 2;
	}
	return Buffer.concat(parts);
}

export function createS3ObjectFixture() {
	let objects = new Map(); // key -> {body, headers, lastModified}
	let rules = []; // {match, nth, action, seen}
	let puts = []; // {key, outcome: "held" | "stored" | "failed"}
	let holds = []; // {key, put, release(outcome)}
	let waiters = [];

	const notify = () => {
		const ready = waiters.filter((w) => w.done());
		waiters = waiters.filter((w) => !ready.includes(w));
		for (const w of ready) w.resolve();
	};

	const store = (key, body, headers) => {
		objects.set(key, { body, headers, lastModified: new Date() });
	};

	const actionFor = (key) => {
		let action = null;
		for (const rule of rules) {
			if (!key.includes(rule.match)) continue;
			rule.seen += 1;
			if (rule.seen === rule.nth && !action) action = rule.action;
		}
		return action;
	};

	const putObject = (req, res, key, body) => {
		const streaming = String(req.headers["x-amz-content-sha256"] || "").startsWith("STREAMING-") || /aws-chunked/.test(String(req.headers["content-encoding"] || ""));
		const data = streaming ? decodeAwsChunked(body) : body;
		const headers = {};
		for (const h of ["content-type", "content-encoding"]) {
			const v = req.headers[h];
			if (v) headers[h] = String(v).split(",").map((s) => s.trim()).filter((s) => s !== "aws-chunked").join(", ");
		}
		if (!headers["content-encoding"]) delete headers["content-encoding"];
		const put = { key, outcome: "stored" };
		puts.push(put);
		const land = (outcome) => {
			put.outcome = outcome;
			if (outcome === "stored") {
				store(key, data, headers);
				res.writeHead(200, { ETag: `"${data.length}-${puts.length}"` });
				res.end();
			} else {
				s3Error(res, 403, "AccessDenied", "the harness failed this PUT on purpose");
			}
			notify();
		};
		const action = actionFor(key);
		if (action === "fail") return land("failed");
		if (action === "hold") {
			put.outcome = "held";
			const hold = { key, put };
			const timer = setTimeout(() => hold.release("stored"), HOLD_LIMIT_MS);
			hold.release = (outcome) => {
				clearTimeout(timer);
				holds = holds.filter((h) => h !== hold);
				land(outcome);
			};
			holds.push(hold);
			notify();
			return;
		}
		land("stored");
	};

	const listObjects = (res, prefix) => {
		const keys = [...objects.keys()].filter((k) => k.startsWith(prefix)).sort();
		const contents = keys
			.map((k) => {
				const o = objects.get(k);
				return `<Contents><Key>${escapeXml(k)}</Key><LastModified>${o.lastModified.toISOString()}</LastModified><ETag>"${o.body.length}"</ETag><Size>${o.body.length}</Size><StorageClass>STANDARD</StorageClass></Contents>`;
			})
			.join("");
		xml(res, 200, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>${S3_BUCKET}</Name><Prefix>${escapeXml(prefix)}</Prefix><KeyCount>${keys.length}</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>${contents}</ListBucketResult>`);
	};

	const deleteObjects = (res, body) => {
		const keys = [...body.toString("utf8").matchAll(/<Key>([\s\S]*?)<\/Key>/g)].map((m) => m[1].replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&"));
		for (const k of keys) objects.delete(k);
		xml(res, 200, `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">${keys.map((k) => `<Deleted><Key>${escapeXml(k)}</Key></Deleted>`).join("")}</DeleteResult>`);
	};

	const state = (match) => ({
		objects: [...objects.entries()].filter(([k]) => k.includes(match)).map(([k, o]) => ({ key: k, size: o.body.length })),
		puts: puts.filter((p) => p.key.includes(match)).map((p) => ({ key: p.key, outcome: p.outcome })),
		held: holds.filter((h) => h.key.includes(match)).map((h) => h.key),
	});

	const control = (req, res, url, body) => {
		const op = url.pathname.slice("/__s3/control/".length);
		let input = {};
		if (body.length) {
			try {
				input = JSON.parse(body.toString("utf8"));
			} catch {
				return json(res, 400, { error: "body is not JSON" });
			}
		}
		if (req.method === "POST" && op === "reset") {
			for (const h of [...holds]) h.release("failed");
			objects = new Map();
			rules = [];
			puts = [];
			return json(res, 200, { ok: true });
		}
		if (req.method === "POST" && op === "rules") {
			const added = (input.rules || []).map((r) => ({ match: String(r.match || ""), nth: r.nth === undefined ? 1 : Number(r.nth), action: r.action, seen: 0 }));
			if (added.some((r) => !["hold", "fail"].includes(r.action) || !Number.isInteger(r.nth) || r.nth < 1)) return json(res, 400, { error: "each rule needs action hold or fail and an integer nth >= 1" });
			rules.push(...added);
			return json(res, 200, { rules: rules.length });
		}
		if (req.method === "POST" && op === "release") {
			const match = String(input.match || "");
			const released = holds.filter((h) => h.key.includes(match));
			for (const h of released) h.release("stored");
			return json(res, 200, { released: released.map((h) => h.key) });
		}
		if (req.method === "GET" && op === "state") {
			const match = url.searchParams.get("match") || "";
			const held = Number(url.searchParams.get("held") || 0);
			const settled = Number(url.searchParams.get("settled") || 0);
			const timeoutMs = Number(url.searchParams.get("timeoutMs") || 15000);
			const done = () => {
				const s = state(match);
				return s.held.length >= held && s.puts.filter((p) => p.outcome !== "held").length >= settled;
			};
			if (done()) return json(res, 200, state(match));
			const waiter = { done };
			const timer = setTimeout(() => {
				waiters = waiters.filter((w) => w !== waiter);
				json(res, 200, { ...state(match), timedOut: true });
			}, timeoutMs);
			waiter.resolve = () => {
				clearTimeout(timer);
				json(res, 200, state(match));
			};
			waiters.push(waiter);
			return;
		}
		if (req.method === "GET" && op === "object") {
			const o = objects.get(url.searchParams.get("key") || "");
			if (!o) return json(res, 404, { error: "no such object" });
			const gz = /gzip/.test(o.headers["content-encoding"] || "");
			res.writeHead(200, { "Content-Type": o.headers["content-type"] || "application/octet-stream" });
			return res.end(gz ? gunzipSync(o.body) : o.body);
		}
		return json(res, 404, { error: `no control operation ${req.method} ${op}` });
	};

	return {
		// handles reports whether a request path belongs to this fixture.
		handles: (pathname) => pathname.startsWith("/__s3/control/") || pathname === `/${S3_BUCKET}` || pathname.startsWith(`/${S3_BUCKET}/`),
		handle(req, res, url, body) {
			if (url.pathname.startsWith("/__s3/control/")) return control(req, res, url, body);
			const key = decodeURIComponent(url.pathname.slice(S3_BUCKET.length + 2));
			if (key === "") {
				if (req.method === "HEAD") return res.writeHead(200).end();
				if (req.method === "POST" && url.searchParams.has("delete")) return deleteObjects(res, body);
				if (req.method === "GET") return listObjects(res, url.searchParams.get("prefix") || "");
				return s3Error(res, 405, "MethodNotAllowed", `${req.method} on the bucket`);
			}
			if (req.method === "PUT") return putObject(req, res, key, body);
			if (req.method === "GET" || req.method === "HEAD") {
				const o = objects.get(key);
				if (!o) return req.method === "HEAD" ? res.writeHead(404).end() : s3Error(res, 404, "NoSuchKey", "The specified key does not exist.");
				res.writeHead(200, { ...o.headers, "Content-Length": o.body.length, "Last-Modified": o.lastModified.toUTCString(), ETag: `"${o.body.length}"` });
				return res.end(req.method === "HEAD" ? undefined : o.body);
			}
			if (req.method === "DELETE") {
				objects.delete(key);
				return res.writeHead(204).end();
			}
			return s3Error(res, 405, "MethodNotAllowed", `${req.method} on an object`);
		},
	};
}
