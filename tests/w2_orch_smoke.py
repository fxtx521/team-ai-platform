"""W2 编排器冒烟：分档正确、L1 红线拦截、standard 全链路顺序、sensitive 绕开服务端模型。"""
from __future__ import annotations

import sys
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

import pytest

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from server.orchestrator import Orchestrator, PolicyViolation, PrivacyPolicy  # noqa: E402


class FakeStore:
    """内存版 store：方法签名与 server/app/store.py 一致，换实现不用改引擎。"""

    def __init__(self) -> None:
        self.tasks: Dict[str, Dict[str, Any]] = {}
        self.steps: Dict[str, List[Dict[str, Any]]] = {}
        self.audits: List[Tuple[str, str, str, Optional[int], Optional[str]]] = []
        self.events: List[Tuple[Any, ...]] = []
        self._n = 0

    def create_task(
        self,
        *,
        request_id: str,
        title: str,
        task_type: str,
        source: str,
        summary: Optional[str],
        user_id: str,
        terminal_id: Optional[str],
    ) -> Tuple[Dict[str, Any], bool]:
        self.events.append(("create_task", request_id, task_type))
        for row in self.tasks.values():
            if row["request_id"] == request_id:
                return dict(row), True
        self._n += 1
        task = {
            "id": f"task_{self._n}",
            "request_id": request_id,
            "user_id": user_id,
            "terminal_id": terminal_id,
            "title": title,
            "type": task_type,
            "source": source,
            "status": "queued",
            "summary": summary,
        }
        self.tasks[task["id"]] = task
        return dict(task), False

    def pull_inbox(self, user_id: str, terminal_id: str, limit: int) -> List[Dict[str, Any]]:
        self.events.append(("pull_inbox", user_id, terminal_id, limit))
        queued = [
            t for t in self.tasks.values() if t["status"] == "queued" and t["user_id"] == user_id
        ]
        picked = queued[:limit]
        for row in picked:
            row["status"] = "running"
            row["terminal_id"] = terminal_id
        return [dict(r) for r in picked]

    def add_steps(
        self, task_id: str, request_id: str, steps: List[Dict[str, Any]], status: Optional[str]
    ) -> Tuple[List[Dict[str, Any]], bool]:
        self.events.append(("add_steps", task_id, request_id, len(steps), status))
        existing = self.steps.get(task_id)
        if existing:
            return [dict(s) for s in existing], True
        rows = [
            {
                "id": f"step_{index}",
                "task_id": task_id,
                "request_id": request_id,
                "seq": step.get("seq") or index,
                "tool": step["tool"],
                "ok": step["ok"],
                "started_at": step["started_at"],
                "finished_at": step.get("finished_at"),
                "error": step.get("error"),
            }
            for index, step in enumerate(steps, start=1)
        ]
        self.steps[task_id] = rows
        if task_id in self.tasks and status:
            self.tasks[task_id]["status"] = status
        return [dict(r) for r in rows], False

    def log_audit(
        self, actor: str, action: str, target: str, status_code: Optional[int], detail: Optional[str] = None
    ) -> None:
        self.audits.append((actor, action, target, status_code, detail))


def make_task(request_id: str = "req-1", task_type: str = "standard") -> Dict[str, Any]:
    """构造一条建任务报文；type 决定分档，其余字段与契约 TaskCreateRequest 对齐。"""
    return {
        "request_id": request_id,
        "title": "写周报",
        "type": task_type,
        "source": "web",
        "summary": "本周进展摘要",
        "user_id": "u_dev",
        "terminal_id": "term_1",
    }


def test_classify_three_tiers() -> None:
    policy = PrivacyPolicy()
    assert policy.classify("local") == "local"
    assert policy.classify("sensitive") == "sensitive"
    assert policy.classify("standard") == "standard"
    assert policy.classify("SENSITIVE ") == "sensitive"
    assert policy.classify("unknown-type") == "standard"  # 未知按 standard：多走审计，不悄悄放行
    assert policy.classify("") == "standard"


