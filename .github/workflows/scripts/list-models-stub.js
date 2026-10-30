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
// The extra-fields-placement collection (upstream #8477) sends chat completions
// streamed and unary, Responses requests streamed and unary, and requests for
// MISSING_MODEL that must fail so Bifrost falls back. The stub answers chat with
// SSE chunks when asked to stream, serves /openai/v1/responses in OpenAI's
// Responses shapes, and returns OpenAI's model_not_found 404 for MISSING_MODEL
// (TXE-5329).
//
// The same collection checks that a virtual key cannot manage fine-tuning through
// /openai_passthrough (TXE-5023): tuning jobs and tuned models record no creator, so
// under the shared key one tenant could list or cancel another's. The stub serves a
// shared job at /openai/v1/fine_tuning/jobs (list, cancel) and a tuned model under
// /openai/v1/models/ft:..., so a request Bifrost wrongly forwards succeeds visibly.
// Like Google APIs, the stub honours X-HTTP-Method-Override (and X-HTTP-Method,
// X-Method-Override, with '_' read as '-'), so a forwarded override that turns a
// POST into a GET shows up as a list in the response.
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
const RESPONSES_USAGE = { input_tokens: 9, output_tokens: 3, total_tokens: 12 };

// The model the extra-fields-placement collection asks for when it needs a provider
// error (and, with a fallback configured, a fallback).
const MISSING_MODEL = "bifrost-e2e-missing-model";

function modelNotFound(res, model) {
  res.statusCode = 404;
  res.end(JSON.stringify({
    error: { message: `The model \`${model}\` does not exist or you do not have access to it.`, type: "invalid_request_error", param: null, code: "model_not_found" },
  }));
}

// readJSON reads a JSON request body; it answers 400 itself when the body is not JSON.
function readJSON(req, res, done) {
  readBody(req, (raw) => {
    let body;
    try {
      body = JSON.parse(raw.toString("utf8"));
    } catch (e) {
      res.statusCode = 400;
      res.end(JSON.stringify({ error: { message: "list-models stub: request body is not JSON" } }));
      return;
    }
    done(body);
  });
}

function sse(res) {
  res.setHeader("Content-Type", "text/event-stream");
  res.setHeader("Cache-Control", "no-cache");
}

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
  readJSON(req, res, (body) => {
    const model = typeof body.model === "string" ? body.model : "stub";
    if (model === MISSING_MODEL) {
      modelNotFound(res, model);
      return;
    }
    const id = `chatcmpl-stub-${Date.now()}`;
    const created = Math.floor(Date.now() / 1000);
    if (body.stream) {
      sse(res);
      const chunk = (choices, extra) => `data: ${JSON.stringify({ id, object: "chat.completion.chunk", created, model, choices, ...extra })}\n\n`;
      res.write(chunk([{ index: 0, delta: { role: "assistant", content: "" }, finish_reason: null }]));
      res.write(chunk([{ index: 0, delta: { content: "ok" }, finish_reason: null }]));
      res.write(chunk([{ index: 0, delta: {}, finish_reason: "stop" }]));
      res.write(chunk([], { usage: USAGE }));
      res.end("data: [DONE]\n\n");
      return;
    }
    res.end(
      JSON.stringify({
        id,
        object: "chat.completion",
        created,
        model,
        choices: [{ index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" }],
        usage: USAGE,
      }),
    );
  });
}

// responses serves OpenAI's Responses API: one assistant message whose text is "ok".
function responses(req, res) {
  readJSON(req, res, (body) => {
    const model = typeof body.model === "string" ? body.model : "stub";
    if (model === MISSING_MODEL) {
      modelNotFound(res, model);
      return;
    }
    const id = `resp_stub_${Date.now()}`;
    const createdAt = Math.floor(Date.now() / 1000);
    const text = { type: "output_text", text: "ok", annotations: [] };
    const message = { type: "message", id: `msg_stub_${Date.now()}`, status: "completed", role: "assistant", content: [text] };
    const response = (status, output, usage) => ({ id, object: "response", created_at: createdAt, status, model, output, ...(usage ? { usage } : {}) });
    if (!body.stream) {
      res.end(JSON.stringify(response("completed", [message], RESPONSES_USAGE)));
      return;
    }
    sse(res);
    let seq = 0;
    const send = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, sequence_number: seq++, ...data })}\n\n`);
    send("response.created", { response: response("in_progress", []) });
    send("response.in_progress", { response: response("in_progress", []) });
    send("response.output_item.added", { output_index: 0, item: { ...message, status: "in_progress", content: [] } });
    send("response.content_part.added", { item_id: message.id, output_index: 0, content_index: 0, part: { ...text, text: "" } });
    send("response.output_text.delta", { item_id: message.id, output_index: 0, content_index: 0, delta: "ok" });
    send("response.output_text.done", { item_id: message.id, output_index: 0, content_index: 0, text: "ok" });
    send("response.content_part.done", { item_id: message.id, output_index: 0, content_index: 0, part: text });
    send("response.output_item.done", { output_index: 0, item: message });
    send("response.completed", { response: response("completed", [message], RESPONSES_USAGE) });
    res.end();
  });
}

// methodOverride returns the method an override header asks for, as an upstream
// honouring one would act on it, or "" when there is none.
function methodOverride(req) {
  for (const [name, value] of Object.entries(req.headers)) {
    const folded = name.toLowerCase().replace(/_/g, "-");
    if ((folded === "x-http-method-override" || folded === "x-http-method" || folded === "x-method-override") && typeof value === "string" && value) {
      return value.toUpperCase();
    }
  }
  return "";
}

// The shared fine-tuning job every tenant reaches under the one provider key.
const sharedJob = { id: "ftjob-stub-shared", object: "fine_tuning.job", model: "gpt-4o-mini-2024-07-18", status: "running", created_at: 1715367049 };

function fineTuningAPI(req, res, path) {
  if (/^\/openai\/v1\/fine_tuning\/jobs\/?$/.test(path) && req.method === "GET") {
    res.end(JSON.stringify({ object: "list", data: [sharedJob], has_more: false }));
    return true;
  }
  const cancel = /^\/openai\/v1\/fine_tuning\/jobs\/([^/]+)\/cancel\/?$/.exec(path);
  if (cancel && req.method === "POST") {
    res.end(JSON.stringify({ ...sharedJob, id: decodeURIComponent(cancel[1]), status: "cancelled" }));
    return true;
  }
  const tuned = /^\/openai\/v1\/models\/(ft:[^/]+)\/?$/.exec(path);
  if (tuned && (req.method === "GET" || req.method === "DELETE")) {
    const id = decodeURIComponent(tuned[1]);
    res.end(JSON.stringify(req.method === "GET" ? { id, object: "model", created: 1715367049, owned_by: "org-stub" } : { id, object: "model", deleted: true }));
    return true;
  }
  return false;
}

const server = http.createServer((req, res) => {
  const path = (req.url || "").split("?")[0];
  res.setHeader("Content-Type", "application/json");
  const override = methodOverride(req);
  if (override) {
    req.method = override;
  }
  if (fineTuningAPI(req, res, path)) {
    return;
  }
  if (path.startsWith("/openai/v1/files") && filesAPI(req, res, path)) {
    return;
  }
  if (/^\/openai\/v1\/responses\/?$/.test(path) && req.method === "POST") {
    responses(req, res);
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
