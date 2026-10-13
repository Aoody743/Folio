#!/usr/bin/env python3
"""Serialized real-package upgrade/recovery checks in an owned QEMU VM.

The candidate archive and health executable remain unchanged. A QA-owned API
drop-in can occupy the actual configured port for only the candidate version;
the prior version executes its real original argv. This creates an actual bind
failure before Agent startup, without replacing health results. It must never
run concurrently with another guest mutation owner.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import stat
import subprocess
import sys
import tarfile
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from http_client import APIClient, data


STATE_SCRIPT = r'''
import hashlib,json,os
from pathlib import Path
base=Path('/opt/powerlinux')
def digest(path):
    p=Path(path)
    return hashlib.sha256(p.read_bytes()).hexdigest() if p.is_file() else None
def files(path):
    p=Path(path)
    return {str(f.relative_to(p)):digest(f) for f in sorted(p.rglob('*')) if f.is_file() and not f.is_symlink()}
lock=Path('/var/lib/powerlinux/server.lock').stat()
print(json.dumps({
 'version':(base/'current/VERSION').read_text().strip(),
 'target':os.readlink(base/'current'),
 'policy_sha256':digest('/etc/powerlinux/agent-policy.json'),
 'server_config_sha256':digest('/etc/powerlinux/server.json'),
 'api_unit_sha256':digest('/etc/systemd/system/powerlinux.service'),
 'agent_unit_sha256':digest('/etc/systemd/system/powerlinux-agent.service'),
 'data_lock_inode':[lock.st_dev,lock.st_ino],
 'database_sha256':digest('/var/lib/powerlinux/powerlinux.db'),
 'backups':sorted(p.name for p in (base/'backups').iterdir() if p.is_dir()),
 'pending':(base/'pending-transaction').read_text().strip() if (base/'pending-transaction').exists() else None,
 'needs_recovery':(base/'agent-recovery-required').exists(),
 'ledger':files('/var/lib/powerlinux-agent'),
}))
'''

FAULT_SCRIPT = r'''
import json,os,socket,subprocess,sys
from pathlib import Path
candidate=sys.argv[1]
argv=sys.argv[2:]
if subprocess.check_output([argv[0],'version'],text=True).strip()!=candidate:
    os.execv(argv[0],argv)
config=json.loads(Path('/etc/powerlinux/server.json').read_text())
host,_,port=config['listen'].rpartition(':')
host=host.strip('[]')
family=socket.AF_INET6 if ':' in host else socket.AF_INET
with socket.socket(family,socket.SOCK_STREAM) as occupied:
    occupied.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
    occupied.bind((host,int(port)))
    occupied.listen(1)
    print('QA candidate-only real configured-port occupancy',flush=True)
    result=subprocess.run(argv,check=False)
    sys.exit(result.returncode if result.returncode else 1)
'''


class Acceptance:
    def __init__(self, args):
        self.args = args
        self.archive = Path(args.archive).resolve()
        self.digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        if self.digest != args.sha256:
            raise RuntimeError("Candidate archive failed the independently supplied trusted SHA256")
        with tarfile.open(self.archive, "r:gz") as package:
            members = package.getmembers()
            expected = {"powerlinux", "VERSION", "PLATFORM", "install.sh", "BUILDINFO", "README.txt"}
            if len(members) != 6 or {m.name for m in members} != expected or not all(m.isfile() for m in members):
                raise RuntimeError("Candidate must have exactly six distinct flat regular release files")
            self.version = package.extractfile("VERSION").read().decode().strip()
            if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,100}", self.version):
                raise RuntimeError("Unsafe candidate version")
            if package.extractfile("PLATFORM").read().decode().strip() != "linux-amd64":
                raise RuntimeError("This actual VM acceptance requires the amd64 package")
            self.installer_bytes = package.extractfile("install.sh").read()
        credential_file = Path(args.credentials)
        if credential_file.is_symlink() or not credential_file.is_file() or stat.S_IMODE(credential_file.stat().st_mode) & 0o077:
            raise RuntimeError("Panel fixture must be a private regular credential file")
        self.credentials = json.loads(credential_file.read_text())
        if not re.fullmatch(r"http://127\.0\.0\.1:[0-9]+", self.credentials["url"]):
            raise RuntimeError("Use the explicit local strict SSH API forward")
        self.run_id = secrets.token_hex(6)
        self.directory = "/root/powerlinux-qa-upgrade-" + self.run_id
        self.fault_directory = "/opt/powerlinux/qa-upgrade-fault-" + self.run_id
        self.dropin = "/etc/systemd/system/powerlinux.service.d/99-powerlinux-qa-upgrade-" + self.run_id + ".conf"
        self.lock_unit = "powerlinux-qa-data-lock-" + self.run_id + ".service"
        self.output = Path(args.output).resolve()
        self.output.parent.mkdir(parents=True, exist_ok=True)
        self.artifacts = self.output.parent / ("agent-upgrade-" + self.run_id)
        self.artifacts.mkdir(mode=0o700)
        self.steps = []
        self.failure = None
        self.api = APIClient(self.credentials["url"])
        self.note = None
        self.dropin_installed = False
        self.dropin_parent_created = False
        self.command_index = 0

    def remote(self, script, *, timeout=50, allow_failure=False, label="command"):
        result = subprocess.run([self.args.ssh_helper, "bash", "-s"],
            input=("set -Eeuo pipefail\n" + script).encode(), capture_output=True, timeout=timeout)
        self.command_index += 1
        path = self.artifacts / (f"{self.command_index:03d}-" + label + ".log")
        path.write_bytes(result.stdout + result.stderr)
        path.chmod(0o600)
        if result.returncode and not allow_failure:
            raise RuntimeError("Owned VM command failed: " + label + "; private diagnostics retained")
        return result

    def python(self, source, *, label="python", timeout=50):
        return self.remote("python3 - <<'QA_PYTHON'\n" + source + "\nQA_PYTHON", label=label, timeout=timeout)

    def put(self, path, content):
        command = "umask 077; cat > " + shlex.quote(path)
        result = subprocess.run([self.args.ssh_helper, command], input=content, capture_output=True, timeout=120)
        if result.returncode:
            raise RuntimeError("Strict SSH fixture transfer failed")

    def state(self, label):
        state = json.loads(self.python(STATE_SCRIPT, label=label).stdout)
        path = self.artifacts / (f"{self.command_index:03d}-" + label + ".json")
        path.write_text(json.dumps(state, indent=2) + "\n")
        path.chmod(0o600)
        return state

    def login(self):
        session = data(self.api.post("/api/v1/auth/login", {
            "username": self.credentials["username"], "password": self.credentials["password"],
        }))
        self.api.csrf = session["csrf_token"]

    def health(self, version, *, agent=True):
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            try:
                status, body, _ = self.api.get("/healthz")
                if status == 200 and body.get("ready") is True and body.get("version") == version:
                    break
            except OSError:
                pass
            time.sleep(0.2)
        else:
            raise AssertionError("Real restored API health did not match expected version")
        self.remote("/opt/powerlinux/current/powerlinux healthcheck --server-config /etc/powerlinux/server.json --expected-version " +
                    shlex.quote(version) + " --timeout 3s", label="actual-api-health-" + version)
        if agent:
            self.remote("policy_hash=$(sha256sum /etc/powerlinux/agent-policy.json); policy_hash=${policy_hash%% *}\n"
                "runuser -u powerlinux -- /opt/powerlinux/current/powerlinux agent-healthcheck "
                "--socket /run/powerlinux-agent/agent.sock --expected-version " + shlex.quote(version) +
                " --expected-policy-sha256 \"$policy_hash\" --timeout 3s", label="actual-agent-health-" + version)
        if self.note:
            self.login()
            actual = data(self.api.get("/api/v1/notes/" + self.note["id"]))
            assert actual["content"] == self.note["content"], "Pre-upgrade real human note was lost"

    def command(self, policy="good-policy.json"):
        return "bash " + shlex.quote(self.directory + "/install.sh") + " --archive " + \
            shlex.quote(self.directory + "/candidate.tar.gz") + " --sha256 " + self.digest + \
            " --enable-agent --agent-policy " + shlex.quote(self.directory + "/" + policy)

    def install(self, label, *, policy="good-policy.json", fail=False):
        result = self.remote(self.command(policy), label=label, timeout=240, allow_failure=fail)
        if fail:
            assert result.returncode != 0, "Faulted actual installation unexpectedly reported success"
        return result

    def prepare(self):
        identity = self.remote("test \"$(id -u)\" = 0\n"
            "test \"$(cat /proc/1/comm)\" = systemd\n"
            "test \"$(systemd-detect-virt)\" = qemu\n"
            "test -f /opt/powerlinux/.powerlinux-install\n"
            "test -f /etc/powerlinux/agent-policy.json\n"
            "test ! -e /opt/powerlinux/pending-transaction\n"
            "test ! -e /opt/powerlinux/agent-recovery-required\n"
            "uname -r\n. /etc/os-release; echo \"$ID $VERSION_ID\"", label="identity")
        self.platform = identity.stdout.decode().splitlines()
        self.baseline = self.state("initial-state")
        assert self.baseline["version"] == self.args.old_version and self.version != self.args.old_version, \
            "Use a distinct immutable candidate and the actual original deployment version"
        self.health(self.args.old_version)
        self.login()
        self.note = data(self.api.post("/api/v1/notes", {
            "title": "Real upgrade QA " + self.run_id, "content": "pre-upgrade human note " + self.run_id,
            "target_id": "host:local",
        }))
        self.remote("install -d -m 0700 " + shlex.quote(self.directory), label="prepare-directory")
        self.put(self.directory + "/candidate.tar.gz", self.archive.read_bytes())
        self.put(self.directory + "/install.sh", self.installer_bytes)
        self.remote("printf '%s  %s\\n' " + shlex.quote(self.digest) + " " +
            shlex.quote(self.directory + "/candidate.tar.gz") + " | sha256sum --check --status\n"
            "cp -a /etc/powerlinux/agent-policy.json " + shlex.quote(self.directory + "/good-policy.json"),
            label="strict-guest-package-pin")
        self.python("from pathlib import Path\nimport json\np=Path(" + repr(self.directory + "/good-policy.json") + ")\n"
            "value=json.loads(p.read_text()); value['allowed_users']=['root']\n"
            "bad=Path(" + repr(self.directory + "/wrong-peer-policy.json") + ")\n"
            "bad.write_text(json.dumps(value,sort_keys=True)+'\\n');bad.chmod(0o600)", label="reviewed-peer-fault")
        self.steps.append({"check": "strict package/guest identity and actual initial API+Agent health", "result": "PASS"})

    def install_fault(self):
        exists = self.remote("test -d /etc/systemd/system/powerlinux.service.d", allow_failure=True, label="fault-parent-before")
        self.dropin_parent_created = exists.returncode != 0
        # Cleanup also covers a partially prepared fixture if setup fails.
        self.dropin_installed = True
        self.remote("test ! -e " + shlex.quote(self.dropin) + "\ninstall -d -m 0755 " +
            shlex.quote(self.fault_directory) + " /etc/systemd/system/powerlinux.service.d", label="fault-directory")
        self.put(self.fault_directory + "/candidate-port.py", FAULT_SCRIPT.encode())
        self.python("from pathlib import Path\nimport shlex,json\n"
            "lines=Path('/etc/systemd/system/powerlinux.service').read_text().splitlines()\n"
            "commands=[x.split('=',1)[1] for x in lines if x.startswith('ExecStart=')]\n"
            "assert len(commands)==1\nargv=shlex.split(commands[0])\n"
            "assert argv[:2]==['/opt/powerlinux/current/powerlinux','serve']\n"
            "fault=['/usr/bin/python3'," + repr(self.fault_directory + "/candidate-port.py") + "," +
            repr(self.version) + "]+argv\n"
            "assert all('\\n' not in a and '\\x00' not in a and '%' not in a for a in fault)\n"
            "# systemd accepts quoted argv; no shell expansion is used.\n"
            "drop=Path(" + repr(self.dropin) + ")\n"
            "drop.write_text('[Service]\\nExecStart=\\nExecStart='+ ' '.join(json.dumps(a) for a in fault)+'\\n')\n",
            label="fault-dropin-build")
        self.remote("chmod 0644 " + shlex.quote(self.fault_directory + "/candidate-port.py") + " " +
            shlex.quote(self.dropin) + "\nsystemctl daemon-reload", label="fault-enable")
        self.remote("sha256sum " + shlex.quote(self.dropin) + " " +
            shlex.quote(self.fault_directory + "/candidate-port.py"), label="actual-fault-fixture-hashes")

    def remove_fault(self):
        self.remote("rm -f -- " + shlex.quote(self.dropin) + "\nrm -f -- " +
            shlex.quote(self.fault_directory + "/candidate-port.py") + "\nrmdir -- " +
            shlex.quote(self.fault_directory) + " 2>/dev/null || true\nsystemctl daemon-reload", label="fault-remove")
        if self.dropin_parent_created:
            self.remote("rmdir /etc/systemd/system/powerlinux.service.d 2>/dev/null || true", label="fault-owned-empty-parent-remove")
        self.dropin_installed = False

    def assert_restored(self, before, after, *, policy_restored, ledger_expected=None):
        for key in ("target", "server_config_sha256", "api_unit_sha256", "agent_unit_sha256", "data_lock_inode"):
            assert after[key] == before[key], "Recovery changed original deployment property: " + key
        if policy_restored:
            assert after["policy_sha256"] == before["policy_sha256"], "Pre-Agent failure did not restore original policy"
        assert after["pending"] is None, "Healthy recovery did not clear its journal"
        ledger = before["ledger"] if ledger_expected is None else ledger_expected
        for name, digest in ledger.items():
            assert after["ledger"].get(name) == digest, "Latest pre-existing root ledger artifact was replaced: " + name

    def lock(self):
        self.remote("systemctl stop powerlinux.service", label="lock-stop-api")
        before = self.state("lock-before")
        try:
            holder = "exec 8</var/lib/powerlinux/server.lock; flock -n 8; exec sleep 60"
            self.remote("systemd-run --quiet --unit " + shlex.quote(self.lock_unit) +
                " --uid=powerlinux --property=RuntimeMaxSec=65 /bin/bash -c " + shlex.quote(holder), label="lock-holder")
            self.remote("for i in $(seq 1 30); do if ! flock -n /var/lib/powerlinux/server.lock true; then exit 0; fi; sleep 0.1; done; exit 1", label="lock-real-contention")
            result = self.install("lock-install-rejected", fail=True)
            assert b"production data lock" in result.stdout + result.stderr
            after = self.state("lock-after")
            for key in ("target", "database_sha256", "data_lock_inode", "backups", "pending"):
                assert after[key] == before[key], "Held production lock allowed modification: " + key
        finally:
            self.remote("systemctl stop " + shlex.quote(self.lock_unit) + " || true\n"
                "systemctl reset-failed " + shlex.quote(self.lock_unit) + " 2>/dev/null || true\n"
                "systemctl restart powerlinux-agent.service powerlinux.service", label="lock-release-restart")
        self.health(self.args.old_version)
        self.steps.append({"check": "real production data flock blocks DB replacement/snapshot and preserves inode", "result": "PASS"})

    def prestart(self):
        before = self.state("prestart-before")
        self.install_fault()
        try:
            self.install("prestart-actual-bind-failure", policy="wrong-peer-policy.json", fail=True)
            self.health(self.args.old_version)
            after = self.state("prestart-after")
            self.assert_restored(before, after, policy_restored=True)
            assert not after["needs_recovery"], "Pure pre-Agent failure unnecessarily left Agent recovery disabled"
            new = sorted(set(after["backups"]) - set(before["backups"]))
            assert len(new) == 1
            self.remote("test \"$(cat /opt/powerlinux/backups/" + new[0] +
                "/candidate-agent-state)\" = not_started", label="prestart-durable-not-started")
        finally:
            self.remove_fault()
        self.steps.append({"check": "actual candidate EADDRINUSE before Agent attempt restores original policy/units/API health", "result": "PASS"})

    def interrupted(self):
        before = self.state("interrupted-before")
        self.install_fault()
        try:
            runner = """import json,os,signal,subprocess,time
