#!/usr/bin/env python3
"""Capture the development UI gallery with explicitly marked read-only display fixtures."""
from __future__ import annotations

import json
import hashlib
import os
from pathlib import Path
import secrets
import sys
import time

from playwright.sync_api import expect, sync_playwright

sys.path.insert(0, str(Path(__file__).resolve().parent))
from fixture import RunningServer
from http_client import APIClient, data


OUT = Path(os.environ.get("POWERLINUX_QA_ARTIFACTS", str(Path(__file__).resolve().parents[2] / "docs/screenshots/ui-polish-current")))
OUT.mkdir(parents=True, exist_ok=True)
binary = os.environ["POWERLINUX_TEST_BINARY"]
server = RunningServer(binary)
api = APIClient(server.url)
user = "gallery-admin"
password = "Gallery-" + secrets.token_urlsafe(18)
data(api.post("/api/v1/auth/setup", {"username": user, "password": password,
                                     "setup_token": server.bootstrap}))

now = "2026-10-22T10:00:00Z"
actor = {"id": "gallery-admin", "name": "Folio Admin", "kind": "human"}
sites = [
    {"id": "site-main", "name": "Northstar Studio", "domains": ["northstar.example.com", "www.northstar.example.com"],
     "status": "running", "web_instance": "nginx-main", "default": True, "document_root": "/srv/www/northstar",
     "php_version": "8.3", "php_instance": "php83-main", "revision": 8, "configuration_mode": "managed",
     "tls": {"enabled": True, "expires_at": "2027-04-18T00:00:00Z", "issuer": "Let's Encrypt"}, "updated_at": now,
     "category_id": "production", "config": {"index": ["index.html", "index.php"], "directory_listing": False},
     "proxies": [{"id": "api", "path": "/api/", "upstream": "http://127.0.0.1:3000", "enabled": True, "websocket": True}]},
    {"id": "site-docs", "name": "Northstar Docs", "domains": ["docs.northstar.example.com"], "status": "running",
     "web_instance": "nginx-main", "document_root": "/srv/www/docs", "revision": 3, "configuration_mode": "managed", "updated_at": now,
     "tls": {"enabled": True, "expires_at": "2027-01-12T00:00:00Z", "issuer": "Let's Encrypt"}, "category_id": "production"},
    {"id": "site-preview", "name": "Campaign Preview", "domains": ["preview.northstar.example.com"], "status": "stopped",
     "web_instance": "nginx-main", "document_root": "/srv/www/preview", "revision": 1, "configuration_mode": "managed", "updated_at": now,
     "tls": {"enabled": False}, "category_id": "staging"},
]
instances = [
    {"id": "nginx-main", "plugin_id": "nginx", "instance_id": "main", "status": "running", "installed_version": "1.26.3", "health": {"status": "healthy"}, "updated_at": now},
    {"id": "php83-main", "plugin_id": "php", "instance_id": "8.3-main", "status": "running", "installed_version": "8.3.14", "health": {"status": "healthy"}, "updated_at": now},
    {"id": "mysql-main", "plugin_id": "mysql", "instance_id": "primary", "status": "running", "installed_version": "8.4.3", "health": {"status": "healthy"}, "updated_at": now},
]
apps = [
    {"id": "billing-api", "name": "Billing API", "runtime": "nodejs", "status": "running", "running": True,
     "user": "billing", "upstream": "http://127.0.0.1:3010", "port": 3010, "revision": 4, "updated_at": now},
    {"id": "customer-portal", "name": "Customer Portal", "runtime": "nextjs", "status": "running", "running": True,
     "user": "portal", "upstream": "http://127.0.0.1:3020", "port": 3020, "revision": 2, "updated_at": now},
    {"id": "preview-worker", "name": "Preview Worker", "runtime": "go", "status": "stopped", "running": False,
     "user": "preview", "upstream": "http://127.0.0.1:3030", "port": 3030, "revision": 1, "updated_at": now},
]
dbs = [
    {"id": "db-primary", "name": "Production database", "engine": "postgresql", "host": "db.internal", "port": 5432,
     "database": "northstar", "access": "database", "status": "connected", "revision": 4, "updated_at": now},
    {"id": "db-analytics", "name": "Analytics MySQL", "engine": "mysql", "host": "analytics.internal", "port": 3306,
     "database": "reports", "access": "database", "status": "connected", "revision": 2, "updated_at": now},
]
caches = [
    {"id": "cache-primary", "name": "Sessions", "engine": "redis", "host": "cache.internal", "port": 6379,
     "database": 0, "access": "database", "status": "connected", "revision": 3, "updated_at": now},
    {"id": "cache-jobs", "name": "Background jobs", "engine": "valkey", "host": "jobs.internal", "port": 6380,
     "database": 2, "access": "database", "status": "connected", "revision": 1, "updated_at": now},
]
file_rows = [
    {"name": "README.md", "path": "README.md", "type": "file", "size": 2480, "mode": "-rw-r--r--", "owner": "folio", "group": "folio", "modified_at": now},
    {"name": "config", "path": "config", "type": "directory", "size": 0, "mode": "drwxr-xr-x", "owner": "folio", "group": "folio", "modified_at": now},
    {"name": "deploy.sh", "path": "deploy.sh", "type": "file", "size": 1872, "mode": "-rwxr-xr-x", "owner": "folio", "group": "folio", "modified_at": now},
    {"name": "logs", "path": "logs", "type": "directory", "size": 0, "mode": "drwxr-x---", "owner": "folio", "group": "folio", "modified_at": now},
    {"name": "nginx.conf", "path": "nginx.conf", "type": "file", "size": 952, "mode": "-rw-r--r--", "owner": "folio", "group": "folio", "modified_at": now},
    {"name": "public", "path": "public", "type": "directory", "size": 0, "mode": "drwxr-xr-x", "owner": "folio", "group": "folio", "modified_at": now},
]
samples = [
    {"timestamp": f"2026-10-22T10:{i:02d}:00Z", "cpu_percent": 18 + i % 11,
     "memory": {"percent": 47 + i % 4}, "disk": {"percent": 38}} for i in range(30)
]
fixture_actions = {
    "sites_categories_list": {"items": [{"id": "production", "name": "Production"}, {"id": "staging", "name": "Staging"}], "total": 2},
    "notes_list": [
        {"id": "note-release", "title": "Release checklist", "content": "Confirm the maintenance window before publishing.\n\nBackup verified; service owner notified.", "pinned": True, "archived": False, "target_id": "host:local", "revision": 3, "created_at": "2026-10-21T10:00:00Z", "updated_at": "2026-10-22T10:00:00Z", "created_by": actor, "updated_by": actor},
        {"id": "note-handoff", "title": "Operations handoff", "content": "Northstar Docs is served from /srv/www/docs.\n\nContact the platform team before changing DNS.", "pinned": False, "archived": False, "target_id": "site:site-docs", "revision": 2, "created_at": "2026-10-20T10:00:00Z", "updated_at": "2026-10-22T09:30:00Z", "created_by": actor, "updated_by": actor},
        {"id": "note-maintenance", "title": "Maintenance window", "content": "Friday · 22:00–23:00 UTC\n\nNotify the team before restarting services.", "pinned": False, "archived": False, "target_id": "host:local", "revision": 1, "created_at": "2026-10-19T10:00:00Z", "updated_at": "2026-10-22T09:00:00Z", "created_by": actor, "updated_by": actor},
    ],
    "settings_panel_get": {"panel_name": "Folio · 折纸面板", "server_name": "folio-demo", "footer": "AI 时代的轻量化面板", "login_entry": "", "captcha_enabled": False, "api_enabled": True, "maintenance": False, "basic_auth_enabled": False, "unauthorized_status": 404, "revision": 2},
    "certificates_list": {"items": [
        {"id": "cert-northstar", "name": "northstar.example.com", "domains": ["northstar.example.com", "www.northstar.example.com"], "issuer": "Let's Encrypt", "status": "active", "expires_at": "2027-04-18T00:00:00Z", "revision": 2},
        {"id": "cert-docs", "name": "docs.northstar.example.com", "domains": ["docs.northstar.example.com"], "issuer": "Let's Encrypt", "status": "active", "expires_at": "2027-01-12T00:00:00Z", "revision": 1}], "total": 2},
    "plugins_list": {"items": instances, "total": len(instances)},
    "database_connections_list": {"items": dbs, "total": len(dbs)},
    "cache_connections_list": {"items": caches, "total": len(caches)},
    "hosted_apps_list": {"items": apps, "total": len(apps)},
    "browser_apps_list": {"items": [{"id": "panel", "name": "Panel browser", "installed": True, "available": True, "revision": 3, "source": "system"}, {"id": "firefox-wasm", "name": "Firefox WASM", "installed": True, "available": True, "revision": 2, "version": "0.0.1"}], "total": 2},
    "fs_roots": [{"id": "workspace", "label": "Workspace", "path": "workspace", "read_only": False}],
    "fs_list": {"root": "workspace", "path": "", "items": file_rows, "total": len(file_rows), "limit": 72, "offset": 0},
    "fs_read": {"content": "# Northstar workspace\n\nThis workspace contains the website configuration and deployment notes.\n\n## Services\n- Nginx: edge routing\n- Node.js: billing API\n- PostgreSQL: production data\n", "encoding": "utf-8", "sha256": "1" * 64, "size": 178},
    "fs_trash_list": {"items": [{"id":"trash-banner","root":"workspace","name": "old-banner.svg", "path": "old-banner.svg", "deleted_at": now, "size": 4830,"directory":False,"state":"trashed"}], "total": 1},
    "services_list": {"items": [
        {"id": "nginx.service", "name": "Nginx web server", "unit": "nginx.service", "active_state": "active", "ownership": "managed"},
        {"id": "powerlinux.service", "name": "Folio panel", "unit": "powerlinux.service", "active_state": "active", "ownership": "managed"},
        {"id": "postgresql.service", "name": "PostgreSQL", "unit": "postgresql.service", "active_state": "active", "ownership": "external"}], "total": 3},
    "monitor_history": {"samples": samples, "sample_interval_seconds": 60},
    "schedules_list": {"items": [
        {"id": "nightly-backup", "name": "Nightly database backup", "enabled": True, "mode": "cron", "cron": "0 2 * * *", "timezone": "UTC", "next_run_at": "2026-10-23T02:00:00Z", "last_run_at": "2026-10-22T02:00:00Z", "last_run_status": "succeeded", "revision": 4},
        {"id": "weekly-report", "name": "Weekly traffic report", "enabled": True, "mode": "cron", "cron": "0 8 * * 1", "timezone": "UTC", "next_run_at": "2026-10-26T08:00:00Z", "last_run_at": "2026-10-19T08:00:00Z", "last_run_status": "succeeded", "revision": 1}], "total": 2},
    "notifications_channels_list": {"items": [
        {"id": "ops-email", "name": "Operations email", "kind": "smtp", "enabled": True, "revision": 2},
        {"id": "team-webhook", "name": "Team Telegram", "kind": "telegram", "enabled": True, "revision": 1}], "total": 2},
    "terminal_targets_list": [{"id": "local", "name": "Managed host", "type": "local", "host": "localhost", "user": "folio"}],
    "terminal_commands_list": [{"id": "disk", "title": "Disk usage", "command": "df -h"}, {"id": "services", "title": "Service status", "command": "systemctl --no-pager --type=service"}],
}


