#!/usr/bin/env python3
"""Actual production Agent PTY/OpenSSH and REST/MCP/WebSocket identities.

Requires test_agent_install.py's private actual VM fixture. Never starts a
root Agent on the shared host; all shell/file probes run in our disposable VM.
Protected-file probes discard every byte and report only permission booleans.
"""
from __future__ import annotations
import base64
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from http_client import APIClient,data
from mcp_client import HTTPMCPClient,StdioMCPClient,tool_data
from websockets.sync.client import connect
from websockets.exceptions import ConnectionClosed,InvalidStatus


def private_json(path):
    file=Path(path)
    assert file.stat().st_mode & 0o077 == 0,"Fixture credentials must be private"
    return json.loads(file.read_text())


def plain(result):
    return b"".join(base64.b64decode(frame["data"]) for frame in result.get("frames",[])).decode(errors="replace")


class ActualVMTerminal(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fixture=private_json(os.environ.get("POWERLINUX_QA_PANEL_FILE","/workspace/tools/qa-ubuntu/agent-acceptance/panel-fixture-private.json"))
        cls.ssh_fixture=private_json(os.environ.get("POWERLINUX_QA_SSH_FILE","/workspace/tools/qa-ubuntu/ssh-terminal-fixtures/credentials.json"))
        cls.remote_script=Path(cls.fixture["guest"])/"ssh.sh"
        identity=subprocess.check_output([str(cls.remote_script),"cat /proc/1/comm; systemd-detect-virt"],timeout=30).decode().splitlines()
        assert identity==["systemd","qemu"],"Requires actual owned QEMU/systemd production target"
        cls.api=APIClient(cls.fixture["url"])
        cls.api.csrf=data(cls.api.post("/api/v1/auth/login",{"username":cls.fixture["username"],"password":cls.fixture["password"]}))["csrf_token"]
        status=cls.api.get("/healthz")
        assert status[0]==200 and status[1]["version"]==cls.fixture["version"],"Actual installed version changed"
        cls.url=cls.fixture["url"]
        cls.local=os.environ.get("POWERLINUX_QA_TERMINAL_TARGET","local")

    def execute(self,name,values=None,client=None):
        status,result,_=(client or self.api).post("/api/v1/actions/"+name,values or {})
        self.assertEqual(status,200,f"{name} HTTP{status}, code={result.get('error',{}).get('code')}")
        return result["data"]

    def token(self,scopes,resources):
        return data(self.api.post("/api/v1/tokens",{"name":"Actual VM terminal external AI","kind":"mcp","scopes":scopes,"resources":resources}))

    def open(self,target=None):
        result=self.execute("terminal_open",{"target_id":target or self.local,"rows":25,"cols":80})
        self.addCleanup(lambda:self.api.post("/api/v1/actions/terminal_close",{"id":result["id"]}))
        ready="QA-SHELL-READY-"+secrets.token_hex(12)
        self.send(result["id"],"stty -echo; printf '\\n"+ready+"\\n'\n")
        # SSH can echo queued input during its real login/MOTD before stty
        # executes. An exact output line cannot match the quoted command.
        self.output(result["id"],"\r\n"+ready+"\r\n")
        return result

    def send(self,session,text,client=None):
        return self.execute("terminal_input",{"id":session,"data":base64.b64encode(text.encode()).decode()},client)

    def output(self,session,expected,client=None):
        seen=""
        after=0
        deadline=time.monotonic()+20
        while time.monotonic()<deadline:
            result=self.execute("terminal_read",{"id":session,"after":after,"limit":8},client)
            seen+=plain(result)
            after=result["next"]
            if expected in seen:
                return seen
            time.sleep(.08)
        self.fail("Actual terminal output did not contain the controlled expected result")

    def test_01_actual_local_pty_independent_uid_groups_and_private_data(self):
        session=self.open()
        identities=subprocess.check_output([str(self.remote_script),"id -u powerlinux; id -u powerlinux-terminal; id -g powerlinux-terminal; systemctl show powerlinux.service -p MainPID --value"],timeout=30).decode().splitlines()
        api_uid,terminal_uid,terminal_gid,api_pid=map(int,identities)
        subprocess.run([str(self.remote_script),"umask 077; printf '%s' 'controlled QA permission canary' > /var/lib/powerlinux-agent/qa-private-canary"],check=True,capture_output=True,timeout=30)
        self.assertNotEqual(api_uid,terminal_uid)
        self.assertNotEqual(terminal_uid,0)
        self.send(session["id"],"printf 'QA-UID:%s GROUPS:%s\\n' \"$(id -u)\" \"$(id -G)\"; test -t 0 && test -t 1 && printf 'QA-ACTUAL-TTY\\n'\n")
        result=self.output(session["id"],"QA-ACTUAL-TTY")
        self.assertIn(f"QA-UID:{terminal_uid} GROUPS:{terminal_gid}\r\n",result,"PTY retained API/supplementary groups")
        for index,path in enumerate(("/var/lib/powerlinux/powerlinux.db","/var/lib/powerlinux/encryption-key","/etc/powerlinux/agent-policy.json","/var/lib/powerlinux-agent/qa-private-canary",f"/proc/{api_pid}/environ")):
            command=f"if head -c 1 {path} >/dev/null 2>&1; then printf 'QA-PRIVATE-{index}-READABLE\\n'; else printf 'QA-PRIVATE-{index}-DENIED\\n'; fi\n"
            self.send(session["id"],command)
            observed=self.output(session["id"],f"QA-PRIVATE-{index}-DENIED")
            self.assertNotIn(f"QA-PRIVATE-{index}-READABLE",observed,"Local terminal read protected API/Agent data")
        self.execute("terminal_resize",{"id":session["id"],"rows":41,"cols":109})
        self.send(session["id"],"printf 'QA-SIZE:'; stty size\n")
        self.assertIn("QA-SIZE:41 109",self.output(session["id"],"QA-SIZE:41 109"))
        denied=APIClient(self.url,self.token(["terminal:connect"],["host:local"])["token"])
        self.assertEqual(denied.post("/api/v1/actions/terminal_open",{"target_id":self.local})[0],403)
        self.assertEqual(self.api.post("/api/v1/actions/terminal_open",{"target_id":self.local,"root_authorized":True})[0],400)

    def test_02_real_mcp_and_websocket_share_session_and_revoke(self):
        session=self.open()
        resource="terminal:"+self.local
        isolated=APIClient(self.url,self.token(["terminal:connect"],[resource])["token"])
        self.assertEqual(isolated.post("/api/v1/actions/terminal_read",{"id":session["id"]})[0],403)
        token=self.token(["terminal:connect","terminal:share"],[resource])
        client=APIClient(self.url,token["token"])
        mcp=HTTPMCPClient(self.url+"/mcp",token["token"])
        mcp.initialize()
        shared=tool_data(mcp.call("terminal_share_discover",{"id":session["id"]}))
        self.assertEqual(shared["actor_id"],session["actor_id"])
        payload=base64.b64encode(b"printf 'QA-ACTUAL-MCP-SHARED\\n'\n").decode()
        tool_data(mcp.call("terminal_input",{"id":session["id"],"data":payload}))
        self.output(session["id"],"QA-ACTUAL-MCP-SHARED")
        binary=os.environ["POWERLINUX_TEST_BINARY"]
        child_env=os.environ.copy()
        child_env["POWERLINUX_MCP_TOKEN"]=token["token"]
        stdio=StdioMCPClient([binary,"mcp","--stdio","--endpoint",self.url],child_env)
        self.addCleanup(stdio.close)
        stdio.initialize()
        wire=tool_data(stdio.request("tools/call",{"name":"terminal_read","arguments":{"id":session["id"],"limit":8}})["result"])
        self.assertEqual(wire["id"],session["id"])
        ws_url=self.url.replace("http://","ws://",1)+"/api/v1/terminal/sessions/"+session["id"]+"/ws"
        cookies="; ".join(c.name+"="+c.value for c in self.api.cookies)
        with self.assertRaises(InvalidStatus) as without_csrf:
            connect(ws_url,origin=self.url,additional_headers={"Cookie":cookies},proxy=None,open_timeout=10)
        self.assertEqual(without_csrf.exception.response.status_code,403)
        with self.assertRaises(InvalidStatus) as foreign_origin:
            connect(ws_url,origin="https://foreign.invalid",subprotocols=["powerlinux-csrf."+self.api.csrf],additional_headers={"Cookie":cookies},proxy=None,open_timeout=10)
        self.assertEqual(foreign_origin.exception.response.status_code,403)
        with connect(ws_url,origin=self.url,subprotocols=["powerlinux-terminal-v1","powerlinux-csrf."+self.api.csrf],additional_headers={"Cookie":cookies},proxy=None,open_timeout=10) as human_socket:
            human_socket.send(json.dumps({"type":"renew"}))
            deadline=time.monotonic()+20
            while True:
                self.assertLess(time.monotonic(),deadline,"Authorized human cookie WebSocket did not renew")
                try:
                    response=json.loads(human_socket.recv(timeout=2))
                except TimeoutError:
                    continue
                if response["type"]=="ack" and response["operation"]=="renew":
                    self.assertEqual(response["data"]["id"],session["id"])
                    break
        with connect(ws_url,origin=self.url,additional_headers={"Authorization":"Bearer "+token["token"]},proxy=None,open_timeout=10) as socket:
            key="actual-ws-"+secrets.token_hex(10)
            socket.send(json.dumps({"type":"input","data":base64.b64encode(b"printf 'QA-ACTUAL-WS-SHARED\\n'\n").decode(),"idempotency_key":key}))
            observed=False
            deadline=time.monotonic()+20
            while not observed:
                self.assertLess(time.monotonic(),deadline,"Real WebSocket terminal produced no output")
                try:
                    message=json.loads(socket.recv(timeout=2))
                except TimeoutError:
                    continue
                if message["type"]=="output" and "QA-ACTUAL-WS-SHARED" in plain(message["data"]):
                    observed=True
            self.output(session["id"],"QA-ACTUAL-WS-SHARED")
            data(self.api.delete("/api/v1/tokens/"+token["id"]))
            with self.assertRaises(ConnectionClosed):
                while True:
                    socket.recv(timeout=5)
        self.assertEqual(client.post("/api/v1/actions/terminal_read",{"id":session["id"]})[0],401)
        revoked=stdio.request("tools/call",{"name":"terminal_read","arguments":{"id":session["id"]}})
        self.assertTrue("error" in revoked or revoked.get("result",{}).get("isError"))
        # The human's session remains usable after the external identity ends.
        self.send(session["id"],"printf 'QA-HUMAN-AFTER-REVOKE\\n'\n")
        self.output(session["id"],"QA-HUMAN-AFTER-REVOKE")
        audit=[row for offset in (0,100) for row in data(self.api.get(f"/api/v1/audit?limit=100&offset={offset}"))]
        self.assertTrue(any(x["action"]=="terminal_input" and x["actor"]["id"]=="token:"+token["id"] for x in audit))
        self.assertFalse("QA-ACTUAL-MCP-SHARED" in json.dumps(audit),"Terminal input content leaked through audit")

    def test_03_actual_openssh_pty_hostkey_failure_and_private_credentials(self):
        fixture=self.ssh_fixture
        key=Path(fixture["private_key_file"])
        self.assertEqual(key.stat().st_mode & 0o077,0)
        private=key.read_text()
        base={"label":"Actual VM SSH "+secrets.token_hex(8),"host":fixture["host"],"port":fixture["port"],
              "username":fixture["username"],"auth_type":"key","host_key":fixture["host_key"],"private_key":private}
        target=self.execute("terminal_targets_create",base)
        self.addCleanup(lambda:self.api.post("/api/v1/actions/terminal_targets_delete",{"id":target["id"]}))
        session=self.open(target["id"])
        self.assertEqual(session["type"],"ssh")
        self.send(session["id"],"printf 'QA-SSH-UID:%s\\n' \"$(id -u)\"; test -t 0 && printf 'QA-SSH-REAL-PTY\\n'\n")
        actual=self.output(session["id"],"QA-SSH-REAL-PTY")
        self.assertIn("QA-SSH-UID:"+str(fixture["user_id"]),actual)
        self.execute("terminal_resize",{"id":session["id"],"rows":47,"cols":117})
        self.send(session["id"],"printf 'QA-SSH-SIZE:'; stty size\n")
        self.output(session["id"],"QA-SSH-SIZE:47 117")
        bad=self.execute("terminal_targets_create",{**base,"label":"Wrong host pin "+secrets.token_hex(8),"host_key":"SHA256:"+base64.b64encode(bytes(32)).decode().rstrip("=")})
        self.addCleanup(lambda:self.api.post("/api/v1/actions/terminal_targets_delete",{"id":bad["id"]}))
        failed=self.api.post("/api/v1/actions/terminal_open",{"target_id":bad["id"]})
        self.assertEqual(failed[0],403,"Actual SSH wrong host-key pin was accepted")
        self.assertIn(failed[1]["error"]["code"],("ssh_auth_failed","host_key_mismatch"))
        metadata=self.execute("terminal_targets_get",{"id":target["id"]})
        self.assertNotIn("secret_ref",metadata)
        self.assertFalse(private in json.dumps(metadata),"SSH private key returned by metadata")
        audit=[row for offset in (0,100) for row in data(self.api.get(f"/api/v1/audit?limit=100&offset={offset}"))]
        self.assertFalse("PRIVATE KEY" in json.dumps(audit),"SSH key leaked through audit")
        # Persistent host/quick-command configuration is checked separately
        # from live PTY process lifetime; no fake session replay is accepted.
        title="Actual persisted command "+secrets.token_hex(8)
        saved=self.execute("terminal_commands_create",{"title":title,"command":"printf 'QA shortcut command'"})
        self.addCleanup(lambda:self.api.post("/api/v1/actions/terminal_commands_delete",{"id":saved["id"]}))
        self.execute("terminal_close",{"id":session["id"]})
        subprocess.run([str(self.remote_script),"systemctl restart powerlinux-agent.service"],check=True,capture_output=True,timeout=90)
        deadline=time.monotonic()+45
        while True:
            status=self.api.post("/api/v1/actions/terminal_targets_get",{"id":target["id"]})
            if status[0]==200:
                break
            self.assertLess(time.monotonic(),deadline,"Actual Agent did not recover persisted target")
            time.sleep(.2)
        self.assertEqual(status[1]["data"]["host_key"],fixture["host_key"])
        commands=self.execute("terminal_commands_list")
        if isinstance(commands,dict):
            commands=commands["items"]
        self.assertTrue(any(command["id"]==saved["id"] and command["title"]==title for command in commands))


if __name__=="__main__":
    unittest.main(verbosity=2)
