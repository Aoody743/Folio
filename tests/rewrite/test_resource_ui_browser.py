"""Verify the real resource UI with browser-local demo data."""
import os
from pathlib import Path
from playwright.sync_api import sync_playwright,expect
url = os.environ.get("FOLIO_DEMO_URL", "http://127.0.0.1:4178")
out = Path(os.environ.get("POWERLINUX_QA_ARTIFACTS", "/tmp/folio-resource-ui"))
out.mkdir(parents=True, exist_ok=True)
with sync_playwright() as p:
 b=p.chromium.launch(executable_path='/usr/bin/chromium',args=['--no-sandbox'])
 page=b.new_page(viewport={'width':1297,'height':778},locale='zh-CN');errors=[]
 page.on('pageerror',lambda e:errors.append(str(e)))
 page.goto(url);page.locator('#demo-enter').click();expect(page.locator('.status-overview')).to_be_visible()
 expect(page.locator('.host-pill')).to_contain_text('Ubuntu')
 expect(page.get_by_role('button',name='全局设置',exact=True)).to_be_visible()
 page.get_by_role('button',name='全局设置',exact=True).click();expect(page.get_by_role('heading',name='面板与访问设置')).to_be_visible()
 def visit(route):
  page.evaluate('(v)=>location.hash=v',route);page.wait_for_timeout(600)
 visit('#/sites');expect(page.locator('.resource-summary-card:visible')).to_have_count(3)
 page.locator('.resource-summary-card:visible').filter(has_text='Northstar Studio').get_by_role('button',name='证书已部署').click()
 expect(page.get_by_role('button',name='管理证书与 DNS 凭据')).to_be_visible()
 visit('#/software?page=installed');expect(page.locator('.resource-summary-card:visible')).to_have_count(3)
 page.locator('.resource-summary-card:visible').filter(has_text='MySQL').get_by_role('button',name='MySQL',exact=True).click();expect(page.get_by_role('region',name='软件 · MySQL')).to_be_visible()
 visit('#/software?page=catalog');expect(page.locator('.catalog-category')).not_to_have_count(0)
 assert page.locator('.pagination:visible').count()==0
 page.locator('.catalog-rail .software-card').first.click();expect(page.locator('.resource-page:visible')).to_be_visible()
 visit('#/files?root=workspace');expect(page.locator('.compact-file-table tbody tr')).to_have_count(6)
 page.get_by_role('button',name='操作 README.md',exact=True).click();expect(page.get_by_role('menuitem',name='编辑文件',exact=True)).to_be_visible();page.keyboard.press('Escape')
 page.locator('tr').filter(has=page.get_by_role('button',name='README.md',exact=True)).click(button='right');expect(page.get_by_role('menuitem',name='权限',exact=True)).to_be_visible()
 page.get_by_role('menuitem',name='权限',exact=True).click();expect(page.get_by_role('dialog')).to_be_visible();page.get_by_role('dialog').get_by_role('button',name='关闭',exact=True).click()
 visit('#/more?page=browser');page.locator('.server-browser').get_by_role('button',name='打开浏览器',exact=True).click();expect(page.locator('.browser-surface img')).to_be_visible();page.wait_for_timeout(1000)
 assert page.locator('.browser-surface').bounding_box()['height']>=250
 for locale in ['zh-CN','en']:
  page.evaluate('(v)=>localStorage.setItem("folio.language",v)',locale);page.reload();page.wait_for_timeout(700)
  for route in ['#/sites','#/software?page=installed','#/software?page=catalog','#/software?page=browsers','#/files?root=workspace','#/more?page=panel','#/more?page=browser']:
   for w,h in [(1297,778),(390,844),(320,568)]:
    page.set_viewport_size({'width':w,'height':h});visit(route)
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'),(locale,route,w)
    if locale=='zh-CN' and w==1297:page.screenshot(path=str(out / ('check-'+route[2:].replace('?','-').replace('=','-').replace('/','-')+'.png')))
 assert not errors,errors
 b.close()
print('PASS: topbar, project cards, TLS, service details, catalog rails, file menu/dialog, browser, bilingual responsive layout')
