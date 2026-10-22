# team-ai-platform

**English** | [中文](./README_CN.md)

Enterprise Team AI Asset Platform — a single Go executor plus cloud orchestration for a cross-platform enterprise AI workspace.

Every employee on Mac or Windows installs **the same app**: one Go codebase, one build pipeline, two platform binaries.

- **Endpoint**: a lightweight (~tens of MB) local executor (files / commands / browser / reporting / heartbeat). Double-click to run, zero dependencies.
- **Server**: FastAPI control plane + agent orchestrator (the brain) + LLM gateway (centralized API keys, role-based access, usage accounted per user).
- **UI**: a browser-based company workspace — one codebase, accessible from both platforms.

## Architecture

See `docs/` for the design documents (technical blueprint V1.3).

## Repository Layout

- `server/` — control plane (FastAPI + orchestrator + LLM gateway)
- `executor/` — employee endpoint (Go, cross-platform)
- `web/` — workspace frontend (zero-CDN, works on intranets)
- `tests/` — contract tests and integration smoke suites
- `contracts/` — OpenAPI + shared schema (single source of truth)

## Status

Work packages W1–W5: server skeleton, orchestrator with three privacy tiers, Go executor toolset, web workspace, and self-healing reconnect drills are done and green. Dual-machine pilot pending.

## License

Apache-2.0
