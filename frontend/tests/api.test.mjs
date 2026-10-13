import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";
const js = ts.transpileModule(
  readFileSync(new URL("../src/lib/api.ts", import.meta.url), "utf8"),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText;
const { api, setCSRF, APIError, source, action, streamRequest } = await import(
  `data:text/javascript;base64,${Buffer.from(js).toString("base64")}`
);
const realFetch = globalThis.fetch;
test.afterEach(() => {
  globalThis.fetch = realFetch;
  setCSRF();
});
test("cookie write uses same origin credentials and session CSRF, preserving revision", async () => {
  setCSRF("test-session-csrf");
  globalThis.fetch = async (url, options) => {
    assert.equal(url, "/api/v1/notes/note-1");
    assert.equal(options.credentials, "same-origin");
    assert.equal(options.cache, "no-store");
    assert.equal(options.headers["X-CSRF-Token"], "test-session-csrf");
    assert.deepEqual(JSON.parse(options.body), {
      title: "交接信息",
      expected_revision: 4,
    });
    return new Response(JSON.stringify({ data: { revision: 5 } }), {
      status: 200,
    });
  };
  assert.deepEqual(
    await api("/notes/note-1", {
      method: "PATCH",
      body: { title: "交接信息", expected_revision: 4 },
    }),
    { revision: 5 },
  );
});
test("412 retains structured current note so UI cannot silently overwrite", async () => {
  globalThis.fetch = async () =>
    new Response(
      JSON.stringify({
        error: {
          code: "revision_conflict",
          message: "Version changed",
          details: { current: { id: "n", revision: 9 } },
        },
      }),
      { status: 412 },
    );
  await assert.rejects(
    api("/notes/n", { method: "PATCH", body: { expected_revision: 8 } }),
    (e) =>
      e instanceof APIError &&
      e.status === 412 &&
      e.code === "revision_conflict" &&
      e.details.current.revision === 9,
  );
});
test("invalid or failed transport cannot be reported as operation success", async () => {
  globalThis.fetch = async () =>
    new Response("<html>gateway error</html>", { status: 502 });
  await assert.rejects(
    api("/tasks"),
    (e) =>
      e instanceof APIError &&
      e.code === "invalid_response" &&
      e.status === 502,
  );
  globalThis.fetch = async () => {
    throw new TypeError("Failed to fetch");
  };
  await assert.rejects(
    api("/notes", { method: "POST", body: { title: "草稿" } }),
    (e) => e instanceof APIError && e.code === "network_error",
  );
});
test("identity source distinguishes human, MCP, API and unknown actors", () => {
  assert.equal(
    source({ id: "human-1", kind: "human", name: "管理员" }),
    "人 · 管理员",
  );
  assert.equal(
    source({ id: "mcp-1", kind: "mcp", name: "运维客户端" }),
    "AI · 运维客户端",
  );
  assert.equal(
    source({ id: "api-1", kind: "api", name: "自动化" }),
    "客户端 · 自动化",
  );
  assert.equal(
    source({ id: "service-1", kind: "service", name: "其他来源" }),
    "客户端 · 其他来源",
  );
});
test("action retry preserves caller intent and file stream keeps raw body with session CSRF", async () => {
  setCSRF("stream-test-csrf");
  let requests = 0;
  globalThis.fetch = async (url, options) => {
    if (url === "/api/v1/actions/fs_edit") {
      assert.equal(options.headers["Idempotency-Key"], "same-user-intent");
      assert.equal(options.headers["X-CSRF-Token"], "stream-test-csrf");
      requests++;
      if (requests === 1) throw new TypeError("connection interrupted");
      return new Response(JSON.stringify({ data: { sha256: "verified" } }));
    }
    assert.equal(url, "/api/v1/files/uploads/upload/chunk?offset=0");
    assert.equal(options.headers["Content-Type"], "application/octet-stream");
    assert.equal(options.headers["X-CSRF-Token"], "stream-test-csrf");
    assert.equal(options.credentials, "same-origin");
    assert.deepEqual([...options.body], [0, 255, 17]);
    return new Response(JSON.stringify({ data: { offset: 3 } }));
  };
  const input = {
    root: "workspace",
    path: "empty.txt",
    content: "",
    expected_hash: "baseline",
  };
  await assert.rejects(
    action("fs_edit", input, { idempotencyKey: "same-user-intent" }),
  );
  assert.deepEqual(
    await action("fs_edit", input, { idempotencyKey: "same-user-intent" }),
    { sha256: "verified" },
  );
  const response = await streamRequest(
    "/api/v1/files/uploads/upload/chunk?offset=0",
    {
      method: "PATCH",
      headers: { "Content-Type": "application/octet-stream" },
      body: new Uint8Array([0, 255, 17]),
    },
  );
  assert.equal(response.status, 200);
});
