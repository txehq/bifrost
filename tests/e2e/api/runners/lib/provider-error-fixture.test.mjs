// Tests provider-error-fixture.mjs through a running instance. Run directly:
// `node provider-error-fixture.test.mjs`.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import net from "node:net";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const FIXTURE = join(dirname(fileURLToPath(import.meta.url)), "..", "provider-error-fixture.mjs");

// startFixture boots the fixture on a random port, its stall proxy on the next one, its media tunnel on
// the one after (post.mediaTunnelPort) and its TLS listener on the one after that, and resolves once it
// listens. Every listener gets its own port: one left on its default collides with any other fixture
// running on the host, and the fixture then exits before it listens. The returned post sends one chat completion and, when the fixture does not answer, fails with
// what it printed on its way out; post.port and post.stallPort name the two listeners.
async function startFixture(t) {
  const port = 20000 + Math.floor(Math.random() * 20000);
  const child = spawn(process.execPath, [FIXTURE], {
    env: { ...process.env, PROVIDER_ERROR_FIXTURE_PORT: String(port), PROVIDER_ERROR_FIXTURE_STALL_PORT: String(port + 1), PROVIDER_ERROR_FIXTURE_MEDIA_TUNNEL_PORT: String(port + 2), PROVIDER_ERROR_FIXTURE_TLS_PORT: String(port + 3) },
    stdio: ["ignore", "pipe", "pipe"],
  });
  t.after(() => child.kill());
  let output = "";
  child.stderr.on("data", (c) => (output += c));
  await new Promise((resolve, reject) => {
    child.stdout.on("data", (c) => {
      output += c;
      if (output.includes("listening")) resolve();
    });
    child.on("exit", (code) => reject(new Error(`the fixture exited (${code}) before it listened: ${output}`)));
  });
  const post = async (body, path = "/v1/chat/completions") => {
    try {
      return await fetch(`http://127.0.0.1:${port}${path}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
    } catch (err) {
      assert.fail(`the fixture stopped answering (${err.cause?.code || err.message}): ${output}`);
    }
  };
  post.port = port;
  post.stallPort = port + 1;
  post.mediaTunnelPort = port + 2;
  return post;
}

// The request's model picks the fixture's behaviour. A model that is not a string names none, so it
// gets the unknown-model answer; it must not stop the one fixture every folder-185 request shares.
test("a model that is not a string is an unknown model and the fixture keeps serving", async (t) => {
  const post = await startFixture(t);
  for (const model of [1, true, { name: "upstream-ok" }, ["upstream-ok"]]) {
    const res = await post({ model, messages: [{ role: "user", content: "hi" }] });
    assert.equal(res.status, 404, `model ${JSON.stringify(model)}`);
    assert.equal((await res.json()).error.code, "model_not_found", `model ${JSON.stringify(model)}`);
  }
  const ok = await post({ model: "upstream-ok", messages: [{ role: "user", content: "hi" }] });
  assert.equal(ok.status, 200, "a valid request after the malformed ones");
});

test("filtered Live create checks accounting events and unchanged command permissions", async (t) => {
  const post = await startFixture(t);
  const top = ["session.started", "session.updated", "session.closed", "session.usage.updated", "session.delegation.created", "session.input_transcript.delta", "session.output_transcript.delta", "error"];
  const nested = ["response.output_item.done", "response.completed", "response.incomplete", "response.failed"];
  const required = [...top.map((type) => ({ type })), ...nested.map((type) => ({ type: "response.event", response_event: type }))];
  const create = (events, commands = ["session.close"]) => post({
    session: {
      model: "gpt-live-1",
      instructions: "filtered-live-create-test",
      client: { data_channel: { allowed_server_events: events, allowed_client_events: commands } },
    },
    transport: { type: "webrtc", sdp: "offer" },
  }, "/v1/live/sessions");
  for (const events of [[], required.filter((event) => event.response_event !== "response.completed")]) {
    const response = await create(events);
    assert.equal(response.status, 400);
    assert.equal((await response.json()).error.code, "missing_gateway_events");
  }
  const changed = await create(required, "all");
  assert.equal(changed.status, 400);
  assert.equal((await changed.json()).error.code, "changed_client_permissions");
  const complete = await create(required);
  assert.equal(complete.status, 201);
  assert.equal((await complete.json()).transport.sdp, "invalid SDP answer");
});

// The stall proxy accepts a connection and never answers the CONNECT it is sent, so a client routed through
// it stalls in its proxy handshake; each CONNECT it reads is counted in /__hits.
test("the stall proxy holds a CONNECT unanswered and counts it", async (t) => {
  const post = await startFixture(t);
  const socket = net.connect(post.stallPort, "127.0.0.1");
  t.after(() => socket.destroy());
  await new Promise((resolve, reject) => {
    socket.once("connect", resolve);
    socket.once("error", reject);
  });
  let answered = "";
  socket.on("data", (c) => (answered += c));
  socket.write("CONNECT 127.0.0.1:" + post.port + " HTTP/1.1\r\nHost: 127.0.0.1:" + post.port + "\r\n\r\n");
  await new Promise((resolve) => setTimeout(resolve, 500));
  assert.equal(answered, "", "the stall proxy must never answer a CONNECT");
  assert.equal(socket.destroyed, false, "the stall proxy must hold the connection open");
  const hits = await (await fetch(`http://127.0.0.1:${post.port}/__hits`)).json();
  assert.equal(hits.data[0].hits.__stall_proxy_connects, 1);
});

// The media tunnel answers a CONNECT to any address and joins it to the fixture itself, so a gateway whose
// SSRF-hardened fetcher was given a public-looking address reaches the fixture's file through the
// provider's proxy (TXE-7036). The address asked for is counted, and reaches the fixture as the Host.
test("the media tunnel joins a CONNECT for any address to the fixture and counts it", async (t) => {
  const post = await startFixture(t);
  const socket = net.connect(post.mediaTunnelPort, "127.0.0.1");
  t.after(() => socket.destroy());
  await new Promise((resolve, reject) => {
    socket.once("connect", resolve);
    socket.once("error", reject);
  });
  let answered = "";
  socket.on("data", (c) => (answered += c.toString("latin1")));
  const until = async (marker) => {
    for (let i = 0; i < 100 && !answered.includes(marker); i++) await new Promise((resolve) => setTimeout(resolve, 20));
    assert.ok(answered.includes(marker), `expected ${JSON.stringify(marker)} in ${JSON.stringify(answered)}`);
  };
  socket.write("CONNECT 203.0.113.10:80 HTTP/1.1\r\nHost: 203.0.113.10:80\r\n\r\n");
  await until("200 Connection established\r\n\r\n");
  answered = "";
  socket.write("GET /__hits HTTP/1.1\r\nHost: 203.0.113.10\r\nConnection: close\r\n\r\n");
  await until('"__media_tunnel_connects@203.0.113.10:80":1');
  assert.match(answered, /^HTTP\/1\.1 200 /);
});

// Folder 482: a finished BytePlus or MiniMax task names its file by URL. The gapped file is named at a
// public-looking address the gateway can only reach through the media tunnel; the loopback task names the
// fixture's own address, which the gateway's SSRF check must refuse.
test("media tasks name the gapped file at a public address and the internal one on loopback", async (t) => {
  const post = await startFixture(t);
  const get = async (path) => (await fetch(`http://127.0.0.1:${post.port}${path}`)).json();
  const ark = await get("/pp/byteplus/api/v3/contents/generations/tasks/fixture-media-gapped");
  assert.equal(ark.status, "succeeded");
  assert.equal(ark.content.video_url, "http://203.0.113.10/pp/media/gapped.mp4?X-Fixture-Signature=signed");
  const minimax = await get("/pp/minimax/v2/query/video_generation/fixture-media-loopback");
  assert.equal(minimax.task.status, "succeeded");
  assert.equal(minimax.task.content.url, `http://127.0.0.1:${post.port}/pp/media/internal.mp4?X-Fixture-Signature=signed`);
  const file = await fetch(`http://127.0.0.1:${post.port}/pp/media/gapped.mp4`);
  assert.equal((await file.arrayBuffer()).byteLength, 2 * 256 * 1024);
  const hits = (await get("/__hits")).data[0].hits;
  assert.equal(hits["media.task@byteplus"], 1);
  assert.equal(hits["media.task@minimax"], 1);
  assert.equal(hits[`media.gapped@127.0.0.1:${post.port}`], 1);
});

// Folder 490: a Replicate answer names addresses a gateway then requests. Each is named on the fixture's own
// origin (or at a public-looking address for an output image) and on another loopback origin, which a
// gateway must refuse to follow; every request is counted under the Host it was asked at.
test("replicate answers name poll, stream, next-page and image addresses on the origin or another loopback host", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}/pp/replicate/v1`;
  const other = `http://localhost:${post.port}/pp/replicate/v1`;
  const create = async (model) => (await post({ input: { prompt: "x" } }, `/pp/replicate/v1/models/owner/${model}/predictions`)).json();
  assert.deepEqual((await create("poll-origin")).urls, { get: `${base}/predictions/pred_poll_origin` });
  assert.deepEqual((await create("poll-loopback")).urls, { get: `${other}/predictions/pred_poll_loopback` });
  assert.deepEqual((await create("stream-origin")).urls, { get: `${base}/predictions/pred_stream_origin`, stream: `${base}/streams/pred_stream_origin` });
  assert.deepEqual((await create("stream-loopback")).urls, { get: `${base}/predictions/pred_stream_loopback`, stream: `${other}/streams/pred_stream_loopback` });
  assert.deepEqual((await create("picture-public")).urls, { get: `${base}/predictions/pred_picture_public` });

  const polled = async (id) => (await (await fetch(`${base}/predictions/${id}`)).json()).output;
  assert.equal(await polled("pred_poll_origin"), "https://replicate-delivery.invalid/fixture/out.png");
  assert.equal(await polled("pred_picture_public"), "http://203.0.113.10/pp/media/image.png?X-Fixture-Signature=signed");
  assert.equal(await polled("pred_picture_loopback"), `http://127.0.0.1:${post.port}/pp/media/internal.png?X-Fixture-Signature=signed`);
  assert.equal(await (await fetch(`${base}/streams/pred_stream_origin`)).text(), "event: output\nid: 1\ndata: fixture stream text\n\nevent: done\nid: 2\ndata: {}\n\n");
  assert.equal((await fetch(`http://127.0.0.1:${post.port}/pp/media/image.png`)).headers.get("content-type"), "image/png");

  const page = async (key, query = "") => (await fetch(`${base}/deployments${query}`, { headers: { Authorization: `Bearer ${key}` } })).json();
  assert.equal((await page("fixture-deploy-origin")).next, `${base}/deployments?page=2`);
  assert.equal((await page("fixture-deploy-loopback")).next, `${other}/deployments?page=2`);
  const last = await page("fixture-deploy-origin", "?page=2");
  assert.equal(last.next, null);
  assert.equal(last.results[0].name, "fixture-deploy-origin-page-2");

  const hits = (await (await fetch(`http://127.0.0.1:${post.port}/__hits`)).json()).data[0].hits;
  assert.equal(hits["replicate.poll@127.0.0.1"], 3);
  assert.equal(hits["replicate.stream@127.0.0.1"], 1);
  assert.equal(hits["media.image@127.0.0.1"], 1);
  assert.equal(hits["replicate.deployments@fixture-deploy-origin?page=2@127.0.0.1"], 1);
  assert.deepEqual(Object.keys(hits).filter((k) => k.endsWith("@localhost")), []);
});

