import { i18nURL } from "./i18n-loader.mjs";
import test from "node:test";
import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import ts from "typescript";
const js=ts.transpileModule(readFileSync(new URL("../src/lib/database-admin-credentials.ts",import.meta.url),"utf8"),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText.replace(/([\"\'])(?:@\/lib\/|\.\/)i18n\1/g, JSON.stringify(i18nURL));
const {databaseAdminState,databaseAdminTaskInput}=await import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);
test("administrator state only exposes known recovery actions and public CAS fields",()=>{
 assert.deepEqual(databaseAdminState({state:"active",current_credential_version:"version",available_actions:["rotate","shell"],password:"never display"}),{state:"active",current_credential_version:"version",available_actions:["rotate"]});
 assert.equal(databaseAdminState({state:"active",available_actions:["rotate"]}),undefined);
 assert.equal(databaseAdminState({state:"unknown",current_credential_version:"version",available_actions:[]}),undefined);
});
test("durable administrator input uses only opaque prepared password pin",()=>{
 const pin="a".repeat(32);
 assert.deepEqual(databaseAdminTaskInput("sql-1","v1",pin),{instance_id:"sql-1",current_credential_version:"v1",password_version:pin});
 assert.deepEqual(databaseAdminTaskInput("sql-1","v1"),{instance_id:"sql-1",current_credential_version:"v1"});
 assert.throws(()=>databaseAdminTaskInput("sql-1","v1","plaintext-password"));
});
