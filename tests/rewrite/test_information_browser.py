#!/usr/bin/env python3
"""UI information regression: real server/files, plus explicit website/app display fixtures.

Display fixtures only cover large/unknown collections without requiring host installs;
backend authorization and count semantics have separate Go integration coverage.
No software/site mutation is mocked or submitted by this test.
"""
import json
import os
from pathlib import Path
import secrets
from playwright.sync_api import sync_playwright, expect
from fixture import RunningServer
from http_client import APIClient, data

VIEWPORTS = [(1366,768),(1440,900),(390,844),(360,640),(320,568)]

def main():
    artifacts = Path(os.environ.get('POWERLINUX_QA_ARTIFACTS','/tmp/folio-information-browser'))
    artifacts.mkdir(parents=True, exist_ok=True)
    server = RunningServer(os.environ['POWERLINUX_TEST_BINARY'])
    password = secrets.token_urlsafe(32)
    api = APIClient(server.url)
    data(api.post('/api/v1/auth/setup', {'username':'information-qa','password':password,'setup_token':server.bootstrap}))
    for i in range(37):
        (server.data_dir/'workspace'/f'{i:03d}-readable-file.txt').write_text('Real file fixture\n')
    sites = [{'id':f'site-{i}','name':f'Website {i:03d}','domains':[f'site-{i}.example.test'],'status':['running','stopped','new-state'][i%3],'revision':1,'document_root':'/srv/www','updated_at':'2026-10-22T10:00:00Z'} for i in range(67)]
    apps = [{'id':f'app-{i}','name':f'Application {i:03d}','runtime':'nodejs','status':['running','stopped','building'][i%3],'running':i%3==0,'revision':1,'upstream':'http://127.0.0.1:10001','user':'app','port':10001,'updated_at':'2026-10-22T10:00:00Z'} for i in range(53)]
    sites[1]["name"] = "Website " + "a readable long resource name " * 5
    calls = []
    errors = []
    try:
        with sync_playwright() as p:
            browser = p.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM','/usr/bin/chromium'),headless=True,args=['--no-sandbox'])
            page = browser.new_page(locale='zh-CN',viewport={'width':1366,'height':768},reduced_motion='reduce')
            page.on('pageerror',lambda error: errors.append(str(error)))
            page.on('request',lambda request: errors.append('external font: '+request.url) if ('fonts.googleapis' in request.url or 'fonts.gstatic' in request.url) else None)
            def fixture(route):
                name = route.request.url.split('/')[-1]
                body = route.request.post_data_json or {}
                if name == 'sites_list':
                    matching = [s for s in sites if body.get('query','').lower() in s['name'].lower()]
                    if body.get('status'):
                        matching = [s for s in matching if (s['status'] if s['status'] in ['running','stopped'] else 'unknown')==body['status']]
                    counts = {state:sum((s['status'] if s['status'] in ['running','stopped'] else 'unknown')==state for s in matching) for state in ['running','stopped','unknown']}
                    offset,limit = body.get('offset',0),body.get('limit',100)
                    calls.append({'name':name,'limit':limit,'offset':offset})
                    result = {'items':matching[offset:offset+limit],'total':len(matching),'status_counts':counts,'limit':limit,'offset':offset}
                elif name == 'sites_get':
                    result = next(s for s in sites if s['id']==body['id'])
                else:
                    result = {'items':apps,'total':len(apps)}
                route.fulfill(status=200,content_type='application/json',body=json.dumps({'data':result}))
            for name in ['sites_list','sites_get','hosted_apps_list']:
                page.route('**/api/v1/actions/'+name, fixture)
            page.goto(server.url)
            page.locator('input[name=username]').fill('information-qa')
            page.locator('input[name=password]').fill(password)
            page.get_by_role('button',name='登录',exact=True).click()
            expect(page.locator('.status-overview')).to_be_visible()
            def goto(path):
                page.evaluate('(hash) => { location.hash=hash; }',path)
                expect(page.locator('.app')).to_have_attribute('data-section', path.lstrip('/').split('?')[0])
            combinations = 0
            for locale in (['en'] if os.environ.get('POWERLINUX_UI_SKIP_MATRIX') else ['zh-CN','en']):
                for font in (['reading'] if os.environ.get('POWERLINUX_UI_SKIP_MATRIX') else ['modern','native','reading']):
                    for palette in (['sand'] if os.environ.get('POWERLINUX_UI_SKIP_MATRIX') else ['moon','space','violet','pine','sand']):
                        goto('/more?page=appearance')
                        page.get_by_label('语言 / Language',exact=True).select_option(locale)
                        page.locator(f'.font-option[data-font-preset="{font}"]').click()
                        page.locator(f'.theme-option.preview-{palette}').click()
                        expect(page.locator('html')).to_have_attribute('data-font',font)
                        goto('/overview')
                        page.evaluate('document.fonts.ready')
                        if font != 'native':
                            family = 'Folio Sans SC' if font == 'modern' else 'Folio Serif SC'
                            assert page.evaluate('(family) => [...document.fonts].some(face => face.family.includes(family) && face.status === "loaded")',family), (font,'font did not load')
                        for w,h in VIEWPORTS:
                            page.set_viewport_size({'width':w,'height':h})
                            page.wait_for_timeout(25)
                            assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), (locale,font,palette,w,'overflow')
                            assert page.locator('.summary-panel').count()==3
                            if w>=1000:
                                assert page.evaluate('document.documentElement.scrollHeight <= innerHeight'), (locale,font,palette,w,'desktop home scroll')
                            else:
                                assert not page.locator('.topbar .user-badge').is_visible()
                                assert page.locator('.top-actions button:visible').count()==1
                            assert page.locator('.topbar .language-control').count()==0
                            if palette=='moon' and font=='modern' and w in [1366,390,320]:
                                page.screenshot(path=str(artifacts/f'{locale}-overview-{w}.png'),full_page=True)
                        combinations+=1
            # Resource lists use real viewport pagination; browser back restores query and page.
            goto('/sites?q=Website')
            page.set_viewport_size({'width':1366,'height':768})
            expect(page.locator('.resource-table tbody tr').first).to_be_visible()
            page.wait_for_timeout(200)
            assert page.locator('.resource-table tbody tr').count()<67
            page.get_by_role('button',name='Next page',exact=True).click()
            first = page.locator('.resource-table .resource-name').first.inner_text()
            url = page.url
            page.locator('.resource-table .resource-name').first.click()
            expect(page.locator('.resource-page:not([hidden])')).to_be_visible()
            page.reload()
            expect(page.locator('.resource-page:not([hidden])')).to_be_visible()
            page.get_by_role('button',name='Back to list',exact=True).click()
            expect(page.locator('.resource-table .resource-name').first).to_have_text(first)
            page.locator('.resource-table .resource-name').first.click()
            page.go_back()
            expect(page.locator('.resource-table .resource-name').first).to_have_text(first)
            for path in ['/sites','/software?page=applications','/files']:
                for w,h in VIEWPORTS:
                    page.set_viewport_size({'width':w,'height':h})
                    goto(path)
                    page.wait_for_timeout(200)
                    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth'), (path,w)
                    if w>=1000:
                        page.screenshot(path=str(artifacts/f'debug-{path.split("?")[0][1:]}-{w}.png'),full_page=True)
                        # ResizeObserver pagination and font metrics can settle
                        # on adjacent frames. Require a stable fitting layout,
                        # rather than taking a second, racy snapshot after the wait.
                        page.wait_for_function("""() => {
                          const key=location.hash+':'+innerWidth+':'+innerHeight;
                          const height=document.documentElement.scrollHeight;
                          const valid=height<=innerHeight;
                          const prior=window.__folioLayoutProbe;
                          if(!valid || !prior || prior.key!==key || prior.height!==height){
                            window.__folioLayoutProbe={key,height,since:performance.now()};return false;
                          }
                          return performance.now()-prior.since>=250;
                        }""",timeout=5000)
                    pager=page.locator('.pagination:visible')
                    expect(pager).to_be_visible()
                    if w in [1366,390]:page.screenshot(path=str(artifacts/f'list-{path.split("?")[0][1:]}-{w}.png'),full_page=True)
            assert any(call['limit']<20 and call['offset']>0 for call in calls),calls
            # State filters from overview carry through to lists; no invented health label.
            goto('/overview')
            page.locator('.resource-status').first.locator('.summary-counts button').first.click()
            assert 'status=running' in page.url
            # Logs open on the third click from Software; executor failures stay visible in the detail.
            goto('/software')
            page.get_by_role('tab',name='Application services',exact=True).click()
            page.locator('.resource-table:visible .resource-name').first.click()
            page.get_by_role('tab',name='Log',exact=True).click()
            expect(page.get_by_role('region',name='Application logs',exact=True).get_by_role('alert')).to_be_visible()
            page.get_by_role('button',name='Back to list',exact=True).click()
            # Common forms remain reachable without nested dialogs or mutation.
            goto('/sites')
            try:
                page.get_by_role('button',name='Create website',exact=True).click()
            except Exception:
                page.screenshot(path=str(artifacts/'create-website-navigation-failed.png'),full_page=True)
                state=page.evaluate('''() => ({hash:location.hash,lang:document.documentElement.lang,section:document.querySelector('.app')?.dataset.section,text:document.body.innerText,details:[...document.querySelectorAll('.resource-page')].map(e=>({hidden:e.hidden,title:e.getAttribute('aria-label')}))})''')
                (artifacts/'create-website-navigation-failed.json').write_text(json.dumps(state,ensure_ascii=False,indent=2))
                print('Create website navigation state',json.dumps(state,ensure_ascii=False),flush=True)
                raise
            expect(page.get_by_role('region',name='Create website',exact=True)).to_be_visible()
            page.get_by_role('button',name='Back to list',exact=True).click()
            goto('/software')
            page.get_by_role('button',name='Install software',exact=True).click()
            page.locator('.software-card').first.click()
            expect(page.locator('.resource-page:not([hidden]) form')).to_be_visible()
            page.get_by_role('button',name='Back to list',exact=True).click()
            goto('/files')
            page.get_by_role('button',name='Upload file',exact=True).click()
            expect(page.get_by_role('region',name='Upload file',exact=True)).to_be_visible()
            page.get_by_role('button',name='Back to list',exact=True).click()
            goto('/more?page=schedules')
            page.get_by_role('button',name='Create schedule',exact=True).click()
            expect(page.locator('.resource-page:not([hidden]) form')).to_be_visible()
            page.get_by_role('button',name='Back to list',exact=True).click()
            goto('/more')
            page.get_by_role('button',name='Global search',exact=True).click()
            expect(page.get_by_role('dialog',name='Global search',exact=True)).to_be_visible()
            page.keyboard.press('Escape')
            # Module changes must not reuse a task ID as a service/schedule ID.
            goto('/more?page=activity&id=stale-task&kind=task')
            page.get_by_role('navigation',name='Main navigation').get_by_role('button',name='More',exact=True).click()
            page.locator('.more-groups .more-tile').first.click()
            assert 'stale-task' not in page.url and 'kind=task' not in page.url
            expect(page.locator('.resource-page:not([hidden])')).to_have_count(0)
            # Permission errors must not become a zero-resource or healthy summary.
            def denied(route):
                route.fulfill(status=403,content_type='application/json',body=json.dumps({'error':{'code':'forbidden','message':'Not authorized'}}))
            page.route('**/api/v1/actions/sites_list',denied)
            goto('/overview')
            expect(page.locator('.resource-status').first).to_contain_text('Access denied')
            expect(page.locator('.resource-status').first.locator('.summary-heading b')).to_have_text('—')
            page.unroute('**/api/v1/actions/sites_list',denied)
            # Settings remain browser-local and survive a full reload.
            goto('/more?page=appearance')
            page.reload()
            expect(page.locator('html')).to_have_attribute('lang','en')
            expect(page.locator('html')).to_have_attribute('data-font','reading')
            expect(page.locator('html')).to_have_attribute('data-palette','sand')
            # An unsaved real CodeMirror draft survives navigation and settings changes.
            page.set_viewport_size({'width':390,'height':844})
            goto('/files?root=workspace')
            page.get_by_role('button',name='000-readable-file.txt',exact=True).click()
            editor = page.get_by_role('region',name='Edit file · 000-readable-file.txt',exact=True)
            content = editor.get_by_role('textbox',name='File content',exact=True)
            content.fill('未保存的原文 · draft survives settings')
            editor.get_by_role('button',name='Back to list',exact=True).click()
            goto('/more?page=appearance')
            page.locator('.font-option[data-font-preset="modern"]').click()
            page.get_by_label('语言 / Language',exact=True).select_option('zh-CN')
            page.get_by_role('navigation',name='主导航').get_by_role('button',name='文件',exact=True).click()
            page.get_by_role('button',name='继续编辑文件草稿',exact=True).click()
            editor = page.get_by_role('region',name='编辑文件 · 000-readable-file.txt',exact=True)
            expect(editor.get_by_role('textbox',name='文件内容',exact=True)).to_have_text('未保存的原文 · draft survives settings')
            previous = page.url
            page.evaluate('location.hash="/files?root=workspace&path=other&file=other.txt&view=editor"')
            page.wait_for_timeout(150)
            assert page.url == previous, 'dirty file navigation was not protected'
            expect(editor.get_by_role('textbox',name='文件内容',exact=True)).to_have_text('未保存的原文 · draft survives settings')
            goto('/more')
            page.screenshot(path=str(artifacts/'zh-more-mobile.png'),full_page=True)
            goto('/more?page=appearance')
            page.screenshot(path=str(artifacts/'zh-appearance-mobile.png'),full_page=True)
            assert not errors,errors
            browser.close()
        print(json.dumps({'real_server_and_files':True,'explicit_site_app_display_fixtures':True,'locale_font_theme_combinations':combinations,'viewports':VIEWPORTS,'back_refresh_filters':True,'mobile_scroll_allowed':True,'errors':errors,'artifacts':str(artifacts)}))
    finally:server.close()

if __name__=='__main__':main()