test("a turns model calls the tool, then answers once the result comes back", async (t) => {
  const post = await startFixture(t);
  const first = await (await post({ model: "turns-priced", messages: [{ role: "user", content: "weather?" }] })).json();
  assert.equal(first.choices[0].finish_reason, "tool_calls");
  assert.equal(first.usage.prompt_tokens, 150000);
  const second = await (await post({ model: "turns-priced", messages: [{ role: "tool", content: "Sunny.", tool_call_id: "call_turn_1" }] })).json();
  assert.equal(second.choices[0].finish_reason, "stop");

  const tiered = await (await post({ model: "turns-tiered", input: [] }, "/v1/responses")).json();
  assert.equal(tiered.service_tier, "priority");
  assert.equal(tiered.output[0].type, "function_call");
  const third = await post({ model: "turns-fail", input: [{ type: "function_call_output" }, { type: "function_call_output" }] }, "/v1/responses");
  assert.equal(third.status, 500);
});

test("a turns model streams its turn with usage on the terminal event", async (t) => {
  const post = await startFixture(t);
  const chat = await (await post({ model: "turns-priced", stream: true, messages: [] })).text();
  assert.match(chat, /"prompt_tokens":150000/);
  assert.match(chat, /data: \[DONE\]/);
  const responses = await (await post({ model: "turns-priced", stream: true, input: [] }, "/v1/responses")).text();
  assert.match(responses, /event: response\.completed/);
  assert.match(responses, /"input_tokens":150000/);
});

