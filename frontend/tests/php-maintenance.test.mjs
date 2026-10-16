import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

const source = readFileSync(
  new URL("../src/lib/php-maintenance.ts", import.meta.url),
  "utf8",
);
const js = ts.transpileModule(source, {
  compilerOptions: {
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.ESNext,
  },
}).outputText;
const {
  phpMaintenanceCapability,
  phpMaintenanceLog,
  phpWorkerCleanupInput,
  phpWorkerPreviewExpired,
  phpWorkerSessionPreview,
  phpWorkerSessionStats,
} = await import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);

const stats = {
  id: "php-1",
  revision: 9,
  generated: true,
  total_sessions: 12,
  expired_sessions: 4,
  older_than_seconds: 86400,
  oldest_age_seconds: 100000,
  allow_actions: ["preview"],
};
const preview = {
  id: "php-1",
  revision: 9,
  preview_id: "p-opaque",
  captured_at: "2026-10-15T00:00:00Z",
  expires_at: "2026-10-15T00:05:00Z",
  total_sessions: 12,
  candidate_count: 3,
  candidate_bytes: 4096,
  older_than_seconds: 86400,
  allow_actions: ["cleanup"],
};

test("PHP log reader accepts only the bounded managed FPM error log shape", () => {
  assert.deepEqual(
    phpMaintenanceLog(
      {
        id: "php-1",
        stream: "fpm_error",
        generated: true,
        lines: ["worker exited"],
        revision: 2,
        truncated: true,
        path: "/private/path",
      },
      "php-1",
    ),
    {
      id: "php-1",
      stream: "fpm_error",
      configured: true,
      generated: true,
      lines: ["worker exited"],
      revision: 2,
      truncated: true,
    },
  );
  assert.equal(
    phpMaintenanceLog(
      { ...stats, stream: "slow", lines: [], generated: true },
      "php-1",
    ),
    undefined,
  );
  assert.equal(
    phpMaintenanceLog(
      {
        id: "php-1",
        stream: "fpm_error",
        generated: false,
        lines: ["unexpected"],
        revision: 2,
        truncated: false,
      },
      "php-1",
    ),
    undefined,
  );
});

test("session stats and preview project only aggregate counts and immutable preview metadata", () => {
  assert.deepEqual(phpWorkerSessionStats({ ...stats, session_ids: ["secret"] }, "php-1"), stats);
  assert.equal(phpWorkerSessionStats({ ...stats, expired_sessions: 13 }, "php-1"), undefined);

  const parsed = phpWorkerSessionPreview({ ...preview, filenames: ["secret"] }, "php-1", 9);
  assert.deepEqual(parsed, preview);
  assert.equal(phpWorkerSessionPreview(preview, "php-1", 10), undefined);
  assert.equal(phpWorkerSessionPreview({ ...preview, candidate_count: 0 }, "php-1", 9), undefined);
});

test("cleanup input pins the immutable preview and refuses expired or disallowed previews", () => {
  const parsed = phpWorkerSessionPreview(preview, "php-1", 9);
  assert.deepEqual(phpWorkerCleanupInput(parsed, Date.parse("2026-10-15T00:04:00Z")), {
    id: "php-1",
    expected_revision: 9,
    preview_id: "p-opaque",
  });
  assert.equal(phpWorkerPreviewExpired(parsed, Date.parse("2026-10-15T00:05:00Z")), true);
  assert.throws(() => phpWorkerCleanupInput(parsed, Date.parse("2026-10-15T00:05:00Z")));
  const disallowed = phpWorkerSessionPreview({ ...preview, allow_actions: [] }, "php-1", 9);
  assert.throws(() => phpWorkerCleanupInput(disallowed));
});

test("PHP action gating binds exact scope, task status and allowed log stream", () => {
  const read = {
    name: "php_maintenance_log_read",
    scope: "php:read",
    read_only: true,
    input_schema: {
      properties: { id: {}, stream: { enum: ["fpm_error"] } },
    },
  };
  assert.equal(
    phpMaintenanceCapability([read], read.name, "php:read", ["id", "stream"], false),
    read,
  );
  const preview = {
    name: "php_maintenance_session_preview",
    scope: "php:read",
    read_only: false,
    input_schema: { properties: { id: {}, expected_revision: {}, older_than_seconds: {} } },
  };
  assert.equal(
    phpMaintenanceCapability([preview], preview.name, "php:read", ["id", "expected_revision", "older_than_seconds"], false, false),
    preview,
  );
  assert.equal(
    phpMaintenanceCapability([{ ...preview, read_only: true }], preview.name, "php:read", ["id", "expected_revision", "older_than_seconds"], false, false),
    undefined,
  );
  assert.equal(
    phpMaintenanceCapability([{ ...read, scope: "php:manage" }], read.name, "php:read", ["id", "stream"], false),
    undefined,
  );
  assert.equal(
    phpMaintenanceCapability(
      [{ ...read, input_schema: { properties: { id: {}, stream: { enum: ["slow"] } } } }],
      read.name,
      "php:read",
      ["id", "stream"],
      false,
    ),
    undefined,
  );
});
