# External OAuth for remote MCP clients

This source tree adds an **optional OAuth resource server** to `/mcp`. The last
published release, v0.2.1, has only static bearer authentication. Build this source
or use a future release that includes these changes. No external OAuth provider or
Codex/Claude/ChatGPT OAuth login has been acceptance-tested by this change.

The design follows the resource-server side of the
[MCP 2026-07-28 authorization specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization).
HostMCP advertises an external authorization server; that provider handles client
registration, consent, authorization code + PKCE, token issuance and refresh. There
is no built-in identity provider, password endpoint or redirect handler.

## Provider requirements

Use an operator-managed OAuth 2.1 provider with all of the following:

- HTTPS RFC 8414 or OIDC issuer discovery, with an exactly matching `issuer`, plus
  HTTPS `jwks_uri`, `authorization_endpoint`, and `token_endpoint`. Redirects from
  discovery/JWKS endpoints are refused. HTTP is accepted only on loopback for tests.
- Public-client authorization code with S256 PKCE and exact redirect URI validation;
  use the provider's documented consent and registration controls. Prefer Client
  ID Metadata Documents (CIMD). Pre-registration is also possible; DCR is an optional
  legacy fallback controlled by the provider. HostMCP implements none of these
  registration mechanisms and cannot add them to an incompatible provider.
- Resource indicators in authorization AND token requests. Issue **JWT access
  tokens**, with `aud` containing the exact canonical resource URL, e.g.
  `https://mcp.example.com/mcp`. A client ID as the only audience is insufficient.
- RS256 with an RSA key of at least 2048 bits, or ES256 with P-256; a nonempty,
  unique `kid` in the JWT and public JWKS. No HS256, `none`, token-supplied key URL,
  opaque token introspection or ID-token login is provided.
- Exact `iss`, nonempty `sub`, mandatory `exp`, and a nonempty `client_id` (or `azp`)
  identifying the OAuth client. If both client claims exist they must agree.
  `nbf` and `iat`, when present, must not be in the future. Keep clocks synchronized;
  there is no configured clock-skew allowance. `scope` is a space-separated string;
  `scp` may additionally be an array of strings.

Ensure the provider restricts who may obtain this resource and scope. Configure
access tokens for the resource, rather than passing ID tokens or third-party API
tokens. The server validates issuer, audience, allowed algorithm, signature and
expiry on every request; discovery or unusable keys prevent Hub startup. Keys are
cached for at most five minutes; discovery failure after that fails closed. Unknown
key IDs may trigger refresh at most once every 30 seconds. Publish new keys before
issuing tokens with them; retire old keys with the cache interval in mind.

## Configure an existing Hub

As the Hub configuration owner, write a private policy file, for example:

```sh
umask 077
cat > oauth.json <<'JSON'
{
  "issuer": "https://identity.example.com/tenant",
  "scopes": ["hostmcp:tools"],
  "principals": [
    {"subject": "provider-stable-user-subject", "devices": ["mac-worker", "linux-worker"]}
  ]
}
JSON
hostmcp-hub hub configure --config /absolute/path/hub.json --oauth-file oauth.json
```

The compatibility binary `hostmcp` supports the same commands. This writes the
optional `oauth` object into the existing format-v1 Hub configuration. Existing
client hashes, node credentials, invitations and node bindings are retained. Node
Agent configuration does not change. Restart only the Hub after enabling OAuth or
changing issuer, public URL or required scopes. Discovery settings and public
metadata are fixed for a running Hub. Settings changes reject OAuth requests until
restart; subject/node mapping changes and removals take effect on new requests and
at tool dispatch without restarting agents.

Each subject must be explicitly mapped to named node IDs; `*` is forbidden in OAuth
mappings. A token with `owner`, `admin`, or `*` scope cannot grant any nodes. An OAuth
subject named `owner` does not inherit the legacy owner token's access. Sessions
are separated from legacy tokens, other subjects, and other OAuth client IDs.
Refreshes for the same subject/client retain session ownership. Separate clients
using the same registered client ID and subject share that identity. Node grants
are account-level authority, including root only if its node ID is explicitly
listed. Treat the issuer and its administrators as trusted identity authorities.

