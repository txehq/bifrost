## 🔧 Changed

- **Transports Go module is now `/v2`** - The transports module path is `github.com/capsohq/bifrost/transports/v2`, matching its 2.x version, so Go's semantic import versioning can resolve a `transports/v2.x.y` tag. Code that imports `bifrost-http` packages changes `github.com/capsohq/bifrost/transports/bifrost-http/...` to `github.com/capsohq/bifrost/transports/v2/bifrost-http/...`. Go imports only: the HTTP API, its `/v1` routes and the binary are unchanged (#8450)
- **Module path lint** - A workflow lint step fails any released module whose version major is 2 or higher unless its path ends in `/vN`, and any module below v2 that carries a `/vN` suffix (#8450)

## 🧪 Tests

- Collection script tests pin that the request-guard cases the OSS setup lock must refuse send no setup token and expect the lock's 401 (e2e-api-fixes; the fork already carried the rest of that commit)

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.3.1-sync.1 is a prerelease: v2.3.0 merged with upstream's v2.3.1 development line up to `acfb69f69` (the `/v2` module path change and the two commits before it), not a release of upstream v2.3.1. It is not tagged `latest`; v2.3.0 stays the stable release
- breaking (Go embedders only): import `github.com/capsohq/bifrost/transports/v2/...` instead of `github.com/capsohq/bifrost/transports/...`. The core, framework and plugin module paths and versions are unchanged, and no new core, framework or plugin version is published
- preserved: the fork's module paths, module versions, custom providers, native Anthropic and xAI APIs (the xAI native Messages passthrough still forwards the body byte-for-byte apart from the provider-prefix model rewrite, TXE-5193), streaming and unary clients, setup-token lock, release guards and every TXE fix through v2.3.0
- chore: the Helm chart is the prerelease 2.1.47-sync.1 with `appVersion` 2.3.1-sync.1; 2.1.46 stays the stable chart and `image.tag` stays required