def page_result(rows, request):
    body = request.post_data_json or {}
    query = str(body.get("query", "")).lower()
    filtered = [row for row in rows if query in " ".join([str(row.get("name", "")), *row.get("domains", [])]).lower()]
    if body.get("category_id"):
        filtered = [row for row in filtered if row.get("category_id") == body["category_id"]]
    sort_key=body.get("sort","name")
    filtered.sort(key=lambda row: (row.get(sort_key,0 if sort_key=="size" else ""),row.get("name","")),reverse=body.get("order")=="desc")
    offset, limit = int(body.get("offset", 0) or 0), int(body.get("limit", 100) or 100)
    return {"items": filtered[offset:offset + limit], "total": len(filtered), "limit": limit, "offset": offset}


def action_fixture(route):
    action = route.request.url.rsplit("/", 1)[-1]
    body = route.request.post_data_json or {}
    if action == "sites_list":
        result = page_result(sites, route.request)
        all_matching=page_result(sites,route.request); query=str(body.get("query", "")).lower(); matching=[site for site in sites if query in " ".join([site["name"],*site["domains"]]).lower() and (not body.get("category_id") or site.get("category_id")==body["category_id"])]; result["status_counts"]={status:sum(site["status"]==status for site in matching) for status in ("running","stopped")}; result["status_counts"]["unknown"]=sum(site["status"] not in ("running","stopped") for site in matching)
    elif action == "plugins_list":
        result = {"items": instances, "total": len(instances), "availability": "available"}
    elif action in ("notes_list", "settings_panel_get"):
        result = fixture_actions[action]
    elif action == "plugins_get":
        result = next(item for item in instances if item["id"] == body.get("id"))
    elif action == "database_connection_get":
        result = next(item for item in dbs if item["id"] == body.get("id"))
    elif action == "cache_connection_get":
        result = next(item for item in caches if item["id"] == body.get("id"))
    elif action == "fs_list":
        result = {**page_result(file_rows,route.request),"root":body.get("root","workspace"),"path":body.get("path", "")}
    elif action == "sites_get":
        result = next((site for site in sites if site["id"] == body.get("id")), sites[0])
    elif action == "sites_backups_list":
        result = {"items": [{"id": "backup-20261022", "created_at": now}, {"id": "backup-20261021", "created_at": "2026-10-21T10:00:00Z"}], "total": 2}
    elif action == "sites_retained_list":
        result = {"items": [{"id": "retained-1", "name": "Campaign microsite", "domains": ["spring.northstar.example.com"], "retained_at": now}], "total": 1}
    elif action in ("services_list", "monitor_history", "schedules_list", "notifications_channels_list", "terminal_targets_list", "terminal_commands_list", "fs_roots", "fs_list", "fs_read", "fs_trash_list", "database_connections_list", "cache_connections_list", "hosted_apps_list", "browser_apps_list"):
        result = fixture_actions[action]
    elif action == "schedules_history":
        result = [{"id": "run-20261022", "status": "succeeded", "started_at": now, "finished_at": now, "task_id": "task-backup"}]
    else:
        return route.continue_()
    route.fulfill(status=200, content_type="application/json", body=json.dumps({"data": result}, ensure_ascii=False))


