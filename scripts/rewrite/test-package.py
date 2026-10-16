#!/usr/bin/env python3
"""Run an actual packaged binary in a private prefix (not systemd/VM evidence)."""
import hashlib
from html.parser import HTMLParser
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
from urllib.error import URLError
from urllib.request import urlopen


class Scripts(HTMLParser):
    def __init__(self):
        super().__init__()
        self.sources = []

    def handle_starttag(self, tag, attrs):
        if tag == "script":
            source = dict(attrs).get("src")
            if source:
                self.sources.append(source)


def main():
    if len(sys.argv) != 2:
        raise SystemExit("Usage: python3 scripts/rewrite/test-package.py ARCHIVE")
    archive = Path(sys.argv[1]).resolve()
    sha = hashlib.sha256(archive.read_bytes()).hexdigest()
    with tempfile.TemporaryDirectory(prefix="powerlinux-package-smoke-") as temporary:
        # Exercise the installer shipped with this immutable binary instead of
        # silently mixing a newer checkout script with an older release.
        installer = Path(temporary) / "packaged-install.sh"
        with tarfile.open(archive, "r:gz") as package:
            members = [item for item in package if item.name == "install.sh"]
            if len(members) != 1 or not members[0].isfile() or members[0].size > 1 << 20:
                raise AssertionError("Package must contain one bounded regular installer")
            installer.write_bytes(package.extractfile(members[0]).read())
        installer.chmod(0o600)
        prefix = Path(temporary) / "powerlinux"
        base_command = ["bash", str(installer), "--prefix", str(prefix)]

        def install(*options, success=True):
            result = subprocess.run(base_command + list(options), text=True, capture_output=True, timeout=30)
            if (result.returncode == 0) != success:
                raise AssertionError(f"Installer outcome mismatch: {result.stdout}{result.stderr}")
            return result

        artifact_args = ["--archive", str(archive), "--sha256", sha]
        install(*artifact_args)
        install(*artifact_args)
        version = (prefix / "current/VERSION").read_text().strip()
        with socket.socket() as available:
            available.bind(("127.0.0.1", 0))
            port = available.getsockname()[1]
        endpoint = f"http://127.0.0.1:{port}"
        command = [str(prefix / "current/powerlinux"), "serve", "--data-dir", str(prefix / "data"), "--listen", f"127.0.0.1:{port}"]
        modern_config = b"healthcheck --server-config" in installer.read_bytes()
        if modern_config:
            command += ["--server-config", ""]
        if os.geteuid() == 0:
            command.append("--allow-root")  # Explicitly isolated test only.
        with (Path(temporary) / "server.log").open("w+") as log:
            process = subprocess.Popen(command, stdout=log, stderr=log)
            try:
                health = None
                for _ in range(150):
                    if process.poll() is not None:
                        raise AssertionError(f"Packaged server exited with {process.returncode}; inspect app startup")
                    try:
                        with urlopen(endpoint + "/healthz", timeout=1) as response:
                            health = json.load(response)
                        break
                    except URLError:
                        time.sleep(0.2)
                assert health == {"status": "ok", "version": version, "ready": True}, health
                # Typed deployment health CLI is part of phase-two artifacts.
                # Legacy immutable phase-one packages still validate HTTP above.
                if modern_config:
                    subprocess.run([str(prefix / "current/powerlinux"), "healthcheck", "--server-config", "", "--listen", f"127.0.0.1:{port}", "--expected-version", version, "--timeout", "3s"], check=True, capture_output=True, timeout=5)
                with urlopen(endpoint + "/api/v1/session", timeout=3) as response:
                    session = json.load(response)
                assert session["data"]["initialized"] is False, session
                assert session["data"]["authenticated"] is False, session
                with urlopen(endpoint + "/", timeout=3) as response:
                    page = response.read().decode()
                    assert "nonce-" in response.headers.get("Content-Security-Policy", "")
                parser = Scripts()
                parser.feed(page)
                assert parser.sources, "Packaged UI has no Next.js scripts (placeholder or incorrect embed)"
                assert "Folio" in page, "Packaged UI must identify the product"
                static = next(source for source in parser.sources if "/_next/static/" in source)
                with urlopen(endpoint + static, timeout=3) as response:
                    assert len(response.read()) > 100
                    assert "immutable" in response.headers.get("Cache-Control", "")
                refusal = install(*artifact_args, success=False)
                assert "Stop the prefix-mode server" in refusal.stdout + refusal.stderr
                assert (prefix / "data/powerlinux.db").is_file(), "Actual SQLite database missing"
                assert (prefix / "data/bootstrap-token").stat().st_mode & 0o077 == 0, "Bootstrap file must remain private"
            finally:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
                    raise AssertionError("Packaged server did not exit gracefully")
            assert process.returncode == 0, f"SIGTERM exit {process.returncode}"
        persistent = prefix / "data/package-retention-test"
        persistent.write_text("retained after uninstall")
        install(*artifact_args)
        install("--uninstall")
        assert persistent.read_text() == "retained after uninstall"
        assert (prefix / "data/powerlinux.db").is_file()
        print(f"Actual Linux package {version}: prefix install/repeat, HTTP health/session, embedded Next assets, live lock, SIGTERM, reinstall and retained-data uninstall passed. Systemd/VM and other architecture execution remain separate.")


if __name__ == "__main__":
    main()
