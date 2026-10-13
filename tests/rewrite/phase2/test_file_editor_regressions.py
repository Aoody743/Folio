#!/usr/bin/env python3
"""Focused regressions against a real immutable UI/API executable.

These supplement, and never replace, test_files_browser.py. No page injection,
mock API response, or mutable frontend source is used.
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import sys
import traceback
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from fixture import RunningServer
from http_client import APIClient, data
from playwright.sync_api import expect, sync_playwright


GUARD = "文件草稿已保留，请先保存或放弃草稿再切换文件。"


class FileEditorRegressions(unittest.TestCase):
    def setUp(self):
        binary = os.environ.get("POWERLINUX_TEST_BINARY")
        if not binary:
            raise RuntimeError("POWERLINUX_TEST_BINARY must select the actual packaged executable")
        self.artifacts = Path(os.environ.get(
            "POWERLINUX_QA_ARTIFACTS", "/workspace/powerlinux/qa-artifacts/file-editor-regressions"))
        self.artifacts.mkdir(parents=True, exist_ok=True)
        self.server = RunningServer(binary)
        self.addCleanup(self.server.close)
        self.api = APIClient(self.server.url)
        self.password = "QA-" + secrets.token_urlsafe(24)
        self.username = "file-editor-regression"
        data(self.api.post("/api/v1/auth/setup", {
            "username": self.username, "password": self.password,
            "setup_token": self.server.bootstrap,
        }))
        session = data(self.api.post("/api/v1/auth/login", {
            "username": self.username, "password": self.password,
        }))
        self.api.csrf = session["csrf_token"]
        self.original = "中文基线\n第二行"
        for filename, content in (("codec.txt", self.original), ("other.txt", "other file")):
            data(self.api.post("/api/v1/actions/fs_create", {
                "root": "workspace", "path": filename, "content": content, "encoding": "utf-8",
            }))
        self.playwright = sync_playwright().start()
        self.addCleanup(self.playwright.stop)
        self.browser = self.playwright.chromium.launch(
            executable_path=os.environ.get("POWERLINUX_CHROMIUM", "/usr/bin/chromium"),
            headless=True, args=["--no-sandbox"],
        )
        self.addCleanup(self.browser.close)
        self.context = self.browser.new_context(
            viewport={"width": 1440, "height": 1000}, locale="zh-CN", reduced_motion="reduce")
        self.page = self.context.new_page()
        self.errors = []
        self.read_responses = []
        self.page.on("pageerror", lambda error: self.errors.append(str(error)))
        self.page.on("response", self.observe_read)
        self.page.goto(self.server.url, wait_until="domcontentloaded")
        self.page.get_by_label(re.compile(r"^用户名(?:\s|$)")).fill(self.username)
        self.page.get_by_label(re.compile(r"^密码(?:\s|$)")).fill(self.password)
        self.page.get_by_role("button", name="登录", exact=True).click()
        expect(self.page.get_by_role("heading", name="主机概览", exact=True)).to_be_visible()
        self.page.get_by_role("navigation", name="主导航").get_by_role(
            "button", name="文件", exact=True).click()
        expect(self.page.get_by_role("heading", name="文件管理", exact=True)).to_be_visible()
        self.open_file("codec.txt")
        expect(self.content).to_have_text(self.original, use_inner_text=True)

    def observe_read(self, response):
        if not response.url.endswith("/api/v1/actions/fs_read"):
            return
        try:
            request = response.request.post_data_json
            if request.get("path") != "codec.txt":
                return
            payload = response.json()
            self.read_responses.append({
                "encoding": request.get("encoding"), "status": response.status,
                "content": payload.get("data", {}).get("content"),
            })
        except Exception as error:
            self.errors.append("Could not inspect actual fs_read response: " + str(error))

    def open_file(self, filename):
        self.page.get_by_role("button", name=filename, exact=True).click()
        self.editor = self.page.get_by_role("dialog", name="编辑文件 · " + filename, exact=True)
        self.content = self.editor.get_by_role("textbox", name="文件内容", exact=True)
        expect(self.content).to_be_visible()

    def provoke_guard(self, draft):
        self.content.fill(draft)
        self.editor.get_by_role("button", name="关闭资源详情", exact=True).click()
        self.page.get_by_role("button", name="other.txt", exact=True).click()
        expect(self.editor).to_be_visible()
        expect(self.content).to_have_text(draft, use_inner_text=True)
        expect(self.page.get_by_text(GUARD, exact=True)).to_be_visible()
        self.assertEqual((self.server.data_dir / "workspace/codec.txt").read_bytes(), self.original.encode())

    def verify_actual(self, content, encoding="utf-8"):
        raw = (self.server.data_dir / "workspace/codec.txt").read_bytes()
        self.assertEqual(raw, content.encode(encoding))
        result = data(self.api.post("/api/v1/actions/fs_read", {
            "root": "workspace", "path": "codec.txt", "encoding": encoding, "limit": 2 << 20,
        }))
        self.assertEqual(result["content"], content)
        self.assertEqual(result["sha256"], hashlib.sha256(raw).hexdigest())

    def test_small_encoding_save_refresh_and_reopen(self):
        text = "中文磁盘编码\n" * 20
        self.editor.get_by_label("文件编码", exact=True).select_option("gb18030")
        self.content.fill(text)
        self.read_responses.clear()
        self.editor.get_by_role("button", name="保存文件", exact=True).click()
        expect(self.editor.locator(".save-status[role=status]")).to_have_text("已保存")
        self.verify_actual(text, "gb18030")
        # Allow the real save-triggered resource refresh to finish, then inspect
        # its result rather than passing merely because an error has not arrived.
        self.page.wait_for_timeout(1200)
        expect(self.editor.get_by_role("alert")).to_have_count(0)
        self.assertTrue(self.read_responses, "No actual save-triggered fs_read was observed")
        self.assertEqual(self.read_responses[-1], {
            "encoding": "gb18030", "status": 200, "content": text,
        })
        self.editor.get_by_role("button", name="关闭资源详情", exact=True).click()
        self.open_file("codec.txt")
        expect(self.editor.get_by_label("文件编码", exact=True)).to_have_value("gb18030")
        expect(self.content).to_have_text(text, use_inner_text=True)
        expect(self.editor.get_by_role("alert")).to_have_count(0)
        self.verify_actual(text, "gb18030")

    def test_guard_resolved_after_successful_save(self):
        draft = "保存后应清除切换提示\n中文内容"
        self.provoke_guard(draft)
        self.editor.get_by_role("button", name="保存文件", exact=True).click()
        expect(self.editor.locator(".save-status[role=status]")).to_have_text("已保存")
        self.verify_actual(draft)
        expect(self.page.get_by_text(GUARD, exact=True)).not_to_be_visible()
        self.editor.get_by_role("button", name="关闭资源详情", exact=True).click()
        self.open_file("other.txt")
        expect(self.content).to_have_text("other file")

    def test_guard_resolved_after_explicit_discard(self):
        self.provoke_guard("放弃的草稿不能写入磁盘")
        self.editor.get_by_role("button", name="放弃文件草稿", exact=True).click()
        expect(self.content).to_have_text(self.original, use_inner_text=True)
        self.verify_actual(self.original)
        expect(self.page.get_by_text(GUARD, exact=True)).not_to_be_visible()
        self.editor.get_by_role("button", name="关闭资源详情", exact=True).click()
        self.open_file("other.txt")
        expect(self.content).to_have_text("other file")

    def tearDown(self):
        failures = getattr(self._outcome.result, "failures", []) + getattr(self._outcome.result, "errors", [])
        failed = next((message for test, message in failures if test is self), None)
        if failed:
            diagnostic = {
                "test": self.id(), "actual_version": self.api.get("/healthz")[1].get("version"),
                "failure": failed, "page_errors": self.errors, "actual_ui_reads": self.read_responses,
            }
            for secret in (self.password, self.api.csrf, self.server.bootstrap):
                diagnostic["failure"] = diagnostic["failure"].replace(secret, "<redacted fixture credential>")
            path = self.artifacts / (self._testMethodName + ".json")
            path.write_text(json.dumps(diagnostic, ensure_ascii=False, indent=2))
            path.chmod(0o600)
            try:
                self.page.screenshot(path=str(path.with_suffix(".png")), full_page=True, timeout=5000)
            except Exception as error:
                diagnostic["screenshot_error"] = str(error)
                path.write_text(json.dumps(diagnostic, ensure_ascii=False, indent=2))
        self.assertFalse(self.errors, self.errors)


if __name__ == "__main__":
    unittest.main(verbosity=2)
