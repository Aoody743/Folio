#!/usr/bin/env python3
"""Bilingual packaged panel: real authentication, live switching and preserved user input."""
import json
import os
from pathlib import Path
import re
import secrets
from playwright.sync_api import sync_playwright, expect
from fixture import RunningServer
from http_client import APIClient, data


def main():
    artifacts = Path(os.environ.get('POWERLINUX_QA_ARTIFACTS', '/tmp/folio-experience-browser'))
    artifacts.mkdir(parents=True, exist_ok=True)
    server = RunningServer(os.environ['POWERLINUX_TEST_BINARY'])
    password = 'QA-' + secrets.token_urlsafe(24)
    api = APIClient(server.url)
    errors = []
    try:
        data(api.post('/api/v1/auth/setup', {'username': 'experience-admin', 'password': password, 'setup_token': server.bootstrap}))
        with sync_playwright() as p:
            browser = p.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM', '/usr/bin/chromium'), headless=True, args=['--no-sandbox'])
            context = browser.new_context(locale='en-US', viewport={'width': 1440, 'height': 1000}, reduced_motion='reduce')
            page = context.new_page()
            page.on('pageerror', lambda err: errors.append(str(err)))
            page.goto(server.url)
            expect(page.locator('html')).to_have_attribute('lang', 'en')
            expect(page.get_by_role('heading', name='Welcome back', exact=True)).to_be_visible()
            page.locator('input[name=username]').fill('experience-admin')
            page.locator('input[name=password]').fill(password)
            page.get_by_label('语言 / Language', exact=True).select_option('zh-CN')
            expect(page.locator('input[name=password]')).to_have_value(password)
            expect(page.get_by_role('heading', name='欢迎回来', exact=True)).to_be_visible()
            page.get_by_label('语言 / Language', exact=True).select_option('en')
            page.get_by_role('button', name='Login', exact=True).click()
            expect(page.get_by_role('heading', name='Host overview', level=1, exact=True)).to_be_visible()
            expect(page).to_have_title('Folio · Server Panel')
            expect(page.locator('.brand')).to_have_text('Folio · Server Panel')
            nav = page.get_by_role('navigation', name='Main navigation')
            for name in ['Overview', 'Websites', 'Software', 'Files', 'More']:
                expect(nav.get_by_role('button', name=name, exact=True)).to_be_visible()
            page.get_by_role('button', name='Set up later', exact=True).click()
            expect(page.get_by_role('button', name='Continue setup', exact=True)).to_be_visible()
            page.reload()
            expect(page.get_by_role('button', name='Continue setup', exact=True)).to_be_visible()
            for width in [1440, 768, 390, 320]:
                page.set_viewport_size({'width': width, 'height': 900})
                assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), f'overflow: en {width}'
                page.screenshot(path=str(artifacts / f'en-overview-{width}.png'), full_page=True)
            page.set_viewport_size({'width': 1440, 'height': 1000})
            # A language change must not reload the document or mutate form data.
            page.get_by_role('button', name='Create new note', exact=True).click()
            dialog = page.get_by_role('dialog')
            dialog.get_by_label(re.compile('^Title')).fill('我的网站 · customer data')
            dialog.get_by_label(re.compile('^Text|^Body|^Content')).fill('原文 stays exactly as typed')
            dialog.get_by_role('button', name='Create note', exact=True).click()
            expect(dialog).not_to_be_visible()
            page.evaluate('window.experienceSentinel = "same-document"')
            page.get_by_label('语言 / Language', exact=True).select_option('zh-CN')
            assert page.evaluate('window.experienceSentinel') == 'same-document'
            expect(page.get_by_role('button', name='我的网站 · customer data', exact=False)).to_be_visible()
            page.get_by_label('语言 / Language', exact=True).select_option('en')
            expect(page.get_by_role('button', name='我的网站 · customer data', exact=False)).to_be_visible()
            nav.get_by_role('button', name='Software', exact=True).click()
            expect(page.get_by_role('heading', name='Software & plugins', exact=True)).to_be_visible()
            # App copy must be English; user-supplied resource names are exempt.
            page.get_by_text('Reading software directory...', exact=True).wait_for(state='hidden')
            text = page.locator('#main').inner_text()
            assert not re.search('[\u3400-\u9fff]', text), text
            page.screenshot(path=str(artifacts / 'en-software.png'), full_page=True)
            nav.get_by_role('button', name='More', exact=True).click()
            page.get_by_role('button', name=re.compile('Appearance.*Five')).click()
            for name, palette in [('Moon','moon'),('Space','space'),('Violet','violet'),('Pine','pine'),('Sand','sand')]:
                page.get_by_role('radio', name=name + ' theme', exact=True).click()
                expect(page.locator('html')).to_have_attribute('data-palette', palette)
                page.screenshot(path=str(artifacts / ('en-theme-' + palette + '.png')), full_page=True)
            page.get_by_label('语言 / Language', exact=True).select_option('zh-CN')
            expect(page.get_by_role('heading', name='外观与配色', exact=True)).to_be_visible()
            page.reload()
            expect(page.locator('html')).to_have_attribute('lang', 'zh-CN')
            assert page.evaluate('localStorage.getItem("folio.language")') == 'zh-CN'
            page.keyboard.press('Control+k')
            search = page.get_by_role('dialog', name='全局搜索', exact=True)
            search.get_by_role('textbox').fill('system_get_status')
            search.locator('.search-results button').first.click()
            expect(search).not_to_be_visible()
            expect(page.get_by_role('button', name='返回操作列表', exact=True)).to_be_visible()
            assert not errors, errors
            browser.close()
        print(json.dumps({'real_api': True, 'real_chromium': True, 'browser_language': True,
                          'live_switch_preserves_input': True, 'preference_persisted': True,
                          'user_content_unchanged': True, 'optional_setup': True,
                          'english_viewports': [1440, 768, 390, 320], 'artifacts': str(artifacts)}))
    finally:
        server.close()

if __name__ == '__main__':
    main()