def test_local_forbids_model_call() -> None:
    policy = PrivacyPolicy()
    policy.validate(False, "local")  # 只跑本地工具是合法路径
    with pytest.raises(PolicyViolation):
        policy.validate(True, "local")
    with pytest.raises(PolicyViolation):
        policy.validate(True, "sensitive")  # L3 只能直连，不经服务端模型

    calls: List[str] = []
    orch = Orchestrator(
        FakeStore(),
        model_call=lambda task: calls.append("model"),
        local_tool=lambda task: calls.append("local"),
    )
    with pytest.raises(PolicyViolation):
        orch.call_model("local", {"id": "task_x"})
    assert calls == []


def test_local_runs_tool_only() -> None:
    store = FakeStore()
    ran: List[str] = []
    orch = Orchestrator(
        store,
        local_tool=lambda task: ran.append(task["id"]),
        model_call=lambda task: pytest.fail("L1 不得调模型"),
    )
    assert orch.dispatch(make_task(task_type="local"))["tier"] == "local"
    out = orch.loop("u_dev", "term_1")[0]
    assert [s["tool"] for s in out["steps"]] == ["executor.local_tool"]
    assert ran == ["task_1"]
    assert out["status"] == "done"


def test_standard_full_chain_order() -> None:
    store = FakeStore()
    model_calls: List[str] = []
    orch = Orchestrator(store, model_call=lambda task: model_calls.append(task["id"]))

    dispatched = orch.dispatch(make_task())
    task_id = dispatched["task"]["id"]
    assert dispatched["tier"] == "standard"
    assert dispatched["replay"] is False
    assert store.tasks[task_id]["status"] == "queued"

    results = orch.loop("u_dev", "term_1")

    # 链路顺序：先落库 → 再拉活 → 最后上报步骤
    assert [e[0] for e in store.events] == ["create_task", "pull_inbox", "add_steps"]
    assert store.events[1] == ("pull_inbox", "u_dev", "term_1", 1)
    assert store.events[2] == ("add_steps", task_id, f"orch-{task_id}", 1, "done")

    assert len(results) == 1
    out = results[0]
    assert out["tier"] == "standard"
    assert out["status"] == "done"
    assert model_calls == [task_id]
    assert [s["seq"] for s in out["steps"]] == list(range(1, len(out["steps"]) + 1))
    assert [s["tool"] for s in out["steps"]] == ["model.call"]
    assert all(s["ok"] is True for s in out["steps"])
    assert out["steps"] == store.steps[task_id]  # 上报内容 = 落库内容
    assert store.tasks[task_id]["status"] == "done"


def test_sensitive_never_hits_server_model() -> None:
    store = FakeStore()
    direct: List[str] = []
    orch = Orchestrator(
        store,
        model_call=lambda task: pytest.fail("L3 不得经服务端模型"),
        direct_call=lambda task: direct.append(task["id"]),
    )
    orch.dispatch(make_task(task_type="sensitive"))
    out = orch.loop("u_dev", "term_1")[0]

    assert out["tier"] == "sensitive"
    assert [s["tool"] for s in out["steps"]] == ["model.direct_call"]
    assert direct == ["task_1"]
    assert out["status"] == "done"
    assert [a[1] for a in store.audits] == ["task.dispatch", "task.run"]
    assert store.audits[-1][4] == "tier=sensitive"


def test_sensitive_without_direct_callback_fails_loudly() -> None:
    store = FakeStore()
    orch = Orchestrator(store, model_call=lambda task: pytest.fail("L3 不得经服务端模型"))
    orch.dispatch(make_task(task_type="sensitive"))
    out = orch.loop("u_dev", "term_1")[0]
    assert out["status"] == "failed"
    assert out["steps"][0]["ok"] is False
    assert "未注入" in out["steps"][0]["error"]


def test_replay_does_not_duplicate_steps() -> None:
    store = FakeStore()
    orch = Orchestrator(store, model_call=lambda task: "ok")
    orch.dispatch(make_task())
    first = orch.loop("u_dev", "term_1")[0]
    again = orch.execute(store.tasks["task_1"])
    assert first["replay"] is False
    assert again["replay"] is True
    assert len(store.steps["task_1"]) == 1


def test_model_failure_reports_failed_step() -> None:
    store = FakeStore()
    orch = Orchestrator(store, model_call=lambda task: (_ for _ in ()).throw(RuntimeError("模型超时")))
    orch.dispatch(make_task())
    out = orch.loop("u_dev", "term_1")[0]
    assert out["status"] == "failed"
    assert out["steps"][0]["ok"] is False
    assert "模型超时" in out["steps"][0]["error"]
