# 服务端控制面（W1 骨架）

FastAPI 控制面 + SQLite 存储层，实现契约里的 auth / heartbeat / tasks / steps / artifacts / inbox 六个端点。
API 路径、错误码、枚举全部 `from contracts.schema import ...`，实现里不抄第二份。

## 跑起来

```bash
cd server
/usr/bin/python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
TAP_DB=$PWD/tap.db .venv/bin/python -m uvicorn app.main:app --host 127.0.0.1 --port 8000
```

冒烟（自己起服务、跑完闭环、退出即清理）：

```bash
bash tests/w1_smoke.sh
```

## 环境变量

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `TAP_DB` | `server/tap.db` | SQLite 文件路径 |
| `TAP_SECRET` | `tap-dev-secret-change-me` | token 签名密钥，上线必须换 |
| `TAP_TOKEN_TTL_SECONDS` | `43200` | token 有效期（12h） |
| `TAP_SEED_API_KEY` | `dev-key-local` | W1 种子用户密钥，W2 换管理后台发放 |
| `TAP_UPLOAD_BASE` | `http://127.0.0.1:8000/v1/artifacts/upload` | 产物上传地址前缀（W2 换预签名） |

若 shell 的 `PYTHONPATH` 指向别的解释器环境（本机 Hermes 就是），启动时用 `env -u PYTHONPATH`。

## 结构

```
app/main.py        装配路由 + 全请求审计中间件 + 启动建表
app/auth.py        key+设备指纹换 token，token HMAC 自签名，不落会话表
app/store.py       SQLite 存储层，幂等靠 UNIQUE 约束兜底
app/errors.py      错误码只从契约包取
app/models.py      请求 DTO
app/routers/       auth / heartbeat / tasks / inbox
```

## W1 已定的取舍

- 幂等重放返回 HTTP 200 + 原记录 + `"code": 40901`（`IDEMPOTENT_REPLAY` 语义是"返回原结果"，不是报错）
- 建任务只落 `title`/`summary`，`prompt`/`content` 等原文字段在 DTO 层丢弃（AGENTS 硬禁令 4）
- inbox 拉取即把任务置 `running` 并绑定终端，防止两台机器抢同一单
- 审计由中间件兜底记全部请求；`auth/token` 额外记一条带 user_id 的登录事件
