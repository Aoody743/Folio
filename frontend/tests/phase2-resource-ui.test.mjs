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
const dockerTerminal = await loadTypeScript("../src/lib/docker-terminal.ts");
const phpMaintenance = await loadTypeScript("../src/lib/php-maintenance.ts");
const sitePHP = await loadTypeScript("../src/lib/site-php-maintenance.ts");
const postgresql = await loadTypeScript("../src/lib/postgresql-maintenance.ts");
const phpPackages = await loadTypeScript("../src/lib/php-extension-packages.ts");
const ftpAccount = await loadTypeScript("../src/lib/ftp-account-state.ts", {
  phase2Capability: binlog.phase2Capability,
});

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

test("Docker terminal sessions bind to the selected owned container and bounded replay cursor", () => {
  const sessionID = `dct_${"e".repeat(32)}`;
  const session = {
    id: sessionID,
    name: "worker_1",
    session_id: sessionID,
    target_id: "docker:docker-main:worker_1",
    type: "docker",
    rows: 24,
    cols: 80,
    created_at: new Date(now).toISOString(),
    expires_at: new Date(now + 60_000).toISOString(),
    execution_state: "running",
    attached: true,
    stream_started: true,
    output_available: true,
  };
  const parsed = dockerTerminal.dockerTerminalSession(session, "docker-main", "worker_1");
  assert.equal(parsed.session_id, sessionID);
  assert.equal(dockerTerminal.dockerTerminalSession({ ...session, target_id: "docker:other:worker_1" }, "docker-main", "worker_1"), undefined);
  assert.equal(dockerTerminal.dockerTerminalSession({ ...session, name: "other" }, "docker-main", "worker_1"), undefined);
  assert.equal(dockerTerminal.dockerTerminalSession({ ...session, rows: 501 }, "docker-main", "worker_1"), undefined);

  const page = {
    ...session,
    frames: [
      { sequence: 1, data: Buffer.from("first").toString("base64") },
      { sequence: 2, data: Buffer.from("second").toString("base64") },
    ],
    truncated: false,
    next: 2,
  };
  assert.equal(dockerTerminal.dockerTerminalRead(page, "docker-main", "worker_1", sessionID, 0).next, 2);
  assert.equal(dockerTerminal.dockerTerminalRead({ ...page, frames: [{ sequence: 1, data: "@@" }] }, "docker-main", "worker_1", sessionID), undefined);
  assert.equal(dockerTerminal.dockerTerminalRead({ ...page, frames: [{ sequence: 1, data: "eA==" }, { sequence: 1, data: "eQ==" }] }, "docker-main", "worker_1", sessionID), undefined);
  assert.equal(dockerTerminal.dockerTerminalRead(page, "docker-main", "worker_1", `dct_${"a".repeat(32)}`), undefined);
});

test("Docker terminal input retries retain a caller frame ID and enforce the 16 KiB bound", () => {
  const sessionID = `dct_${"f".repeat(32)}`;
  const frameID = "intent-1234567890";
  const payload = Buffer.alloc(16 * 1024, 65).toString("base64");
  assert.deepEqual(dockerTerminal.dockerTerminalInputFrame(sessionID, frameID, payload), {
    session_id: sessionID,
    input_id: frameID,
    data: payload,
  });
  assert.throws(() => dockerTerminal.dockerTerminalInputFrame(sessionID, frameID, Buffer.alloc(16 * 1024 + 1, 65).toString("base64")));
  assert.throws(() => dockerTerminal.dockerTerminalInputFrame(sessionID, "short", "eA=="));
});

test("PHP slowlog status distinguishes disabled configuration from an empty generated log", () => {
  const capability = {
    name: "php_maintenance_log_read",
    scope: "php:read",
    task: false,
    read_only: true,
    input_schema: {
      required: ["id", "stream"],
      properties: { id: { type: "string" }, stream: { enum: ["fpm_error", "slow"] }, limit: { type: "integer" } },
    },
  };
  assert.ok(phpMaintenance.phpMaintenanceCapability([capability], capability.name, "php:read", ["id", "stream"], false, true, "slow"));
  assert.equal(phpMaintenance.phpMaintenanceCapability([{ ...capability, input_schema: { ...capability.input_schema, properties: { ...capability.input_schema.properties, stream: { enum: ["fpm_error"] } } } }], capability.name, "php:read", ["id", "stream"], false, true, "slow"), undefined);
  assert.equal(phpMaintenance.phpMaintenanceLog({ id: "php-main", stream: "slow", configured: false, generated: false, lines: [], revision: 5, truncated: false }, "php-main", "slow").configured, false);
  assert.equal(phpMaintenance.phpMaintenanceLog({ id: "php-main", stream: "slow", configured: false, generated: true, lines: ["leak"], revision: 5, truncated: false }, "php-main", "slow"), undefined);
});