def api_fixture(route):
    from urllib.parse import urlsplit
    path = urlsplit(route.request.url).path
    if path == "/api/v1/system/status":
        result = {"hostname": "folio-demo", "os": "Ubuntu 24.04 LTS", "kernel": "6.8.0", "architecture": "amd64",
                  "cpu_percent": 23.4, "cpu_cores": 8, "uptime_seconds": 186324,
                  "memory": {"total_bytes": 17179869184, "used_bytes": 7730941132, "available_bytes": 9448928052},
                  "disk": {"total_bytes": 536870912000, "used_bytes": 204010946560}, "available": True, "sampled_at": now}
    elif path == "/api/v1/notes":
        result = [
            {"id": "note-release", "title": "Release checklist", "content": "Confirm the maintenance window before publishing.\n\nBackup verified; service owner notified.", "pinned": True, "archived": False, "target_id": "host:local", "revision": 3, "created_at": now, "updated_at": now, "created_by": actor, "updated_by": actor},
            {"id": "note-handoff", "title": "Operations handoff", "content": "Northstar Docs is served from /srv/www/docs.\n\nContact the platform team before changing DNS.", "pinned": False, "archived": False, "target_id": "site:site-docs", "revision": 1, "created_at": now, "updated_at": now, "created_by": actor, "updated_by": actor},
        ]
        from urllib.parse import parse_qs
        archived=parse_qs(urlsplit(route.request.url).query).get("archived",["false"])[0]=="true"
        result=[note for note in result if note["archived"]==archived]
    elif path == "/api/v1/tasks":
        result = [
            {"id": "task-backup", "name": "Nightly database backup", "action": "database_backup", "target": "db-primary", "status": "succeeded", "progress": 100, "created_at": now, "updated_at": now, "actor": actor},
            {"id": "task-certificate", "name": "Renew site certificate", "action": "certificate_renew", "target": "site-main", "status": "running", "progress": 72, "created_at": now, "updated_at": now, "actor": actor},
        ]
    elif path == "/api/v1/audit":
        result = [
            {"id": "audit-1", "action": "sites_update", "actor": actor, "created_at": now, "status": "succeeded", "success": True, "summary": "Updated Northstar Studio configuration", "target": "site:site-main"},
            {"id": "audit-2", "action": "database_backup", "actor": actor, "created_at": now, "status": "succeeded", "success": True, "summary": "Created scheduled database backup", "target": "db:db-primary"},
        ]
    else:
        return route.continue_()
    route.fulfill(status=200, content_type="application/json", body=json.dumps({"data": result}, ensure_ascii=False))


