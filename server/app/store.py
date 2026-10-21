"""SQLite 存储层：所有写操作集中在这里，幂等语义由 UNIQUE 约束兜底。"""
from __future__ import annotations

import json
import os
import sqlite3
import uuid
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterator, List, Optional, Tuple

DEFAULT_DB_PATH = Path(__file__).resolve().parents[1] / "tap.db"

SCHEMA = """
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  api_key TEXT NOT NULL UNIQUE,
  role TEXT NOT NULL DEFAULT 'member',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS terminals (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id),
  device_fingerprint TEXT NOT NULL UNIQUE,
  os TEXT,
  version TEXT,
  queue_len INTEGER NOT NULL DEFAULT 0,
  last_seen_at TEXT,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL UNIQUE,
  user_id TEXT NOT NULL,
  terminal_id TEXT,
  title TEXT NOT NULL,
  type TEXT NOT NULL,
  source TEXT NOT NULL,
  status TEXT NOT NULL,
  summary TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_inbox ON tasks (status, user_id, created_at);
CREATE TABLE IF NOT EXISTS task_steps (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  request_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  tool TEXT NOT NULL,
  ok INTEGER NOT NULL,
  started_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT,
  created_at TEXT NOT NULL,
  UNIQUE (task_id, request_id, seq)
);
CREATE TABLE IF NOT EXISTS artifacts (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  request_id TEXT,
  sha256 TEXT NOT NULL,
  visibility TEXT NOT NULL,
  provenance TEXT NOT NULL DEFAULT '{}',
  name TEXT,
  size_bytes INTEGER,
  upload_url TEXT,
  created_at TEXT NOT NULL,
  UNIQUE (task_id, sha256)
);
CREATE TABLE IF NOT EXISTS audit_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL,
  status_code INTEGER,
  detail TEXT,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs (created_at);
"""


def db_path() -> Path:
    """运行期可换库：测试用 TAP_DB 指到临时文件，避免污染真实数据。"""
    return Path(os.environ.get("TAP_DB", str(DEFAULT_DB_PATH))).expanduser()


def now_iso() -> str:
    """统一 UTC 秒级时间戳，执行器按字符串比较游标即可。"""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


@contextmanager
def connect() -> Iterator[sqlite3.Connection]:
    """一次操作一条连接，WAL 让读写不互锁（W1 单进程足够）。"""
    path = db_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path), timeout=10)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    conn.execute("PRAGMA busy_timeout=5000")
    try:
        yield conn
        conn.commit()
    except sqlite3.Error:
        conn.rollback()
        raise
    finally:
        conn.close()


def init_db() -> None:
    """启动即建表并种一个开发用户，让 W1 有可用的 api_key。"""
    with connect() as conn:
        conn.executescript(SCHEMA)
        if conn.execute("SELECT COUNT(*) AS n FROM users").fetchone()["n"] == 0:
            conn.execute(
                "INSERT INTO users (id, name, api_key, role, created_at) VALUES (?, ?, ?, ?, ?)",
                ("u_dev", "开发种子用户", os.environ.get("TAP_SEED_API_KEY", "dev-key-local"), "admin", now_iso()),
            )


def get_user_by_api_key(api_key: str) -> Optional[Dict[str, Any]]:
    """认证第一步：key 必须命中才谈设备指纹。"""
    with connect() as conn:
        row = conn.execute("SELECT * FROM users WHERE api_key = ?", (api_key,)).fetchone()
    return dict(row) if row else None


def get_terminal_by_fingerprint(fingerprint: str) -> Optional[Dict[str, Any]]:
    """同一指纹只能绑一个终端，防止 key 被搬到别的机器上复用。"""
    with connect() as conn:
        row = conn.execute("SELECT * FROM terminals WHERE device_fingerprint = ?", (fingerprint,)).fetchone()
    return dict(row) if row else None


def create_terminal(user_id: str, fingerprint: str, os_name: Optional[str]) -> Dict[str, Any]:
    """首次登录即注册终端；指纹唯一约束挡住并发重复注册。"""
    terminal_id = "term_" + uuid.uuid4().hex[:12]
    with connect() as conn:
        conn.execute(
            "INSERT INTO terminals (id, user_id, device_fingerprint, os, last_seen_at, created_at)"
            " VALUES (?, ?, ?, ?, ?, ?)",
            (terminal_id, user_id, fingerprint, os_name, now_iso(), now_iso()),
        )
        row = conn.execute("SELECT * FROM terminals WHERE id = ?", (terminal_id,)).fetchone()
    return dict(row)


