import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";
import { i18nURL } from "./i18n-loader.mjs";
const { t, setLocale, getLocale, subscribeLocale, initializeLocale } =
  await import(i18nURL);
const compiled = ts
  .transpileModule(
    readFileSync(new URL("../src/lib/labels.ts", import.meta.url), "utf8"),
    {
      compilerOptions: {
        target: ts.ScriptTarget.ES2022,
        module: ts.ModuleKind.ESNext,
      },
    },
  )
  .outputText.replace('"./i18n"', JSON.stringify(i18nURL));
const { fieldLabel, actionLabel, choiceLabel } = await import(
  `data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`
);
test.afterEach(() => {
  setLocale("zh-CN", false);
});
test("language changes update existing label maps and never translate interpolated resource data", () => {
  assert.equal(fieldLabel("name"), "名称");
  let notifications = 0;
  const stop = subscribeLocale(() => notifications++);
  setLocale("en", false);
  assert.equal(getLocale(), "en");
  assert.equal(fieldLabel("name"), "Name");
  assert.equal(
    actionLabel({ name: "software_apply", description: "确认应用软件计划" }),
    "Apply changes",
  );
  assert.equal(choiceLabel("install"), "Install");
  assert.equal(t("共 {0}", ["网站 {1}"]), "Total 网站 {1}");
  assert.equal(t("用户网站名称"), "用户网站名称");
  setLocale("en", false);
  assert.equal(notifications, 1);
  stop();
  setLocale("zh-CN", false);
  assert.equal(fieldLabel("name"), "名称");
});
test("an explicit browser preference wins and disabled storage still permits switching", () => {
  const documentBefore = globalThis.document,
    storageBefore = globalThis.localStorage,
    navigatorBefore = Object.getOwnPropertyDescriptor(globalThis, "navigator");
  try {
    globalThis.document = { documentElement: { lang: "" }, title: "" };
    globalThis.localStorage = {
      getItem: () => "zh-CN",
      setItem: () => {
        throw new Error("blocked");
      },
    };
    Object.defineProperty(globalThis, "navigator", {
      configurable: true,
      value: { language: "en-US" },
    });
    initializeLocale();
    assert.equal(getLocale(), "zh-CN");
    setLocale("en");
    assert.equal(document.documentElement.lang, "en");
    assert.equal(document.title, "Folio");
    setLocale("zh-CN");
    assert.equal(document.title, "折纸面板");
    setLocale("en");
    globalThis.localStorage.getItem = () => {
      throw new Error("blocked");
    };
    initializeLocale();
    assert.equal(getLocale(), "en");
  } finally {
    globalThis.document = documentBefore;
    globalThis.localStorage = storageBefore;
    if (navigatorBefore)
      Object.defineProperty(globalThis, "navigator", navigatorBefore);
  }
});
test("every application translation preserves interpolation slots", () => {
  const source = ts.transpileModule(
    readFileSync(new URL("../src/lib/messages.en.ts", import.meta.url), "utf8"),
    {
      compilerOptions: {
        target: ts.ScriptTarget.ES2022,
        module: ts.ModuleKind.CommonJS,
      },
    },
  ).outputText;
  const exports = {};
  new Function("exports", source)(exports);
  for (const [key, value] of Object.entries(exports.english)) {
    assert.deepEqual(
      (value.match(/\{\d+\}/g) ?? []).sort(),
      (key.match(/\{\d+\}/g) ?? []).sort(),
      key,
    );
    assert.ok(value.trim(), key);
  }
});

test("application calls have English catalog coverage", async () => {
  const { english } = await import(
    `data:text/javascript;base64,${Buffer.from(ts.transpileModule(readFileSync(new URL("../src/lib/messages.en.ts", import.meta.url), "utf8"), { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText).toString("base64")}`
  );
  const { readdirSync } = await import("node:fs");
  function scan(dir) {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = new URL(entry.name + (entry.isDirectory() ? "/" : ""), dir);
      if (entry.isDirectory()) scan(path);
      else if (
        /\.tsx?$/.test(entry.name) &&
        !/^(messages.en|i18n)\.ts$/.test(entry.name)
      ) {
        const sf = ts.createSourceFile(
          entry.name,
          readFileSync(path, "utf8"),
          ts.ScriptTarget.Latest,
          true,
        );
        function walk(node) {
          if (
            ts.isCallExpression(node) &&
            ts.isIdentifier(node.expression) &&
            node.expression.text === "translate"
          ) {
            const first = node.arguments[0];
            if (
              first &&
              ts.isStringLiteral(first) &&
              /[\u3400-\u9fff]/.test(first.text)
            )
              assert.ok(
                Object.hasOwn(english, first.text),
                `${path.pathname}: ${first.text}`,
              );
          }
          ts.forEachChild(node, walk);
        }
        walk(sf);
      }
    }
  }
  scan(new URL("../src/", import.meta.url));
});
