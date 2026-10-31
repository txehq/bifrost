// Tests provider-error-fixture.mjs through a running instance. Run directly:
// `node provider-error-fixture.test.mjs`.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import net from "node:net";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const FIXTURE = join(dirname(fileURLToPath(import.meta.url)), "..", "provider-error-fixture.mjs");

// startFixture boots the fixture on a random port, its stall proxy on the next one, and resolves once it
// listens. The returned post sends one chat completion and, when the fixture does not answer, fails with
// what it printed on its way out; post.port and post.stallPort name the two listeners.
async function startFixture(t) {
  const port = 20000 + Math.floor(Math.random() * 20000);
  const child = spawn(process.execPath, [FIXTURE], {
    env: { ...process.env, PROVIDER_ERROR_FIXTURE_PORT: String(port), PROVIDER_ERROR_FIXTURE_STALL_PORT: String(port + 1) },
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
