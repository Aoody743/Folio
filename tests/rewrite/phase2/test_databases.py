#!/usr/bin/env python3
"""Actual SQL/Mongo/cache targets through the packaged REST and native MCP.

Credentials must be private fixture JSON outside the checkout. These tests
cover connection/data/identity behavior, not installation, backups, replicas,
Oracle MySQL, or 23 complete plugin lifecycles.
"""
from __future__ import annotations
import hashlib
import json
import os
from pathlib import Path
import secrets
import sys
import time
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from fixture import RunningServer
from http_client import APIClient,data
from mcp_client import HTTPMCPClient,StdioMCPClient,tool_data


def read_private(path):
    file=Path(path)
    if file.stat().st_mode & 0o077:
        raise RuntimeError("Database fixture credentials must be private")
    return json.loads(file.read_text())


def cache_resource(profile):
    namespace=str(profile["database"])+"|"+profile["prefix"]
    return "cache:"+profile["id"]+":keys:"+hashlib.sha256(namespace.encode()).hexdigest()[:16]


class RealDatabaseAdapters(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        binary=os.environ.get("POWERLINUX_TEST_BINARY")
        if not binary:
            raise RuntimeError("POWERLINUX_TEST_BINARY must identify an actual immutable release executable")
        cls.fixtures=read_private(os.environ.get("POWERLINUX_QA_DATABASE_FILE","/workspace/tools/qa-vm/database-fixtures/credentials.json"))
        cls.fixtures.update(read_private(os.environ.get("POWERLINUX_QA_MONGO_FILE","/workspace/tools/qa-mongodb-fixture-20261013/credentials.json")))
        cls.server=RunningServer(binary)
        cls.addClassCleanup(cls.server.close)
        cls.admin=APIClient(cls.server.url)
        password="QA-"+secrets.token_urlsafe(24)
        data(cls.admin.post("/api/v1/auth/setup",{"username":"database-qa","password":password,"setup_token":cls.server.bootstrap}))
        session=data(cls.admin.post("/api/v1/auth/login",{"username":"database-qa","password":password}))
        cls.admin.csrf=session["csrf_token"]
        cls.registered={}
        for key in ("mariadb","mariadb_admin","postgresql","postgresql_admin","mongodb","mongodb_admin","redis","memcached"):
            config=dict(cls.fixtures[key])
            config["expected_revision"]=0
            if key not in ("redis","memcached"):
                config["name"]="Actual QA "+key
            status,result,_=cls.admin.post("/api/v1/actions/"+("cache_connection_register" if key in ("redis","memcached") else "database_connection_register"),config)
            if status!=200 or "data" not in result:
                # Never print registration bodies or diagnostics that might
                # contain provider credentials before leak checks can run.
                raise AssertionError(f"Actual {key} registration failed: HTTP{status}, code="+str(result.get("error",{}).get("code")))
            cls.registered[key]=result["data"]

    def execute(self,name,values=None,client=None):
        status,result,_=(client or self.admin).post("/api/v1/actions/"+name,values or {})
        self.assertEqual(status,200,f"{name} failed HTTP{status}, code={result.get('error',{}).get('code')}")
        self.assertIn("data",result)
        return result["data"]

    def token(self,scopes,resources):
        return data(self.admin.post("/api/v1/tokens",{"name":"Actual database/cache external AI","kind":"mcp","scopes":scopes,"resources":resources}))

    def query(self,key,sql,parameters=None,mode="query",client=None,**extra):
        return self.execute("database_query",{"id":self.fixtures[key]["id"],"sql":sql,"parameters":parameters or [],"mode":mode,**extra},client)

    def test_01_actual_status_and_metadata_have_no_credentials(self):
        all_metadata=[]
        for key,profile in self.registered.items():
            name="cache_status" if key in ("redis","memcached") else "database_status"
            status=self.execute(name,{"id":profile["id"]})
            self.assertTrue(status["connected"],key)
            self.assertEqual(status["engine"],self.fixtures[key]["engine"])
            self.assertIn("sampled_at",status)
            all_metadata.extend((profile,status))
        for key in ("mariadb","postgresql"):
            tables=self.execute("database_tables_list",{"id":self.fixtures[key]["id"]})
            names={row.get("table_name") or row.get("TABLE_NAME") or row.get("tablename") for row in tables["items"]}
            self.assertIn("qa_records",names)
        all_metadata.append(self.execute("database_connections_list"))
        all_metadata.append(self.execute("cache_connections_list"))
        encoded=json.dumps(all_metadata)
        for config in self.fixtures.values():
            if isinstance(config,dict) and config.get("password"):
                self.assertFalse(config["password"] in encoded,"Raw database/cache credential returned by metadata")

    def test_02_dangerous_database_grants_are_rejected_before_persistence(self):
        for key in ("postgresql_danger","mariadb_danger","mongodb_global"):
            with self.subTest(identity=key):
                config={**self.fixtures[key],"name":"Actual negative scope fixture","expected_revision":0}
                status,result,_=self.admin.post("/api/v1/actions/database_connection_register",config)
                self.assertEqual(status,403,f"{key} excessive credential scope accepted or failed to authenticate properly")
                self.assertEqual(result["error"]["code"],"credential_scope")
                items=self.execute("database_connections_list")["items"]
                self.assertNotIn(config["id"],[item["id"] for item in items])

    def test_03_real_parameterized_sql_crud_paging_and_mcp(self):
        for key in ("mariadb","postgresql"):
            with self.subTest(engine=key):
                identity=self.fixtures[key]
                token=self.token(["databases:read","databases:query"],["db:"+identity["id"]+":"+identity["database"]])
                mcp=HTTPMCPClient(self.server.url+"/mcp",token["token"])
                mcp.initialize()
                parameter="?" if key=="mariadb" else "$1"
                row_id=secrets.randbelow(100000000)+10000
                label="actual UTF8 中文 '; DROP TABLE qa_records; -- " + secrets.token_hex(8)
                try:
                    params=[row_id,label]
                    placeholders="?,?" if key=="mariadb" else "$1,$2"
                    changed=tool_data(mcp.call("database_query",{"id":identity["id"],"sql":"INSERT INTO qa_records(id,label) VALUES("+placeholders+")","parameters":params,"mode":"execute"}))
                    self.assertEqual(changed["rows_affected"],1)
                    read=self.query(key,"SELECT id,label FROM qa_records WHERE id="+parameter,[row_id])
                    self.assertEqual(read["items"],[{"id":row_id,"label":label}])
                    paged=self.query(key,"SELECT id,label FROM qa_records ORDER BY id",limit=1)
                    self.assertEqual(paged["count"],1)
                    self.assertTrue(paged["truncated"])
                    update="UPDATE qa_records SET label=? WHERE id=?" if key=="mariadb" else "UPDATE qa_records SET label=$1 WHERE id=$2"
                    self.query(key,update,["actual updated content",row_id],mode="execute")
                    self.assertEqual(self.query(key,"SELECT label FROM qa_records WHERE id="+parameter,[row_id])["items"],[{"label":"actual updated content"}])
                finally:
                    self.query(key,"DELETE FROM qa_records WHERE id="+parameter,[row_id],mode="execute")

    def test_04_database_resources_and_instance_credentials_are_distinct(self):
        profile=self.fixtures["mariadb"]
        resource="db:"+profile["id"]+":"+profile["database"]
        token=self.token(["databases:read","databases:query"],[resource])
        client=APIClient(self.server.url,token["token"])
        listed=self.execute("database_connections_list",client=client)
        self.assertEqual([p["id"] for p in listed["items"]],[profile["id"]])
        foreign=self.fixtures["postgresql"]
        self.assertEqual(client.post("/api/v1/actions/database_status",{"id":foreign["id"]})[0],403)
        self.assertEqual(client.post("/api/v1/actions/database_query",{"id":profile["id"],"database":"qa_scope_private","sql":"SELECT 1"})[0],403)
        self.assertEqual(client.post("/api/v1/actions/database_instance_query",{"id":profile["id"],"sql":"SELECT 1"})[0],403)
        instance=self.fixtures["mariadb_admin"]
        status,result,_=self.admin.post("/api/v1/actions/database_query",{"id":instance["id"],"sql":"SELECT 1"})
        self.assertEqual(status,403)
        self.assertEqual(result["error"]["code"],"credential_scope")
        actual=self.execute("database_instance_query",{"id":instance["id"],"sql":"SELECT 1 AS actual_instance_admin"})
        self.assertEqual(actual["items"],[{"actual_instance_admin":1}])
        self.assertEqual(client.post("/api/v1/actions/database_query",{"id":profile["id"],"sql":"SELECT 1","actor":{"kind":"human"}})[0],400)

    def test_05_mongodb_real_crud_and_authorized_pagination(self):
        profile=self.fixtures["mongodb"]
        token=self.token(["databases:read","databases:query","databases:write"],["db:"+profile["id"]+":"+profile["database"]])
        mcp=HTTPMCPClient(self.server.url+"/mcp",token["token"])
        mcp.initialize()
        marker="actual-mongo-qa-"+secrets.token_hex(10)
        common={"id":profile["id"],"collection":"qa_records"}
        try:
            inserted=tool_data(mcp.call("mongodb_documents_insert",{**common,"documents":[{"marker":marker,"label":"中文one"},{"marker":marker,"label":"two"}]}))
            self.assertEqual(inserted["inserted_count"],2)
            first=self.execute("mongodb_documents_find",{**common,"filter":{"marker":marker},"projection":{"_id":0,"label":1},"sort":{"label":1},"limit":1})
            second=self.execute("mongodb_documents_find",{**common,"filter":{"marker":marker},"projection":{"_id":0,"label":1},"sort":{"label":1},"offset":1,"limit":1})
            self.assertTrue(first["truncated"])
            self.assertEqual({row["label"] for row in first["items"]+second["items"]},{"中文one","two"})
            update=self.execute("mongodb_documents_update",{**common,"filter":{"marker":marker,"label":"two"},"update":{"$set":{"label":"updated"}}})
            self.assertEqual(update["modified_count"],1)
            found=self.execute("mongodb_documents_find",{**common,"filter":{"marker":marker,"label":"updated"},"projection":{"_id":0,"label":1}})
            self.assertEqual(found["items"],[{"label":"updated"}])
        finally:
            deleted=self.execute("mongodb_documents_delete",{**common,"filter":{"marker":marker}})
            self.assertEqual(deleted["deleted_count"],2)

    def test_06_real_redis_and_memcached_strings_ttl_and_scope(self):
        for key in ("redis","memcached"):
            with self.subTest(engine=key):
                profile=self.fixtures[key]
                token=self.token(["cache:read","cache:write"],[cache_resource(profile)])
                client=APIClient(self.server.url,token["token"])
                name="actual-string-"+secrets.token_hex(8)
                self.execute("cache_key_set",{"id":profile["id"],"key":name,"value":"实际cache内容","ttl_seconds":1},client)
                found=self.execute("cache_key_get",{"id":profile["id"],"key":name},client)
                self.assertTrue(found["found"])
                self.assertEqual(found["value"],"实际cache内容")
                self.assertEqual([x["id"] for x in self.execute("cache_connections_list",client=client)["items"]],[profile["id"]])
                other=self.fixtures["memcached" if key=="redis" else "redis"]
                self.assertEqual(client.post("/api/v1/actions/cache_key_get",{"id":other["id"],"key":name})[0],403)
                deadline=time.monotonic()+5
                while self.execute("cache_key_get",{"id":profile["id"],"key":name},client)["found"]:
                    self.assertLess(time.monotonic(),deadline,"Actual cache TTL did not expire")
                    time.sleep(.15)
                self.execute("cache_key_set",{"id":profile["id"],"key":name,"value":"","ttl_seconds":0},client)
                self.assertTrue(self.execute("cache_key_get",{"id":profile["id"],"key":name},client)["found"])
                self.execute("cache_keys_delete",{"id":profile["id"],"keys":[name]},client)
                self.assertFalse(self.execute("cache_key_get",{"id":profile["id"],"key":name},client)["found"])

    def test_07_redis_scan_collection_continuations_omit_no_members(self):
        profile=self.fixtures["redis"]
        prefix="actual-scan-"+secrets.token_hex(8)
        names=[prefix+"-"+str(i) for i in range(35)]
        collection_names=[prefix+"-hash",prefix+"-set",prefix+"-list"]
        common={"id":profile["id"]}
        def scan(action,**values):
            cursor=0
            items=[]
            for _ in range(1000):
                result=self.execute(action,{**common,**values,"cursor":cursor,"limit":1})
                items.extend(result["items"])
                cursor=result["cursor"]
                if cursor==0:
                    return items
            self.fail("Actual Redis scan cursor did not terminate")
        try:
            for key in names:
                self.execute("cache_key_set",{**common,"key":key,"value":"scan fixture"})
            seen=set(scan("cache_keys_scan"))
            self.assertTrue(set(names)<=seen,"Actual SCAN discarded members while advancing its cursor")
            fields={"field-"+str(i):"value-"+str(i) for i in range(91)}
            self.execute("cache_hash_set",{**common,"key":collection_names[0],"fields":fields})
            pairs=scan("cache_hash_read",key=collection_names[0])
            self.assertEqual(dict(zip(pairs[::2],pairs[1::2])),fields)
            members=["member-"+str(i) for i in range(91)]
            self.execute("cache_set_add",{**common,"key":collection_names[1],"members":members})
            self.assertEqual(set(scan("cache_set_read",key=collection_names[1])),set(members))
            self.execute("cache_list_push",{**common,"key":collection_names[2],"members":members})
            self.assertEqual(self.execute("cache_list_read",{**common,"key":collection_names[2],"start":10,"stop":12})["items"],members[10:13])
        finally:
            self.execute("cache_keys_delete",{**common,"keys":names+collection_names})

    def test_08_real_profile_cas_restart_and_durable_secret_storage(self):
        profile=self.fixtures["postgresql"]
        stale={**profile,"name":"Stale profile attempt","expected_revision":0}
        self.assertEqual(self.admin.post("/api/v1/actions/database_connection_register",stale)[0],412)
        revision=self.execute("database_connection_get",{"id":profile["id"]})["revision"]
        self.server.restart()
        self.assertEqual(self.execute("database_connection_get",{"id":profile["id"]})["revision"],revision)
        self.assertTrue(self.execute("database_status",{"id":profile["id"]})["connected"])
        audit=[row for offset in (0,100) for row in data(self.admin.get(f"/api/v1/audit?limit=100&offset={offset}"))]
        text=json.dumps(audit)
        actual_state=b"".join(p.read_bytes() for p in self.server.data_dir.glob("powerlinux.db*"))
        for fixture in self.fixtures.values():
            if isinstance(fixture,dict) and fixture.get("password"):
                self.assertFalse(fixture["password"] in text,"Credential leaked in audit")
                self.assertFalse(fixture["password"].encode() in actual_state,"Raw credential persisted in SQLite/WAL")

    def test_09_stdio_database_and_cache_revoke_actual_identity(self):
        database=self.fixtures["postgresql"]
        cache=self.fixtures["redis"]
        token=self.token(["databases:read","databases:query","cache:read"],["db:"+database["id"]+":"+database["database"],cache_resource(cache)])
        child_env=os.environ.copy()
        child_env["POWERLINUX_MCP_TOKEN"]=token["token"]
        client=StdioMCPClient([self.server.binary,"mcp","--stdio","--endpoint",self.server.url],child_env)
        try:
            client.initialize()
            listed=client.request("tools/list",{})
            names=[item["name"] for item in listed["result"]["tools"]]
            self.assertIn("database_query",names)
            self.assertIn("cache_key_get",names)
            self.assertNotIn("database_connection_register",names)
            result=tool_data(client.request("tools/call",{"name":"database_query","arguments":{"id":database["id"],"sql":"SELECT 1 AS actual_stdio"}})["result"])
            self.assertEqual(result["items"],[{"actual_stdio":1}])
            data(self.admin.delete("/api/v1/tokens/"+token["id"]))
            denied=client.request("tools/call",{"name":"cache_status","arguments":{"id":cache["id"]}})
            self.assertTrue("error" in denied or denied.get("result",{}).get("isError"),"Revoked DB/cache stdio identity remained authorized")
        finally:
            client.close()

    def test_10_cache_same_peer_alias_and_prefix_overlap_are_rejected(self):
        original=self.fixtures["redis"]
        cases=("same-namespace","parent-prefix","child-prefix","localhost-alias")
        for index,case in enumerate(cases):
            with self.subTest(boundary=case):
                config={**original,"id":"qa-cache-overlap-"+str(index),"expected_revision":0}
                if case=="parent-prefix":
                    config["prefix"]=original["prefix"].removesuffix(":")
                elif case=="child-prefix":
                    config["prefix"]=original["prefix"]+":child"
                elif case=="localhost-alias":
                    config["host"]="localhost"
                status,result,_=self.admin.post("/api/v1/actions/cache_connection_register",config)
                self.assertEqual(status,409,"Equivalent/overlapping physical namespace accepted as a different resource")
                self.assertEqual(result["error"]["code"],"namespace_conflict")
        current=self.execute("cache_connection_get",{"id":original["id"]})
        self.assertTrue(current["endpoint"],"Actual probed cache peer was not persisted")
        spoof={**original,"id":"qa-cache-endpoint-spoof","endpoint":"127.0.0.1:1","expected_revision":0}
        self.assertEqual(self.admin.post("/api/v1/actions/cache_connection_register",spoof)[0],400)
        self.assertEqual(self.admin.post("/api/v1/actions/cache_connection_delete",{"id":original["id"],"expected_revision":0})[0],412)
        self.assertTrue(self.execute("cache_status",{"id":original["id"]})["connected"])


if __name__=="__main__":
    unittest.main(verbosity=2)
