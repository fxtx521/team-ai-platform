"""GET /v1/inbox/pull — 终端拉取待执行任务，拉走即置 running。"""
from __future__ import annotations

from fastapi import APIRouter, Depends, Query
from contracts.schema import API

from .. import store
from ..auth import Session, require_session

router = APIRouter()


@router.get(API["inbox_pull"])
def pull_inbox(
    limit: int = Query(default=20, ge=1, le=100),
    session: Session = Depends(require_session),
) -> dict:
    """离线任务挂队列，上线即拉；绑定终端后别的机器抢不到同一单。"""
    tasks = store.pull_inbox(session.user_id, session.terminal_id, limit)
    return {"tasks": tasks, "count": len(tasks), "pulled_at": store.now_iso()}
