import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';
const compile = async name => {
  const source = ts.transpileModule(readFileSync(new URL(`../src/lib/${name}.ts`, import.meta.url), 'utf8'), { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText.replace('"react"', JSON.stringify(new URL('../node_modules/react/index.js', import.meta.url).href));
  return import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`);
};
const { parseRoute, routeHash, navigate, updateRoute } = await compile('navigation');
const { pageItems } = await compile('collection');
const { applicationCounts, applicationState } = await compile('overview');
test('static hash routes round-trip Unicode names and discard non-view fields', () => {
  const route = parseRoute('#/sites?q=%E4%B8%AD%E6%96%87&offset=12&id=site-1&password=secret&sql=DROP&draft=text');
  assert.deepEqual(route, { section: 'sites', params: { q: '中文', offset: '12', id: 'site-1' } });
  assert.deepEqual(parseRoute(routeHash(route)), route);
  assert.equal(parseRoute('#temporary_login=secret').section, 'overview');
  assert.equal(parseRoute('#/missing').section, 'overview');
});
test('navigation restores each list and patches the latest route without dropping earlier updates', () => {
  const oldLocation = globalThis.location, oldHistory = globalThis.history;
  try {
    globalThis.location = { hash: '#/sites?q=prod&offset=12' };
    const writes = [];
    globalThis.history = Object.fromEntries(['pushState','replaceState'].map(method => [method, (_state, _title, hash) => { writes.push(method); globalThis.location.hash = hash; }]));
    updateRoute('sites', 'id', 'site-1', true);
    updateRoute('sites', 'group', 'logs');
    assert.deepEqual(parseRoute(location.hash).params, { q:'prod',offset:'12',id:'site-1',group:'logs' });
    navigate('files', { root:'www', path:'a/b' });
    updateRoute('sites', 'q', 'hidden-view');
    assert.equal(parseRoute(location.hash).section, 'files');
    navigate('sites');
    assert.equal(parseRoute(location.hash).params.q, 'prod');
    assert.equal(writes[0], 'pushState');
    assert.equal(writes[1], 'replaceState');
  } finally { globalThis.location = oldLocation; globalThis.history = oldHistory; }
});
test('client pagination handles deleted last pages, empty collections and full coverage', () => {
  const data = Array.from({length:17}, (_,i)=>i);
  assert.deepEqual([0,6,12].flatMap(offset => pageItems(data,offset,6).items), data);
  assert.deepEqual(pageItems([1,2],12,6), {items:[1,2],offset:0,total:2});
  assert.deepEqual(pageItems([],12,6), {items:[],offset:0,total:0});
});
test('application summaries preserve conflicts, transitional and unobserved states', () => {
  assert.equal(applicationState({status:'running',running:false}), 'unknown');
  assert.equal(applicationState({status:'running'}), 'unknown');
  assert.equal(applicationState({status:'building',running:true}), 'unknown');
  assert.deepEqual(applicationCounts([{status:'running',running:true},{status:'built',running:false},{status:'failed',running:false},{status:'stopped',running:true}]), {running:1,stopped:1,unknown:2});
});