def main():
    errors = []
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(executable_path=os.environ.get("POWERLINUX_CHROMIUM", "/usr/bin/chromium"), headless=True, args=["--no-sandbox"])
            context = browser.new_context(viewport={"width": 1440, "height": 960}, locale="zh-CN", timezone_id="Asia/Singapore", reduced_motion="reduce")
            context.add_init_script("localStorage.setItem('folio.language','zh-CN')")
            page = context.new_page()
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.route("**/api/v1/actions/**", action_fixture)
            page.route("**/api/v1/system/status", api_fixture)
            page.route("**/api/v1/notes**", api_fixture)
            page.route("**/api/v1/tasks**", api_fixture)
            page.route("**/api/v1/audit**", api_fixture)
            page.goto(server.url)
            expect(page.get_by_role("heading", name="欢迎回来", exact=True)).to_be_visible()
            page.screenshot(path=str(OUT / "00-login.png"), full_page=True)
            page.locator("input[name=username]").fill(user)
            page.locator("input[name=password]").fill(password)
            page.get_by_role("button", name="登录", exact=True).click()
            expect(page.locator(".status-overview")).to_be_visible(timeout=15000)
            expect(page.get_by_role("heading", name="主机概览", exact=True)).to_be_visible()

            def capture(slug: str, hash_route: str, section: str, full_page: bool = False):
                page.evaluate("(hash) => { location.hash = hash; window.scrollTo(0,0); }", hash_route)
                expect(page.locator(".app")).to_have_attribute("data-section", section, timeout=10000)
                page.wait_for_timeout(550)
                page.evaluate("document.fonts.ready")
                page.evaluate("document.activeElement?.blur()")
                page.mouse.move(0, 0)
                assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), (slug, "horizontal overflow")
                page.screenshot(path=str(OUT / f"{slug}.png"), full_page=full_page)

            capture("01-overview", "#/overview", "overview")
            capture("02-sites", "#/sites", "sites")
            capture("03-site-detail", "#/sites?id=site-main&group=overview", "sites")
            capture("04-site-tls", "#/sites?id=site-main&group=tls", "sites")
            capture("05-site-backups", "#/sites?id=site-main&group=backup", "sites")
            capture("06-sites-certificates", "#/sites?page=certificates", "sites")
            capture("07-site-create", "#/sites?create=true", "sites")
            capture("08-software-installed", "#/software?page=installed", "software")
            capture("09-software-library", "#/software?page=catalog", "software")
            capture("10-database-connections", "#/software?page=database", "software")
            capture("11-cache-connections", "#/software?page=cache", "software")
            capture("12-deployed-apps", "#/software?page=applications&subgroup=hosted", "software")
            capture("13-browser-apps", "#/software?page=applications&subgroup=browsers", "software")
            capture("14-files", "#/files?root=workspace", "files")
            capture("15-file-editor", "#/files?root=workspace&file=README.md&view=editor", "files")
            capture("16-file-upload", "#/files?root=workspace&new=true", "files")
            capture("17-file-trash", "#/files?root=workspace&filter=true", "files")
            capture("18-more-menu", "#/more", "more")
            capture("19-monitor", "#/more?page=monitor", "more")
            capture("20-terminal", "#/more?page=terminal", "more")
            capture("21-schedules", "#/more?page=schedules", "more")
            page.get_by_role("tab", name="通知渠道", exact=True).click()
            page.wait_for_timeout(350)
            page.screenshot(path=str(OUT / "22-notifications.png"), full_page=False)
            capture("23-tasks", "#/more?page=activity", "more")
            capture("24-shared-notes", "#/more?page=notes", "more")
            capture("25-security", "#/more?page=security", "more")
            capture("26-appearance", "#/more?page=appearance", "more")
            capture("27-mcp", "#/more?page=mcp", "more")
            capture("28-audit", "#/more?page=audit", "more")
            capture("29-advanced-actions", "#/more?page=actions", "more", full_page=False)
            page.keyboard.press("Control+k")
            expect(page.get_by_role("dialog", name="全局搜索", exact=True)).to_be_visible()
            page.wait_for_timeout(250)
            page.screenshot(path=str(OUT / "30-global-search.png"), full_page=False)
            page.keyboard.press("Escape")
            capture("31-environment", "#/more?page=environment", "more")
            capture("32-monitor-alerts", "#/more?page=monitor&view=alerts", "more")
            capture("33-database-detail", "#/software?page=database&id=db-primary", "software")
            capture("34-cache-detail", "#/software?page=cache&id=cache-primary", "software")
            capture("35-software-detail", "#/software?page=installed&id=nginx-main&group=operations", "software")
            capture("36-application-detail", "#/software?page=applications&subgroup=hosted&id=billing-api&group=overview", "software")
            capture("37-panel-settings", "#/more?page=panel", "more")
            page.evaluate("(hash) => { location.hash = hash; window.scrollTo(0,0); }", "#/overview")
            expect(page.locator(".overview-note")).to_have_count(3, timeout=10000)
            page.locator(".overview-note").first.click()
            expect(page.get_by_role("dialog", name="便签详情", exact=True)).to_be_visible()
            page.wait_for_timeout(350)
            page.screenshot(path=str(OUT / "38-overview-note-editor.png"), full_page=False)
            page.get_by_role("button", name="关闭便签", exact=True).click()
            expect(page.get_by_role("dialog", name="便签详情", exact=True)).to_have_count(0)

            for width, height in ((390, 844),):
                page.set_viewport_size({"width": width, "height": height})
                for slug, route, section in (("31-environment", "#/more?page=environment", "more"), ("32-monitor-alerts", "#/more?page=monitor&view=alerts", "more"), ("01-overview", "#/overview", "overview"), ("02-sites", "#/sites", "sites"), ("08-software-installed", "#/software?page=installed", "software"), ("14-files", "#/files?root=workspace", "files"), ("18-more-menu", "#/more", "more"), ("26-appearance", "#/more?page=appearance", "more"), ("03-site-detail", "#/sites?id=site-main&group=overview", "sites"), ("09-software-library", "#/software?page=catalog", "software"), ("13-browser-apps", "#/software?page=browsers", "software"), ("21-schedules", "#/more?page=schedules", "more"), ("25-security", "#/more?page=security", "more"), ("27-mcp", "#/more?page=mcp", "more")):
                    capture(f"mobile-{slug}", route, section)
                    if slug == "14-files":
                        for name in page.locator(".compact-file-table .file-entry-name").all():
                            box = name.bounding_box()
                            styles = name.evaluate("el => ({whiteSpace:getComputedStyle(el).whiteSpace, overflow:getComputedStyle(el).overflow, textOverflow:getComputedStyle(el).textOverflow})")
                            assert box and box["height"] <= 22 and styles == {"whiteSpace": "nowrap", "overflow": "hidden", "textOverflow": "ellipsis"}, (box, styles)
            mobile_shots = sorted(OUT.glob("mobile-*.png"))
            mobile_hashes = {hashlib.sha256(path.read_bytes()).hexdigest() for path in mobile_shots}
            assert len(mobile_shots) == 14 and len(mobile_hashes) == 14, "mobile captures are missing or duplicate"
            assert not errors, errors
            browser.close()
    finally:
        server.close()
    print(json.dumps({"screenshots": len(list(OUT.glob("*.png"))), "output": str(OUT), "errors": errors}, ensure_ascii=False))


if __name__ == "__main__":
    main()
