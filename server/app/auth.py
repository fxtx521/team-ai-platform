"""认证：API Key + 设备指纹双因子换短期 token，token 自签名不落会话表。"""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import time
from datetime import datetime, timezone
from typing import Any, Dict, NamedTuple, Optional, Tuple

from fastapi import Header

from . import store
from .errors import fail

SECRET = os.environ.get("TAP_SECRET", "tap-dev-secret-change-me").encode()
TOKEN_TTL_SECONDS = int(os.environ.get("TAP_TOKEN_TTL_SECONDS", "43200"))


class Session(NamedTuple):
    """认证后的调用者身份，路由层只认这三个字段。"""

    user_id: str
    terminal_id: str
    fingerprint_hash: str


def _b64e(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode().rstrip("=")


def _b64d(text: str) -> bytes:
    return base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))


def _sign(payload: str) -> str:
    return _b64e(hmac.new(SECRET, payload.encode(), hashlib.sha256).digest())


def fingerprint_hash(fingerprint: str) -> str:
    """只把指纹摘要放进 token，避免明文设备信息随报文到处跑。"""
    return hashlib.sha256(fingerprint.encode()).hexdigest()[:32]


def issue_token(user_id: str, terminal_id: str, fingerprint: str) -> Tuple[str, str]:
    """签发短期 token，同时返回 ISO 过期时间给执行器做续期提醒。"""
    expires_at = int(time.time()) + TOKEN_TTL_SECONDS
    body = {
        "sub": user_id,
        "tid": terminal_id,
        "fp": fingerprint_hash(fingerprint),
        "exp": expires_at,
    }
    payload = _b64e(json.dumps(body, separators=(",", ":")).encode())
    token = f"{payload}.{_sign(payload)}"
    iso = datetime.fromtimestamp(expires_at, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    return token, iso


def decode_token(token: str) -> Optional[Dict[str, Any]]:
    """验签+验期，任何一步不过都返回 None（签名比较用常数时间）。"""
    try:
        payload, signature = token.split(".", 1)
    except ValueError:
        return None
    if not hmac.compare_digest(_sign(payload), signature):
        return None
    try:
        body = json.loads(_b64d(payload))
    except (ValueError, json.JSONDecodeError):
        return None
    if int(body.get("exp", 0)) < int(time.time()):
        return None
    return body


def require_session(authorization: Optional[str] = Header(default=None)) -> Session:
    """FastAPI 依赖：所有业务路由挂上它就等于挂上审计与身份。"""
    if not authorization or not authorization.lower().startswith("bearer "):
        raise fail(401, "AUTH_FAILED", "缺少 Bearer token")
    token = authorization.split(" ", 1)[1].strip()
    body = decode_token(token)
    if body is None:
        raise fail(401, "TOKEN_EXPIRED", "token 无效或已过期")
    return Session(user_id=str(body["sub"]), terminal_id=str(body["tid"]), fingerprint_hash=str(body["fp"]))


def actor_of(authorization: Optional[str]) -> str:
    """审计中间件用：验不过也算匿名请求，照样留痕。"""
    if not authorization or not authorization.lower().startswith("bearer "):
        return "anonymous"
    body = decode_token(authorization.split(" ", 1)[1].strip())
    return str(body["sub"]) if body else "anonymous"


def authenticate(api_key: str, fingerprint: str, os_name: Optional[str] = None) -> Tuple[Dict[str, Any], Dict[str, Any]]:
    """双因子校验：key 决定人，指纹决定机器；新机器首次见即注册终端。"""
    user = store.get_user_by_api_key(api_key)
    if not user:
        raise fail(401, "AUTH_FAILED", "api_key 无效")
    terminal = store.get_terminal_by_fingerprint(fingerprint)
    if terminal and terminal["user_id"] != user["id"]:
        raise fail(401, "AUTH_FAILED", "设备指纹已绑定其他账号")
    if not terminal:
        terminal = store.create_terminal(user["id"], fingerprint, os_name)
    return user, terminal