test("per-site PHP preview inputs stay bound to the site revision and assigned runtime", () => {
  const siteID = "site-7";
  const phpInstance = "php-main";
  const stats = {
    site_id: siteID,
    site_revision: 12,
    php_instance: phpInstance,
    generated: true,
    total_sessions: 8,
    expired_sessions: 3,
    older_than_seconds: 86400,
    oldest_age_seconds: 172800,
    allow_actions: ["preview"],
    private_path: "/must-not-be-projected",
  };
  const parsedStats = sitePHP.sitePHPSessionStats(stats, siteID, 12, phpInstance);
  assert.equal(parsedStats.expired_sessions, 3);
  assert.equal("private_path" in parsedStats, false);
  assert.equal(sitePHP.sitePHPSessionStats(stats, siteID, 13, phpInstance), undefined);
  assert.equal(sitePHP.sitePHPSessionStats(stats, siteID, 12, "other-php"), undefined);

  const preview = sitePHP.sitePHPSessionPreview({
    site_id: siteID,
    site_revision: 12,
    php_instance: phpInstance,
    preview_id: `spv_${"a".repeat(48)}`,
    captured_at: new Date(now).toISOString(),
    expires_at: new Date(now + 60_000).toISOString(),
    total_sessions: 8,
    candidate_count: 3,
    candidate_bytes: 900,
    older_than_seconds: 86400,
    allow_actions: ["cleanup"],
    session_names: ["must-not-be-returned"],
  }, siteID, 12, phpInstance);
  assert.deepEqual(sitePHP.sitePHPSessionCleanupInput(preview, 12, now), {
    id: siteID,
    expected_revision: 12,
    preview_id: `spv_${"a".repeat(48)}`,
  });
  assert.equal("session_names" in preview, false);
  assert.throws(() => sitePHP.sitePHPSessionCleanupInput(preview, 13, now));
  assert.throws(() => sitePHP.sitePHPSessionCleanupInput(preview, 12, now + 60_000));
});

