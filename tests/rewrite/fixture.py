"""Launch a real compiled server in an isolated private temporary directory."""
from __future__ import annotations
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time

from http_client import APIClient, data


class RunningServer:
    def __init__(self, binary: str):
        self.binary = str(Path(binary).resolve())
        self.directory = tempfile.TemporaryDirectory(prefix="powerlinux-integration-")
        self.data_dir = Path(self.directory.name) / "data"
        self.log_file = open(Path(self.directory.name) / "server.log", "wb")
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            self.port = sock.getsockname()[1]
        self.url = f"http://127.0.0.1:{self.port}"
        self.start()
        self.bootstrap = (self.data_dir / "bootstrap-token").read_text().strip()

    def start(self):
        self.process = subprocess.Popen([
            self.binary, "serve", "--data-dir", str(self.data_dir),
            "--listen", f"127.0.0.1:{self.port}", "--public-url", self.url + "/mcp",
            "--allowed-origins", self.url,
        ], stdout=self.log_file, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                self.log_file.flush()
                diagnostic = (Path(self.directory.name) / "server.log").read_text()[-2048:]
                self.close()
                raise AssertionError("Compiled server exited before health check: " + diagnostic)
            try:
                response = APIClient(self.url).get("/healthz")
                if response[0] == 200:
                    break
            except OSError:
                pass
            time.sleep(0.1)
        else:
            self.log_file.flush()
            diagnostic = (Path(self.directory.name) / "server.log").read_text()[-2048:]
            self.close()
            raise AssertionError("Compiled server did not become healthy within 20 seconds: " + diagnostic)
    def restart(self):
        self.process.terminate()
        self.process.wait(timeout=12)
        self.start()

    def close(self):
        if getattr(self, "process", None) and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        if getattr(self, "log_file", None):
            self.log_file.close()
        self.directory.cleanup()
