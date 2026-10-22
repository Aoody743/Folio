import { i18nURL } from "./i18n-loader.mjs";
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";
const compiled = ts.transpileModule(
  readFileSync(new URL("../src/lib/schema.ts", import.meta.url), "utf8"),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText.replace(/([\"\'])(?:@\/lib\/|\.\/)i18n\1/g, JSON.stringify(i18nURL));
const { normalizeInput, initialValue, projectInput } = await import(
  `data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`
);
test("nested resource schema keeps optional omission and preserves intentional empty file content", () => {
  const schema = {
    type: "object",
    required: ["path", "content"],
    additionalProperties: false,
    properties: {
      path: { type: "string", minLength: 1 },
      content: { type: "string" },
      recursive: { type: "boolean" },
      rules: {
        type: "array",
        maxItems: 2,
        items: {
          type: "object",
          required: ["port"],
          additionalProperties: false,
          properties: { port: { type: "integer", minimum: 1, maximum: 65535 } },
        },
      },
    },
  };
  const input = normalizeInput(schema, {
    path: "empty.txt",
    content: "",
    rules: [{ port: "443" }],
  });
  assert.deepEqual(JSON.parse(JSON.stringify(input)), {
    path: "empty.txt",
    content: "",
    rules: [{ port: 443 }],
  });
  assert.equal(Object.hasOwn(input, "recursive"), false);
  assert.throws(
    () =>
      normalizeInput(schema, {
        path: "x",
        content: "",
        rules: [{ port: "65536" }],
      }),
    /超出允许范围/,
  );
  assert.throws(
    () =>
      normalizeInput(schema, { path: "x", content: "", unauthorized: true }),
    /不是有效字段/,
  );
  assert.equal(Object.hasOwn(initialValue(schema), "recursive"), false);
});
test("map schemas reject invalid values and enforce limits without prototype mutation", () => {
  const schema = {
    type: "object",
    maxProperties: 2,
    additionalProperties: { type: "string", maxLength: 4 },
  };
  const input = normalizeInput(
    schema,
    JSON.parse('{"__proto__":"safe","X":"ok"}'),
  );
  assert.equal(Object.getPrototypeOf(input), null);
  assert.equal(input.__proto__, "safe");
  assert.throws(() => normalizeInput(schema, { X: 42 }), /需要文本/);
  assert.throws(() => normalizeInput(schema, { X: "longer" }), /长度超出/);
  assert.throws(
    () => normalizeInput(schema, { A: "1", B: "2", C: "3" }),
    /字段数量/,
  );
});
test("union schemas distinguish exactly one branch and allow declared null", () => {
  assert.equal(normalizeInput({ type: ["string", "null"] }, null), null);
  const schema = {
    oneOf: [
      { type: "string", minLength: 2 },
      { type: "integer", minimum: 1 },
    ],
  };
  assert.equal(normalizeInput(schema, "1"), 1);
  assert.throws(
    () =>
      normalizeInput(
        { oneOf: [{ type: "number" }, { type: "integer" }] },
        "12",
      ),
    /联合类型/,
  );
});
test("resource presets discard server-only metadata and nested credential references", () => {
  const schema = {
    type: "object",
    additionalProperties: false,
    properties: {
      id: { type: "string" },
      expected_revision: { type: "integer" },
      protection: {
        type: "object",
        additionalProperties: false,
        properties: {
          enabled: { type: "boolean" },
          username: { type: "string" },
          password: { type: "string" },
        },
      },
    },
  };
  const projected = projectInput(schema, {
    id: "website",
    revision: 4,
    expected_revision: 4,
    credential_ref: "internal",
    protection: {
      enabled: true,
      username: "admin",
      password_ref: "internal-password-reference",
    },
  });
  assert.deepEqual(JSON.parse(JSON.stringify(projected)), {
    id: "website",
    expected_revision: 4,
    protection: { enabled: true, username: "admin" },
  });
});
test("container CPU bounds reject zero and non-finite values while allowing fractional limits", () => {
  const schema = { type: "number", exclusiveMinimum: 0, maximum: 16 };
  assert.equal(normalizeInput(schema, "0.5"), 0.5);
  assert.throws(() => normalizeInput(schema, "0"), /超出允许范围/);
  assert.throws(() => normalizeInput(schema, "Infinity"), /需要数字/);
  assert.throws(() => normalizeInput(schema, "16.1"), /超出允许范围/);
});
