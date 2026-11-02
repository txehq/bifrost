# Bifrost acceptance adapter

## 1. Product and repository layout

Bifrost exposes native and provider-compatible inference APIs together with an admin API. The gateway entrypoint is `transports/bifrost-http`, the handlers are in its `handlers` package, provider drivers are in `core/providers`, and configuration storage is in `framework/configstore`. The dashboard is built from `ui` and embedded into the gateway.

## 2. Environment

Use the Go version in `go.work` and Node version in `.nvmrc`. Build dashboard assets with `npm ci` and `npm run build` in `ui`, then build the gateway from the repository root with `go build -o .acceptances/bifrost-http ./transports/bifrost-http`. Keep `.acceptances` ignored with its own `.gitignore` containing `*` and set `TMPDIR` to a dedicated directory inside the worktree.

Start the built gateway with `-app-dir`, `-host 127.0.0.1`, and `-port`, choosing an unused task-owned port. Check listeners before starting and preserve other running instances. The app directory owns `config.json` and its SQLite configuration store. A standalone API acceptance can use SQLite and task-owned loopback upstream endpoints. Configure pricing, model-parameter and MCP catalogs with local `file://` URLs or an owned HTTP endpoint to keep the run independent of public feeds. Disable request/content logging for audio probes. Stop only the process this run started with SIGTERM and wait for it to exit.

## 3. Auth

Use an isolated app directory. On a fresh local instance with no admin account, the admin API is reachable for initial configuration. Environments with auth configured require an authenticated admin session; verify `/api/config` before capturing admin evidence. Provider keys belong to the task's local endpoints during free API acceptance. Keep credentials and secret values out of published evidence.

## 4. Surfaces

Use CLI evidence for gateway APIs and provider routing: capture actual request status, response headers/body, persisted readback and owned upstream observations. Use the dashboard only for changes to its rendered behavior. Open the completed HTML acceptance report in a browser and inspect its layout before publication.

## 5. Project probes and quick navigation

`GET /health` checks the running process. Admin provider routes are `/api/providers`, `/api/providers/{provider}` and `/api/providers/{provider}/keys`. Single-provider GET reports `supported_base_providers` for custom-driver capability discovery. Audio routes are `/v1/audio/speech` and `/v1/audio/transcriptions`; streamed speech uses `stream_format: "sse"`. Routed identity is visible in `x-bifrost-provider` and `x-bifrost-routing-info-key` response headers. Verify persistence by restarting the built process against the same app directory.

## 6. Known constraints

Live provider-harness execution is reserved for the user because it calls paid providers. Add and structurally validate provider-harness cases during development, and provide the exact scoped user-run command. Free owned-endpoint acceptance exercises real handlers, storage, provider construction and network dispatch. It establishes gateway protocol behavior; paid/staging comparisons and deployment identity retain their separate gates. Each Go module has its own dependency file; run module operations within the relevant module and keep the repository workspace enabled for local cross-module builds.