from pathlib import Path
command=COMMAND
log=open(LOG,'wb')
p=subprocess.Popen(command,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
deadline=time.monotonic()+60
base=Path('/opt/powerlinux')
try:
 while time.monotonic()<deadline:
  if p.poll() is not None: raise RuntimeError('Installer exited before controlled interruption')
  journal=base/'pending-transaction'
  if journal.exists() and (base/'current/VERSION').read_text().strip()==__QA_CANDIDATE_VERSION__:
   transaction=journal.read_text().strip()
   state=(base/transaction/'candidate-agent-state').read_text().strip()
   if state=='not_started':
    os.killpg(p.pid,signal.SIGKILL);p.wait(timeout=10)
    print(json.dumps({'installer_pid':p.pid,'signal':'SIGKILL','transaction':transaction,'agent_state':state,'exit':p.returncode}))
    break
  time.sleep(0.02)
 else: raise RuntimeError('No actual pre-Agent transaction observed within 60 seconds')
finally:
 if p.poll() is None: os.killpg(p.pid,signal.SIGKILL);p.wait(timeout=10)
 log.close()
""".replace("COMMAND", repr(shlex.split(self.command("wrong-peer-policy.json")))).replace(
                    "LOG", repr(self.directory + "/interrupted-installer.log")).replace("__QA_CANDIDATE_VERSION__", repr(self.version))
            detail = json.loads(self.python(runner, label="actual-installer-sigkill", timeout=75).stdout)
            assert detail["agent_state"] == "not_started" and detail["exit"] == -9
            pending = self.state("interrupted-pending")
            assert pending["pending"] and pending["version"] == self.version
            result = self.install("interrupted-real-recovery", fail=True)
            assert b"Interrupted installation detected" in result.stdout + result.stderr
            self.health(self.args.old_version)
            self.assert_restored(before, self.state("interrupted-after"), policy_restored=True)
        finally:
            self.remove_fault()
        self.steps.append({"check": "SIGKILL actual installer after durable snapshot; next invocation recovers with real old health", "result": "PASS"})

    def postattempt(self):
        before = self.state("postattempt-before")
        # An attempted constructor may legitimately migrate the authoritative
        # host ledger. Capture it after actual candidate root-peer readiness,
        # before automatic recovery, rather than comparing with obsolete state.
        runner = '''import hashlib,json,os,signal,subprocess,time
from pathlib import Path
command=__QA_INSTALL_COMMAND__
version=__QA_CANDIDATE_VERSION__
log=open(__QA_INSTALL_LOG__,'wb')
base=Path('/opt/powerlinux')
policy_hash=hashlib.sha256(Path(__QA_PEER_POLICY__).read_bytes()).hexdigest()
p=subprocess.Popen(command,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
observed=None
deadline=time.monotonic()+150
try:
 while time.monotonic()<deadline and p.poll() is None:
  journal=base/'pending-transaction'
  if journal.exists() and (base/'current/VERSION').read_text().strip()==version:
   transaction=journal.read_text().strip()
   state=(base/transaction/'candidate-agent-state').read_text().strip()
   if state=='attempted':
    health=subprocess.run([str(base/'current/powerlinux'),'agent-healthcheck',
      '--socket','/run/powerlinux-agent/agent.sock','--expected-version',version,
      '--expected-policy-sha256',policy_hash,'--timeout','1s'],
      stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=3)
    if health.returncode==0:
     ledger=Path('/var/lib/powerlinux-agent')
     hashes={str(f.relative_to(ledger)):hashlib.sha256(f.read_bytes()).hexdigest()
       for f in sorted(ledger.rglob('*')) if f.is_file() and not f.is_symlink()}
     observed={'version':version,'transaction':transaction,'agent_state':state,
       'policy_sha256':policy_hash,'actual_root_peer_health_exit':health.returncode,
       'ledger':hashes}
     Path(__QA_READY_SNAPSHOT__).write_text(json.dumps(observed,sort_keys=True)+'\\n')
     break
  time.sleep(0.05)
 if observed is None: raise RuntimeError('Candidate actual root-peer readiness was not observed; latest-ledger gate unverified')
 result=p.wait(timeout=150)
 if result==0: raise RuntimeError('Faulted actual installation unexpectedly succeeded')
 observed['installer_exit']=result
 print(json.dumps(observed))
finally:
 if p.poll() is None: os.killpg(p.pid,signal.SIGTERM);p.wait(timeout=30)
 log.close()
'''
        replacements = {
            "__QA_INSTALL_COMMAND__": repr(shlex.split(self.command("wrong-peer-policy.json"))),
            "__QA_CANDIDATE_VERSION__": repr(self.version),
            "__QA_INSTALL_LOG__": repr(self.directory + "/postattempt-installer.log"),
            "__QA_PEER_POLICY__": repr(self.directory + "/wrong-peer-policy.json"),
            "__QA_READY_SNAPSHOT__": repr(self.directory + "/postattempt-candidate-ready.json"),
        }
        for key, value in replacements.items():
            runner = runner.replace(key, value)
        compile(runner, "postattempt-guest-runner", "exec")
        candidate = json.loads(self.python(runner, label="postattempt-actual-peer-failure", timeout=320).stdout)
        assert candidate["actual_root_peer_health_exit"] == 0 and candidate["installer_exit"] != 0
        self.health(self.args.old_version, agent=False)
        after = self.state("postattempt-after")
        self.assert_restored(before, after, policy_restored=False, ledger_expected=candidate["ledger"])
        assert after["policy_sha256"] == candidate["policy_sha256"]
        assert after["policy_sha256"] != before["policy_sha256"] and after["needs_recovery"]
        self.remote("cmp -s " + shlex.quote(self.directory + "/wrong-peer-policy.json") +
            " /etc/powerlinux/agent-policy.json\n! systemctl is-active --quiet powerlinux-agent.service\n"
            "! systemctl is-enabled --quiet powerlinux-agent.service", label="postattempt-retained-policy-disabled-agent")
        new = sorted(set(after["backups"]) - set(before["backups"]))
        assert len(new) == 1
        self.remote("test \"$(cat /opt/powerlinux/backups/" + new[0] +
            "/candidate-agent-state)\" = attempted", label="postattempt-durable-attempted")
        self.steps.append({"check": "actual API peer denial after Agent attempt retains policy/ledger, recovers API, disables Agent with marker", "result": "PASS"})

    def upgrade(self):
        before = self.state("success-before")
        self.install("actual-successful-upgrade")
        self.health(self.version)
        after = self.state("success-after")
        assert after["version"] == self.version and not after["needs_recovery"] and after["pending"] is None
        assert after["data_lock_inode"] == before["data_lock_inode"]
        assert after["policy_sha256"] == self.baseline["policy_sha256"]
        self.steps.append({"check": "actual distinct-version upgrade and explicit reviewed policy recovery, API+Agent health and shared note", "result": "PASS"})

    def rollback(self):
        before = self.state("rollback-before")
        self.login()
        later = data(self.api.post("/api/v1/notes", {
            "title": "post-upgrade QA " + self.run_id, "content": "snapshot boundary " + self.run_id,
            "target_id": "host:local",
        }))
        self.remote("bash " + shlex.quote(self.directory + "/install.sh") + " --rollback", label="actual-manual-rollback", timeout=240)
        self.health(self.args.old_version)
        after = self.state("rollback-after")
        assert after["data_lock_inode"] == before["data_lock_inode"]
        assert after["policy_sha256"] == before["policy_sha256"] and after["pending"] is None
        assert self.api.get("/api/v1/notes/" + later["id"])[0] == 404, "API rollback did not restore the actual pre-upgrade data snapshot"
        for name, digest in before["ledger"].items():
            assert after["ledger"].get(name) == digest, "Manual rollback replaced authoritative root ledger artifact: " + name
        self.steps.append({"check": "real manual rollback restores API binary/config/units/pre-upgrade note snapshot while retaining latest Agent policy/ledger", "result": "PASS"})
        # Return the reusable acceptance guest to the new verified package.
        self.upgrade()

    def run(self):
        try:
            self.prepare()
            for check in self.args.checks.split(","):
                print("VM upgrade stage: " + check, flush=True)
                getattr(self, check)()
        except Exception as error:
            self.failure = type(error).__name__ + ": " + str(error)
            raise
        finally:
            if self.dropin_installed:
                self.remove_fault()
            report = {
                "candidate_version": self.version, "candidate_archive_sha256": self.digest,
                "packaged_installer_sha256": hashlib.sha256(self.installer_bytes).hexdigest(),
                "actual_platform": getattr(self, "platform", []), "steps": self.steps, "failure": self.failure,
                "private_artifacts": str(self.artifacts),
                "fault_scope": "QA-owned drop-in and actual candidate-only occupied listen port; unchanged binary/archive/health; latest root ledger never reverted",
                "limits": ["Serialized owned QEMU amd64 VM only", "No nativeARM or external-provider proof"],
            }
            self.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
            self.output.chmod(0o600)
            print(json.dumps({"candidate_version": self.version, "steps": self.steps, "failure": self.failure,
                              "evidence": str(self.output)}, ensure_ascii=False), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--owned-vm", action="store_true", required=True)
    parser.add_argument("--exclusive", action="store_true", required=True)
    parser.add_argument("--ssh-helper", required=True)
    parser.add_argument("--credentials", required=True)
    parser.add_argument("--archive", required=True)
    parser.add_argument("--sha256", required=True)
    parser.add_argument("--old-version", required=True)
    parser.add_argument("--checks", default="lock,prestart,interrupted,postattempt,upgrade,rollback")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"[a-f0-9]{64}", args.sha256):
        parser.error("--sha256 must be the trusted lowercase SHA256")
    allowed = {"lock", "prestart", "interrupted", "postattempt", "upgrade", "rollback"}
    if not args.checks or any(c not in allowed for c in args.checks.split(",")):
        parser.error("Unknown or empty check selection")
    Acceptance(args).run()


if __name__ == "__main__":
    main()
