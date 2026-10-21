"""POST /v1/auth/token — API Key + 设备指纹换短期 token。"""
from __future__ import annotations

from fastapi import APIRouter
from contracts.schema import API

from .. import auth as auth_service
from .. import store
from ..models import TokenRequest

router = APIRouter()


@router.post(API["auth_token"])
def create_token(payload: TokenRequest) -> dict:
    """双因子都过才发 token；新机器第一次来顺带完成终端注册。"""
    user, terminal = auth_service.authenticate(payload.api_key, payload.device_fingerprint, payload.os)
    token, expires_at = auth_service.issue_token(user["id"], terminal["id"], payload.device_fingerprint)
    store.log_audit(user["id"], "auth.token", terminal["id"], 200, "signin")
    return {
        "token": token,
        "expires_at": expires_at,
        "user_id": user["id"],
        "terminal_id": terminal["id"],
        "role": user["role"],
    }