def touch_terminal(terminal_id: str, version: Optional[str], queue_len: int) -> Optional[Dict[str, Any]]:
    """心跳只更新在线事实，不记任务内容，符合\"不落盘原文\"红线。"""
    with connect() as conn:
        conn.execute(
            "UPDATE terminals SET last_seen_at = ?, version = COALESCE(?, version), queue_len = ? WHERE id = ?",
            (now_iso(), version, queue_len, terminal_id),
        )
        row = conn.execute("SELECT * FROM terminals WHERE id = ?", (terminal_id,)).fetchone()
    return dict(row) if row else None


def _task_row(conn: sqlite3.Connection, task_id: str) -> Optional[Dict[str, Any]]:
    row = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
    return dict(row) if row else None


def get_task(task_id: str) -> Optional[Dict[str, Any]]:
    with connect() as conn:
        return _task_row(conn, task_id)


def create_task(
    *,
    request_id: str,
    title: str,
    task_type: str,
    source: str,
    summary: Optional[str],
    user_id: str,
    terminal_id: Optional[str],
) -> Tuple[Dict[str, Any], bool]:
    """request_id 是幂等键：命中即原样返回旧任务，绝不重复落库。

    只存 title/summary，任务原文（prompt/content）在路由层就被丢弃。
    """
    with connect() as conn:
        existing = conn.execute("SELECT * FROM tasks WHERE request_id = ?", (request_id,)).fetchone()
        if existing:
            return dict(existing), True
        task_id = "task_" + uuid.uuid4().hex[:16]
        created = now_iso()
        try:
            conn.execute(
                "INSERT INTO tasks (id, request_id, user_id, terminal_id, title, type, source, status, summary,"
                " created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    task_id,
                    request_id,
                    user_id,
                    terminal_id,
                    title,
                    task_type,
                    source,
                    "queued",
                    summary,
                    created,
                    created,
                ),
            )
        except sqlite3.IntegrityError:
            row = conn.execute("SELECT * FROM tasks WHERE request_id = ?", (request_id,)).fetchone()
            return dict(row), True
        row = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
    return dict(row), False


def add_steps(
    task_id: str, request_id: str, steps: List[Dict[str, Any]], status: Optional[str]
) -> Tuple[List[Dict[str, Any]], bool]:
    """按 (task, request_id) 整批幂等：重放返回已存步骤，不重复计费/计数。"""
    with connect() as conn:
        stored = conn.execute(
            "SELECT * FROM task_steps WHERE task_id = ? AND request_id = ? ORDER BY seq", (task_id, request_id)
        ).fetchall()
        if stored:
            return [dict(r) for r in stored], True
        created = now_iso()
        for index, step in enumerate(steps, start=1):
            conn.execute(
                "INSERT INTO task_steps (id, task_id, request_id, seq, tool, ok, started_at, finished_at, error,"
                " created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    "step_" + uuid.uuid4().hex[:12],
                    task_id,
                    request_id,
                    step.get("seq") or index,
                    step["tool"],
                    1 if step["ok"] else 0,
                    step["started_at"],
                    step.get("finished_at"),
                    step.get("error"),
                    created,
                ),
            )
        next_status = status or ("failed" if any(not s["ok"] for s in steps) else "running")
        conn.execute(
            "UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?", (next_status, created, task_id)
        )
        rows = conn.execute(
            "SELECT * FROM task_steps WHERE task_id = ? AND request_id = ? ORDER BY seq", (task_id, request_id)
        ).fetchall()
    return [dict(r) for r in rows], False


def add_artifact(
    *,
    task_id: str,
    request_id: Optional[str],
    sha256: str,
    visibility: str,
    provenance: Dict[str, Any],
    name: Optional[str],
    size_bytes: Optional[int],
    upload_base: str,
) -> Tuple[Dict[str, Any], bool]:
    """(task, sha256) 唯一：同一产物反复上报只登记一次，避免资产库重复计。"""
    with connect() as conn:
        existing = conn.execute(
            "SELECT * FROM artifacts WHERE task_id = ? AND sha256 = ?", (task_id, sha256)
        ).fetchone()
        if existing:
            return dict(existing), True
        artifact_id = "art_" + uuid.uuid4().hex[:16]
        upload_url = f"{upload_base.rstrip('/')}/{artifact_id}"
        conn.execute(
            "INSERT INTO artifacts (id, task_id, request_id, sha256, visibility, provenance, name, size_bytes,"
            " upload_url, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                artifact_id,
                task_id,
                request_id,
                sha256,
                visibility,
                json.dumps(provenance, ensure_ascii=False),
                name,
                size_bytes,
                upload_url,
                now_iso(),
            ),
        )
        row = conn.execute("SELECT * FROM artifacts WHERE id = ?", (artifact_id,)).fetchone()
    return dict(row), False


