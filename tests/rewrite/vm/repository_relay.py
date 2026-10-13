#!/usr/bin/env python3
"""Read-only signed-repository transport fixture for the owned QEMU guests.

Fetch real bytes from fixed official distribution HTTPS origins with normal host TLS
verification. Guests still verify the publisher signatures and package hashes.
This is a temporary mirror precondition, not a fake repository or install pass.
"""
from __future__ import annotations
import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import ssl
import threading
import urllib.error
import urllib.parse
import urllib.request

UBUNTU_ORIGINS={"openresty":"https://openresty.org/package/ubuntu/",
         "ubuntu":"https://archive.ubuntu.com/ubuntu/",
         "ubuntu-security":"https://security.ubuntu.com/ubuntu/"}
DEBIAN_ORIGINS={"debian":"https://deb.debian.org/debian/",
                "debian-security":"https://deb.debian.org/debian-security/"}


class OfficialRedirects(urllib.request.HTTPRedirectHandler):
    def __init__(self,origins):
        super().__init__()
        self.origins=origins
    def redirect_request(self,req,fp,code,msg,headers,newurl):
        source=next((origin for origin in self.origins.values() if req.full_url.startswith(origin)),None)
        if source is None or not newurl.startswith(source):
            raise urllib.error.HTTPError(req.full_url,502,"Unreviewed repository redirect",headers,fp)
        return super().redirect_request(req,fp,code,msg,headers,newurl)


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument("--state",required=True,type=Path)
    parser.add_argument("--distribution",choices=("ubuntu","debian"),default="ubuntu")
    args=parser.parse_args()
    args.state.mkdir(parents=True,exist_ok=True,mode=0o700)
    args.state.chmod(0o700)
    context=ssl.create_default_context()
    assert context.check_hostname and context.verify_mode==ssl.CERT_REQUIRED
    origins=DEBIAN_ORIGINS if args.distribution=="debian" else UBUNTU_ORIGINS
    opener=urllib.request.build_opener(OfficialRedirects(origins),urllib.request.HTTPSHandler(context=context))
    log_lock=threading.Lock()
    slots=threading.BoundedSemaphore(8)
    def log(values):
        with log_lock, (args.state/"actual-upstream-requests.jsonl").open("a") as stream:
            stream.write(json.dumps(values)+"\n")
    class Handler(BaseHTTPRequestHandler):
        protocol_version="HTTP/1.0"
        def log_message(self,format,*values):
            pass
        def do_HEAD(self):
            self.fetch(True)
        def do_GET(self):
            self.fetch(False)
        def fetch(self,head):
            parsed=urllib.parse.urlsplit(self.path)
            parts=urllib.parse.unquote(parsed.path).split("/")
            if parsed.scheme or parsed.netloc or parsed.query or parsed.fragment or len(parts)<3 or parts[1] not in origins or any(p in (".","..") or "\\" in p or "\x00" in p or "%" in p for p in parts[2:]):
                self.send_error(403,"Fixed official repository paths only")
                return
            if not slots.acquire(blocking=False):
                self.send_error(503,"Repository relay concurrency bound")
                return
            upstream=origins[parts[1]]+urllib.parse.quote("/".join(parts[2:]),safe="/._-+~")
            sent=False
            size=0
            digest=hashlib.sha256()
            status=502
            try:
                headers={"Accept-Encoding":"identity"}
                for name in ("Range","If-Modified-Since","If-None-Match"):
                    if self.headers.get(name):
                        headers[name]=self.headers[name]
                request=urllib.request.Request(upstream,headers=headers,method="HEAD" if head else "GET")
                try:
                    response=opener.open(request,timeout=60)
                except urllib.error.HTTPError as error:
                    response=error
                with response:
                    status=response.status
                    length=response.headers.get("Content-Length")
                    if length is not None and int(length)>256*1024*1024:
                        raise ValueError("Repository object exceeds fixture byte bound")
                    self.send_response(status)
                    for name in ("Content-Length","Content-Type","Content-Encoding","Last-Modified","ETag","Accept-Ranges","Content-Range"):
                        if response.headers.get(name):
                            self.send_header(name,response.headers[name])
                    self.send_header("Connection","close")
                    self.end_headers()
                    sent=True
                    if not head:
                        while chunk:=response.read(65536):
                            size+=len(chunk)
                            if size>256*1024*1024:
                                raise ValueError("Repository object exceeds fixture byte bound")
                            digest.update(chunk)
                            self.wfile.write(chunk)
                log({"upstream":upstream,"status":status,"bytes":size,"sha256":None if head else digest.hexdigest(),"tls_verified":True})
            except Exception as error:
                log({"upstream":upstream,"status":status,"bytes":size,"error_type":type(error).__name__,"tls_verification_required":True})
                if not sent:
                    self.send_error(502,"Official upstream fetch failed")
            finally:
                slots.release()
    server=ThreadingHTTPServer(("127.0.0.1",0),Handler)
    server.daemon_threads=True
    (args.state/"relay.json").write_text(json.dumps({"listen":"127.0.0.1","port":server.server_port,"distribution":args.distribution,"origins":origins,"tls_check_hostname":True,"tls_verify_mode":"CERT_REQUIRED","scope":"real official upstream bytes; guest must verify publisher signatures and package hashes"},indent=2)+"\n")
    print("Read-only official repository relay ready",flush=True)
    server.serve_forever()


if __name__=="__main__":
    main()
