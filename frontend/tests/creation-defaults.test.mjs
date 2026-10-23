import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';
import { i18nURL } from './i18n-loader.mjs';
async function compile(name) {
  const js=ts.transpileModule(readFileSync(new URL(`../src/lib/${name}.ts`,import.meta.url),'utf8'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText.replace('"./i18n"',JSON.stringify(i18nURL));
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
}
const {resourceIdentity,websiteIdentity,domainLines}=await compile('resource-identity');
const {tokenPresetScopes,scopeLabel}=await compile('token-presets');
const {setLocale}=await import(i18nURL);
test('creation defaults are stable valid identifiers for Unicode, wildcards and long names',()=>{
  assert.equal(resourceIdentity('', 'app'),'');
  assert.equal(resourceIdentity('Customer Portal', 'app'),'app-customer-portal');
  assert.equal(resourceIdentity('Café API','app'),'app-cafe-api');
  assert.match(resourceIdentity('我的应用','app'),/^app-[a-z0-9]+$/);
  assert.equal(resourceIdentity('我的应用','app'),resourceIdentity('我的应用','app'));
  assert.notEqual(resourceIdentity('我的应用','app'),resourceIdentity('另一应用','app'));
  const long=websiteIdentity('x'.repeat(64)+'.example.com');
  assert.ok(long.length<=64);
  assert.match(long,/^[a-z][a-z0-9_-]*$/);
  assert.match(websiteIdentity('例子.测试'),/^site-xn-/);
  assert.notEqual(websiteIdentity('*.example.com'),websiteIdentity('example.com'));
  assert.deepEqual(domainLines('example.com\nwww.example.com,example.com，api.example.com '),['example.com','www.example.com','api.example.com']);
});
test('MCP purpose presets only use scopes already supplied by the authorized capability list',()=>{
  assert.deepEqual(tokenPresetScopes('monitor',['system:read','notes:write','*']),['system:read']);
  assert.deepEqual(tokenPresetScopes('notes',['system:read','notes:read']),['notes:read']);
  assert.deepEqual(tokenPresetScopes('notes',[]),[]);
  assert.deepEqual(tokenPresetScopes('custom',['system:read']),[]);
  setLocale('en',false);
  assert.equal(scopeLabel('system:read'),'Host · View');
  assert.equal(scopeLabel('extension:custom'),'extension:custom');
  setLocale('zh-CN',false);
});
