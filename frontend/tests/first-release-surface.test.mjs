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

test("selected application and certificate actions are visible in the first-release panel", () => {
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
    [
      "sites_list",
      "certificates_import",
      "certificates_acme_issue",
      "certificates_dns_credentials_list",
      "hosted_apps_create",
      "terminal_open",
    ],
  );
});

test("unsupported integrations stay hidden while selected runtimes are visible", () => {
  for (const id of [
	"acme_pandominassl_apply",
    "mongodb",
    "phpmyadmin",
    "pgadmin",
    "pureftp",
    "backup_ftp",
    "data_query",
    "webssh",
    "php-yum",
  ]) {
    assert.equal(surface.isFirstReleaseCatalogPlugin(id), false, id);
  }
  assert.equal(surface.isFirstReleaseCatalogPlugin("nginx"), true);
  assert.equal(surface.isFirstReleaseCatalogPlugin("postgresql"), true);
  assert.equal(surface.isFirstReleaseCatalogPlugin("git"), true);
  assert.equal(surface.isFirstReleaseCatalogPlugin("nodejs"), true);
  assert.equal(surface.isFirstReleaseCatalogPlugin("golang"), true);
});
