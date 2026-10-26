#!/usr/bin/env node
// Offline stand-in for provider list-models endpoints, used by
// test-api-integrations.sh when a provider's API key secret is not set.
//
// The api-management collection's "Governance - VK List Models Restriction"
// folder asserts that GET /v1/models through a virtual key returns models from
// the VK's allowed providers and none from the others. That needs at least one
// provider whose list-models call succeeds. Without credentials every provider
// fails key selection, so the gateway has nothing to list (TXE-4950).
//
// Each stubbed provider's base_url is pointed at http://127.0.0.1:<port>/<provider>,
// so the provider's real client code issues GET /<provider>/v1/models here and
// parses an OpenAI-format list.
//
// The governance suites run in the same job also send chat completions through
// openai (the time-of-day pricing collection prices the logged token counts), so
// POST /<provider>/v1/chat/completions answers a fixed, non-streaming OpenAI-format
// completion with a usage block (TXE-5026). A streaming request, and every other
// request, gets a 404.
//
// The vk-object-ownership collection uploads, reads, lists and deletes files
// through openai to check that Bifrost keeps each file to the virtual key that
// created it (TXE-5254). Bifrost enforces that; the stub only has to be a files
// API, so /openai/v1/files keeps uploads in memory in OpenAI's shapes.
//
// Usage: node list-models-stub.js <port>

const http = require("http");

const MODELS = {
  openai: [
    { id: "gpt-4o", object: "model", created: 1715367049, owned_by: "system" },
    { id: "gpt-4o-mini", object: "model", created: 1721172741, owned_by: "system" },
  ],
  mistral: [
    { id: "mistral-small-latest", object: "model", created: 1727740800, owned_by: "mistralai" },
  ],
  groq: [
    { id: "llama-3.1-8b-instant", object: "model", created: 1693721698, owned_by: "Meta" },
  ],
};

// Anthropic's list-models shape, which its provider client parses instead of
// OpenAI's. The api-management "VK Allow All Providers" folder (upstream #7375)
// needs a provider the VK grants by allow_all_providers alone, with no config of
// its own, to show up in the /v1/models fan-out; anthropic is that provider.
// Anthropic is stubbed for list-models only: it has no chat route here.
const ANTHROPIC_MODELS = [
  { id: "claude-haiku-4-5", type: "model", display_name: "Claude Haiku 4.5", created_at: "2025-10-01T00:00:00Z" },
];

const port = Number(process.argv[2]);
if (!Number.isInteger(port) || port <= 0) {
  console.error("usage: node list-models-stub.js <port>");
  process.exit(2);
}

// Fixed token counts, so a priced log row has non-zero prompt and completion tokens.
const USAGE = { prompt_tokens: 9, completion_tokens: 3, total_tokens: 12 };

function notFound(res, req, path) {
  res.statusCode = 404;
  res.end(JSON.stringify({ error: { message: `list-models stub: no route for ${req.method} ${path}` } }));
}

// files holds the uploads of /openai/v1/files: id -> { meta, content }.
const files = new Map();
let fileSeq = 0;

function readBody(req, done) {
  const chunks = [];
  req.on("data", (chunk) => chunks.push(chunk));
  req.on("end", () => done(Buffer.concat(chunks)));
}

// multipartFields splits a multipart/form-data body into its named parts.
function multipartFields(req, body) {
  const m = /boundary=(?:"([^"]+)"|([^;]+))/i.exec(req.headers["content-type"] || "");
  const fields = {};
  if (!m) {
    return fields;
  }
  const boundary = Buffer.from(`--${m[1] || m[2]}`);
  let start = body.indexOf(boundary);
  while (start !== -1) {
    const next = body.indexOf(boundary, start + boundary.length);
    if (next === -1) {
      break;
    }
    const part = body.subarray(start + boundary.length + 2, next - 2);
    const split = part.indexOf("\r\n\r\n");
    if (split !== -1) {
      const head = part.subarray(0, split).toString("utf8");
      const name = /name="([^"]*)"/i.exec(head);
      const filename = /filename="([^"]*)"/i.exec(head);
      if (name) {
        fields[name[1]] = { value: part.subarray(split + 4), filename: filename ? filename[1] : undefined };
      }
    }
    start = next;
  }
  return fields;
}

