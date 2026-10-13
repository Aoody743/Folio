#!/usr/bin/env python3
"""Deploy the verified dev9 baseline beside preserved, uninstalled P1 data.

Explicitly authorized QA fixture setup, only for the retained owned Debian VM.
No SQLite rewrite, authentication bypass or credential output. Preserve the
old data via same-filesystem rename; future package upgrades use the new
normally initialized production fixture continuously.
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import socket
import subprocess
import sys
import tarfile
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from http_client import APIClient, data


def main():
    guest = Path("/workspace/tools/qa-vm/guest")
    helper = str(guest / "ssh.sh")
    evidence = Path("/workspace/tools/qa-vm/agent-acceptance-dev9")
    evidence.mkdir(mode=0o700)
    archive = Path("/workspace/.cache/rewrite-release/0.1.0-dev.9/powerlinux-0.1.0-dev.9-linux-amd64.tar.gz")
    digest = "ca7d61746eb3023c93c79265feb4b350d869e8bc2a56c230b08f79211ab1afb1"
    assert hashlib.sha256(archive.read_bytes()).hexdigest() == digest
    with tarfile.open(archive, "r:gz") as package:
        members = package.getmembers()
        assert len(members) == 6 and all(m.isfile() for m in members)
        assert {m.name for m in members} == {"powerlinux", "VERSION", "PLATFORM", "BUILDINFO", "README.txt", "install.sh"}
        assert package.extractfile("VERSION").read().strip() == b"0.1.0-dev.9"
        installer = package.extractfile("install.sh").read()
    runtime = "/root/powerlinux-qa-baseline-dev9"
    retained = "/var/lib/powerlinux-qa-p1-retained-20261013"
    def remote(command, *, timeout=50, label=None):
        result = subprocess.run([helper, command], capture_output=True, timeout=timeout)
        if label:
            target = evidence / (label + ".log")
            target.write_bytes(result.stdout + result.stderr)
            target.chmod(0o600)
        if result.returncode:
            raise RuntimeError("Owned Debian command failed" + (": " + label if label else ""))
        return result.stdout
    def put(path, content):
        result = subprocess.run([helper, "umask 077; cat > " + shlex.quote(path)],
                                input=content, capture_output=True, timeout=150)
        if result.returncode:
            raise RuntimeError("Strict fixture transfer failed")
    remote("set -eu; test \"$(id -u)\" = 0; test \"$(cat /proc/1/comm)\" = systemd; "
           "test \"$(systemd-detect-virt)\" = qemu; test ! -e /opt/powerlinux/current; "
           "! systemctl is-active --quiet powerlinux.service; ! systemctl is-active --quiet powerlinux-agent.service; "
           "test ! -e /etc/systemd/system/powerlinux.service; test ! -e /etc/systemd/system/powerlinux-agent.service; "
           "test -f /var/lib/powerlinux/powerlinux.db; test ! -e " + retained, label="original-identity-and-uninstalled-state")
    filenames = remote("find /root /var/tmp /tmp -maxdepth 4 -type f \\( -iname '*credential*' -o -iname '*panel*private*' -o -iname '*password*' \\) -print 2>/dev/null", label="existing-private-credential-filenames-only")
    assert not filenames.strip(), "Inspect an existing private credential filename before separating the old fixture"
    metadata = remote("set -eu; stat -c '%d:%i %a %u:%g %n' /var/lib/powerlinux; "
           "find /var/lib/powerlinux -printf '%m %u %g %s %p\\n'; "
           "find /var/lib/powerlinux -type f -exec sha256sum -- {} +", label="original-p1-data-metadata-and-hashes")
    policy = json.loads(Path("/workspace/tools/qa-ubuntu/agent-acceptance/qa-agent-policy-v2-dev8.json").read_text())
    policy["files"]["protected_paths"].append(retained)
    policy_bytes = (json.dumps(policy, ensure_ascii=False, indent=2) + "\n").encode()
    local_policy = evidence / "reviewed-debian-agent-policy-dev9.json"
    local_policy.write_bytes(policy_bytes)
    local_policy.chmod(0o600)
    remote("set -eu; install -d -m 0700 " + runtime + "; install -d -m 0755 /srv/powerlinux-qa-files", label="prepare-owned-qa-roots")
    put(runtime + "/candidate.tar.gz", archive.read_bytes())
    put(runtime + "/install.sh", installer)
    put(runtime + "/policy.json", policy_bytes)
    remote("set -eu; printf '%s  %s\\n' '" + digest + "' '" + runtime +
           "/candidate.tar.gz' | sha256sum --check --status; "
           "flock -n /var/lib/powerlinux/server.lock true; mv /var/lib/powerlinux " + retained +
           "; stat -c '%d:%i %a %u:%g %n' " + retained, label="same-inode-preserved-p1-data-rename")
    assert metadata.splitlines()[0].split()[:3] == remote("stat -c '%d:%i %a %u:%g' " + retained).strip().split()
    command = "bash " + runtime + "/install.sh --archive " + runtime + "/candidate.tar.gz --sha256 " + digest + \
              " --enable-agent --agent-policy " + runtime + "/policy.json --enable-terminal --enable-software --enable-sites"
    remote(command, timeout=240, label="actual-immutable-dev9-production-deployment")
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        local_port = sock.getsockname()[1]
    public_url = "http://127.0.0.1:" + str(local_port)
    server_config = json.loads(remote("cat /etc/powerlinux/server.json"))
    server_config["public_url"] = public_url + "/mcp"
    server_config["allowed_origins"] = [public_url]
    put(runtime + "/server.json", (json.dumps(server_config, indent=2) + "\n").encode())
    remote("set -eu; systemctl stop powerlinux.service; install -m 0640 -o root -g powerlinux " + runtime +
           "/server.json /etc/powerlinux/server.json; systemctl start powerlinux.service", label="actual-managed-public-origin-config")
    connection = ["ssh", "-F", "/dev/null", "-i", str(guest / "id_ed25519"),
        "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
        "-o", "UserKnownHostsFile=" + str(guest / "known_hosts"), "-o", "ExitOnForwardFailure=yes", "-p", "19022"]
    tunnel_log = (evidence / "strict-panel-forward-private.log").open("ab")
    tunnel = subprocess.Popen(connection + ["-N", "-L", "127.0.0.1:" + str(local_port) + ":127.0.0.1:9000", "root@127.0.0.1"],
                              stdout=subprocess.DEVNULL, stderr=tunnel_log, start_new_session=True)
    api = APIClient(public_url)
    deadline = time.monotonic() + 40
    while time.monotonic() < deadline:
        assert tunnel.poll() is None, "Strict panel SSH forward failed"
        try:
            status, health, _ = api.get("/healthz")
            if status == 200 and health["version"] == "0.1.0-dev.9" and health["ready"] is True:
                break
        except OSError:
            pass
        time.sleep(.2)
    else:
        raise AssertionError("Actual Debian dev9 API failed health")
    bootstrap = remote("cat /var/lib/powerlinux/bootstrap-token").decode().strip()
    password = "QA-" + secrets.token_urlsafe(24)
    username = "debian-v2-qa"
    data(api.post("/api/v1/auth/setup", {"username": username, "password": password, "setup_token": bootstrap}))
    session = data(api.post("/api/v1/auth/login", {"username": username, "password": password}))
    api.csrf = session["csrf_token"]
    note = data(api.post("/api/v1/notes", {
        "title": "Debian native owned lifecycle baseline", "content": "Continuous dev9+ fixture with P1 private data preserved separately",
        "target_id": "host:local",
    }))
    remote("set -eu; test ! -e /var/lib/powerlinux/bootstrap-token; "
        "/opt/powerlinux/current/powerlinux healthcheck --server-config /etc/powerlinux/server.json --expected-version 0.1.0-dev.9 --timeout 3s; "
        "runuser -u powerlinux -- /opt/powerlinux/current/powerlinux agent-healthcheck --socket /run/powerlinux-agent/agent.sock "
        "--expected-version 0.1.0-dev.9 --expected-policy-sha256 " + hashlib.sha256(policy_bytes).hexdigest() + " --timeout 3s", label="actual-api-and-authenticated-agent-health")
    private = {"url": public_url, "username": username, "password": password,
        "tunnel_pid": tunnel.pid, "guest": str(guest), "ssh_port": 19022,
        "version": "0.1.0-dev.9", "archive_sha256": digest, "note_id": note["id"]}
    path = evidence / "panel-fixture-private.json"
    path.write_text(json.dumps(private, indent=2) + "\n")
    path.chmod(0o600)
    proof = {"actual_version": health["version"], "archive_sha256": digest,
        "policy_sha256": hashlib.sha256(policy_bytes).hexdigest(), "retained_p1_data": retained,
        "original_data_metadata_sha256": hashlib.sha256(metadata).hexdigest(),
        "checks": ["strict actual immutable package", "uninstalled original P1 data preserved same inode and owner/mode", "normally initialized new private QA account",
                   "original password/MFA/DB not altered", "actual API health", "actual ordinary-UID Unix Agent health", "real shared human note"],
        "private_panel_fixture": str(path), "production_baseline": "Continuous new dev9 fixture for future dev10 upgrade",
        "cleanup_plan": "Stop API/Agent before any data switch; retain new QA data under a private separately protected name, uninstall only PowerLinux API/Agent if desired, then rename original P1 data to /var/lib/powerlinux. Preserve latest managed software/site ledger and owned services for deliberate recovery; never delete either data tree or copy an old root ledger over the current host.",
        "limits": ["Fresh authorized QA identity alongside retained P1 state; does not prove original P1 credential continuity", "No native software lifecycle, new DB operations or ARM proof yet"]}
    (evidence / "debian-baseline-evidence.json").write_text(json.dumps(proof, indent=2) + "\n")
    print(json.dumps(proof))


if __name__ == "__main__":
    main()
