#!/usr/bin/env python3
"""Black-box release gates against the actual Go service and native MCP wire."""
from __future__ import annotations

import base64
from concurrent.futures import ThreadPoolExecutor
import hashlib
import hmac
import json
import os
from pathlib import Path
import secrets
import sqlite3
import subprocess
import statistics
import struct
import sys
import time
import unittest
import urllib.parse

from fixture import RunningServer
from http_client import APIClient, data
from mcp_client import HTTPMCPClient, StdioMCPClient, tool_data


def totp(secret, counter=None):
    if counter is None:
        counter = int(time.time()) // 30
    key = base64.b32decode(secret + "=" * (-len(secret) % 8))
    digest = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    offset = digest[-1] & 15
    value = struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7FFFFFFF
    return f"{value % 1000000:06d}"


class PhaseOne(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        binary = os.environ.get("POWERLINUX_TEST_BINARY")
        if not binary:
            raise RuntimeError("POWERLINUX_TEST_BINARY must point to the real compiled release binary")
        cls.server = RunningServer(binary)
        cls.addClassCleanup(cls.server.close)
        cls.admin = APIClient(cls.server.url)
        cls.username = "qa-admin"
        cls.password = "QA-" + secrets.token_urlsafe(24)
        cls.initial_status = data(cls.admin.get("/api/v1/session"))
        setup = cls.admin.post("/api/v1/auth/setup", {
            "username": cls.username, "password": cls.password,
            "setup_token": cls.server.bootstrap,
        })
        data(setup)
        session = data(cls.admin.post("/api/v1/auth/login", {
            "username": cls.username, "password": cls.password,
        }))
        cls.admin.csrf = session["csrf_token"]

    @classmethod
    def tearDownClass(cls):
        pass  # addClassCleanup also runs when setUpClass fails

    def new_token(self, *, scopes=None, resources=None, name="QA external AI"):
        return data(self.admin.post("/api/v1/tokens", {
            "name": name, "kind": "mcp", "scopes": scopes or ["notes:read", "notes:write", "system:read"],
            "resources": resources or ["host:local"], "expires_in_seconds": 3600,
        }))

    def new_note(self, **kwargs):
        body = {"title": "交接便签", "content": "由人创建", "target_id": "host:local", **kwargs}
        return data(self.admin.post("/api/v1/notes", body))

    def test_01_bootstrap_is_local_and_one_use(self):
        self.assertFalse(self.initial_status["initialized"])
        self.assertFalse(self.initial_status["authenticated"])
        self.assertFalse((self.server.data_dir / "bootstrap-token").exists())
        status, payload, _ = APIClient(self.server.url).post("/api/v1/auth/setup", {
            "username": "another-admin", "password": self.password,
            "setup_token": self.server.bootstrap,
        })
        self.assertEqual(status, 409, payload)

    def test_02_authentication_cookie_csrf_and_logout(self):
        unauthenticated = APIClient(self.server.url)
        self.assertEqual(unauthenticated.get("/api/v1/notes")[0], 401)
        status, payload, _ = self.admin.post("/api/v1/notes", {"title": "CSRF", "content": ""}, csrf=False)
        self.assertEqual(status, 403, payload)
        cookie = next(c for c in self.admin.cookies if c.name == "pl_session")
        self.assertTrue(cookie.has_nonstandard_attr("HttpOnly"))
        self.assertIn(cookie.get_nonstandard_attr("SameSite"), ("Strict", "Lax"))
        other = APIClient(self.server.url)
        session = data(other.post("/api/v1/auth/login", {"username": self.username, "password": self.password}))
        other.csrf = session["csrf_token"]
        data(other.post("/api/v1/auth/logout", {}))
        self.assertEqual(other.get("/api/v1/notes")[0], 401)

    def test_03_human_mcp_notes_history_and_conflict(self):
        note = self.new_note()
        token = self.new_token()
        mcp = HTTPMCPClient(self.server.url + "/mcp", token["token"])
        server_info = mcp.initialize()
        self.assertEqual(server_info["serverInfo"]["name"], "powerlinux")
        status, tools = mcp.request("tools/list", {})
        self.assertEqual(status, 200)
        names = [t["name"] for t in tools["result"]["tools"]]
        self.assertIn("notes_update", names)
        observed = tool_data(mcp.call("notes_get", {"id": note["id"]}))
        self.assertEqual(observed["content"], "由人创建")
        ai_note = tool_data(mcp.call("notes_update", {
            "id": note["id"], "expected_revision": note["revision"], "content": "由外部 AI 修改",
        }))
        self.assertEqual(ai_note["revision"], note["revision"] + 1)
        self.assertEqual(ai_note["updated_by"]["kind"], "mcp")
        self.assertEqual(ai_note["updated_by"]["name"], token["name"])
        human_observed = data(self.admin.get("/api/v1/notes/" + note["id"]))
        self.assertEqual(human_observed["content"], "由外部 AI 修改")
        stale = self.admin.patch("/api/v1/notes/" + note["id"], {
            "expected_revision": note["revision"], "content": "不能静默覆盖 AI",
        })
        self.assertEqual(stale[0], 412, stale[1])
        self.assertEqual(stale[1]["error"]["code"], "revision_conflict")
        self.assertEqual(stale[1]["error"]["details"]["current"]["content"], "由外部 AI 修改")
        unchanged = data(self.admin.get("/api/v1/notes/" + note["id"]))
        self.assertEqual(unchanged["content"], "由外部 AI 修改")
        history = data(self.admin.get("/api/v1/notes/" + note["id"] + "/history"))
        self.assertEqual([x["revision"] for x in history], [2, 1])
        self.assertEqual(history[0]["updated_by"]["kind"], "mcp")

    def test_04_scope_resource_and_actor_are_server_enforced(self):
        other_note = self.new_note(target_id="site:private")
        token = self.new_token(scopes=["notes:read"], resources=["host:local"])
        client = APIClient(self.server.url, token["token"])
        visible = data(client.get("/api/v1/notes"))
        self.assertNotIn(other_note["id"], [n["id"] for n in visible])
        self.assertEqual(client.get("/api/v1/notes/" + other_note["id"])[0], 403)
        self.assertEqual(client.post("/api/v1/notes", {"title": "write denied", "content": ""})[0], 403)
        mcp = HTTPMCPClient(self.server.url + "/mcp", token["token"])
        mcp.initialize()
        _, tools = mcp.request("tools/list", {})
        self.assertNotIn("notes_create", [t["name"] for t in tools["result"]["tools"]])
        forged = self.admin.post("/api/v1/notes", {
            "title": "actor spoof", "content": "", "created_by": {"kind": "mcp", "name": "forged"},
        })
        self.assertEqual(forged[0], 400, forged[1])

    def test_05_mcp_auth_origin_and_revocation(self):
        bad = HTTPMCPClient(self.server.url + "/mcp", "invalid-test-token")
        self.assertEqual(bad.request("initialize", {})[0], 401)
        token = self.new_token()
        mcp = HTTPMCPClient(self.server.url + "/mcp", token["token"])
        mcp.initialize()
        self.assertEqual(mcp.request("tools/list", {}, origin="https://attacker.example")[0], 403)
        data(self.admin.delete("/api/v1/tokens/" + token["id"]))
        self.assertEqual(mcp.request("tools/list", {})[0], 401)
        self.assertEqual(APIClient(self.server.url, token["token"]).get("/api/v1/notes")[0], 401)

    def test_06_stdio_uses_live_backend_and_rechecks_token(self):
        token = self.new_token()
        note = self.new_note(title="stdio shared state")
        env = os.environ.copy()
        env["POWERLINUX_MCP_TOKEN"] = token["token"]
        client = StdioMCPClient([
            self.server.binary, "mcp", "--stdio", "--endpoint", self.server.url,
        ], env)
        try:
            client.initialize()
            payload = client.request("tools/call", {"name": "notes_get", "arguments": {"id": note["id"]}})
            self.assertEqual(tool_data(payload["result"])["id"], note["id"])
            payload = client.request("tools/call", {"name": "notes_update", "arguments": {
                "id": note["id"], "expected_revision": 1, "content": "stdio writes same database",
            }})
            self.assertEqual(tool_data(payload["result"])["revision"], 2)
            self.assertEqual(data(self.admin.get("/api/v1/notes/" + note["id"]))["content"], "stdio writes same database")
            data(self.admin.delete("/api/v1/tokens/" + token["id"]))
            denied = client.request("tools/call", {"name": "notes_get", "arguments": {"id": note["id"]}})
            if "error" in denied:
                self.assertEqual(denied["error"]["code"], -32001, denied)
                self.assertIn("authentication", denied["error"]["message"].lower(), denied)
            else:
                self.assertTrue(denied["result"]["isError"], denied)
                self.assertEqual(denied["result"]["structuredContent"]["error"]["code"], "unauthorized")
        finally:
            client.close()

    def test_07_limits_and_note_lifecycle(self):
        self.assertEqual(self.admin.post("/api/v1/notes", {"title": "字" * 81, "content": ""})[0], 400)
        self.assertEqual(self.admin.post("/api/v1/notes", {"title": "overlong", "content": "字" * 10001})[0], 400)
        note = self.new_note(title="字" * 80, content="字" * 10000)
        archived = data(self.admin.post("/api/v1/notes/" + note["id"] + "/archive", {"expected_revision": 1}))
        self.assertTrue(archived["archived"])
        self.assertNotIn(note["id"], [x["id"] for x in data(self.admin.get("/api/v1/notes"))])
        restored = data(self.admin.post("/api/v1/notes/" + note["id"] + "/restore", {"expected_revision": 2}))
        self.assertFalse(restored["archived"])
        deleted = data(self.admin.delete("/api/v1/notes/" + note["id"], {"expected_revision": 3}))
        self.assertTrue(deleted["deleted"])
        self.assertEqual(self.admin.get("/api/v1/notes/" + note["id"])[0], 404)

    def test_08_totp_has_no_alternate_login_bypass(self):
        enrollment = data(self.admin.post("/api/v1/account/totp/enroll", {"password": self.password}))
        counter = int(time.time()) // 30
        data(self.admin.post("/api/v1/account/totp/confirm", {"otp": totp(enrollment["secret"], counter)}))
        try:
            no_otp = APIClient(self.server.url).post("/api/v1/auth/login", {"username": self.username, "password": self.password})
            self.assertEqual(no_otp[0], 401)
            with_otp = APIClient(self.server.url)
            credentials = {"username": self.username, "password": self.password,
                           "otp": totp(enrollment["secret"], counter + 1)}
            data(with_otp.post("/api/v1/auth/login", credentials))
            replay = APIClient(self.server.url).post("/api/v1/auth/login", credentials)
            self.assertEqual(replay[0], 401)
            self.assertIn(APIClient(self.server.url).post("/quick_login", credentials)[0], (404, 405))
        finally:
            data(self.admin.post("/api/v1/account/totp/disable", {"password": self.password,
                 "otp": totp(enrollment["secret"], counter)}))

    def test_09_unknown_user_failures_do_not_disable_panel(self):
        client = APIClient(self.server.url)
        codes = [client.post("/api/v1/auth/login", {"username": "unknown-test-user", "password": "invalid"})[0]
                 for _ in range(10)]
        self.assertIn(429, codes)
        self.assertEqual(APIClient(self.server.url).get("/healthz")[0], 200)
        data(client.post("/api/v1/auth/login", {"username": self.username, "password": self.password}))

    def test_10_durable_file_task_has_real_verified_output(self):
        token = self.new_token(scopes=["files:write", "files:read", "tasks:read", "audit:read"], resources=["files:workspace"])
        client = APIClient(self.server.url, token["token"])
        task = data(client.post("/api/v1/actions/files_write", {
            "root": "workspace", "path": "qa-written.txt", "content": "real durable job verification",
        }))
        self.assertIn("id", task)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            task = data(client.get("/api/v1/tasks/" + task["id"]))
            if task.get("state", task.get("status")) not in ("queued", "running", "verifying"):
                break
            time.sleep(.05)
        self.assertEqual(task.get("state", task.get("status")), "succeeded", task)
        observed = (self.server.data_dir / "workspace" / "qa-written.txt").read_text()
        self.assertEqual(observed, "real durable job verification")
        self.assertEqual(APIClient(self.server.url, self.new_token()["token"]).get("/api/v1/tasks/" + task["id"])[0], 403)
        tasks = data(client.get("/api/v1/tasks"))
        self.assertTrue(any(x["id"] == task["id"] for x in tasks))
        audit = data(client.get("/api/v1/audit"))
        self.assertTrue(any(x["action"] == "files_write" and x["resource"] == "files:workspace"
                            and x["actor"]["id"] == "token:" + token["id"] for x in audit), audit)

    def test_11_remote_oauth_pkce_consent_and_revocation(self):
        issuer = self.server.url
        client = APIClient(issuer)
        status, metadata, _ = client.get("/.well-known/oauth-authorization-server")
        self.assertEqual(status, 200)
        self.assertIn("S256", metadata["code_challenge_methods_supported"])
        registration = client.post("/oauth/register", {"client_name": "QA PKCE client",
            "redirect_uris": ["http://127.0.0.1:19887/callback"], "token_endpoint_auth_method": "none",
            "grant_types": ["authorization_code"], "response_types": ["code"]})
        self.assertEqual(registration[0], 201, registration[1])
        verifier = secrets.token_urlsafe(48)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
        request = {"client_id": registration[1]["client_id"], "redirect_uri": "http://127.0.0.1:19887/callback",
            "response_type": "code", "code_challenge_method": "S256", "code_challenge": challenge,
            "resource": issuer + "/mcp", "scope": "system:read notes:read", "state": secrets.token_urlsafe(16)}
        consent = self.admin.get("/oauth/authorize?" + urllib.parse.urlencode(request))
        self.assertEqual(consent[0], 200)
        self.assertIn(b"QA PKCE client", consent[1])
        unauthorized_consent = self.admin.form("/oauth/authorize", {**request, "decision": "allow", "csrf_token": "invalid"}, follow_redirects=False)
        self.assertEqual(unauthorized_consent[0], 403)
        authorized = self.admin.form("/oauth/authorize", {**request, "decision": "allow", "csrf_token": self.admin.csrf}, follow_redirects=False)
        self.assertEqual(authorized[0], 303)
        response = urllib.parse.parse_qs(urllib.parse.urlsplit(authorized[2]["Location"]).query)
        self.assertEqual(response["state"], [request["state"]])
        exchange = {"grant_type": "authorization_code", "client_id": request["client_id"],
                    "redirect_uri": request["redirect_uri"], "code": response["code"][0],
                    "code_verifier": verifier, "resource": request["resource"]}
        issued = client.form("/oauth/token", exchange)
        self.assertEqual(issued[0], 200)
        self.assertEqual(issued[1]["token_type"], "Bearer")
        self.assertEqual(client.form("/oauth/token", exchange)[0], 400)
        mcp = HTTPMCPClient(issuer + "/mcp", issued[1]["access_token"])
        mcp.initialize()
        tool_data(mcp.call("system_get_status", {}))
        self.assertEqual(client.form("/oauth/revoke", {"token": issued[1]["access_token"]})[0], 200)
        self.assertEqual(mcp.request("tools/list", {})[0], 401)

    def test_12_real_status_latency_and_audit_no_secret(self):
        token = self.new_token(scopes=["system:read"], resources=["host:local"])
        # Python urllib creates an SSL CA context in each new opener, even for
        # HTTP. That cost is excluded; the independent Go client reuses20 live
        # connections and measures200 actual requests after100 warmup requests.
        benchmark = os.environ.get("POWERLINUX_TEST_BENCHMARK")
        if not benchmark:
            benchmark = str(Path(self.server.directory.name) / "benchmark-http")
            pinned_go = Path("/workspace/.tools/go-1.27.2/go/bin/go")
            go_binary = os.environ.get("GO_BIN", str(pinned_go) if pinned_go.exists() else "go")
            build_env = os.environ.copy()
            build_env.setdefault("GOCACHE", "/workspace/.cache/go-build" if Path("/workspace").exists()
                                 else str(Path(self.server.directory.name) / "go-build"))
            build_env["GOTOOLCHAIN"] = "local"
            subprocess.run([go_binary, "build", "-o", benchmark, str(Path(__file__).with_name("benchmark_http.go"))],
                           check=True, timeout=120, env=build_env)
        # Validate the same business snapshot separately from timing mechanics.
        snapshot = data(APIClient(self.server.url, token["token"]).get("/api/v1/system/status"))
        self.assertIn("hostname", snapshot)
        self.assertIn("sampled_at", snapshot)
        measured = json.loads(subprocess.check_output([benchmark], input=json.dumps({
            "url": self.server.url, "token": token["token"],
        }).encode(), timeout=30))
        self.assertEqual(measured["status"]["requests"], 200)
        self.assertEqual(measured["status"]["concurrency"], 20)
        p95 = measured["status"]["p95_ms"]
        rss = "unknown"
        proc = Path(f"/proc/{self.server.process.pid}/status")
        for line in proc.read_text().splitlines():
            if line.startswith("VmRSS:"):
                rss = line.split(":", 1)[1].strip()
        evidence = {"requests": 200, "concurrency": 20, "http_p50_ms": measured["status"]["p50_ms"],
                    "http_p95_ms": p95, "go_service_rss": rss,
                    "client": "Go net/http keep-alive; HTTP-only timing;100 warmup requests",
                    "health_http_p95_ms": measured["health"]["p95_ms"],
                    "limits": "Cloud workspace measurement; not a fixed 2vCPU/2GiB benchmark VM"}
        print("PHASE1_BASELINE " + json.dumps(evidence))
        self.assertLess(p95, 100, evidence)
        self.assertNotEqual(rss, "unknown")
        self.assertLessEqual(int(rss.split()[0]), 100 * 1024, evidence)
        audit = data(self.admin.get("/api/v1/audit"))
        raw = json.dumps(audit)
        self.assertNotIn(token["token"], raw)
        self.assertNotIn(self.password, raw)
        self.assertNotIn(self.server.bootstrap, raw)
        self.assertTrue(any(x["actor"]["kind"] == "mcp" for x in audit))

    def test_13_expired_session_cannot_refresh_itself(self):
        client = APIClient(self.server.url)
        session = data(client.post("/api/v1/auth/login", {"username": self.username, "password": self.password}))
        cookie = next(c for c in client.cookies if c.name == "pl_session")
        # Advance only this disposable session's persisted expiry, instead of waiting12h.
        with sqlite3.connect(self.server.data_dir / "powerlinux.db", timeout=5) as database:
            database.execute("UPDATE sessions SET expires=0 WHERE hash=?", (hashlib.sha256(cookie.value.encode()).hexdigest(),))
        self.assertEqual(client.get("/api/v1/notes")[0], 401)
        client.csrf = session["csrf_token"]
        self.assertEqual(client.post("/api/v1/notes", {"title": "expired session", "content": ""})[0], 401)
        self.assertFalse(data(client.get("/api/v1/session"))["authenticated"])

    def test_14_restart_preserves_state_and_excludes_duplicate_server(self):
        note = self.new_note(title="restart persistence")
        duplicate = subprocess.run([self.server.binary, "serve", "--data-dir", str(self.server.data_dir),
                                    "--listen", "127.0.0.1:0"], capture_output=True, timeout=10)
        self.assertNotEqual(duplicate.returncode, 0)
        self.assertIn(b"already used", duplicate.stderr)
        self.server.restart()
        observed = data(self.admin.get("/api/v1/notes/" + note["id"]))
        self.assertEqual(observed["title"], "restart persistence")
        self.assertFalse((self.server.data_dir / "bootstrap-token").exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
