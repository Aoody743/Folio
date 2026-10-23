#!/usr/bin/env python3
"""Real server-browser regression: sandboxed Chromium, authenticated API and panel UI.

A local HTTP fixture records requests received by the remote Chromium. The panel's
client browser explicitly blocks direct requests to that fixture, proving that
navigation and typing use the server browser rather than an iframe/local proxy.
No remote website, browser engine, action response or screenshot is mocked.
"""
from __future__ import annotations

import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import sqlite3
import threading
import time
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import sync_playwright, expect
from fixture import RunningServer
from http_client import APIClient, data


def eventually(check, message, timeout=15):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(.1)
    raise AssertionError(message)


class Website:
    def __init__(self):
        self.requests = []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                parsed = urlsplit(self.path)
                owner.requests.append({"path": parsed.path, "query": parse_qs(parsed.query),
                                       "peer": self.client_address[0], "ua": self.headers.get("User-Agent", "")})
                if parsed.path in ("/ready", "/entered", "/scrolled", "/dialog-answer"):
                    self.send_response(204)
                    self.end_headers()
                    return
                if parsed.path == "/unsafe-redirect":
                    self.send_response(302)
                    self.send_header("Location", "file:///etc/passwd")
                    self.end_headers()
                    return
                if parsed.path == "/download":
                    self.send_response(200)
                    self.send_header("Content-Type", "application/octet-stream")
                    self.send_header("Content-Disposition", 'attachment; filename="browser-qa-download.txt"')
                    self.end_headers()
                    self.wfile.write(b"Downloads must be denied by the remote browser.")
                    return
                second = parsed.path == "/second"
                title = "Server browser second page" if second else "Server browser controlled page"
                html = f'''<!doctype html><html><head><meta charset="utf-8"><title>{title}</title>
<style>body{{margin:0;background:#f2f5fa;font:20px sans-serif;color:#152238}}header{{padding:16px;height:28px;background:#23395d;color:white}}main{{padding:20px}}input{{position:absolute;left:20px;top:80px;width:420px;height:38px;font:20px sans-serif}}button{{position:absolute;left:20px;top:140px;width:180px;height:42px}}a.next{{position:absolute;left:20px;top:205px}}a.popup{{position:absolute;left:20px;top:250px}}a.download{{position:absolute;left:20px;top:295px}}.bottom{{position:absolute;top:1600px;padding:20px}}</style>
</head><body><header>{title}</header><main>Rendered by the server browser.
<input aria-label="Remote text" id="remote-text" autocomplete="off"><button onclick="fetch('/entered?value='+encodeURIComponent(document.getElementById('remote-text').value))">Submit text</button>
<a class="next" href="/second">Open second page</a><a class="popup" href="/popup" target="_blank">Open popup tab</a><a class="download" href="/download" download>Blocked download</a><div class="bottom">Bottom of remote page</div></main>
<button style="top:350px" onclick="const answer=prompt('网页需要回复','默认');fetch('/dialog-answer?value='+encodeURIComponent(answer))">Prompt page</button>
<script>fetch('/ready?width='+innerWidth+'&path='+encodeURIComponent(location.pathname));let sent=false;addEventListener('scroll',()=>{{if(!sent&&scrollY>100){{sent=true;fetch('/scrolled?y='+Math.round(scrollY));}}}});</script></body></html>'''.encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(html)))
                self.end_headers()
                self.wfile.write(html)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def saw(self, pathname, **query):
        return any(row["path"] == pathname and all(row["query"].get(key) == [str(value)] for key, value in query.items()) for row in self.requests)

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)


def descendants(pid):
    processes = {}
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            stat = (entry / "stat").read_text().split(") ", 1)[1].split()
            processes[int(entry.name)] = {"ppid": int(stat[1]), "state": stat[0],
                                         "args": (entry / "cmdline").read_bytes().decode(errors="replace").split("\0")}
        except (OSError, ValueError, IndexError):
            pass
    result = {}
    pending = [pid]
    while pending:
        parent = pending.pop()
        for child, info in processes.items():
            if info["ppid"] == parent and child not in result:
                result[child] = info
                pending.append(child)
    return result