test("the MCP endpoint serves the tool the turns models call", async (t) => {
  const post = await startFixture(t);
  const call = async (method) => (await post({ jsonrpc: "2.0", id: 1, method }, "/mcp")).json();
  assert.equal((await call("initialize")).result.serverInfo.name, "provider-error-fixture");
  assert.equal((await call("tools/list")).result.tools[0].name, "web_search");
  assert.equal((await call("tools/call")).result.content[0].text, "Sunny.");
  assert.equal((await post({ jsonrpc: "2.0", method: "notifications/initialized" }, "/mcp")).status, 202);
});

test("the OpenAPI document declares no servers and its operations are recorded", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  const doc = await (await fetch(`${base}/openapi-fixture/docs/no-servers.json`)).json();
  assert.equal(doc.servers, undefined);
  assert.equal(doc.paths["/fixture-api/items/{id}"].put.operationId, "putItem");
  await fetch(`${base}/fixture-api/items/7`, { method: "PUT", body: "{\"name\":\"n\"}" });
  const calls = async () => (await (await fetch(`${base}/__openapi-calls`)).json()).data[0].calls;
  assert.deepEqual(await calls(), [{ method: "PUT", path: "/fixture-api/items/7", body: "{\"name\":\"n\"}" }]);
  await fetch(`${base}/__reset`, { method: "POST" });
  assert.deepEqual(await calls(), []);
});

test("decision routes reject their answers but report usage", async (t) => {
  const post = await startFixture(t);
  const native = await (await post({ model: "gpt-6-luna" }, "/v1/decisions")).json();
  assert.deepEqual(native.answers, []);
  assert.equal(native.usage.total_tokens, 1010);
  const routed = await (await post({ model: "typesafe/jev-1.13" }, "/alpha/decisions")).json();
  assert.equal(routed.usage.cost, 0.25);
});

test("the Gemini audio models answer in the generateContent shape, streamed or not", async (t) => {
  const post = await startFixture(t);
  const call = (model, method) => post({}, `/pp/gemini/models/${model}:${method}`);
  const ok = await (await call("pp-tts-ok", "generateContent")).json();
  assert.equal(ok.candidates[0].finishReason, "STOP");
  assert.equal(ok.usageMetadata.thoughtsTokenCount, 12);
  assert.deepEqual(ok.usageMetadata.candidatesTokensDetails, [{ modality: "AUDIO", tokenCount: 50 }]);
  const okStream = await (await call("pp-tts-ok", "streamGenerateContent")).text();
  assert.equal(okStream.match(/^data: /gm).length, 2);
  assert.match(okStream, /"finishReason":"STOP"/);
  const blocked = await (await call("pp-tts-blocked", "generateContent")).json();
  assert.equal(blocked.candidates, undefined);
  assert.equal(blocked.promptFeedback.blockReason, "SAFETY");
  const truncated = await (await call("pp-tts-truncated", "streamGenerateContent")).text();
  assert.equal(truncated.match(/^data: /gm).length, 1);
  assert.doesNotMatch(truncated, /finishReason/, "the truncated stream must end without the vendor's end marker");
  const transcript = await (await call("pp-stt-ok", "generateContent")).json();
  assert.equal(transcript.candidates[0].content.parts[0].text, "hello from the fixture");
  const other = await (await call("pp-gemini", "generateContent")).json();
  assert.equal(other.modelVersion, "pp-gemini", "any other Gemini model keeps its existing answer");
});

test("a /pp/ provider call is kept byte for byte until a reset", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  const sent = '{ "model":"grok",   "max_tokens": 1 }';
  const answer = await fetch(`${base}/pp/xai/v1/messages?beta=1`, { method: "POST", body: sent });
  assert.equal(answer.status, 200);
  assert.equal((await answer.json()).content[0].text, "hello");
  const refused = await fetch(`${base}/pp/cloudflare/accounts/other/ai/run/x`, { method: "POST", body: "{}" });
  assert.equal(refused.status, 500, "a path the fixture has no answer for must not look like a success");
  const wire = async () => (await (await fetch(`${base}/__wire`)).json()).data[0].calls;
  assert.deepEqual((await wire())[0], { path: "/pp/xai/v1/messages", query: "?beta=1", body: sent });
  assert.equal((await wire()).length, 2);
  await fetch(`${base}/__reset`, { method: "POST" });
  assert.deepEqual(await wire(), []);
});

