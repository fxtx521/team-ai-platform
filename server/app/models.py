"""请求 DTO：枚举与必填集合直接引用契约包，实现里不再抄一遍。"""
from __future__ import annotations

from typing import Any, Dict, List, Literal, Optional

from pydantic import BaseModel, ConfigDict, Field

from contracts.schema import JobStatus, TaskSource, TaskType


class TokenRequest(BaseModel):
    """双因子换 token 的入参。"""

    api_key: str = Field(min_length=1)
    device_fingerprint: str = Field(min_length=1)
    os: Optional[str] = None


class HeartbeatRequest(BaseModel):
    """终端心跳：只报运行事实，不报任务内容。"""

    version: Optional[str] = None
    os: Optional[str] = None
    queue_len: int = Field(default=0, ge=0)


class TaskCreateRequest(BaseModel):
    """建任务入参；prompt/content 等原文字段被显式丢弃（AGENTS 硬禁令 4）。"""

    model_config = ConfigDict(extra="ignore")

    request_id: str = Field(min_length=1, max_length=128)
    title: str = Field(min_length=1, max_length=200)
    type: TaskType
    source: TaskSource = TaskSource.WEB
    summary: Optional[str] = Field(default=None, max_length=500)


class StepIn(BaseModel):
    """单个步骤；必填由契约 STEP_SCHEMA_REQUIRED 在路由层校验。"""

    tool: Optional[str] = None
    ok: Optional[bool] = None
    started_at: Optional[str] = None
    seq: Optional[int] = None
    finished_at: Optional[str] = None
    error: Optional[str] = None


class StepsRequest(BaseModel):
    """批量步骤上报；request_id 是整批的幂等键。"""

    request_id: str = Field(min_length=1, max_length=128)
    steps: List[StepIn] = Field(default_factory=list)
    status: Optional[JobStatus] = None


class ArtifactRequest(BaseModel):
    """产物登记：只认哈希和可见性，文件本体走对象存储。"""

    request_id: Optional[str] = None
    sha256: str
    visibility: Literal["private", "group", "company"] = "private"
    provenance: Dict[str, Any] = Field(default_factory=dict)
    name: Optional[str] = None
    size_bytes: Optional[int] = Field(default=None, ge=0)
