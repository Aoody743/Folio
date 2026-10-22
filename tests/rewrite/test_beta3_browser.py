#!/usr/bin/env python3
"""Real packaged UI, REST and MCP regression for beta3 schedule outcomes."""
import json
import os
from pathlib import Path
import re
import secrets
import time

from playwright.sync_api import sync_playwright, expect
from fixture import RunningServer
from http_client import APIClient, data
from mcp_client import HTTPMCPClient, tool_data


def main():
    server = RunningServer(os.environ["POWERLINUX_TEST_BINARY"])
    password = "QA-" + secrets.token_urlsafe(24)
    api = APIClient(server.url)
    try:
        data(api.post("/api/v1/auth/setup", {"username": "beta3-admin", "password": password, "setup_token": server.bootstrap}))
        session = data(api.post("/api/v1/auth/login", {"username": "beta3-admin", "password": password}))
        api.csrf = session["csrf_token"]
        token = data(api.post("/api/v1/tokens", {"name": "beta3 schedule regression", "kind": "mcp", "scopes": ["schedules:read", "schedules:write", "files:read", "tasks:read"], "resources": ["*"]}))
        mcp = HTTPMCPClient(server.url + "/mcp", token["token"])
        mcp.initialize()
        (server.data_dir / "workspace" / "present.txt").write_text("beta3 real file hash\n")
        capabilities = data(api.post("/api/v1/actions/capabilities_list", {}))
        create = next(item for item in capabilities if item["name"] == "schedules_create")
        assert "managed_database_backup" in create["input_schema"]["properties"]["action"]["enum"]
        for name, path, outcome in [("Success schedule", "present.txt", "completed"), ("Failure schedule", "missing.txt", "failed")]:
            schedule = tool_data(mcp.call("schedules_create", {"name": name, "mode": "interval", "timezone": "UTC", "interval_seconds": 3600, "action": "files_hash", "input": {"root": "workspace", "path": path}, "enabled": True}))
            run = tool_data(mcp.call("schedules_run", {"id": schedule["id"]}))
            assert run["status"] == "submitted" and run["task_id"]
            deadline = time.monotonic() + 12
            while time.monotonic() < deadline:
                history = data(api.post("/api/v1/actions/schedules_history", {"id": schedule["id"]}))
                if history[0]["status"] == outcome:
                    break
                time.sleep(.1)
            else:
                raise AssertionError("actual task result was not reconciled")
        # The same persisted outcomes survive an actual API process restart.
        server.restart()
        assert tool_data(mcp.call("schedules_history", {"id": schedule["id"]}))[0]["status"] == "failed"
        with sync_playwright() as p:
            browser = p.chromium.launch(executable_path=os.environ.get("POWERLINUX_CHROMIUM", "/usr/bin/chromium"), headless=True, args=["--no-sandbox"])
            page = browser.new_page(viewport={"width": 1440, "height": 1000}, locale="zh-CN")
            errors = []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.goto(server.url, wait_until="domcontentloaded")
            page.get_by_label(re.compile(r"^用户名(?:\s|$)")).fill("beta3-admin")
            page.get_by_label(re.compile(r"^密码(?:\s|$)")).fill(password)
            page.get_by_role("button", name="登录", exact=True).click()
            page.get_by_role("navigation", name="主导航").get_by_role("button", name="更多", exact=True).click()
            page.get_by_role("button", name="计划任务", exact=True).click()
            page.get_by_role("button", name="Success schedule", exact=True).click()
            expect(page.get_by_role("heading", name="最近执行", exact=True)).to_be_visible()
            page.get_by_role("button", name="查看关联任务", exact=True).click()
            expect(page.get_by_role("button", name="读取任务日志", exact=True)).to_be_visible()
            page.get_by_role("button", name="返回列表", exact=True).click()
            page.get_by_role("button", name="创建计划任务", exact=True).click()
            expect(page.get_by_label("目标动作", exact=False)).to_contain_text("managed_database_backup")
            page.locator(".resource-page:not([hidden]) .optional-options > summary").first.click()
            expect(page.get_by_label("上次任务未结束时", exact=False)).to_be_visible()
            expect(page.get_by_label("错过计划执行时", exact=False)).to_be_visible()
            assert not errors, errors
            browser.close()
        print(json.dumps({"real_api": True, "real_mcp": True, "real_chromium": True, "database_backup_schedulable": True, "success_failure_history": True, "api_restart": True, "linked_task_ui": True, "policy_ui": True}))
    finally:
        server.close()


if __name__ == "__main__":
    main()
