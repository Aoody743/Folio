import { i18nURL } from "./i18n-loader.mjs";
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

const js = ts.transpileModule(
  readFileSync(
    new URL("../src/lib/site-backup-download.ts", import.meta.url),
    "utf8",
  ),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText.replace(/([\"\'])(?:@\/lib\/|\.\/)i18n\1/g, JSON.stringify(i18nURL));
const { verifiedSiteBackupDownloadHref } = await import(
  `data:text/javascript;base64,${Buffer.from(js).toString("base64")}`
);

const origin = "https://panel.example";
const expected = {
  siteID: "site-a",
  backupID: "b-123",
  size: 4096,
  sha256: "a".repeat(64),
};
const response = (downloadURL) => ({
  backup: {
    id: expected.backupID,
    site_id: expected.siteID,
    size: expected.size,
    sha256: expected.sha256,
    includes_tls: false,
  },
  download_url: downloadURL,
});
const path = `/api/v1/sites/backups/download?id=${expected.siteID}&backup_id=${expected.backupID}`;

test("accepts matching backup metadata and returns the fixed same-origin route", () => {
  assert.equal(
    verifiedSiteBackupDownloadHref(response(path), expected, origin),
    path,
  );
  assert.equal(
    verifiedSiteBackupDownloadHref(
      response(`${origin}${path}`),
      expected,
      origin,
    ),
    path,
  );
});

test("rejects mismatched metadata and unsafe or non-fixed download URLs", () => {
  for (const value of [
    { ...response(path), backup: { ...response(path).backup, site_id: "other" } },
    { ...response(path), backup: { ...response(path).backup, size: 0 } },
    { ...response(path), backup: { ...response(path).backup, includes_tls: true } },
    response(`https://other.example${path}`),
    response(`https://user@panel.example${path}`),
    response(`${path}&token=secret`),
    response(`${path}&id=other`),
    response(`/api/v1/files/download?id=${expected.siteID}&backup_id=${expected.backupID}`),
    response(`${path}#fragment`),
  ]) {
    assert.throws(
      () => verifiedSiteBackupDownloadHref(value, expected, origin),
      /网站备份信息无效/,
    );
  }
});
