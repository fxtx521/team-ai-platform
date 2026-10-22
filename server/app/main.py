"""FastAPI 入口：装配路由 + 全请求审计 + 启动建表。"""
from __future__ import annotations

import logging
from contextlib import asynccontextmanager
from typing import AsyncIterator, Callable

from pathlib import Path

from fastapi import FastAPI, Request, Response
from fastapi.staticfiles import StaticFiles
from starlette.concurrency import run_in_threadpool

from . import store
from .auth import actor_of
from .routers import auth as auth_router
from .routers import heartbeat, inbox, model_route, skills, tasks

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("tap")

API_PREFIX = "/v1"
# 工作台静态页（登录页 index.html + 看板 dashboard.html）。
# 同源托管：页面和 API 同一个端口，浏览器无跨域，登录页"服务地址"留空即用。
WEB_DIR = Path(__file__).resolve().parent.parent.parent / "web"


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

# 工作台挂根路径：/ 进登录页，/dashboard.html 进看板。
# 挂在路由注册之后、且用 check_dir=False：web/ 缺失时服务端 API 照常起，不连坐。
if WEB_DIR.is_dir():
    app.mount("/", StaticFiles(directory=str(WEB_DIR), html=True), name="web")
