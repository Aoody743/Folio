#!/usr/bin/env python3
"""Independent black-box filesystem checks against a real release executable."""
from __future__ import annotations

import base64
import hashlib
import io
import json
import os
from pathlib import Path
import secrets
import sys
import tarfile
import time
import unittest
import urllib.error
import urllib.parse
import urllib.request
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from fixture import RunningServer
from http_client import APIClient, data
from mcp_client import HTTPMCPClient, StdioMCPClient, tool_data


def digest(content):
    return hashlib.sha256(content).hexdigest()


def binary_request(client, method, path, body=None, *, csrf=True, headers=None):
    extra = {"Accept": "application/octet-stream"}
    if client.token:
        extra["Authorization"] = "Bearer " + client.token
    elif csrf and client.csrf:
        extra["X-CSRF-Token"] = client.csrf
    if body is not None:
        extra["Content-Type"] = "application/octet-stream"
    extra.update(headers or {})
    request = urllib.request.Request(client.base + path, data=body, headers=extra, method=method)
    try:
        response = client.opener.open(request, timeout=30)
    except urllib.error.HTTPError as exc:
        response = exc
    with response:
        raw = response.read()
        return response.status, raw, response.headers


class RealFiles(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        binary = os.environ.get("POWERLINUX_TEST_BINARY")
        if not binary:
            raise RuntimeError("POWERLINUX_TEST_BINARY must identify the actual packaged executable")
        cls.server = RunningServer(binary)
        cls.addClassCleanup(cls.server.close)
        cls.admin = APIClient(cls.server.url)
        password = "QA-" + secrets.token_urlsafe(24)
        data(cls.admin.post("/api/v1/auth/setup", {"username":"files-qa", "password":password, "setup_token":cls.server.bootstrap}))
        session = data(cls.admin.post("/api/v1/auth/login", {"username":"files-qa", "password":password}))
        cls.admin.csrf = session["csrf_token"]
        cls.workspace = cls.server.data_dir / "workspace"
        cls.version = cls.admin.get("/healthz")[1].get("version")

    def token(self, scopes=None, resources=None):
        return data(self.admin.post("/api/v1/tokens", {"name":"Filesystem isolated QA", "kind":"mcp", "scopes":scopes or ["files:read","files:write","files:permissions","tasks:read"], "resources":resources if resources is not None else ["fs:workspace"], "expires_in_seconds":3600}))

    def execute(self, name, values=None, client=None):
        return data((client or self.admin).post("/api/v1/actions/" + name, values or {}))

    def poll(self, initial, *, success=True, client=None):
        self.assertIn("id", initial)
        deadline = time.monotonic() + 25
        while time.monotonic() < deadline:
            job = data((client or self.admin).get("/api/v1/tasks/" + initial["id"]))
            if job["status"] in ("succeeded","failed","canceled","interrupted"):
                self.assertEqual(job["resource"], "fs:workspace")
                if success:
                    self.assertEqual(job["status"], "succeeded", job)
                    self.assertEqual(job["result"]["exit_code"], 0, job)
                    return job["result"].get("data")
                self.assertNotEqual(job["status"], "succeeded", job)
                return job
            time.sleep(.04)
        self.fail("Actual file task did not reach a terminal state")

    def make(self, path, content="fixture", encoding="utf-8"):
        return self.execute("fs_create", {"root":"workspace","path":path,"content":content,"encoding":encoding})

    def read(self, path, **params):
        return self.execute("fs_read", {"root":"workspace","path":path,**params})

    def test_01_permissions_and_native_mcp_registry(self):
        token = self.token(scopes=["files:read"], resources=["fs:workspace"])
        client = APIClient(self.server.url, token["token"])
        roots = self.execute("fs_roots", client=client)
        self.assertEqual([x["id"] for x in roots], ["workspace"])
        self.assertNotIn("path", roots[0])
        denied = self.token(scopes=["files:read"], resources=["host:local"])
        other = APIClient(self.server.url, denied["token"])
        self.assertEqual(self.execute("fs_roots", client=other), [])
        self.assertEqual(other.post("/api/v1/actions/fs_list", {"root":"workspace"})[0], 403)
        self.assertEqual(client.post("/api/v1/actions/fs_create", {"root":"workspace","path":"read-only-denied"})[0], 403)
        mcp = HTTPMCPClient(self.server.url + "/mcp", token["token"])
        mcp.initialize()
        _, tools = mcp.request("tools/list", {})
        names = [t["name"] for t in tools["result"]["tools"]]
        self.assertIn("fs_read", names)
        self.assertIn("fs_download", names)
        self.assertNotIn("fs_edit", names)
        self.assertEqual(tool_data(mcp.call("fs_roots", {})), roots)
        self.assertEqual(client.post("/api/v1/actions/fs_list", {"root":"workspace","actor":{"kind":"human"}})[0], 400)

    def test_02_unicode_encoding_history_and_conflicting_edits(self):
        path = "多语言便签 GB18030.txt"
        self.make(path, "中文与é café 🧪", "gb18030")
        initial = self.read(path, encoding="gb18030")
        self.assertEqual(initial["content"], "中文与é café 🧪")
        self.assertEqual((self.workspace / path).read_bytes(), "中文与é café 🧪".encode("gb18030"))
        token = self.token()
        mcp = HTTPMCPClient(self.server.url + "/mcp", token["token"])
        mcp.initialize()
        tool_data(mcp.call("fs_edit", {"root":"workspace","path":path,"encoding":"gb18030","content":"外部 AI 编辑", "expected_hash":initial["sha256"]}))
        stale = self.admin.post("/api/v1/actions/fs_edit", {"root":"workspace","path":path,"encoding":"gb18030","content":"不能覆盖", "expected_hash":initial["sha256"]})
        self.assertEqual(stale[0], 409, stale[1])
        current = self.read(path, encoding="gb18030")
        self.assertEqual(current["content"], "外部 AI 编辑")
        revisions = self.execute("fs_history", {"root":"workspace","path":path})
        self.assertEqual(len(revisions),2)
        self.assertEqual(revisions[0]["sha256"],current["sha256"])
        self.assertEqual(revisions[0]["actor_id"],"token:" + token["id"])
        original_revision = next(r for r in revisions if r["sha256"] == initial["sha256"])
        self.assertNotEqual(original_revision["actor_id"],revisions[0]["actor_id"])
        self.assertNotEqual(original_revision["actor_id"],"external")
        old = self.execute("fs_history", {"root":"workspace","path":path,"id":original_revision["id"]})
        self.assertEqual(old["content"], "中文与é café 🧪")
        self.execute("fs_edit", {"root":"workspace","path":path,"encoding":"gb18030","content":old["content"],"expected_hash":current["sha256"]})
        self.server.restart()
        self.assertEqual(self.read(path, encoding="gb18030")["content"], old["content"])
        self.assertEqual(len(self.execute("fs_history", {"root":"workspace","path":path})), 3)
        audit = data(self.admin.get("/api/v1/audit?limit=100"))
        edits = [a for a in audit if a["action"] == "fs_edit" and a["resource"] == "fs:workspace"]
        self.assertTrue(any(a["actor"]["kind"] == "mcp" and a["actor"]["id"] == "token:" + token["id"] for a in edits))
        self.assertTrue(any(a["actor"]["kind"] == "human" for a in edits))
        serialized = json.dumps(audit,ensure_ascii=False)
        for private in ("外部 AI 编辑", "中文与é café 🧪", token["token"]):
            self.assertFalse(private in serialized,"File content or credential leaked through audit")

    def test_03_directory_paging_search_and_actual_size(self):
        self.execute("fs_mkdir", {"root":"workspace","path":"paging"})
        self.execute("fs_mkdir", {"root":"workspace","path":"paging/nested"})
        for i in range(7):
            self.make("paging/QA-%02d.txt" % i, "row-%d" % i)
        self.make("paging/nested/QA-deep.txt", "deep")
        self.make("paging/.hidden", "hidden")
        first = self.execute("fs_list", {"root":"workspace","path":"paging","limit":3,"offset":0,"sort":"name"})
        second = self.execute("fs_list", {"root":"workspace","path":"paging","limit":3,"offset":3,"sort":"name"})
        self.assertEqual(first["total"], 9)
        self.assertEqual(len(first["items"]), 3)
        self.assertFalse(set(x["name"] for x in first["items"]) & set(x["name"] for x in second["items"]))
        found = self.poll(self.execute("fs_search", {"root":"workspace","path":"paging","query":"QA-","limit":100}))
        self.assertEqual(found["total"], 8)
        self.assertIn("paging/nested/QA-deep.txt", [x["path"] for x in found["items"]])
        size = self.poll(self.execute("fs_size", {"root":"workspace","path":"paging"}))
        expected = sum(p.stat().st_size for p in (self.workspace / "paging").rglob("*") if p.is_file())
        self.assertEqual(size["bytes"], expected)

    def test_04_copy_move_scope_and_overwrite_conflict(self):
        self.make("copy-source", "source bytes")
        result = self.poll(self.execute("fs_copy", {"root":"workspace","path":"copy-source","destination_root":"workspace","destination_path":"copied"}))
        self.assertEqual((self.workspace / "copied").read_bytes(), b"source bytes")
        self.make("conflicting-destination", "existing bytes")
        self.poll(self.execute("fs_copy", {"root":"workspace","path":"copy-source","destination_root":"workspace","destination_path":"conflicting-destination"}), success=False)
        self.assertEqual((self.workspace / "conflicting-destination").read_bytes(), b"existing bytes")
        self.poll(self.execute("fs_move", {"root":"workspace","path":"copied","destination_root":"workspace","destination_path":"moved"}))
        self.assertFalse((self.workspace / "copied").exists())
        self.assertEqual((self.workspace / "moved").read_bytes(), b"source bytes")
        token = self.token(scopes=["files:write","tasks:read"])
        client = APIClient(self.server.url, token["token"])
        self.assertEqual(client.post("/api/v1/actions/fs_copy", {"root":"workspace","path":"missing","destination_root":"private","destination_path":"secret"})[0], 403)
        self.assertEqual(client.post("/api/v1/actions/fs_copy", {"root":"workspace","path":"copy-source","destination_root":"workspace","destination_path":"read-scope-required"})[0], 403)
        self.assertFalse((self.workspace / "read-scope-required").exists())

    def test_05_permissions_and_symlink_boundaries(self):
        self.make("mode-fixture", "content")
        self.poll(self.execute("fs_chmod", {"root":"workspace","path":"mode-fixture","mode":0o640}))
        self.assertEqual((self.workspace / "mode-fixture").stat().st_mode & 0o777, 0o640)
        key = self.server.data_dir / "qa-private-sentinel"
        key.write_text("private fixture must stay outside managed root")
        (self.workspace / "outside-link").symlink_to(key)
        for action, params in [("fs_read", {"path":"outside-link"}), ("fs_read", {"path":"../qa-private-sentinel"}), ("fs_edit", {"path":"outside-link","content":"overwrite","overwrite":True})]:
            response = self.admin.post("/api/v1/actions/" + action, {"root":"workspace", **params})
            self.assertIn(response[0], (400,403), response[1])
        self.assertEqual(key.read_text(), "private fixture must stay outside managed root")
        attempted = self.admin.post("/api/v1/actions/fs_delete", {"root":"workspace","path":".","recursive":True})
        if attempted[0] == 200:
            self.poll(data(attempted),success=False)
        else:
            self.assertEqual(attempted[0],403,attempted[1])
        self.assertTrue(self.workspace.is_dir())
        self.assertEqual((self.workspace / "mode-fixture").read_text(),"content")

    def test_06_recycle_conflict_restore_purge_and_restart(self):
        self.make("recycle-fixture", "retain bytes")
        original = (self.workspace / "recycle-fixture").stat().st_mode & 0o777
        record = self.poll(self.execute("fs_delete", {"root":"workspace","path":"recycle-fixture"}))
        self.assertFalse((self.workspace / "recycle-fixture").exists())
        self.server.restart()
        self.assertIn(record["id"], [r["id"] for r in self.execute("fs_trash_list", {"root":"workspace"})])
        self.make("recycle-fixture", "conflicting replacement")
        self.poll(self.execute("fs_restore", {"root":"workspace","id":record["id"]}), success=False)
        self.assertEqual((self.workspace / "recycle-fixture").read_bytes(), b"conflicting replacement")
        new = self.poll(self.execute("fs_delete", {"root":"workspace","path":"recycle-fixture"}))
        self.poll(self.execute("fs_restore", {"root":"workspace","id":record["id"]}))
        self.assertEqual((self.workspace / "recycle-fixture").read_bytes(), b"retain bytes")
        self.assertEqual((self.workspace / "recycle-fixture").stat().st_mode & 0o777, original)
        self.poll(self.execute("fs_purge", {"root":"workspace","id":new["id"]}))
        self.assertNotIn(new["id"], [r["id"] for r in self.execute("fs_trash_list", {"root":"workspace"})])

    def test_07_large_upload_http_mcp_resume_download_range_auth(self):
        token = self.token()
        client = APIClient(self.server.url, token["token"])
        content = bytes(range(256)) * ((4 << 20)//256) + b"last-MCP" * 4096
        session = self.execute("fs_upload_begin", {"root":"workspace","path":"large-transfer.bin","size":len(content),"sha256":digest(content)}, client)
        route = "/api/v1/files/uploads/" + session["id"] + "/chunk?root=workspace&offset=0"
        first = content[:4 << 20]
        response = binary_request(client,"PATCH", route + "&sha256=" + digest(first), first)
        self.assertEqual(response[0],200,response[1][:300])
        self.assertEqual(json.loads(response[1])["data"]["offset"], len(first))
        self.assertEqual(binary_request(client,"PATCH",route,b"duplicate")[0],409)
        self.server.restart()
        self.assertEqual(self.execute("fs_upload_status", {"root":"workspace","id":session["id"]},client)["offset"],len(first))
        mcp = HTTPMCPClient(self.server.url + "/mcp",token["token"])
        mcp.initialize()
        last = content[len(first):]
        received = tool_data(mcp.call("fs_upload_chunk", {"root":"workspace","id":session["id"],"offset":len(first),"sha256":digest(last),"data_base64":base64.b64encode(last).decode()}))
        self.assertEqual(received["offset"],len(content))
        self.poll(self.execute("fs_upload_commit", {"root":"workspace","id":session["id"]},client),client=client)
        self.assertEqual(digest((self.workspace / "large-transfer.bin").read_bytes()),digest(content))
        download = "/api/v1/files/download?root=workspace&path=large-transfer.bin"
        response = binary_request(client,"GET",download)
        self.assertEqual(response[0],200)
        self.assertEqual(digest(response[1]),digest(content))
        response = binary_request(client,"HEAD",download)
        self.assertEqual(response[0],200)
        self.assertEqual(response[1],b"")
        self.assertEqual(int(response[2]["Content-Length"]),len(content))
        response = binary_request(client,"GET",download,headers={"Range":"bytes=17-1024"})
        self.assertEqual(response[0],206)
        self.assertEqual(response[1],content[17:1025])
        bounded = tool_data(mcp.call("fs_download", {"root":"workspace","path":"large-transfer.bin","offset":123,"length":32768}))
        self.assertEqual(base64.b64decode(bounded["data_base64"]),content[123:123+32768])
        self.assertFalse(bounded["complete"])
        stranger = APIClient(self.server.url,self.token()["token"])
        self.assertEqual(stranger.post("/api/v1/actions/fs_upload_status", {"root":"workspace","id":session["id"]})[0],403)
        anonymous = APIClient(self.server.url)
        self.assertEqual(binary_request(anonymous,"GET",download)[0],401)
        data(self.admin.delete("/api/v1/tokens/" + token["id"]))
        self.assertEqual(binary_request(client,"GET",download)[0],401)

    def test_08_upload_oversize_checksum_csrf_rollback_and_cancel(self):
        token = self.token()
        client = APIClient(self.server.url,token["token"])
        session = self.execute("fs_upload_begin", {"root":"workspace","path":"must-not-commit","size":6 << 20},client)
        route = "/api/v1/files/uploads/" + session["id"] + "/chunk?root=workspace&offset=0"
        response = binary_request(client,"PATCH",route,b"x" * ((4 << 20)+1))
        self.assertEqual(response[0],413,response[1][:300])
        self.assertEqual(self.execute("fs_upload_status", {"root":"workspace","id":session["id"]},client)["offset"],0)
        response = binary_request(client,"PATCH",route + "&sha256=" + digest(b"different"),b"real")
        self.assertEqual(response[0],400,response[1][:300])
        self.assertEqual(self.execute("fs_upload_status", {"root":"workspace","id":session["id"]},client)["offset"],0)
        self.execute("fs_upload_cancel", {"root":"workspace","id":session["id"]},client)
        self.assertFalse((self.workspace / "must-not-commit").exists())
        human = self.execute("fs_upload_begin", {"root":"workspace","path":"csrf","size":4})
        humanroute = "/api/v1/files/uploads/" + human["id"] + "/chunk?root=workspace&offset=0"
        self.assertEqual(binary_request(self.admin,"PATCH",humanroute,b"csrf",csrf=False)[0],403)
        self.assertEqual(self.execute("fs_upload_status", {"root":"workspace","id":human["id"]})["offset"],0)

    def test_09_real_archive_roundtrip_and_malicious_entries(self):
        self.execute("fs_mkdir", {"root":"workspace","path":"archive-input"})
        self.make("archive-input/中文.txt", "archive unicode content")
        self.make("archive-input/.hidden", "hidden content")
        formats = self.execute("fs_formats")
        mapping = {f["format"]:f for f in formats}
        for format in ("zip","tar","tar.gz"):
            with self.subTest(format=format):
                self.assertTrue(mapping[format]["create"] and mapping[format]["extract"])
                destination = "bundle." + format
                self.poll(self.execute("fs_archive", {"root":"workspace","path":"archive-input","format":format,"destination_root":"workspace","destination_path":destination}))
                target = "extracted-" + format.replace(".","-")
                self.poll(self.execute("fs_extract", {"root":"workspace","path":destination,"format":format,"destination_root":"workspace","destination_path":target}))
                for original in (self.workspace / "archive-input").rglob("*"):
                    if original.is_file():
                        candidates = list((self.workspace / target).rglob(original.name))
                        self.assertEqual(len(candidates),1)
                        self.assertEqual(candidates[0].read_bytes(),original.read_bytes())
        for label,filename,blob in self.bad_archives():
            with self.subTest(attack=label):
                (self.workspace / filename).write_bytes(blob)
                self.poll(self.execute("fs_extract", {"root":"workspace","path":filename,"destination_root":"workspace","destination_path":"reject-" + label}),success=False)
                self.assertFalse((self.server.data_dir / "escaped-archive").exists())

    def bad_archives(self):
        data_out = io.BytesIO()
        with zipfile.ZipFile(data_out,"w") as archive:
            archive.writestr("../../escaped-archive","escaped")
        yield "zip-traversal","bad.zip",data_out.getvalue()
        for label,kind,link in [("tar-symlink",tarfile.SYMTYPE,"/etc/passwd"),("tar-hardlink",tarfile.LNKTYPE,"../../escaped-archive")]:
            data_out = io.BytesIO()
            with tarfile.open(fileobj=data_out,mode="w") as archive:
                item = tarfile.TarInfo("linked")
                item.type=kind
                item.linkname=link
                archive.addfile(item)
            yield label,label + ".tar",data_out.getvalue()

    def test_10_tail_matches_real_line_semantics(self):
        text = "".join("line-%03d\n" % i for i in range(200))
        self.make("tail-fixture",text)
        result = self.execute("fs_tail", {"root":"workspace","path":"tail-fixture","lines":3})
        self.assertEqual(result["content"], "line-197\nline-198\nline-199\n")


    def test_11_stdio_provider_files_share_actual_runtime(self):
        self.make("stdio-managed.txt","human-created actual file")
        token=self.token(scopes=["files:read","files:write"])
        child_env=os.environ.copy()
        child_env["POWERLINUX_MCP_TOKEN"]=token["token"]
        client=StdioMCPClient([self.server.binary,"mcp","--stdio","--endpoint",self.server.url],child_env)
        try:
            client.initialize()
            listed=client.request("tools/list",{})
            self.assertIn("fs_edit",[tool["name"] for tool in listed["result"]["tools"]])
            seen=tool_data(client.request("tools/call",{"name":"fs_read","arguments":{"root":"workspace","path":"stdio-managed.txt"}})["result"])
            self.assertEqual(seen["content"],"human-created actual file")
            changed=tool_data(client.request("tools/call",{"name":"fs_edit","arguments":{"root":"workspace","path":"stdio-managed.txt","expected_hash":seen["sha256"],"content":"stdio external AI same runtime"}})["result"])
            self.assertEqual((self.workspace/"stdio-managed.txt").read_text(),"stdio external AI same runtime")
            self.assertEqual(self.read("stdio-managed.txt")["sha256"],changed["sha256"])
            data(self.admin.delete("/api/v1/tokens/"+token["id"]))
            rejected=client.request("tools/call",{"name":"fs_read","arguments":{"root":"workspace","path":"stdio-managed.txt"}})
            self.assertTrue("error" in rejected or rejected.get("result",{}).get("isError"),"Revoked stdio provider remained authorized")
        finally:
            client.close()


    def test_12_streamed_text_encoding_conflict_csrf_and_idempotency(self):
        path="large-text-gb18030.txt"
        original="原内容🧪"
        self.make(path,original,"gb18030")
        baseline=self.read(path,encoding="gb18030")
        text="中文🧪mixedé\n"*10000
        query=urllib.parse.urlencode({"root":"workspace","path":path,"encoding":"gb18030","expected_hash":baseline["sha256"]})
        route="/api/v1/files/text?"+query
        key="QA-"+secrets.token_hex(16)
        response=binary_request(self.admin,"PUT",route,text.encode("utf-8"),headers={"Content-Type":"text/plain;charset=utf-8","Idempotency-Key":key})
        self.assertEqual(response[0],200,response[1][:300])
        saved=json.loads(response[1])["data"]
        self.assertEqual((self.workspace/path).read_bytes(),text.encode("gb18030"))
        replay=binary_request(self.admin,"PUT",route,text.encode("utf-8"),headers={"Idempotency-Key":key})
        self.assertEqual(replay[0],200,replay[1][:300])
        self.assertEqual(json.loads(replay[1])["data"],saved)
        history=self.execute("fs_history",{"root":"workspace","path":path})
        self.assertEqual(len(history),2)
        # A distinct intent with a stale baseline must reject; replay cannot add a revision.
        stale=binary_request(self.admin,"PUT",route,b"stale overwrite",headers={"Idempotency-Key":"QA-"+secrets.token_hex(16)})
        self.assertEqual(stale[0],409,stale[1][:300])
        self.assertEqual((self.workspace/path).read_bytes(),text.encode("gb18030"))
        current_route="/api/v1/files/text?"+urllib.parse.urlencode({"root":"workspace","path":path,"encoding":"gb18030","expected_hash":saved["sha256"]})
        self.assertEqual(binary_request(self.admin,"PUT",current_route,b"csrf bypass",csrf=False)[0],403)
        self.assertEqual(binary_request(self.admin,"PUT",current_route,b"\xff\xfe")[0],400)
        over=binary_request(self.admin,"PUT",current_route,b"x"*((2 << 20)+1))
        self.assertEqual(over[0],413,over[1][:300])
        self.assertEqual((self.workspace/path).read_bytes(),text.encode("gb18030"))
        denied=APIClient(self.server.url,self.token(scopes=["files:read"])["token"])
        self.assertEqual(binary_request(denied,"PUT",current_route,b"write without scope")[0],403)


    def test_13_recycle_toggle_persists_and_preserves_prior_entries(self):
        initial=self.execute("fs_trash_settings",{"root":"workspace"})
        self.assertTrue(initial["enabled"])
        prior=self.execute("fs_trash_list",{"root":"workspace"})
        self.make("trash-toggle-retained.txt","previously recycled content")
        recycled=self.poll(self.execute("fs_delete",{"root":"workspace","path":"trash-toggle-retained.txt"}))
        self.assertFalse((self.workspace/"trash-toggle-retained.txt").exists())
        saved=self.execute("fs_trash_configure",{"root":"workspace","enabled":False})
        self.assertFalse(saved["enabled"])
        self.server.restart()
        self.assertFalse(self.execute("fs_trash_settings",{"root":"workspace"})["enabled"])
        self.make("trash-toggle-permanent.txt","new deletion must be permanent")
        removed=self.poll(self.execute("fs_delete",{"root":"workspace","path":"trash-toggle-permanent.txt"}))
        self.assertTrue(removed["deleted"] and removed["permanent"])
        self.assertFalse((self.workspace/"trash-toggle-permanent.txt").exists())
        remaining=self.execute("fs_trash_list",{"root":"workspace"})
        self.assertEqual({x["id"] for x in remaining},{x["id"] for x in prior}|{recycled["id"]})
        reader=APIClient(self.server.url,self.token(scopes=["files:read"])["token"])
        self.assertEqual(reader.post("/api/v1/actions/fs_trash_configure",{"root":"workspace","enabled":True})[0],403)
        self.assertEqual(reader.post("/api/v1/actions/fs_trash_clear",{"root":"workspace"})[0],403)
        cleared=self.poll(self.execute("fs_trash_clear",{"root":"workspace"}))
        self.assertEqual(cleared["completed"],len(remaining))
        self.assertEqual(cleared["failed"],0)
        self.assertEqual(self.execute("fs_trash_list",{"root":"workspace"}),[])
        self.execute("fs_trash_configure",{"root":"workspace","enabled":True})


    def test_14_batch_preflight_rejects_stale_and_unsafe_before_mutation(self):
        for directory in ("batch-source","batch-destination"):
            self.execute("fs_mkdir",{"root":"workspace","path":directory})
        for name in ("a.txt","b.txt"):
            self.make("batch-source/"+name,"new-"+name)
            self.make("batch-destination/"+name,"old-"+name)
        inputs={"root":"workspace","path":"batch-source","items":["a.txt","b.txt"],"operation":"copy","destination_root":"workspace","destination_path":"batch-destination"}
        preflight=self.poll(self.execute("fs_preflight",inputs))
        self.assertEqual((preflight["total"],preflight["conflicts"]),(2,2))
        hashes={x["destination"]:x["current_hash"] for x in preflight["items"]}
        self.assertEqual(set(hashes),{"batch-destination/a.txt","batch-destination/b.txt"})
        token=self.token()
        mcp=HTTPMCPClient(self.server.url+"/mcp",token["token"])
        mcp.initialize()
        tool_data(mcp.call("fs_edit",{"root":"workspace","path":"batch-destination/b.txt","content":"AI changed destination after preflight","expected_hash":hashes["batch-destination/b.txt"]}))
        rejected=self.poll(self.execute("fs_batch",{**inputs,"expected_hashes":hashes}),success=False)
        self.assertEqual(rejected["status"],"failed")
        self.assertEqual(self.read("batch-destination/a.txt")["content"],"old-a.txt")
        self.assertEqual(self.read("batch-destination/b.txt")["content"],"AI changed destination after preflight")
        unsafe=self.poll(self.execute("fs_batch",{**inputs,"items":["a.txt","../outside"],"expected_hashes":hashes}),success=False)
        self.assertEqual(unsafe["status"],"failed")
        self.assertEqual(self.read("batch-destination/a.txt")["content"],"old-a.txt")
        refreshed=self.poll(self.execute("fs_preflight",inputs))
        fresh={x["destination"]:x["current_hash"] for x in refreshed["items"]}
        queued=tool_data(mcp.call("fs_batch",{**inputs,"expected_hashes":fresh}))
        result=self.poll(queued)
        self.assertEqual((result["completed"],result["failed"]),(2,0))
        for name in inputs["items"]:
            self.assertEqual(self.read("batch-destination/"+name)["content"],"new-"+name)
            revisions=self.execute("fs_history",{"root":"workspace","path":"batch-destination/"+name})
            self.assertEqual(revisions[0]["actor_id"],"token:"+token["id"])
        # Generic batch write authority must not bypass the dedicated chmod
        # permission merely because the action name remains fs_batch.
        permissions_before=[(self.workspace/"batch-source"/name).stat().st_mode for name in inputs["items"]]
        permissionless=APIClient(self.server.url,self.token(scopes=["files:read","files:write","tasks:read"])["token"])
        denied=permissionless.post("/api/v1/actions/fs_batch",{"root":"workspace","path":"batch-source","items":inputs["items"],"operation":"chmod","mode":0o644})
        self.assertEqual(denied[0],403)
        self.assertEqual(permissions_before,[(self.workspace/"batch-source"/name).stat().st_mode for name in inputs["items"]])
        # A later nested symlink must prevent publishing any staged directory,
        # even when earlier regular entries could already have been copied.
        self.execute("fs_mkdir",{"root":"workspace","path":"staged-copy-source"})
        self.make("staged-copy-source/000-safe.txt","safe pre-symlink content")
        (self.workspace/"staged-copy-source/zzz-link").symlink_to(self.workspace/"batch-source/a.txt")
        self.poll(self.execute("fs_copy",{"root":"workspace","path":"staged-copy-source","destination_root":"workspace","destination_path":"staged-copy-must-not-publish","recursive":True}),success=False)
        self.assertFalse((self.workspace/"staged-copy-must-not-publish").exists())
        self.assertEqual(self.read("staged-copy-source/000-safe.txt")["content"],"safe pre-symlink content")


if __name__ == "__main__":
    unittest.main(verbosity=2)
