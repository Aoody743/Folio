#!/usr/bin/env python3
"""Real alert CRUD and retention policy persistence; cleanup submission intercepted.
Website/backup display fixtures do not represent an installed privileged Agent.
"""
import datetime as dt
import json,os,secrets
from pathlib import Path
from playwright.sync_api import sync_playwright,expect
from fixture import RunningServer
from http_client import APIClient,data

def main():
 server=RunningServer(os.environ['POWERLINUX_TEST_BINARY']);password=secrets.token_urlsafe(32);errors=[];applies=[]
 out=Path(os.environ.get('POWERLINUX_QA_ARTIFACTS','/tmp/folio-safety-tools'));out.mkdir(parents=True,exist_ok=True)
 try:
  api=APIClient(server.url);data(api.post('/api/v1/auth/setup',{'username':'safety-qa','password':password,'setup_token':server.bootstrap}));login=data(api.post('/api/v1/auth/login',{'username':'safety-qa','password':password}));api.csrf=login['csrf_token']
  with sync_playwright() as pw:
   browser=pw.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM','/usr/bin/chromium'),headless=True,args=['--no-sandbox'])
   page=browser.new_page(viewport={'width':1440,'height':900},locale='zh-CN',reduced_motion='reduce');page.on('pageerror',lambda e:errors.append(str(e)))
   page.goto(server.url);page.locator('input[name=username]').fill('safety-qa');page.locator('input[name=password]').fill(password);page.get_by_role('button',name='登录',exact=True).click();expect(page.locator('.status-overview')).to_be_visible()
   def visit(route):page.evaluate('(r)=>{location.hash=r;window.scrollTo(0,0)}',route)
   visit('#/more?page=monitor&view=alerts');alerts=page.get_by_role('region',name='监控告警',exact=True)
   expect(alerts.get_by_text('尚未配置告警规则',exact=True)).to_be_visible();alerts.get_by_role('button',name='新增告警规则',exact=True).click()
   alerts.get_by_label('规则名称',exact=True).fill('Disk capacity watch');alerts.get_by_label('监控指标',exact=True).select_option('disk');alerts.get_by_label('触发阈值 (%)',exact=True).fill('92');alerts.get_by_label('持续时间（秒）',exact=True).fill('60')
   assert not alerts.get_by_label('启用此规则',exact=True).is_checked()
   alerts.get_by_role('button',name='保存告警规则',exact=True).click();expect(alerts.get_by_text('确认操作与变更参数',exact=True)).to_be_visible()
   assert data(api.post('/api/v1/actions/monitor_alerts_list',{}))==[],'rule saved before confirmation'
   alerts.get_by_role('button',name='确认执行',exact=True).click();expect(alerts.get_by_role('heading',name='Disk capacity watch',exact=True)).to_be_visible()
   rules=data(api.post('/api/v1/actions/monitor_alerts_list',{}));assert len(rules)==1 and rules[0]['metric']=='disk' and rules[0]['threshold']==92 and not rules[0]['enabled']
   alerts.get_by_label('触发阈值 (%)',exact=True).fill('93');alerts.get_by_role('button',name='保存告警规则',exact=True).click();alerts.get_by_role('button',name='确认执行',exact=True).click()
   expect(alerts.get_by_text('确认操作与变更参数',exact=True)).not_to_be_visible()
   rules=data(api.post('/api/v1/actions/monitor_alerts_list',{}));assert len(rules)==1 and rules[0]['revision']==2 and rules[0]['threshold']==93
   alerts.get_by_role('heading',name='Disk capacity watch',exact=True).scroll_into_view_if_needed()
   page.screenshot(path=str(out/'monitor-alert-rule.png'))
   for width,height in [(390,844),(320,568)]:
    page.set_viewport_size({'width':width,'height':height});assert page.evaluate('document.documentElement.scrollWidth<=innerWidth');page.screenshot(path=str(out/f'alert-{width}.png'))
   page.set_viewport_size({'width':1440,'height':900})
   site={'id':'retention-fixture','name':'Retention fixture','domains':['fixture.example.test'],'status':'running','revision':1,'document_root':'/srv/example','updated_at':dt.datetime.now(dt.timezone.utc).isoformat()}
   page.route('**/api/v1/actions/sites_get',lambda r:r.fulfill(status=200,json={'data':site}));page.route('**/api/v1/actions/sites_list',lambda r:r.fulfill(status=200,json={'data':{'items':[site],'total':1}}));page.route('**/api/v1/actions/sites_backups_list',lambda r:r.fulfill(status=200,json={'data':[]}))
   def cleanup(r):
    name=r.request.url.rsplit('/',1)[-1]
    if name=='sites_retention_apply':applies.append(r.request.post_data_json);return r.fulfill(status=409,json={'error':{'code':'fixture_no_delete','message':'Display fixture: cleanup was not executed'}})
    policy=data(api.post('/api/v1/actions/sites_retention_get',{'id':site['id']}));now=dt.datetime.now(dt.timezone.utc)
    r.fulfill(status=200,json={'data':{'plan_id':'cleanup_display_fixture','policy':policy,'created_at':now.isoformat(),'expires_at':(now+dt.timedelta(minutes=15)).isoformat(),'total':4,'candidates':[{'id':'backup-old-fixture','created_at':(now-dt.timedelta(days=90)).isoformat(),'size':1024}]}})
   page.route('**/api/v1/actions/sites_retention_plan',cleanup);page.route('**/api/v1/actions/sites_retention_apply',cleanup)
   visit('#/sites?id=retention-fixture&group=backup');site_region=page.get_by_role('region',name='网站 · Retention fixture',exact=True)
   site_region.locator('.backup-retention > summary').click();retention=site_region.locator('.backup-retention')
   retention.get_by_label('至少保留份数',exact=False).fill('2');retention.get_by_label('至少保留天数',exact=False).fill('14');retention.get_by_role('button',name='保存网站备份保留策略',exact=True).click();retention.get_by_role('button',name='确认执行',exact=True).click()
   expect(retention.get_by_text('确认操作与变更参数',exact=True)).not_to_be_visible()
   actual=data(api.post('/api/v1/actions/sites_retention_get',{'id':site['id']}));assert actual['keep_count']==2 and actual['keep_days']==14 and actual['revision']==1
   retention.get_by_role('button',name='预览网站备份清理',exact=True).click();expect(retention.get_by_text('待清理备份',exact=True)).to_be_visible()
   retention.get_by_role('button',name='清理预览中的网站备份',exact=True).scroll_into_view_if_needed()
   page.screenshot(path=str(out/'backup-retention-preview.png'))
   retention.get_by_role('button',name='清理预览中的网站备份',exact=True).click();expect(retention.get_by_text('确认操作与变更参数',exact=True)).to_be_visible();assert not applies
   retention.get_by_role('button',name='确认执行',exact=True).click();expect(retention.get_by_role('alert')).to_contain_text('cleanup was not executed')
   assert applies==[{'id':site['id'],'plan_id':'cleanup_display_fixture'}],applies
   assert not errors,errors;browser.close()
  print(json.dumps({'result':'passed','real_alert_create_update':True,'real_retention_policy':True,'cleanup':'intercepted; no backup deleted','errors':errors,'artifacts':str(out)},ensure_ascii=False))
 finally:server.close()
if __name__=='__main__':main()