// Folder 289 (TXE-6519): the OpenAI-shaped video, file and batch endpoints answer and count under fixed
// keys, so a case can prove the gateway's accounting sweeper never called them on its own.
test("video, file and batch endpoints answer in OpenAI shape and count their hits", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  await fetch(`${base}/__reset`, { method: "POST" });
  const created = await post({ model: "sora-fixture", prompt: "a clip" }, "/v1/videos");
  assert.equal(created.status, 200);
  const video = await created.json();
  assert.equal(video.object, "video");
  assert.equal(video.status, "queued");
  const retrieved = await (await fetch(`${base}/v1/videos/${video.id}`)).json();
  assert.equal(retrieved.status, "completed");
  const content = await fetch(`${base}/v1/videos/${video.id}/content`);
  assert.equal(content.headers.get("content-type"), "video/mp4");
  assert.ok((await content.text()).includes("FIXTURE-MP4"));
  const deleted = await (await fetch(`${base}/v1/videos/${video.id}`, { method: "DELETE" })).json();
  assert.equal(deleted.deleted, true);
  const file = await (await fetch(`${base}/v1/files`, { method: "POST", body: "not a multipart body, drained all the same" })).json();
  assert.equal(file.purpose, "batch");
  const batch = await (await post({ input_file_id: file.id, endpoint: "/v1/chat/completions" }, "/v1/batches")).json();
  assert.equal(batch.object, "batch");
  const polled = await (await fetch(`${base}/v1/batches/${batch.id}`)).json();
  assert.equal(polled.status, "in_progress");
  const hits = (await (await fetch(`${base}/__hits`)).json()).data[0].hits;
  const counted = {};
  for (const key of ["video.create", "video.retrieve", "video.content", "video.delete", "file.upload", "batch.create", "batch.retrieve"]) counted[key] = hits[key];
  assert.deepEqual(counted, { "video.create": 1, "video.retrieve": 1, "video.content": 1, "video.delete": 1, "file.upload": 1, "batch.create": 1, "batch.retrieve": 1 });
  assert.equal(hits["file.content"], undefined, "nothing downloaded a results file");
});

// Folder 407 (TXE-6665): the bigmodel upstream answers files and batches only under /api/paas/v4 and keeps
// every call with its method and the name of the key it carried, so a case can prove which path and which key
// the gateway used; a call on the default /v1 path is a 404 that still shows in the wire.
test("the bigmodel upstream answers only under /api/paas/v4 and records method and key name", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}/pp/bigmodel`;
  const keyA = { Authorization: "Bearer bm-fixture-key-a" };
  const keyB = { Authorization: "Bearer bm-fixture-key-b" };
  const file = await (await fetch(`${base}/api/paas/v4/files`, { method: "POST", headers: keyB, body: "multipart bytes" })).json();
  assert.equal(file.id, "file-bm-input");
  const created = await (await fetch(`${base}/api/paas/v4/batches`, { method: "POST", headers: keyB, body: JSON.stringify({ input_file_id: file.id, endpoint: "/v4/chat/completions", metadata: { tokun_batch_id: "b1" } }) })).json();
  assert.equal(created.status, "validating");
  assert.deepEqual(created.metadata, { tokun_batch_id: "b1" });
  const batch = await (await fetch(`${base}/api/paas/v4/batches/${created.id}`, { headers: keyB })).json();
  assert.equal(batch.status, "completed");
  assert.equal(batch.endpoint, "/v4/chat/completions");
  const cancelled = await (await fetch(`${base}/api/paas/v4/batches/${created.id}/cancel`, { method: "POST", headers: keyA })).json();
  assert.equal(cancelled.status, "cancelling");
  const output = (await (await fetch(`${base}/api/paas/v4/files/${batch.output_file_id}/content`, { headers: keyB })).text()).trim().split("\n").map((l) => JSON.parse(l));
  assert.deepEqual(output.map((l) => l.custom_id), ["request-1", "request-2"]);
  assert.equal(output[0].response.body.usage.total_tokens, 115);
  const errors = (await (await fetch(`${base}/api/paas/v4/files/${batch.error_file_id}/content`)).text()).trim().split("\n").map((l) => JSON.parse(l));
  assert.equal(errors[0].response.body.error.code, "1210");
  for (const [method, path] of [["GET", `/v1/batches/${created.id}`], ["POST", `/v1/batches/${created.id}/cancel`], ["GET", "/v1/files/file-bm-output/content"], ["GET", "/api/paas/v4/batches/other"]]) {
    assert.equal((await fetch(`${base}${path}`, { method, headers: keyB })).status, 404, `${method} ${path}`);
  }
  const calls = (await (await fetch(`http://127.0.0.1:${post.port}/__wire`)).json()).data[0].calls;
  assert.deepEqual(calls.slice(0, 6).map((c) => `${c.method} ${c.path} ${c.key}`), [
    "POST /pp/bigmodel/api/paas/v4/files tk-key-bigmodel-b",
    "POST /pp/bigmodel/api/paas/v4/batches tk-key-bigmodel-b",
    `GET /pp/bigmodel/api/paas/v4/batches/${created.id} tk-key-bigmodel-b`,
    `POST /pp/bigmodel/api/paas/v4/batches/${created.id}/cancel tk-key-bigmodel-a`,
    "GET /pp/bigmodel/api/paas/v4/files/file-bm-output/content tk-key-bigmodel-b",
    "GET /pp/bigmodel/api/paas/v4/files/file-bm-error/content unknown",
  ]);
  assert.equal(calls.length, 10, "the refused calls are recorded too");
  assert.ok(!JSON.stringify(calls).includes("bm-fixture-key"), "no key value is kept");
});

