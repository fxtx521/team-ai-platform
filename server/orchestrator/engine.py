"""任务编排引擎：拉活 → 分档 → 走对应执行路径 → 步骤上报；分档判断只在这里做一次。"""
from __future__ import annotations

from datetime import datetime, timezone
from typing import Any, Callable, Dict, List, Mapping, Optional, Tuple

from .policy import LOCAL, SENSITIVE, PolicyViolation, PrivacyPolicy

ModelCall = Callable[[Mapping[str, Any]], Any]
ToolCall = Callable[[Mapping[str, Any]], Any]

_ERROR_MAX = 200  # 步骤里的错误串截断，避免把上游原文顺带落进库（硬禁令 4）


def _now() -> str:
    """UTC 秒级时间戳，与 store.now_iso 同格式，队列游标可直接字符串比较。"""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class Orchestrator:
    """store 与回调全部注入：单测用内存 store + fake 回调，生产换 sqlite store 不改引擎。"""

    def __init__(
        self,
        store_module: Any,
        *,
        model_call: Optional[ModelCall] = None,
        local_tool: Optional[ToolCall] = None,
        direct_call: Optional[ToolCall] = None,
        policy: Optional[PrivacyPolicy] = None,
    ) -> None:
        self._store = store_module
        self._model_call = model_call
        self._local_tool = local_tool
        self._direct_call = direct_call
        self._policy = policy or PrivacyPolicy()

    # ---- 对外三件事：派活、拉活跑批、跑单任务 --------------------------------

    def dispatch(self, task: Mapping[str, Any]) -> Dict[str, Any]:
        """先分档再落库：档位随结果回给调用方，后续审计不必重新推断。"""
        tier = self._policy.classify(str(task.get("type", "")))
        row, replay = self._store.create_task(
            request_id=str(task["request_id"]),
            title=str(task["title"]),
            task_type=str(task.get("type", "")),
            source=str(task.get("source", "web")),
            summary=task.get("summary"),
            user_id=str(task["user_id"]),
            terminal_id=task.get("terminal_id"),
        )
        self._audit(str(task["user_id"]), "task.dispatch", str(row.get("id", "")), tier)
        return {"task": dict(row), "tier": tier, "replay": replay}

    def loop(self, user_id: str, terminal_id: str, limit: int = 1) -> List[Dict[str, Any]]:
        """W2 同步串行：一次拉一批逐条跑完，单任务失败不拖垮整批，排障也简单。"""
        tasks = self._store.pull_inbox(user_id, terminal_id, limit)
        return [self.execute(task) for task in tasks]

    def execute(self, task: Mapping[str, Any]) -> Dict[str, Any]:
        """单任务一个循环：分档 → 按档执行 → 整批上报步骤，request_id 固定以便重放幂等。"""
        task_id = str(task["id"])
        task_type = str(task.get("type", ""))
        tier = self._policy.classify(task_type)
        self._audit(str(task.get("user_id", "?")), "task.run", task_id, tier)
        steps = self._run_tier(tier, task_type, task)
        status = "done" if all(step["ok"] for step in steps) else "failed"
        stored, replay = self._store.add_steps(task_id, f"orch-{task_id}", steps, status)
        return {
            "task_id": task_id,
            "tier": tier,
            "status": status,
            "steps": stored,
            "replay": replay,
        }

    def call_model(self, task_type: str, task: Mapping[str, Any]) -> Any:
        """服务端模型调用的唯一入口：先过策略再打回调，L1/L3 在这里就被挡住。"""
        self._policy.validate(True, task_type)
        if self._model_call is None:
            raise RuntimeError("model_call 未注入，standard 档无法执行")
        return self._model_call(task)

    # ---- 内部：按档执行 ------------------------------------------------------

    def _run_tier(self, tier: str, task_type: str, task: Mapping[str, Any]) -> List[Dict[str, Any]]:
        """三档三条路径，只有 standard 会碰服务端模型；任一档异常都折成失败步骤上报。"""
        if tier == LOCAL:
            return self._collect([("executor.local_tool", self._local_tool, task)])
        if tier == SENSITIVE:
            return self._collect([("model.direct_call", self._direct_call, task)])
        return self._collect(
            [("model.call", lambda payload: self.call_model(task_type, payload), task)]
        )

    def _collect(
        self, calls: List[Tuple[str, Optional[ToolCall], Mapping[str, Any]]]
    ) -> List[Dict[str, Any]]:
        """统一包 step：没注入回调时按失败上报，而不是静默跳过让人以为跑过了。"""
        steps: List[Dict[str, Any]] = []
        for seq, (tool, callback, payload) in enumerate(calls, start=1):
            steps.append(self._run_step(seq, tool, callback, payload))
        return steps

    def _run_step(
        self, seq: int, tool: str, callback: Optional[ToolCall], payload: Mapping[str, Any]
    ) -> Dict[str, Any]:
        """一个回调一个 step：起止时间与错误都在这里落定，调用方无需关心异常。"""
        started = _now()
        if callback is None:
            return {
                "seq": seq,
                "tool": tool,
                "ok": False,
                "started_at": started,
                "finished_at": _now(),
                "error": f"{tool} 回调未注入",
            }
        try:
            callback(payload)
            return {"seq": seq, "tool": tool, "ok": True, "started_at": started, "finished_at": _now()}
        except PolicyViolation:
            raise
        except Exception as exc:  # 模型/工具失败是常态，落失败步骤而不是打断整批
            return {
                "seq": seq,
                "tool": tool,
                "ok": False,
                "started_at": started,
                "finished_at": _now(),
                "error": str(exc)[:_ERROR_MAX],
            }

    def _audit(self, actor: str, action: str, target: str, detail: str) -> None:
        """审计是编排的副产物，store 没实现也不能影响主流程。"""
        logger = getattr(self._store, "log_audit", None)
        if logger is None:
            return
        try:
            logger(actor, action, target, None, f"tier={detail}")
        except Exception:
            pass
