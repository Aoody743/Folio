#!/usr/bin/env python3
"""Create one key-auth SSH account only in the owned actual Ubuntu QEMU VM.

No sshd configuration is changed. The account has its own home, UID, and
private authorized_keys; private keys and password hashes never go to logs.
This prepares an actual OpenSSH target, not a PowerLinux terminal acceptance.
"""
from pathlib import Path
import json
import os
import secrets
import shlex
import subprocess


def main():
    guest=Path(os.environ.get("POWERLINUX_QA_SSH_VM_DIR","/workspace/tools/qa-ubuntu/guest"))
    base=Path(os.environ.get("POWERLINUX_QA_SSH_FIXTURE_DIR","/workspace/tools/qa-ubuntu/ssh-terminal-fixtures"))
    base.mkdir(parents=True,exist_ok=True,mode=0o700)
    base.chmod(0o700)
    ssh_port=os.environ.get("POWERLINUX_QA_SSH_PORT","19023")
    def remote(command,content=None):
        result=subprocess.run([str(guest/"ssh.sh"),command],input=content,capture_output=True,timeout=90)
        if result.returncode:
            error=base/"setup-private-error.log"
            error.write_bytes(result.stderr)
            error.chmod(0o600)
            raise RuntimeError("Owned SSH fixture setup failed; diagnostics retained privately")
        return result.stdout
    identity=remote("cat /proc/1/comm; systemd-detect-virt").decode().splitlines()
    assert identity[:2]==["systemd","qemu"],"Target must be owned actual QEMU/systemd"
    platform=remote('. /etc/os-release; printf "%s %s\\n" "$ID" "$VERSION_ID"').decode().strip()
    key=base/"id_ed25519"
    if not key.exists():
        subprocess.run(["ssh-keygen","-q","-t","ed25519","-N","","-C","PowerLinux disposable terminal QA","-f",str(key)],check=True,capture_output=True,timeout=30)
    key.chmod(0o600)
    public=key.with_suffix(".pub").read_text().strip()
    assert public.startswith("ssh-ed25519 ")
    hashed=subprocess.check_output(["openssl","passwd","-6","-stdin"],input=secrets.token_urlsafe(32).encode()+b"\n",timeout=30).decode().strip()
    script="""set -euo pipefail
qa_user=powerlinux-qa-ssh
qa_home=/var/lib/powerlinux-qa-ssh
if ! getent passwd "$qa_user" >/dev/null; then useradd --create-home --home-dir "$qa_home" --shell /bin/bash "$qa_user"; fi
test "$(getent passwd "$qa_user" | cut -d: -f6)" = "$qa_home"
test ! -L "$qa_home"
install -d -m 0700 -o "$qa_user" -g "$qa_user" "$qa_home/.ssh"
test ! -L "$qa_home/.ssh/authorized_keys"
"""
    script += "usermod --password "+shlex.quote(hashed)+" \"$qa_user\"\n"
    script += "printf '%s\\n' "+shlex.quote(public)+" > \"$qa_home/.ssh/authorized_keys\"\n"
    script += "chown \"$qa_user:$qa_user\" \"$qa_home/.ssh/authorized_keys\"\nchmod 0600 \"$qa_home/.ssh/authorized_keys\"\n"
    script += "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '{print $2}'\n"
    fingerprint=remote("bash -s",script.encode()).decode().strip()
    assert fingerprint.startswith("SHA256:") and len(fingerprint)>40,"Actual SSH fingerprint missing"
    verify=["ssh","-F","/dev/null","-i",str(key),"-o","IdentitiesOnly=yes","-o","BatchMode=yes",
            "-o","StrictHostKeyChecking=yes","-o","UserKnownHostsFile="+str(guest/"known_hosts"),
            "-p",ssh_port,"powerlinux-qa-ssh@127.0.0.1","id -un; id -u; pwd; test -t 0 && echo actual-PTY"]
    # Force a real server PTY; batch mode still authenticates only our key.
    verify.insert(1,"-tt")
    checked=subprocess.run(verify,stdin=subprocess.DEVNULL,capture_output=True,timeout=90)
    if checked.returncode:
        error=base/"auth-private-error.log"
        error.write_bytes(checked.stderr)
        error.chmod(0o600)
        raise RuntimeError("Actual non-root key-auth OpenSSH PTY failed")
    rows=checked.stdout.decode().splitlines()
    assert rows[0]=="powerlinux-qa-ssh" and int(rows[1])>0 and rows[-1]=="actual-PTY","Real SSH identity or PTY mismatch"
    config={"username":"powerlinux-qa-ssh","private_key_file":str(key),"host":"127.0.0.1","port":22,"host_key":fingerprint,
            "host_forward_port":int(ssh_port),"guest":str(guest),"user_id":int(rows[1]),"home":rows[2],"scope":"owned disposable SSH fixture only"}
    path=base/"credentials.json"
    path.write_text(json.dumps(config,indent=2)+"\n")
    path.chmod(0o600)
    print(json.dumps({"actual_VM":platform+" QEMU/systemd","actual_daemon":"OpenSSH","key_auth":True,
                     "actual_PTY":True,"UID":int(rows[1]),"private_fixture_file":str(path),"sshd_configuration_changed":False,
                     "not_verified":"PowerLinux Agent/terminal/WS/MCP integration"}))


if __name__=="__main__":
    main()