// Folder 405 (TXE-6587): the paged video list and the gapped download count each call under the caller's key,
// and the download probe reports when the first byte arrived against when the fixture wrote the second half.
test("the video list pages, the gapped download pauses, and both count per caller key", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  await fetch(`${base}/__reset`, { method: "POST" });
  const asKey = (key) => ({ headers: { Authorization: `Bearer ${key}` } });
  const first = await (await fetch(`${base}/v1/videos?limit=2&status=failed&status=completed`, asKey("key-b"))).json();
  assert.deepEqual([first.data.map((v) => v.id), first.has_more, first.last_id], [["video_a", "video_b"], true, "video_b"]);
  const last = await (await fetch(`${base}/v1/videos?after=video_b&limit=2`, asKey("key-b"))).json();
  assert.deepEqual([last.data.map((v) => v.id), last.has_more], [["video_c"], false]);

  // The probe is pointed at the fixture itself, standing in for a gateway that streams.
  const probed = await (await post({ base_url: base, path: "/v1/videos/video_gapped_1/content", headers: { Authorization: "Bearer key-a" } }, "/__probe/download")).json();
  const r = probed.data[0];
  assert.equal(r.status, 200);
  assert.equal(r.content_type, "video/mp4");
  assert.equal(r.bytes, 2 * 256 * 1024);
  assert.equal(r.sends.length, 2);
  assert.ok(r.first_byte_ms < r.sends[1], `the first byte (${r.first_byte_ms}ms) precedes the second half (${r.sends[1]}ms)`);
  assert.ok(r.end_ms >= r.gap_ms, `the file ends after the gap: ${JSON.stringify(r)}`);

  const hits = (await (await fetch(`${base}/__hits`)).json()).data[0].hits;
  assert.deepEqual(hits, {
    "video.list@key-b?limit=2&status=failed&status=completed": 1,
    "video.list@key-b?after=video_b&limit=2": 1,
    "video.content@key-a": 1,
  });
});

// upstream-idle-stream answers a streamed chat request in two writes with a 2.5 s gap, and /__sent keeps the
// exact bytes, so a passthrough case can compare what the gateway relayed byte for byte.
test("upstream-idle-stream pauses between its events and records the bytes it sent", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  const started = Date.now();
  const res = await post({ model: "upstream-idle-stream", messages: [{ role: "user", content: "hi" }], stream: true });
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("content-type"), "text/event-stream");
  const body = await res.text();
  assert.ok(Date.now() - started >= 2000, "the stream must stay idle for longer than a heartbeat interval");
  assert.ok(body.startsWith("data: "), body);
  assert.ok(body.endsWith("data: [DONE]\n\n"), body);
  const sent = (await (await fetch(`${base}/__sent?model=upstream-idle-stream`)).json()).data[0];
  assert.equal(sent.body, body);
  await fetch(`${base}/__reset`, { method: "POST" });
  const cleared = (await (await fetch(`${base}/__sent?model=upstream-idle-stream`)).json()).data[0];
  assert.equal(cleared.body, null, "a reset clears the recorded stream");
});

// upstream-speech-empty is a speech 200 with no audio and no error object, which Bifrost turns into
// 502 provider_empty_result (provider-harness folder 330).
test("upstream-speech-empty answers 200 with an empty audio body and a vendor request id", async (t) => {
  const post = await startFixture(t);
  const res = await post({ model: "upstream-speech-empty", input: "hi", voice: "alloy" }, "/v1/audio/speech");
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("content-type"), "audio/mpeg");
  assert.equal(res.headers.get("x-request-id"), "fixture-speech-empty");
  assert.equal((await res.arrayBuffer()).byteLength, 0);
});

