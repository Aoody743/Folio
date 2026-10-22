import { i18nURL } from "./i18n-loader.mjs";
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";
const code = ts.transpileModule(
  readFileSync(new URL("../src/lib/auth-hints.ts", import.meta.url), "utf8"),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText.replace(/([\"\'])(?:@\/lib\/|\.\/)i18n\1/g, JSON.stringify(i18nURL));
const { loginHints, authorizationReturn } = await import(
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
);
test("login credentials are consumed only from fragment and removed from remaining URL", () => {
  assert.deepEqual(
    loginHints(
      "#temporary_login=test%2Bsingle-use&login_entry=private-entry&view=notes",
    ),
    {
      temporaryToken: "test+single-use",
      loginEntry: "private-entry",
      remainingHash: "#view=notes",
    },
  );
  assert.equal(loginHints("#main").remainingHash, "#main");
  assert.equal(loginHints("#login_entry=private-entry").remainingHash, "");
});
test("post authentication return accepts only a local authorization path", () => {
  assert.equal(
    authorizationReturn(
      "?return_to=" + encodeURIComponent("/oauth/authorize?client_id=client"),
    ),
    "/oauth/authorize?client_id=client",
  );
  for (const destination of [
    "https://attacker.example",
    "//attacker.example",
    "/oauth/authorize?x=\\attacker",
    "/settings",
  ]) {
    assert.equal(
      authorizationReturn("?return_to=" + encodeURIComponent(destination)),
      undefined,
    );
  }
});
