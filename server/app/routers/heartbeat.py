"""POST /v1/heartbeat — 终端在线心跳，只报事实不报内容。"""
from __future__ import annotations

from fastapi import APIRouter, Depends
from contracts.schema import API

from .. import store
from ..auth import Session, require_session
from ..models import HeartbeatRequest

router = APIRouter()


@router.post(API["heartbeat"])
def heartbeat(payload: HeartbeatRequest, session: Session = Depends(require_session)) -> dict:
    """心跳顺带刷新版本与队列长度，服务端据此判断终端能否派活。"""
    terminal = store.touch_terminal(session.terminal_id, payload.version, payload.queue_len)
    return {
        "ok": terminal is not None,
        "terminal_id": session.terminal_id,
        "server_time": store.now_iso(),
        "queue_len": payload.queue_len,
    }
