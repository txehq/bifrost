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
// message/stream is where the gateway's stream adaptation is pinned. The text of
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
    case "message/send": {
      const text = textOf(params.message);
      const task = newTask(params.message, text.startsWith("complete") ? "TASK_STATE_COMPLETED" : "TASK_STATE_INPUT_REQUIRED");
      return reply(res, id, { task });
    }
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
    case "tasks/get": {
      const task = requireTask(res, id, params.id);
      if (task) reply(res, id, { task });
      return;
    }
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
    case "tasks/pushNotificationConfig/get": {
      if (!requireTask(res, id, params.taskId)) return;
      const config = pushConfigs.get(`${params.taskId}\u0000${params.id}`);
      if (!config) return fail(res, id, -32602, "invalid params: push config not found");
      return reply(res, id, config);
    }
    case "tasks/pushNotificationConfig/list": {
      if (!requireTask(res, id, params.taskId)) return;
      const configs = [];
      for (const [key, config] of pushConfigs) {
        if (key.startsWith(`${params.taskId}\u0000`)) configs.push(config);
      }
      return reply(res, id, { configs });
    }
    case "tasks/pushNotificationConfig/delete": {
      if (!requireTask(res, id, params.taskId)) return;
      pushConfigs.delete(`${params.taskId}\u0000${params.id}`);
      return reply(res, id, null);
    }
    default:
      return fail(res, id, -32601, `method not found: ${method}`);
  }
}

const server = http.createServer((req, res) => {
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
