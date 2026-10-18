# Folio frontend

Next.js static export with React 19 and official Astryx 0.6.8 components. All runtime data comes from the Go service at `/api/v1`; there is no built-in model, chat UI, fixture database, or production Node server.

```sh
npm ci
npm run typecheck
npm test
NEXT_TELEMETRY_DISABLED=1 npm run build
```

The release pipeline copies `out/` into the Go asset embed directory. Test the interface through the same Go origin as the API, not a separate static host, because authentication uses a secure session cookie, CSRF header and same-origin events. For local UI iteration, use `next dev` with a trusted API reverse proxy, or rebuild and serve exported assets through the Go application.

The compact workspace includes real host metrics, revisioned shared notes, durable tasks, task errors, a five-section bottom navigation, five persistent appearance preferences, action discovery, account security and scoped MCP tokens. Unavailable or unauthorized modules show their real availability rather than fabricated installed resources. Browser integration tests in `tests/rewrite` are owned by the repository QA pipeline.

Notes retain an open editor and its unsaved draft when dismissed. Switching to another note is blocked while the existing draft is unsaved. Existing notes autosave after 800 ms; creating a note is explicit. A 412 conflict preserves the draft and requires manual reconciliation or creation of a separate note. History restoration creates a new server revision. Identity labels are always server-issued. Markdown renders through Astryx's sanitized parser, with raw HTML disabled and remote image rendering disabled.

Phase-two resource screens consume the shared registered providers: paginated websites with grouped revisioned settings, real software catalog/instances and reviewable lifecycle plans, authorized relative file roots with hash-conflict editing/history/trash, resumable bounded-memory SHA-256 uploads and streamed downloads, terminal sessions, schedules/notification channels and persisted monitoring history. CodeMirror and xterm load only when needed. Long operations expose actual task state, logs and cancellation. JSON schemas support nested objects, arrays, enums and limits; a failed write retains its draft and intent key. Interrupted operations requiring recovery are blocked from blind replay. File text beyond the JSON budget uses the authenticated UTF-8 text stream while preserving the chosen disk encoding.

Local storage contains only the personal palette preference. Shared notes, identities, tasks, tokens and audit are never stored there. Dates use the viewer's browser time zone. `return_to` after authentication is restricted to the local OAuth authorization endpoint.