The public resource is derived ONLY from the configured `public_url` plus `/mcp`,
never from Host or forwarded headers. Expose these paths through the HTTPS proxy:

```text
/mcp
/.well-known/oauth-protected-resource/mcp
/.well-known/oauth-protected-resource
```

Both metadata paths describe the same resource and issuer. A request without valid
authorization returns 401 with `WWW-Authenticate: Bearer resource_metadata="...",
scope="hostmcp:tools"`. Valid tokens lacking a scope or explicit subject mapping
receive 403. Credentials are required on every MCP request, including discovery of
tools; query-string tokens and duplicate Authorization headers are rejected.
Metadata alone is public. Existing host/origin allowlists apply to metadata too;
explicitly allow a browser client's Origin if its metadata fetch requires that.
Forward Authorization unchanged; do not enable authentication-page redirects or
proxy caching on these paths. `/agent/` and `/enroll` retain their separate device
and invitation credentials. OAuth tokens cannot enroll or authenticate a node.

## Client setup and fallback

Point an OAuth-capable client's remote MCP configuration to the canonical `/mcp`
URL. It must follow protected-resource discovery, discover the issuer, register
using a mechanism supported by that provider/client pair, and send access tokens
with the resource audience on each HTTP request. Provider-specific client IDs,
redirect URI registration and consent must be configured using the provider's
instructions; a generic OIDC provider alone does not guarantee MCP client support.

Codex, Claude and ChatGPT versions and account policies differ. This implementation
is tested with the official Go MCP client and an isolated issuer fixture; it does
not establish product-specific OAuth compatibility. Clients unable to complete the
provider's flow may use a pre-issued legacy client bearer or the existing local
`hostmcp connect --token-file ...` stdio bridge. That CLI does not perform browser
login or refresh OAuth tokens. STDIO uses local process/ENV/file credentials and
has no OAuth transport handshake. Never use an Agent credential for MCP access.

Disable OAuth with `hostmcp-hub hub configure --config ... --disable-oauth` and
restart the Hub. This leaves legacy clients intact. To roll back to an older Hub
binary that rejects unknown config fields, first remove the optional OAuth block
with this version's CLI. Agents need no upgrade or re-enrollment for either action.

## Isolated local checks

Use a new temporary directory and unused loopback port; do not use service configs:

```sh
task_dir=$(mktemp -d)
chmod 700 "$task_dir"
hostmcp hub init --config "$task_dir/hub.json" --listen 127.0.0.1:18787 \
  --public-url http://127.0.0.1:18787
# Configure a local test issuer policy with the issuer's loopback URL and real
# ephemeral subject. Its access-token aud must be http://127.0.0.1:18787/mcp.
hostmcp hub configure --config "$task_dir/hub.json" --oauth-file /path/to/private-test-oauth.json
hostmcp hub serve --config "$task_dir/hub.json"
# In another shell:
curl -i http://127.0.0.1:18787/mcp
curl -fsS http://127.0.0.1:18787/.well-known/oauth-protected-resource/mcp
```

The first response should be 401 with the challenge; the second should identify
this exact loopback resource and test issuer. Do not put tokens on the command
line or in URLs. For deterministic tests requiring no external issuer or listener:

```sh
go test ./internal/oauth ./internal/hub -run '^TestOAuth' -count=1
go test ./internal/hub ./internal/agent -run '^TestCompatibility' -count=1
```

These tests sign ephemeral JWTs, drive the official MCP client through in-memory
HTTP, and test websocket wire-v1 traffic over `net.Pipe`. They include bad
signatures, algorithms, issuer/audience, expired/missing expiry, key rotation,
failed discovery, metadata challenges, legacy bearer, explicit node grants,
Origin checks, agent-channel rejection and mixed-generation handshakes. They do
not contact a deployed Hub, an installed application or a real identity account.
