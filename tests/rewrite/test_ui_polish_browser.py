#!/usr/bin/env python3
"""Batch-one UX regression against an embedded binary.

Authentication, file roots and empty recycle bin use a real isolated server.
Website rows and one executor failure are explicit display fixtures. No mutations
of websites, software or the host are submitted.
"""
import json
import os
from pathlib import Path
import secrets
from playwright.sync_api import expect, sync_playwright
from fixture import RunningServer
from http_client import APIClient, data


def main():
    server = RunningServer(os.environ['POWERLINUX_TEST_BINARY'])
    password = secrets.token_urlsafe(32)
    errors = []
    artifacts = Path(os.environ.get('POWERLINUX_QA_ARTIFACTS', '/tmp/folio-ui-polish'))
    artifacts.mkdir(parents=True, exist_ok=True)
    try:
        api = APIClient(server.url)
        data(api.post('/api/v1/auth/setup', {'username':'polish-qa','password':password,'setup_token':server.bootstrap}))
        (server.data_dir/'workspace'/'hello.txt').write_text('Real file fixture\n')
        with sync_playwright() as pw:
            browser = pw.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM','/usr/bin/chromium'),headless=True,args=['--no-sandbox'])
            page = browser.new_page(viewport={'width':1440,'height':900},locale='zh-CN',reduced_motion='reduce')
            page.on('pageerror',lambda error: errors.append(str(error)))
            site = {'id':'demo','name':'展示网站','domains':['demo.example.test'],'status':'running','document_root':'/srv/www','revision':1,'updated_at':'2026-10-23T00:00:00Z','tls':{'enabled':True,'expires_at':'2027-04-01T00:00:00Z'}}
            page.route('**/api/v1/actions/sites_list', lambda route: route.fulfill(status=200,json={'data':{'items':[site],'total':1,'status_counts':{'running':1,'stopped':0,'unknown':0}}}))
            certificate_failure={'status':503,'code':'agent_unavailable','message':'Website management requires an explicitly configured privileged Agent'}
            page.route('**/api/v1/actions/certificates_list', lambda route: route.fulfill(status=certificate_failure['status'],json={'error':{'code':certificate_failure['code'],'message':certificate_failure['message']}}))
            page.route('**/api/v1/actions/schedules_list', lambda route: route.fulfill(status=200,json={'data':{'items':[{'id':'nightly','name':'Nightly backup','enabled':True,'next_run_at':'2026-10-24T01:00:00Z','last_run_status':'succeeded','revision':1}],'total':1}}))
            page.goto(server.url)
            page.locator('input[name=username]').fill('polish-qa')
            page.locator('input[name=password]').fill(password)
            page.get_by_role('button',name='登录',exact=True).click()
            expect(page.locator('.status-overview')).to_be_visible()
            page.evaluate('document.fonts.ready')
            font_resources = page.evaluate('performance.getEntriesByType("resource").filter(r=>r.name.includes("/fonts/")).map(r=>({name:r.name,bytes:r.encodedBodySize}))')
            assert any('/noto-sans-sc/base.woff2' in r['name'] for r in font_resources),font_resources
            assert not any(r['name'].endswith('/noto-sans-sc.woff2') for r in font_resources)
            assert sum(r['bytes'] for r in font_resources) < 1500000,font_resources
            def visit(route):
                page.evaluate('(route)=>location.hash=route',route)
                section=route.split('?')[0].split('/')[-1]
                expect(page.locator('.app')).to_have_attribute('data-section',section)
            for width,height in [(390,844),(320,568)]:
                page.set_viewport_size({'width':width,'height':height})
                visit('#/sites')
                expect(page.locator('.sites-table .site-tls').first).to_be_visible()
                expect(page.locator('.sites-table .site-updated').first).not_to_be_visible()
                expect(page.locator('.sites-table .site-runtime').first).not_to_be_visible()
                assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
                assert page.locator('.site-toolbar input').evaluate('(e)=>parseFloat(getComputedStyle(e).fontSize)') >= 16
                page.screenshot(path=str(artifacts/f'sites-{width}.png'))
            visit('#/more?page=schedules')
            expect(page.locator('.operations-table .operation-next_run_at').first).to_be_visible()
            expect(page.locator('.operations-table .operation-last_run_status').first).to_be_visible()
            expect(page.locator('.operations-table .operation-enabled').first).not_to_be_visible()
            visit('#/files?root=workspace&filter=true')
            expect(page.get_by_text('回收站为空',exact=True)).to_be_visible()
            assert '当前服务未提供此能力' not in page.locator('.resource-page:visible').inner_text()
            visit('#/files?root=workspace&new=true')
            expect(page.get_by_role('button',name='选择本地文件',exact=True)).to_be_visible()
            page.get_by_label('本地上传文件',exact=True).set_input_files({'name':'batch-one.txt','mimeType':'text/plain','buffer':b'not uploaded'})
            expect(page.locator('.file-picker > span[role=status]')).to_have_text('batch-one.txt')
            page.screenshot(path=str(artifacts/'upload-mobile.png'))
            page.set_viewport_size({'width':1440,'height':900})
            visit('#/sites?page=certificates')
            expect(page.get_by_text('主机执行器未配置',exact=True)).to_be_visible()
            expect(page.locator('.notice pre').filter(has_text='explicitly configured privileged')).not_to_be_visible()
            page.screenshot(path=str(artifacts/'certificate-error.png'))
            certificate_failure.update(status=403,code='forbidden',message='Read access denied')
            page.get_by_role('button',name='重试',exact=True).click()
            expect(page.get_by_text('无访问权限',exact=True)).to_be_visible()
            visit('#/more?page=actions')
            page.wait_for_function('() => document.querySelectorAll(".action-tile").length > 0')
            assert 0 < page.locator('.action-tile').count() <= 12
            first=page.locator('.action-tile').first.inner_text()
            page.get_by_role('button',name='下一页',exact=True).click()
            expect(page.locator('.action-tile').first).not_to_have_text(first)
            page.get_by_label('操作分类',exact=True).select_option('files')
            expect(page.locator('.pagination')).to_contain_text('第 1 页')
            names=page.locator('.action-tile p').all_text_contents()
            assert names and all(t.startswith(('fs_','file_','files_','directories_','paths_')) for t in names),names
            page.evaluate('window.scrollTo(0,0)')
            page.screenshot(path=str(artifacts/'advanced-tools.png'))
            visit('#/more?page=terminal')
            expect(page.get_by_text('尚未连接，选择目标后连接终端。',exact=True)).to_be_visible()
            assert not errors,errors
            browser.close()
        print(json.dumps({'result':'passed','font_bytes':sum(r['bytes'] for r in font_resources),'viewports':[390,320],'errors':errors,'artifacts':str(artifacts)},ensure_ascii=False))
    finally:
        server.close()

if __name__=='__main__':
    main()
