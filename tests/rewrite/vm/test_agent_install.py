#!/usr/bin/env python3
"""Install the actual v2 package only into a clean owned systemd QEMU guest.

Keep the installed service and a private fixture credential for the following
terminal/software/site checks. This test does not uninstall or claim upgrade,
rollback, reboot, external providers, or individual module business flows.
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

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from http_client import APIClient,data


def main():
    archive=Path(os.environ["POWERLINUX_TEST_ARCHIVE"]).resolve()
    expected=os.environ["POWERLINUX_TEST_SHA256"]
    digest=hashlib.sha256(archive.read_bytes()).hexdigest()
    assert digest==expected,"Release archive failed independently supplied SHA verification"
    policy_file=Path(os.environ["POWERLINUX_QA_POLICY_FILE"]).resolve()
    policy=json.loads(policy_file.read_text())
    assert policy["version"]==2 and policy["allowed_users"]==["powerlinux"],"QA policy must explicitly authorize only the panel UID"
    assert policy.get("terminal"),"This acceptance needs a real independently configured terminal module"
    guest=Path(os.environ.get("POWERLINUX_QA_VM_DIR","/workspace/tools/qa-ubuntu/guest"))
    port=os.environ.get("POWERLINUX_QA_VM_SSH_PORT","19023")
    evidence_dir=Path(os.environ.get("POWERLINUX_QA_VM_EVIDENCE_DIR","/workspace/tools/qa-ubuntu/agent-acceptance"))
    evidence_dir.mkdir(parents=True,exist_ok=True,mode=0o700)
    evidence_dir.chmod(0o700)
    connection=["-F","/dev/null","-i",str(guest/"id_ed25519"),"-o","IdentitiesOnly=yes","-o","BatchMode=yes",
                "-o","StrictHostKeyChecking=yes","-o","UserKnownHostsFile="+str(guest/"known_hosts"),"-o","ConnectTimeout=30"]
    ssh=["ssh",*connection,"-p",port,"root@127.0.0.1"]
    scp=["scp",*connection,"-P",port]
    def remote(command,timeout=90,content=None,allowed_failure=False):
        result=subprocess.run(ssh+[command],input=content,capture_output=True,timeout=timeout)
        if result.returncode and not allowed_failure:
            path=evidence_dir/"command-private-error.log"
            path.write_bytes(result.stdout+result.stderr)
            path.chmod(0o600)
            raise RuntimeError("Owned VM command failed; private diagnostics retained")
        return result
    identity=remote("cat /proc/1/comm; systemd-detect-virt; uname -r; . /etc/os-release; echo \"$ID $VERSION_ID\"").stdout.decode().splitlines()
    assert identity[:2]==["systemd","qemu"],"Only actual owned QEMU/systemd guests are supported"
    clean=remote("test ! -e /opt/powerlinux && test ! -e /var/lib/powerlinux && test ! -e /etc/powerlinux && test ! -e /etc/systemd/system/powerlinux.service && test ! -e /etc/systemd/system/powerlinux-agent.service",allowed_failure=True)
    assert clean.returncode==0,"Guest is not clean; existing user deployment must be preserved"
    snapshot="for f in /bin/sh /etc/default/locale /etc/locale.gen /etc/hosts /etc/resolv.conf /etc/profile /etc/motd; do test ! -f \"$f\" || sha256sum \"$f\"; done; find /etc/cron.d /etc/cron.daily /var/spool/cron -type f -exec sha256sum {} + 2>/dev/null || true"
    global_before=remote(snapshot).stdout
    # Extract just the install script from the verified package. Never test a
    # mutable checkout installer against an older binary/configuration format.
    with tarfile.open(archive,"r:gz") as source:
        install_member=source.getmember("install.sh")
        assert install_member.isfile(),"Packaged installer must be a regular file"
        version_member=source.getmember("VERSION")
        assert version_member.isfile(),"Packaged VERSION must be a regular file"
        version=source.extractfile(version_member).read().decode().strip()
        installer=evidence_dir/"install-from-verified-package.sh"
        installer.write_bytes(source.extractfile(install_member).read())
        installer.chmod(0o600)
    remote("install -d -m 0700 /root/powerlinux-qa-runtime")
    for local,target in ((archive,"candidate.tar.gz"),(installer,"install.sh"),(policy_file,"policy.json")):
        subprocess.run(scp+[str(local),"root@127.0.0.1:/root/powerlinux-qa-runtime/"+target],check=True,capture_output=True,timeout=120)
    remote("chmod 0600 /root/powerlinux-qa-runtime/*")
    command="bash /root/powerlinux-qa-runtime/install.sh --archive /root/powerlinux-qa-runtime/candidate.tar.gz --sha256 "+digest+" --enable-agent --agent-policy /root/powerlinux-qa-runtime/policy.json --enable-terminal"
    if policy.get("plugins"):
        command+=" --enable-software"
    if policy.get("sites"):
        command+=" --enable-sites"
    # A clean /etc does not imply no existing deployment: systemd may resolve
    # a vendor unit. Test both services before creating panel configuration.
    # FIFO fixtures must be rejected before reading or consulting systemd.
    # Each fixture is an inert owned-VM unit; none is ever started.
    preflight=[]
    for service in ("powerlinux.service","powerlinux-agent.service"):
        vendor="/usr/lib/systemd/system/"+service
        local_unit="/etc/systemd/system/"+service
        remote("test ! -e "+shlex.quote(vendor)+" && test ! -L "+shlex.quote(vendor)+" && test ! -e /lib/systemd/system/"+service+" && test ! -e "+shlex.quote(local_unit))
        fixture=("# Owned PowerLinux QA inert vendor-unit fixture\n[Unit]\nDescription=PowerLinux QA foreign vendor fixture\n[Service]\nType=oneshot\nExecStart=/bin/true\n").encode()
        try:
            remote("umask 022; cat > "+shlex.quote(vendor),content=fixture)
            remote("systemctl daemon-reload",timeout=60)
            fragment=remote("systemctl show "+service+" --property=FragmentPath --value").stdout.decode().strip()
            assert fragment in (vendor,"/lib/systemd/system/"+service),"Vendor fixture did not resolve through real systemd"
            rejected=remote("timeout 8s "+command,timeout=35,allowed_failure=True)
            assert rejected.returncode not in (0,124),"Existing vendor unit was accepted or installer hung"
            assert b"unmanaged location" in rejected.stderr,"Installer failed before exercising vendor ownership gate"
            assert remote("cat "+shlex.quote(vendor)).stdout==fixture,"Rejected installation changed the existing vendor unit"
            assert remote("systemctl show "+service+" --property=ActiveState --value").stdout.strip()==b"inactive","Preflight started a fixture service"
            assert remote("test ! -e /opt/powerlinux && test ! -e /var/lib/powerlinux && test ! -e /etc/powerlinux && test ! -e "+shlex.quote(local_unit),allowed_failure=True).returncode==0,"Rejected preflight created deployment paths"
            preflight.append({"service":service,"fixture":"foreign vendor","fragment":fragment,"exit":rejected.returncode,"stderr":rejected.stderr.decode()})
        finally:
            remote("rm -f -- "+shlex.quote(vendor)+"; systemctl daemon-reload",timeout=60)
        try:
            remote("mkfifo -m 0600 "+shlex.quote(local_unit))
            rejected=remote("timeout 8s "+command,timeout=35,allowed_failure=True)
            assert rejected.returncode not in (0,124),"FIFO unit was accepted or installer blocked reading it"
            assert b"root-owned regular file" in rejected.stderr,"Installer failed before exercising FIFO gate"
            assert remote("test -p "+shlex.quote(local_unit)+" && test ! -e /opt/powerlinux && test ! -e /var/lib/powerlinux && test ! -e /etc/powerlinux",allowed_failure=True).returncode==0,"Rejected preflight changed FIFO or deployment paths"
            preflight.append({"service":service,"fixture":"FIFO","exit":rejected.returncode,"stderr":rejected.stderr.decode()})
        finally:
            remote("rm -f -- "+shlex.quote(local_unit)+"; systemctl daemon-reload",timeout=60)
    (evidence_dir/"actual-unit-preflight-evidence.json").write_text(json.dumps(preflight,indent=2)+"\n")
    # Configured filesystem roots are owned QA fixtures only. Broad host roots
    # already exist; refuse to create arbitrary directories from a policy.
    for root in policy.get("files",{}).get("roots",[]):
        if root["path"]=="/" and root.get("host_access"):
            continue
        assert root["path"].startswith("/srv/powerlinux-qa-"),"Unexpected QA root path"
        remote("install -d -m 0755 "+shlex.quote(root["path"]))
    with socket.socket() as bound:
        bound.bind(("127.0.0.1",0))
        api_port=bound.getsockname()[1]
    public="http://127.0.0.1:"+str(api_port)
    # A clean install must create its own managed configuration directory.
    # Preseeding an unmarked /etc/powerlinux would correctly be rejected.
    installed=remote(command,timeout=180)
    (evidence_dir/"actual-install.log").write_bytes(installed.stdout+installed.stderr)
    # Then update only the newly managed root-owned public endpoint document
    # for our actual strict SSH forward. Retain all package settings and its
    # existing owner/mode; do not forge a managed marker or alter units.
    config=json.loads(remote("cat /etc/powerlinux/server.json").stdout)
    config.update({"public_url":public+"/mcp","allowed_origins":[public]})
    config_stat=remote("stat -c '%u %g %a' /etc/powerlinux/server.json").stdout
    remote("systemctl stop powerlinux.service; cat > /etc/powerlinux/server.json",content=(json.dumps(config)+"\n").encode())
    assert config_stat==remote("stat -c '%u %g %a' /etc/powerlinux/server.json").stdout,"QA endpoint update changed trusted configuration ownership/mode"
    remote("systemctl start powerlinux.service",timeout=60)
    remote("/opt/powerlinux/current/powerlinux healthcheck --server-config /etc/powerlinux/server.json --expected-version "+shlex.quote(version),timeout=45)
    active=remote("systemctl is-active powerlinux.service powerlinux-agent.service; systemctl show powerlinux.service -p User --value; systemctl show powerlinux-agent.service -p User --value; id -u powerlinux; id -u powerlinux-terminal").stdout.decode().splitlines()
    assert active[:4]==["active","active","powerlinux","root"],"Production API/Agent identities or services mismatch"
    assert active[4]!=active[5] and active[5]!="0","Terminal UID must be independent from API/root"
    repeated=remote(command,timeout=180)
    (evidence_dir/"actual-repeat-install.log").write_bytes(repeated.stdout+repeated.stderr)
    tree="find /var/lib/powerlinux-agent /etc/powerlinux -xdev -printf '%p %m %u %g\\n' | sort"
    static_before=remote(tree).stdout
    checked=json.loads(remote("/opt/powerlinux/current/powerlinux agent --policy /etc/powerlinux/agent-policy.json --check-policy").stdout)
    assert checked["valid"] and checked["policy_sha256"]==hashlib.sha256(policy_file.read_bytes()).hexdigest()
    assert static_before==remote(tree).stdout,"Read-only check-policy changed filesystem metadata"
    health=json.loads(remote("runuser -u powerlinux -- /opt/powerlinux/current/powerlinux agent-healthcheck --expected-policy-sha256 "+checked["policy_sha256"]).stdout)
    assert health["ready"] and "terminal" in health["modules"]
    denied=remote("/opt/powerlinux/current/powerlinux agent-healthcheck",allowed_failure=True)
    assert denied.returncode!=0,"Kernel SO_PEERCRED accepted unlisted root client UID"
    assert global_before==remote(snapshot).stdout,"Installer changed protected global files"
    bootstrap=remote("cat /var/lib/powerlinux/bootstrap-token").stdout.decode().strip()
    tunnel=subprocess.Popen(ssh[:-1]+["-o","ExitOnForwardFailure=yes","-N","-L",f"127.0.0.1:{api_port}:127.0.0.1:9000",ssh[-1]],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True)
    try:
        api=APIClient(public)
        deadline=time.monotonic()+30
        while True:
            assert tunnel.poll() is None,"Verified guest tunnel exited"
            try:
                if api.get("/healthz")[0]==200:
                    break
            except OSError:
                pass
            assert time.monotonic()<deadline,"Actual production API is unreachable"
            time.sleep(.2)
        password="QA-"+secrets.token_urlsafe(24)
        data(api.post("/api/v1/auth/setup",{"username":"vm-v2-qa","password":password,"setup_token":bootstrap}))
        api.csrf=data(api.post("/api/v1/auth/login",{"username":"vm-v2-qa","password":password}))["csrf_token"]
        note=data(api.post("/api/v1/notes",{"title":"Actual v2 VM note","content":"separate API and root Agent","target_id":"host:local"}))
        remote("systemctl restart powerlinux-agent.service powerlinux.service",timeout=90)
        deadline=time.monotonic()+45
        while True:
            try:
                if api.get("/healthz")[0]==200:
                    break
            except OSError:
                pass
            assert time.monotonic()<deadline,"Restarted production API did not become ready"
            time.sleep(.2)
        assert data(api.get("/api/v1/notes/"+note["id"]))["content"]==note["content"]
        private={"url":public,"username":"vm-v2-qa","password":password,"tunnel_pid":tunnel.pid,"guest":str(guest),"ssh_port":int(port),"version":health["version"],"archive_sha256":digest,"note_id":note["id"]}
        path=evidence_dir/"panel-fixture-private.json"
        path.write_text(json.dumps(private,indent=2)+"\n")
        path.chmod(0o600)
        evidence={"platform":identity[3],"kernel":identity[2],"virtualization":"QEMU TCG","version":health["version"],"archive_sha256":digest,
                  "checks":["API and Agent foreign vendor/FIFO units rejected before deployment","clean actual v2 production install","repeat install","API ordinary UID / Agent root / terminal independent UID","read-only policy check","real authenticated Agent health","SO_PEERCRED root client denied","protected global files unchanged","actual note persists through API+Agent restart"],
                  "modules_configured":health["modules"],"private_panel_fixture":str(path),
                  "not_verified":["terminal/API/WS/MCP business flows","plugin lifecycles","site/TLS closure","failed upgrade and rollback","reboot","native ARM"]}
        (evidence_dir/"actual-install-evidence.json").write_text(json.dumps(evidence,indent=2)+"\n")
        print(json.dumps(evidence))
    except Exception:
        tunnel.terminate()
        tunnel.wait(timeout=10)
        raise
    # Successful tunnel intentionally remains available for subsequent actual
    # integration tests. The private fixture records its newly started PID.


if __name__=="__main__":
    main()
