"""Small independent MCP wire clients for integration verification, not runtime use."""
from __future__ import annotations

import json
import os
import select
import subprocess
import time
import urllib.error
import urllib.request


class HTTPMCPClient:
    def __init__(self, endpoint: str, token: str):
        self.endpoint = endpoint
        self.token = token
        self.seq = 0
        self.protocol_version = "2025-06-18"

    def request(self, method: str, params=None, *, origin=None):
        self.seq += 1
        body = {"jsonrpc": "2.0", "id": self.seq, "method": method}
        if params is not None:
            body["params"] = params
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
            "Authorization": f"Bearer {self.token}",
            "MCP-Protocol-Version": self.protocol_version,
        }
        if origin:
            headers["Origin"] = origin
        request = urllib.request.Request(self.endpoint, json.dumps(body).encode(), headers)
        try:
            with urllib.request.urlopen(request, timeout=15) as response:
                raw = response.read()
                mime = response.headers.get("Content-Type", "")
        except urllib.error.HTTPError as exc:
            return exc.code, exc.read()
        if "text/event-stream" in mime:
            for line in raw.decode().splitlines():
                if line.startswith("data:"):
                    payload = json.loads(line[5:].strip())
                    if payload.get("id") == self.seq:
                        return 200, payload
            raise AssertionError("MCP SSE response did not contain matching JSON-RPC id")
        payload = json.loads(raw)
        assert payload.get("id") == self.seq, payload
        return 200, payload

    def initialize(self):
        status, payload = self.request("initialize", {
            "protocolVersion": self.protocol_version,
            "capabilities": {},
            "clientInfo": {"name": "powerlinux-independent-qa", "version": "1"},
        })
        assert status == 200 and "result" in payload, payload
        self.protocol_version = payload["result"]["protocolVersion"]
        return payload["result"]

    def call(self, name: str, arguments: dict):
        status, payload = self.request("tools/call", {"name": name, "arguments": arguments})
        assert status == 200, status
        assert "result" in payload, payload
        return payload["result"]


class StdioMCPClient:
    def __init__(self, command: list[str], env: dict | None = None):
        self.process = subprocess.Popen(command, stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                        env=env or os.environ.copy(), bufsize=0)
        self.seq = 0
        self.buffer = b""

    def request(self, method: str, params=None):
        self.seq += 1
        body = {"jsonrpc": "2.0", "id": self.seq, "method": method}
        if params is not None:
            body["params"] = params
        self.process.stdin.write(json.dumps(body).encode() + b"\n")
        self.process.stdin.flush()
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if b"\n" not in self.buffer:
                ready, _, _ = select.select([self.process.stdout], [], [],
                                           max(0, deadline - time.monotonic()))
                if not ready:
                    break
                chunk = os.read(self.process.stdout.fileno(), 65536)
                if not chunk:
                    raise AssertionError("stdio MCP exited before its response")
                self.buffer += chunk
            while b"\n" in self.buffer:
                line, self.buffer = self.buffer.split(b"\n", 1)
                payload = json.loads(line)  # logs on stdout are a protocol failure
                if payload.get("id") == self.seq:
                    return payload
        raise AssertionError(f"stdio MCP timed out for {method}")

    def initialize(self):
        payload = self.request("initialize", {
            "protocolVersion": "2025-06-18", "capabilities": {},
            "clientInfo": {"name": "powerlinux-independent-qa", "version": "1"},
        })
        assert "result" in payload, payload
        self.process.stdin.write(b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n')
        self.process.stdin.flush()
        return payload["result"]

    def close(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                self.process.wait(timeout=5)
        self.process.stdout.close()
        self.process.stderr.close()


def tool_data(result: dict):
    """Only structured success data is evidence that a tool action succeeded."""
    assert not result.get("isError"), result
    payload = result.get("structuredContent")
    assert isinstance(payload, dict) and "data" in payload, result
    return payload["data"]