test("PostgreSQL maintenance mutations require exact owned revisions and loopback CIDRs", () => {
  const access = {
    instance_id: "pg-main",
    database: "app_db",
    instance_revision: 8,
    database_revision: 3,
    config_revision: digest,
    configuration_state: "settled",
    protected_local_admin_preserved: true,
    remote_access_supported: false,
    remote_access_limitation: "managed_tls_not_configured",
    rules: [{
      rule_id: `pga_${"b".repeat(32)}`,
      database: "app_db",
      role: "app_user",
      cidr: "127.0.0.0/8",
      auth_method: "scram-sha-256",
      state: "allowed",
    }],
  };
  const parsed = postgresql.parsePostgreSQLAccessList(access, "pg-main", "app_db");
  assert.equal(parsed.rules[0].role, "app_user");
  assert.equal(postgresql.parsePostgreSQLAccessList(access, "pg-other", "app_db"), undefined);
  assert.equal(postgresql.parsePostgreSQLAccessList({ ...access, config_revision: "stale" }, "pg-main", "app_db"), undefined);
  assert.equal(postgresql.isLoopbackPostgreSQLCIDR("127.0.0.0/8"), true);
  assert.equal(postgresql.isLoopbackPostgreSQLCIDR("127.0.0.1/32"), true);
  assert.equal(postgresql.isLoopbackPostgreSQLCIDR("::1/128"), true);
  assert.equal(postgresql.isLoopbackPostgreSQLCIDR("127.0.0.1/8"), false);
  assert.equal(postgresql.isLoopbackPostgreSQLCIDR("10.0.0.0/8"), false);
  assert.equal(postgresql.isLoopbackPostgreSQLCIDR("127.0.0.0/7"), false);
  assert.deepEqual(
    postgresql.postgresqlAccessMutationInput(parsed, "pg-main", "app_db", "add", "127.0.0.0/8"),
    {
      instance_id: "pg-main",
      database: "app_db",
      expected_config_revision: digest,
      expected_instance_revision: 8,
      expected_database_revision: 3,
      cidr: "127.0.0.0/8",
      auth_method: "scram-sha-256",
    },
  );
  assert.equal(postgresql.postgresqlAccessMutationInput(parsed, "pg-main", "app_db", "add", "10.0.0.0/8"), undefined);
  const pending = postgresql.parsePostgreSQLAccessList({ ...access, configuration_state: "pending_recovery" }, "pg-main", "app_db");
  assert.equal(postgresql.postgresqlAccessMutationInput(pending, "pg-main", "app_db", "remove", access.rules[0].rule_id), undefined);
  assert.equal(postgresql.postgresqlAccessMutationInput(pending, "pg-main", "app_db", "recover", "apply_pending").recovery_choice, "apply_pending");
  assert.equal(postgresql.postgresqlAccessMutationInput(pending, "pg-main", "app_db", "recover", "guess"), undefined);

  const slow = {
    instance_id: "pg-main",
    database: "app_db",
    instance_revision: 8,
    database_revision: 3,
    config_revision: digest,
    enabled: true,
    minimum_duration_ms: 900,
    configuration_state: "settled",
    reload_applied: true,
  };
  const status = postgresql.parsePostgreSQLSlowlogStatus(slow, "pg-main", "app_db");
  assert.deepEqual(
    postgresql.postgresqlSlowlogInput(status, "configure", false, 900),
    {
      instance_id: "pg-main",
      database: "app_db",
      expected_config_revision: digest,
      expected_instance_revision: 8,
      expected_database_revision: 3,
      enabled: false,
      minimum_duration_ms: 900,
    },
  );
  assert.equal(postgresql.postgresqlSlowlogInput(status, "configure", true, 600001), undefined);
  const log = postgresql.parsePostgreSQLSlowlogRead({
    ...slow,
    redaction_notice: "String and numeric literals and SQL comments are masked; SQL identifiers may still contain sensitive text.",
    items: [{ started_at: new Date(now).toISOString(), duration_ms: 1200, database: "app_db", role: "app_user", statement: "SELECT 1" }],
    truncated: false,
    log_available: true,
  }, "pg-main", "app_db");
  assert.equal(log.items[0].statement, "SELECT 1");
  assert.equal(postgresql.parsePostgreSQLSlowlogRead({
    ...slow,
    redaction_notice: "No data is sensitive",
    items: [],
    truncated: false,
    log_available: true,
  }, "pg-main", "app_db").redaction_notice, "No data is sensitive");
  assert.equal(postgresql.parsePostgreSQLSlowlogRead({
    ...slow,
    redaction_notice: "",
    items: [],
    truncated: false,
    log_available: true,
  }, "pg-main", "app_db"), undefined);
  assert.equal(postgresql.parsePostgreSQLSlowlogRead({
    ...slow,
    redaction_notice: "String and numeric literals and SQL comments are masked; SQL identifiers may still contain sensitive text.",
    items: [{ started_at: new Date(now).toISOString(), duration_ms: 1, database: "other_db", role: "x", statement: "SELECT 1" }],
    truncated: false,
    log_available: true,
  }, "pg-main", "app_db"), undefined);
});

