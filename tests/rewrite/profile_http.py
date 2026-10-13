#!/usr/bin/env python3
"""Separate test-client construction cost from actual localhost HTTP latency."""
from concurrent.futures import ThreadPoolExecutor
import json
import os
import secrets
import statistics
import threading
import time
from fixture import RunningServer
from http_client import APIClient, data


def stats(samples):
    return {"p50_ms": statistics.median(samples), "p95_ms": sorted(samples)[int(len(samples)*.95)-1]}


def main():
    server = RunningServer(os.environ["POWERLINUX_TEST_BINARY"])
    try:
        admin = APIClient(server.url)
        password = "QA-" + secrets.token_urlsafe(24)
        data(admin.post("/api/v1/auth/setup", {"username":"qa-profile","password":password,"setup_token":server.bootstrap}))
        session=data(admin.post("/api/v1/auth/login", {"username":"qa-profile","password":password}))
        admin.csrf=session["csrf_token"]
        token=data(admin.post("/api/v1/tokens", {"name":"QA profile", "scopes":["system:read"],"resources":["host:local"]}))
        def construct(_):
            start=time.perf_counter(); APIClient(server.url)
            return (time.perf_counter()-start)*1000
        result={"conditions":{"concurrency":20,"requests":200,"separate_constructor_and_http":True}}
        with ThreadPoolExecutor(max_workers=20) as pool:
            result["client_constructor"]=stats(list(pool.map(construct,range(200))))
        for name,path,bearer in [("health_no_db","/healthz",""),("session_one_count","/api/v1/session",""),
                                 ("status_pat","/api/v1/system/status",token["token"])]:
            local=threading.local()
            def query(_):
                if not hasattr(local,"client"):
                    local.client=APIClient(server.url,bearer)
                start=time.perf_counter(); response=local.client.get(path)
                assert response[0]==200
                return (time.perf_counter()-start)*1000
            with ThreadPoolExecutor(max_workers=20) as pool:
                # Warm each worker's client before recording the steady-state batch.
                list(pool.map(query,range(40)))
                result[name]=stats(list(pool.map(query,range(200))))
        print(json.dumps(result,indent=2))
    finally:
        server.close()

if __name__=="__main__":main()