function filesAPI(req, res, path) {
  const one = /^\/openai\/v1\/files\/([^/]+)(\/content)?\/?$/.exec(path);
  if (/^\/openai\/v1\/files\/?$/.test(path)) {
    if (req.method === "POST") {
      readBody(req, (body) => {
        const fields = multipartFields(req, body);
        if (!fields.file) {
          res.statusCode = 400;
          res.end(JSON.stringify({ error: { message: "list-models stub: upload has no file part" } }));
          return;
        }
        fileSeq += 1;
        const meta = {
          id: `file-stub-${fileSeq}`,
          object: "file",
          bytes: fields.file.value.length,
          created_at: Math.floor(Date.now() / 1000),
          filename: fields.file.filename || "upload",
          purpose: fields.purpose ? fields.purpose.value.toString("utf8") : "user_data",
          status: "processed",
        };
        files.set(meta.id, { meta, content: Buffer.from(fields.file.value) });
        res.end(JSON.stringify(meta));
      });
      return true;
    }
    if (req.method === "GET") {
      const data = [...files.values()].map((f) => f.meta).reverse();
      res.end(JSON.stringify({ object: "list", data, has_more: false, first_id: data[0]?.id, last_id: data[data.length - 1]?.id }));
      return true;
    }
    return false;
  }
  if (!one) {
    return false;
  }
  const entry = files.get(decodeURIComponent(one[1]));
  if (!entry) {
    res.statusCode = 404;
    res.end(JSON.stringify({ error: { message: `No such File object: ${one[1]}`, type: "invalid_request_error", code: null } }));
    return true;
  }
  if (one[2] && req.method === "GET") {
    res.setHeader("Content-Type", "application/octet-stream");
    res.end(entry.content);
    return true;
  }
  if (!one[2] && req.method === "GET") {
    res.end(JSON.stringify(entry.meta));
    return true;
  }
  if (!one[2] && req.method === "DELETE") {
    files.delete(entry.meta.id);
    res.end(JSON.stringify({ id: entry.meta.id, object: "file", deleted: true }));
    return true;
  }
  return false;
}

function chatCompletion(req, res, path) {
  let raw = "";
  req.setEncoding("utf8");
  req.on("data", (chunk) => {
    raw += chunk;
  });
  req.on("end", () => {
    let body;
    try {
      body = JSON.parse(raw);
    } catch (e) {
      res.statusCode = 400;
      res.end(JSON.stringify({ error: { message: "list-models stub: request body is not JSON" } }));
      return;
    }
    if (body.stream) {
      notFound(res, req, `${path} (stream)`);
      return;
    }
    res.end(
      JSON.stringify({
        id: `chatcmpl-stub-${Date.now()}`,
        object: "chat.completion",
        created: Math.floor(Date.now() / 1000),
        model: typeof body.model === "string" ? body.model : "stub",
        choices: [{ index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" }],
        usage: USAGE,
      }),
    );
  });
}

const server = http.createServer((req, res) => {
  const path = (req.url || "").split("?")[0];
  res.setHeader("Content-Type", "application/json");
  if (path.startsWith("/openai/v1/files") && filesAPI(req, res, path)) {
    return;
  }
  const chat = /^\/([a-z0-9-]+)\/v1\/chat\/completions\/?$/.exec(path);
  if (chat && req.method === "POST" && MODELS[chat[1]]) {
    chatCompletion(req, res, path);
    return;
  }
  const match = /^\/([a-z0-9-]+)\/v1\/models\/?$/.exec(path);
  if (match && req.method === "GET" && match[1] === "anthropic") {
    const ids = ANTHROPIC_MODELS.map((m) => m.id);
    res.end(JSON.stringify({ data: ANTHROPIC_MODELS, has_more: false, first_id: ids[0], last_id: ids[ids.length - 1] }));
    return;
  }
  const models = match && req.method === "GET" ? MODELS[match[1]] : undefined;
  if (!models) {
    notFound(res, req, path);
    return;
  }
  res.end(JSON.stringify({ object: "list", data: models }));
});

server.listen(port, "127.0.0.1", () => {
  console.log(`list-models stub listening on 127.0.0.1:${port} (${Object.keys(MODELS).concat("anthropic").join(", ")})`);
});
