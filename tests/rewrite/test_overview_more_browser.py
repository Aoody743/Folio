"""Check dashboard notes and centered tile geometry using the actual UI."""
import os
from pathlib import Path
from playwright.sync_api import sync_playwright,expect
out=Path(os.environ.get('POWERLINUX_QA_ARTIFACTS','/tmp/folio-overview-more'));out.mkdir(parents=True,exist_ok=True)
with sync_playwright() as p:
 b=p.chromium.launch(executable_path='/usr/bin/chromium',args=['--no-sandbox'])
 page=b.new_page(viewport={'width':1297,'height':778},locale='zh-CN');errors=[]
 page.on('pageerror',lambda e:errors.append(str(e)))
 page.goto(os.environ.get('FOLIO_DEMO_URL','http://127.0.0.1:4178'));page.locator('#demo-enter').click()
 expect(page.locator('.overview-note')).to_have_count(3)
 left=page.locator('.status-overview').bounding_box();right=page.locator('.overview-notes').bounding_box();assert left['x']+left['width']<right['x']
 assert abs(left['height']-right['height'])<2,(left,right)
 resources=page.locator('.status-overview .resource-status').all();assert resources[1].bounding_box()['y']>=resources[0].bounding_box()['y']+resources[0].bounding_box()['height']
 page.screenshot(path=str(out/'overview-notes.png'))
 page.locator('.overview-note').first.click()
 expect(page.get_by_role('textbox',name='标题',exact=False)).to_be_visible()
 page.get_by_role('textbox',name='标题',exact=False).fill('首页编辑保存成功')
 page.get_by_role('button',name='完成',exact=True).click()
 expect(page.locator('.overview-note').first).to_contain_text('首页编辑保存成功')
 page.evaluate('location.hash="#/more"');expect(page.locator('.more-tile')).to_have_count(14)
 for name in ['运维','活动与记录','设置与接入']:expect(page.get_by_role('heading',name=name,exact=True)).to_be_visible()
 assert page.get_by_role('region',name='运维',exact=True).locator('.more-tile').count()==4
 assert page.get_by_role('region',name='活动与记录',exact=True).locator('.more-tile').count()==3
 assert page.get_by_role('region',name='设置与接入',exact=True).locator('.more-tile').count()==7
 tiles=page.locator('.more-tile').all();boxes=[tile.bounding_box() for tile in tiles]
 assert len({round(box['y']) for box in boxes})==2,boxes
 for box in boxes:assert abs(box['width']-boxes[0]['width'])<1
 for tile in tiles:
  box=tile.bounding_box();icon=tile.locator('.more-tile-icon').bounding_box();label=tile.locator('strong').bounding_box()
  assert abs(icon['x']+icon['width']/2-box['x']-box['width']/2)<1
  assert tile.locator('strong').evaluate('(el)=>getComputedStyle(el).textAlign')=='center'
  assert label['y']>icon['y']+icon['height']
  assert label['y']+label['height']<=box['y']+box['height']
 signout=page.locator('.more-signout').bounding_box();nav=page.locator('.bottom-nav').bounding_box();assert signout['y']+signout['height']<nav['y']
 assert abs(page.locator('.more-groups').bounding_box()['x']+page.locator('.more-groups').bounding_box()['width']/2-1297/2)<2
 page.screenshot(path=str(out/'more-centered.png'))
 for locale in ['zh-CN','en']:
  page.evaluate('(v)=>localStorage.setItem("folio.language",v)',locale);page.reload();page.wait_for_timeout(400)
  for w,h in [(1297,778),(390,844),(320,568)]:
   page.set_viewport_size({'width':w,'height':h})
   for route in ['#/overview','#/more']:
    page.evaluate('(v)=>location.hash=v',route);page.wait_for_timeout(300)
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth'),(locale,w,route)
 assert not errors,errors
 b.close()
print('PASS: latest notes edit/save, two-column dashboard, equal centered tiles, icon/label layout, sign-out placement and bilingual responsive views')
