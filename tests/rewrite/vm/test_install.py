#!/usr/bin/env python3
"""Production installer checks in the real, disposable systemd QEMU guest."""
from __future__ import annotations
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from http_client import APIClient, data


def main():
    archive = Path(os.environ["POWERLINUX_TEST_ARCHIVE"]).resolve()
    guest = Path(os.environ.get("POWERLINUX_QA_VM_DIR", "/workspace/tools/qa-vm/guest"))
    installer = Path(__file__).resolve().parents[3] / "scripts/rewrite/install.sh"
    ssh = ["ssh", "-F", "/dev/null", "-i", str(guest / "id_ed25519"), "-o", "IdentitiesOnly=yes",
           "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={guest / 'known_hosts'}",
           "-o", "ConnectTimeout=30", "-p", "19022", "root@127.0.0.1"]
    scp = ["scp", "-F", "/dev/null", "-i", str(guest / "id_ed25519"), "-o", "IdentitiesOnly=yes",
           "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={guest / 'known_hosts'}", "-P", "19022"]
    def remote(command, timeout=90):
        return subprocess.check_output(ssh + [command], timeout=timeout).decode()
    identity = remote("cat /proc/1/comm; systemd-detect-virt; uname -r")
    assert identity.splitlines()[:2] == ["systemd", "qemu"], identity
    # Pin a protected-file set. Content is hashed, never printed.
    snapshot_command = "for f in /bin/sh /etc/default/locale /etc/locale.gen /etc/hosts /etc/resolv.conf /etc/profile /etc/motd; do test ! -f \"$f\" || sha256sum \"$f\"; done; find /etc/cron.d /etc/cron.daily /var/spool/cron -type f -exec sha256sum {} + 2>/dev/null || true"
    before = remote(snapshot_command)
    assert remote("test ! -e /opt/powerlinux && test ! -e /var/lib/powerlinux && test ! -e /etc/systemd/system/powerlinux.service && echo clean").strip() == "clean"
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    subprocess.run(scp + [str(archive), "root@127.0.0.1:/tmp/powerlinux-release.tar.gz"], check=True, timeout=120)
    subprocess.run(scp + [str(installer), "root@127.0.0.1:/tmp/powerlinux-install.sh"], check=True, timeout=60)
    install = f"bash /tmp/powerlinux-install.sh --archive /tmp/powerlinux-release.tar.gz --sha256 {digest}"
    first = remote(install, timeout=150)
    state = remote("systemctl is-active powerlinux.service; systemctl show powerlinux.service --property=User --value; ps -o user= -p $(systemctl show powerlinux.service -p MainPID --value); curl --noproxy '*' -fsS http://127.0.0.1:9000/healthz")
    assert [line.strip() for line in state.splitlines()[:3]] == ["active", "powerlinux", "powerlinux"], state
    health = json.loads(state.splitlines()[3])
    assert health["status"] == "ok" and health["ready"] is True
    second = remote(install, timeout=150)
    assert before == remote(snapshot_command), "installer modified protected global settings"
    # Securely capture the local bootstrap token without writing it to console.
    bootstrap = remote("cat /var/lib/powerlinux/bootstrap-token").strip()
    forward = subprocess.Popen(ssh[:-1] + ["-o", "ExitOnForwardFailure=yes", "-N", "-L", "19900:127.0.0.1:9000", ssh[-1]],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        client = APIClient("http://127.0.0.1:19900")
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                if client.get("/healthz")[0] == 200:
                    break
            except OSError:
                pass
            time.sleep(.2)
        password = "QA-" + secrets.token_urlsafe(24)
        data(client.post("/api/v1/auth/setup", {"username": "vm-admin", "password": password, "setup_token": bootstrap}))
        session = data(client.post("/api/v1/auth/login", {"username": "vm-admin", "password": password}))
        client.csrf = session["csrf_token"]
        note = data(client.post("/api/v1/notes", {"title": "VM persisted note", "content": "real production service", "target_id": "host:local"}))
        remote("systemctl restart powerlinux.service", timeout=90)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                if client.get("/healthz")[0] == 200:
                    break
            except OSError:
                pass
            time.sleep(.2)
        assert data(client.get("/api/v1/notes/" + note["id"]))["content"] == "real production service"
        remote("bash /tmp/powerlinux-install.sh --uninstall", timeout=90)
        assert remote("test -f /var/lib/powerlinux/powerlinux.db && test ! -e /etc/systemd/system/powerlinux.service && test ! -e /opt/powerlinux/current && echo retained").strip() == "retained"
        assert before == remote(snapshot_command), "uninstall modified protected global settings"
        evidence = {"platform": "Debian13 amd64", "virtualization": "QEMU TCG", "pid1": "systemd", "kernel": identity.splitlines()[2],
                    "archive_sha256": digest, "version": health["version"], "checks": ["clean production install", "ordinary-user service", "health", "repeated install",
                    "protected global-file hashes unchanged", "shared note real API", "service restart persists state", "uninstall retains data"],
                    "not_yet_verified": ["different-version upgrade+failed health rollback", "guest reboot", "Ubuntu", "arm64"]}
        (guest / "installer-evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
        print(json.dumps(evidence))
    finally:
        forward.terminate()
        forward.wait(timeout=10)


if __name__ == "__main__":
    main()
