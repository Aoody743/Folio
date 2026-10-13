#!/usr/bin/env python3
"""Real Chromium tests against the release UI and API; no mocked API data."""
from __future__ import annotations
import json
import os
from pathlib import Path
import secrets
import re
import time

from playwright.sync_api import sync_playwright, expect
from fixture import RunningServer
from http_client import APIClient, data
from mcp_client import HTTPMCPClient, tool_data


def main():
    binary = os.environ.get("POWERLINUX_TEST_BINARY")
    if not binary:
        raise RuntimeError("POWERLINUX_TEST_BINARY must identify the actual release binary with static UI")
    artifacts = Path(os.environ.get("POWERLINUX_QA_ARTIFACTS", "/workspace/powerlinux/qa-artifacts"))
    artifacts.mkdir(parents=True, exist_ok=True)
    server = RunningServer(binary)
    password = "QA-" + secrets.token_urlsafe(24)
    api = APIClient(server.url)
    try:
        data(api.post("/api/v1/auth/setup", {"username": "browser-admin", "password": password,
                                            "setup_token": server.bootstrap}))
        session = data(api.post("/api/v1/auth/login", {"username": "browser-admin", "password": password}))
        api.csrf = session["csrf_token"]
        token = data(api.post("/api/v1/tokens", {"name": "Browser QA external AI", "kind": "mcp",
                     "scopes": ["notes:read", "notes:write"], "resources": ["host:local"]}))
        mcp = HTTPMCPClient(server.url + "/mcp", token["token"])
        mcp.initialize()
        with sync_playwright() as p:
            browser = p.chromium.launch(executable_path=os.environ.get("POWERLINUX_CHROMIUM", "/usr/bin/chromium"),
                                        headless=True, args=["--no-sandbox"])
            context = browser.new_context(viewport={"width": 1440, "height": 1000}, reduced_motion="reduce", locale="zh-CN", timezone_id="Asia/Singapore")
            page = context.new_page()
            errors = []
            page.on("pageerror", lambda err: errors.append(str(err)))
            page.on("response", lambda response: errors.append(f"static asset HTTP {response.status}: {response.url}") if response.status >= 400 and "/_next/" in response.url else None)
            try:
                page.goto(server.url, wait_until="domcontentloaded")
                page.get_by_label(re.compile(r"^用户名(?:\s|$)")).fill("browser-admin")
                page.get_by_label(re.compile(r"^密码(?:\s|$)")).fill(password)
                page.get_by_role("button", name="登录", exact=True).click()
                expect(page.get_by_role("heading", name="主机概览", exact=True)).to_be_visible()
                for name in ("主机总览", "共享便签", "最近任务", "待处理事件"):
                    expect(page.get_by_role("heading", name=name, exact=True)).to_be_visible()
                nav = page.get_by_role("navigation", name="主导航")
                assert nav.get_by_role("button").count() == 5
                for name in ("概览", "网站", "软件", "文件", "更多"):
                    expect(nav.get_by_role("button", name=name, exact=True)).to_be_visible()
                assert "聊天" not in page.locator("body").inner_text()
                for width in (1440, 768, 390, 320):
                    page.set_viewport_size({"width": width, "height": 900})
                    assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth"), f"horizontal overflow at {width}px"
                    box = nav.bounding_box()
                    assert box and box["y"] + box["height"] <= 901, (width, box)
                    page.screenshot(path=str(artifacts / f"overview-{width}.png"), full_page=True)
                page.set_viewport_size({"width": 1440, "height": 1000})
                nav.get_by_role("button", name="更多", exact=True).click()
                page.get_by_role("button", name=re.compile("外观.*五套配色")).click()
                for label, palette in (("月白", "moon"), ("深空", "space"), ("雾紫", "violet"), ("松绿", "pine"), ("暖砂", "sand")):
                    radio = page.get_by_role("radio", name=label + "主题")
                    radio.click()
                    expect(radio).to_have_attribute("aria-checked", "true")
                    assert page.locator("html").get_attribute("data-palette") == palette
                    page.screenshot(path=str(artifacts / f"theme-{palette}.png"), full_page=True)
                page.reload(wait_until="domcontentloaded")
                assert page.locator("html").get_attribute("data-palette") == "sand"
                nav = page.get_by_role("navigation", name="主导航")
                nav.get_by_role("button", name="概览", exact=True).click()
                page.get_by_role("button", name="新建便签", exact=True).click()
                dialog = page.get_by_role("dialog", name="新建便签")
                expect(dialog).to_be_visible()
                dialog.get_by_label(re.compile(r"^标题(?:\s|$)")).fill("浏览器共享便签")
                dialog.get_by_label(re.compile(r"^正文(?:\s|$)")).fill("人类交接信息\n\n<script>window.qaXSS=1</script>\n[危险链接](javascript:alert(1))")
                expect(dialog.get_by_label(re.compile(r"^关联资源(?:\s|$)"))).to_have_value("host:local")
                dialog.get_by_role("button", name="创建便签", exact=True).click()
                expect(dialog).not_to_be_visible()
                note = next(n for n in data(api.get("/api/v1/notes")) if n["title"] == "浏览器共享便签")
                observed = tool_data(mcp.call("notes_get", {"id": note["id"]}))
                assert observed["created_by"]["kind"] == "human"
                assert "人类交接信息" in observed["content"]
                ai_note = tool_data(mcp.call("notes_update", {"id": note["id"], "expected_revision": 1,
                                                             "content": "外部 AI 更新了这条便签"}))
                expect(page.get_by_text("外部 AI 更新了这条便签", exact=True)).to_be_visible(timeout=5000)
                page.get_by_role("button", name="浏览器共享便签").click()
                dialog = page.get_by_role("dialog", name="便签详情")
                expect(dialog.get_by_text("AI · Browser QA external AI", exact=False)).to_be_visible()
                dialog.get_by_role("button", name="修改历史", exact=True).click()
                expect(dialog.get_by_role("region", name="修改历史")).to_be_visible()
                dialog.get_by_role("button", name="关闭历史", exact=True).click()
                dialog.get_by_role("button", name="编辑", exact=True).click()
                race = {"used": False}
                def real_concurrent_write(route):
                    if route.request.method == "PATCH" and not race["used"]:
                        race["used"] = True
                        tool_data(mcp.call("notes_update", {"id": note["id"], "expected_revision": 2,
                                                            "content": "AI 并发更改"}))
                    route.continue_()
                context.route("**/api/v1/notes/" + note["id"], real_concurrent_write)
                dialog.get_by_label(re.compile(r"^正文(?:\s|$)")).fill("我的人类草稿")
                expect(dialog.get_by_role("region", name="版本冲突")).to_be_visible(timeout=5000)
                expect(dialog.get_by_label(re.compile(r"^正文(?:\s|$)"))).to_have_value("我的人类草稿")
                dialog.get_by_role("button", name="以最新版本为基础合并", exact=True).click()
                dialog.get_by_label(re.compile(r"^正文(?:\s|$)")).fill("AI 并发更改\n我的人类草稿")
                dialog.get_by_role("button", name="完成", exact=True).click()
                expect(dialog).not_to_be_visible()
                assert data(api.get("/api/v1/notes/" + note["id"]))["content"] == "AI 并发更改\n我的人类草稿"
                page.get_by_role("button", name="浏览器共享便签").click()
                dialog = page.get_by_role("dialog", name="便签详情")
                dialog.get_by_role("button", name="编辑", exact=True).click()
                context.set_offline(True)
                dialog.get_by_label(re.compile(r"^正文(?:\s|$)")).fill("断线时不能丢失的草稿")
                expect(dialog.get_by_role("alert")).to_be_visible(timeout=5000)
                dialog.get_by_role("button", name="关闭便签", exact=True).click()
                page.get_by_role("button", name="浏览器共享便签").click()
                expect(dialog.get_by_label(re.compile(r"^正文(?:\s|$)"))).to_have_value("断线时不能丢失的草稿")
                context.set_offline(False)
                dialog.get_by_role("button", name="重试保存", exact=True).click()
                expect(dialog.locator(".save-status[role=status]")).to_have_text("已保存", timeout=5000)
                dialog.get_by_role("button", name="完成", exact=True).click()
                expect(dialog).not_to_be_visible()
                assert data(api.get("/api/v1/notes/" + note["id"]))["content"] == "断线时不能丢失的草稿"
                # Hostile Markdown is stored as text, never executable HTML/JS.
                xss = data(api.post("/api/v1/notes", {"title": "XSS regression", "target_id": "host:local",
                                "content": "<img src=x onerror=window.qaXSS=1>\n[bad](javascript:alert(1))"}))
                page.get_by_role("button", name="刷新数据", exact=True).click()
                page.get_by_role("button", name="XSS regression").click()
                expect(page.get_by_role("dialog", name="便签详情")).to_be_visible()
                assert page.evaluate("window.qaXSS") is None
                assert page.locator('a[href^="javascript:"]').count() == 0
                page.keyboard.press("Escape")
                expect(page.get_by_role("dialog", name="便签详情")).not_to_be_visible()
                api_token = data(api.post("/api/v1/tokens", {"name": "Browser QA API client", "kind": "api",
                    "scopes": ["notes:read", "notes:write"], "resources": ["host:local"]}))
                automated = APIClient(server.url, api_token["token"])
                current = data(automated.get("/api/v1/notes/" + note["id"]))
                api_note = data(automated.patch("/api/v1/notes/" + note["id"], {"expected_revision": current["revision"], "content": "由真实 API 身份修改"}))
                assert api_note["updated_by"]["kind"] == "api"
                expect(page.get_by_text("由真实 API 身份修改", exact=True)).to_be_visible(timeout=5000)
                page.get_by_role("button", name="浏览器共享便签").click()
                api_dialog = page.get_by_role("dialog", name="便签详情")
                expect(api_dialog.get_by_text("客户端 · Browser QA API client", exact=False)).to_be_visible()
                assert "人 · Browser QA API client" not in api_dialog.inner_text()
                assert "AI · Browser QA API client" not in api_dialog.inner_text()
                page.keyboard.press("Escape")
                expect(api_dialog).not_to_be_visible()
                page.keyboard.press("Control+k")
                expect(page.get_by_role("dialog", name="全局搜索")).to_be_visible()
                page.keyboard.press("Escape")
                expect(page.get_by_role("dialog", name="全局搜索")).not_to_be_visible()
                assert not errors, errors
                print(json.dumps({"browser": "system Chromium", "real_api": True, "static_export": True,
                                  "viewports": [1440, 768, 390, 320], "themes": 5,
                                  "flows": ["login", "UI creates/MCP reads", "MCP updates/UI SSE", "history",
                                            "real concurrent conflict", "manual merge", "offline draft+retry", "Markdown XSS", "human/MCP/API source labels", "keyboard/Escape"],
                                  "artifacts": str(artifacts)}, ensure_ascii=False))
            except Exception:
                page.screenshot(path=str(artifacts / "browser-failure.png"), full_page=True)
                (artifacts / "browser-failure.json").write_text(json.dumps({
                    "page_errors": errors, "visible_text": page.locator("body").inner_text(),
                }, ensure_ascii=False, indent=2))
                raise
            browser.close()
    finally:
        server.close()

if __name__ == "__main__":
    main()
