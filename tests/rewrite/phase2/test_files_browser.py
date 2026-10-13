#!/usr/bin/env python3
"""Real Next/Astryx/CodeMirror file workflows; actual REST/MCP, no canned data."""
from __future__ import annotations
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import sys
import traceback
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from fixture import RunningServer
from http_client import APIClient,data
from mcp_client import HTTPMCPClient,tool_data
from playwright.sync_api import sync_playwright,expect


def main():
    binary=os.environ.get("POWERLINUX_TEST_BINARY")
    if not binary:
        raise RuntimeError("POWERLINUX_TEST_BINARY must point to the actual packaged UI+API executable")
    artifacts=Path(os.environ.get("POWERLINUX_QA_ARTIFACTS","/workspace/powerlinux/qa-artifacts/phase2-files"))
    artifacts.mkdir(parents=True,exist_ok=True)
    server=RunningServer(binary)
    api=APIClient(server.url)
    password="QA-"+secrets.token_urlsafe(24)
    try:
        data(api.post("/api/v1/auth/setup",{"username":"file-browser-qa","password":password,"setup_token":server.bootstrap}))
        session=data(api.post("/api/v1/auth/login",{"username":"file-browser-qa","password":password}))
        api.csrf=session["csrf_token"]
        token=data(api.post("/api/v1/tokens",{"name":"Browser file QA external AI","kind":"mcp","scopes":["files:read","files:write","tasks:read"],"resources":["fs:workspace"]}))
        mcp=HTTPMCPClient(server.url+"/mcp",token["token"])
        mcp.initialize()
        def read(path,encoding="utf-8"):
            return tool_data(mcp.call("fs_read",{"root":"workspace","path":path,"encoding":encoding,"limit":2 << 20}))
        with sync_playwright() as playwright:
            browser=playwright.chromium.launch(executable_path=os.environ.get("POWERLINUX_CHROMIUM","/usr/bin/chromium"),headless=True,args=["--no-sandbox"])
            context=browser.new_context(viewport={"width":1440,"height":1000},locale="zh-CN",timezone_id="Asia/Singapore",reduced_motion="reduce")
            page=context.new_page()
            errors=[]
            current_stage="load/login"
            def stage(name):
                nonlocal current_stage
                current_stage=name
                print("Browser stage: "+name,flush=True)
            page.on("pageerror",lambda error:errors.append(str(error)))
            page.on("response",lambda response:errors.append("static asset HTTP "+str(response.status)) if response.status>=400 and "/_next/" in response.url else None)
            try:
                page.goto(server.url,wait_until="domcontentloaded")
                page.get_by_label(re.compile(r"^用户名(?:\s|$)")).fill("file-browser-qa")
                page.get_by_label(re.compile(r"^密码(?:\s|$)")).fill(password)
                page.get_by_role("button",name="登录",exact=True).click()
                expect(page.get_by_role("heading",name="主机概览",exact=True)).to_be_visible()
                page.get_by_role("navigation",name="主导航").get_by_role("button",name="文件",exact=True).click()
                expect(page.get_by_role("heading",name="文件管理",exact=True)).to_be_visible()
                expect(page.get_by_label("文件根目录",exact=True)).to_have_value("workspace")
                stage("responsive file page")
                for width in (1440,768,390,320):
                    page.set_viewport_size({"width":width,"height":900})
                    assert page.evaluate("document.documentElement.scrollWidth<=innerWidth"),f"file page horizontal overflow at{width}px"
                    page.screenshot(path=str(artifacts/f"files-{width}.png"),full_page=True)
                page.set_viewport_size({"width":1440,"height":1000})
                filename="qa-browser-file.txt"
                stage("create real file")
                page.get_by_role("button",name="新建文件",exact=True).click()
                create=page.get_by_role("dialog",name="新建文件",exact=True)
                expect(create).to_be_visible()
                create.get_by_label(re.compile(r"^(?:路径|相对路径|文件路径|path)(?:\s|$)")).fill(filename)
                create.get_by_role("button",name="执行操作",exact=True).click()
                create.get_by_role("button",name="确认执行",exact=True).click()
                expect(create.get_by_role("heading",name="操作结果",exact=True)).to_be_visible()
                create.get_by_role("button",name="关闭资源详情",exact=True).click()
                page.get_by_role("button",name=filename,exact=True).click()
                stage("open CodeMirror and save real Chinese content")
                editor=page.get_by_role("dialog",name="编辑文件 · "+filename,exact=True)
                content=editor.get_by_role("textbox",name="文件内容",exact=True)
                expect(content).to_be_visible()
                content.fill("人类通过真实CodeMirror编辑\n第二行")
                editor.get_by_role("button",name="保存文件",exact=True).click()
                expect(editor.locator(".save-status[role=status]")).to_have_text("已保存")
                baseline=read(filename)
                assert baseline["content"]=="人类通过真实CodeMirror编辑\n第二行"
                expect(editor.locator("p.muted").filter(has_text=filename)).to_contain_text(f"· {len(baseline['content'].encode())} B ·")
                stage("actual byte metadata passed; concurrent MCP conflict")
                race={"used":False}
                def concurrent_write(route):
                    if route.request.method=="POST" and not race["used"]:
                        race["used"]=True
                        tool_data(mcp.call("fs_edit",{"root":"workspace","path":filename,"encoding":"utf-8","content":"AI并发编辑","expected_hash":baseline["sha256"]}))
                    route.continue_()
                context.route("**/api/v1/actions/fs_edit",concurrent_write)
                content.fill("冲突时的人类草稿")
                editor.get_by_role("button",name="保存文件",exact=True).click()
                expect(editor.get_by_role("region",name="文件版本冲突")).to_be_visible()
                expect(content).to_have_text("冲突时的人类草稿")
                editor.get_by_role("button",name="关闭资源详情",exact=True).click()
                page.get_by_role("button",name=filename,exact=True).click()
                expect(content).to_have_text("冲突时的人类草稿")
                editor.get_by_role("button",name="采用最新基线并手动合并",exact=True).click()
                content.fill("AI并发编辑\n人类手动合并")
                editor.get_by_role("button",name="保存文件",exact=True).click()
                expect(editor.locator(".save-status[role=status]")).to_have_text("已保存")
                assert read(filename)["content"]=="AI并发编辑\n人类手动合并"
                context.unroute("**/api/v1/actions/fs_edit",concurrent_write)
                stage("offline save retention and retry")
                context.set_offline(True)
                content.fill("断线仍保留文件草稿")
                editor.get_by_role("button",name="保存文件",exact=True).click()
                expect(editor.get_by_role("alert")).to_be_visible()
                editor.get_by_role("button",name="关闭资源详情",exact=True).click()
                page.get_by_role("button",name=filename,exact=True).click()
                expect(content).to_have_text("断线仍保留文件草稿")
                context.set_offline(False)
                editor.get_by_role("button",name="重试保存文件",exact=True).click()
                expect(editor.locator(".save-status[role=status]")).to_have_text("已保存")
                assert read(filename)["content"]=="断线仍保留文件草稿"
                editor.get_by_role("button",name="文件修改历史",exact=True).click()
                stage("actual historical content restore")
                history=editor.get_by_role("region",name="文件历史版本")
                expect(history).to_be_visible()
                revisions=data(api.post("/api/v1/actions/fs_history",{"root":"workspace","path":filename}))
                chosen=next(i for i,item in enumerate(revisions) if item["sha256"]==baseline["sha256"])
                expect(history.locator(".history-row")).to_have_count(len(revisions))
                history.locator(".history-row").nth(chosen).get_by_role("button").click()
                expect(history.locator(".preview pre")).to_have_text(baseline["content"])
                history.get_by_role("button",name="采用此历史内容",exact=True).click()
                expect(content).to_have_text(baseline["content"],use_inner_text=True)
                assert read(filename)["content"]=="断线仍保留文件草稿", "Selecting history mutated the actual file before explicit save"
                editor.get_by_role("button",name="保存文件",exact=True).click()
                expect(editor.locator(".save-status[role=status]")).to_have_text("已保存")
                restored=read(filename)
                assert restored["content"]==baseline["content"]
                after_restore=data(api.post("/api/v1/actions/fs_history",{"root":"workspace","path":filename}))
                assert len(after_restore)==len(revisions)+1, "Saving a chosen historical version did not create a new revision"
                assert after_restore[0]["sha256"]==baseline["sha256"]
                assert after_restore[0]["actor_id"]==revisions[chosen]["actor_id"], "Historical restore did not retain the actual logged-in human author"
                assert after_restore[0]["actor_id"]!= "token:"+token["id"], "Historical restore attributed the real human save to the MCP actor"
                editor.get_by_role("button",name="关闭文件历史",exact=True).click()
                large_text="在线大文件中文 🧪\n"*4000
                stage("large CodeMirror UTF8 PUT and GB18030 disk bytes")
                assert len(large_text.encode())>48*1024
                text_requests=[]
                def observe_text_save(route):
                    if route.request.method=="PUT":
                        text_requests.append({"url":route.request.url,"body":route.request.post_data_buffer})
                    route.continue_()
                context.route("**/api/v1/files/text?*",observe_text_save)
                editor.get_by_label("文件编码",exact=True).select_option("gb18030")
                content.fill(large_text)
                editor.get_by_role("button",name="保存文件",exact=True).click()
                expect(editor.locator(".save-status[role=status]")).to_have_text("已保存",timeout=15000)
                assert len(text_requests)==1, "Large UI edit did not use exactly one real streaming PUT"
                assert text_requests[0]["body"]==large_text.encode(), "UI large text transport was not UTF8"
                assert "encoding=gb18030" in text_requests[0]["url"]
                assert (server.data_dir/"workspace"/filename).read_bytes()==large_text.encode("gb18030")
                assert read(filename,"gb18030")["content"]==large_text
                disk_size=len(large_text.encode("gb18030"))
                expect(editor.locator("p.muted").filter(has_text=filename)).to_contain_text(f"· {disk_size/1024:.1f} KiB ·")
                context.unroute("**/api/v1/files/text?*",observe_text_save)
                page.screenshot(path=str(artifacts/"file-editor.png"),full_page=True)
                editor.get_by_role("button",name="完成文件编辑",exact=True).click()
                large=bytes(range(256))*((5 << 20)//256)+b"verified binary tail"
                stage("5MiB interrupted upload/resume/download")
                page.get_by_role("button",name="上传文件",exact=True).click()
                upload=page.get_by_role("dialog",name="上传文件",exact=True)
                upload.get_by_label("本地上传文件",exact=True).set_input_files({"name":"ui-large.bin","mimeType":"application/octet-stream","buffer":large})
                chunks={"count":0,"id":""}
                def interrupt_second_chunk(route):
                    if route.request.method=="PATCH":
                        chunks["count"]+=1
                        chunks["id"]=route.request.url.split("/uploads/")[1].split("/chunk")[0]
                        if chunks["count"]==2:
                            route.abort("internetdisconnected")
                            return
                    route.continue_()
                context.route("**/api/v1/files/uploads/*/chunk?*",interrupt_second_chunk)
                upload.get_by_role("button",name="开始上传",exact=True).click()
                expect(upload.get_by_text("上传暂停，可续传",exact=False)).to_be_visible(timeout=15000)
                assert chunks["count"]==2
                partial=data(api.post("/api/v1/actions/fs_upload_status",{"root":"workspace","id":chunks["id"]}))
                assert partial["offset"]==1024*1024, "Interrupted UI upload did not retain exactly its acknowledged first chunk"
                upload.get_by_role("button",name="关闭资源详情",exact=True).click()
                page.get_by_role("button",name="上传文件",exact=True).click()
                context.unroute("**/api/v1/files/uploads/*/chunk?*",interrupt_second_chunk)
                upload.get_by_role("button",name="继续上传",exact=True).click()
                expect(upload.get_by_text("已校验并保存",exact=False)).to_be_visible(timeout=30000)
                assert hashlib.sha256((server.data_dir/"workspace/ui-large.bin").read_bytes()).digest()==hashlib.sha256(large).digest()
                upload.get_by_role("button",name="关闭资源详情",exact=True).click()
                with page.expect_download() as info:
                    page.get_by_role("link",name="下载 ui-large.bin",exact=True).click()
                downloaded=Path(info.value.path())
                assert hashlib.sha256(downloaded.read_bytes()).digest()==hashlib.sha256(large).digest()
                assert not errors,errors
                print(json.dumps({"actual_release":api.get("/healthz")[1]["version"],"flows":["UIcreate","CodeMirror edit+realMCP read+actual byte metadata","realMCP concurrent409","closed conflict draft retention","manual merge","offline save retention+retry","select real historical version+human restore creates new revision","large CodeMirror UTF8PUT+GB18030 disk encoding+byte metadata","5MiB binary HTTP upload/transport interruption/resume","actual browser download hash"],"viewports":[1440,768,390,320],"artifacts":str(artifacts)},ensure_ascii=False))
            except Exception:
                original=traceback.format_exc()
                for secret in (password,token["token"],api.csrf,server.bootstrap):
                    original=original.replace(secret,"<redacted fixture credential>")
                diagnostic={"stage":current_stage,"original_failure":original,"page_errors":errors}
                failure=artifacts/"file-browser-failure.json"
                failure.write_text(json.dumps(diagnostic,ensure_ascii=False,indent=2))
                failure.chmod(0o600)
                # Diagnostic capture must never replace the real assertion.
                # A hung browser renderer can also time out on screenshots.
                try:
                    page.screenshot(path=str(artifacts/"file-browser-failure.png"),full_page=True,timeout=5000)
                except Exception as capture:
                    diagnostic["screenshot_error"]=str(capture)
                try:
                    diagnostic["visible_text"]=page.locator("body").inner_text(timeout=5000)
                except Exception as capture:
                    diagnostic["visible_text_error"]=str(capture)
                failure.write_text(json.dumps(diagnostic,ensure_ascii=False,indent=2))
                raise
            finally:
                browser.close()
    finally:
        server.close()

if __name__=="__main__":
    main()
