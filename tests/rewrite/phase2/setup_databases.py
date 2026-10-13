#!/usr/bin/env python3
"""Prepare real least-privilege SQL/cache targets only in the owned QEMU guest.
Credential values stay in private fixture files, never stdout/stderr or Git.
"""
from __future__ import annotations
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import time


def main():
    guest=Path(os.environ.get("POWERLINUX_QA_VM_DIR","/workspace/tools/qa-vm/guest"))
    fixture=Path(os.environ.get("POWERLINUX_QA_DATABASE_DIR","/workspace/tools/qa-vm/database-fixtures"))
    fixture.mkdir(parents=True,exist_ok=True,mode=0o700)
    fixture.chmod(0o700)
    def remote(command,content=None):
        result=subprocess.run([str(guest/"ssh.sh"),command],input=content,capture_output=True,timeout=90)
        if result.returncode:
            # Do not display database CLI diagnostics; a malformed password SQL
            # statement can echo credentials. Capture privately for redaction.
            path=fixture/"setup-private-error.log"
            path.write_bytes(result.stderr)
            path.chmod(0o600)
            raise RuntimeError("Owned VM database fixture setup failed; diagnostics are private")
        return result.stdout
    actual=remote("cat /proc/1/comm; systemd-detect-virt").decode().splitlines()
    assert actual[:2]==["systemd","qemu"],"Fixture target must be actual owned systemd/QEMU"
    passwords={key:secrets.token_urlsafe(32) for key in ("mariadb","mariadb_admin","mariadb_danger","pg","pg_admin","pg_danger","redis")}
    maria=f"""CREATE DATABASE IF NOT EXISTS qa_scope_public;
CREATE DATABASE IF NOT EXISTS qa_scope_private;
DROP USER IF EXISTS 'qa_scope_user'@'127.0.0.1';
DROP USER IF EXISTS 'qa_scope_admin'@'127.0.0.1';
DROP USER IF EXISTS 'qa_scope_pattern'@'127.0.0.1';
CREATE USER 'qa_scope_user'@'127.0.0.1' IDENTIFIED BY '{passwords['mariadb']}';
CREATE USER 'qa_scope_admin'@'127.0.0.1' IDENTIFIED BY '{passwords['mariadb_admin']}';
CREATE USER 'qa_scope_pattern'@'127.0.0.1' IDENTIFIED BY '{passwords['mariadb_danger']}';
GRANT SELECT,INSERT,UPDATE,DELETE ON `qa\\_scope\\_public`.* TO 'qa_scope_user'@'127.0.0.1';
GRANT SELECT ON qa_scope_public.* TO 'qa_scope_pattern'@'127.0.0.1';
GRANT ALL PRIVILEGES ON *.* TO 'qa_scope_admin'@'127.0.0.1' WITH GRANT OPTION;
CREATE TABLE IF NOT EXISTS qa_scope_public.qa_records(id INTEGER PRIMARY KEY, label VARCHAR(255));
INSERT INTO qa_scope_public.qa_records VALUES(1,'actual MariaDB record'),(2,'second MariaDB row') ON DUPLICATE KEY UPDATE label=VALUES(label);
CREATE TABLE IF NOT EXISTS qa_scope_private.private_records(id INTEGER PRIMARY KEY, label VARCHAR(255));
INSERT INTO qa_scope_private.private_records VALUES(1,'private owned fixture') ON DUPLICATE KEY UPDATE label=VALUES(label);
"""
    remote("mariadb --batch",maria.encode())
    pg=rf"""SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'qa_scope_user', '{passwords['pg']}') WHERE NOT EXISTS(SELECT FROM pg_roles WHERE rolname='qa_scope_user') \gexec
ALTER ROLE qa_scope_user NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '{passwords['pg']}';
SELECT format('CREATE ROLE %I LOGIN SUPERUSER PASSWORD %L', 'qa_scope_admin', '{passwords['pg_admin']}') WHERE NOT EXISTS(SELECT FROM pg_roles WHERE rolname='qa_scope_admin') \gexec
ALTER ROLE qa_scope_admin SUPERUSER PASSWORD '{passwords['pg_admin']}';
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'qa_scope_danger', '{passwords['pg_danger']}') WHERE NOT EXISTS(SELECT FROM pg_roles WHERE rolname='qa_scope_danger') \gexec
ALTER ROLE qa_scope_danger NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '{passwords['pg_danger']}';
GRANT pg_read_server_files TO qa_scope_danger;
SELECT 'CREATE DATABASE qa_scope_public' WHERE NOT EXISTS(SELECT FROM pg_database WHERE datname='qa_scope_public') \gexec
SELECT 'CREATE DATABASE qa_scope_private' WHERE NOT EXISTS(SELECT FROM pg_database WHERE datname='qa_scope_private') \gexec
REVOKE CONNECT ON DATABASE qa_scope_private FROM PUBLIC;
GRANT CONNECT ON DATABASE qa_scope_public TO qa_scope_user,qa_scope_danger;
\connect qa_scope_public
CREATE TABLE IF NOT EXISTS qa_records(id INTEGER PRIMARY KEY,label VARCHAR(255));
INSERT INTO qa_records VALUES(1,'actual PostgreSQL record'),(2,'second PostgreSQL row') ON CONFLICT(id) DO UPDATE SET label=excluded.label;
GRANT USAGE ON SCHEMA public TO qa_scope_user,qa_scope_danger;
GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO qa_scope_user,qa_scope_danger;
"""
    remote("runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1",pg.encode())
    # Redis ACL writes occur on a real daemon and forbid dangerous/admin ops.
    acl="ACL SETUSER qa_scope_cache reset on >"+passwords['redis']+" ~qa_scope:* +@read +@write -@dangerous -@admin +ping +hello +info +client|setinfo\n"
    remote("redis-cli --pipe",acl.encode())
    ports={}
    for key in ("mariadb","postgresql","redis","memcached"):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1",0))
            ports[key]=sock.getsockname()[1]
    ssh=["ssh","-F","/dev/null","-i",str(guest/"id_ed25519"),"-o","IdentitiesOnly=yes","-o","BatchMode=yes","-o","StrictHostKeyChecking=yes","-o","UserKnownHostsFile="+str(guest/"known_hosts"),"-o","ExitOnForwardFailure=yes","-N","-p","19022"]
    for key,remote_port in {"mariadb":3306,"postgresql":5432,"redis":6379,"memcached":11211}.items():
        ssh.extend(["-L",f"127.0.0.1:{ports[key]}:127.0.0.1:{remote_port}"])
    ssh.append("root@127.0.0.1")
    err=open(fixture/"ssh-forward-private.log","wb")
    process=subprocess.Popen(ssh,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=err,start_new_session=True)
    err.close()
    deadline=time.monotonic()+30
    while time.monotonic()<deadline:
        if process.poll() is not None:
            raise RuntimeError("Owned SSH tunnel failed; see private fixture diagnostics")
        try:
            for port in ports.values():
                with socket.create_connection(("127.0.0.1",port),timeout=1):
                    pass
            break
        except OSError:
            time.sleep(.2)
    else:
        process.terminate()
        raise RuntimeError("Owned database forwards did not become reachable")
    payload={
      "mariadb":{"id":"qa-mariadb","engine":"mariadb","host":"127.0.0.1","port":ports["mariadb"],"database":"qa_scope_public","username":"qa_scope_user","password":passwords['mariadb'],"access":"database","tls":False},
      "mariadb_admin":{"id":"qa-mariadb-admin","engine":"mariadb","host":"127.0.0.1","port":ports["mariadb"],"database":"qa_scope_public","username":"qa_scope_admin","password":passwords['mariadb_admin'],"access":"instance","tls":False},
      "mariadb_danger":{"id":"qa-mariadb-pattern","engine":"mariadb","host":"127.0.0.1","port":ports["mariadb"],"database":"qa_scope_public","username":"qa_scope_pattern","password":passwords['mariadb_danger'],"access":"database","tls":False},
      "postgresql":{"id":"qa-postgresql","engine":"postgresql","host":"127.0.0.1","port":ports["postgresql"],"database":"qa_scope_public","username":"qa_scope_user","password":passwords['pg'],"access":"database","tls":False},
      "postgresql_admin":{"id":"qa-postgresql-admin","engine":"postgresql","host":"127.0.0.1","port":ports["postgresql"],"database":"qa_scope_public","username":"qa_scope_admin","password":passwords['pg_admin'],"access":"instance","tls":False},
      "postgresql_danger":{"id":"qa-postgresql-danger","engine":"postgresql","host":"127.0.0.1","port":ports["postgresql"],"database":"qa_scope_public","username":"qa_scope_danger","password":passwords['pg_danger'],"access":"database","tls":False},
      "redis":{"id":"qa-redis","engine":"redis","host":"127.0.0.1","port":ports["redis"],"database":0,"prefix":"qa_scope:","username":"qa_scope_cache","password":passwords['redis'],"tls":False},
      "memcached":{"id":"qa-memcached","engine":"memcached","host":"127.0.0.1","port":ports["memcached"],"database":0,"prefix":"qa_scope:","username":"","password":"","tls":False},
      "tunnel_pid":process.pid,
    }
    path=fixture/"credentials.json"
    path.write_text(json.dumps(payload,indent=2)+"\n")
    path.chmod(0o600)
    (fixture/"ssh-forward-private.log").chmod(0o600)
    print(json.dumps({"credential_file":str(path),"tunnel_pid":process.pid,"loopback_ports":ports,"actual_VM":"Debian13 QEMU","cases":["dedicated MariaDB DB-only grants + isolated admin","dedicated PG role + admin + dangerous predefined role registration negative","actual Redis namespace ACL","actual Memcached namespace"],"not_verified":"PowerLinux adapters still pending actual package tests"}))

if __name__=="__main__":
    main()
