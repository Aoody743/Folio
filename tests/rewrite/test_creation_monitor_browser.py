#!/usr/bin/env python3
"""Business forms, real scoped tokens and monitoring interaction.

A real isolated server supplies auth, schemas and token creation. Runtime listings
and metric samples are explicit UI fixtures. Website creation is intercepted with
409 to verify its wire input/confirmation without modifying a host.
"""
import datetime as dt
import json
import os
from pathlib import Path
import secrets
from playwright.sync_api import sync_playwright, expect
from fixture import RunningServer
from http_client import APIClient, data


def main():
    server=RunningServer(os.environ['POWERLINUX_TEST_BINARY'])
    password=secrets.token_urlsafe(32)
    output=Path(os.environ.get('POWERLINUX_QA_ARTIFACTS','/tmp/folio-creation-monitor'))
    output.mkdir(parents=True,exist_ok=True)
    errors=[];creates=[];history_requests=[]
    available={'enabled':True}
    try:
        api=APIClient(server.url)
        data(api.post('/api/v1/auth/setup',{'username':'business-qa','password':password,'setup_token':server.bootstrap}))
        with sync_playwright() as pw:
            browser=pw.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM','/usr/bin/chromium'),headless=True,args=['--no-sandbox'])
            page=browser.new_page(viewport={'width':1440,'height':900},locale='zh-CN',reduced_motion='reduce')
            page.on('pageerror',lambda error:errors.append(str(error)))
            def fixture(route):
                action=route.request.url.rsplit('/',1)[-1]
                if action=='plugins_list':
                    result={'items':[{'id':'nginx-main','plugin_id':'nginx','instance_id':'main','status':'running','installed_version':'1.26'}] if available['enabled'] else [],'availability':'ready' if available['enabled'] else 'agent_not_configured'}
                elif action=='sites_create':
                    creates.append({'body':route.request.post_data_json,'key':route.request.headers.get('idempotency-key')})
                    return route.fulfill(status=409,json={'error':{'code':'fixture_conflict','message':'Display fixture: no website was created'}})
                elif action=='monitor_history':
                    history_requests.append(route.request.post_data_json)
                    now=dt.datetime.now(dt.timezone.utc)
                    result={'sample_interval_seconds':60,'samples':[
                        {'timestamp':(now-dt.timedelta(minutes=1)).isoformat(),'cpu_percent':10,'memory':None,'disk':{'percent':42},'load_average':[0.2,0.3,0.4],'network':[{'interface':'eth0','receive_bytes_per_second':1024,'transmit_bytes_per_second':0}]},
                        {'timestamp':now.isoformat(),'cpu_percent':80,'memory':{'percent':55},'disk':{'percent':42},'load_average':[1.2,0.8,0.4],'network':[{'interface':'eth0','receive_bytes_per_second':2048,'transmit_bytes_per_second':1024}]},
                    ]}
                else:
                    return route.continue_()
                route.fulfill(status=200,json={'data':result})
            for action in ['plugins_list','sites_create','monitor_history']:
                page.route('**/api/v1/actions/'+action,fixture)
            page.goto(server.url)
            page.locator('input[name=username]').fill('business-qa')
            page.locator('input[name=password]').fill(password)
            page.get_by_role('button',name='登录',exact=True).click()
            expect(page.locator('.status-overview')).to_be_visible()
            def visit(route):
                page.evaluate('(route)=>{location.hash=route;window.scrollTo(0,0)}',route)
                expect(page.locator('.app')).to_have_attribute('data-section',route.split('?')[0].split('/')[-1])
            visit('#/sites?create=true')
            form=page.get_by_role('region',name='创建网站',exact=True)
            domains=form.get_by_label('网站域名',exact=False)
            try:
                domains.fill('example.test\nwww.example.test',timeout=10000)
            except Exception:
                page.screenshot(path=str(output/'debug-create.png'))
                print(json.dumps({'hash':page.url.split('#')[-1],'text':form.inner_text(),'errors':errors},ensure_ascii=False))
                raise
            expect(form.get_by_label('网站名称',exact=False)).to_have_value('example.test')
            expect(form.get_by_role('heading',name='创建网站',exact=True)).to_have_count(1)
            form.locator('summary').filter(has_text='可选设置').first.click()
            expect(form.get_by_label('资源标识',exact=True)).to_have_value('site-example-test')
            form.get_by_label('网站名称',exact=False).fill('My Website')
            form.get_by_label('资源标识',exact=True).fill('custom-site')
            domains.fill('changed.test\nwww.changed.test')
            expect(form.get_by_label('网站名称',exact=False)).to_have_value('My Website')
            expect(form.get_by_label('资源标识',exact=True)).to_have_value('custom-site')
            form.locator('summary').filter(has_text='可选设置').first.click()
            visit('#/more?page=appearance')
            page.get_by_label('语言 / Language',exact=True).select_option('en')
            page.locator('.font-option[data-font-preset="native"]').click()
            page.get_by_label('语言 / Language',exact=True).select_option('zh-CN')
            page.locator('.font-option[data-font-preset="modern"]').click()
            visit('#/sites?create=true')
            expect(domains).to_have_value('changed.test\nwww.changed.test')
            expect(form.get_by_label('网站名称',exact=False)).to_have_value('My Website')
            page.screenshot(path=str(output/'create-website.png'))
            form.get_by_role('button',name='创建网站',exact=True).click()
            expect(form.get_by_text('确认操作与变更参数',exact=True)).to_be_visible()
            assert not creates,'creation ran before confirmation'
            form.get_by_role('button',name='确认执行',exact=True).click()
            expect(form.get_by_role('alert')).to_contain_text('Display fixture: no website was created')
            assert creates[0]['body']['id']=='custom-site' and creates[0]['body']['name']=='My Website'
            assert creates[0]['body']['domains']==['changed.test','www.changed.test'] and creates[0]['body']['web_instance']=='nginx-main'
            assert creates[0]['key']
            with page.expect_response(lambda response:response.url.endswith('/actions/sites_create')):
                form.get_by_role('button',name='确认执行',exact=True).click()
            assert len(creates)==2 and creates[1]['key']==creates[0]['key']
            assert 'changed.test' not in page.url
            visit('#/software?page=applications&subgroup=hosted&create=true')
            app=page.get_by_role('region',name='部署应用',exact=True)
            app.get_by_label('应用名称',exact=True).fill('Customer Portal')
            app.locator('summary').filter(has_text='可选设置').click()
            expect(app.get_by_label('应用编号（小写字母开头）',exact=True)).to_have_value('app-customer-portal')
            app.get_by_label('应用编号（小写字母开头）',exact=True).fill('custom-app')
            app.get_by_label('应用名称',exact=True).fill('Renamed portal')
            expect(app.get_by_label('应用编号（小写字母开头）',exact=True)).to_have_value('custom-app')
            visit('#/more?page=monitor')
            expect(page.locator('.chart-readout')).to_contain_text('80.0%')
            selector=page.get_by_label('查看时间点',exact=True)
            selector.focus();selector.press('Home')
            expect(page.locator('.chart-readout')).to_contain_text('10.0%')
            expect(page.locator('.chart-readout')).to_contain_text('—')
            expect(page.locator('.network-readout')).to_contain_text('eth0')
            expect(page.locator('.network-readout')).to_contain_text('KiB/s')
            with page.expect_response(lambda response:response.url.endswith('/actions/monitor_history')):
                page.get_by_label('监控时间范围',exact=True).select_option('1')
            start=dt.datetime.fromisoformat(history_requests[-1]['start'].replace('Z','+00:00'))
            assert abs((dt.datetime.now(dt.timezone.utc)-start).total_seconds()-3600)<20
            page.screenshot(path=str(output/'monitor.png'))
            page.get_by_role('button',name='管理系统变更',exact=True).click()
            expect(page.get_by_role('region',name='系统变更与恢复',exact=True)).to_be_visible()
            page.get_by_role('button',name='返回列表',exact=True).click()
            visit('#/more?page=mcp')
            page.get_by_label('授权用途',exact=True).select_option('monitor')
            page.get_by_label('令牌有效期',exact=True).select_option('7')
            page.get_by_label('客户端名称',exact=False).fill('Read-only fixture')
            page.screenshot(path=str(output/'mcp-purpose.png'))
            with page.expect_response(lambda response:response.url.endswith('/api/v1/tokens') and response.request.method=='POST') as response_info:
                page.get_by_role('button',name='创建访问令牌',exact=True).click()
            response=response_info.value
            assert response.ok
            body=response.request.post_data_json
            assert body['scopes'] and set(body['scopes'])<={'system:read','tasks:read','audit:read'}
            assert body['resources']==['host:local'] and body['expires_in_seconds']==604800
            listed=page.request.get(server.url+'/api/v1/tokens').json()['data']
            rows=listed if isinstance(listed,list) else listed['items']
            token=next(row for row in rows if row['name']=='Read-only fixture')
            created=dt.datetime.fromisoformat(token['created_at'].replace('Z','+00:00'))
            expiry=dt.datetime.fromisoformat(token['expires_at'].replace('Z','+00:00'))
            assert (expiry-created).total_seconds()==604800
            available['enabled']=False
            page.reload()
            visit('#/sites?create=true')
            expect(page.get_by_text('创建网站前，请先配置执行器并安装 Nginx 或 OpenResty。',exact=True)).to_be_visible()
            expect(page.get_by_role('region',name='创建网站',exact=True).locator('form')).to_have_count(0)
            assert not errors,errors
            browser.close()
        print(json.dumps({'result':'passed','real_scoped_token':True,'website_submission_intercepted':True,'metric_samples':'display_fixture','errors':errors,'artifacts':str(output)},ensure_ascii=False))
    finally:
        server.close()

if __name__=='__main__':main()
