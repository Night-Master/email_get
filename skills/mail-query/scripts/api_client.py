#!/usr/bin/env python3
from __future__ import annotations

import http.cookiejar
import json
import os
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_PASSWORD_FILE = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "release", "auth_password.txt"
)


def normalize_base_url(base_url: str) -> str:
    return base_url.rstrip("/")


def resolve_password(password: str = "") -> str:
    if password:
        return password
    env_pw = os.environ.get("EMAIL_GET_PASSWORD", "")
    if env_pw:
        return env_pw
    path = os.path.normpath(DEFAULT_PASSWORD_FILE)
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return ""


class ServiceClient:
    """带登录态的服务客户端。所有请求共享同一个 cookie jar。"""

    def __init__(self, base_url: str, password: str = ""):
        self.base_url = normalize_base_url(base_url)
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar)
        )
        pw = resolve_password(password)
        if pw:
            self.login(pw)

    def login(self, password: str) -> None:
        req = urllib.request.Request(
            f"{self.base_url}/api/auth/login",
            data=json.dumps({"password": password}).encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        self._send(req)

    def get(self, path: str, params: dict[str, str] | None = None) -> dict:
        query = ""
        if params:
            query = "?" + urllib.parse.urlencode({k: v for k, v in params.items() if v != ""})
        req = urllib.request.Request(f"{self.base_url}{path}{query}", method="GET")
        return self._send(req)

    def post(self, path: str, payload: dict) -> dict:
        req = urllib.request.Request(
            f"{self.base_url}{path}",
            data=json.dumps(payload).encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        return self._send(req)

    def delete(self, path: str) -> dict:
        req = urllib.request.Request(f"{self.base_url}{path}", method="DELETE")
        return self._send(req)

    def _send(self, req: urllib.request.Request) -> dict:
        try:
            with self.opener.open(req, timeout=120) as resp:
                raw = resp.read().decode("utf-8")
        except urllib.error.HTTPError as exc:
            body = exc.read().decode("utf-8", errors="replace")
            if exc.code == 401:
                raise RuntimeError(
                    "HTTP 401: 未登录或密码错误,请用 --password 指定管理密码,"
                    "或设置环境变量 EMAIL_GET_PASSWORD"
                ) from exc
            raise RuntimeError(f"HTTP {exc.code}: {body}") from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(f"请求失败: {exc}") from exc

        if not raw.strip():
            return {}
        try:
            return json.loads(raw)
        except json.JSONDecodeError as exc:
            raise RuntimeError(f"服务返回的不是 JSON: {raw[:500]}") from exc


# 兼容旧调用方式的无登录函数(仅适用于未启用认证的服务)。
def api_get(base_url: str, path: str, params: dict[str, str] | None = None) -> dict:
    return ServiceClient(base_url).get(path, params)


def api_post(base_url: str, path: str, payload: dict) -> dict:
    return ServiceClient(base_url).post(path, payload)


def api_delete(base_url: str, path: str) -> dict:
    return ServiceClient(base_url).delete(path)
