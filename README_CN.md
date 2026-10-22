[English](./README.md) | **中文**

# team-ai-platform

企业团队 AI 资产平台 —— 单一 Go 执行器 + 云端编排的跨平台企业 AI 工作台。

Mac 与 Windows 员工安装**同一个软件**（Go 单代码库，一次构建双平台产物）：

- 员工端：几十 MB 的本地执行器（文件/命令/浏览器/上报/心跳），双击就跑，零依赖
- 服务端：FastAPI 控制面 + Agent 编排器（大脑）+ LLM 网关（key 集中管理、按角色授权、记账到人）
- UI：浏览器打开的公司工作台，一套代码双平台访问

## 架构

见 `docs/` 下设计文档（技术方案 V1.3）。

## 目录

- `server/` 服务端（FastAPI + 编排器 + LLM 网关）
- `executor/` 员工端执行器（Go，跨平台）
- `web/` 工作台前端（零 CDN，内网可用）
- `tests/` 契约测试与集成冒烟
- `contracts/` OpenAPI + 共享 schema（唯一事实源）

## 进度

W1–W5 主体已完成：服务端骨架、编排器（隐私三档）、Go 执行器工具集、Web 工作台、断线自愈演练，全部绿灯。双机真试点进行中。

## License

Apache-2.0
