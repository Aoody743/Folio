"""Exercise project filters and the compact file toolbar on the real exported UI."""
import os
from pathlib import Path
from playwright.sync_api import sync_playwright, expect
url=os.environ.get('FOLIO_DEMO_URL','http://127.0.0.1:4178')
out=Path(os.environ.get('POWERLINUX_QA_ARTIFACTS','/tmp/folio-project-filters'));out.mkdir(parents=True,exist_ok=True)
with sync_playwright() as p:
 b=p.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM','/usr/bin/chromium'),args=['--no-sandbox'])
 page=b.new_page(viewport={'width':1297,'height':778},locale='zh-CN');errors=[]
 page.on('pageerror',lambda e:errors.append(str(e)))
 page.goto(url);page.locator('#demo-enter').click();expect(page.locator('.status-overview')).to_be_visible()
 def visit(route):
  page.evaluate('(route)=>location.hash=route',route)
  page.wait_for_timeout(300)
 visit('#/sites')
 tabs=page.get_by_role('tablist',name='项目类型')
 tabs.get_by_role('tab',name='PHP',exact=True).click();expect(page.locator('.resource-summary-card:visible')).to_have_count(1)
 page.reload();expect(page.get_by_role('tab',name='PHP',exact=True)).to_have_attribute('aria-selected','true');expect(page.locator('.resource-summary-card:visible')).to_have_count(1)
 tabs.get_by_role('tab',name='静态',exact=True).click();expect(page.locator('.resource-summary-card:visible')).to_have_count(2)
 page.get_by_role('combobox',name='网站状态筛选').click()
 page.get_by_role('option',name='已启用',exact=True).click();expect(page.locator('.resource-summary-card:visible')).to_have_count(1)
 page.get_by_role('option',name='已停用',exact=True).click();expect(page.locator('.resource-summary-card:visible')).to_have_count(2)
 page.keyboard.press('Escape')
 tabs.get_by_role('tab',name='Git',exact=True).click();expect(page.locator('.resource-summary-card:visible')).to_have_count(0)
 visit('#/sites')
 expect(page.locator('.resource-summary-card:visible')).to_have_count(3)
 page.screenshot(path=str(out/'sites-project-tabs.png'))
 visit('#/files?root=workspace')
 expect(page.get_by_role('navigation',name='文件路径')).to_be_visible()
 expect(page.locator('.compact-file-table tbody tr')).to_have_count(6)
 toolbar=page.locator('.file-location-toolbar')
 assert page.locator('.breadcrumbs').count()==1
 assert toolbar.get_by_role('textbox').bounding_box()['width']<300
 toolbar.get_by_role('button',name='更多文件选项',exact=True).click()
 page.get_by_role('menuitem',name='全选本页',exact=True).click()
 expect(page.get_by_role('checkbox',name='选择本页文件',exact=True)).to_be_checked()
 toolbar.get_by_role('button',name='更多文件选项',exact=True).click()
 page.get_by_role('menuitem',name='取消选择',exact=True).click()
 expect(page.get_by_role('checkbox',name='选择本页文件',exact=True)).not_to_be_checked()
 toolbar.get_by_role('textbox',name='按文件名筛选').fill('README')
 toolbar.get_by_role('button',name='筛选文件',exact=True).click();expect(page.locator('.compact-file-table tbody tr')).to_have_count(1)
 toolbar.get_by_role('textbox',name='按文件名筛选').fill('');toolbar.get_by_role('button',name='筛选文件',exact=True).click();expect(page.locator('.compact-file-table tbody tr')).to_have_count(6)
 page.screenshot(path=str(out/'files-one-toolbar.png'))
 visit('#/more')
 expect(page.locator('.more-groups')).to_be_visible()
 assert page.locator('.more-tile').first.bounding_box()['height'] >= 90
 page.screenshot(path=str(out/'more-tiles.png'))
 expect(page.locator('.distro-logo')).to_be_visible()
 assert 'ubuntu.svg' in page.locator('.distro-logo').get_attribute('style')
 visit('#/more?page=panel')
 alias=page.get_by_role('textbox',name='服务器显示名称',exact=False)
 expect(alias).to_be_visible();alias.fill('生产服务器 A')
 form=alias.locator('xpath=ancestor::form')
 form.locator('button[type="submit"]').click()
 confirm=form.get_by_role('button',name='确认执行',exact=True)
 if confirm.count():confirm.click()
 expect(page.locator('.host-pill')).to_contain_text('生产服务器 A')
 page.reload();expect(page.locator('.host-pill')).to_contain_text('生产服务器 A')
 for locale in ['zh-CN','en']:
  page.evaluate('(value)=>localStorage.setItem("folio.language",value)',locale);page.reload();page.wait_for_timeout(500)
  for route in ['#/sites','#/sites?project=php&status=running%2Cstopped','#/files?root=workspace','#/files?root=workspace&path=config','#/more','#/more?page=panel']:
   for w,h in [(1297,778),(390,844),(320,568)]:
    page.set_viewport_size({'width':w,'height':h});visit(route)
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth'),(locale,route,w)
 assert not errors,errors
 b.close()
print('PASS: project filters, multi-status, refresh, file toolbar, select-all, search, bilingual responsive layouts')
