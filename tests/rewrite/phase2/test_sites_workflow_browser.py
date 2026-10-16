#!/usr/bin/env python3
"""Real browser coverage for dev12 site categories and system-change planning.

This test targets an actual QA-owned dev12 panel and Agent. It performs real
category/site mutations, so it requires an explicitly opened exclusive QA
window and a private fixture file. It never mocks the SDK/API and never clicks
system-change apply, confirm, or rollback.

Fixture JSON (0600, outside the checkout):
  url, username, password, version, site_id, site_name, service_id

Run only after QA opens the exclusive VM window:
  POWERLINUX_QA_EXCLUSIVE_WRITE_WINDOW=YES
  POWERLINUX_QA_WORKFLOW_BROWSER_FILE=/private/fixture.json
  python3 tests/rewrite/phase2/test_sites_workflow_browser.py

The selected site must be safe to edit, currently unclassified, and managed
(not advanced/manual configuration). `service_id` must name a QA-approved
managed service for plan-only preflight. No credentials are printed.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import re
import secrets
import sys
import stat
import time
import traceback
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from http_client import APIClient
from playwright.sync_api import expect, sync_playwright


TERMINAL_TASK_STATES = {"succeeded", "failed", "canceled", "cancelled", "interrupted", "completed"}


def read_private_fixture(path: str) -> dict[str, Any]:
    fixture_path = Path(path)
    if fixture_path.is_symlink() or not fixture_path.is_file():
        raise RuntimeError("The QA browser fixture must be a regular private file")
    if stat.S_IMODE(fixture_path.stat().st_mode) != 0o600:
        raise RuntimeError("The QA browser fixture must have mode 0600")
    repo_root = Path(__file__).resolve().parents[3]
    if fixture_path.resolve().is_relative_to(repo_root):
        raise RuntimeError("The QA browser fixture must be outside the checkout")
    value = json.loads(fixture_path.read_text())
    required = ("url", "username", "password", "version", "site_id", "site_name", "service_id")
    missing = [name for name in required if not isinstance(value.get(name), str) or not value[name]]
    if missing:
        raise RuntimeError("The QA browser fixture is missing required fields: " + ", ".join(missing))
    if not value["url"].startswith(("https://", "http://")):
        raise RuntimeError("The QA browser fixture URL must use HTTP or HTTPS")
    return value


def api_action(api: APIClient, name: str, body: dict[str, Any]) -> Any:
    status, payload, _ = api.post("/api/v1/actions/" + name, body)
    if status != 200 or not isinstance(payload, dict) or "data" not in payload:
        error = payload.get("error", {}) if isinstance(payload, dict) else {}
        code = error.get("code", "unknown") if isinstance(error, dict) else "unknown"
        raise AssertionError(f"{name} returned HTTP {status}, code={code}")
    return payload["data"]


def categories(api: APIClient) -> list[dict[str, Any]]:
    value = api_action(api, "sites_categories_list", {})
    items = value.get("items") if isinstance(value, dict) else None
    if not isinstance(items, list):
        raise AssertionError("sites_categories_list returned no category items")
    return items


def category_by_id(api: APIClient, category_id: str) -> dict[str, Any] | None:
    return next((item for item in categories(api) if item.get("id") == category_id), None)


def site_by_id(api: APIClient, site_id: str) -> dict[str, Any]:
    value = api_action(api, "sites_get", {"id": site_id})
    if not isinstance(value, dict) or value.get("id") != site_id:
        raise AssertionError("sites_get did not return the selected QA site")
    return value


def site_category(api: APIClient, site_id: str) -> str:
    return str(site_by_id(api, site_id).get("category_id") or "")


def wait_for_site_category(api: APIClient, site_id: str, expected: str, timeout: float = 120) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = site_by_id(api, site_id)
        if str(last.get("category_id") or "") == expected:
            return last
        time.sleep(0.4)
    raise AssertionError(f"site category did not reach expected state; actual={str((last or {}).get('category_id') or '')!r}")


def restore_site_category(api: APIClient, site_id: str, expected: str) -> None:
    current = site_by_id(api, site_id)
    if str(current.get("category_id") or "") == expected:
        return
    revision = current.get("revision")
    if not isinstance(revision, int):
        raise AssertionError("Cannot safely restore the site without its current revision")
    task = api_action(api, "sites_update", {
        "id": site_id,
        "expected_revision": revision,
        "category_id": expected,
    })
    task_id = task.get("id", task.get("task_id")) if isinstance(task, dict) else None
    if not isinstance(task_id, str) or not task_id:
        raise AssertionError("Site category restore did not return a task ID")
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        value = api_action(api, "tasks_get", {"id": task_id})
        state = value.get("status", value.get("state")) if isinstance(value, dict) else None
        if state in TERMINAL_TASK_STATES:
            if state not in {"succeeded", "completed"}:
                raise AssertionError(f"Site category restore task ended in {state}")
            wait_for_site_category(api, site_id, expected)
            return
        time.sleep(0.5)
    raise AssertionError("Site category restore task did not finish")


def cleanup_category(api: APIClient, category_id: str) -> None:
    category = category_by_id(api, category_id)
    if not category:
        return
    status, payload, _ = api.post("/api/v1/actions/sites_category_delete", {
        "id": category_id,
        "expected_revision": category["revision"],
    })
    if status != 200:
        error = payload.get("error", {}) if isinstance(payload, dict) else {}
        code = error.get("code", "unknown") if isinstance(error, dict) else "unknown"
        raise AssertionError(f"QA category cleanup returned HTTP {status}, code={code}")


def navigate_to_sites(page) -> None:
    page.get_by_role("navigation", name="主导航").get_by_role("button", name="网站", exact=True).click()
    expect(page.get_by_role("heading", name="网站资源", exact=True)).to_be_visible()


def find_site_row(page, site_name: str) -> None:
    page.get_by_placeholder("名称或域名").fill(site_name)
    expect(page.get_by_role("button", name=site_name, exact=True)).to_be_visible()


def set_category_filter(page, category_id: str) -> None:
    select = page.get_by_role("combobox", name=re.compile(r"^网站分类筛选(?:\s|$)"))
    if category_id:
        expect(select.locator(f'option[value="{category_id}"]')).to_have_count(1)
    select.select_option(category_id)


def update_site_category_in_ui(page, api: APIClient, site_id: str, site_name: str, category_id: str) -> dict[str, Any]:
    page.get_by_role("button", name=site_name, exact=True).click()
    drawer = page.get_by_role("dialog", name=f"网站 · {site_name}", exact=True)
    drawer.get_by_role("tab", name="配置", exact=True).click()
    category_select = drawer.get_by_role("combobox", name="网站分类", exact=True)
    if category_select.count() == 0:
        drawer.get_by_label("网站分类 · 可选", exact=True).check()
        category_select = drawer.get_by_role("combobox", name="网站分类", exact=True)
    expect(category_select).to_be_visible()
    category_select.select_option(category_id)
    form = drawer.locator(".action-form")
    form.get_by_role("button", name="执行操作", exact=True).click()
    form.get_by_role("button", name="确认执行", exact=True).click()
    task = form.get_by_role("region", name="任务详情", exact=True)
    expect(task.get_by_text("执行成功", exact=True)).to_be_visible(timeout=120_000)
    return wait_for_site_category(api, site_id, category_id)


def run_categories(page, api: APIClient, fixture: dict[str, Any], stage) -> dict[str, Any]:
    site_id = fixture["site_id"]
    site_name = fixture["site_name"]
    initial_site = site_by_id(api, site_id)
    if initial_site.get("name") != site_name:
        raise AssertionError("QA fixture site name does not match the live site")
    original_category = str(initial_site.get("category_id") or "")
    if original_category:
        raise AssertionError("Choose an unclassified QA site so category assignment can be restored safely")
    if initial_site.get("configuration_mode") == "advanced":
        raise AssertionError("Choose a managed site; category coverage must not touch advanced configuration")

    category_id = ""
    category_name = "QA分类-" + secrets.token_hex(6)
    edited_name = category_name + "-编辑"
    try:
        stage("category create via actual website UI")
        navigate_to_sites(page)
        find_site_row(page, site_name)
        page.get_by_role("button", name="管理分类", exact=True).click()
        manager = page.get_by_role("dialog", name="网站分类", exact=True)
        manager.get_by_role("button", name="新建分类", exact=True).click()
        editor = manager.locator('section[aria-label="分类编辑"]')
        category_id = editor.locator("code").inner_text().strip()
        if not re.fullmatch(r"cat-[0-9a-f-]{36}", category_id):
            raise AssertionError("UI did not generate a valid category ID")
        editor.get_by_label(re.compile(r"^分类名称(?:\s|$)")).fill(category_name)
        editor.get_by_role("button", name="创建分类", exact=True).click()
        created = next((item for item in categories(api) if item.get("id") == category_id), None)
        if not created or created.get("name") != category_name or created.get("revision") != 1:
            raise AssertionError("UI category creation did not match the actual API record")
        expect(manager.get_by_role("button", name=f"编辑 {category_name}", exact=True)).to_be_visible()

        stage("category edit preserves draft through real CAS conflict")
        manager.get_by_role("button", name=f"编辑 {category_name}", exact=True).click()
        editor = manager.locator('section[aria-label="分类编辑"]')
        editor.get_by_label(re.compile(r"^分类名称(?:\s|$)")).fill(edited_name)
        before_race = category_by_id(api, category_id)
        if not before_race:
            raise AssertionError("Created category disappeared before conflict test")
        raced = api_action(api, "sites_category_save", {
            "id": category_id,
            "name": category_name + "-并发版本",
            "expected_revision": before_race["revision"],
        })
        if not isinstance(raced, dict) or raced.get("revision") != before_race["revision"] + 1:
            raise AssertionError("Concurrent API write did not advance the category revision")
        with page.expect_response(
            lambda response: response.request.method == "POST"
            and response.url.endswith("/api/v1/actions/sites_category_save"),
            timeout=30_000,
        ) as conflict_info:
            editor.get_by_role("button", name="保存分类", exact=True).click()
        conflict_response = conflict_info.value
        conflict_payload = conflict_response.json()
        conflict_error = conflict_payload.get("error", {}) if isinstance(conflict_payload, dict) else {}
        if conflict_response.status != 412 or conflict_error.get("code") != "revision_conflict":
            raise AssertionError("UI stale save did not receive the real 412 revision_conflict response")
        expect(manager.get_by_role("alert")).to_contain_text("分类版本已变化")
        expect(editor.get_by_label(re.compile(r"^分类名称(?:\s|$)"))).to_have_value(edited_name)
        editor.get_by_role("button", name="载入最新版本并保留名称", exact=True).click()
        editor.get_by_role("button", name="保存分类", exact=True).click()
        updated = category_by_id(api, category_id)
        if not updated or updated.get("name") != edited_name or updated.get("revision") != raced["revision"] + 1:
            raise AssertionError("UI rebase did not save the retained draft against the current revision")

        stage("category filter, assignment, and UI clearing")
        manager.get_by_role("button", name="关闭资源详情", exact=True).click()
        set_category_filter(page, category_id)
        expect(page.get_by_role("button", name=site_name, exact=True)).to_have_count(0)
        set_category_filter(page, "")
        find_site_row(page, site_name)
        update_site_category_in_ui(page, api, site_id, site_name, category_id)
        page.get_by_role("dialog", name=f"网站 · {site_name}", exact=True).get_by_role("button", name="关闭资源详情", exact=True).click()
        set_category_filter(page, category_id)
        expect(page.get_by_role("button", name=site_name, exact=True)).to_be_visible()

        stage("delete-in-use is reported and category remains")
        page.get_by_role("button", name="管理分类", exact=True).click()
        manager = page.get_by_role("dialog", name="网站分类", exact=True)
        manager.get_by_role("button", name=f"删除 {edited_name}", exact=True).click()
        with page.expect_response(
            lambda response: response.request.method == "POST"
            and response.url.endswith("/api/v1/actions/sites_category_delete"),
            timeout=30_000,
        ) as delete_info:
            manager.locator('section[aria-label="确认删除分类"]').get_by_role("button", name="确认删除分类", exact=True).click()
        delete_response = delete_info.value
        delete_payload = delete_response.json()
        delete_error = delete_payload.get("error", {}) if isinstance(delete_payload, dict) else {}
        if delete_response.status != 409 or delete_error.get("code") != "category_in_use":
            raise AssertionError("UI delete-in-use did not receive the real 409 category_in_use response")
        expect(manager.get_by_role("alert")).to_contain_text("仍被网站使用")
        if not category_by_id(api, category_id):
            raise AssertionError("Rejected delete-in-use removed the category from the real API")
        manager.get_by_role("button", name="关闭资源详情", exact=True).click()

        stage("clearing site category and successful category delete")
        page.get_by_role("button", name=site_name, exact=True).click()
        drawer = page.get_by_role("dialog", name=f"网站 · {site_name}", exact=True)
        drawer.get_by_role("tab", name="配置", exact=True).click()
        drawer.get_by_role("combobox", name="网站分类", exact=True).select_option("")
        form = drawer.locator(".action-form")
        form.get_by_role("button", name="执行操作", exact=True).click()
        form.get_by_role("button", name="确认执行", exact=True).click()
        expect(form.get_by_role("region", name="任务详情", exact=True).get_by_text("执行成功", exact=True)).to_be_visible(timeout=120_000)
        wait_for_site_category(api, site_id, "")
        drawer.get_by_role("button", name="关闭资源详情", exact=True).click()
        set_category_filter(page, category_id)
        expect(page.get_by_role("button", name=site_name, exact=True)).to_have_count(0)
        set_category_filter(page, "")
        find_site_row(page, site_name)

        page.get_by_role("button", name="管理分类", exact=True).click()
        manager = page.get_by_role("dialog", name="网站分类", exact=True)
        manager.get_by_role("button", name=f"删除 {edited_name}", exact=True).click()
        manager.locator('section[aria-label="确认删除分类"]').get_by_role("button", name="确认删除分类", exact=True).click()
        if category_by_id(api, category_id):
            raise AssertionError("Successful category delete did not remove the API record")
        manager.get_by_role("button", name="关闭资源详情", exact=True).click()
        return {"category_ui": "created-edited-conflict-filter-clear-delete-in-use-delete-passed", "site_restored": True}
    finally:
        # Only restore the QA-selected site if it still carries this test's unique
        # category. Never overwrite an unrelated concurrent site change.
        if category_id:
            current_category = site_category(api, site_id)
            if current_category == category_id:
                restore_site_category(api, site_id, original_category)
            elif current_category != original_category:
                raise AssertionError("QA site changed to an unrelated category during test; manual review required")
            cleanup_category(api, category_id)


def run_system_change_preview(page, fixture: dict[str, Any], stage) -> dict[str, Any]:
    stage("system-change plan-only preflight")
    forbidden_mutations: list[str] = []
    page.on(
        "request",
        lambda request: forbidden_mutations.append(request.url)
        if re.search(r"/api/v1/actions/system_change_(apply|confirm|rollback)$", request.url)
        else None,
    )
    page.get_by_role("navigation", name="主导航").get_by_role("button", name="更多", exact=True).click()
    page.get_by_role("button", name=re.compile(r"^监控(?:\s|$)")).click()
    workflow = page.locator(".system-change-workflow")
    expect(workflow.get_by_role("heading", name="系统变更计划", exact=True)).to_be_visible()
    form = workflow.locator('section[aria-label="创建系统变更计划"] .action-form')
    expect(form).to_be_visible()
    form.get_by_role("combobox", name=re.compile(r"^类型(?:\s|$)")).select_option(label="服务状态")
    form.get_by_label("服务标识 · 可选", exact=True).check()
    form.get_by_label("服务标识", exact=True).fill(fixture["service_id"])
    form.get_by_label("服务操作 · 可选", exact=True).check()
    form.get_by_role("combobox", name=re.compile(r"^服务操作(?:\s|$)")).select_option(label="启动")
    form.get_by_role("button", name="执行操作", exact=True).click()
    with page.expect_response(
        lambda response: response.request.method == "POST"
        and response.url.endswith("/api/v1/actions/system_change_plan"),
        timeout=30_000,
    ) as response_info:
        form.get_by_role("button", name="确认执行", exact=True).click()
    response = response_info.value
    payload = response.json()
    # There is intentionally no click path below for apply/confirm/rollback.
    if forbidden_mutations:
        raise AssertionError("The UI sent an apply/confirm/rollback action during plan preview")
    if response.status != 200:
        error = payload.get("error", {}) if isinstance(payload, dict) else {}
        code = error.get("code", "unknown") if isinstance(error, dict) else "unknown"
        expect(form.get_by_role("alert")).to_be_visible()
        return {"status": "blocked_preflight", "http_status": response.status, "error_code": code, "apply_verified": False}
    plan = payload.get("data") if isinstance(payload, dict) else None
    if not isinstance(plan, dict) or plan.get("state") != "planned":
        raise AssertionError("Actual plan response did not contain a verified planned-state record")
    if plan.get("change", {}).get("service_id") != fixture["service_id"] or plan.get("change", {}).get("service_state") != "start":
        raise AssertionError("Actual preview did not reflect the QA-approved service input")
    current_plan = workflow.get_by_role("region", name="当前系统变更计划", exact=True)
    expect(current_plan.get_by_text("待应用", exact=True)).to_be_visible()
    expect(current_plan.get_by_role("button", name="应用计划", exact=True)).to_be_visible()
    return {"status": "planned_preview_only", "plan_state": plan["state"], "apply_verified": False}


def main() -> None:
    if os.environ.get("POWERLINUX_QA_EXCLUSIVE_WRITE_WINDOW") != "YES":
        raise RuntimeError("Set POWERLINUX_QA_EXCLUSIVE_WRITE_WINDOW=YES only during the QA-owned exclusive VM window")
    fixture_path = os.environ.get("POWERLINUX_QA_WORKFLOW_BROWSER_FILE")
    if not fixture_path:
        raise RuntimeError("POWERLINUX_QA_WORKFLOW_BROWSER_FILE must point to a 0600 QA fixture outside the checkout")
    fixture = read_private_fixture(fixture_path)
    api = APIClient(fixture["url"])
    login_status, login, _ = api.post("/api/v1/auth/login", {
        "username": fixture["username"],
        "password": fixture["password"],
    }, csrf=False)
    if login_status != 200 or not isinstance(login, dict) or not isinstance(login.get("data"), dict):
        raise RuntimeError(f"QA panel login failed with HTTP {login_status}")
    api.csrf = login["data"]["csrf_token"]
    health_status, health, _ = api.get("/healthz")
    if health_status != 200 or health.get("version") != fixture["version"]:
        raise RuntimeError("QA panel version does not match the private fixture")

    results: dict[str, Any] = {"version": health["version"]}
    errors: list[str] = []
    stage_name = "browser startup/login"
    def stage(value: str) -> None:
        nonlocal stage_name
        stage_name = value
        print("Browser stage: " + value, flush=True)

    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(
            executable_path=os.environ.get("POWERLINUX_CHROMIUM", "/usr/bin/chromium"),
            headless=True,
            args=["--no-sandbox"],
        )
        context = browser.new_context(
            viewport={"width": 1440, "height": 1000},
            locale="zh-CN",
            timezone_id="Asia/Singapore",
            reduced_motion="reduce",
        )
        page = context.new_page()
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.on(
            "response",
            lambda response: errors.append(f"Next asset HTTP {response.status}")
            if response.status >= 400 and "/_next/" in response.url
            else None,
        )
        try:
            page.goto(fixture["url"], wait_until="domcontentloaded")
            page.get_by_label(re.compile(r"^用户名(?:\s|$)")).fill(fixture["username"])
            page.get_by_label(re.compile(r"^密码(?:\s|$)")).fill(fixture["password"])
            page.get_by_role("button", name="登录", exact=True).click()
            expect(page.get_by_role("heading", name="主机概览", exact=True)).to_be_visible(timeout=30_000)
            results.update(run_categories(page, api, fixture, stage))
            system_result = run_system_change_preview(page, fixture, stage)
            results["system_change"] = system_result
            if system_result["status"] == "blocked_preflight":
                results["scope"] = "category browser flows passed; system-change plan preflight blocked; no apply/confirm/rollback attempted"
            else:
                results["scope"] = "category browser flows passed; system-change plan preview passed; apply/confirm/rollback unrun"
            if errors:
                raise AssertionError("Browser page or Next.js asset errors: " + "; ".join(errors))
            print(json.dumps(results, ensure_ascii=False))
            if system_result["status"] == "blocked_preflight":
                raise SystemExit(2)
        except BaseException:
            diagnostic = traceback.format_exc()
            redacted_errors = list(errors)
            for secret in (fixture["password"], fixture["username"], fixture["url"]):
                diagnostic = diagnostic.replace(secret, "<redacted QA fixture value>")
                redacted_errors = [error.replace(secret, "<redacted QA fixture value>") for error in redacted_errors]
            sys.stderr.write(json.dumps({"stage": stage_name, "failure": diagnostic, "page_errors": redacted_errors}, ensure_ascii=False) + "\n")
            raise
        finally:
            context.close()
            browser.close()


if __name__ == "__main__":
    main()
