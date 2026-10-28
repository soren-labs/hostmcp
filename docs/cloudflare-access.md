# Cloudflare Access Managed OAuth for HostMCP Hub

HostMCP v0.2.3 adds an optional **Hub-only** backend identity mode for Cloudflare
Access. ChatGPT and other remote MCP clients connect to the public `/mcp` URL.
Access handles authorization code/PKCE, login and OAuth token issuance at the edge.
The Hub validates the signed `Cf-Access-Jwt-Assertion` forwarded to the origin.
Agent configuration, device credentials, native authority and wire version 1 do
not change. This source change has **not** acceptance-tested a real ChatGPT login,
Cloudflare application or deployed origin. No deployment is performed by these
instructions or the local tests.

[Cloudflare's Managed OAuth documentation](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/managed-oauth/)
requires MCP origins to validate this assertion. Managed OAuth issues an **opaque**
client bearer (for example, `oauth:...`); Access resolves it to an identity and
forwards the JWT. Do not configure the Access team as HostMCP's external `oauth`
issuer: that separate mode expects a JWT **access token**, resource-URL audience,
scopes and OAuth client identity. Access assertions use the Access application AUD.

## Policy file

As the existing Hub configuration owner, prepare a private policy file. Every
value below is an example/placeholder: replace the team, application AUD, email
and device IDs with the reviewed values for your installation. Device IDs denote
specific machine/OS-account nodes already registered with the Hub.

```sh
umask 077
cat > cloudflare-access.json <<'JSON'
{
  "team_domain": "https://example-team.cloudflareaccess.com",
  "audience": "REPLACE_WITH_EXACT_ACCESS_APPLICATION_AUD",
  "principals": [
    {"email": "operator@example.com", "devices": ["mac-worker", "linux-worker"]},
    {"email": "reviewer@example.com", "devices": ["linux-worker"]}
  ]
}
JSON
hostmcp-hub hub configure --config /absolute/path/hub.json \
  --cloudflare-access-file cloudflare-access.json
```

The compatibility binary `hostmcp` accepts the same command. The policy file
contains only the object shown above, with no wrapper or version field. The CLI
stores it under optional `cloudflare_access` in the existing Hub JSON. The config
format remains **1** (`"version": 1` in the file; `config_version` in CLI version
output). Existing client hashes, devices, invitations, host/origin allowlists and
the separate `oauth` block are preserved. No shared client secret is required.

`team_domain` must be exactly an HTTPS origin of the form
`https://TEAM.cloudflareaccess.com`, using a lowercase DNS team label, without
credentials, port, trailing slash, path, query or fragment. Arbitrary key endpoints,
HTTP and TLS bypasses are unavailable. `audience` must be the exact application's
AUD from Access, normally 64 hexadecimal characters. Other nonblank configured
strings are compared exactly too; no URL derivation, prefix matching, case folding
or wildcard interpretation is applied to the audience.

Emails are specific addresses, compared after case normalization. Display names,
wildcards, blank addresses and case-alias duplicate mappings are rejected. Each
mapping needs explicit device IDs: `*` is forbidden. An address or subject named
`owner` cannot inherit the legacy owner's privileges. An empty `principals` list
is a valid deny-all policy, useful for revoking everyone. A missing block leaves
legacy and external OAuth operation unchanged and makes no cert request.

`--cloudflare-access-file` and `--disable-cloudflare-access` conflict with each
other. They are independent of `--oauth-file` and `--disable-oauth`; both optional
modes can coexist with static client bearers. In particular, removing OAuth does
not remove Access, and removing Access does not alter OAuth or legacy credentials.

## Production connection procedure

1. Review and build/install the v0.2.3 Hub artifact using the existing release
   procedure. Back up the existing Hub config privately before configuring it.
   No Agent upgrade or re-enrollment is required.
2. In the intended Cloudflare account, review the self-hosted Access application's
   public hostname and `/mcp` path, identity policies and Managed OAuth setting.
   Use that application's AUD and the Zero Trust team's domain in the policy file.
   Restrict the Access policy to the intended people. Review the client's actual
   redirect URI against Managed OAuth registration settings; do not broadly allow
   redirect destinations to work around a login failure.
3. Apply the Hub policy as shown above, then deliberately restart **only the Hub**
   using your existing service procedure. Startup fetches and validates
   `https://TEAM.cloudflareaccess.com/cdn-cgi/access/certs`; unavailable, timed-out
   or malformed keys fail the entire enabled Hub startup, including legacy MCP
   access. Check service health before proceeding. These commands do not restart
   any service automatically.
4. Keep the origin reachable through the intended Tunnel/proxy and its normal
   host/origin allowlists. Forward the public Host and the Access assertion
   unchanged, preserve normal TLS validation and avoid logging authentication
   headers. Do not synthesize the assertion from an email header. Review direct
   origin exposure: a captured valid assertion is a bearer credential until its
   expiry. Cloudflare's edge must handle its Managed OAuth discovery and challenges;
   the Hub does not implement Access's authorization server or token exchange.
5. In the client's remote MCP configuration, use the public HTTPS MCP endpoint,
   for example `https://mcp.example.com/mcp`, and its OAuth login flow. The client
   receives the opaque bearer; the edge forwards a separately signed assertion.
   The origin accepts a valid assertion even if Authorization is absent or empty.
   Do not paste an opaque OAuth bearer into `--oauth-file` or provision it as a
   static Hub token.
6. Complete the actual ChatGPT login and call `list_devices`. Verify only the
   mapped devices appear, exercise a harmless allowed-node identity call, and
   verify an unmapped node is denied. Test logout/revocation and refresh through
   the real edge/client pair. **These ChatGPT/Cloudflare acceptance steps remain
   untested by this change.** Isolated SDK tests cannot establish that acceptance.

Protecting `/mcp` with Managed OAuth must not turn `/agent/` or `/enroll` into
interactive OAuth endpoints. Agents continue using outbound WebSockets and device
bearers; enrollment requires a valid one-use invitation. Neither endpoint trusts
Access assertions, OAuth client bearers or email headers. Preserve the deployment's
existing connectivity and separate credential mechanisms for these paths.

When external OAuth is also configured, its Hub metadata still describes that
external issuer. Access's edge-owned discovery is separate; review routing so the
chosen public login flow sees the intended metadata. HostMCP does not rewrite
external OAuth metadata to impersonate the Access authorization server.

## Verification and ongoing policy

Every assertion must be at most 16 KiB and pass RS256 verification against the
fetched JWKS `kid`, exact `iss=https://TEAM.cloudflareaccess.com`, and an `aud`
containing the exact configured application AUD. `exp` is mandatory and unexpired;
`nbf` and `iat`, if present, cannot be future-dated. There is no clock-skew allowance.
A specific email and a nonempty `sub` are required after signature verification.
Unsigned/forged JWTs, unsupported algorithms, unknown keys, duplicate JSON security
fields, malformed JWTs and duplicate assertion headers are rejected. Token-provided
key URLs or keys never supply verification material. Email headers and cookies
alone grant no authority.

Cert fetching uses normal HTTPS certificate verification, refuses redirects, has
a five-second HTTP timeout and caps the document at 512 KiB and 128 RSA keys. RSA
moduli must be 2048–8192 bits. Key cache lifetime is five minutes; refresh attempts
are serialized and rate-limited to once per 30 seconds. Unknown keys fail closed;
an expired cache cannot be used after failed refresh. Existing trusted keys remain
usable during their bounded lifetime if a refresh for another key fails. Changes
at Cloudflare may therefore take the cache lifetime to affect locally cached keys.

When Access is enabled, a supplied assertion is authoritative: invalid, empty,
duplicate or unmapped assertions are rejected even alongside a valid legacy or
external OAuth bearer. Without an assertion, the independent bearer modes still
work. When Access has never been enabled, incidental Access headers do not change
legacy behavior. Token query parameters and duplicate Authorization headers remain
rejected. Host and Origin checks apply before authentication.

After verification the Hub clones the request and supplies the SDK's required
`Authorization: Bearer access-assertion-validated` header, with the verified token
identity in context. This public string is **not a credential** and is rejected
when submitted by a client alone. The original request remains unchanged. Raw
assertions are removed from the downstream clone and no JWT, email or token is
included in Hub audit logs. Ensure upstream access logs have the same privacy rule.

Synthetic principals bind sessions to the Access team, application audience, Hub
resource, normalized email and verified subject. They are separate from legacy
and external OAuth principals and fit existing wire-v1 ownership semantics. Access
assertions do not identify individual OAuth client registrations; clients with the
same verified subject/email/application share that principal. Assertion refresh
preserves ownership, while a different subject or trust configuration does not.

Update the policy with the same configure command to change/revoke grants. The Hub
rereads mappings on every authentication and tool dispatch, including requests on
existing MCP connections. Removing an email or device denies subsequent dispatch;
it does not cancel an operation already dispatched. This is local policy refresh,
not online Access-policy introspection: an unexpired assertion alone cannot tell
the Hub that an edge identity/session was revoked. Apply Cloudflare's session
revocation procedures and remove the local mapping when immediate Hub denial is
needed. Changing team domain, audience or Hub public URL, or initially enabling
the mode, requires a Hub restart. Until then Access requests and old principals
fail closed; unchanged legacy credentials still work with valid config.

Disable with:

```sh
hostmcp-hub hub configure --config /absolute/path/hub.json --disable-cloudflare-access
```

The running Hub immediately denies its old Access principals; restart it to finish
the configuration transition. For rollback to an older binary that rejects unknown
fields, remove the optional block using this CLI first. Review edge login routing
before a rollback. Other credentials remain intact.

## Local checks

```sh
go test ./internal/cloudflareaccess ./internal/config ./cmd/hostmcp -run '^TestAccess' -count=1
go test -race ./internal/hub -run '^TestAccess' -count=1
```

These tests generate ephemeral RSA keys/JWTs, serve certs and MCP HTTP in memory,
and frame an unchanged legacy Agent handshake over `net.Pipe`. They test scoped
SDK calls, wire dispatch identity, real isolated process-session ownership,
invalid credentials, cert failures/timeouts/rotation, revocation, mode coexistence,
and agent/enrollment isolation. No real Access team, deployed service, live account,
browser, installed application or production config is contacted. See the
[v0.2.3 validation record](validation-cloudflare-access.md) for command results and
environment limits.
