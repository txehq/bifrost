## 🔒 Security

- **Proxied requests are pinned to the address the policy checked** - guarded HTTP clients (catalog and datasheet fetches, OpenAPI spec previews, RSS feeds) and MCP clients that reach their destination through a proxy now open a tunnel to the address the destination policy resolved and approved (CONNECT ip:port, or SOCKS5 to the IP, plain http included) instead of handing the proxy the hostname to resolve again. A DNS answer that changes after the check (rebinding) can no longer carry the request to a private, loopback or metadata address. The proxy's credentials, TLS settings and private-network reachability are unchanged
- **Unresolvable proxied destinations are refused** - a hostname the gateway cannot resolve is refused on every proxied guarded path instead of being left to the proxy, as MCP clients already were: nothing could check where the proxy would send it. A deployment whose DNS lives only behind the proxy must let the gateway resolve the catalog, datasheet, OpenAPI and MCP hosts it reaches through that proxy
- **Agent card discovery keeps a registration's public-target restriction** - live card discovery for a registration created without authentication applies its persisted public-only restriction, as registration and runtime calls do, so an operator's private-network allowlist no longer lets it fetch a card from a private address

## 🐞 Fixed

- A proxy changed from one URL to another never reuses a tunnel through the old proxy; guarded HTTP transports close idle connections after 90 seconds
- MCP verification and instruction-refresh handshakes for an SSE client use SSE, so an SSE-only server no longer answers 405 and an edit no longer keeps stale instructions
- Image and document URL fetches through the environment proxy choose `HTTPS_PROXY` or `HTTP_PROXY` by the request's scheme rather than its port, and the provider auth and fetch clients keep each target's scheme only while a request needs it instead of for the life of the process
- A stream attempt whose connection stalls in DNS, dialing or the proxy handshake ends at its first-token deadline and moves to its fallback on time

## 🗄️ Database Migrations

- No new database migrations in this release.

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.3.3-sync.1 is a prerelease on top of v2.3.2 carrying the network and destination-policy fixes from the PR5 preservation audit (TXE-5374); v2.3.2 stays the stable release and `latest`
- note: module versions. This fork publishes no core, framework or plugin module versions: their module paths stay `github.com/capsohq/bifrost/...`, their version files and the transport's requires are unchanged, and the workspace builds them from source
- preserved: everything v2.3.2 listed under Fork changes
- chore: the Helm chart is 2.1.49-sync.1, a prerelease; `appVersion` is 2.3.3-sync.1 and `image.tag` stays required
