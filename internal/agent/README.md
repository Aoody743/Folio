# Scoped Linux host executor

This package is a trusted operating-system operation dispatcher, not an AI or
remote shell. Web and MCP authorization belongs to their common business layer.
The host executor separately restricts peer UID, operation names and filesystem
roots.

Native phase-one operations:

| Operation | JSON input | Effect |
| --- | --- | --- |
| `file_write` | `root`, `path`, `content` | Atomic private regular-file replacement |
| `file_backup` | `root`, `path`, `destination_root`, `destination_path` | Copy a bounded regular file, returning its SHA-256 |
| `file_hash` | `root`, `path` | Read-only SHA-256 |

Roots are installed by trusted configuration and addressed by IDs. Authorize
both root IDs for backup. Never grant the entire host `/`, the panel data
directory containing identities, `/proc`, devices or mounted outside trees.
Use a dedicated workspace. Paths are relative; traversal, final symlink reads,
device/FIFO reads and external symlink escapes are rejected. The limit defaults
to 8 MiB per file and 1 MiB per operation JSON. Atomic writes complete as one
operation; canceling after commit does not undo committed data.

A host-executor policy, with paths and UID set by the installer:

```json
{
  "version": 1,
  "allowed_uids": [1000],
  "roots": [{"id": "workspace", "path": "/var/lib/powerlinux/workspace", "read_only": false}],
  "operations": ["file_write", "file_backup", "file_hash"],
  "max_file_bytes": 8388608
}
```

Load it with `LoadPolicy`, then `NewPolicyExecutor`, `NewUnixServer` and
`Serve(ctx)`. The policy must be a regular file owned by the Agent service UID,
not a symlink or writable by group/others. The Unix socket admits only kernel `SO_PEERCRED` UIDs; the
`NewUnixClient` verifies the server UID. The socket is mode 0600. A root Agent
can hand the socket to one configured panel UID, leaving the parent directory
root-owned and nonwritable by that UID. This directory must be traversable by
the panel UID. An existing socket/file is never overwritten automatically.

`Runner` separately accepts trusted absolute program paths plus typed argument
builders configured by application code. It drains both output streams even
after its capture limit, reports actual exit status, and kills the process
group on cancellation, timeout or direct-child exit. Live subscriber delivery
is nonblocking and may drop chunks; durable final output is bounded and reports
truncation. Registered programs must keep descendants in their process group.
They must not turn user input into shell commands. Executables and operation
registration must be managed by a trusted installation.

`jobs.Manager` accepts either native, process or Unix executors with the same
`Validate`/`Run` interface. Supply the panel SQLite adapter or `jobs.FileStore`.
The Manager admits at most 128 active/queued tasks and executes at most four
at once, including native filesystem operations. Tasks waiting for a slot stay
queued; cancellation before their slot does not start an operation.
Queued/running tasks surviving a crash are marked interrupted, never replayed
automatically. Task input and final output are private persisted data; HTTP/MCP
adapters must apply actor and resource authorization before exposing them.
