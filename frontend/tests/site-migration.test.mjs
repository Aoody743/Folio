import { i18nURL } from "./i18n-loader.mjs";
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

const compiled = ts.transpileModule(
  readFileSync(new URL("../src/lib/site-migration.ts", import.meta.url), "utf8"),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText.replace(/([\"\'])(?:@\/lib\/|\.\/)i18n\1/g, JSON.stringify(i18nURL));
const migration = await import(
  `data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`
);

const plan = {
  id: "m-0123456789abcdef0123456789abcdef",
  site_id: "site-main",
  site_name: "Main site",
  expected_revision: 7,
  source_web_instance: "web-old",
  target_web_instance: "web-new",
  php_instance: "php-shared",
  tls_certificate_id: "cert-main",
  tls_certificate_revision: 4,
  domains: ["example.test", "www.example.test"],
  default: true,
  tls_enabled: true,
  configuration_mode: "managed",
  reset_advanced_config: false,
  expires_at: "2026-10-14T13:00:00Z",
};

test("a migration plan can apply only to its original site revision before expiry", () => {
  const parsed = migration.parseSiteMigrationPlan(plan);
  assert.ok(parsed);
  assert.equal(
    migration.siteMigrationGate(parsed, { id: "site-main", revision: 7, web_instance: "web-old" }, Date.parse("2026-10-14T12:00:00Z")),
    undefined,
  );
  assert.equal(
    migration.siteMigrationGate(parsed, { id: "site-main", revision: 8, web_instance: "web-old" }, Date.parse("2026-10-14T12:00:00Z")),
    "site_changed",
  );
  assert.equal(
    migration.siteMigrationGate(parsed, { id: "another-site", revision: 7, web_instance: "web-old" }, Date.parse("2026-10-14T12:00:00Z")),
    "foreign_site",
  );
  assert.equal(
    migration.siteMigrationGate(parsed, { id: "site-main", revision: 7, web_instance: "web-old" }, Date.parse("2026-10-14T13:00:00Z")),
    "expired",
  );
});

test("the fetched protected plan must still match the exact reviewed snapshot", () => {
  const reviewed = migration.parseSiteMigrationPlan(plan);
  assert.ok(reviewed);
  assert.equal(
    migration.sameSiteMigrationPlan(reviewed, migration.parseSiteMigrationPlan({ ...plan })),
    true,
  );
  assert.equal(
    migration.sameSiteMigrationPlan(
      reviewed,
      migration.parseSiteMigrationPlan({ ...plan, target_web_instance: "web-other" }),
    ),
    false,
  );
  assert.equal(
    migration.sameSiteMigrationPlan(
      reviewed,
      migration.parseSiteMigrationPlan({ ...plan, domains: ["changed.test"] }),
    ),
    false,
  );
});

test("plan parsing rejects malformed identities and projects only review-safe fields", () => {
  assert.equal(migration.parseSiteMigrationPlan({ ...plan, id: "not-a-plan" }), undefined);
  const parsed = migration.parseSiteMigrationPlan({ ...plan, runtime_path: "/private/path" });
  assert.ok(parsed);
  assert.equal(Object.hasOwn(parsed, "runtime_path"), false);
});

test("migration actions stay behind the dedicated review flow while other site actions remain visible", () => {
  for (const name of [
    "sites_migration_plan",
    "sites_migration_get",
    "sites_migration_apply",
  ])
    assert.equal(migration.isDedicatedSiteMigrationAction(name), true, name);
  for (const name of ["sites_list", "sites_update", "sites_backups_list"])
    assert.equal(migration.isDedicatedSiteMigrationAction(name), false, name);
});
