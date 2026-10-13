#!/usr/bin/env python3
"""Real installed API↔privileged Agent file transfers in an owned VM.

Use only an exclusive guest window and the private actual deployment fixture.
All filesystem mutations stay under a unique QA prefix. This complements the
workspace-only Files14 and kernel-only EXDEV suites, not software lifecycle.
"""
from __future__ import annotations

import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import subprocess
import sys
import time
import unittest
import urllib.parse

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from http_client import APIClient, data
from mcp_client import HTTPMCPClient, tool_data
from test_files import binary_request


class AgentFileTransfers(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        assert os.environ.get("POWERLINUX_QA_VM_EXCLUSIVE") == "1", "Requires the coordinated exclusive owned-VM window"
        path = Path(os.environ["POWERLINUX_QA_PANEL_FILE"])
        assert path.is_file() and not path.is_symlink() and not path.stat().st_mode & 0o077
        cls.fixture = json.loads(path.read_text())
        cls.version = os.environ["POWERLINUX_QA_PACKAGE_VERSION"]
        cls.helper = str(Path(cls.fixture["guest"]) / "ssh.sh")
        identity = subprocess.check_output([cls.helper, "cat /proc/1/comm; systemd-detect-virt"], timeout=30).decode().splitlines()
        assert identity == ["systemd", "qemu"]
        cls.api = APIClient(cls.fixture["url"])
        cls.api.csrf = data(cls.api.post("/api/v1/auth/login", {
            "username": cls.fixture["username"], "password": cls.fixture["password"],
        }))["csrf_token"]
        status, health, _ = cls.api.get("/healthz")
        assert status == 200 and health["version"] == cls.version and health["ready"] is True
        cls.shared = os.environ.get("POWERLINUX_QA_SHARED_ROOT", "shared")
        cls.shared_path = os.environ.get("POWERLINUX_QA_SHARED_PATH", "/srv/powerlinux-qa-files")
        cls.prefix = "qa-cross-files-" + secrets.token_hex(8)
        cls.tokens = []
        cls.addClassCleanup(cls.cleanup)
        roots = data(cls.api.post("/api/v1/actions/fs_roots", {}))
        assert {"workspace", cls.shared, "host"} <= {r["id"] for r in roots}
        assert all("path" not in r for r in roots), "Agent root absolute paths leaked into public root listing"
        for root in ("workspace", cls.shared):
            data(cls.api.post("/api/v1/actions/fs_mkdir", {"root": root, "path": cls.prefix}))

    @classmethod
    def remote(cls, script):
        result = subprocess.run([cls.helper, "set -eu; " + script], capture_output=True, timeout=35)
        assert result.returncode == 0, "Controlled owned-VM fixture command failed"
        return result.stdout

    @classmethod
    def cleanup(cls):
        # Only known prefix trees are removed; follow neither fixture symlinks
        # nor unrelated private metadata/receipts. Purge only matching trash.
        cls.agent_ready()
        for root in ("workspace", cls.shared):
            rows = data(cls.api.post("/api/v1/actions/fs_trash_list", {"root": root}))
            for row in rows:
                if row.get("path", "").startswith(cls.prefix + "/"):
                    initial = data(cls.api.post("/api/v1/actions/fs_purge", {"root": root, "id": row["id"]}))
                    cls.poll(cls.api, initial)
        for token in cls.tokens:
            cls.api.delete("/api/v1/tokens/" + token["id"])
        paths = ["/var/lib/powerlinux/workspace/" + cls.prefix, cls.shared_path + "/" + cls.prefix]
        cls.remote("rm -rf -- " + " ".join(shlex.quote(p) for p in paths) +
                   "; test ! -e " + shlex.quote(paths[0]) + "; test ! -e " + shlex.quote(paths[1]))

    @classmethod
    def agent_ready(cls):
        # Type=simple acknowledges fork, before module constructors/socket
        # readiness. Wait for the real ordinary-UID authenticated health check.
        cls.remote("policy_hash=$(sha256sum /etc/powerlinux/agent-policy.json); policy_hash=${policy_hash%% *}; "
            "for attempt in $(seq 1 30); do if runuser -u powerlinux -- /opt/powerlinux/current/powerlinux "
            "agent-healthcheck --socket /run/powerlinux-agent/agent.sock --expected-version " + shlex.quote(cls.version) +
            " --expected-policy-sha256 \"$policy_hash\" --timeout 1s >/dev/null 2>&1; "
            "then exit 0; fi; sleep 0.1; done; exit 1")

    @staticmethod
    def poll(client, initial, *, success=True):
        deadline = time.monotonic() + 35
        while time.monotonic() < deadline:
            job = data(client.get("/api/v1/tasks/" + initial["id"]))
            if job["status"] in ("succeeded", "failed", "canceled", "interrupted"):
                assert (job["status"] == "succeeded") == success, "Actual cross-endpoint job had unexpected terminal state"
                if success:
                    assert job["result"]["exit_code"] == 0
                    return job["result"].get("data")
                return job
            time.sleep(.05)
        raise AssertionError("Actual Agent file job did not finish within 35 seconds")

    def execute(self, name, values, client=None):
        response = (client or self.api).post("/api/v1/actions/" + name, values)
        self.assertEqual(response[0], 200, name + " failed; response body intentionally not exposed")
        return response[1]["data"]

    def token(self, scopes, resources):
        result = data(self.api.post("/api/v1/tokens", {
            "name": "Actual cross-Agent file QA", "kind": "mcp", "scopes": scopes, "resources": resources,
        }))
        self.tokens.append(result)
        return result

    def make(self, root, path, text):
        return self.execute("fs_create", {"root": root, "path": self.prefix + "/" + path,
                                          "content": text, "encoding": "utf-8"})

    def read(self, root, path):
        return self.execute("fs_read", {"root": root, "path": self.prefix + "/" + path, "encoding": "utf-8"})

    def entry(self, root, path):
        parent, _, name = path.rpartition("/")
        listing = self.execute("fs_list", {"root": root, "path": parent, "limit": 50})
        matches = [row for row in listing["items"] if row["name"] == name]
        self.assertEqual(len(matches), 1, "Expected actual entry not found through public fs_list")
        return matches[0]

    def task(self, name, values, client=None, *, success=True):
        return self.poll(client or self.api, self.execute(name, values, client), success=success)

    def test_01_copy_move_scope_and_real_destination_identity(self):
        text = "API与真正Agent之间的中文传输 🧪"
        self.make("workspace", "source.txt", text)
        token = self.token(["files:read", "files:write", "tasks:read"], ["fs:workspace", "fs:" + self.shared])
        mcp = HTTPMCPClient(self.fixture["url"] + "/mcp", token["token"])
        mcp.initialize()
        client = APIClient(self.fixture["url"], token["token"])
        initial = tool_data(mcp.call("fs_copy", {"root": "workspace", "path": self.prefix + "/source.txt",
            "destination_root": self.shared, "destination_path": self.prefix + "/remote.txt"}))
        self.poll(client, initial)
        self.assertEqual(self.read(self.shared, "remote.txt")["content"], text)
        identities = self.remote("stat -c '%u:%g' " + shlex.quote(self.shared_path) + " " +
            shlex.quote(self.shared_path + "/" + self.prefix + "/remote.txt")).decode().splitlines()
        self.assertEqual(identities[0], identities[1], "Agent destination did not inherit its safe root identity")
        original = self.read(self.shared, "remote.txt")
        self.make("workspace", "conflict.txt", "keep local bytes")
        self.task("fs_move", {"root": self.shared, "path": self.prefix + "/remote.txt",
            "destination_root": "workspace", "destination_path": self.prefix + "/conflict.txt"}, success=False)
        self.assertEqual(self.read(self.shared, "remote.txt")["sha256"], original["sha256"])
        self.assertEqual(self.read("workspace", "conflict.txt")["content"], "keep local bytes")
        limited = APIClient(self.fixture["url"], self.token(
            ["files:read", "files:write", "tasks:read"], ["fs:workspace"])["token"])
        self.assertEqual(limited.post("/api/v1/actions/fs_copy", {
            "root": "workspace", "path": self.prefix + "/source.txt", "destination_root": self.shared,
            "destination_path": self.prefix + "/denied.txt"})[0], 403)
        self.task("fs_move", {"root": self.shared, "path": self.prefix + "/remote.txt",
            "destination_root": "workspace", "destination_path": self.prefix + "/returned.txt"})
        self.assertEqual(self.read("workspace", "returned.txt")["content"], text)
        names = [row["name"] for row in self.execute("fs_list", {"root": self.shared, "path": self.prefix})["items"]]
        self.assertNotIn("remote.txt", names)
        owners = self.remote("stat -c '%u:%g' /var/lib/powerlinux/workspace " +
            shlex.quote("/var/lib/powerlinux/workspace/" + self.prefix + "/returned.txt")).decode().splitlines()
        self.assertEqual(owners[0], owners[1], "Returned API file retained privileged source identity")

    def test_02_real_directory_roundtrip_mode_and_unsafe_tree_rollback(self):
        source = self.prefix + "/tree"
        for path in (source, source + "/readonly"):
            self.execute("fs_mkdir", {"root": "workspace", "path": path})
        self.make("workspace", "tree/readonly/file.txt", "readonly subtree bytes")
        for path, mode in ((source + "/readonly/file.txt", 0o440), (source + "/readonly", 0o500), (source, 0o770)):
            self.task("fs_chmod", {"root": "workspace", "path": path, "mode": mode})
        self.task("fs_copy", {"root": "workspace", "path": source, "recursive": True, "destination_root": self.shared,
            "destination_path": self.prefix + "/tree-remote"})
        self.task("fs_copy", {"root": self.shared, "path": self.prefix + "/tree-remote", "recursive": True, "destination_root": "workspace",
            "destination_path": self.prefix + "/tree-back"})
        for root, directory in ((self.shared, "tree-remote"), ("workspace", "tree-back")):
            for path, mode in ((directory, 0o770), (directory + "/readonly", 0o500), (directory + "/readonly/file.txt", 0o440)):
                entry = self.entry(root, self.prefix + "/" + path)
                self.assertEqual(entry["mode"], mode)
            self.assertEqual(self.read(root, directory + "/readonly/file.txt")["content"], "readonly subtree bytes")
        bad = self.shared_path + "/" + self.prefix + "/unsafe-tree"
        self.remote("mkdir -m 0755 " + shlex.quote(bad) + "; printf '%s' safe > " + shlex.quote(bad + "/first.txt") +
                    "; ln -s /var/lib/powerlinux/encryption-key " + shlex.quote(bad + "/late-link"))
        self.task("fs_copy", {"root": self.shared, "path": self.prefix + "/unsafe-tree", "recursive": True, "destination_root": "workspace",
            "destination_path": self.prefix + "/unsafe-destination"}, success=False)
        names = [row["name"] for row in self.execute("fs_list", {"root": "workspace", "path": self.prefix})["items"]]
        self.assertNotIn("unsafe-destination", names)

    def test_03_explicit_host_scope_and_private_path_symlink_boundaries(self):
        self.make(self.shared, "host-public.txt", "controlled public fixture")
        no_host = APIClient(self.fixture["url"], self.token(["files:read"], ["fs:host"])["token"])
        self.assertEqual(self.execute("fs_roots", {}, no_host), [])
        public = self.shared_path.lstrip("/") + "/" + self.prefix + "/host-public.txt"
        self.assertEqual(no_host.post("/api/v1/actions/fs_read", {"root": "host", "path": public})[0], 403)
        host = APIClient(self.fixture["url"], self.token(["files:read", "files:host"], ["fs:host"])["token"])
        self.assertEqual(self.execute("fs_read", {"root": "host", "path": public}, host)["content"], "controlled public fixture")
        for path in ("var/lib/powerlinux/encryption-key", "var/lib/powerlinux/powerlinux.db",
                     "etc/powerlinux/agent-policy.json", "etc/ssh/ssh_host_ed25519_key"):
            self.assertEqual(host.post("/api/v1/actions/fs_read", {"root": "host", "path": path})[0], 403)
        link = self.shared_path + "/" + self.prefix + "/private-link"
        self.remote("ln -s /var/lib/powerlinux " + shlex.quote(link))
        self.assertEqual(self.api.post("/api/v1/actions/fs_read", {
            "root": self.shared, "path": self.prefix + "/private-link/encryption-key"})[0], 403)

    def test_04_real_agent_http_upload_download_mcp_identity_recycle_restart(self):
        payload = bytes(range(256)) * (3 * 1024 * 1024 // 256) + b"actual Agent stream tail"
        path = self.prefix + "/binary.bin"
        upload = self.execute("fs_upload_begin", {"root": self.shared, "path": path,
            "size": len(payload), "sha256": hashlib.sha256(payload).hexdigest()})
        offset = 0
        while offset < len(payload):
            chunk = payload[offset:offset + 1024 * 1024]
            query = urllib.parse.urlencode({"root": self.shared, "offset": offset, "sha256": hashlib.sha256(chunk).hexdigest()})
            status, raw, _ = binary_request(self.api, "PATCH", "/api/v1/files/uploads/" + upload["id"] + "/chunk?" + query, chunk)
            self.assertEqual(status, 200)
            offset += len(chunk)
            self.assertEqual(json.loads(raw)["data"]["offset"], offset)
        self.task("fs_upload_commit", {"root": self.shared, "id": upload["id"]})
        query = urllib.parse.urlencode({"root": self.shared, "path": path})
        status, body, headers = binary_request(self.api, "GET", "/api/v1/files/download?" + query)
        self.assertEqual(status, 200)
        self.assertEqual(hashlib.sha256(body).digest(), hashlib.sha256(payload).digest())
        status, body, headers = binary_request(self.api, "HEAD", "/api/v1/files/download?" + query)
        self.assertEqual((status, body, int(headers["Content-Length"])), (200, b"", len(payload)))
        status, body, _ = binary_request(self.api, "GET", "/api/v1/files/download?" + query, headers={"Range": "bytes=71-1024"})
        self.assertEqual((status, body), (206, payload[71:1025]))
        token = self.token(["files:read", "files:write", "tasks:read"], ["fs:" + self.shared])
        mcp = HTTPMCPClient(self.fixture["url"] + "/mcp", token["token"])
        mcp.initialize()
        bounded = tool_data(mcp.call("fs_download", {"root": self.shared, "path": path, "offset": 83, "length": 32768}))
        self.assertEqual(base64.b64decode(bounded["data_base64"]), payload[83:83 + 32768])
        self.make(self.shared, "history.txt", "before external AI")
        old = self.read(self.shared, "history.txt")
        tool_data(mcp.call("fs_edit", {"root": self.shared, "path": self.prefix + "/history.txt",
            "content": "native Agent external AI author", "encoding": "utf-8", "expected_hash": old["sha256"]}))
        history = self.execute("fs_history", {"root": self.shared, "path": self.prefix + "/history.txt"})
        self.assertEqual(history[0]["actor_id"], "token:" + token["id"])
        recycled = self.task("fs_delete", {"root": self.shared, "path": path})
        self.remote("systemctl restart powerlinux-agent.service")
        self.agent_ready()
        self.assertEqual(self.read(self.shared, "history.txt")["content"], "native Agent external AI author")
        ids = [r["id"] for r in self.execute("fs_trash_list", {"root": self.shared})]
        self.assertIn(recycled["id"], ids)
        self.task("fs_restore", {"root": self.shared, "id": recycled["id"]})
        status, body, _ = binary_request(self.api, "GET", "/api/v1/files/download?" + query)
        self.assertEqual((status, hashlib.sha256(body).digest()), (200, hashlib.sha256(payload).digest()))
        data(self.api.delete("/api/v1/tokens/" + token["id"]))
        client = APIClient(self.fixture["url"], token["token"])
        self.assertEqual(client.post("/api/v1/actions/fs_read", {"root": self.shared, "path": self.prefix + "/history.txt"})[0], 401)


if __name__ == "__main__":
    unittest.main(verbosity=2)
