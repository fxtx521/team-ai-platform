#!/usr/bin/env python3
"""契约一致性校验（CI 用）：openapi.yaml vs Python/Go 常量包，任一漂移非零退出。

安全说明：CI 中无任何 untrusted 输入进入 run: 或 ref:（无 issue/PR 标题引用），
本脚本只读仓库文件，属纯静态校验。
"""
import subprocess
import sys
import os

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

try:
    import yaml
except ImportError:
    sys.exit("需要 pyyaml: pip install pyyaml")

failures = []

# 1) openapi 结构完整
spec = yaml.safe_load(open(f"{ROOT}/contracts/openapi.yaml", encoding="utf-8"))
required = [
    "/auth/token", "/heartbeat", "/tasks", "/tasks/{task_id}/steps",
    "/tasks/{task_id}/artifacts", "/inbox/dispatch", "/inbox/pull",
    "/skills", "/model/route",
]
missing = [p for p in required if p not in spec["paths"]]
if missing or not str(spec.get("openapi", "")).startswith("3."):
    failures.append(f"openapi: missing={missing}")
else:
    print(f"[1] openapi.yaml OK ({len(spec['paths'])} paths)")

# 2) Python 常量包：可导入且取值精确
r = subprocess.run(
    [sys.executable, "-c",
     "import sys; sys.path.insert(0, '%s/contracts');"
     "from schema import API, ERROR_CODES, TaskType, JobStatus;"
     "assert len(API) == 9 and len(ERROR_CODES) == 7;"
     "assert set(t.value for t in TaskType) == {'local','standard','sensitive'};"
     "assert set(s.value for s in JobStatus) == {'queued','running','done','failed','cancelled'};"
     "print('exact')" % ROOT],
    capture_output=True, text=True)
if r.returncode != 0 or "exact" not in r.stdout:
    failures.append(f"schema.py: {r.stderr.strip()[:200]}")
else:
    print("[2] schema.py OK")

# 3) Go 常量包：能编译且源码含全部契约值（单向包含 = 零漂移的必要条件）
g = subprocess.run(["go", "build", "./..."], cwd=f"{ROOT}/contracts",
                   capture_output=True, text=True)
if g.returncode != 0:
    failures.append(f"go build: {g.stderr.strip()[:200]}")
else:
    go_src = open(f"{ROOT}/contracts/go/contracts.go", encoding="utf-8").read()
    sys.path.insert(0, f"{ROOT}/contracts")
    from schema import API, ERROR_CODES
    drift = [API[k] for k in ("auth_token", "heartbeat", "tasks",
                              "inbox_pull", "skills", "model_route")
             if API[k] not in go_src]
    drift += [str(ERROR_CODES[k]) for k in ("AUTH_FAILED", "TOKEN_EXPIRED",
                                            "PATH_ESCAPE", "RATE_LIMITED")
              if str(ERROR_CODES[k]) not in go_src]
    if drift:
        failures.append(f"Go/Py drift: {drift}")
    else:
        print("[3] Go contracts OK + parity")

print("\nALL CHECKS PASSED" if not failures else "\nFAILURES:\n" + "\n".join(failures))
sys.exit(1 if failures else 0)