def alive(pid):
    try:
        return Path(f"/proc/{pid}/stat").read_text().split(") ", 1)[1].split()[0] != "Z"
    except (OSError, IndexError):
        return False


def main():
    binary = os.environ["POWERLINUX_TEST_BINARY"]
    artifacts = Path(os.environ.get("POWERLINUX_QA_ARTIFACTS", "/tmp/folio-server-browser-qa"))
    artifacts.mkdir(parents=True, exist_ok=True)
    if os.geteuid() == 0:
        raise RuntimeError("Run this regression as an unprivileged user; production Chromium sandbox must remain enabled")
    website = Website()
    server = RunningServer(binary)
    api = APIClient(server.url)
    password = secrets.token_urlsafe(32)
    errors = []
    try:
        data(api.post("/api/v1/auth/setup", {"username": "remote-browser-qa", "password": password, "setup_token": server.bootstrap}))
        login = data(api.post("/api/v1/auth/login", {"username": "remote-browser-qa", "password": password}))
        api.csrf = login["csrf_token"]
        status = data(api.post("/api/v1/actions/browser_status", {}))
        assert status["available"], status
        assert status["sessions"] == [], status

        def call(name, body, client=api):
            headers = {"Idempotency-Key": secrets.token_urlsafe(20)} if name in ("browser_input", "browser_close") else {}
            return data(client.post("/api/v1/actions/" + name, body, headers=headers))

        browser_session = call("browser_open", {"width": 1024, "height": 768})
        sid = browser_session["id"]
        tid = browser_session["tabs"][0]["id"]
        base = {"id": sid, "tab_id": tid}
        frame_url = f"/api/v1/browser/sessions/{sid}/tabs/{tid}/frame"
        remote = eventually(lambda: {pid: info for pid, info in descendants(server.process.pid).items() if any("remote-debugging-pipe" in arg for arg in info["args"])}, "No private-pipe Chromium process was launched")
        remote_pid, remote_info = next(iter(remote.items()))
        (artifacts / "sandbox-process.json").write_text(json.dumps({"pid": remote_pid, "argv": remote_info["args"], "unprivileged_uid": os.geteuid(), "pipe_fds": sorted(item.name for item in Path(f"/proc/{remote_pid}/fd").iterdir())}, ensure_ascii=False, indent=2) + "\n")
        assert not any("--no-sandbox" in arg or "--remote-debugging-port" in arg or "--ignore-certificate-errors" in arg for info in descendants(server.process.pid).values() for arg in info["args"])
        profiles = [Path(arg.split("=", 1)[1]) for arg in remote_info["args"] if arg.startswith("--user-data-dir=")]
        assert profiles and all(path.exists() for path in profiles)
        assert APIClient(server.url).get(frame_url)[0] == 401
        assert api.post("/api/v1/actions/browser_navigate", {**base, "url": website.url}, csrf=False)[0] == 403
        assert api.get(frame_url + "?ignored=1")[0] in (200, 400)
        assert "no-store" in api.get(frame_url)[2].get("Cache-Control", "")
        for unsafe in ("file:///etc/passwd", "javascript:alert(1)", "data:text/html,hello", "chrome://settings", "devtools://devtools", "https://user:secret@example.com/"):
            code, _, _ = api.post("/api/v1/actions/browser_navigate", {**base, "url": unsafe})
            assert code in (400, 403), (unsafe, code)
        before = len(website.requests)
        call("browser_navigate", {**base, "url": website.url + "/first"})
        eventually(lambda: website.saw("/ready", width=1024, path="/first"), "Server Chromium did not execute the controlled webpage")
        requests = website.requests[before:]
        assert any(row["path"] == "/first" and row["peer"] == "127.0.0.1" and "Chrome/" in row["ua"] for row in requests), requests
        code, first_frame, headers = api.get(frame_url)
        assert code == 200 and isinstance(first_frame, bytes) and first_frame.startswith(b"\xff\xd8") and headers.get("Content-Type", "").startswith("image/jpeg")
        (artifacts / "remote-first.jpg").write_bytes(first_frame)

        def click(x, y):
            call("browser_input", {**base, "kind": "mouse", "event": "mousePressed", "button": "left", "x": x, "y": y})
            call("browser_input", {**base, "kind": "mouse", "event": "mouseReleased", "button": "left", "x": x, "y": y})

        click(100, 100)
        private_text = "QA-private-text-中文-" + secrets.token_hex(5)
        call("browser_input", {**base, "kind": "text", "text": private_text + "x"})
        call("browser_input", {**base, "kind": "key", "event": "keyDown", "key": "Backspace", "code": "Backspace"})
        call("browser_input", {**base, "kind": "key", "event": "keyUp", "key": "Backspace", "code": "Backspace"})
        click(90, 160)
        eventually(lambda: website.saw("/entered", value=private_text), "Unicode text was not entered in the real remote page")
        call("browser_input", {**base, "kind": "wheel", "x": 400, "y": 400, "delta_y": 500, "delta_x": 0})
        eventually(lambda: website.saw("/scrolled"), "Remote wheel input did not scroll")
        assert hashlib.sha256(api.get(frame_url)[1]).digest() != hashlib.sha256(first_frame).digest()
        call("browser_navigate", {**base, "url": website.url + "/second"})
        eventually(lambda: website.saw("/ready", path="/second", width=1024), "Second page did not load")
        call("browser_history", {**base, "direction": "back"})
        eventually(lambda: next((tab for tab in call("browser_tabs", {"id": sid})["tabs"] if tab["id"] == tid and "/first" in tab["url"]), None), "Back navigation failed")
        call("browser_history", {**base, "direction": "forward"})
        eventually(lambda: next((tab for tab in call("browser_tabs", {"id": sid})["tabs"] if tab["id"] == tid and "/second" in tab["url"]), None), "Forward navigation failed")
        resized = call("browser_resize", {**base, "width": 800, "height": 600})
        assert resized["width"] == 800 and resized["height"] == 600
        opened = call("browser_tab_open", {"id": sid, "url": website.url + "/popup"})
        assert len(opened["tabs"]) == 2
        second_tab = next(tab["id"] for tab in opened["tabs"] if tab["id"] != tid)
        closed = call("browser_tab_close", {"id": sid, "tab_id": second_tab})
        assert len(closed["tabs"]) == 1
        call("browser_navigate", {**base, "url": website.url + "/first"})
        eventually(lambda: website.saw("/ready", path="/first", width=800), "First page did not finish loading before popup click")
        popup_ready_before = sum(row["path"] == "/ready" and row["query"].get("path") == ["/popup"] for row in website.requests)
        click(65, 260)
        eventually(lambda: sum(row["path"] == "/ready" and row["query"].get("path") == ["/popup"] for row in website.requests) > popup_ready_before, "New-window link did not finish loading through the server browser")
        eventually(lambda: any(tab["id"] == tid and "/popup" in tab["url"] for tab in call("browser_tabs", {"id": sid})["tabs"]), "New-window destination did not stay in the controlled tab")
        click(65, 370)
        dialog_session = eventually(lambda: (value if any(tab.get("dialog") for tab in value["tabs"]) else None) if (value := call("browser_tabs", {"id": sid})) else None, "Website prompt was not exposed")
        assert next(tab for tab in dialog_session["tabs"] if tab["id"] == tid)["dialog"]["type"] == "prompt"
        assert api.get(frame_url)[0] == 200, "An open website dialog blocked frame transport"
        call("browser_dialog", {**base, "accept": True, "prompt_text": "真实中文回复"})
        eventually(lambda: website.saw("/dialog-answer", value="真实中文回复"), "Prompt reply was not delivered")
        audit = data(api.post("/api/v1/actions/audit_list", {"limit": 100}))
        assert private_text not in json.dumps(audit, ensure_ascii=False), "Typed browser text leaked into audit records"
        assert website.url not in json.dumps(audit, ensure_ascii=False), "Visited browser URLs leaked into audit records"
        with sqlite3.connect(f"file:{server.data_dir / 'powerlinux.db'}?mode=ro", uri=True) as stored:
            cached = json.dumps(stored.execute("SELECT result,error FROM idempotency WHERE action LIKE 'browser_%'").fetchall(), ensure_ascii=False)
            assert private_text not in cached and website.url not in cached, "Normal browser controls persisted input or visited addresses"
        call("browser_close", {"id": sid})
        eventually(lambda: not alive(remote_pid) and all(not path.exists() for path in profiles), "Closing the session retained a Chromium process or private profile")
        assert api.get(frame_url)[0] in (403, 404, 410)

        # Bearer owners are separate identities; revocation is checked on every frame.
        def token(name, scopes, resources):
            value = data(api.post("/api/v1/tokens", {"name": name, "kind": "api", "scopes": scopes, "resources": resources}))
            return value, APIClient(server.url, value["token"])

        token_a, actor_a = token("remote browser owner A", ["browser:use"], ["browser:server"])
        token_b, actor_b = token("remote browser owner B", ["browser:use"], ["browser:server"])
        _, readonly = token("remote browser no permission", ["system:read"], ["*"])
        _, wrong_resource = token("remote browser wrong resource", ["browser:use"], ["browser:other"])
        assert readonly.post("/api/v1/actions/browser_open", {})[0] == 403
        assert wrong_resource.post("/api/v1/actions/browser_open", {})[0] == 403
        owned = call("browser_open", {}, actor_a)
        owner_frame = f'/api/v1/browser/sessions/{owned["id"]}/tabs/{owned["tabs"][0]["id"]}/frame'
        assert actor_b.get(owner_frame)[0] == 403
        assert call("browser_status", {}, actor_b)["sessions"] == []
        assert actor_a.get(owner_frame)[0] == 200
        data(api.delete("/api/v1/tokens/" + token_a["id"]))
        assert actor_a.get(owner_frame)[0] == 401
        owned_processes = descendants(server.process.pid)
        owned_profiles = [Path(arg.split("=", 1)[1]) for info in owned_processes.values() for arg in info["args"] if arg.startswith("--user-data-dir=")]
        server.restart()
        eventually(lambda: not any(alive(pid) for pid in owned_processes) and all(not path.exists() for path in owned_profiles), "Server shutdown left Chromium or profiles alive")
        assert data(api.post("/api/v1/actions/browser_status", {}))["sessions"] == []
        if not os.environ.get("POWERLINUX_BROWSER_API_ONLY"):
            run_ui(server, website, password, artifacts, errors)
        assert not errors, errors
        print(json.dumps({"real_api": True, "real_server_chromium": True, "sandbox_enabled": True,
                          "private_cdp_pipe": True, "remote_http_requests": True, "unicode_input": True,
                          "history_tabs_resize": True, "owner_auth_csrf_revocation": True,
                          "process_profile_cleanup": True, "ui": not bool(os.environ.get("POWERLINUX_BROWSER_API_ONLY")),
                          "errors": errors, "artifacts": str(artifacts)}))
    finally:
        server.close()
        website.close()


