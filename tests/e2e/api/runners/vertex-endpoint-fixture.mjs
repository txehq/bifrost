import crypto from "node:crypto";
import { argValue, createInterceptProxy, writeFixtureGatewayConfig } from "./lib/tls-intercept-proxy.mjs";

// Local stand-in for Vertex AI, used to observe which host, path and headers the gateway sends.
//
// Vertex hardcodes https://<host>/... with no base-URL override, so the fixture works as the
// gateway's HTTP proxy instead (see lib/tls-intercept-proxy.mjs): it terminates TLS for the
// requested host, records the decrypted request, and replies with a canned Gemini, Claude or Imagen body, a finished Veo operation, or an Omni interaction.
//
//   node vertex-endpoint-fixture.mjs --app-dir <dir> [--port 8792]
//
// It writes <dir>/config.json (one Vertex key per region, a service account whose token_uri points
// back here, and the proxy plus the CA the gateway must trust), then starts listening. Point a
// gateway at <dir> with -app-dir. GET /__connects reads what was recorded, POST /__reset clears it.
// It also stands in for the Cloud Storage and batch job calls of a Vertex batch (see vertexBatchRespond below);
// GET /__gcs returns the objects the gateway uploaded.

const args = process.argv.slice(2);
const appDir = argValue(args, "--app-dir", "");
const port = Number(argValue(args, "--port", process.env.VERTEX_FIXTURE_PORT || "8792"));
if (!appDir) {
	console.error("usage: node vertex-endpoint-fixture.mjs --app-dir <dir> [--port 8792]");
	process.exit(2);
}

const geminiBody = JSON.stringify({
	candidates: [{ content: { role: "model", parts: [{ text: "hello" }] }, finishReason: "STOP", index: 0 }],
	usageMetadata: { promptTokenCount: 1, candidatesTokenCount: 1, totalTokenCount: 2 },
	modelVersion: "gemini-2.5-flash",
});
const claudeBody = JSON.stringify({
	id: "msg_fixture",
	type: "message",
	role: "assistant",
	model: "claude-opus-4-7",
	content: [{ type: "text", text: "hello" }],
	stop_reason: "end_turn",
	usage: { input_tokens: 1, output_tokens: 1 },
});
const imagenBody = JSON.stringify({
	predictions: [
		{ bytesBase64Encoded: "aW1hZ2Ux", mimeType: "image/png" },
		{ raiFilteredReason: "Unable to show generated images. Support codes: 58061214" },
		{ bytesBase64Encoded: "aW1hZ2Uy", mimeType: "image/png" },
	],
});

const fixtureVideo = Buffer.concat([Buffer.from([0x1a, 0x45, 0xdf, 0xa3]), Buffer.from("fixture-video")]).toString("base64");

// The Interactions API an Omni model runs on (harness folder 518, TXE-6538): an interaction whose
// state its id names. omni-completed carries the video inline with its token usage, as Vertex
// returns it; omni-incomplete is a status with no task status of the same meaning; any other id is
// still running.
const omniInteraction = (id) => {
	const interaction = { id, object: "interaction", model: "gemini-omni-1.1-flash-preview", status: "in_progress", created: "2026-11-10T12:22:47Z", updated: "2026-11-10T12:22:47Z" };
	if (id === "omni-incomplete") return { ...interaction, status: "incomplete", updated: "2026-11-10T12:25:15Z" };
	if (id !== "omni-completed") return interaction;
	return {
		...interaction,
		status: "completed",
		updated: "2026-11-10T12:25:15Z",
		steps: [{ type: "model_output", content: [{ type: "video", mime_type: "video/webm", data: fixtureVideo }] }],
		usage: { output_tokens_by_modality: [{ modality: "video", tokens: 46336 }] },
	};
};

