#!/usr/bin/env python3
"""Capture fresh README screenshots of both live, server-routed browser modes."""
from __future__ import annotations

import os
from pathlib import Path
import secrets
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading
import time

from playwright.sync_api import sync_playwright, expect

sys.path.insert(0,str(Path(__file__).resolve().parent))
from fixture import RunningServer
from http_client import APIClient, data


class DemoSites:
    """Small, distinct demo websites served locally for the Firefox WASM capture."""

    def __init__(self):
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
                if self.path.startswith("/atlas"):
                    title = "Open Atlas — a field guide to our world"
                    html = '''<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Open Atlas</title>
<style>*{box-sizing:border-box}body{margin:0;background:#fbfaf7;color:#202522;font:16px/1.55 system-ui,-apple-system,sans-serif}header{height:66px;background:white;border-bottom:1px solid #e8e5dd;display:flex;align-items:center;gap:42px;padding:0 max(calc((100vw - 1120px)/2),32px)}.brand{font-family:Georgia,serif;font-size:25px;letter-spacing:-1px;color:#315e52}.brand b{color:#c8834d}.nav{display:flex;gap:26px;color:#5a615c;font-size:14px}.login{margin-left:auto;color:#315e52;font-weight:600}main{max-width:1120px;margin:56px auto;padding:0 24px}.eyebrow{color:#b56837;text-transform:uppercase;letter-spacing:2px;font-size:12px;font-weight:700}.hero{display:grid;grid-template-columns:1.2fr .8fr;align-items:center;gap:40px}.hero h1{font:500 54px/1.08 Georgia,serif;letter-spacing:-2px;margin:15px 0}.hero p{max-width:560px;color:#66706a;font-size:18px}.search{margin-top:28px;background:#fff;border:1px solid #dedbd2;border-radius:9px;padding:12px 16px;color:#858a84;max-width:530px;box-shadow:0 5px 20px #3d47330b}.globe{height:270px;display:grid;place-items:center;position:relative;color:#315e52}.orb{width:225px;height:225px;border-radius:50%;background:radial-gradient(circle at 34% 31%,#f6d8a4,#d2e1ce 47%,#7ca89a 100%);box-shadow:inset -18px -14px 26px #24544831,0 20px 50px #486c5424;display:grid;place-items:center}.orb svg{width:150px;height:150px;fill:none;stroke:#477b68;stroke-width:2;opacity:.82}.ribbon{position:absolute;right:2px;bottom:24px;background:#fff;padding:12px 16px;border-radius:8px;color:#53625a;box-shadow:0 8px 30px #273c2512;font-size:13px}.section{display:flex;justify-content:space-between;align-items:end;margin-top:54px}.section h2{font:500 27px Georgia,serif;margin:0}.section span{font-size:13px;color:#788079}.cards{display:grid;grid-template-columns:repeat(3,1fr);gap:16px;margin-top:18px}.card{background:white;border:1px solid #e9e6df;border-radius:11px;padding:20px;min-height:145px}.icon{font-size:11px;letter-spacing:1px;font-weight:700;color:#527764;background:#edf3ed;border-radius:5px;padding:5px 7px;width:max-content}.card h3{font:600 17px Georgia,serif;margin:10px 0 4px}.card p{margin:0;color:#727973;font-size:13px}.foot{margin-top:34px;padding-top:20px;border-top:1px solid #e8e5dd;color:#90958e;font-size:12px}@media(max-width:700px){.nav{display:none}main{margin-top:32px}.hero{grid-template-columns:1fr}.globe{display:none}.hero h1{font-size:42px}.cards{grid-template-columns:1fr}}</style></head>
<body><header><div class="brand">Open <b>Atlas</b></div><nav class="nav"><span>Places</span><span>Nature</span><span>History</span><span>Culture</span></nav><div class="login">Explore the atlas</div></header><main><section class="hero"><div><div class="eyebrow">A small guide to a big world</div><h1>Curiosity has<br>a place to start.</h1><p>Explore the landscapes, ideas and everyday wonders that connect us.</p><div class="search">Search a place, topic or question</div></div><div class="globe"><div class="orb"><svg viewBox="0 0 120 120"><circle cx="60" cy="60" r="43"/><ellipse cx="60" cy="60" rx="21" ry="43"/><path d="M18 60h84M25 39h70M25 81h70M60 17v86"/><path d="M32 35l16 7-5 12 14 7-9 14-4 18M78 29l-6 13 12 7 4 12-13 4-8 19" fill="#477b68" fill-opacity=".24"/></svg></div><div class="ribbon">195 countries to explore</div></div></section><div class="section"><h2>Choose a path</h2><span>Ideas for your next discovery</span></div><section class="cards"><article class="card"><div class="icon">GEO</div><h3>Land &amp; water</h3><p>Mountains, rivers and the places in between.</p></article><article class="card"><div class="icon">LIFE</div><h3>Living world</h3><p>Meet the plants and animals around us.</p></article><article class="card"><div class="icon">TIME</div><h3>People &amp; time</h3><p>Stories that travel across generations.</p></article></section><footer class="foot">An independent sample page · Folio browser demonstration</footer></main></body></html>'''
                else:
                    title = "The Web, explained — Fieldnotes"
                    html = '''<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Fieldnotes · The Web, explained</title>
<style>*{box-sizing:border-box}body{margin:0;background:#f5f7fb;color:#14243a;font:16px/1.55 system-ui,-apple-system,sans-serif}header{height:66px;background:#fff;border-bottom:1px solid #e0e6ef;display:flex;align-items:center;padding:0 max(calc((100vw - 1120px)/2),32px);gap:36px}.logo{font-weight:750;letter-spacing:-.7px;font-size:20px;color:#254f8a}.logo i{font-style:normal;color:#4a83c7}.nav{display:flex;gap:25px;color:#5e6b7e;font-size:14px}.search{margin-left:auto;border:1px solid #e0e6ef;border-radius:7px;padding:8px 12px;color:#8792a1;font-size:13px;width:210px}main{max-width:1120px;margin:42px auto;padding:0 24px}.crumb{font-size:13px;color:#77869b}.layout{display:grid;grid-template-columns:225px minmax(0,1fr) 205px;gap:34px;margin-top:23px}.side{border-right:1px solid #e0e6ef;padding-right:20px}.side h3{font-size:12px;text-transform:uppercase;letter-spacing:1px;color:#71819a;margin:7px 0 12px}.side a{display:block;padding:8px 10px;color:#586a83;font-size:14px;text-decoration:none;border-radius:6px}.side a.active{background:#e6effb;color:#23599a;font-weight:650}.article .kicker{color:#3977b9;font-size:13px;font-weight:650}.article h1{font-size:37px;line-height:1.18;letter-spacing:-1px;margin:9px 0 8px}.meta{color:#7f8b9c;font-size:13px;border-bottom:1px solid #e1e6ee;padding-bottom:20px}.lead{font-size:18px;color:#45556b;margin:24px 0}.diagram{background:#fff;border:1px solid #dfe7f0;border-radius:10px;padding:18px;margin:20px 0}.diagram svg{width:100%;height:130px}.article h2{font-size:23px;margin:22px 0 8px}.article p{color:#42536a}.callout{background:#edf4fc;border-left:3px solid #5893ce;padding:13px 15px;color:#526a85;font-size:14px;margin-top:20px}.toc{border-left:1px solid #e0e6ef;padding-left:18px;color:#6e7d90;font-size:13px}.toc b{display:block;color:#293b54;margin-bottom:12px}.toc div{padding:6px 0}.footer{color:#a0a9b5;font-size:12px;margin-top:24px}@media(max-width:800px){.layout{grid-template-columns:1fr}.side,.toc{display:none}.nav{display:none}.article h1{font-size:32px}}</style></head>
<body><header><div class="logo">Field<i>notes</i></div><nav class="nav"><span>Explore</span><span>Guides</span><span>Reference</span><span>About</span></nav><div class="search">⌕ &nbsp; Search fieldnotes</div></header><main><div class="crumb">Guides &nbsp;›&nbsp; How the web works &nbsp;›&nbsp; Foundations</div><div class="layout"><aside class="side"><h3>On this page</h3><a class="active">The Web, explained</a><a>Finding an address</a><a>Request and response</a><a>What happens next?</a><a>Further reading</a></aside><article class="article"><div class="kicker">THE WEB, EXPLAINED</div><h1>A small guide to how the web works</h1><div class="meta">A 6 minute read &nbsp;·&nbsp; Updated today</div><p class="lead">When you open a page, your browser and a web server work together to find, send and display the information you asked for.</p><div class="diagram"><svg viewBox="0 0 620 150" role="img" aria-label="Browser and server exchange"><rect x="18" y="28" width="185" height="92" rx="12" fill="#eef4fc" stroke="#9cb8d9"/><rect x="417" y="28" width="185" height="92" rx="12" fill="#f1f6f1" stroke="#a7c4a7"/><text x="110" y="67" text-anchor="middle" fill="#234f86" font-size="16" font-family="sans-serif">Your browser</text><text x="110" y="94" text-anchor="middle" fill="#657890" font-size="12" font-family="sans-serif">asks for a page</text><text x="510" y="67" text-anchor="middle" fill="#386749" font-size="16" font-family="sans-serif">Web server</text><text x="510" y="94" text-anchor="middle" fill="#657890" font-size="12" font-family="sans-serif">sends the response</text><path d="M220 54 H398" stroke="#4e83bd" stroke-width="3"/><path d="M390 47 L400 54 L390 61" fill="none" stroke="#4e83bd" stroke-width="3"/><path d="M400 97 H222" stroke="#78a27d" stroke-width="3"/><path d="M230 90 L220 97 L230 104" fill="none" stroke="#78a27d" stroke-width="3"/><text x="310" y="43" text-anchor="middle" fill="#61738a" font-size="11" font-family="sans-serif">request</text><text x="310" y="119" text-anchor="middle" fill="#61738a" font-size="11" font-family="sans-serif">response</text></svg></div><h2>It starts with an address</h2><p>A domain name gives people a memorable way to find a website. The browser looks up the server that hosts it, then asks for the page over a connection.</p><div class="callout">A web browser is both a reader and a messenger: it requests the page, then turns the response into the site you see.</div><div class="footer">Fieldnotes · Practical guides for a more curious web</div></article><aside class="toc"><b>In this guide</b><div>The request</div><div>The response</div><div>What to remember</div></aside></div></main></body></html>'''
                body = html.encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)


