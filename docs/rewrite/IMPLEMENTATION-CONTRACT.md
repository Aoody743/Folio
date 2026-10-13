# Shared phase-one implementation contract

Root module: github.com/Aoody743/PowerLinux, Go 1.27.2. New UI is frontend/ (Next.js static export, Astryx, React 19). Existing Python implementation remains a reference, never called by the new runtime. No built-in AI chat.

Agent file ownership is disjoint. Root owns go.mod/go.sum, cmd/powerlinux, integration, docs and progress. Never commit/push from child agents. Root will integrate and test before advancing stages. Do not claim unspecified or untested behavior as done.

Core public interface (backend agent implements):
- core.Actor {ID, Name, Kind string; Scopes []string; Resources []string}, json snake_case.
- core.Action {Name, Description string; InputSchema map[string]any; ReadOnly bool}.
- core.Event {Type string; Data any}.
- core.New(dataDir string) (*Service,error), Service.Close() error.
- Service.Execute(context.Context, Actor, string, json.RawMessage) (any,error), Service.Actions(Actor) []Action.
- Service.AuthenticateToken(context.Context,string) (Actor,error).
- Service.Subscribe(context.Context,Actor) <-chan Event.
Core contains SQLite identities, password/TOTP/session/API token lifecycle, action registry, notes revisions/conflicts/history, audit. HTTP adapter owns routes and CSRF, uses the same actions. Expose httpapi.New(service *core.Service) http.Handler or document constructor options required for cookie/bootstrapping security. Export typed errors carrying code/status.

MCP owns internal/mcpbridge. Use official Go SDK; bridge typed actions to core.Execute, same actor ACL. Streamable HTTP handler authenticates every connection, handles Origin appropriately. Stdio adapter forwards to authenticated backend over local protected connection or verified loopback endpoint; never a second state engine. Shared actor identity must not be spoofable through tool args. Native SDK tools/resources/actions no shell wrapping. Contract adjustments go through parent promptly.

Executor owns internal/agent and internal/jobs. No imports from internal/core (avoids cycles). Standalone Runner with bounded output, timeout/cancel/process-group cleanup, argv-only allowlisted operations, Unix socket peer credentials and scoped file roots. Provide a Go library and tests, root/backend integrate durable jobs. Must never execute old insecure installers on shared host.

Front-end routes: GET /healthz. JSON /api/v1 endpoints use {data:...} successes; errors {error:{code,message,details?}}.
GET /session -> {initialized,authenticated,user?,csrf_token?}. POST /auth/setup (bootstrapping must require local secret), POST /auth/login {username,password,otp?}, POST /auth/logout. Cookie pl_session; cookie writes require X-CSRF-Token. Bearer credentials use server scopes.
GET /system/status, /notes, /tasks, /audit, /capabilities. POST /notes. GET/PATCH/DELETE /notes/{id}; POST /notes/{id}/archive|restore; GET /notes/{id}/history. PATCH expected_revision. GET /events authenticated SSE for invalidation. GET/POST/DELETE /tokens and TOTP/account management as backend documents. POST /actions/{name} (same business registry).
Notes {id,title,content,pinned,archived,target_id,revision,created_at,updated_at,created_by,updated_by}; authors server-issued actor objects. Registry names notes_list/get/create/update/archive/restore/delete/history, system_get_status, capabilities_list, audit_list; more actions follow contracts.
UI five navigation sections overview/sites/software/files/more; four overview cards system/notes/tasks/events; five themes. Notes editor handles conflicts, autosave, history and author source. Prototype fixture data allowed ONLY in tests; UI runtime reads real APIs and shows unavailable modules honestly. Additional phase-two routes to be defined after phase-one tests pass.

Release owns scripts/rewrite, Makefile, .github/workflows/rewrite*.yml, release packaging and installer tests. Installation supports a nonprivileged prefix for tests, production owns only PowerLinux dirs/user/service, verified release artifacts, no global shell/locale/firewall/cron changes. No insecure TLS switches or unconditional destructive uninstall.

QA owns tests/rewrite and validates phase-one flows from actual servers and real UI. Investigate clean cloud VM capability (QEMU/cloud image or existing sandbox), report concrete restrictions; do not silently substitute a container for VM evidence. No external credentials currently supplied. Use /workspace tools/caches and trusted download checksums.
