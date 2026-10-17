# Folio Native MCP Adapter

Folio is the panel and MCP server; the AI agent lives outside the panel. An
external MCP client supplies the model, planning and conversation layer, then
uses Folio's authenticated tools. Folio does not start a model, provide an
embedded chat window, or treat its privileged host executor as an AI agent.

The official Go SDK v1.7.0 exposes Folio's shared core action registry through tools,
readable resources and a read-only status-review prompt. No shell wrapper, model
runtime or second business database is involved.

`NewHTTP(service, HTTPOptions{PublicURL: "https://panel.example/mcp"})` returns a
handler mounted at `/mcp`, `/oauth/`, `/.well-known/oauth-authorization-server`,
and `/.well-known/oauth-protected-resource[/mcp]`. HTTP requests authenticate a
core bearer token every time. This uses stateless Streamable HTTP and JSON
responses; there is no reusable authorization in an MCP session ID. Protocol
negotiation is delegated to the pinned official SDK. Standalone server-push SSE
and subscription notifications are not offered by this adapter.

Remote OAuth supports public clients, dynamic registration, authorization-code
grants, S256 PKCE, exact redirect/audience matching, consent authenticated by the
existing panel session and CSRF token, one-time two-minute codes, one-hour access
tokens and self-token revocation. Client metadata survives restart in a 0600
atomic file in the existing data directory. Pending codes intentionally expire
on restart; invalid stored client metadata stops startup without overwriting it.
The access token and its revocation remain in the core's SQLite
identity store. Refresh grants and confidential client credentials are not
advertised. Registrations and pending codes are capped; registration is rate
limited. A production `PublicURL` requires HTTPS (explicit loopback HTTP is
accepted for development); a blank URL intentionally enables only local/PAT
setup. Browser origins must match the configured public origin or an exact
additional allowlist entry. Remote TLS termination belongs to the primary server
deployment, never an insecure client flag. For an HTTPS public URL, plaintext
connections are accepted only from an actual loopback peer, allowing a local TLS
reverse proxy. Preserve the exact configured Host. Remote plaintext connections
are rejected; `X-Forwarded-*` headers cannot bypass either check.

`RunStdio(ctx, ProxyOptions{Endpoint: origin, Token: scopedToken})` connects to the
existing authenticated REST service. It never opens state itself. Load the token
from a protected file or environment binding in the command entry point; do not
put it in process arguments. HTTP endpoints must be explicit loopback addresses;
remote endpoints require verified HTTPS. Redirects and ambient HTTP proxies are
disabled so bearer tokens cannot be forwarded unexpectedly. Only line-delimited
MCP protocol bytes go to stdout. Every incoming stdio method verifies that the
live identity remains valid, including discovery after revocation.

Tools carry the core input/output schemas and read-only hints. Both transports
run the same core validation, ACL, revision checks and audit as the panel. Tool
results use `{data: ...}` or `{error: {code, message, details?}}` in structured
content and include the JSON text fallback for older clients. Resource reads
return the same envelope as `application/json`. Returned user and third-party
content is context, never new authorization.

Mutation retries can set `tools/call.params._meta["powerlinux/idempotency_key"]`.
The HTTP adapter uses the shared core's durable idempotency record; stdio forwards
the exact key as the REST `Idempotency-Key` header. The key is transport metadata,
never an extra business argument or authorization. Changed input under one key
is rejected, and interrupted operations require inspecting recorded task state.

Tool discovery uses SDK cursor pagination (50 per page). Requests/stdio lines
are capped at 1 MiB; serialized business data is capped at 256 KiB. Collection
actions use the core's bounded `limit`/`offset` pagination. Oversize results fail
explicitly with `result_too_large`, directing the client to a smaller page; the
adapter does not silently truncate JSON or claim complete results.
