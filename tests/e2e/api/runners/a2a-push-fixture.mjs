#!/usr/bin/env node
// Offline A2A upstream for the provider harness's Agent Gateway rows (TXE-5477).
//
// Bifrost's Agent Gateway proxies A2A calls to a registered upstream agent and
// keeps its own record of each client's push callback. The harness has no
// upstream agent of its own, so this fixture is one: an agent card at
// /.well-known/agent-card.json and a strict-v1 JSON-RPC endpoint at /jsonrpc that
// answers message/send with a task, stores push configurations in memory
// (tasks/pushNotificationConfig/set, get, list, delete) and answers tasks/get.
//
// SendStreamingMessage is where the gateway's stream adaptation is pinned. The text of
// the sent message selects the reply:
//   "json-error-reply"      a small, valid JSON-RPC error as a plain 200
//                           application/json body (what the Python SDK sends),
//                           which the gateway reframes as one SSE event;
//   "malformed-json-reply"  a 200 application/json body that is not JSON;
//   "oversized-json-reply"  a 200 application/json body larger than the 1 MiB
//                           the gateway will reframe;
//   anything else           a real SSE stream with a completed task.
// The last two must surface from the gateway as errors, never as an empty or
// truncated successful stream.
//
// Delivery rows use two more endpoints. The fixture is also the client's push
// callback: POST /push-sink/<run> records what the gateway delivers (the
// A2A-Notification-Token it presents, the event, the order), answers 500 once
// per POST /push-sink/<run>/fail-next so a delivery backs off, and
// GET /push-sink/<run>/deliveries?min=N&timeoutMs=T waits for N deliveries.
// POST /control/push {taskId, configId, event, repeat} makes the fixture push
// an event to the gateway's ingress with the token the gateway registered,
// exactly as an upstream agent would, and returns the ingress status codes.
//
// Usage: node a2a-push-fixture.mjs [port]   (default 18091, bound to 127.0.0.1)
// Nothing here is a credential; the push tokens the gateway registers are stored
// only in memory for the life of the process.
import http from "node:http";
import { randomUUID } from "node:crypto";

const port = Number(process.argv[2] || 18091);
const host = "127.0.0.1";
const base = `http://${host}:${port}`;

const card = {
  name: "Harness push fixture",
  description: "Offline A2A agent for the provider harness Agent Gateway rows",
  version: "1.0.0",
  supportedInterfaces: [{ url: `${base}/jsonrpc`, protocolBinding: "JSONRPC", protocolVersion: "1.0" }],
  defaultInputModes: ["text/plain"],
  defaultOutputModes: ["text/plain"],
  capabilities: { streaming: true, pushNotifications: true },
  skills: [{ id: "push-fixture", name: "Push fixture", description: "Creates tasks and stores push configurations", tags: ["fixture"] }],
};

const tasks = new Map(); // task id -> task
const pushConfigs = new Map(); // `${taskId}\u0000${configId}` -> config

function textOf(message) {
  const parts = (message && message.parts) || [];
  for (const part of parts) {
    if (typeof part.text === "string") return part.text;
  }
  return "";
}

function newTask(message, state) {
  const task = {
    id: randomUUID(),
    contextId: (message && message.contextId) || randomUUID(),
    status: { state },
    history: message ? [message] : [],
  };
  tasks.set(task.id, task);
  return task;
}

function reply(res, id, result) {
  const body = JSON.stringify({ jsonrpc: "2.0", id, result });
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(body);
}

function fail(res, id, code, message) {
  const body = JSON.stringify({ jsonrpc: "2.0", id, error: { code, message } });
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(body);
}

function requireTask(res, id, taskId) {
  const task = tasks.get(String(taskId || ""));
  if (!task) fail(res, id, -32001, "task not found");
  return task;
}

