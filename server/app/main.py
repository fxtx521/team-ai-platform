"""FastAPI 入口：装配路由 + 全请求审计 + 启动建表。"""
from __future__ import annotations

import logging
from contextlib import asynccontextmanager
from typing import AsyncIterator, Callable

from fastapi import FastAPI, Request, Response
from starlette.concurrency import run_in_threadpool

from . import store
from .auth import actor_of
from .routers import auth as auth_router
from .routers import heartbeat, inbox, model_route, skills, tasks

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("tap")

API_PREFIX = "/v1"


@asynccontextmanager
async def lifespan(_: FastAPI) -> AsyncIterator[None]:
    """启动先建表，避免第一个请求撞上空库。"""
    store.init_db()
    log.info("TAP 控制面就绪, db=%s", store.db_path())
    yield


app = FastAPI(title="Team AI Platform 控制面", version="1.0.0", lifespan=lifespan)


@app.middleware("http")
async def audit_all(request: Request, call_next: Callable) -> Response:
    """所有请求都留痕：actor 从 token 解出，验不过记 anonymous 也照样记。"""
    response = await call_next(request)
    await run_in_threadpool(
        store.log_audit,
        actor_of(request.headers.get("authorization")),
        "http.request",
        f"{request.method} {request.url.path}",
        response.status_code,
        request.query_params.get("limit"),
    )
    return response


for module in (auth_router, heartbeat, tasks, inbox, skills, model_route):
    app.include_router(module.router, prefix=API_PREFIX)
