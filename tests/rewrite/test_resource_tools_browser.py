#!/usr/bin/env python3
"""Search display fixtures; real environment probe, queued uploads and batch copy.
All filesystem mutations are confined to RunningServer's temporary workspace.
"""
import json,os,secrets
from collections import Counter
from pathlib import Path
from playwright.sync_api import sync_playwright,expect
from fixture import RunningServer
from http_client import APIClient,data

def main():
    server=RunningServer(os.environ['POWERLINUX_TEST_BINARY'])
    password=secrets.token_urlsafe(32)
    output=Path(os.environ.get('POWERLINUX_QA_ARTIFACTS','/tmp/folio-resource-tools'));output.mkdir(parents=True,exist_ok=True)
    errors=[];begins=[];fail={'once':True};reads=[]
    try:
        api=APIClient(server.url)
        data(api.post('/api/v1/auth/setup',{'username':'tools-qa','password':password,'setup_token':server.bootstrap}))
        workspace=server.data_dir/'workspace';(workspace/'copies').mkdir();(workspace/'a.txt').write_text('a');(workspace/'b.txt').write_text('bbb')
        with sync_playwright() as pw:
            browser=pw.chromium.launch(executable_path=os.environ.get('POWERLINUX_CHROMIUM','/usr/bin/chromium'),headless=True,args=['--no-sandbox'])
            page=browser.new_page(viewport={'width':1440,'height':900},locale='zh-CN',reduced_motion='reduce')
            page.on('pageerror',lambda e:errors.append(str(e)))
            def upload_begin(route):
                path=route.request.post_data_json['path'];begins.append(path)
                if path=='queue-b.txt' and fail['once']:
                    fail['once']=False
                    return route.fulfill(status=503,json={'error':{'code':'fixture_disconnect','message':'Simulated upload interruption'}})
                route.continue_()
            page.route('**/api/v1/actions/fs_upload_begin',upload_begin)
            page.goto(server.url);page.locator('input[name=username]').fill('tools-qa');page.locator('input[name=password]').fill(password);page.get_by_role('button',name='登录',exact=True).click();expect(page.locator('.status-overview')).to_be_visible()
            def visit(route):
                page.evaluate('(r)=>{location.hash=r;window.scrollTo(0,0)}',route)
                expect(page.locator('.app')).to_have_attribute('data-section',route.split('?')[0].split('/')[-1])
            visit('#/more?page=environment')
            expect(page.locator('.environment-panel')).to_contain_text('1.0-beta7-ui-batch3')
            expect(page.locator('.environment-panel')).to_contain_text('执行器尚未配置')
            page.screenshot(path=str(output/'environment.png'))
            visit('#/more?page=monitor&view=alerts')
            expect(page.get_by_role('region',name='监控告警',exact=True)).to_be_visible()
            visit('#/files?root=workspace&new=true')
            expect(page.locator('.action-tile:visible')).to_have_count(0)
            upload=page.get_by_role('region',name='上传文件',exact=True)
            upload.get_by_label('本地上传文件',exact=True).set_input_files([{'name':'queue-a.txt','mimeType':'text/plain','buffer':b'first actual upload'},{'name':'queue-b.txt','mimeType':'text/plain','buffer':b'second actual upload'}])
            upload.get_by_role('button',name='开始上传',exact=True).click()
            expect(upload.locator('.upload-queue li').nth(0)).to_contain_text('已校验并保存',timeout=20000)
            expect(upload.locator('.upload-queue li').nth(1)).to_contain_text('上传已暂停')
            assert (workspace/'queue-a.txt').read_bytes()==b'first actual upload'
            assert not (workspace/'queue-b.txt').exists()
            page.screenshot(path=str(output/'upload-paused.png'))
            upload.get_by_role('button',name='继续上传',exact=True).click()
            expect(upload.locator('.upload-queue li').nth(1)).to_contain_text('已校验并保存',timeout=20000)
            assert Counter(begins)=={'queue-a.txt':1,'queue-b.txt':2},begins
            assert (workspace/'queue-b.txt').read_bytes()==b'second actual upload'
            upload.get_by_role('button',name='返回列表',exact=True).click()
            page.get_by_label('文件排序',exact=True).select_option('size:desc')
            expect(page).to_have_url(__import__('re').compile('sort=size.*order=desc'))
            page.reload();expect(page.get_by_label('文件排序',exact=True)).to_have_value('size:desc')
            page.get_by_label('文件排序',exact=True).select_option('name:asc')
            page.get_by_label('选择 a.txt',exact=True).check();page.get_by_label('选择 b.txt',exact=True).check()
            page.get_by_role('button',name='批量操作 (2)',exact=True).click()
            batch=page.get_by_role('region',name='批量文件操作',exact=True)
            batch.get_by_label('目标目录',exact=True).fill('copies')
            batch.get_by_role('button',name='预检批量文件变更',exact=True).click()
            expect(batch).to_contain_text('预检 2 项，目标冲突 0 项。',timeout=20000)
            assert not (workspace/'copies'/'a.txt').exists(),'preview mutated files'
            batch.get_by_role('button',name='应用批量文件变更',exact=True).click()
            expect(batch.get_by_text('确认操作与变更参数',exact=True)).to_be_visible()
            assert not (workspace/'copies'/'a.txt').exists(),'mutation before confirmation'
            batch.get_by_role('button',name='确认执行',exact=True).click()
            expect(batch.get_by_text('执行成功',exact=True)).to_have_count(2,timeout=20000)
            assert (workspace/'copies'/'a.txt').read_text()=='a' and (workspace/'copies'/'b.txt').read_text()=='bbb'
            assert (workspace/'a.txt').read_text()=='a'
            page.screenshot(path=str(output/'batch-copy.png'))
            batch.get_by_role('button',name='返回列表',exact=True).click()
            # Search listings are explicit read fixtures. No resource creation is submitted.
            def search_fixture(route):
                action=route.request.url.rsplit('/',1)[-1];reads.append(action)
                values={'sites_list':{'items':[{'id':'site-needle','name':'Needle website','domains':['needle.example.test'],'status':'running','revision':1}],'total':1},'hosted_apps_list':{'items':[{'id':'app-needle','name':'Needle app'}]},'database_connections_list':{'items':[{'id':'db-needle','name':'Needle database','host':'db.example.test','engine':'mysql'}]},'cache_connections_list':{'items':[]}}
                route.fulfill(status=200,json={'data':values[action]})
            for name in ['sites_list','hosted_apps_list','database_connections_list','cache_connections_list']:page.route('**/api/v1/actions/'+name,search_fixture)
            visit('#/more')
            page.get_by_role('button',name='全局搜索',exact=True).click()
            dialog=page.get_by_role('dialog',name='全局搜索',exact=True)
            dialog.get_by_placeholder('页面、域名、应用与数据库…',exact=True).fill('needle')
            expect(dialog.get_by_role('button').filter(has_text='Needle website')).to_be_visible()
            expect(dialog.get_by_role('button').filter(has_text='Needle app')).to_be_visible()
            expect(dialog.get_by_role('button').filter(has_text='Needle database')).to_be_visible()
            page.screenshot(path=str(output/'resource-search.png'))
            dialog.get_by_role('button').filter(has_text='Needle database').click()
            expect(page).to_have_url(__import__('re').compile('page=database&id=db-needle'))
            expect(dialog).not_to_be_visible()
            assert not errors,errors
            for width,height in [(390,844),(320,568)]:
                page.set_viewport_size({'width':width,'height':height});visit('#/more?page=environment');expect(page.locator('.environment-panel')).to_be_visible()
                assert page.evaluate('document.documentElement.scrollWidth<=innerWidth')
                page.screenshot(path=str(output/f'environment-{width}.png'))
                visit('#/files?root=workspace')
                expect(page.locator('.file-root-field')).to_be_visible();expect(page.locator('.file-sort-field')).to_be_visible()
                root_box=page.locator('.file-root-field').bounding_box();sort_box=page.locator('.file-sort-field').bounding_box()
                assert root_box['y']+root_box['height']<=sort_box['y'],(root_box,sort_box)
                expect(page.locator('.resource-table:visible tbody tr')).to_have_count(4)
                assert page.evaluate('document.documentElement.scrollWidth<=innerWidth')
                expect(page.locator('.action-tile:visible')).to_have_count(0)
            browser.close()
        print(json.dumps({'result':'passed','real_uploads':2,'completed_file_uploaded_once':True,'real_batch_copy':2,'search':'display fixtures','errors':errors,'artifacts':str(output)},ensure_ascii=False))
    finally:server.close()
if __name__=='__main__':main()