function handleRPC(req, res, body) {
  let rpc;
  try {
    rpc = JSON.parse(body);
  } catch {
    return fail(res, null, -32700, "parse error");
  }
  const { id, method, params = {} } = rpc;
  switch (method) {
    case "SendMessage":
    case "message/send": {
      const text = textOf(params.message);
      const task = newTask(params.message, text.startsWith("complete") ? "TASK_STATE_COMPLETED" : "TASK_STATE_INPUT_REQUIRED");
      return reply(res, id, { task });
    }
    case "SendStreamingMessage":
    case "message/stream": {
      const text = textOf(params.message);
      if (text === "json-error-reply") {
        return fail(res, id, -32603, "fixture-error: the agent answered the stream with a plain JSON error");
      }
      if (text === "malformed-json-reply") {
        res.writeHead(200, { "Content-Type": "application/json" });
        return res.end('{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"fixture-malformed');
      }
      if (text === "oversized-json-reply") {
        const filler = "x".repeat((1 << 20) + 64);
        res.writeHead(200, { "Content-Type": "application/json" });
        return res.end(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"fixture-oversized ${filler}"}}`);
      }
      const task = newTask(params.message, "TASK_STATE_COMPLETED");
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
      res.write(`data: ${JSON.stringify({ jsonrpc: "2.0", id, result: { task } })}\n\n`);
      return res.end();
    }
    case "GetTask":
    case "tasks/get": {
      const task = requireTask(res, id, params.id);
      if (task) reply(res, id, { task });
      return;
    }
    case "CreateTaskPushNotificationConfig":
    case "tasks/pushNotificationConfig/set": {
      if (!requireTask(res, id, params.taskId)) return;
      const config = {
        taskId: String(params.taskId),
        id: params.id || randomUUID(),
        url: params.url,
        token: params.token,
        tenant: params.tenant,
        authentication: params.authentication,
      };
      pushConfigs.set(`${config.taskId}\u0000${config.id}`, config);
      return reply(res, id, config);
    }
    case "GetTaskPushNotificationConfig":
    case "tasks/pushNotificationConfig/get": {
      if (!requireTask(res, id, params.taskId)) return;
      const config = pushConfigs.get(`${params.taskId}\u0000${params.id}`);
      if (!config) return fail(res, id, -32602, "invalid params: push config not found");
      return reply(res, id, config);
    }
    case "ListTaskPushNotificationConfigs":
    case "tasks/pushNotificationConfig/list": {
      if (!requireTask(res, id, params.taskId)) return;
      const configs = [];
      for (const [key, config] of pushConfigs) {
        if (key.startsWith(`${params.taskId}\u0000`)) configs.push(config);
      }
      return reply(res, id, { configs });
    }
    case "DeleteTaskPushNotificationConfig":
    case "tasks/pushNotificationConfig/delete": {
      if (!requireTask(res, id, params.taskId)) return;
      pushConfigs.delete(`${params.taskId}\u0000${params.id}`);
      return reply(res, id, null);
    }
    default:
      return fail(res, id, -32601, `method not found: ${method}`);
  }
}

// --- push sink and ingress control -----------------------------------------
const sinks = new Map(); // run -> { deliveries: [], failures: [], failNext: 0, waiters: [] }
function sinkOf(run) {
  if (!sinks.has(run)) sinks.set(run, { deliveries: [], failures: [], failNext: 0, waiters: [] });
  return sinks.get(run);
}
function describeEvent(body) {
  try {
    const sr = JSON.parse(body);
    if (sr.artifactUpdate) {
      const parts = (sr.artifactUpdate.artifact && sr.artifactUpdate.artifact.parts) || [];
      return { kind: "artifactUpdate", append: sr.artifactUpdate.append === true, text: parts.map((p) => p.text || "").join("") };
    }
    if (sr.statusUpdate) return { kind: "statusUpdate", state: sr.statusUpdate.status && sr.statusUpdate.status.state };
    if (sr.task) return { kind: "task", id: sr.task.id };
    if (sr.message) return { kind: "message", id: sr.message.messageId };
    return { kind: "unknown" };
  } catch {
    return { kind: "invalid" };
  }
}
function notifyWaiters(sink) {
  for (const w of [...sink.waiters]) {
    if (sink.deliveries.length >= w.min) {
      sink.waiters.splice(sink.waiters.indexOf(w), 1);
      clearTimeout(w.timer);
      w.resolve();
    }
  }
}
function json(res, code, value) {
  res.writeHead(code, { "Content-Type": "application/json" });
  res.end(JSON.stringify(value));
}
function readBody(req) {
  return new Promise((resolve) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
  });
}
async function handleSink(req, res, run, rest) {
  const sink = sinkOf(run);
  if (req.method === "POST" && rest === "") {
    const body = await readBody(req);
    const token = req.headers["a2a-notification-token"] || "";
    const entry = { order: sink.deliveries.length + sink.failures.length + 1, token, event: describeEvent(body), contentType: req.headers["content-type"] || "" };
    if (sink.failNext > 0) {
      sink.failNext--;
      sink.failures.push(entry);
      return json(res, 500, { error: "sink asked to fail this delivery" });
    }
    sink.deliveries.push(entry);
    notifyWaiters(sink);
    res.writeHead(200);
    return res.end();
  }
  if (req.method === "POST" && rest === "/fail-next") {
    sink.failNext++;
    return json(res, 200, { failNext: sink.failNext });
  }
  if (req.method === "GET" && rest.startsWith("/deliveries")) {
    const q = new URL(req.url, base).searchParams;
    const min = Number(q.get("min") || 0);
    const timeoutMs = Math.min(Number(q.get("timeoutMs") || 0), 60000);
    const reply = () => json(res, 200, { deliveries: sink.deliveries, failures: sink.failures });
    if (sink.deliveries.length >= min || timeoutMs <= 0) return reply();
    const w = { min, resolve: reply, timer: null };
    w.timer = setTimeout(() => { sink.waiters.splice(sink.waiters.indexOf(w), 1); reply(); }, timeoutMs);
    sink.waiters.push(w);
    return;
  }
  res.writeHead(404);
  res.end();
}
// POST /control/push: push one event (repeat times) to the gateway's ingress with the
// registered token, as the upstream agent would after accepting the configuration.
async function handleControlPush(req, res) {
  const body = await readBody(req);
  let cmd;
  try { cmd = JSON.parse(body); } catch { return json(res, 400, { error: "invalid JSON" }); }
  const config = pushConfigs.get(`${cmd.taskId}\u0000${cmd.configId}`);
  if (!config) return json(res, 404, { error: "no push config registered for that task and id" });
  const statuses = [];
  const payload = JSON.stringify(cmd.event);
  for (let i = 0; i < (cmd.repeat || 1); i++) {
    try {
      const r = await fetch(config.url, { method: "POST", headers: { "Content-Type": "application/json", "A2A-Notification-Token": config.token || "" }, body: payload });
      statuses.push(r.status);
      await r.text();
    } catch (e) {
      statuses.push(`error: ${e.message}`);
    }
  }
  return json(res, 200, { ingressUrl: config.url, statuses });
}

const server = http.createServer((req, res) => {
  const sinkMatch = /^\/push-sink\/([A-Za-z0-9._-]+)(.*)$/.exec(req.url.split("?")[0]);
  if (sinkMatch) {
    handleSink(req, res, sinkMatch[1], sinkMatch[2]).catch((e) => json(res, 500, { error: e.message }));
    return;
  }
  if (req.method === "POST" && req.url === "/control/push") {
    handleControlPush(req, res).catch((e) => json(res, 500, { error: e.message }));
    return;
  }
  if (req.method === "GET" && req.url === "/.well-known/agent-card.json") {
    res.writeHead(200, { "Content-Type": "application/json" });
    return res.end(JSON.stringify(card));
  }
  if (req.method === "POST" && req.url === "/jsonrpc") {
    const chunks = [];
    req.on("data", (chunk) => chunks.push(chunk));
    req.on("end", () => handleRPC(req, res, Buffer.concat(chunks).toString("utf8")));
    return;
  }
  res.writeHead(404, { "Content-Type": "text/plain" });
  res.end("not found");
});

server.listen(port, host, () => {
  console.log(`a2a-push-fixture listening on ${base} (card ${base}/.well-known/agent-card.json)`);
});
