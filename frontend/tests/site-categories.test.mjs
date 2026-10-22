import { i18nURL } from "./i18n-loader.mjs";
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

const schemaJS = ts.transpileModule(
  readFileSync(new URL("../src/lib/schema.ts", import.meta.url), "utf8"),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText.replace(/([\"\'])(?:@\/lib\/|\.\/)i18n\1/g, JSON.stringify(i18nURL));
const schemaURL = `data:text/javascript;base64,${Buffer.from(schemaJS).toString("base64")}`;
const source = readFileSync(
  new URL("../src/lib/site-categories.ts", import.meta.url),
  "utf8",
).replace(
  'import { normalizeInput, type Schema } from "./schema";',
  `import { normalizeInput } from "${schemaURL}";`,
);
const js = ts.transpileModule(source, {
  compilerOptions: {
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.ESNext,
  },
}).outputText.replace(/([\"\'])(?:@\/lib\/|\.\/)i18n\1/g, JSON.stringify(i18nURL));
const categories = await import(
  `data:text/javascript;base64,${Buffer.from(js).toString("base64")}`
);
const { APIError } = await import(
  `data:text/javascript;base64,${Buffer.from(
    ts.transpileModule(
      readFileSync(new URL("../src/lib/api.ts", import.meta.url), "utf8"),
      {
        compilerOptions: {
          target: ts.ScriptTarget.ES2022,
          module: ts.ModuleKind.ESNext,
        },
      },
    ).outputText.replace(/([\"\'])(?:@\/lib\/|\.\/)i18n\1/g, JSON.stringify(i18nURL)),
  ).toString("base64")}`
);

const listCapability = {
  name: "sites_categories_list",
  read_only: true,
  input_schema: { type: "object", properties: {} },
};
const saveCapability = {
  name: "sites_category_save",
  input_schema: {
    type: "object",
    additionalProperties: false,
    required: ["id", "expected_revision", "name"],
    properties: {
      id: { type: "string", pattern: "^[a-z][a-z0-9_-]{0,95}$" },
      expected_revision: { type: "integer", minimum: 0 },
      name: { type: "string", minLength: 1, maxLength: 160 },
    },
  },
};
const deleteCapability = {
  name: "sites_category_delete",
  input_schema: {
    type: "object",
    additionalProperties: false,
    required: ["id", "expected_revision"],
    properties: {
      id: { type: "string", pattern: "^[a-z][a-z0-9_-]{0,95}$" },
      expected_revision: { type: "integer", minimum: 0 },
    },
  },
};

test("category actions and server filter are gated by current capability schemas", () => {
  assert.deepEqual(
    categories.siteCategoryCapabilities([
      listCapability,
      saveCapability,
      deleteCapability,
      { name: "sites_category_create" },
    ]),
    { list: listCapability, save: saveCapability, delete: deleteCapability },
  );
  const siteList = {
    name: "sites_list",
    input_schema: {
      properties: {
        limit: {},
        offset: {},
        query: {},
        category_id: {},
      },
    },
  };
  assert.equal(categories.supportsSiteCategoryFilter(siteList), true);
  assert.equal(categories.supportsSiteCategoryFilter({ name: "sites_list" }), false);
  assert.deepEqual(
    categories.siteListInput(siteList, {
      limit: 20,
      offset: 40,
      query: "docs",
      categoryID: "docs",
    }),
    { limit: 20, offset: 40, query: "docs", category_id: "docs" },
  );
  assert.deepEqual(
    categories.siteListInput(siteList, {
      limit: 20,
      offset: 0,
      query: "",
      categoryID: "",
    }),
    { limit: 20, offset: 0 },
    "empty category means all categories and is omitted from the request",
  );
  assert.deepEqual(
    categories.siteListInput(
      { name: "sites_list", input_schema: { properties: { offset: {} } } },
      { limit: 20, offset: 0, query: "x", categoryID: "docs" },
    ),
    { offset: 0 },
    "unsupported filters are not sent to the server",
  );
});

test("site assignment choices contain only visible categories and a clear option", () => {
  const visible = [{ id: "docs", name: "文档", revision: 3 }];
  assert.deepEqual(categories.siteCategoryReferences(visible), {
    category_id: [
      { value: "", label: "未分类" },
      { value: "docs", label: "文档 · docs" },
    ],
  });
  assert.deepEqual(categories.siteCategoryReferences([]).category_id, [
    { value: "", label: "未分类" },
  ]);
});

test("generated IDs fit the website category identity contract", () => {
  const id = categories.newSiteCategoryID("2d5f4c08-4ac8-4f18-ae12-61f53d90a7ad");
  assert.equal(id, "cat-2d5f4c08-4ac8-4f18-ae12-61f53d90a7ad");
  assert.match(id, /^[a-z][a-z0-9_-]{0,95}$/);
  assert.throws(
    () => categories.newSiteCategoryID("not-a-uuid"),
    /有效的网站分类标识/,
  );
});

test("save and delete payloads are normalized against the live action schema", () => {
  assert.deepEqual(
    JSON.parse(
      JSON.stringify(
        categories.siteCategoryMutationInput(saveCapability, {
          id: "cat-example",
          expected_revision: "4",
          name: " 文档 ",
        }),
      ),
    ),
    { id: "cat-example", expected_revision: 4, name: " 文档 " },
  );
  assert.deepEqual(
    JSON.parse(
      JSON.stringify(
        categories.siteCategoryMutationInput(deleteCapability, {
          id: "cat-example",
          expected_revision: 4,
        }),
      ),
    ),
    { id: "cat-example", expected_revision: 4 },
  );
  assert.throws(
    () =>
      categories.siteCategoryMutationInput(saveCapability, {
        id: "cat-example",
        expected_revision: 4,
        name: "docs",
        extra: "not from the live schema",
      }),
    /不是有效字段/,
  );
  assert.throws(
    () =>
      categories.siteCategoryMutationInput(
        { ...saveCapability, name: "sites_category_create" },
        { id: "cat-example", expected_revision: 0, name: "docs" },
      ),
    /没有可用的网站分类写入能力/,
  );
});

test("permission and revision errors preserve the draft, refetch, and explain backend rejection", () => {
  for (const status of [403, 409, 412])
    assert.equal(
      categories.shouldReloadSiteCategoriesAfterError(new APIError("failure", "error", status)),
      true,
      String(status),
    );
  assert.equal(
    categories.shouldReloadSiteCategoriesAfterError(new APIError("bad_request", "error", 400)),
    false,
  );
  assert.equal(
    categories.isSiteCategoryRevisionConflict(
      new APIError("revision_conflict", "changed", 412),
    ),
    true,
  );
  assert.equal(
    categories.isSiteCategoryRevisionConflict(
      new APIError("category_in_use", "still referenced", 409),
    ),
    false,
    "a live site reference is not a revision conflict",
  );
  assert.match(
    categories.siteCategoryErrorMessage(
      new APIError("category_in_use", "Move the category's websites before deleting it", 409),
    ),
    /仍被网站使用/,
  );
  assert.match(
    categories.siteCategoryErrorMessage(
      new APIError("revision_conflict", "Category revision changed", 412),
    ),
    /版本已变化，草稿已保留/,
  );
  assert.match(
    categories.siteCategoryErrorMessage(new APIError("forbidden", "denied", 403)),
    /无权.*草稿已保留/,
  );
});
