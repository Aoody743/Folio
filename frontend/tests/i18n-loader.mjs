import { readFileSync } from "node:fs";
import ts from "typescript";
const compile = (file) =>
  ts.transpileModule(readFileSync(new URL(file, import.meta.url), "utf8"), {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  }).outputText;
const url = (code) =>
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
const messagesURL = url(compile("../src/lib/messages.en.ts"));
export const i18nURL = url(
  compile("../src/lib/i18n.ts").replace(
    '"./messages.en"',
    JSON.stringify(messagesURL),
  ),
);
export function withI18n(code) {
  return code.replace(
    /(["'])(?:@\/lib\/|\.\/)i18n\1/g,
    JSON.stringify(i18nURL),
  );
}