// TXE-6528: the openai provider's images API. A JSON or multipart request to /v1/images/generations or
// /v1/images/edits is recorded on /__wire with its Content-Type, answered with n images and OpenAI's token usage
// (no image count), and gpt-image-blocked gets OpenAI's moderation block.
test("OpenAI image paths answer in the gpt-image shape, record the wire, and block gpt-image-blocked", async (t) => {
  const post = await startFixture(t);
  const generated = await post({ model: "gpt-image-1.5", prompt: "a fox", n: 2 }, "/v1/images/generations");
  assert.equal(generated.status, 200);
  const body = await generated.json();
  assert.equal(body.data.length, 2);
  assert.deepEqual(body.usage, { input_tokens: 12, input_tokens_details: { image_tokens: 0, text_tokens: 12 }, output_tokens: 544, output_tokens_details: { image_tokens: 544, text_tokens: 0 }, total_tokens: 556 });
  assert.equal(body.usage.output_images, undefined, "OpenAI reports no image count");

  const form = new FormData();
  form.set("model", "gpt-image-blocked");
  form.set("prompt", "blocked");
  form.set("image[]", new Blob([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], { type: "image/png" }), "image0.png");
  const blocked = await fetch(`http://127.0.0.1:${post.port}/v1/images/edits`, { method: "POST", body: form });
  assert.equal(blocked.status, 400);
  assert.equal((await blocked.json()).error.code, "moderation_blocked");

  const edited = new FormData();
  edited.set("model", "gpt-image-1.5");
  edited.set("prompt", "two hats");
  edited.set("n", "2");
  const twice = await fetch(`http://127.0.0.1:${post.port}/v1/images/edits`, { method: "POST", body: edited });
  assert.equal((await twice.json()).data.length, 2, "n is read from a multipart form");

  const streamed = await post({ model: "gpt-image-1.5", prompt: "a hat", images: [{ image_url: "https://images.example.invalid/a.png" }], stream: true }, "/v1/images/edits");
  assert.equal(streamed.status, 200);
  assert.match(streamed.headers.get("content-type"), /^text\/event-stream/);
  const events = (await streamed.text()).split("\n\n").filter(Boolean).map((e) => JSON.parse(e.split("\ndata: ")[1]));
  assert.deepEqual(events.map((e) => e.type), ["image_edit.partial_image", "image_edit.completed"]);
  assert.equal(events[1].usage.total_tokens, 284);

  // A streamed generation with two previews: partial_image_index numbers the previews of the one image (TXE-6984).
  const generatedStream = await post({ model: "gpt-image-1.5", prompt: "a fox", stream: true, partial_images: 2 }, "/v1/images/generations");
  assert.match(generatedStream.headers.get("content-type"), /^text\/event-stream/);
  const generationEvents = (await generatedStream.text()).split("\n\n").filter(Boolean).map((e) => JSON.parse(e.split("\ndata: ")[1]));
  assert.deepEqual(generationEvents.map((e) => [e.type, e.partial_image_index]), [["image_generation.partial_image", 0], ["image_generation.partial_image", 1], ["image_generation.completed", undefined]]);
  assert.equal(generationEvents[2].usage.output_images, undefined, "OpenAI reports no image count");

  const wire = await (await fetch(`http://127.0.0.1:${post.port}/__wire`)).json();
  const calls = wire.data[0].calls;
  assert.deepEqual(calls.map((c) => c.path), ["/v1/images/generations", "/v1/images/edits", "/v1/images/edits", "/v1/images/edits", "/v1/images/generations"]);
  assert.equal(calls[0].contentType, "application/json");
  assert.match(calls[1].contentType, /^multipart\/form-data/);
});

// The video task routes count every request by method and bearer token, which is how a harness case
// proves which key a task query used and that a refused request never reached the upstream.
test("video task routes answer a queued task and count requests per key", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  const hits = async () => (await (await fetch(`${base}/__hits`)).json()).data[0].hits;
  const get = (id, key) => fetch(`${base}/v1/videos/${id}`, { headers: { Authorization: `Bearer ${key}` } });

  const task = await get("task_1", "video-secret-b");
  assert.equal(task.status, 200);
  assert.deepEqual(await task.json(), { id: "task_1", object: "video", model: "sora-2", status: "queued", progress: 0, created_at: 1762700000, seconds: "4", size: "720x1280" });
  await get("task_1", "video-secret-b");
  await get("task_1", "video-secret-a");
  const cancel = await fetch(`${base}/v1/videos/task_1/cancel`, { method: "POST", headers: { Authorization: "Bearer video-secret-a" } });
  assert.equal(cancel.status, 404, "OpenAI has no cancel API, so the fixture has none");
  assert.deepEqual(await hits(), { "videos GET video-secret-b": 2, "videos GET video-secret-a": 1, "videos POST video-secret-a": 1 });

  await fetch(`${base}/__reset`, { method: "POST" });
  assert.deepEqual(await hits(), {});
});

// TXE-6526: the model in a Gemini generateContent path picks the image answer; any other model keeps the
// answer the path had before.
test("a Gemini generateContent path answers by its image fixture model", async (t) => {
  const post = await startFixture(t);
  const call = async (model) => (await post({ contents: [] }, `/pp/gemini/models/${model}:generateContent`)).json();
  const delivered = await call("fixture-image-delivered");
  assert.equal(delivered.candidates[0].content.parts[1].inlineData.mimeType, "image/png");
  assert.equal(delivered.usageMetadata.thoughtsTokenCount, 200);
  assert.equal((await call("fixture-image-safety")).candidates[0].finishReason, "IMAGE_SAFETY");
  const blocked = await call("fixture-image-prompt-blocked");
  assert.equal(blocked.promptFeedback.blockReason, "PROHIBITED_CONTENT");
  assert.equal(blocked.candidates, undefined);
  assert.equal((await call("fixture-image-text-only")).candidates[0].content.parts[0].text, "I can't create that image.");
  const thoughtsOnly = await call("fixture-image-thoughts-only");
  assert.equal(thoughtsOnly.usageMetadata.thoughtsTokenCount, 200);
  assert.equal(thoughtsOnly.usageMetadata.candidatesTokensDetails, undefined);
  const pricing = await (await fetch(`http://127.0.0.1:${post.port}/pricing.json`)).json();
  assert.equal(pricing["fixture-image-thoughts-only"].output_cost_per_token, 0.00003);
  const uploadA = await (await post({}, "/pp/gemini/files")).json();
  const uploadB = await (await post({}, "/pp/gemini/files")).json();
  assert.match(uploadA.file.name, /^files\/fixture-upload-/);
  assert.notEqual(uploadA.file.name, uploadB.file.name);
  assert.equal(uploadA.file.uri, `https://generativelanguage.googleapis.com/v1beta/${uploadA.file.name}`);
  const empty = await call("fixture-image-empty");
  assert.equal(empty.candidates, undefined);
  assert.equal(empty.promptFeedback, undefined);
  const other = await call("gemini-2.5-flash");
  assert.ok(other.candidates[0].content.parts.some((p) => p.executableCode || p.codeExecutionResult || p.text !== undefined), "the code-execution answer");
});