def list_task_artifacts(task_id: str) -> List[Dict[str, Any]]:
    with connect() as conn:
        rows = conn.execute(
            "SELECT * FROM artifacts WHERE task_id = ? ORDER BY created_at", (task_id,)
        ).fetchall()
    return [dict(r) for r in rows]


def pull_inbox(user_id: str, terminal_id: str, limit: int) -> List[Dict[str, Any]]:
    """离线排队、上线即拉：拉走的任务立刻置 running 并绑定本终端，防两个终端抢同一活。"""
    with connect() as conn:
        rows = conn.execute(
            "SELECT * FROM tasks WHERE status = 'queued' AND user_id = ?"
            " AND (terminal_id IS NULL OR terminal_id = ?) ORDER BY created_at LIMIT ?",
            (user_id, terminal_id, limit),
        ).fetchall()
        tasks = [dict(r) for r in rows]
        if tasks:
            stamp = now_iso()
            conn.executemany(
                "UPDATE tasks SET status = 'running', terminal_id = ?, updated_at = ? WHERE id = ?",
                [(terminal_id, stamp, t["id"]) for t in tasks],
            )
            for task in tasks:
                task.update(status="running", terminal_id=terminal_id, updated_at=stamp)
    return tasks


TASK_CARD_FIELDS = ("title", "type", "status", "request_id", "created_at", "updated_at")
"""列表卡片字段：summary/user_id/terminal_id 留给详情接口，列表不多吐一眼用不上的东西。"""

LIST_LIMIT_DEFAULT = 50
LIST_LIMIT_MAX = 200
"""默认 50 上限 200：看板一屏够用，也挡住 ?limit=100000 把库拖垮。"""


def _task_card(row: Dict[str, Any]) -> Dict[str, Any]:
    """行转看板卡片，主键改叫 task_id 与响应体字段对齐。"""
    card = {"task_id": row["id"]}
    card.update({field: row[field] for field in TASK_CARD_FIELDS})
    return card


def list_tasks(cursor: Optional[str], limit: int, user_id: Optional[str] = None) -> Dict[str, Any]:
    """按 (created_at DESC, id DESC) 做键集分页而不是 offset：

    全序里 id 兜住同秒并列，所以新任务插进来也不会让已翻过的页错位或漏条。
    cursor 传最后一条的 id，任务已删/传乱码就回空页，不让看板吃 500。
    user_id 过滤：普通员工只看自己的任务；None（管理端）看全员——
    权限边界落在存储层，路由拿不到"漏过滤"的口子。
    """
    size = int(limit) if limit else LIST_LIMIT_DEFAULT
    size = max(1, min(size, LIST_LIMIT_MAX))
    with connect() as conn:
        params: List[Any] = []
        where_parts: List[str] = []
        if user_id is not None:
            where_parts.append("user_id = ?")
            params.append(user_id)
        if cursor:
            anchor = _task_row(conn, cursor)
            if not anchor:
                return {"items": [], "next_cursor": None}
            where_parts.append("(created_at < ? OR (created_at = ? AND id < ?))")
            params += [anchor["created_at"], anchor["created_at"], anchor["id"]]
        where = (" WHERE " + " AND ".join(where_parts)) if where_parts else ""
        rows = conn.execute(
            "SELECT * FROM tasks" + where + " ORDER BY created_at DESC, id DESC LIMIT ?",
            (*params, size + 1),
        ).fetchall()
    cards = [_task_card(dict(row)) for row in rows[:size]]
    next_cursor = cards[-1]["task_id"] if len(rows) > size else None
    return {"items": cards, "next_cursor": next_cursor}


def log_audit(
    actor: str, action: str, target: str, status_code: Optional[int], detail: Optional[str] = None
) -> None:
    """审计必须在请求链路上无条件写，失败也不能影响业务响应。"""
    try:
        with connect() as conn:
            conn.execute(
                "INSERT INTO audit_logs (actor, action, target, status_code, detail, created_at)"
                " VALUES (?, ?, ?, ?, ?, ?)",
                (actor, action, target, status_code, detail, now_iso()),
            )
    except sqlite3.Error:
        pass
