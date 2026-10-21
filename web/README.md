# web/ — Web 工作台（唯一 UI）

纯静态：两个 HTML，零框架、零构建链、零外部资源引用（不依赖任何 CDN/远程字体），
内网离线可用。所有交互只用原生 `fetch` 打服务端 REST API。

```
web/
├── index.html      登录页：API Key + 设备标识换 token，存 localStorage
└── dashboard.html  看板页：终端状态卡 + 任务列表（10s 自动刷新）+ 待办
```

## 本地跑

服务端（另开一个终端，仓库根目录执行）：

```bash
cd server
env -u VIRTUAL_ENV -u PYTHONPATH TAP_DB=/tmp/tap.db TAP_SEED_API_KEY=dev-key-local \
  .venv/bin/python -m uvicorn app.main:app --port 8000
```

静态站：

```bash
cd web && python3 -m http.server 8080
```

打开 `http://127.0.0.1:8080/index.html`，Key 填 `dev-key-local`（即 `TAP_SEED_API_KEY`）。

**服务地址**一栏留空 = 同源 `/v1`（服务端托管 web/ 时的形态）；分开部署就填 `127.0.0.1:8000`
这种 `主机:端口`，页面会自动补 `//` 前缀和 `/v1` 后缀。

## 部署形态

设计形态是**服务端托管 web/**，浏览器与 API 同源，`/v1` 相对路径直接可用，不需要 CORS。

目前 `server/` 还没挂静态目录，也没发 `Access-Control-Allow-Origin`，所以
「静态站和 API 分不同端口」这种开发态会被浏览器按同源策略拦掉（现象是登录页报
"连不上服务端"，但 `curl` 同一个地址却是通的）。要支持这种分开部署，需在服务端加
CORS 中间件或将 `web/` 挂到 FastAPI 静态路由上 —— 属于 server/ 的改动，本目录无权处理。

## 约定

- localStorage 键统一 `tap.` 前缀：`tap.apiKey` / `tap.token` / `tap.tokenExpires` /
  `tap.apiBase` / `tap.deviceId` / `tap.userId` / `tap.terminalId`，两个页面共用同一套，改一处要同步。
- 服务地址存在 `tap.apiBase`，默认 `/v1`（相对路径，天然同源）。
- 设备标识 `tap.deviceId`：浏览器拿不到硬件指纹，用本地生成并持久化的随机 ID 代替，
  清缓存/换浏览器即视为新设备——不含任何可识别个人的明文。
- 401 一律清 token 并跳回 `index.html?expired=1`；登录页读这个参数提示"登录已过期"。
- 排错只用 `console.error`，不留 `console.log`。

## 冒烟

```bash
./tests/w4_web_smoke.sh   # 起服务端 8127 + 静态站 8128，验页面可达/零外链/端点对契约/401/任务数据通路
```
