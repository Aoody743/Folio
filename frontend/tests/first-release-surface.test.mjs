import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

const source = readFileSync(
  new URL("../src/lib/first-release-surface.ts", import.meta.url),
  "utf8",
);
const compiled = ts.transpileModule(source, {
  compilerOptions: {
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.ESNext,
  },
}).outputText;
const surface = await import(
  `data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`
);

test("deferred RC15 features stay out of the first-release panel", () => {
  const capabilities = [
    { name: "sites_list" },
    { name: "certificates_import" },
    { name: "certificates_acme_issue" },
    { name: "certificates_dns_credentials_list" },
    { name: "sites_migration_plan" },
    { name: "hosted_apps_create" },
    { name: "backup_ftp_transfer" },
    { name: "ftp_create" },
    { name: "mongodb_documents_find" },
    { name: "nosql_mongodb.get_db_list" },
    { name: "ssh_server_configure" },
    { name: "terminal_open" },
  ];

  assert.deepEqual(
    surface.firstReleaseCapabilities(capabilities).map((item) => item.name),
    ["sites_list", "certificates_import", "terminal_open"],
  );
});

test("deferred catalog integrations are hidden while core packages remain", () => {
  for (const id of [
    "mongodb",
    "phpmyadmin",
    "pgadmin",
    "pureftp",
    "backup_ftp",
    "acme_pandominassl_apply",
    "data_query",
    "webssh",
    "git",
    "nodejs",
    "golang",
    "php-yum",
  ]) {
    assert.equal(surface.isFirstReleaseCatalogPlugin(id), false, id);
  }
  assert.equal(surface.isFirstReleaseCatalogPlugin("nginx"), true);
  assert.equal(surface.isFirstReleaseCatalogPlugin("postgresql"), true);
});
