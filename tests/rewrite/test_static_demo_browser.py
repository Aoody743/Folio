#!/usr/bin/env python3
"""Exercise the actual exported UI through its browser-local demo transport."""
import json
import os
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

url = os.environ.get('FOLIO_DEMO_URL', 'http://127.0.0.1:4178')
out = Path(os.environ.get('POWERLINUX_QA_ARTIFACTS','/tmp/folio-static-demo'))
out.mkdir(parents=True, exist_ok=True)
with sync_playwright() as pw:
    browser = pw.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM','/usr/bin/chromium'), headless=True, args=['--no-sandbox'])
    page = browser.new_page(viewport={'width':1440,'height':960}, locale='zh-CN', reduced_motion='reduce')
    errors=[];api_network=[]
    page.on('pageerror', lambda e:errors.append(str(e)))
    page.on('request', lambda r:api_network.append(r.url) if '/api/' in r.url or '/healthz' in r.url else None)
    page.goto(url)
    expect(page.locator('#demo-enter')).to_be_visible()
    page.evaluate('document.fonts.ready')
    # Check the rendered separator uses the shipped custom font, not .notdef.
    cdp=page.context.new_cdp_session(page)
    cdp.send('DOM.enable');cdp.send('CSS.enable')
    root=cdp.send('DOM.getDocument')['root']['nodeId']
    node=cdp.send('DOM.querySelector',{'nodeId':root,'selector':'.astryx-field-label [aria-hidden="true"]'})['nodeId']
    fonts=cdp.send('CSS.getPlatformFontsForNode',{'nodeId':node})['fonts']
    assert any(f['isCustomFont'] and 'Source Sans 3' in f['familyName'] and f['glyphCount']>0 for f in fonts),fonts
    page.screenshot(path=str(out/'login-font-fixed.png'))
    page.locator('#demo-enter').click()
    expect(page.locator('.status-overview')).to_be_visible()
    def visit(route):
        page.evaluate('route=>{location.hash=route;window.scrollTo(0,0)}',route)
        expect(page.locator('.app')).to_have_attribute('data-section',route.lstrip('#/').split('?')[0])
    visit('#/sites?create=true')
    form=page.get_by_role('region',name='创建网站',exact=True)
    form.get_by_label('网站域名',exact=False).fill('review-demo.example.com')
    form.get_by_label('网站服务',exact=False).select_option('nginx-main')
    form.get_by_role('button',name='创建网站',exact=True).click()
    page.screenshot(path=str(out/'create-confirmation.png'))
    print('confirmation:',form.inner_text(),flush=True)
    buttons=form.get_by_role('button').all_text_contents()
    print('buttons',buttons,flush=True)
    confirm=form.get_by_role('button',name='确认执行',exact=True)
    if confirm.count():confirm.click()
    else:
        confirm=form.get_by_role('button',name='确认创建网站',exact=True)
        if confirm.count():confirm.click()
    page.wait_for_function("FolioDemo.getState().sites.some(s=>s.domains.includes('review-demo.example.com'))",timeout=12000)
    visit('#/sites')
    expect(page.get_by_role('button',name='review-demo.example.com',exact=False).first).to_be_visible()
    page.reload();expect(page.get_by_role('button',name='review-demo.example.com',exact=False).first).to_be_visible()
    visit('#/files?root=workspace&file=README.md&view=editor')
    expect(page.locator('.cm-editor')).to_be_visible()
    editor=page.locator('.cm-content');editor.click();page.keyboard.press('Control+a');page.keyboard.insert_text('# Edited in the static demo\nLocal changes only.\n')
    page.get_by_role('button',name='保存文件',exact=True).click()
    page.wait_for_function("FolioDemo.getState().contents['README.md'].includes('Edited in the static demo')")
    page.screenshot(path=str(out/'file-edited.png'))
    visit('#/more?page=terminal')
    page.get_by_role('button',name='连接终端',exact=True).click()
    expect(page.locator('.xterm')).to_be_visible()
    page.wait_for_timeout(1000)
    page.locator('.xterm-helper-textarea').focus()
    page.keyboard.type('pwd');page.keyboard.press('Enter')
    page.wait_for_function("Object.values(FolioDemo.getState().terminals).some(t=>t.frames.some(f=>atob(f.data).includes('/srv/folio/workspace')))")
    visit('#/more?page=browser')
    page.locator('.server-browser').get_by_role('button',name='打开浏览器',exact=True).click()
    expect(page.locator('.browser-surface img')).to_be_visible()
    page.get_by_role('textbox',name='网址或搜索',exact=True).fill('https://developer.mozilla.org')
    page.get_by_role('textbox',name='网址或搜索',exact=True).press('Enter')
    page.wait_for_function("FolioDemo.getState().sessions.some(s=>s.tabs.some(t=>t.url.includes('mozilla')))")
    page.get_by_role('button',name='新建标签页',exact=True).click()
    expect(page.locator('.browser-tab')).to_have_count(2)
    for route in ['#/overview','#/sites','#/software?page=installed','#/software?page=browsers','#/files','#/more','#/more?page=monitor','#/more?page=schedules','#/more?page=security','#/more?page=appearance','#/more?page=mcp','#/more?page=environment']:
        for width,height in [(1366,768),(390,844),(320,568)]:
            page.set_viewport_size({'width':width,'height':height});visit(route);page.wait_for_timeout(150)
            assert page.evaluate('document.documentElement.scrollWidth<=innerWidth'),(route,width)
    assert not errors,errors
    assert not api_network,api_network
    browser.close()
print(json.dumps({'result':'passed','glyph_font':fonts,'real_ui_with_demo_data':True,'server_api_requests':0,'errors':errors}))
