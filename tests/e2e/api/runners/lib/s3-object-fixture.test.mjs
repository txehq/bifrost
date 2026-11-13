// Tests for the harness S3 object store (TXE-7326): the S3 operations the hybrid log store uses, and the
// control API that holds or fails a chosen PUT. The gateway-level proof is harness folder 547, run by
// fixture-folders.mjs's hybrid-logstore group.
import assert from "node:assert/strict";
import http from "node:http";
import { after, before, test } from "node:test";
import { gzipSync } from "node:zlib";
import { createS3ObjectFixture, decodeAwsChunked, S3_BUCKET } from "./s3-object-fixture.mjs";

let server;
let base;

before(async () => {
	const s3 = createS3ObjectFixture();
	server = http.createServer((req, res) => {
		const url = new URL(req.url, "http://localhost");
		if (!s3.handles(url.pathname)) {
			res.writeHead(404).end();
			return;
		}
		const chunks = [];
		req.on("data", (c) => chunks.push(c));
		req.on("end", () => s3.handle(req, res, url, Buffer.concat(chunks)));
	});
	await new Promise((r) => server.listen(0, "127.0.0.1", r));
	base = `http://127.0.0.1:${server.address().port}`;
});

after(() => new Promise((r) => server.close(r)));

const control = async (op, body) => {
	const res = await fetch(`${base}/__s3/control/${op}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });
	return { status: res.status, body: await res.json() };
};
const state = async (query) => (await fetch(`${base}/__s3/control/state?${query}`)).json();
const put = (key, body, headers = {}) => fetch(`${base}/${S3_BUCKET}/${key}`, { method: "PUT", headers, body });

test("stores, reads, lists and deletes objects the way the S3 client uses them", async () => {
	await control("reset");
	assert.equal((await fetch(`${base}/${S3_BUCKET}`, { method: "HEAD" })).status, 200, "HeadBucket");
	assert.equal((await put("p/logs/a.json", "{\"a\":1}", { "Content-Type": "application/json" })).status, 200);
	assert.equal((await put("p/logs/b.json", "{\"b\":2}")).status, 200);
	const got = await fetch(`${base}/${S3_BUCKET}/p/logs/a.json`);
	assert.equal(got.status, 200);
	assert.equal(await got.text(), "{\"a\":1}");
	assert.equal(got.headers.get("content-type"), "application/json");
	const missing = await fetch(`${base}/${S3_BUCKET}/p/logs/none.json`);
	assert.equal(missing.status, 404);
	assert.match(await missing.text(), /<Code>NoSuchKey<\/Code>/);
	const list = await (await fetch(`${base}/${S3_BUCKET}?list-type=2&prefix=p/logs/`)).text();
	assert.match(list, /<KeyCount>2<\/KeyCount>/);
	assert.equal((await fetch(`${base}/${S3_BUCKET}/p/logs/a.json`, { method: "DELETE" })).status, 204);
	const batch = await fetch(`${base}/${S3_BUCKET}?delete`, { method: "POST", body: "<Delete><Object><Key>p/logs/b.json</Key></Object></Delete>" });
	assert.match(await batch.text(), /<Deleted><Key>p\/logs\/b\.json<\/Key><\/Deleted>/);
	assert.deepEqual((await state("match=p/logs/")).objects, []);
});

test("keeps a gzipped body as sent and serves it decompressed through the control API", async () => {
	await control("reset");
	await put("g/logs/z.json.gz", gzipSync("{\"z\":true}"), { "Content-Type": "application/json", "Content-Encoding": "gzip" });
	const raw = await fetch(`${base}/${S3_BUCKET}/g/logs/z.json.gz`);
	assert.equal(raw.headers.get("content-encoding"), "gzip");
	assert.equal(await (await fetch(`${base}/__s3/control/object?key=g/logs/z.json.gz`)).text(), "{\"z\":true}");
});

test("holds the nth matching PUT until released, and waits on it through state", async () => {
	await control("reset");
	assert.equal((await control("rules", { rules: [{ match: "run-1", nth: 2, action: "hold" }] })).status, 200);
	assert.equal((await put("k/run-1-first", "1")).status, 200, "the first matching PUT is not the nth");
	const second = put("k/run-1-second", "2");
	const held = await state("match=run-1&held=1&timeoutMs=5000");
	assert.notEqual(held.timedOut, true);
	assert.deepEqual(held.held, ["k/run-1-second"]);
	assert.deepEqual(held.objects.map((o) => o.key), ["k/run-1-first"], "a held PUT stores nothing yet");
	assert.deepEqual((await control("release", { match: "run-1" })).body.released, ["k/run-1-second"]);
	assert.equal((await second).status, 200);
	const done = await state("match=run-1&settled=2&timeoutMs=5000");
	assert.deepEqual(done.puts.map((p) => p.outcome), ["stored", "stored"]);
	assert.equal(done.objects.length, 2);
});

test("fails the nth matching PUT with a 403 the SDK does not retry, storing nothing", async () => {
	await control("reset");
	await control("rules", { rules: [{ match: "run-2", nth: 1, action: "fail" }] });
	const res = await put("k/run-2", "x");
	assert.equal(res.status, 403);
	assert.match(await res.text(), /<Code>AccessDenied<\/Code>/);
	const s = await state("match=run-2&settled=1&timeoutMs=1000");
	assert.deepEqual(s.puts.map((p) => p.outcome), ["failed"]);
	assert.deepEqual(s.objects, []);
	assert.equal((await put("k/run-2-again", "y")).status, 200, "only the nth matching PUT fails");
});

test("reset fails held PUTs and forgets rules; state times out rather than waiting for ever", async () => {
	await control("reset");
	await control("rules", { rules: [{ match: "run-3", nth: 1, action: "hold" }] });
	const pending = put("k/run-3", "x");
	await state("match=run-3&held=1&timeoutMs=5000");
	await control("reset");
	assert.equal((await pending).status, 403);
	assert.equal((await put("k/run-3-after", "y")).status, 200, "the rule is gone");
	assert.equal((await state("match=run-3&held=1&timeoutMs=50")).timedOut, true);
});

test("rejects a rule with no action or a bad nth", async () => {
	assert.equal((await control("rules", { rules: [{ match: "x", nth: 1, action: "drop" }] })).status, 400);
	assert.equal((await control("rules", { rules: [{ match: "x", nth: 0, action: "hold" }] })).status, 400);
});

test("decodeAwsChunked joins the chunks of an aws-chunked body and drops the trailer", () => {
	const body = Buffer.from("5;chunk-signature=aa\r\nhello\r\n6;chunk-signature=bb\r\n world\r\n0;chunk-signature=cc\r\nx-amz-checksum-crc32:AAAA\r\n\r\n");
	assert.equal(decodeAwsChunked(body).toString(), "hello world");
});
