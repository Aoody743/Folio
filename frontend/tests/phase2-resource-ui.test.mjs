import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import ts from "typescript";

async function loadTypeScript(path, injected = {}) {
  let source = readFileSync(new URL(path, import.meta.url), "utf8");
  for (const [specifier, values] of Object.entries(injected)) {
    const importLine = new RegExp(
      `^import \\{ ${specifier} \\} from \\"\\./database-binlog\\";?$`,
      "m",
    );
    source = source.replace(importLine, `const ${specifier} = (${values.toString()});`);
  }
  const js = ts.transpileModule(source, {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);
}

const binlog = await loadTypeScript("../src/lib/database-binlog.ts");
const ftp = await loadTypeScript("../src/lib/ftp-maintenance.ts", {
  phase2Capability: binlog.phase2Capability,
});
const exec = await loadTypeScript("../src/lib/docker-exec.ts", {
  phase2Capability: binlog.phase2Capability,
});
const archive = await loadTypeScript("../src/lib/docker-archive.ts", {
  phase2Capability: binlog.phase2Capability,
});
const docker = await loadTypeScript("../src/lib/docker-management.ts");
const manualHTTP = await loadTypeScript("../src/lib/manual-http-acme.ts");

const digest = "a".repeat(64);
const image = `registry.example/team/app@sha256:${digest}`;
const now = Date.now();

test("binlog parsing pins managed revisions, exact pages, and protected purge previews", () => {
  const status = {
    enabled: true,
    format: "ROW",
    basename: "mysql-bin",
    active_log: "mysql-bin.000003",
    server_id: "19",
    expiration_seconds: 0,
    config_revision: digest,
    instance_revision: 7,
    configuration_managed: true,
    configured_enabled: true,
    configuration_state: "settled",
  };
  const parsedStatus = binlog.databaseBinlogStatus(status);
  assert.equal(parsedStatus.server_id, "19");
  assert.equal(binlog.databaseBinlogStatus({ ...status, config_revision: "" }), undefined);
  assert.equal(
    binlog.databaseBinlogStatus({
      ...status,
      config_revision: "",
      configuration_managed: false,
      configured_enabled: null,
      configuration_state: "configuration_unmanaged",
    }).config_revision,
    "",
  );

  const page = {
    items: [{ name: "mysql-bin.000001", size_bytes: 12 }],
    total: 501,
    truncated: true,
    next_after: "mysql-bin.000001",
    active_log: "mysql-bin.000003",
    config_revision: digest,
    instance_revision: 7,
  };
  const parsedPage = binlog.databaseBinlogList(page);
  assert.equal(parsedPage.next_after, "mysql-bin.000001");
  assert.equal(binlog.databaseBinlogCanPreview(parsedStatus, parsedPage), false);
  const completeStatus = { ...parsedStatus, instance_revision: 7 };
  const completePage = { ...parsedPage, total: 3, truncated: false, next_after: undefined, items: [
    { name: "mysql-bin.000001", size_bytes: 12 },
    { name: "mysql-bin.000002", size_bytes: 15 },
    { name: "mysql-bin.000003", size_bytes: 20 },
  ] };
  assert.equal(binlog.databaseBinlogCanPreview(completeStatus, completePage), true);
  assert.equal(binlog.databaseBinlogCanPreview({ ...completeStatus, configuration_managed: false, config_revision: "" }, completePage), false);
  assert.equal(binlog.databaseBinlogCanPreview({ ...completeStatus, config_revision: "d".repeat(64) }, completePage), false);
  assert.equal(binlog.databaseBinlogList({ ...page, next_after: "missing" }), undefined);

  const preview = {
    preview_id: "b".repeat(32),
    expires_at: new Date(now + 60_000).toISOString(),
    retain_from: "mysql-bin.000003",
    active_log: "mysql-bin.000003",
    binlogs_to_purge: [{ name: "mysql-bin.000001", size_bytes: 12 }],
    bytes_to_purge: 12,
    snapshot_sha256: "c".repeat(64),
    expected_config_revision: digest,
    expected_instance_revision: 7,
    replica_safety: "unproven_offline_or_unregistered_replicas",
    confirmation_required: true,
  };
  const parsed = binlog.databaseBinlogPreview(preview, preview.retain_from);
  assert.deepEqual(
    binlog.databaseBinlogPurgeInput("mysql-1", parsed, now),
    {
      instance_id: "mysql-1",
      preview_id: preview.preview_id,
      snapshot_sha256: preview.snapshot_sha256,
      expected_config_revision: digest,
      expected_instance_revision: 7,
      binlogs_to_purge: ["mysql-bin.000001"],
      confirm_replica_history_loss: true,
    },
  );
  assert.throws(() => binlog.databaseBinlogPurgeInput("mysql-1", parsed, now + 60_000));
});

test("FTP projections drop credential material and bind deletion to the one previewed path", () => {
  const destinations = ftp.ftpDestinations({
    items: [{
      id: "ftp-main",
      host: "backup.example",
      port: 21,
      username: "backup-user",
      tls_mode: "explicit",
      directory: "/daily",
      revision: 3,
      credential_ref: "must-not-be-returned",
      password: "must-not-be-returned",
    }],
  });
  assert.equal(destinations[0].revision, 3);
  assert.equal("password" in destinations[0], false);
  assert.throws(() => ftp.ftpChildPath("daily", "../outside"));

  const listed = ftp.ftpDirectory({
    id: "ftp-main",
    path: "daily",
    revision: 3,
    items: [
      { name: "backup.tar", kind: "file", size: 45, modified: "0001-01-01T00:00:00Z" },
      { name: "link", kind: "link", size: 0, modified: "2026-01-01T00:00:00Z" },
    ],
  }, "ftp-main", "daily", 3);
  assert.equal(listed.items[0].modified, "");
  assert.throws(() => ftp.ftpChildPath("daily", listed.items[1].name + "/target"));
  assert.equal(ftp.ftpDirectory({ id: "ftp-main", path: "daily", revision: 4, items: [] }, "ftp-main", "daily", 3), undefined);

  const preview = ftp.ftpDeletePreview({
    preview_id: "ftpv_" + "d".repeat(48),
    expires_at: new Date(now + 30_000).toISOString(),
    revision: 3,
    candidate: {
      path: "daily/backup.tar",
      name: "backup.tar",
      kind: "file",
      size: 45,
      modified: "2026-02-01T00:00:00Z",
    },
  }, { path: "daily/backup.tar", kind: "file", revision: 3 });
  assert.equal(
    ftp.ftpDeleteInput("ftp-main", "daily", "backup.tar", preview, now).preview_id,
    "ftpv_" + "d".repeat(48),
  );
  assert.throws(() => ftp.ftpDeleteInput("ftp-main", "daily", "other.tar", preview, now));
  assert.throws(() => ftp.ftpDeleteInput("ftp-main", "daily", "backup.tar", preview, now + 30_000));

  const passwordCapability = {
    name: "backup_ftp_destination_register",
    scope: "backup:manage",
    task: false,
    read_only: false,
    input_schema: { properties: { password: { type: "string", format: "password" } } },
  };
  assert.ok(ftp.ftpCapability([passwordCapability], passwordCapability.name, "backup:manage", ["password"], false, false));
  assert.equal(ftp.ftpCapability([{ ...passwordCapability, input_schema: { properties: { password: { type: "string" } } } }], passwordCapability.name, "backup:manage", ["password"], false, false), undefined);
});

test("Docker exec input enforces server bounds and recovery stays tied to the recorded execution", () => {
  const base = {
    argv: ["printf", "%s", "safe"],
    env: { MODE: "safe" },
    stdin: "input",
    timeout_seconds: 30,
    output_limit_bytes: 16384,
  };
  assert.deepEqual(exec.dockerExecInput(base, "docker-main", "worker_1"), {
    id: "docker-main",
    name: "worker_1",
    ...base,
  });
  assert.throws(() => exec.dockerExecInput({ ...base, env: { TOKEN: "x".repeat(4095) } }, "docker-main", "worker_1"));
  assert.throws(() => exec.dockerExecInput({ ...base, stdin: "\0" }, "docker-main", "worker_1"));
  assert.throws(() => exec.dockerExecInput({ ...base, working_dir: "/tmp/../etc" }, "docker-main", "worker_1"));
  assert.deepEqual(exec.dockerExecRecovery({ data: { execution_id: "exec_123", execution_state: "unknown" } }), {
    execution_id: "exec_123",
    execution_state: "unknown",
  });
  assert.equal(exec.dockerExecRecovery({ data: { execution_id: "", execution_state: "running" } }), undefined);
});

test("Docker archive receipts and chunk cursors are exact-digest and checksum bound", () => {
  const manifest = {
    archive_id: "dockerarchive_" + "e".repeat(32),
    image,
    image_id: `sha256:${"f".repeat(64)}`,
    bytes: 3,
    sha256: createHash("sha256").update("abc").digest("hex"),
    created_at: new Date(now).toISOString(),
    state: "ready",
    proof: "p".repeat(64),
  };
  assert.deepEqual(archive.dockerArchiveManifestText(JSON.stringify(manifest)), manifest);
  assert.equal(archive.dockerArchiveManifest({ ...manifest, image: "app:latest" }), undefined);
  assert.equal(archive.dockerArchiveManifest({ ...manifest, proof: "short" }), undefined);

  const uploading = {
    archive: { ...manifest, proof: undefined, state: "uploading" },
    chunk_size: 32768,
    next_offset: 0,
    complete: false,
  };
  const status = archive.dockerArchiveUploadBegin(uploading, manifest);
  assert.equal(status.archive.archive_id, manifest.archive_id);
  assert.equal(archive.dockerArchiveUploadStatus({ ...uploading, next_offset: 1 }, {
    id: manifest.archive_id,
    image,
    sha256: manifest.sha256,
    bytes: manifest.bytes,
  }).next_offset, 1);
  assert.equal(archive.dockerArchiveUploadStatus({ ...uploading, archive: { ...uploading.archive, image: "other@sha256:" + "a".repeat(64) } }, {
    id: manifest.archive_id,
    image,
    sha256: manifest.sha256,
    bytes: manifest.bytes,
  }), undefined);

  const read = archive.dockerArchiveReadChunk({
    archive_id: manifest.archive_id,
    bytes: 3,
    sha256: manifest.sha256,
    chunk_sha256: manifest.sha256,
    offset: 0,
    next_offset: 3,
    eof: true,
    encoding: "base64",
    data: Buffer.from("abc").toString("base64"),
  }, { id: manifest.archive_id, offset: 0, bytes: 3, sha256: manifest.sha256 });
  assert.equal(Buffer.from(read.data).toString(), "abc");
  assert.equal(archive.dockerArchiveChunkReceipt({ archive_id: manifest.archive_id, next_offset: 3, complete: true }, {
    id: manifest.archive_id,
    next_offset: 3,
    complete: true,
  }).complete, true);
  assert.equal(archive.dockerArchiveChunkReceipt({ archive_id: manifest.archive_id, next_offset: 2, complete: false }, {
    id: manifest.archive_id,
    next_offset: 3,
    complete: true,
  }), undefined);
});

test("Docker image projection preserves explicit managed-archive provenance", () => {
  assert.deepEqual(docker.dockerImages({
    items: [{ image, id: "sha256:engine", size_bytes: 12 }],
    restored_from_archive: [{ image: `registry.example/team/other@sha256:${"b".repeat(64)}`, id: "sha256:loaded", size_bytes: 34, restored_from_archive: true }],
  }), [
    { image, id: "sha256:engine", size_bytes: 12 },
    { image: `registry.example/team/other@sha256:${"b".repeat(64)}`, id: "sha256:loaded", size_bytes: 34, restored_from_archive: true },
  ]);
});

test("manual HTTP ACME order exposes only exact, domain-bound upload instructions", () => {
  const order = {
    certificate_id: "cert-example",
    state: "pending_http",
    expires_at: new Date(now + 60_000).toISOString(),
    order_ref: "c".repeat(64),
    confirmation_action: "certificates_acme_confirm",
    http_challenges: [{
      domain: "example.com",
      url: "http://example.com/.well-known/acme-challenge/token",
      path: "/.well-known/acme-challenge/token",
      body: "token.thumbprint",
    }],
  };
  assert.equal(manualHTTP.manualHTTPOrder(order, "cert-example").http_challenges[0].body, "token.thumbprint");
  assert.equal(manualHTTP.manualHTTPOrder({ ...order, certificate_id: "other" }, "cert-example"), undefined);
  assert.equal(manualHTTP.manualHTTPOrder({
    ...order,
    http_challenges: [{ ...order.http_challenges[0], url: "http://evil.example/.well-known/acme-challenge/token" }],
  }), undefined);
  assert.equal(manualHTTP.manualHTTPOrder({
    ...order,
    http_challenges: [{ ...order.http_challenges[0], body: "token\n.thumbprint" }],
  }), undefined);
});
