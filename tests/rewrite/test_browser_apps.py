#!/usr/bin/env python3
"""Optional app lifecycle and a real Firefox WASM browser, through Folio Wisp.

Downloads the checksum-pinned upstream release. No browser, proxy, website or
business response is mocked. The outer browser blocks direct website access.
"""
from __future__ import annotations
import json
import os
from pathlib import Path
import secrets
import time

from playwright.sync_api import sync_playwright, expect
from fixture import RunningServer
from http_client import APIClient, data
from test_server_browser import Website, eventually


def main():
    binary = os.environ['POWERLINUX_TEST_BINARY']
    artifacts = Path(os.environ.get('POWERLINUX_QA_ARTIFACTS', '/tmp/folio-browser-apps')) / 'optional-browser-apps'
    artifacts.mkdir(parents=True, exist_ok=True)
    server = RunningServer(binary)
    website = Website()
    try:
        api = APIClient(server.url)
        password = secrets.token_urlsafe(24)
        data(api.post('/api/v1/auth/setup', {'username': 'browser-apps-qa', 'password': password, 'setup_token': server.bootstrap}))
        api.csrf = data(api.post('/api/v1/auth/login', {'username': 'browser-apps-qa', 'password': password}))['csrf_token']

        def call(name, body=None):
            return data(api.post('/api/v1/actions/' + name, body or {}))

        def task_done(task_id):
            job = call('tasks_get', {'id': task_id})
            if job['status'] in ('failed', 'canceled', 'interrupted'):
                raise AssertionError(job)
            return job['status'] == 'succeeded'

        apps = call('browser_apps_list')['items']
        assert len(apps) == 2 and not any(a['installed'] for a in apps), apps
        assert not call('browser_status')['available']
        assert api.get('/api/v1/browser/firefox-wasm/')[0] == 409

        with sync_playwright() as play:
            browser = play.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM', '/usr/bin/chromium'), headless=True,
                        args=['--no-sandbox', '--enable-unsafe-swiftshader'])
            context = browser.new_context(viewport={'width': 1366, 'height': 768}, locale='zh-CN',
                                          permissions=['clipboard-read', 'clipboard-write'])
            context.add_cookies([{'name': c.name, 'value': c.value, 'url': server.url, 'httpOnly': True, 'sameSite': 'Strict'} for c in api.cookies])
            context.add_init_script("if(location.pathname==='/')localStorage.setItem('folio.language','zh-CN')")
            page = context.new_page()
            errors = []
            page.on('pageerror', lambda error: errors.append(str(error)))
            page.goto(server.url + '/#/software?page=applications&subgroup=browsers')
            expect(page.get_by_role('heading', name='浏览器应用', exact=True)).to_be_visible()
            cards = page.locator('.browser-app-row')
            expect(cards).to_have_count(2)
            # Each independent install goes through the actual app UI and task service.
            for index in (0, 1):
                card = cards.nth(index)
                card.get_by_role('button', name='安装', exact=True).click()
                expected_id = 'panel' if index == 0 else 'firefox-wasm'
                def installed():
                    app = next(a for a in call('browser_apps_list')['items'] if a['id'] == expected_id)
                    if app.get('task_id'):
                        task_done(app['task_id'])
                    return app['installed'] and app.get('task_status') == 'succeeded'
                eventually(installed, 'App installation did not finish: ' + expected_id, 180)
                expect(card.get_by_role('button', name='卸载', exact=True)).to_be_visible(timeout=15000)
            print('optional app UI installs passed',flush=True)
            page.screenshot(path=str(artifacts / 'zh-apps-desktop.png'), full_page=True)
            for width, height in ((390, 844), (320, 568)):
                page.set_viewport_size({'width': width, 'height': height})
                assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'App page overflows horizontally'
                cards.nth(1).scroll_into_view_if_needed()
                expect(cards.nth(1).get_by_role('link', name='打开 Firefox', exact=True)).to_be_visible()
                page.screenshot(path=str(artifacts / f'zh-apps-{width}.png'), full_page=True)
            page.set_viewport_size({'width': 1366, 'height': 768})
            with context.expect_page() as popup:
                cards.nth(1).get_by_role('link', name='打开 Firefox', exact=True).click()
            firefox = popup.value
            requests = []
            console_errors = []
            firefox.on('request', lambda request: requests.append(request.url))
            firefox.on('console', lambda message: console_errors.append(message.text[:500]) if message.type == 'error' else None)
            firefox.route(website.url + '/**', lambda route: route.abort())
            firefox.wait_for_load_state()
            assert firefox.evaluate("isSecureContext && crossOriginIsolated && typeof WebAssembly.Suspending === 'function'")
            firefox.locator('#start-btn').click(timeout=120000)
            firefox.wait_for_function("typeof window.geckoEvalChrome === 'function' || document.getElementById('start-btn').textContent === 'Retry'", timeout=120000)
            assert firefox.evaluate("typeof window.geckoEvalChrome === 'function'"), console_errors
            print('Firefox WASM booted',flush=True)
            firefox.wait_for_timeout(3000)
            canvas = firefox.locator('#screen')
            canvas.click(position={'x': 350, 'y': 64})
            firefox.keyboard.press('Control+l')
            nonce = secrets.token_hex(10)
            firefox.keyboard.type(website.url + '/first?wasm_nonce=' + nonce, delay=15)
            firefox.keyboard.press('Enter')
            try:
                eventually(lambda: website.saw('/first', wasm_nonce=nonce), 'Firefox did not reach the website through Wisp', 30)
            except Exception:
                firefox.screenshot(path=str(artifacts / 'firefox-navigation-failed.png'))
                print('Firefox URL debug',firefox.evaluate('window.geckoEvalChrome("JSON.stringify({url:gBrowser.currentURI.spec, focus:document.activeElement?.id, value:gURLBar.value})")'),flush=True)
                print('Network debug',website.requests,console_errors,flush=True)
                raise
            record = next(r for r in website.requests if r['path'] == '/first' and r['query'].get('wasm_nonce') == [nonce])
            assert 'Firefox/' in record['ua'], record
            print('Firefox native navigation and server egress passed',flush=True)
            assert not any(url.startswith(website.url) for url in requests), 'Outer browser accessed the target directly'
            assert not any('puter.work' in url or 'fonts.googleapis' in url for url in requests), requests
            firefox.wait_for_timeout(2500)
            image = firefox.screenshot(path=str(artifacts / 'firefox-wasm-desktop.png'))
            assert len(image) > 12000, 'Firefox canvas appears blank'
            # Clipboard input exercises the upstream browser's real Unicode path.
            canvas.click(position={'x': 100, 'y': 215})
            value = 'Firefox中文输入-' + secrets.token_hex(4)
            firefox.evaluate('(text) => navigator.clipboard.writeText(text)', value)
            firefox.keyboard.press('Control+v')
            firefox.wait_for_timeout(1500)
            canvas.click(position={'x': 90, 'y': 274})
            eventually(lambda: website.saw('/entered', value=value), 'Firefox clipboard input did not reach the server website', 20)
            app = next(a for a in call('browser_apps_list')['items'] if a['id'] == 'firefox-wasm')
            submitted = call('browser_app_change', {'id': app['id'], 'operation': 'uninstall', 'expected_revision': app['revision']})
            eventually(lambda: task_done(submitted['task_id']), 'Firefox uninstall did not finish', 30)
            assert api.get('/api/v1/browser/firefox-wasm/')[0] == 409
            assert not (server.data_dir / 'browser-apps/firefox-wasm').exists()
            assert call('browser_status')['available'], 'Firefox uninstall disabled the panel mode'
            session = call('browser_open')
            call('browser_close', {'id': session['id']})
            firefox.close()
            # Locale updates are delivered through the existing browser storage listener.
            page.evaluate("localStorage.setItem('folio.language','en');dispatchEvent(new StorageEvent('storage',{key:'folio.language'}))")
            expect(page.get_by_role('heading', name='Browser apps', exact=True)).to_be_visible()
            page.screenshot(path=str(artifacts / 'en-apps-desktop.png'), full_page=True)
            assert errors == [], errors
            browser.close()
        server.restart()
        assert api.get('/api/v1/browser/firefox-wasm/')[0] == 409
        assert call('browser_status')['available'], 'Optional app state was lost after restart'
        result = {'optional_default_off': True, 'independent_installs': True, 'actual_firefox_wasm': True,
                  'server_wisp_egress': True, 'no_public_proxy': True, 'native_firefox_address_bar': True,
                  'unicode_clipboard': True, 'independent_uninstall': True, 'state_after_restart': True,
                  'bilingual_apps_ui': True, 'mobile_widths': [390, 320], 'errors': []}
        (artifacts / 'result.json').write_text(json.dumps(result, indent=2))
        print(json.dumps(result))
    finally:
        website.close()
        server.close()


if __name__ == '__main__':
    main()