def wait_until(check, description: str, timeout: int = 240):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.2)
    raise RuntimeError(f"Timed out: {description}")


binary = os.environ["POWERLINUX_TEST_BINARY"]
output = Path(os.environ.get("POWERLINUX_QA_ARTIFACTS",str(Path(__file__).resolve().parents[2]/"docs/screenshots/ui-polish-browsers")))
output.mkdir(parents=True, exist_ok=True)
server = RunningServer(binary)
demo_sites = DemoSites()
api = APIClient(server.url)
password = secrets.token_urlsafe(24)

try:
    data(api.post("/api/v1/auth/setup", {"username": "readme-browser-capture", "password": password,
                                        "setup_token": server.bootstrap}))
    data(api.post("/api/v1/auth/login", {"username": "readme-browser-capture", "password": password}))
    login = data(api.post("/api/v1/auth/login", {"username": "readme-browser-capture", "password": password}))
    api.csrf = login["csrf_token"]

    def call(name, body=None):
        return data(api.post("/api/v1/actions/" + name, body or {}))

    apps = call("browser_apps_list")["items"]
    for app_id in ("panel", "firefox-wasm"):
        app = next(item for item in apps if item["id"] == app_id)
        if app["installed"]:
            continue
        submitted = call("browser_app_change", {"id": app_id, "operation": "install",
                                                  "expected_revision": app["revision"]})
        wait_until(lambda: (lambda task: task["status"] == "succeeded" if task["status"] == "succeeded"
                            else (_ for _ in ()).throw(RuntimeError(str(task)))
                            if task["status"] in ("failed", "canceled", "interrupted") else False)
                   (call("tasks_get", {"id": submitted["task_id"]})), app_id + " installation")

    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(
            executable_path=os.environ.get("POWERLINUX_CHROMIUM", "/usr/bin/chromium"),
            headless=True,
            args=["--no-sandbox", "--enable-unsafe-swiftshader"],
        )
        context = browser.new_context(viewport={"width": 1440, "height": 960}, locale="en-US")
        context.add_cookies([{"name": cookie.name, "value": cookie.value, "url": server.url,
                              "httpOnly": True, "sameSite": "Strict"} for cookie in api.cookies])
        context.add_init_script("if(location.pathname==='/')localStorage.setItem('folio.language','en')")
        page = context.new_page()
        page.goto(server.url + "/#/software?page=applications&subgroup=browsers")
        expect(page.get_by_role("heading", name="Browser apps", exact=True)).to_be_visible()
        expect(page.locator(".browser-app-row")).to_have_count(2)
        page.screenshot(path=str(output / "browser-modes-installed.png"))

        page.evaluate("location.hash='/more?page=browser'")
        view = page.locator(".server-browser")
        expect(view).to_be_visible()
        view.get_by_role("button", name="Open browser", exact=True).click()
        address = view.get_by_role("textbox", name="Address or search", exact=True)
        expect(address).to_be_visible(timeout=15000)
        address.fill("https://developer.mozilla.org/en-US/docs/Web/HTTP/Guides/Overview")
        address.press("Enter")
        image = view.locator(".server-browser-viewport img")
        expect(image).to_be_visible(timeout=20000)
        page.wait_for_function("() => { const x=document.querySelector('.server-browser-viewport img'); return x && x.naturalWidth>0 && x.naturalHeight>0 }")
        page.wait_for_timeout(9000)
        page.set_viewport_size({"width": 1440, "height": 1100})
        page.evaluate("window.scrollTo(0, 0)")
        page.wait_for_timeout(500)
        # A cross-origin navigation may briefly interrupt screenshot capture.
        # Exercise the existing read-only reconnect action; never replay navigation/input.
        for attempt in range(2):
            reconnect = view.get_by_role("button", name="Reconnect", exact=True)
            if not reconnect.is_visible():
                break
            reconnect.click()
            page.wait_for_timeout(2500)
        expect(view.locator(".browser-error")).to_have_count(0)
        expect(image).to_be_visible()
        # A loaded <img> can still be an obsolete blank frame after a disconnect.
        from io import BytesIO
        from PIL import Image, ImageStat
        frame = Image.open(BytesIO(image.screenshot())).convert("RGB")
        assert max(ImageStat.Stat(frame).stddev) > 12, "Remote website frame is blank"
        page.evaluate("document.activeElement?.blur(); window.scrollTo(0, 0)")
        page.mouse.move(0, 0)
        page.screenshot(path=str(output / "panel-browser-mdn.png"))

        page.evaluate("location.hash='/software?page=applications&subgroup=browsers'")
        firefox_link = page.get_by_role("link", name="Open Firefox", exact=True)
        expect(firefox_link).to_be_visible()
        with context.expect_page() as popup:
            firefox_link.click()
        firefox = popup.value
        firefox_errors: list[str] = []
        firefox.on("console", lambda message: firefox_errors.append(message.text[:400])
                   if message.type == "error" else None)
        # Block direct client navigation to the target; Firefox WASM must reach it through Folio Wisp.
        firefox.wait_for_load_state()
        expect(firefox.locator("#start-btn")).to_be_visible()
        assert firefox.evaluate("isSecureContext && crossOriginIsolated && typeof WebAssembly.Suspending === 'function'")
        firefox.locator("#start-btn").click(timeout=120000)
        firefox.wait_for_function("typeof window.geckoEvalChrome === 'function'", timeout=120000)
        firefox.wait_for_timeout(4000)
        canvas = firefox.locator("#screen")
        canvas.click(position={"x": 350, "y": 64})
        firefox.keyboard.press("Control+l")
        firefox.keyboard.type(demo_sites.url + "/atlas", delay=20)
        firefox.keyboard.press("Enter")
        firefox.wait_for_timeout(7000)
        state = firefox.evaluate("() => window.geckoEvalChrome('JSON.stringify({url:gBrowser.currentURI.spec,state:content.document.readyState,title:content.document.title,body:content.document.body?.innerText?.slice(0,120)})')")
        print("Firefox browser state:", state)
        print("Firefox console errors:", firefox_errors[:8])
        import json
        loaded = json.loads(state) if isinstance(state, str) else state
        assert loaded["state"] == "complete" and loaded["title"] == "Open Atlas", loaded
        assert not firefox_errors, firefox_errors
        firefox.screenshot(path=str(output / "firefox-wasm-atlas.png"))
        assert len((output / "firefox-wasm-atlas.png").read_bytes()) > 30000, firefox_errors
        browser.close()
finally:
    demo_sites.close()
    server.close()

for screenshot in sorted(output.glob("*.png")):
    print(f"{screenshot.name}: {screenshot.stat().st_size} bytes")