// A Vertex batch, TXE-6971: the gateway uploads the lines to Cloud Storage as JSONL, creates a batchPredictionJobs
// job over that file, and reads the job's predictions file back for results. The fixture keeps the uploaded objects,
// finishes a job the moment it is created, and writes its predictions file the way Vertex does: each input line's
// request as it was sent, labels included, beside a response. GET /__gcs on the control port returns the objects, so
// a case can read the exact lines the gateway uploaded.
const gcsObjects = new Map(); // "<bucket>/<object name>" -> body
const batchJobs = new Map(); // job id -> job resource
const json = (status, value) => ({ status, body: JSON.stringify(value) });
const gcsMeta = (bucket, name) => ({ kind: "storage#object", bucket, name, size: String(Buffer.byteLength(gcsObjects.get(`${bucket}/${name}`))), contentType: "application/jsonl", timeCreated: "2026-01-01T00:00:00Z", updated: "2026-01-01T00:00:00Z" });
const parseGcsUri = (uri) => {
	const m = /^gs:\/\/([^/]+)\/?(.*)$/.exec(uri || "");
	return m ? { bucket: m[1], name: m[2] } : { bucket: "", name: "" };
};
// gcsUpload stores a multipart/related upload: a JSON metadata part naming the object, then the file part.
const gcsUpload = (req, body, bucket) => {
	const boundary = (/boundary=([^;]+)/.exec(req.headers["content-type"] || "") || [])[1];
	const parts = boundary ? body.split(`--${boundary}`).filter((part) => part.includes("\r\n\r\n")).map((part) => part.slice(part.indexOf("\r\n\r\n") + 4).replace(/\r\n$/, "")) : [];
	if (parts.length !== 2) return json(400, { error: { code: 400, message: "fixture expects a multipart/related upload of a metadata part and a file part" } });
	const name = JSON.parse(parts[0]).name;
	gcsObjects.set(`${bucket}/${name}`, parts[1]);
	return json(200, gcsMeta(bucket, name));
};
const batchJobCreate = (req, body) => {
	const job = JSON.parse(body);
	const input = parseGcsUri(job.inputConfig?.gcsSource?.uris?.[0]);
	const lines = gcsObjects.get(`${input.bucket}/${input.name}`);
	if (lines === undefined) return json(400, { error: { code: 400, message: "fixture has no input object " + job.inputConfig?.gcsSource?.uris?.[0], status: "INVALID_ARGUMENT" } });
	const output = parseGcsUri(job.outputConfig?.gcsDestination?.outputUriPrefix);
	const id = String(batchJobs.size + 1);
	const directory = `${output.name.replace(/\/+$/, "")}/prediction-model-fixture-${id}`;
	const predictions = lines.split("\n").filter(Boolean).map((line) => JSON.stringify({ request: JSON.parse(line).request, status: "", response: JSON.parse(geminiBody) }));
	gcsObjects.set(`${output.bucket}/${directory}/predictions.jsonl`, predictions.join("\n") + "\n");
	const location = (/\/locations\/([^/]+)\//.exec(req.url) || [])[1];
	const created = { ...job, name: `projects/fixture-project/locations/${location}/batchPredictionJobs/${id}`, state: "JOB_STATE_SUCCEEDED", createTime: "2026-01-01T00:00:00Z", endTime: "2026-01-01T00:00:01Z", outputInfo: { gcsOutputDirectory: `gs://${output.bucket}/${directory}` } };
	batchJobs.set(id, created);
	return json(200, created);
};
// vertexBatchRespond answers the Cloud Storage and batch job calls, or returns undefined for any other request.
const vertexBatchRespond = (req, body) => {
	const url = new URL(req.url, "https://fixture.invalid");
	const upload = /^\/upload\/storage\/v1\/b\/([^/]+)\/o$/.exec(url.pathname);
	if (upload && req.method === "POST") return gcsUpload(req, body, decodeURIComponent(upload[1]));
	const list = /^\/storage\/v1\/b\/([^/]+)\/o$/.exec(url.pathname);
	if (list && req.method === "GET") {
		const bucket = decodeURIComponent(list[1]);
		const prefix = `${bucket}/${url.searchParams.get("prefix") || ""}`;
		return json(200, { kind: "storage#objects", items: [...gcsObjects.keys()].filter((key) => key.startsWith(prefix)).map((key) => gcsMeta(bucket, key.slice(bucket.length + 1))) });
	}
	const object = /^\/storage\/v1\/b\/([^/]+)\/o\/(.+)$/.exec(url.pathname);
	if (object && req.method === "GET") {
		const content = gcsObjects.get(`${decodeURIComponent(object[1])}/${decodeURIComponent(object[2])}`);
		if (content === undefined) return json(404, { error: { code: 404, message: "No such object" } });
		if (url.searchParams.get("alt") !== "media") return json(200, gcsMeta(decodeURIComponent(object[1]), decodeURIComponent(object[2])));
		// Results are paged by byte offset: a Range request gets the rest of the file, or 416 past its end.
		const bytes = Buffer.from(content);
		const offset = Number((/^bytes=(\d+)-$/.exec(req.headers.range || "") || [])[1] || 0);
		if (offset >= bytes.length && offset > 0) return { status: 416, body: "", headers: { "Content-Range": `bytes */${bytes.length}` } };
		return { status: offset > 0 ? 206 : 200, body: bytes.subarray(offset), headers: { "Content-Type": "application/jsonl" } };
	}
	if (/\/batchPredictionJobs$/.test(url.pathname) && req.method === "POST") return batchJobCreate(req, body);
	const job = /\/batchPredictionJobs\/([^/:]+)$/.exec(url.pathname);
	if (job && req.method === "GET") return batchJobs.has(job[1]) ? json(200, batchJobs.get(job[1])) : json(404, { error: { code: 404, message: "fixture has no batch job " + job[1], status: "NOT_FOUND" } });
	return undefined;
};

// A fresh gateway downloads a pricing datasheet and a model-parameters datasheet on first start, and the model
// catalog drives behaviour these cases assert (a model flagged vertex_multi_region_only is promoted to the multi-region
// pool). Serving both from the control port keeps the run offline and the result independent of a remote file.
const proxy = createInterceptProxy({
	port,
	staticJson: {
		"/pricing.json": {},
		"/model-parameters.json": { "claude-opus-4-7": { provider: "vertex", vertex_multi_region_only: true } },
	},
	// The path decides the body shape.
	respond: (req, body) => {
		const batch = vertexBatchRespond(req, body);
		if (batch) return batch;
		if (/:generateContent/.test(req.url)) return { status: 200, body: geminiBody };
		if (/:rawPredict/.test(req.url)) return { status: 200, body: claudeBody };
		if (/:predict(\?|$)/.test(req.url)) return { status: 200, body: imagenBody };
		// The video is inline so download needs no second fetch; the WebM magic marks it binary to the harness shape check.
		if (/:fetchPredictOperation/.test(req.url)) return { status: 200, body: JSON.stringify({ name: (body.match(/"operationName":"([^"]+)"/) || [])[1], done: true, response: { videos: [{ bytesBase64Encoded: fixtureVideo, mimeType: "video/webm" }] } }) };
		const interactionPath = /\/locations\/[^/]+\/interactions(?:\/([^/?]+)(\/cancel)?)?(?:\?|$)/.exec(req.url);
		if (interactionPath) {
			const [, id, cancel] = interactionPath;
			if (!id && req.method === "POST") return { status: 200, body: JSON.stringify(omniInteraction("omni-running")) };
			if (id && cancel && req.method === "POST") return { status: 200, body: JSON.stringify({ ...omniInteraction(id), status: "cancelled" }) };
			if (id && !cancel && req.method === "GET") return { status: 200, body: JSON.stringify(omniInteraction(id)) };
		}
		return { status: 404, body: JSON.stringify({ error: { code: 404, message: "fixture does not serve " + req.url, status: "NOT_FOUND" } }) };
	},
	// OAuth token endpoint for the service account below, so no call goes to Google.
	route: (url, req, res) => {
		if (req.method === "GET" && url.pathname === "/__gcs") {
			res.writeHead(200, { "Content-Type": "application/json" });
			res.end(JSON.stringify({ data: [...gcsObjects.entries()].map(([name, body]) => ({ name, body })) }));
			return true;
		}
		if (url.pathname !== "/token") return false;
		req.resume();
		req.on("end", () => {
			res.writeHead(200, { "Content-Type": "application/json" });
			res.end(JSON.stringify({ access_token: "fixture-access-token", token_type: "Bearer", expires_in: 3600 }));
		});
		return true;
	},
});

// A service account the OAuth library will accept: a real RSA key to sign the JWT, and a token_uri
// that points at this fixture.
const { privateKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048, privateKeyEncoding: { type: "pkcs8", format: "pem" }, publicKeyEncoding: { type: "spki", format: "pem" } });
const serviceAccount = JSON.stringify({
	type: "service_account",
	project_id: "fixture-project",
	private_key_id: "fixture-key-id",
	private_key: privateKey,
	client_email: "fixture@fixture-project.iam.gserviceaccount.com",
	client_id: "1",
	token_uri: `http://127.0.0.1:${port}/token`,
});

const regions = {
	"vertex-global": { region: "global" },
	"vertex-us": { region: "us" },
	"vertex-eu": { region: "eu" },
	"vertex-us-east5": { region: "us-east5" },
	"vertex-us-central1": { region: "us-central1" },
	"vertex-us-central1-pinned": { region: "us-central1", force_single_region: true },
};
const configPath = writeFixtureGatewayConfig(appDir, {
	datasheetOrigin: `http://127.0.0.1:${port}`,
	providers: {
		vertex: {
			keys: Object.entries(regions).map(([name, r]) => ({
				name,
				value: "fixture-api-key",
				models: ["*"],
				weight: 1,
				use_for_batch_api: true,
				vertex_key_config: { project_id: "fixture-project", region: r.region, auth_credentials: serviceAccount, ...(r.force_single_region ? { force_single_region: true } : {}) },
			})),
			network_config: { max_retries: 0, default_request_timeout_in_seconds: 15 },
			proxy_config: { type: "http", url: `http://127.0.0.1:${port}`, ca_cert_pem: proxy.caPem },
		},
	},
});

proxy.listen(() => {
	console.log(`vertex-endpoint-fixture listening on http://127.0.0.1:${port} (config written to ${configPath})`);
});
