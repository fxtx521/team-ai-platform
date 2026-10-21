# AGENTS.md — team-ai-platform 开发手册

> 本文件是给 AI 开发者（Hermes/DSH/子智能体）和人类协作者的仓库宪法。
> 每个设计决策必须写"为什么"。人走了知识在，上下文丢了知识也在。

## 项目一句话

Mac/Win 统一的企业 AI 工作台：Go 单代码库执行器（员工本地）+ FastAPI 服务端（大脑+账房）+ Web 工作台（唯一 UI）。

## 硬禁令（违反即打回）

1. 执行器不得出现 UI 代码（UI 只在 web/）
2. 执行器不得内置模型调用（大脑只在 server/，key 永不下发到员工机）
3. 执行器不得做本地记忆存储（记忆在服务端）
4. 服务端不得落盘任务原文（L2 只存摘要与产物引用）
5. 不上 K8s/微服务/消息队列；不做多租户计费；不做屏幕录制；不做远程控制
6. 写接口必须幂等（request_id）
7. 路径硬边界：执行器文件操作必须在授权目录内，拒绝绝对路径与符号链接逃逸
8. 禁止 git push --force 到 main

## 模块边界表

模块 | 路径 | 负责人 | 依赖方向
--- | --- | --- | ---
服务端控制面 | server/app/ | 小慧 | 只依赖 server/core
Agent 编排器 | server/orchestrator/ | 小慧 | 依赖 server/core
LLM 网关 | server/gateway/ | 小慧 | 依赖 server/core
执行器 | executor/ | 砺(Go) | 只依赖 executor/internal + 契约包
契约定义 | contracts/ | 双方共管 | 零依赖（两端都 import 它）
Web 工作台 | web/ | 小慧 | 只调服务端 REST API

## 契约即法律

API 契约在 `contracts/openapi.yaml`。两端改动先改契约、评审、再写实现。
执行器和服务端对契约的实现偏差 = bug，不是"特性"。

## 提交规范

- 分支：feat/xxx、fix/xxx、docs/xxx
- 提交信息：一行说清人话，如"执行器: 文件工具加路径边界校验"
- 合并到 main 必须过 CI（契约测试+双平台构建）
- 每次 PR 必须自述：改了什么、为什么、怎么验的

## 测试策略

- 契约测试：CI 对同一 mock 服务跑执行器报文一致性
- 单元测试：路径边界、幂等、脱敏正则必须 100% 覆盖
- 集成测试：一个任务从下发到上报的全链路 curl 脚本

## 当前阶段

W1（服务端骨架+API 全通）进行中。里程碑见 docs/技术方案_团队AI资产平台V1.3统一版.md 第七章。
