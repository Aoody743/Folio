"""External API client preserving cookies without exposing credential values."""
from __future__ import annotations

import http.cookiejar
import json
import urllib.error
import urllib.request
import urllib.parse


class APIClient:
    def __init__(self, base: str, token: str = ""):
        self.base = base.rstrip("/")
        self.token = token
        self.cookies = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.cookies))
        self.csrf = ""

    def request(self, method: str, path: str, body=None, *, csrf=True, headers=None):
        request_headers = {"Accept": "application/json"}
        if self.token:
            request_headers["Authorization"] = f"Bearer {self.token}"
        if csrf and self.csrf and not self.token:
            request_headers["X-CSRF-Token"] = self.csrf
        if body is not None:
            request_headers["Content-Type"] = "application/json"
        request_headers.update(headers or {})
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(self.base + path, data=data,
                                         headers=request_headers, method=method)
        try:
            response = self.opener.open(request, timeout=15)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            raw = response.read()
            try:
                parsed = json.loads(raw)
            except (ValueError, UnicodeDecodeError):
                parsed = raw
            return response.status, parsed, response.headers

    def form(self, path, fields, *, follow_redirects=True):
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, request, response, code, message, headers, newurl):
                return None
        handlers = [urllib.request.HTTPCookieProcessor(self.cookies)]
        if not follow_redirects:
            handlers.append(NoRedirect())
        opener = urllib.request.build_opener(*handlers)
        request = urllib.request.Request(self.base + path, urllib.parse.urlencode(fields).encode(),
                    {"Content-Type": "application/x-www-form-urlencoded"}, method="POST")
        try:
            response = opener.open(request, timeout=15)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            raw = response.read()
            try:
                parsed = json.loads(raw)
            except (ValueError, UnicodeDecodeError):
                parsed = raw
            return response.status, parsed, response.headers

    def get(self, path):
        return self.request("GET", path)

    def post(self, path, body, **kwargs):
        return self.request("POST", path, body, **kwargs)

    def patch(self, path, body, **kwargs):
        return self.request("PATCH", path, body, **kwargs)

    def delete(self, path, body=None, **kwargs):
        return self.request("DELETE", path, body, **kwargs)


def data(response, expected=200):
    status, payload, _ = response
    assert status == expected, (status, payload)
    assert isinstance(payload, dict) and "data" in payload, payload
    return payload["data"]
