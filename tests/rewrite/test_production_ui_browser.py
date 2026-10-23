#!/usr/bin/env python3
"""Verify the packaged UI with real authentication, persistence and API calls only."""
import json
import os
from pathlib import Path
import secrets
from playwright.sync_api import sync_playwright,expect
from fixture import RunningServer
from http_client import APIClient,data

server=RunningServer(os.environ['POWERLINUX_TEST_BINARY'])
password=secrets.token_urlsafe(32)
out=Path(os.environ.get('POWERLINUX_QA_ARTIFACTS','/tmp/folio-production-ui'));out.mkdir(parents=True,exist_ok=True)
try:
 api=APIClient(server.url)
 data(api.post('/api/v1/auth/setup',{'username':'production-qa','password':password,'setup_token':server.bootstrap}))
 login=data(api.post('/api/v1/auth/login',{'username':'production-qa','password':password}));api.csrf=login['csrf_token']
 assert api.get('/demo/runtime.js')[0]==404
 for i in range(4):
  data(api.post('/api/v1/notes',{'title':f'Real note {i}','content':f'Persisted by the real server: {i}','target_id':'host:local'}))
 with sync_playwright() as p:
  browser=p.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM','/usr/bin/chromium'),args=['--no-sandbox'])
  page=browser.new_page(viewport={'width':1366,'height':900},locale='zh-CN');errors=[];requests=[]
  page.on('pageerror',lambda e:errors.append(str(e)))
  page.on('request',lambda r:requests.append(r.url) if '/api/' in r.url else None)
  page.goto(server.url)
  assert page.locator('#demo-enter').count()==0
  assert page.evaluate('typeof window.FolioDemo')=='undefined'
  page.locator('input[name=username]').fill('production-qa');page.locator('input[name=password]').fill(password)
  page.get_by_role('button',name='登录',exact=True).click()
  expect(page.locator('.overview-note')).to_have_count(3)
  left=page.locator('.status-overview').bounding_box();right=page.locator('.overview-notes').bounding_box();assert abs(left['height']-right['height'])<2
  page.locator('.overview-note').first.click()
  page.get_by_role('textbox',name='标题',exact=False).fill('正式接口保存的便签')
  page.get_by_role('button',name='完成',exact=True).click()
  expect(page.locator('.overview-note').first).to_contain_text('正式接口保存的便签')
  saved=next(note for note in data(api.get('/api/v1/notes')) if note['title']=='正式接口保存的便签')
  assert saved['revision']>=2
  page.reload();expect(page.locator('.overview-note').first).to_contain_text('正式接口保存的便签')
  page.get_by_role('button',name='全局设置',exact=True).click()
  alias=page.get_by_role('textbox',name='服务器显示名称',exact=False);alias.fill('正式接口 QA')
  form=alias.locator('xpath=ancestor::form');form.locator('button[type=submit]').click()
  confirm=form.get_by_role('button',name='确认执行',exact=True)
  if confirm.count():confirm.click()
  expect(page.locator('.host-pill')).to_contain_text('正式接口 QA')
  settings=data(api.post('/api/v1/actions/settings_panel_get',{}));assert settings['server_name']=='正式接口 QA'
  for locale in ['zh-CN','en']:
   page.evaluate('(v)=>localStorage.setItem("folio.language",v)',locale);page.reload();page.wait_for_timeout(300)
   for width,height in [(1366,900),(390,844),(320,568)]:
    page.set_viewport_size({'width':width,'height':height})
    for route in ['#/overview','#/sites','#/software?page=installed','#/software?page=catalog','#/files?root=workspace','#/more']:
     page.evaluate('(v)=>location.hash=v',route);page.wait_for_timeout(200)
     assert page.evaluate('document.documentElement.scrollWidth<=innerWidth'),(locale,route,width)
  assert requests and any('notes_list' in url for url in requests)
  assert not errors,errors
  browser.close()
 # Restart verifies database persistence rather than localStorage state.
 server.restart()
 api=APIClient(server.url);login=data(api.post('/api/v1/auth/login',{'username':'production-qa','password':password}));api.csrf=login['csrf_token']
 assert data(api.get('/api/v1/notes/'+saved['id']))['title']=='正式接口保存的便签'
 assert data(api.post('/api/v1/actions/settings_panel_get',{}))['server_name']=='正式接口 QA'
 print(json.dumps({'result':'passed','real_api':True,'demo_runtime':False,'persisted_after_restart':True,'errors':errors}))
finally:
 server.close()
