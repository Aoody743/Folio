import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';

const compiled = ts.transpileModule(
  readFileSync(new URL('../src/lib/server-browser.ts', import.meta.url), 'utf8'),
  { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } },
).outputText;
const { browserAddress, browserPoint } = await import(
  `data:text/javascript;base64,${Buffer.from(compiled).toString('base64')}`,
);

test('server browser resolves explicit URLs, hostnames and Unicode searches without local navigation', () => {
  assert.equal(browserAddress('  http://127.0.0.1:8080/a?q=1  '), 'http://127.0.0.1:8080/a?q=1');
  assert.equal(browserAddress('example.com/path'), 'https://example.com/path');
  assert.equal(browserAddress('localhost:3000'), 'https://localhost:3000/');
  assert.equal(browserAddress(''), 'about:blank');
  assert.equal(browserAddress('中文 & status'), 'https://duckduckgo.com/?q=%E4%B8%AD%E6%96%87%20%26%20status');
});

test('server browser address entry rejects executable/local schemes and embedded credentials', () => {
  for (const value of ['file:///etc/passwd', 'javascript:alert(1)', 'data:text/html,hello', 'chrome://settings', 'https://user:password@example.com']) {
    assert.throws(() => browserAddress(value));
  }
});

test('scaled remote browser input tracks the remote viewport and clamps outside pointer positions', () => {
  const rect = { left: 20, top: 40, width: 320, height: 180 };
  const viewport = { width: 1280, height: 720 };
  assert.deepEqual(browserPoint(180, 130, rect, viewport), { x: 640, y: 360 });
  assert.deepEqual(browserPoint(-100, 10, rect, viewport), { x: 0, y: 0 });
  assert.deepEqual(browserPoint(900, 900, rect, viewport), { x: 1279, y: 719 });
});