def run_ui(server, website, password, artifacts, errors):
    """The real UI drives a separate sandboxed server Chromium process."""
    with sync_playwright() as p:
        client = p.chromium.launch(executable_path=os.environ.get("POWERLINUX_CHROMIUM", "/usr/bin/chromium"), headless=True, args=["--no-sandbox"])
        try:
            page = client.new_page(viewport={"width": 1366, "height": 900}, locale="zh-CN")
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.on("console", lambda message: errors.append(message.text) if "Content Security Policy" in message.text and message.type == "error" else None)
            direct = []
            def block_direct(route):
                direct.append(route.request.url)
                route.abort()
            page.route(website.url + "/**", block_direct)
            page.goto(server.url)
            page.get_by_label(re.compile(r"^用户名(?:\s|$)")).fill("remote-browser-qa")
            page.get_by_label(re.compile(r"^密码(?:\s|$)")).fill(password)
            page.get_by_role("button", name="登录", exact=True).click()
            page.evaluate("location.hash='/more?page=browser'")
            view = page.locator(".server-browser")
            expect(view).to_be_visible()
            # Labels are shared with the product UI and intentionally language-aware.
            view.get_by_role("button", name="打开浏览器", exact=True).click()
            address = view.get_by_role("textbox", name="网址或搜索", exact=True)
            expect(address).to_be_visible(timeout=15000)
            ui_nonce = secrets.token_hex(8)
            address.fill(website.url + "/first?ui_nonce=" + ui_nonce)
            address.press("Enter")
            screen = view.locator(".server-browser-viewport img")
            expect(screen).to_be_visible(timeout=15000)
            eventually(lambda: website.saw("/first", ui_nonce=ui_nonce), "UI did not navigate the server browser")
            page.wait_for_function("document.querySelector('.server-browser-viewport img')?.naturalWidth > 0")
            def remote_click(x, y):
                screen.scroll_into_view_if_needed()
                box = screen.bounding_box()
                assert box
                width = screen.get_attribute("width")
                height = screen.get_attribute("height")
                screen.click(position={"x": x * box["width"] / int(width), "y": y * box["height"] / int(height)})
            remote_click(100, 100)
            view.get_by_role("button", name="输入文字", exact=True).click()
            ui_text = "面板中文输入-" + secrets.token_hex(5)
            view.get_by_label("发送到网页当前输入框", exact=True).fill(ui_text)
            view.get_by_role("button", name="发送文字", exact=True).click()
            remote_click(90, 160)
            eventually(lambda: website.saw("/entered", value=ui_text), "Panel text input did not reach the server page")
            remote_click(70, 370)
            dialog = view.get_by_role("dialog", name="网页提示", exact=True)
            expect(dialog).to_be_visible(timeout=10000)
            dialog.get_by_label("回复内容", exact=True).fill("面板提示回复")
            dialog.get_by_role("button", name="确定", exact=True).click()
            expect(dialog).not_to_be_visible(timeout=10000)
            eventually(lambda: website.saw("/dialog-answer", value="面板提示回复"), "Panel prompt response did not reach the remote website")
            view.get_by_role("button", name="新建标签页", exact=True).click()
            expect(view.get_by_role("tab")).to_have_count(2)
            view.get_by_role("button", name="关闭标签页 2", exact=True).click()
            expect(view.get_by_role("tab")).to_have_count(1)
            view.get_by_role("button", name="适应窗口", exact=True).click()
            page.wait_for_function("Number(document.querySelector('.server-browser-viewport img')?.getAttribute('width')) !== 1280")
            view.get_by_role("button", name="全屏", exact=True).click()
            expect(view).to_have_class(re.compile("is-fullscreen"))
            page.wait_for_timeout(500)
            view.get_by_role("button", name="退出全屏", exact=True).click()
            expect(view).not_to_have_class(re.compile("is-fullscreen"))
            page.screenshot(path=str(artifacts / "server-browser-desktop.png"), full_page=True)
            for width, height in [(390, 844), (320, 568)]:
                page.set_viewport_size({"width": width, "height": height})
                view.get_by_role("button", name="适应窗口", exact=True).click()
                page.wait_for_timeout(300)
                assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), (width, "horizontal UI overflow")
                page.screenshot(path=str(artifacts / f"server-browser-{width}.png"), full_page=True)
            assert not direct, "Panel client requested website directly: " + str(direct)
            view.get_by_role("button", name="关闭浏览器", exact=True).click()
            expect(view.get_by_role("button", name="打开浏览器", exact=True)).to_be_visible()
        finally:
            client.close()


if __name__ == "__main__":
    main()
