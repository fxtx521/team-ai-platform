"""契约包 — 服务端与执行器共享的 API 路径、错误码、数据结构定义。

契约即法律（AGENTS.md）：两端改动先改这里，评审后再动实现。
"""
from dataclasses import dataclass, field
from enum import Enum


class TaskType(str, Enum):
    LOCAL = "local"          # L1 纯本地工具指令，不过大脑
    STANDARD = "standard"    # L2 经服务端编排+模型，不落盘原文
    SENSITIVE = "sensitive"  # L3 员工选直连模式，绕开公司服务器


class TaskSource(str, Enum):
    WEB = "web"
    FEISHU = "feishu"
    CRON = "cron"
    EXECUTOR = "executor"


class JobStatus(str, Enum):
    QUEUED = "queued"
    RUNNING = "running"
    DONE = "done"
    FAILED = "failed"
    CANCELLED = "cancelled"


API = {
    "auth_token": "/auth/token",
    "heartbeat": "/heartbeat",
    "tasks": "/tasks",
    "task_steps": "/tasks/{task_id}/steps",
    "task_artifacts": "/tasks/{task_id}/artifacts",
    "inbox_dispatch": "/inbox/dispatch",
    "inbox_pull": "/inbox/pull",
    "skills": "/skills",
    "model_route": "/model/route",
}

ERROR_CODES = {
    "AUTH_FAILED": 40101,        # key 或设备指纹无效
    "TOKEN_EXPIRED": 40102,
    "PATH_ESCAPE": 40301,        # 路径越界（硬边界触发）
    "DANGER_CMD_CONFIRM": 40302, # 危险命令需 Web 端二次确认
    "NOT_FOUND": 40401,
    "IDEMPOTENT_REPLAY": 40901,  # 幂等重放（返回原结果）
    "RATE_LIMITED": 42901,
}


@dataclass
class Artifact:
    sha256: str
    visibility: str = "private"  # private | group | company
    provenance: dict = field(default_factory=dict)  # 产出方+依据材料


STEP_SCHEMA_REQUIRED = {"tool", "ok", "started_at"}
"""步骤上报必填字段；缺一即 400，执行器必须本地校验后再发。"""