test("trusted PHP extension plans require pinned resources, ABI revision, expiry, and stable apply keys", () => {
  const planCap = {
    name: "php_extension_plan",
    scope: "php:manage",
    task: false,
    read_only: true,
    input_schema: {
      required: ["id", "operation", "module_id", "expected_revision"],
      properties: {
        id: { type: "string" },
        operation: { enum: ["install", "uninstall"] },
        module_id: { enum: ["curl", "zip", "opcache", "imagick"] },
        expected_revision: { type: "integer", minimum: 1 },
      },
    },
  };
  const applyCap = {
    name: "php_extension_apply",
    scope: "php:manage",
    task: true,
    read_only: false,
    input_schema: {
      required: ["id", "plan_id", "expected_revision", "idempotency_key"],
      properties: {
        id: { type: "string" }, plan_id: { type: "string" },
        expected_revision: { type: "integer" }, idempotency_key: { type: "string" },
      },
    },
  };
  const actions = phpPackages.phpExtensionActions([planCap, applyCap]);
  assert.deepEqual(actions.modules, ["curl", "zip"]);
  const plan = {
    plan_id: "phpext_plan_01",
    id: "php-main",
    operation: "install",
    module_id: "curl",
    php_version: "8.4.2",
    php_module_api: "20240924",
    platform: { os: "linux", arch: "amd64", distro: "debian", release: "13" },
    package_pins: [{ name: "php8.4-curl", version: "8.4.2-1", architecture: "amd64", repository: "debian-security", sha256: digest }],
    affected_profiles: [{ id: "php-main", php_version: "8.4.2", module_enabled: false, package_reference: false }],
    changes: [{ kind: "install", target: "php8.4-curl", description: "Install signed curl module package" }],
    resources: ["software:php-main", "host:packages"],
    expected_revision: 9,
    created_at: new Date(now).toISOString(),
    expires_at: new Date(now + 30 * 60_000).toISOString(),
    already_installed: false,
    already_enabled: false,
    package_removable: false,
  };
  const parsed = phpPackages.parsePHPExtensionPlan(plan, {
    id: "php-main", operation: "install", module_id: "curl", expected_revision: 9,
  });
  assert.equal(parsed.package_pins[0].sha256, digest);
  assert.equal(phpPackages.parsePHPExtensionPlan({ ...plan, resources: ["software:php-main"] }, {
    id: "php-main", operation: "install", module_id: "curl", expected_revision: 9,
  }), undefined);
  assert.equal(phpPackages.phpExtensionApplyInput(parsed, "php-main", 10, now, "intent-key-123456"), undefined);
  assert.equal(phpPackages.phpExtensionApplyInput(parsed, "php-other", 9, now, "intent-key-123456"), undefined);
  assert.deepEqual(phpPackages.phpExtensionApplyInput(parsed, "php-main", 9, now, "intent-key-123456"), {
    id: "php-main", plan_id: plan.plan_id, expected_revision: 9, idempotency_key: "intent-key-123456",
  });
  assert.equal(phpPackages.phpExtensionApplyInput(parsed, "php-main", 9, now + 30 * 60_000, "intent-key-123456"), undefined);
  const uninstallPlan = {
    ...plan,
    operation: "uninstall",
    package_pins: [],
    changes: [{ kind: "disable", target: "php-main", description: "Disable curl in this PHP instance" }],
    package_retained_reason: "Physical package removal is deferred; the package will be retained.",
  };
  assert.equal(phpPackages.parsePHPExtensionPlan(uninstallPlan, {
    id: "php-main", operation: "uninstall", module_id: "curl", expected_revision: 9,
  }).package_removable, false);
  assert.equal(phpPackages.parsePHPExtensionPlan({ ...uninstallPlan, package_retained_reason: undefined }, {
    id: "php-main", operation: "uninstall", module_id: "curl", expected_revision: 9,
  }), undefined);
  assert.equal(phpPackages.parsePHPExtensionPlan({ ...uninstallPlan, package_removable: true }, {
    id: "php-main", operation: "uninstall", module_id: "curl", expected_revision: 9,
  }), undefined);
});

test("FTP legacy recovery requires the current recovery phase and never claims prior login blocking", () => {
  const inspection = {
    id: "ftp-main",
    name: "nightly",
    revision: 6,
    recovery_required: true,
    phase: "legacy_recovery_required",
    allowed_recovery_commands: ["legacy_recover"],
  };
  const parsed = ftpAccount.parseFTPAccountInspection(inspection, "ftp-main", "nightly");
  assert.equal(parsed.last_confirmed_enabled, undefined);
  assert.deepEqual(ftpAccount.ftpLegacyRecoveryInput(parsed, "ftp-main", "nightly"), {
    id: "ftp-main",
    name: "nightly",
    expected_revision: 6,
    confirm_clear_legacy_deny_rule: true,
  });
  assert.equal(ftpAccount.parseFTPAccountInspection(inspection, "ftp-main", "other"), undefined);
  assert.equal(ftpAccount.parseFTPAccountInspection({ ...inspection, allowed_recovery_commands: ["resume"] }, "ftp-main", "nightly"), undefined);
  assert.equal(ftpAccount.parseFTPAccountInspection({ ...inspection, last_confirmed_enabled: false }, "ftp-main", "nightly"), undefined);
  assert.equal(ftpAccount.parseFTPAccountInspection({ ...inspection, phase: "legacy_paused", last_confirmed_enabled: false }, "ftp-main", "nightly"), undefined);
  assert.equal(ftpAccount.ftpLegacyRecoveryInput({ ...parsed, phase: "active" }, "ftp-main", "nightly"), undefined);
  const pending = ftpAccount.parseFTPAccountInspection({
    ...inspection,
    revision: 5,
    phase: "legacy_recovery_pending",
  }, "ftp-main", "nightly");
  assert.equal(ftpAccount.ftpLegacyRecoveryInput(pending, "ftp-main", "nightly").expected_revision, 5);
});
