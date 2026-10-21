"""GET /v1/skills — 执行器按游标增量拉技能，只回 since 之后更新过的条目。"""
from __future__ import annotations

from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, Query
from contracts.schema import API

from .. import store
from ..auth import Session, require_session

router = APIRouter()

SKILLS_SCHEMA = """
CREATE TABLE IF NOT EXISTS skills (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  version INTEGER NOT NULL DEFAULT 1,
  content TEXT NOT NULL,
  visibility TEXT NOT NULL DEFAULT 'company',
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS skill_versions (
  skill_id TEXT NOT NULL REFERENCES skills(id),
  version INTEGER NOT NULL,
  content TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (skill_id, version)
);
"""


def ensure_schema() -> None:
    """技能表由本模块自建：W2 不动 store.py，表生命周期跟着技能路由走。"""
    with store.connect() as conn:
        conn.executescript(SKILLS_SCHEMA)


ensure_schema()


def fetch_skills(since: Optional[str]) -> List[Dict[str, Any]]:
    """字符串比较即时间比较，因为 updated_at 统一是 UTC 秒级 ISO（同 store.now_iso）。"""
    with store.connect() as conn:
        if since:
            rows = conn.execute(
                "SELECT * FROM skills WHERE updated_at > ? ORDER BY updated_at, id", (since,)
            ).fetchall()
        else:
            rows = conn.execute("SELECT * FROM skills ORDER BY updated_at, id").fetchall()
    return [dict(row) for row in rows]


@router.get(API["skills"])
def list_skills(
    since: Optional[str] = Query(default=None),
    session: Session = Depends(require_session),
) -> dict:
    """无 since 全量、有 since 增量；cursor 回最大 updated_at 供下次原地续拉。

    同一秒内写入的多条会被下一次游标跳过（秒级精度），技能写入频率低，W2 先接受。
    """
    items = fetch_skills(since)
    cursor = max((item["updated_at"] for item in items), default=since or store.now_iso())
    return {"items": items, "count": len(items), "cursor": cursor}
