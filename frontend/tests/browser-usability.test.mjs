import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import ts from 'typescript';
const js=ts.transpileModule(readFileSync(new URL('../src/lib/browser-usability.ts',import.meta.url),'utf8'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText;
const {wasmCompatibility,bookmarkURL,readBookmarks}=await import('data:text/javascript;base64,'+Buffer.from(js).toString('base64'));
test('WASM launch requires secure context and both JSPI APIs',()=>{
  const api={Suspending:()=>{},promising:()=>{}};
  assert.equal(wasmCompatibility(false,api),'insecure');
  assert.equal(wasmCompatibility(true,{}),'unsupported');
  assert.equal(wasmCompatibility(true,{promising:api.promising}),'unsupported');
  assert.equal(wasmCompatibility(true,api),'ready');
});
test('bookmarks do not persist query tokens, fragments, credentials or unsafe protocols',()=>{
  assert.equal(bookmarkURL('https://example.com/docs?q=secret#access_token=token'),'https://example.com/docs');
  assert.equal(bookmarkURL('https://user:pass@example.com'),undefined);
  assert.equal(bookmarkURL('javascript:alert(1)'),undefined);
  assert.equal(bookmarkURL('file:///etc/passwd'),undefined);
  assert.deepEqual(readBookmarks(['https://example.com/?a=1','https://example.com/#x',null,42,'https://u:p@example.com']),['https://example.com/']);
  assert.equal(readBookmarks(Array.from({length:40},(_,i)=>`https://example.com/${i}`)).length,12);
});
