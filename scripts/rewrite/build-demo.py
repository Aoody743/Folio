#!/usr/bin/env python3
"""Package the actual static UI with an isolated, browser-local demo transport."""
import argparse
import ast
import hashlib
import json
from pathlib import Path
import shutil
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tests/rewrite'))
from fixture import RunningServer
from http_client import APIClient, data

parser = argparse.ArgumentParser()
parser.add_argument('--binary', required=True)
parser.add_argument('--output', required=True)
args = parser.parse_args()
output = Path(args.output).resolve()
if output == ROOT or output in ROOT.parents or (output / '.git').exists():
    parser.error('Use a dedicated demo output directory, outside repository roots')
if output.exists() and any(output.iterdir()) and not (output / 'demo/runtime.js').is_file():
    parser.error('Refusing to replace a nonempty directory without an existing demo build')
assert (ROOT / 'frontend/out/index.html').is_file(), 'Build the production frontend first'

# Reuse the screenshot fixture content without importing its server setup or capture loop.
names = {'now', 'actor', 'sites', 'instances', 'apps', 'dbs', 'caches', 'file_rows', 'samples', 'fixture_actions'}
module = ast.parse((ROOT / 'tests/rewrite/capture_panel_gallery.py').read_text())
assignments = [node for node in module.body if isinstance(node, ast.Assign) and any(isinstance(t, ast.Name) and t.id in names for t in node.targets)]
fixture = {}
exec(compile(ast.Module(body=assignments, type_ignores=[]), '<display-fixtures>', 'exec'), fixture)

server = RunningServer(args.binary)
try:
    import secrets
    api = APIClient(server.url)
    password = secrets.token_urlsafe(32)
    data(api.post('/api/v1/auth/setup', {'username': 'demo-build', 'password': password, 'setup_token': server.bootstrap}))
    login = data(api.post('/api/v1/auth/login', {'username': 'demo-build', 'password': password}))
    api.csrf = login['csrf_token']
    caps = data(api.get('/api/v1/capabilities'))
    catalog = data(api.post('/api/v1/actions/plugins_catalog', {}))
finally:
    server.close()

if output.exists():
    shutil.rmtree(output)
shutil.copytree(ROOT / 'frontend/out', output)
(output / 'demo').mkdir(exist_ok=True)
seed = {key: fixture[key] for key in names}
seed['capabilities'] = caps
seed['catalog'] = catalog
seed['notes'] = [
 {'id':'note-handoff','title':'欢迎体验 Folio','content':'这里的主机、网站、应用和文件都是演示数据。\n\n可以新建网站、编辑文件、安装软件、创建计划任务。修改仅保存在当前浏览器，点击右下角「演示数据」可重置。','pinned':True,'archived':False,'target_id':'host:local','revision':1,'created_at':seed['now'],'updated_at':seed['now'],'created_by':seed['actor'],'updated_by':seed['actor']},
 {'id':'note-review','title':'UI 反馈清单','content':'记录页面名称、屏幕尺寸和操作步骤，方便反馈布局或操作上的问题。','pinned':False,'archived':False,'revision':1,'created_at':seed['now'],'updated_at':seed['now'],'created_by':seed['actor'],'updated_by':seed['actor']},
]
seed['contents'] = {'README.md': '# Folio 交互演示\n\n文件内容可以编辑、保存、下载和还原。\n所有操作只修改当前浏览器中的演示数据。\n', 'deploy.sh': '#!/bin/sh\necho "Folio demo — no server command is executed"\n', 'nginx.conf': 'server {\n    listen 80;\n    server_name northstar.example.com;\n    root /srv/www/northstar;\n}\n', 'config/app.json': '{\n  "name": "Northstar",\n  "mode": "demo"\n}\n', 'logs/access.log': '127.0.0.1 - - [23/Oct/2026] "GET / HTTP/1.1" 200\n', 'public/index.html': '<h1>Northstar Studio</h1>\n'}
for row in seed['file_rows']:
    if row['type'] == 'file':
        content = seed['contents'].get(row['path'], '')
        row['size'] = len(content.encode())
        row['sha256'] = hashlib.sha256(content.encode()).hexdigest()
(output / 'demo/seed.js').write_text('window.FOLIO_DEMO_SEED = ' + json.dumps(seed, ensure_ascii=False) + ';\n')
shutil.copy2(ROOT / 'frontend/demo/runtime.js', output / 'demo/runtime.js')
shutil.copy2(ROOT / 'frontend/demo/firefox.html', output / 'demo/firefox.html')
index = output / 'index.html'
html = index.read_text().replace('<head>', '<head><script src="/demo/seed.js"></script><script src="/demo/runtime.js"></script>', 1)
index.write_text(html)
# Static browser mode uses explicitly labelled sample frames, with no outbound target requests.
from PIL import Image, ImageDraw
for name, title, subtitle in [('welcome','Folio browser demo','Try the address bar, tabs, bookmarks and viewport controls.'),('mdn','MDN Web Docs','A sample page for reviewing the panel browser interface.'),('atlas','Open Atlas','A second sample page. No website is contacted from this demo.')]:
    im = Image.new('RGB', (1280,800), '#f5f7fb'); d = ImageDraw.Draw(im)
    d.rectangle((0,0,1280,75), fill='white'); d.text((40,28), title, fill='#182230',font_size=24)
    d.text((80,170), title, fill='#182230', font_size=48)
    d.text((80,250), subtitle, fill='#596579', font_size=22)
    d.rounded_rectangle((80,330,1200,620), radius=16, fill='white', outline='#dde3ec', width=2)
    d.text((115,375), 'STATIC DEMO', fill='#2859d6',font_size=28)
    d.text((115,435), 'Browser controls are interactive; this page image is a sample.', fill='#596579',font_size=22)
    d.text((115,485), 'Use More > Browser to compare the browser layouts.', fill='#596579',font_size=22)
    im.save(output / f'demo/browser-{name}.jpg', quality=86)
print(json.dumps({'output':str(output),'capabilities':len(caps.get('items',caps.get('actions',[]))) if isinstance(caps,dict) else len(caps),'size_bytes':sum(p.stat().st_size for p in output.rglob('*') if p.is_file())}))
