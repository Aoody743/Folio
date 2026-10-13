#!/usr/bin/env python3
"""Authenticate to actual owned MongoDB; never expose fixture credentials.

This is engine/credential setup evidence only, not software lifecycle or VM
acceptance. The disposable container must use our label and immutable image.
"""
from pathlib import Path
import json
import os
import subprocess


def main():
    base=Path(os.environ.get("POWERLINUX_QA_MONGO_DIR","/workspace/tools/qa-mongodb-fixture-20261013"))
    path=base/"credentials.json"
    assert path.stat().st_mode & 0o077 == 0,"Credentials must be private"
    config=json.loads(path.read_text())
    container=config["container_name"]
    raw=subprocess.check_output(["docker","inspect",container],timeout=30)
    info=json.loads(raw)[0]
    assert info["Config"]["Labels"].get("powerlinux.qa")=="phase2","Target is not our disposable fixture"
    assert info["Config"]["Image"]==config["image_digest"],"Actual Mongo image is not the pinned digest"
    assert all(v["HostIp"]=="127.0.0.1" for values in info["NetworkSettings"]["Ports"].values() if values for v in values),"Fixture exposes a nonloopback port"
    public=config["mongodb"]
    danger=config["mongodb_global"]
    # Create the negative account in the selected authentication DB. A user
    # created in admin but authenticated against public would merely fail login
    # and would not exercise PowerLinux's actual credential-scope rejection.
    js="""const fs=require('fs');
const admin=db.getSiblingDB('admin');
if (!admin.auth('qa_scope_admin',fs.readFileSync('/docker-entrypoint-initdb.d/root-password.secret','utf8').trim())) throw Error('QA admin auth failed');
const scoped=db.getSiblingDB('qa_scope_public');
"""
    js += "const negative="+json.dumps({"user":danger["username"],"pwd":danger["password"],"roles":[{"role":"readAnyDatabase","db":"admin"}]})+";\n"
    js += "if(scoped.getUser(negative.user)) scoped.updateUser(negative.user,{pwd:negative.pwd,roles:negative.roles}); else scoped.createUser(negative);\n"
    js += "const roles=scoped.getUser("+json.dumps(public["username"])+").roles;\n"
    js += "if(roles.length!==1 || roles[0].role!=='readWrite' || roles[0].db!=='qa_scope_public') throw Error('Positive Mongo role scope is not exact');\n"
    js += "const version=db.version(); const rows=scoped.qa_records.countDocuments({});\n"
    js += "scoped.logout(); if(!scoped.auth("+json.dumps(public["username"])+","+json.dumps(public["password"])+")) throw Error('QA scoped user auth failed');\n"
    js += "if(scoped.qa_records.countDocuments({})!==rows) throw Error('QA actual scoped query failed'); scoped.logout();\n"
    js += "if(!scoped.auth(negative.user,negative.pwd)) throw Error('QA negative user auth failed');\n"
    js += "print(JSON.stringify({engine:'MongoDB',version,authenticated_scoped_rows:rows,negative_authenticated:true,negative_role:'readAnyDatabase',private_database_reads:false}));\n"
    script="/tmp/powerlinux-qa-health-private.js"
    try:
        written=subprocess.run(["docker","exec","-i",container,"sh","-c","umask 077; cat > "+script],input=js.encode(),capture_output=True,timeout=30)
        if written.returncode:
            raise RuntimeError("Private Mongo health script transfer failed")
        result=subprocess.run(["docker","exec",container,"mongosh","--quiet","--file",script],capture_output=True,timeout=90)
        if result.returncode:
            error=base/"health-private-error.log"
            error.write_bytes(result.stderr+result.stdout)
            error.chmod(0o600)
            raise RuntimeError("Actual Mongo authenticated health failed; diagnostics retained privately")
        lines=result.stdout.decode().splitlines()
        evidence=json.loads(next(line for line in reversed(lines) if line.startswith('{"engine":')))
        evidence.update(credential_file=str(path),loopback_port=public["port"],image_digest=config["image_digest"],
                        not_verified="PowerLinux adapters; software lifecycle; real VM")
        print(json.dumps(evidence))
    finally:
        subprocess.run(["docker","exec",container,"rm","-f",script],capture_output=True,timeout=30)


if __name__=="__main__":
    main()
