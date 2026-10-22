import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ts from "typescript";
import { i18nURL } from "./i18n-loader.mjs";
const require = createRequire(import.meta.url);
const urls = { "@/lib/i18n": i18nURL, "./i18n": i18nURL };
function compile(path) {
  let code = ts.transpileModule(
    readFileSync(new URL(path, import.meta.url), "utf8"),
    {
      compilerOptions: {
        target: ts.ScriptTarget.ES2022,
        module: ts.ModuleKind.ESNext,
        jsx: ts.JsxEmit.ReactJSX,
      },
    },
  ).outputText;
  for (const [key, value] of Object.entries(urls))
    code = code.replaceAll(JSON.stringify(key), JSON.stringify(value));
  code = code.replaceAll(
    '"react/jsx-runtime"',
    JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href),
  );
  return `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
}
urls["@/lib/api"] = compile("../src/lib/api.ts");
urls["@/lib/labels"] = compile("../src/lib/labels.ts");
const { ChangeSummary } = await import(
  compile("../src/components/change-summary.tsx")
);
const { setLocale } = await import(i18nURL);
test("a real software plan presents structured version/impact and never exposes secrets", () => {
  const plan = {
    id: "plan-1",
    plugin_id: "nginx",
    instance_id: "我的网站",
    operation: "install",
    state: "ready",
    version: {
      version: "1.28.0",
      source: {
        kind: "system_package",
        packages: [{ name: "nginx", version: "1.28.0" }],
      },
    },
    changes: [
      {
        kind: "service",
        target: "nginx.service",
        description: "Install the verified service",
      },
    ],
    config: {
      administrator_password: "secret-value",
      credentials: { token: "private-token" },
      port: 8080,
    },
    expected_revision: 1,
  };
  for (const locale of ["en", "zh-CN"]) {
    setLocale(locale, false);
    const html = renderToStaticMarkup(createElement(ChangeSummary, { plan }));
    assert.ok(html.includes("nginx.service"));
    assert.ok(html.includes("1.28.0"));
    assert.ok(html.includes("我的网站"));
    assert.ok(!html.includes("[object Object]"));
    assert.ok(!html.includes("secret-value"));
    assert.ok(!html.includes("private-token"));
    assert.ok(html.includes("[redacted]"));
    assert.ok(!/<details[^>]* open/.test(html));
  }
  setLocale("zh-CN", false);
});
