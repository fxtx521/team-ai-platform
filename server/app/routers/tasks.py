"""任务相关路由：建任务、上报步骤、登记产物，全部按 request_id 幂等。"""
from __future__ import annotations

import os
import re
from typing import Any, Dict, List

from fastapi import APIRouter, Depends, HTTPException, Response
from contracts.schema import API, ERROR_CODES, STEP_SCHEMA_REQUIRED

from .. import store
from ..auth import Session, require_session
from ..errors import fail
from ..models import ArtifactRequest, StepsRequest, TaskCreateRequest

router = APIRouter()

UPLOAD_BASE = os.environ.get("TAP_UPLOAD_BASE", "http://127.0.0.1:8000/v1/artifacts/upload")
SHA256_RE = re.compile(r"^[0-9a-fA-F]{64}$")


def _owned_task(task_id: str, session: Session) -> Dict[str, Any]:
    """越权一律按不存在处理，不给探测别人任务的入口。"""
    task = store.get_task(task_id)
    if not task or task["user_id"] != session.user_id:
        raise fail(404, "NOT_FOUND", f"任务不存在: {task_id}")
    return task


@router.post(API["tasks"], status_code=201)
def create_task(payload: TaskCreateRequest, response: Response, session: Session = Depends(require_session)) -> dict:
    """建任务只存标题与摘要，原文在 DTO 层就被丢掉（硬禁令 4）。"""
    task, replay = store.create_task(
        request_id=payload.request_id,
        title=payload.title,
        task_type=payload.type.value,
        source=payload.source.value,
        summary=payload.summary,
        user_id=session.user_id,
        terminal_id=session.terminal_id,
    )
    if replay:
        response.status_code = 200
    body = dict(task)
    body["replay"] = replay
    if replay:
        body["code"] = ERROR_CODES["IDEMPOTENT_REPLAY"]
    return body


@router.post(API["task_steps"])
def report_steps(task_id: str, payload: StepsRequest, session: Session = Depends(require_session)) -> dict:
    """整批幂等：同一 request_id 重发返回已存步骤，执行器断线重试不用去重。"""
    if not payload.steps:
        raise HTTPException(status_code=400, detail="steps 不能为空")
    missing: List[str] = []
    for index, step in enumerate(payload.steps):
        lack = sorted(STEP_SCHEMA_REQUIRED - set(step.model_dump(exclude_none=True)))
        if lack:
            missing.append(f"steps[{index}] 缺 {', '.join(lack)}")
    if missing:
        raise HTTPException(status_code=400, detail="; ".join(missing))

    _owned_task(task_id, session)
    rows, replay = store.add_steps(
        task_id,
        payload.request_id,
        [step.model_dump(exclude_none=True) for step in payload.steps],
        payload.status.value if payload.status else None,
    )
    task = store.get_task(task_id) or {}
    body = {
        "task_id": task_id,
        "request_id": payload.request_id,
        "status": task.get("status"),
        "count": len(rows),
        "steps": rows,
        "replay": replay,
    }
    if replay:
        body["code"] = ERROR_CODES["IDEMPOTENT_REPLAY"]
    return body


@router.post(API["task_artifacts"], status_code=201)
def register_artifact(
    task_id: str,
    payload: ArtifactRequest,
    response: Response,
    session: Session = Depends(require_session),
) -> dict:
    """产物按 sha256 去重；返回上传地址，W2 接对象存储后换预签名 URL。"""
    if not SHA256_RE.match(payload.sha256):
        raise HTTPException(status_code=400, detail="sha256 必须是 64 位十六进制")
    _owned_task(task_id, session)
    artifact, replay = store.add_artifact(
        task_id=task_id,
        request_id=payload.request_id,
        sha256=payload.sha256.lower(),
        visibility=payload.visibility,
        provenance=payload.provenance,
        name=payload.name,
        size_bytes=payload.size_bytes,
        upload_base=UPLOAD_BASE,
    )
    if replay:
        response.status_code = 200
    body = {
        "artifact": artifact,
        "upload_url": artifact["upload_url"],
        "replay": replay,
    }
    if replay:
        body["code"] = ERROR_CODES["IDEMPOTENT_REPLAY"]
    return body
