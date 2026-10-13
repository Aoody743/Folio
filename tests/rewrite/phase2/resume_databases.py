#!/usr/bin/env python3
"""Reapply ephemeral QA Redis ACL and recreate verified VM tunnels after restart.
Only private fixture files are read; no credential values are printed.
"""
from pathlib import Path
import json
import os
import socket
import subprocess
import time


def main():
    guest=Path(os.environ.get("POWERLINUX_QA_VM_DIR","/workspace/tools/qa-vm/guest"))
    base=Path(os.environ.get("POWERLINUX_QA_DATABASE_DIR","/workspace/tools/qa-vm/database-fixtures"))
    path=base/"credentials.json"
    assert path.stat().st_mode & 0o077 == 0,"Fixture credentials must be private"
    config=json.loads(path.read_text())
    identity=subprocess.check_output([str(guest/"ssh.sh"),"cat /proc/1/comm; systemd-detect-virt"],timeout=30).decode().splitlines()
    assert identity[:2]==["systemd","qemu"],"Target must be owned actual systemd/QEMU"
    redis=config["redis"]
    # Redis credentials were generated as URL-safe tokens in setup_databases.py;
    # use a RESP request rather than a shell command to preserve literal bytes.
    parts=[b"ACL",b"SETUSER",redis["username"].encode(),b"reset",b"on",(">"+redis["password"]).encode(),b"~qa_scope:*",b"+@read",b"+@write",b"-@dangerous",b"-@admin",b"+ping",b"+hello",b"+info",b"+client|setinfo"]
    payload=b"*"+str(len(parts)).encode()+b"\r\n"+b"".join(b"$"+str(len(v)).encode()+b"\r\n"+v+b"\r\n" for v in parts)
    result=subprocess.run([str(guest/"ssh.sh"),"redis-cli --pipe"],input=payload,capture_output=True,timeout=30)
    if result.returncode:
        err=base/"resume-private-error.log"
        err.write_bytes(result.stderr)
        err.chmod(0o600)
        raise RuntimeError("Redis ACL reapplication failed; diagnostics are private")
    ssh=["ssh","-F","/dev/null","-i",str(guest/"id_ed25519"),"-o","IdentitiesOnly=yes","-o","BatchMode=yes","-o","StrictHostKeyChecking=yes","-o","UserKnownHostsFile="+str(guest/"known_hosts"),"-o","ExitOnForwardFailure=yes","-N","-p","19022"]
    ports={}
    for name,remote_port in {"mariadb":3306,"postgresql":5432,"redis":6379,"memcached":11211}.items():
        port=config[name]["port"]
        with socket.socket() as sock:
            sock.bind(("127.0.0.1",port))  # Reject collision, never kill a stale PID.
        ports[name]=port
        ssh.extend(["-L",f"127.0.0.1:{port}:127.0.0.1:{remote_port}"])
    ssh.append("root@127.0.0.1")
    err=open(base/"resume-tunnel-private.log","wb")
    process=subprocess.Popen(ssh,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=err,start_new_session=True)
    err.close()
    (base/"resume-tunnel-private.log").chmod(0o600)
    deadline=time.monotonic()+30
    while time.monotonic()<deadline:
        if process.poll() is not None:
            raise RuntimeError("Verified guest tunnel exited")
        try:
            for port in ports.values():
                with socket.create_connection(("127.0.0.1",port),timeout=1):
                    pass
            break
        except OSError:
            time.sleep(.2)
    else:
        process.terminate()
        raise RuntimeError("Verified guest tunnel did not bind")
    config["tunnel_pid"]=process.pid
    path.write_text(json.dumps(config,indent=2)+"\n")
    path.chmod(0o600)
    print(json.dumps({"credential_file":str(path),"new_tunnel_pid":process.pid,"loopback_ports":ports,"VM":"real Debian systemd/QEMU","note":"Original SQL passwords retained; Redis ephemeral ACL reapplied; old PID never used"}))

if __name__=="__main__":
    main()
