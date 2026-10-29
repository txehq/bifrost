## 🔒 Security

- **Proxied requests are pinned to the address the policy checked** - guarded HTTP clients (catalog and datasheet fetches, OpenAPI spec previews, RSS feeds) and MCP clients that reach their destination through a proxy now open a tunnel to the address the destination policy resolved and approved (CONNECT ip:port, or SOCKS5 to the IP, plain http included) instead of handing the proxy the hostname to resolve again. A DNS answer that changes after the check (rebinding) can no longer carry the request to a private, loopback or metadata address. The proxy's credentials, TLS settings and private-network reachability are unchanged
- **Unresolvable proxied destinations are refused** - a hostname the gateway cannot resolve is refused on every proxied guarded path instead of being left to the proxy, as MCP clients already were: nothing could check where the proxy would send it. A deployment whose DNS lives only behind the proxy must let the gateway resolve the catalog, datasheet, OpenAPI and MCP hosts it reaches through that proxy
- **Proxied MCP OAuth requests are pinned to the checked address** - MCP OAuth discovery, dynamic client registration and token requests (every hop, redirects included, of a chain whose later URLs come from the remote MCP server) that go through a proxy now open a tunnel to the address the destination policy approved, under both the public-only and the allowlisted policy, instead of handing the proxy the hostname to resolve again. A direct OAuth request whose destination happens to be a proxy's address is no longer dialed outside the destination policy
- **The catalog URL check is pinned behind a proxy** - the reachability check for a pricing, model-parameters or MCP-library URL saved over the API applies the public-only policy to the destination before the proxy is contacted and tunnels to the checked address, so a name resolving to loopback or turning private after the save-time check is refused, and the check no longer keeps an idle connection open after it returns
- **Agent credential references are stored without their resolved values** - an agent registration's discovery or runtime header, OAuth client credential or TLS material that references an environment variable or vault secret is written to the database as the reference only, whatever the encryption setting; the resolved value used to be stored next to it in plaintext, because encryption covers literals only. Loading resolves the reference again. A row saved by an earlier version keeps its stored value until the registration is next saved
- **Provider extra headers need real auth to reference server secrets** - while dashboard auth is disabled or unconfigured, creating or updating a provider refuses an extra header that introduces or repoints an `env.` or `vault.` reference (403), since its resolved value is sent to the provider on every request. Unchanged references, removed headers and literal values are still accepted
- **The setup lock accepts scoped temp tokens only for their own route** - while the OSS setup lock is active, an MCP consent or per-user auth temp token authorizes exactly its method, path and resource, as it does once dashboard auth is on, without running as the admin; the flows no longer fail with 401 mid-consent
- **The outbound client factory starts on the reconciled proxy** - the proxy from config.json is applied to the stored global proxy before the client factory for webhooks, MCP, OAuth, catalog sync and other non-inference traffic is built, so that traffic no longer runs on the previous or absent proxy until the next dashboard edit
- **Agent card discovery keeps a registration's public-target restriction** - live card discovery for a registration created without authentication applies its persisted public-only restriction, as registration and runtime calls do, so an operator's private-network allowlist no longer lets it fetch a card from a private address

## 🐞 Fixed

- A proxy changed from one URL to another never reuses a tunnel through the old proxy; guarded HTTP transports close idle connections after 90 seconds
- MCP verification and instruction-refresh handshakes for an SSE client use SSE, so an SSE-only server no longer answers 405 and an edit no longer keeps stale instructions
- Image and document URL fetches through the environment proxy choose `HTTPS_PROXY` or `HTTP_PROXY` by the request's scheme rather than its port, and the provider auth and fetch clients keep each target's scheme only while a request needs it instead of for the life of the process
- A stream attempt whose connection stalls in DNS, dialing or the proxy handshake ends at its first-token deadline and moves to its fallback on time
- With config.json as the source of truth, an unchanged agent declaration is written again on every load, so an API edit to that agent is reverted as for other sections; split mode keeps the edit until the file changes
- While dashboard auth is disabled, an agent update that resends the stored credential origins unchanged is accepted; only a change to them needs dashboard auth
- `extra_headers` values in config.json and the Helm chart's `networkConfig` may be structured secret references (`{"type": "env", "ref": "env.VAR_NAME"}`), which the gateway already accepted

## 🗄️ Database Migrations

- No new database migrations in this release.

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.3.3-sync.3 is a prerelease on top of v2.3.2 carrying the network and destination-policy fixes from the PR5 preservation audit (TXE-5374, in v2.3.3-sync.1), the proxied MCP OAuth and catalog URL check pinning (TXE-5377, in v2.3.3-sync.2) and the PR5 credential-reference and destination-change fixes (TXE-5411); v2.3.2 stays the stable release and `latest`
- note: module versions. This fork publishes no core, framework or plugin module versions: their module paths stay `github.com/capsohq/bifrost/...`, their version files and the transport's requires are unchanged, and the workspace builds them from source
- preserved: everything v2.3.2 listed under Fork changes
- chore: the Helm chart is 2.1.49-sync.3, a prerelease; `appVersion` is 2.3.3-sync.3, `image.tag` stays required, and `networkConfig.extra_headers` values may be structured secret references
- note: the CLI changes (every stored credential of a context is cleared when its gateway URL changes) ship with the next CLI release, not with this transport