// TXE-6527: the OpenAI-compatible chat paths record the body the vendor received, so a harness row can read
// max_completion_tokens, the prompt-cache fields and the service tier back; the Azure blocked-prompt answer is
// the documented 400 with code content_filter; the capability sheet lists the tiers of oc-tiered.
test("compatible chat requests are recorded, and the azure content filter answer is the documented 400", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  for (const path of ["/pp/oc/v1/chat/completions", "/pp/oc/chat/completions"]) {
    const res = await post({ model: "oc-plain", messages: [{ role: "user", content: "hi" }], max_completion_tokens: 5, service_tier: "priority", prompt_cache_options: { ttl: "1h" } }, path);
    assert.equal(res.status, 200, path);
    assert.equal((await res.json()).service_tier, "priority", `${path} echoes the tier it was asked for`);
  }
  const { calls } = (await (await fetch(`${base}/__wire`)).json()).data[0];
  assert.deepEqual(calls.map((c) => c.path), ["/pp/oc/v1/chat/completions", "/pp/oc/chat/completions"]);
  assert.equal(JSON.parse(calls[0].body).max_completion_tokens, 5);
  assert.equal(JSON.parse(calls[0].body).prompt_cache_options.ttl, "1h");

  const blocked = await post({ model: "azure-content-filter", messages: [{ role: "user", content: "hi" }] }, "/openai/v1/chat/completions");
  assert.equal(blocked.status, 400);
  const { error } = await blocked.json();
  assert.equal(error.code, "content_filter");
  assert.equal(error.innererror.code, "ResponsibleAIPolicyViolation");

  const sheet = await (await fetch(`${base}/model-parameters.json`)).json();
  assert.deepEqual(sheet["oc-tiered"].service_tiers, ["default", "priority"]);
  assert.equal(sheet["oc-tiered"].provider, "moonshot");
});

// Folder 429 (TXE-7044): gpt-image-drop is read and recorded, then loses its connection unanswered.
test("gpt-image-drop is recorded on the wire and its connection dropped without a response", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  await fetch(`${base}/__reset`, { method: "POST" });
  await assert.rejects(fetch(`${base}/v1/images/generations`, { method: "POST", body: JSON.stringify({ model: "gpt-image-drop", prompt: "a dog" }) }));
  const ok = await post({ model: "gpt-image-1.5", prompt: "a dog" }, "/v1/images/generations");
  assert.equal(ok.status, 200);
  const calls = (await (await fetch(`${base}/__wire`)).json()).data[0].calls;
  assert.deepEqual(calls.map((c) => JSON.parse(c.body).model), ["gpt-image-drop", "gpt-image-1.5"]);
});

// Folder 471 (TXE-7071): the stale-get probe. Pointed at the fixture itself as the "gateway", every held GET
// is one connection, all are answered together and then closed, and the last GET is counted once.
test("the stale-get probe holds its burst on separate connections, closes them and counts the last GET", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  const probe = await post({ base_url: base, hold_path: "/v1/videos/video_stale_hold", path: "/v1/videos/video_stale_after", count: 4 }, "/__probe/stale-get");
  assert.equal(probe.status, 200);
  const r = (await probe.json()).data[0];
  assert.deepEqual(r.burst, [200, 200, 200, 200]);
  assert.equal(r.connections, 4);
  assert.equal(r.after.status, 200);
  assert.equal(r.after_upstream_requests, 1);
  // Outside a probe the held task answers at once.
  assert.equal((await fetch(`${base}/v1/videos/video_stale_hold`)).status, 200);
});

// Folder 421 (TXE-6431): BFL's submit answers AsyncResponse and is kept on the wire, and get_result answers per
// task id with BFL's statuses, counted under "bfl get_result <id>".
test("the BFL stand-in answers a submit and the task query by id", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  await fetch(`${base}/__reset`, { method: "POST" });
  const submitted = await post({ prompt: "a fox", width: 1024 }, "/pp/bfl/v1/flux-pro-1.1");
  assert.equal(submitted.status, 200);
  const task = await submitted.json();
  assert.equal(task.id, "bfl-ready");
  assert.equal(task.polling_url, "http://127.0.0.1:8791/pp/bfl/v1/get_result?id=bfl-ready");
  assert.equal(task.cost, 4);
  const wire = (await (await fetch(`${base}/__wire`)).json()).data[0].calls;
  assert.equal(wire[0].path, "/pp/bfl/v1/flux-pro-1.1");
  assert.deepEqual(JSON.parse(wire[0].body), { prompt: "a fox", width: 1024 });
  const status = async (id) => (await (await fetch(`${base}/pp/bfl/v1/get_result?id=${id}`)).json()).status;
  assert.equal(await status("bfl-ready"), "Ready");
  assert.equal(await status("bfl-moderated"), "Content Moderated");
  assert.equal(await status("bfl-pending"), "Pending");
  assert.equal(await status("nope"), "Task not found");
  const hits = (await (await fetch(`${base}/__hits`)).json()).data[0].hits;
  assert.equal(hits["bfl get_result bfl-ready"], 1);
});

// Folder 456 (TXE-7056): a Replicate prediction create is accepted with no urls to poll.
test("a Replicate prediction create answers 201 with no urls", async (t) => {
  const post = await startFixture(t);
  const answer = await post({ input: { prompt: "a dog" } }, "/pp/replicate/v1/models/owner/model/predictions");
  assert.equal(answer.status, 201);
  assert.deepEqual(await answer.json(), { id: "pred_fixture_no_urls", status: "starting" });
});

