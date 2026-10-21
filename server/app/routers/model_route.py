"""GET /v1/model/route — 下发本角色可用模型与代理地址，key 永不出服务端。"""
from __future__ import annotations

import json
import logging
import os
from typing import Any, Dict, List

from fastapi import APIRouter, Depends
from contracts.schema import API

from ..auth import Session, require_session

router = APIRouter()

ROUTE_ENV = "TAP_MODEL_ROUTE"
PROXY_ENV = "TAP_PROXY_BASE_URL"
KEY_NOTE = "keys are never sent to terminals"

log = logging.getLogger("tap.model_route")


def load_models(raw: str) -> List[Dict[str, Any]]:
    """运营把环境变量写坏了不能把端点打 500：解析失败按未配置处理。"""
    try:
        parsed = json.loads(raw) if raw.strip() else []
    except json.JSONDecodeError:
        log.warning("%s 不是合法 JSON，按未配置处理", ROUTE_ENV)
        return []
    if not isinstance(parsed, list):
        log.warning("%s 必须是数组，按未配置处理", ROUTE_ENV)
        return []
    models: List[Dict[str, Any]] = []
    for item in parsed:
        if not isinstance(item, dict) or not item.get("id"):
            continue
        models.append(
            {
                "id": str(item["id"]),
                "provider": str(item.get("provider") or ""),
                "base_url": str(item.get("base_url") or ""),
                "enabled": bool(item.get("enabled", True)),
            }
        )
    return models


@router.get(API["model_route"])
def model_route(session: Session = Depends(require_session)) -> dict:
    """每次请求现读环境变量：改配置重启即生效，不在导入期缓存。"""
    return {
        "models": load_models(os.environ.get(ROUTE_ENV, "")),
        "proxy_base_url": os.environ.get(PROXY_ENV, ""),
        "note": KEY_NOTE,
    }
