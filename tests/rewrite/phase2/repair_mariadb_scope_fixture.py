#!/usr/bin/env python3
"""Fix only QA grants, keeping live fixture passwords/tunnels unchanged.

MariaDB/MySQL database grants treat '_' as a wildcard even inside backticks.
The positive account escapes every underscore; a separate negative account
intentionally retains a pattern grant. No application/private DB is queried.
"""
from pathlib import Path
import json
import os
import re
import secrets
import subprocess


def main():
    guest=Path(os.environ.get("POWERLINUX_QA_VM_DIR","/workspace/tools/qa-vm/guest"))
    base=Path(os.environ.get("POWERLINUX_QA_DATABASE_DIR","/workspace/tools/qa-vm/database-fixtures"))
    path=base/"credentials.json"
    assert path.stat().st_mode & 0o077 == 0, "Credential file must be private"
    config=json.loads(path.read_text())
    def remote(command, content=None):
        result=subprocess.run([str(guest/"ssh.sh"),command],input=content,capture_output=True,timeout=90)
        if result.returncode:
            err=base/"mariadb-grants-private-error.log"
            err.write_bytes(result.stderr)
            err.chmod(0o600)
            raise RuntimeError("Owned MariaDB grant repair failed; private diagnostics retained")
        return result.stdout
    identity=remote("cat /proc/1/comm; systemd-detect-virt").decode().splitlines()
    assert identity[:2]==["systemd","qemu"], "Only the owned QEMU/systemd VM is supported"
    danger=dict(config["mariadb"])
    danger.update(id="qa-mariadb-pattern", username="qa_scope_pattern",
                  password=config.get("mariadb_danger",{}).get("password") or secrets.token_urlsafe(32))
    # Fixture passwords are generated URL-safe; reject anything requiring SQL
    # escaping. They travel only over encrypted SSH stdin, never argv/stdout.
    assert re.fullmatch(r"[A-Za-z0-9_-]+",danger["password"]), "Unexpected fixture password format"
    sql=rf"""REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'qa_scope_user'@'127.0.0.1';
GRANT SELECT,INSERT,UPDATE,DELETE ON `qa\_scope\_public`.* TO 'qa_scope_user'@'127.0.0.1';
CREATE USER IF NOT EXISTS 'qa_scope_pattern'@'127.0.0.1' IDENTIFIED BY '{danger['password']}';
ALTER USER 'qa_scope_pattern'@'127.0.0.1' IDENTIFIED BY '{danger['password']}';
REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'qa_scope_pattern'@'127.0.0.1';
GRANT SELECT ON qa_scope_public.* TO 'qa_scope_pattern'@'127.0.0.1';
SELECT User,Db FROM mysql.db WHERE User IN ('qa_scope_user','qa_scope_pattern') ORDER BY User;
"""
    rows=remote("mariadb --batch --raw --skip-column-names",sql.encode()).decode().splitlines()
    assert rows==["qa_scope_pattern\tqa_scope_public", "qa_scope_user\tqa\\_scope\\_public"], "Literal/pattern grant metadata did not match"
    config["mariadb_danger"]=danger
    path.write_text(json.dumps(config,indent=2)+"\n")
    path.chmod(0o600)
    print(json.dumps({"credential_file":str(path),"actual_VM":"Debian13 QEMU",
        "literal_underscore_grant":True,"pattern_grant_negative_fixture":True,
        "private_database_reads":False,"PowerLinux_registration":"not yet validated"}))


if __name__=="__main__":
    main()
