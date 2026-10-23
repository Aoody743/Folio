#!/usr/bin/env python3
"""Shared navigation and installed-service fallback; no host changes are run."""
import json
import os
from pathlib import Path
import secrets
from playwright.sync_api import expect, sync_playwright
from fixture import RunningServer
from http_client import APIClient, data


def main():
    server = RunningServer(os.environ["POWERLINUX_TEST_BINARY"])
    password = secrets.token_urlsafe(32)
    errors = []
    out = Path(os.environ.get("POWERLINUX_QA_ARTIFACTS", "/tmp/folio-design-system"))
    out.mkdir(parents=True, exist_ok=True)
    try:
        api = APIClient(server.url)
        data(api.post("/api/v1/auth/setup", {"username": "design-qa", "password": password, "setup_token": server.bootstrap}))
        with sync_playwright() as pw:
            browser = pw.chromium.launch(executable_path=os.environ.get("POWERLINUX_CHROMIUM", "/usr/bin/chromium"), headless=True, args=["--no-sandbox"])
            page = browser.new_page(viewport={"width": 1366, "height": 768}, locale="zh-CN", reduced_motion="reduce")
            page.on("pageerror", lambda error: errors.append(str(error)))
            instance = {"id": "nginx-existing", "plugin_id": "nginx", "instance_id": "existing", "status": "running", "installed_version": "1.28.0", "updated_at": "2026-10-23T00:00:00Z"}
            page.route("**/api/v1/actions/plugins_catalog", lambda route: route.fulfill(status=403, json={"error": {"code": "forbidden", "message": "Catalog scope denied"}}))
            page.route("**/api/v1/actions/plugins_list", lambda route: route.fulfill(json={"data": {"items": [instance], "total": 1}}))
            page.route("**/api/v1/actions/plugins_get", lambda route: route.fulfill(json={"data": instance}))
            page.goto(server.url)
            page.locator("input[name=username]").fill("design-qa")
            page.locator("input[name=password]").fill(password)
            page.get_by_role("button", name="登录", exact=True).click()
            expect(page.locator(".status-overview")).to_be_visible()

            def visit(route):
                page.evaluate("route => location.hash = route", route)
                expect(page.locator(".app")).to_have_attribute("data-section", route.lstrip("#/").split("?")[0])

            visit("#/software?page=installed")
            tabs = page.get_by_role("tablist", name="软件资源类型", exact=True)
            expect(tabs.get_by_role("tab", name="已安装服务", exact=True)).to_have_attribute("aria-selected", "true")
            # Shared Astryx roving focus: only one tab is in the Tab sequence.
            assert tabs.locator('[role=tab][tabindex="0"]').count() == 1
            first = tabs.get_by_role("tab", name="已安装服务", exact=True)
            first.focus()
            page.keyboard.press("ArrowRight")
            expect(tabs.get_by_role("tab", name="软件库", exact=True)).to_be_focused()
            page.keyboard.press("Enter")
            expect(page.get_by_role("alert").filter(has_text="无访问权限")).to_be_visible()
            expect(page.locator(".software-empty")).to_have_count(0)
            tabs.get_by_role("tab", name="已安装服务", exact=True).click()
            page.get_by_role("button", name="nginx existing", exact=True).click()
            detail = page.get_by_role("region", name="软件 · nginx", exact=True)
            expect(detail).to_be_visible()
            expect(detail.get_by_text("existing", exact=True)).to_be_visible()
            expect(detail.get_by_role("tab", name="管理操作", exact=True)).to_have_attribute("aria-selected", "true")
            # A missing catalog cannot expose an unverified install plan.
            expect(detail.locator("form:visible")).to_have_count(0)
            detail.get_by_role("tab", name="实例", exact=True).click()
            expect(detail.locator("form")).to_have_count(0)
            page.screenshot(path=str(out / "installed-without-catalog.png"))
            page.get_by_role("button", name="返回列表", exact=True).click()
            # New and bookmarked routes both select the same single-level app tab.
            for route in ("#/software?page=browsers", "#/software?page=applications&subgroup=browsers"):
                visit(route)
                expect(tabs.get_by_role("tab", name="浏览器应用", exact=True)).to_have_attribute("aria-selected", "true")
                expect(page.get_by_role("heading", name="浏览器应用", exact=True)).to_be_visible()
                assert page.get_by_role("tablist", name="应用类型", exact=True).count() == 0
            tabs.get_by_role("tab", name="已部署应用", exact=True).click()
            expect(tabs.get_by_role("tab", name="已部署应用", exact=True)).to_have_attribute("aria-selected", "true")
            for width, height in ((1440, 900), (390, 844), (320, 568)):
                page.set_viewport_size({"width": width, "height": height})
                for route in ("#/software?page=browsers", "#/more?page=appearance", "#/more?page=security", "#/more?page=mcp"):
                    visit(route)
                    page.wait_for_timeout(150)
                    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), (route, width)
                    expect(page.locator("html")).to_have_attribute("data-astryx-theme", "folio")
                    assert page.get_by_role("navigation", name="主导航").get_by_role("button").count() == 5
            assert not errors, errors
            browser.close()
    finally:
        server.close()
    print(json.dumps({"installed_without_catalog": "passed", "tabs_keyboard": "passed", "legacy_browser_route": "passed", "errors": errors}))


if __name__ == "__main__":
    main()