// TXE-6439: one /pp/<site> base per vendor site, each answering chat with the usage shape its vendor
// documents. StepFun's international site is the one that puts cached_tokens at the top of usage and
// sends usage on every streamed chunk; the cases of folder 408 depend on exactly these shapes.
test("the vendor chat sites answer with each vendor's documented usage shape", async (t) => {
  const post = await startFixture(t);
  const ask = async (site, extra = {}) =>
    post({ model: "any-model", messages: [{ role: "user", content: "hi" }], ...extra }, `/pp/${site}/v1/chat/completions`);

  const stepfun = await (await ask("stepfun")).json();
  assert.deepEqual(stepfun.usage, { cached_tokens: 512, prompt_tokens: 591, completion_tokens: 120, total_tokens: 711 });
  assert.equal(stepfun.model, "any-model", "the answer names the requested model");

  const domestic = await (await ask("stepfun-cn")).json();
  assert.equal(domestic.usage.cached_tokens, undefined, "the domestic site nests the count");
  assert.equal(domestic.usage.prompt_tokens_details.cached_tokens, 512);

  for (const site of ["tencent", "xiaomi"]) {
    const answer = await (await ask(site)).json();
    assert.equal(answer.usage.cached_tokens, undefined, `${site} has no top-level cached_tokens`);
    assert.equal(answer.usage.total_tokens, 145, site);
  }

  const frames = async (site, extra) =>
    (await (await ask(site, { stream: true, ...extra })).text())
      .split("\n")
      .filter((line) => line.startsWith("data: {"))
      .map((line) => JSON.parse(line.slice(6)));
  const stepfunFrames = await frames("stepfun", {});
  assert.equal(stepfunFrames.length, 3);
  for (const frame of stepfunFrames) assert.equal(frame.usage.cached_tokens, 512, "StepFun sends usage on every chunk");
  const xiaomiFrames = await frames("xiaomi", { stream_options: { include_usage: true } });
  assert.deepEqual(xiaomiFrames.map((f) => f.usage !== undefined), [false, false, false, true], "usage only on the trailing frame");
  assert.equal((await frames("xiaomi", {})).length, 3, "no usage frame unless include_usage asks for one");

  const unknown = await ask("stepfun-eu");
  assert.equal(unknown.status, 500, "a site the fixture does not play has no answer");

  // The capability sheet lists service tiers for one model per vendor provider, so a harness row can send a
  // tier outside the list and be refused.
  const sheet = await (await fetch(`http://127.0.0.1:${post.port}/model-parameters.json`)).json();
  for (const [model, provider] of [["hy-tiered", "tencent_tokenhub"], ["step-tiered", "stepfun"], ["mimo-tiered", "xiaomi"]]) {
    assert.equal(sheet[model].provider, provider, model);
    assert.deepEqual(sheet[model].service_tiers, ["default", "priority"], model);
  }
});

test("Cloud Text-to-Speech answers text:synthesize by the voice name", async (t) => {
  const post = await startFixture(t);
  const synthesize = (name) => post({ input: { text: "hi" }, voice: { languageCode: "en-GB", name }, audioConfig: { audioEncoding: "MP3" } }, "/pp/google-cloud-tts/v1/text:synthesize");
  const ok = await synthesize("en-GB-Wavenet-A");
  assert.equal(ok.status, 200);
  assert.equal((await ok.json()).audioContent, "//NExAASCCIIAAhEAGAAEMW4kAYPnwwIKw/BBTpwTvB+IAxIfghU");
  const unknown = await synthesize("en-GB-Wavenet-ZZ");
  assert.equal(unknown.status, 400);
  assert.equal((await unknown.json()).error.status, "INVALID_ARGUMENT");
  const quota = await synthesize("en-GB-Wavenet-Quota");
  assert.equal(quota.status, 429);
  assert.equal((await quota.json()).error.status, "RESOURCE_EXHAUSTED");
  const silent = await synthesize("en-GB-Wavenet-Silent");
  assert.equal(silent.status, 200);
  assert.deepEqual(await silent.json(), {});
});

// The Seedance task routes (TXE-6426, provider-harness folder 413): a query answers the task whose state its id
// names, a create is kept in /__wire, and every query and DELETE is counted per task id.
test("seedance task routes answer by task id and count queries and deletes", async (t) => {
  const post = await startFixture(t);
  const base = `http://127.0.0.1:${post.port}`;
  const tasks = `${base}/pp/byteplus/api/v3/contents/generations/tasks`;
  const hits = async () => (await (await fetch(`${base}/__hits`)).json()).data[0].hits;

  const created = await post({ model: "dreamina-seedance-2-0-fast-260128", content: [{ type: "text", text: "a boat" }] }, "/pp/byteplus/api/v3/contents/generations/tasks");
  assert.deepEqual(await created.json(), { id: "cgt-fixture-video-task" });

  const done = await (await fetch(`${tasks}/cgt-fixture-succeeded`)).json();
  assert.equal(done.status, "succeeded");
  assert.deepEqual(done.usage, { completion_tokens: 108900, total_tokens: 108900 });
  assert.equal(done.content.video_url, "https://fixture.invalid/seedance/video.mp4");
  assert.equal((await (await fetch(`${tasks}/cgt-fixture-expired`)).json()).status, "expired");
  assert.equal((await (await fetch(`${tasks}/cgt-fixture-failed`)).json()).error.code, "OutputVideoSensitiveContentDetected");

  const deleted = await fetch(`${tasks}/cgt-fixture-queued`, { method: "DELETE" });
  assert.equal(deleted.status, 200);
  assert.deepEqual(await hits(), {
    "seedance.retrieve cgt-fixture-succeeded": 1,
    "seedance.retrieve cgt-fixture-expired": 1,
    "seedance.retrieve cgt-fixture-failed": 1,
    "seedance.delete cgt-fixture-queued": 1,
  });
  const wire = (await (await fetch(`${base}/__wire`)).json()).data[0].calls;
  assert.deepEqual(wire.map((c) => c.path), ["/pp/byteplus/api/v3/contents/generations/tasks"]);
});
