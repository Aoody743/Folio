#!/usr/bin/env python3
"""Explicit, serialized QA guest acceptance. Never run against a shared host.

Requires a running installed package, private panel credentials, and an existing
strict-host-key SSH helper for a disposable QEMU/systemd guest. Host actions go
through the actual HTTP registry, durable tasks, Unix Agent and native programs.
This script is not part of ordinary unit tests and is not run by --help.
"""
import argparse
from datetime import datetime, timedelta, timezone
import hashlib
from http.cookiejar import CookieJar
import json
import os
from pathlib import Path
import shlex
import ssl
import subprocess
import tarfile
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlparse
from urllib.request import build_opener, Request, HTTPCookieProcessor, HTTPSHandler, HTTPRedirectHandler
import uuid


class APIError(RuntimeError):
    def __init__(self, status, code):
        super().__init__(f"HTTP {status}: {code}")
        self.status, self.code = status, code


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        return None


class Acceptance:
    def __init__(self, args):
        self.args = args
        archive = Path(args.archive)
        with archive.open("rb") as source:
            self.archive_sha256 = hashlib.file_digest(source, "sha256").hexdigest()
        if self.archive_sha256 != args.sha256:
            raise RuntimeError("Immutable archive SHA-256 does not match the reviewed artifact")
        with tarfile.open(archive, "r:gz") as package:
            binaries = [item for item in package if item.name == "powerlinux"]
            if len(binaries) != 1 or not binaries[0].isfile():
                raise RuntimeError("Immutable archive requires one regular binary")
            with package.extractfile(binaries[0]) as source:
                self.binary_sha256 = hashlib.file_digest(source, "sha256").hexdigest()
            if package.extractfile("VERSION").read(256).decode().strip() != args.version:
                raise RuntimeError("Immutable archive version does not match the requested gate")
        self.installed_binary_sha256 = None
        path = Path(args.credentials)
        if path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077:
            raise RuntimeError("Credentials require a private regular file")
        self.credentials = json.loads(path.read_text())
        self.url = self.credentials["url"].rstrip("/")
        parsed = urlparse(self.url)
        if parsed.scheme not in ("http", "https") or parsed.hostname not in ("127.0.0.1", "localhost", "::1") or parsed.username or parsed.query or parsed.fragment or parsed.path:
            raise RuntimeError("The API endpoint must be the explicit local QA SSH forward")
        context = ssl.create_default_context()
        if args.ca_file:
            context.load_verify_locations(cafile=args.ca_file)
        self.http = build_opener(NoRedirect(), HTTPCookieProcessor(CookieJar()), HTTPSHandler(context=context))
        self.csrf = ""
        self.token = ""
        self.run_id = uuid.uuid4().hex[:12]
        self.state = "/var/lib/powerlinux-agent/systemops-vm-" + self.run_id
        self.units = [f"powerlinux-plugin-qasysops-{self.run_id}-{kind}.service" for kind in ("normal", "fail")]
        self.plans, self.scripts, self.schedules, self.results, self.tasks = [], [], [], [], []
        self.tokens = []
        self.prepared = False

    def guest(self, script, timeout=30):
        process = subprocess.run([self.args.ssh_helper, "bash", "-s"], input="set -Eeuo pipefail\n" + script,
                                 capture_output=True, text=True, timeout=timeout)
        if process.returncode:
            # Root policy and panel credentials are never included in errors.
            raise RuntimeError(f"Guest command failed with exit {process.returncode}")
        return process.stdout.strip()

    def request(self, path, data=None, method=None):
        headers = {"Origin": self.url, "Content-Type": "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        elif self.csrf:
            headers["X-CSRF-Token"] = self.csrf
        if data is not None:
            headers["Idempotency-Key"] = "vm-sysops-" + uuid.uuid4().hex
        request = Request(self.url + path, json.dumps(data).encode() if data is not None else None, headers, method=method)
        try:
            with self.http.open(request, timeout=6) as response:
                return json.load(response)
        except HTTPError as error:
            try:
                code = json.load(error)["error"]["code"]
            except (ValueError, KeyError, TypeError):
                code = "unexpected_response"
            raise APIError(error.code, code) from None

    def action(self, name, data):
        return self.request("/api/v1/actions/" + name, data)["data"]

    def login(self):
        session = self.request("/api/v1/auth/login", {"username": self.credentials["username"], "password": self.credentials["password"]})["data"]
        self.csrf = session["csrf_token"]

    def ready(self):
        until = time.monotonic() + 40
        while time.monotonic() < until:
            try:
                health = self.request("/healthz")
                if health.get("ready") is True and health.get("version") == self.args.version:
                    return
            except (URLError, APIError, OSError):
                pass
            time.sleep(0.25)
        raise RuntimeError("Actual API version/readiness did not recover")

    def task(self, task_id, expected="succeeded"):
        until = time.monotonic() + 45
        while time.monotonic() < until:
            try:
                item = self.action("tasks_get", {"id": task_id})
                if item["status"] not in ("queued", "running"):
                    result = item.get("result") or {}
                    self.tasks.append({"id": task_id, "operation": item.get("operation"), "status": item["status"],
                                       "exit_code": result.get("exit_code"), "canceled": result.get("canceled"),
                                       "timed_out": result.get("timed_out")})
                    if expected is not None and item["status"] != expected:
                        raise RuntimeError("Durable task " + task_id + " finished as " + item["status"])
                    return item
            except (URLError, OSError):
                pass
            time.sleep(0.2)
        raise RuntimeError("Durable task exceeded the acceptance deadline")

    def plan(self, change):
        item = self.action("system_change_plan", {"change": change})
        assert item["state"] == "planned"
        self.plans.append(item["id"])
        return item["id"]

    def apply(self, plan_id, expected="succeeded"):
        submitted = self.action("system_change_apply", {"id": plan_id})
        self.task(submitted["task_id"], expected)
        return self.action("system_change_get", {"id": plan_id})

    def rollback(self, plan_id):
        submitted = self.action("system_change_rollback", {"id": plan_id})
        self.task(submitted["task_id"])
        assert self.action("system_change_get", {"id": plan_id})["state"] == "rolled_back"

    def prepare(self):
        checks = self.args.checks
        self.guest("test \"$(id -u)\" = 0\ncase $(systemd-detect-virt) in qemu|kvm) ;; *) exit 81 ;; esac\ntest -d /run/systemd/system\ntest \"$(/opt/powerlinux/current/powerlinux version)\" = " + shlex.quote(self.args.version))
        installed_sha = self.guest("sha256sum /opt/powerlinux/current/powerlinux").split()[0]
        self.installed_binary_sha256 = installed_sha
        if installed_sha != self.binary_sha256:
            raise RuntimeError("Installed binary does not match the immutable archive bytes")
        programs = {"systemctl": "/usr/bin/systemctl"}
        kinds = ["service_state", "script_execution"]
        for check, program, path in (("ssh", "sshd", "/usr/sbin/sshd"), ("firewall", "nft", self.args.nft_path), ("timezone", "timedatectl", "/usr/bin/timedatectl"), ("ntp", "timedatectl", "/usr/bin/timedatectl"), ("power", "shutdown", "/usr/sbin/shutdown")):
            if check in checks:
                programs[program] = path
                kinds.append({"ssh": "ssh_config", "firewall": "firewall_rules"}.get(check, check))
        if "panel" in checks:
            kinds.append("panel_config")
        options = json.dumps({"state": self.state, "units": self.units, "programs": programs, "kinds": kinds})
        # The guest snapshots only its trusted policy/config and creates two
        # uniquely named test units. Real host receipts are never overwritten.
        self.guest("python3 - <<'PY'\nimport json,hashlib,os\nfrom pathlib import Path\noptions=json.loads(" + repr(options) + ")\n" + r'''
state=Path(options["state"])
state.mkdir(mode=0o700)
policy=Path('/etc/powerlinux/agent-policy.json')
baseline=policy.read_bytes()
(state/'baseline-policy.json').write_bytes(baseline)
(state/'baseline-policy.json').chmod(0o600)
(state/'baseline-server.json').write_bytes(Path('/etc/powerlinux/server.json').read_bytes())
(state/'baseline-server.json').chmod(0o600)
doc=json.loads(baseline)
profiles=[]
for unit,program in zip(options['units'],('/usr/bin/sleep 900','/usr/bin/false')):
    path=Path('/etc/systemd/system')/unit
    if path.exists() or path.is_symlink(): raise RuntimeError('fixture unit already exists')
    service_type='simple' if unit==options['units'][0] else 'oneshot'
    data=('# PowerLinux owned disposable VM acceptance\n[Unit]\nDescription=PowerLinux QA sysops\n[Service]\nType='+service_type+'\nUser=powerlinux-terminal\nExecStart='+program+'\n').encode()
    path.write_bytes(data); path.chmod(0o644)
    profiles.append({'id':'qa-'+('normal' if unit==options['units'][0] else 'fail'),'unit':unit,'ownership':'managed','unit_sha256':hashlib.sha256(data).hexdigest()})
profiles += [{'id':'ssh','unit':'ssh.service','ownership':'adopted','protected':True},{'id':'panel','unit':'powerlinux.service','ownership':'adopted','protected':True}]
doc['host']={'version':1,'state_dir':str(state),'api_user':'powerlinux','enabled_kinds':options['kinds'],'programs':options['programs'],'services':profiles,'protected_ports':[22,80,443,9000],'confirmation_seconds':30,'enable_root_scripts':False,'script_profiles':[{'id':'local','user':'powerlinux-terminal','shell':'/bin/bash','home':'/var/lib/powerlinux-terminal','work_dir':'/var/lib/powerlinux-terminal/workspace','timeout_seconds':30}]}
candidate=state/'candidate-policy.json'
candidate.write_text(json.dumps(doc)); candidate.chmod(0o600)
PY
systemctl daemon-reload
''', timeout=40)
        self.prepared = True
        self.guest("/opt/powerlinux/current/powerlinux agent --policy " + shlex.quote(self.state + "/candidate-policy.json") + " --check-policy\nsystemctl stop powerlinux-agent.service\ncp " + shlex.quote(self.state + "/candidate-policy.json") + " /etc/powerlinux/agent-policy.json\nchmod 0600 /etc/powerlinux/agent-policy.json\nsystemctl start powerlinux-agent.service", timeout=40)
        self.agent_health()
        self.ready()
        self.login()

    def agent_health(self):
        self.guest("policy_hash=$(sha256sum /etc/powerlinux/agent-policy.json); policy_hash=${policy_hash%% *}\nrunuser -u powerlinux -- /opt/powerlinux/current/powerlinux agent-healthcheck --socket /run/powerlinux-agent/agent.sock --expected-policy-sha256 \"$policy_hash\" --expected-version " + shlex.quote(self.args.version) + " --timeout 5s")

    def services(self):
        plan = self.plan({"kind": "service_state", "service_id": "qa-normal", "service_state": "start"})
        assert self.apply(plan)["state"] == "applied"
        self.guest("systemctl is-active --quiet " + self.units[0])
        self.rollback(plan)
        assert self.guest("systemctl show --property=ActiveState --value " + self.units[0]) == "inactive"
        failed = self.plan({"kind": "service_state", "service_id": "qa-fail", "service_state": "start"})
        # The native failing process is allowed to start; verification must
        # detect it and restore the pre-change inactive state.
        assert self.apply(failed, "failed")["state"] == "rolled_back"
        assert self.guest("systemctl show --property=ActiveState --value " + self.units[1]) == "inactive"

    def scripts(self):
        marker = "private-script-comment-" + self.run_id
        body = "id -u\nprintf 'SCRIPT_WORKFLOW_OK\\n'\nif test -r /var/lib/powerlinux/powerlinux.db || test -r /var/lib/powerlinux/encryption-key; then exit 99; fi\n# " + marker + "\n"
        script = self.action("scripts_save", {"name": "VM private script", "profile_id": "local", "body": body})
        self.scripts.append(script["id"])
        submitted = self.action("scripts_run", {"id": script["id"], "expected_revision": 1})
        task = self.task(submitted["task_id"])
        uid = self.guest("id -u powerlinux-terminal")
        logs = self.action("tasks_logs", {"id": task["id"]})
        assert logs["stdout"].splitlines() == [uid, "SCRIPT_WORKFLOW_OK"]
        assert marker not in json.dumps(task)
        updated = self.action("scripts_save", {"id": script["id"], "name": "VM private script", "profile_id": "local", "body": body, "expected_revision": 1})
        try:
            self.action("scripts_run", {"id": script["id"], "expected_revision": 1})
        except APIError as error:
            assert error.status in (409, 412)
        else:
            raise RuntimeError("Stale script revision executed")
        assert updated["revision"] == 2
        self.guest("python3 - <<'PY'\nimport sqlite3\nconnection=sqlite3.connect('file:/var/lib/powerlinux/powerlinux.db?mode=ro',uri=True)\nassert " + repr(marker) + " not in '\\n'.join(connection.iterdump())\nPY")

    def schedules(self):
        if not self.scripts:
            self.scripts()
        run_at = (datetime.now(timezone.utc) + timedelta(seconds=4)).isoformat(timespec="seconds").replace("+00:00", "Z")
        schedule = self.action("schedules_create", {"name": "VM once script", "mode": "once", "timezone": "UTC", "run_at": run_at, "action": "scripts_run", "input": {"id": self.scripts[0], "expected_revision": 2}, "enabled": True})
        self.schedules.append(schedule["id"])
        until = time.monotonic() + 15
        history = []
        while time.monotonic() < until:
            history = self.action("schedules_history", {"id": schedule["id"]})
            if history and history[0]["status"] == "submitted":
                break
            time.sleep(0.3)
        assert len(history) == 1 and history[0]["status"] == "submitted"
        self.task(history[0]["result"]["task_id"])
        self.guest("systemctl restart powerlinux.service", timeout=40)
        self.ready()
        retained = self.action("schedules_history", {"id": schedule["id"]})
        assert len(retained) == 1 and retained[0]["id"] == history[0]["id"]
        records = self.action("schedules_list", {})
        assert next(item for item in records if item["id"] == schedule["id"])["enabled"] is False
        template = {"name": "VM scoped script", "mode": "interval", "timezone": "UTC", "interval_seconds": 15, "action": "scripts_run", "input": {"id": self.scripts[0], "expected_revision": 2}, "enabled": False}
        admin_schedule = self.action("schedules_create", template)
        self.schedules.append(admin_schedule["id"])
        borrower = self.scoped_client(["schedules:read", "schedules:write"], ["host:local", "schedule:*"])
        try:
            borrower.action("schedules_run", {"id": admin_schedule["id"]})
        except APIError as error:
            assert error.status == 403
        else:
            raise RuntimeError("Manual schedule borrowed its administrator author's authority")
        revoked = self.scoped_client(["schedules:read", "schedules:write", "scripts:read", "scripts:execute"], ["host:local", "schedule:*", "script:" + self.scripts[0]])
        at = (datetime.now(timezone.utc) + timedelta(seconds=4)).isoformat(timespec="seconds").replace("+00:00", "Z")
        once = {key: value for key, value in template.items() if key != "interval_seconds"}
        pending = revoked.action("schedules_create", {**once, "mode": "once", "run_at": at, "enabled": True})
        self.schedules.append(pending["id"])
        self.request("/api/v1/tokens/" + self.tokens[-1], method="DELETE")
        until = time.monotonic() + 12
        while time.monotonic() < until:
            disabled = next(item for item in self.action("schedules_list", {}) if item["id"] == pending["id"])
            if disabled["enabled"] is False:
                break
            time.sleep(0.3)
        assert disabled["enabled"] is False and "revoked" in disabled["last_error"]
        assert self.action("schedules_history", {"id": pending["id"]}) == []

    def scoped_client(self, scopes, resources):
        issued = self.request("/api/v1/tokens", {"name": "VM sysops scoped actor", "kind": "mcp", "scopes": scopes, "resources": resources, "expires_in_seconds": 600})["data"]
        self.tokens.append(issued["id"])
        client = Acceptance(self.args)
        client.token = issued["token"]
        return client

    def history(self):
        history = self.action("monitor_history", {"limit": 20})
        assert history["source"] == "persisted_host_sampler" and history["samples"]
        assert any(item.get("memory") and item.get("disk") and item.get("network") for item in history["samples"])

    def timezone(self):
        original = self.guest("timedatectl show --property=Timezone --value")
        chosen = "Asia/Shanghai" if original != "Asia/Shanghai" else "UTC"
        plan = self.plan({"kind": "timezone", "timezone": chosen})
        assert self.apply(plan)["state"] == "applied"
        assert self.guest("timedatectl show --property=Timezone --value") == chosen
        self.rollback(plan)
        assert self.guest("timedatectl show --property=Timezone --value") == original

    def ntp(self):
        assert self.guest("timedatectl show --property=CanNTP --value") == "yes", "Positive native NTP acceptance requires a real supported provider"
        original = self.guest("timedatectl show --property=NTP --value")
        plan = self.plan({"kind": "ntp", "ntp": original != "yes"})
        assert self.apply(plan)["state"] == "applied"
        assert self.guest("timedatectl show --property=NTP --value") != original
        self.rollback(plan)
        assert self.guest("timedatectl show --property=NTP --value") == original

    def ssh(self):
        self.guest("test ! -e /etc/ssh/sshd_config.d/powerlinux.conf")
        plan = self.plan({"kind": "ssh_config", "ssh": {"ports": [22], "password_authentication": False, "permit_root_login": "prohibit-password", "allow_users": ["root", "ubuntu"]}})
        assert self.apply(plan)["state"] == "awaiting_confirmation"
        effective = self.guest("/usr/sbin/sshd -T")
        assert "port 22\n" in effective + "\n"
        users = [item for line in effective.splitlines() if line.startswith("allowusers ") for item in line.split()[1:]]
        assert sorted(users) == ["root", "ubuntu"]
        assert self.action("system_change_confirm", {"id": plan})["state"] == "applied"
        self.rollback(plan)
        self.guest("test ! -e /etc/ssh/sshd_config.d/powerlinux.conf\n/usr/sbin/sshd -t\nsystemctl is-active --quiet ssh.service")

    def firewall(self):
        nft = shlex.quote(self.args.nft_path)
        self.guest("if " + nft + " list table inet powerlinux >/dev/null 2>&1; then exit 84; fi")
        change = {"kind": "firewall_rules", "firewall": {"default_policy": "accept", "rules": [{"id": "qa-deny", "protocol": "tcp", "port": 4567, "action": "drop"}]}}
        plan = self.plan(change)
        assert self.apply(plan)["state"] == "awaiting_confirmation"
        actual = json.loads(self.guest(nft + " --json list table inet powerlinux"))
        assert actual.get("nftables")
        # A fresh strict-SSH request proves the management connection survives.
        self.guest("true")
        until = time.monotonic() + 45
        while time.monotonic() < until:
            if self.action("system_change_get", {"id": plan})["state"] == "rolled_back":
                break
            time.sleep(0.3)
        assert self.action("system_change_get", {"id": plan})["state"] == "rolled_back"
        self.guest("if " + nft + " list table inet powerlinux >/dev/null 2>&1; then exit 85; fi")

    def panel(self):
        original = json.loads(self.guest("cat /etc/powerlinux/server.json"))
        chosen = dict(original)
        chosen["allowed_origins"] = list(dict.fromkeys(original.get("allowed_origins", []) + ["http://127.0.0.1:18997"]))
        plan = self.plan({"kind": "panel_config", "panel": chosen})
        # The job must remain truthful even when this action restarts its own
        # API process. Successful config/plan alone does not satisfy this gate.
        assert self.apply(plan)["state"] == "awaiting_confirmation"
        assert json.loads(self.guest("cat /etc/powerlinux/server.json")) == chosen
        assert self.action("system_change_confirm", {"id": plan})["state"] == "applied"
        self.rollback(plan)
        assert json.loads(self.guest("cat /etc/powerlinux/server.json")) == original

    def power(self):
        self.guest("test ! -e /run/systemd/shutdown/scheduled")
        plan = self.plan({"kind": "power", "power": "reboot", "delay_seconds": 600})
        assert self.apply(plan)["state"] == "applied"
        self.guest("test -f /run/systemd/shutdown/scheduled\ngrep -Fxq MODE=reboot /run/systemd/shutdown/scheduled")
        self.rollback(plan)
        self.guest("test ! -e /run/systemd/shutdown/scheduled")

    def cleanup(self):
        if not self.prepared:
            return
        errors = []
        for plan in reversed(self.plans):
            try:
                state = self.action("system_change_get", {"id": plan})["state"]
                if state not in ("rolled_back", "planned"):
                    task = self.action("system_change_rollback", {"id": plan})
                    self.task(task["task_id"], None)
                    if self.action("system_change_get", {"id": plan})["state"] != "rolled_back":
                        raise RuntimeError("Plan did not restore its original state")
            except Exception:
                errors.append("Plan requires recovery: " + plan)
        # A failed test must not accidentally leave a timed VM reboot armed.
        if "power" in self.args.checks:
            self.guest("/usr/sbin/shutdown -c --no-wall || true")
        for schedule in self.schedules:
            try:
                record = next(item for item in self.action("schedules_list", {}) if item["id"] == schedule)
                self.action("schedules_delete", {"id": schedule, "expected_revision": record["revision"]})
            except Exception:
                errors.append("Schedule cleanup failed")
        for script in self.scripts:
            try:
                record = self.action("scripts_get", {"id": script})["script"]
                self.action("scripts_delete", {"id": script, "expected_revision": record["revision"]})
            except Exception:
                errors.append("Script cleanup failed")
        for token in self.tokens:
            try:
                self.request("/api/v1/tokens/" + token, method="DELETE")
            except Exception:
                errors.append("Token cleanup failed")
        if errors:
            raise RuntimeError("; ".join(errors) + "; latest test policy/plans retained at " + self.state)
        self.guest("systemctl stop powerlinux-agent.service\ncp " + shlex.quote(self.state + "/baseline-policy.json") + " /etc/powerlinux/agent-policy.json\nchmod 0600 /etc/powerlinux/agent-policy.json\nsystemctl start powerlinux-agent.service\nsystemctl stop " + " ".join(self.units) + "\nrm -- " + " ".join("/etc/systemd/system/" + unit for unit in self.units) + "\nsystemctl daemon-reload", timeout=40)
        self.agent_health()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--owned-vm", action="store_true", required=True, help="Explicitly authorize mutations in your disposable, exclusively reserved VM")
    parser.add_argument("--ssh-helper", required=True, help="Existing strict-host-key root SSH helper, never a shared production host")
    parser.add_argument("--credentials", required=True, help="Private local fixture JSON: url, username, password")
    parser.add_argument("--version", required=True, help="Exact immutable installed package version")
    parser.add_argument("--archive", required=True, help="Immutable archive whose exact binary is installed in the guest")
    parser.add_argument("--sha256", required=True, help="Reviewed SHA-256 of the immutable archive")
    parser.add_argument("--checks", required=True, help="Comma-separated services,scripts,schedules,history,timezone,ntp,ssh,firewall,panel,power")
    parser.add_argument("--nft-path", default="/usr/sbin/nft", help="Reviewed root-owned actual nft executable")
    parser.add_argument("--ca-file", help="Additional trusted CA for an HTTPS local forward")
    parser.add_argument("--output", required=True, help="Redacted JSON evidence file")
    args = parser.parse_args()
    args.checks = args.checks.split(",")
    choices = {"services", "scripts", "schedules", "history", "timezone", "ntp", "ssh", "firewall", "panel", "power"}
    if not args.checks or set(args.checks) - choices or len(set(args.checks)) != len(args.checks):
        parser.error("Choose explicit unique known checks")
    if not args.nft_path.startswith("/"):
        parser.error("nft requires an absolute trusted path")
    acceptance = Acceptance(args)
    failure = None
    try:
        acceptance.prepare()
        for check in args.checks:
            started = time.monotonic()
            getattr(acceptance, check)()
            result = {"check": check, "passed": True, "seconds": round(time.monotonic() - started, 3)}
            acceptance.results.append(result)
            print(json.dumps(result), flush=True)
    except Exception as error:
        failure = {"type": type(error).__name__, "message": str(error)}
    finally:
        try:
            acceptance.cleanup()
        except Exception as error:
            failure = {"type": type(error).__name__, "message": "Cleanup: " + str(error), "prior_failure": failure}
        evidence = {"version": args.version, "archive_sha256": acceptance.archive_sha256, "archive_binary_sha256": acceptance.binary_sha256,
                    "installed_binary_sha256": acceptance.installed_binary_sha256,
                    "harness_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), "state_dir": acceptance.state,
                    "plan_ids": acceptance.plans, "tasks": acceptance.tasks, "results": acceptance.results, "failure": failure, "passed": failure is None}
        Path(args.output).write_text(json.dumps(evidence, indent=2) + "\n")
    if failure:
        raise SystemExit(json.dumps(failure))
    print("Actual guest sysops checks and restoration passed; reboot execution and external notifications remain separate.")


if __name__ == "__main__":
    main()
