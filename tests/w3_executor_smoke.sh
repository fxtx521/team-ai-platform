#!/usr/bin/env bash
# W3 冒烟：执行器 ↔ 服务端真连通。
#
# 链路：起 FastAPI 控制面 → 起 Go 执行器（真认证/真心跳）→ curl 建 3 个任务
#       （成功链 / 路径越界 / 命令非零退出）→ 执行器自己拉活执行上报 →
#       断言文件真落盘、步骤真落库、审计真有记录、任务状态真变化。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${TAP_W3_PORT:-8126}"
BASE="http://127.0.0.1:${PORT}"
PY="$ROOT/server/.venv/bin/python"
API_KEY="test-w3"
FP="w3test"
WORK="$(mktemp -d -t tap_w3_smoke.XXXXXX)"
DB="$WORK/tap_w3.db"
LOG="$WORK/server.log"
EXEC_LOG="$WORK/executor.log"
DATA="$WORK/data"
BIN="$WORK/executor"
TOKEN=""
HTTP_CODE=""
BODY=""
SERVER_PID=""
EXEC_PID=""
BODY_FILE="$WORK/body.json"

cleanup() {
  local code=$?
  for pid in "$EXEC_PID" "$SERVER_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [ "$code" -eq 0 ]; then
    rm -rf "$WORK"
  else
    echo "现场保留在 ${WORK}（server.log / executor.log）" >&2
  fi
}
trap cleanup EXIT

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

expect_eq() { # 实际 期望 说明
  if [ "$1" != "$2" ]; then
    fail "$3 — 期望 [$2] 实际 [$1]"
  fi
  pass "$3 = $2"
}

expect_contains() { # 文本 子串 说明
  case "$1" in
    *"$2"*) pass "$3（含 \"$2\"）" ;;
    *) fail "$3 — 未找到 \"$2\"，实际: $1" ;;
  esac
}

curl_call() { # method path [body] [auth: yes|no]
  local method="$1" path="$2" body="${3-}" auth="${4:-yes}"
  local args=(-s -o "$BODY_FILE" -w "%{http_code}" -X "$method" "$BASE$path" -H "Content-Type: application/json")
  if [ "$auth" = "yes" ] && [ -n "$TOKEN" ]; then
    args+=(-H "Authorization: Bearer $TOKEN")
  fi
  if [ -n "$body" ]; then
    args+=(-d "$body")
  fi
  HTTP_CODE="$(curl "${args[@]}")"
  BODY="$(cat "$BODY_FILE")"
}

jget() { # 点号取 JSON 字段，数组用数字下标
  printf '%s' "$BODY" | "$PY" -c '
import json, sys
node = json.load(sys.stdin)
for part in sys.argv[1].split("."):
    node = node[int(part)] if part.lstrip("-").isdigit() else node[part]
print(node)
' "$1"
}

sql() { sqlite3 "$DB" "$1"; }

file_mode() { stat -f %Lp "$1" 2>/dev/null || stat -c %a "$1"; }

b64_of() { "$PY" -c 'import base64,sys; print(base64.b64encode(sys.argv[1].encode()).decode())' "$1"; }

make_task_body() { # request_id title summary
  "$PY" - "$1" "$2" "$3" <<'PY'
import json, sys
print(json.dumps({
    "request_id": sys.argv[1],
    "title": sys.argv[2],
    "type": "local",
    "source": "executor",
    "summary": sys.argv[3],
}, ensure_ascii=False))
PY
}

wait_task_status() { # task_id 期望状态 超时秒
  local tid="$1" want="$2" limit="${3:-30}" i
  for i in $(seq 1 $((limit * 4))); do
    if [ "$(sql "SELECT status FROM tasks WHERE id='$tid';")" = "$want" ]; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}

# ---------------------------------------------------------------------------

if [ ! -x "$PY" ]; then
  echo "缺少 venv：先跑 cd server && /usr/bin/python3 -m venv .venv && .venv/bin/pip install -r requirements.txt" >&2
  exit 1
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "缺少 sqlite3 命令行（断言落库要用）" >&2
  exit 1
fi
if curl -s -o /dev/null --max-time 1 "$BASE/v1/inbox/pull"; then
  echo "端口 $PORT 已被占用，换一个：TAP_W3_PORT=8127 bash tests/w3_executor_smoke.sh" >&2
  exit 1
fi

echo "== 1. 编译执行器（真实二进制，不用 go run 掩盖构建问题） =="
(cd "$ROOT/executor" && go build -trimpath -o "$BIN" .) || fail "go build 失败"
[ -x "$BIN" ] || fail "产物不可执行: $BIN"
pass "go build 成功"

echo "== 2. 启动控制面 (db=$DB, port=$PORT) =="
cd "$ROOT/server"
# shellcheck disable=SC1091
source "$ROOT/server/.venv/bin/activate"
env -u VIRTUAL_ENV -u PYTHONPATH \
  TAP_DB="$DB" TAP_SEED_API_KEY="$API_KEY" TAP_SECRET="w3-smoke-secret" \
  "$PY" -m uvicorn app.main:app --host 127.0.0.1 --port "$PORT" >"$LOG" 2>&1 &
SERVER_PID=$!

ready=""
for _ in $(seq 1 60); do
  code="$(curl -s -o /dev/null -w "%{http_code}" --max-time 1 "$BASE/v1/inbox/pull" || true)"
  if [ "$code" = "401" ]; then # 401 = 没 token，但应用已经能应答
    ready=1
    break
  fi
  sleep 0.25
done
if [ -z "$ready" ]; then
  echo "服务未起来，日志：" >&2
  cat "$LOG" >&2
  exit 1
fi
pass "服务就绪（/v1/inbox/pull 无 token → 401）"

echo "== 3. 错误 key 启动执行器 → 退出码必须为 1 =="
set +e
TAP_SERVER_URL="$BASE" TAP_API_KEY="wrong-key" TAP_DEVICE_FP="w3bad" \
  TAP_DATA_DIR="$WORK/bad-data" "$BIN" >"$WORK/authfail.log" 2>&1
bad_code=$?
set -e
expect_eq "$bad_code" "1" "认证失败退出码"
expect_contains "$(cat "$WORK/authfail.log")" "认证失败" "认证失败日志"

echo "== 4. 启动执行器（TAP_POLL_INTERVAL 压到 1s，免得冒烟干等 30s） =="
TAP_SERVER_URL="$BASE" TAP_API_KEY="$API_KEY" TAP_DEVICE_FP="$FP" \
  TAP_DATA_DIR="$DATA" TAP_POLL_INTERVAL=1s "$BIN" >"$EXEC_LOG" 2>&1 &
EXEC_PID=$!

authed=""
for _ in $(seq 1 80); do
  if grep -q "executor 启动" "$EXEC_LOG" 2>/dev/null; then
    authed=1
    break
  fi
  if ! kill -0 "$EXEC_PID" 2>/dev/null; then
    echo "执行器提前退出，日志：" >&2
    cat "$EXEC_LOG" >&2
    exit 1
  fi
  sleep 0.25
done
[ -n "$authed" ] || { echo "执行器未完成认证，日志：" >&2; cat "$EXEC_LOG" >&2; exit 1; }
expect_contains "$(cat "$EXEC_LOG")" "tools=cmd,file_read,file_write" "启动日志列出注册工具"

echo "== 5. 心跳真到服务端 =="
beat=""
for _ in $(seq 1 40); do
  if [ "$(sql "SELECT COUNT(*) FROM audit_logs WHERE target='POST /v1/heartbeat';")" -ge 1 ]; then
    beat=1
    break
  fi
  sleep 0.25
done
[ -n "$beat" ] || fail "服务端没有任何心跳审计记录"
pass "心跳审计 ≥1 条"
expect_eq "$(sql "SELECT device_fingerprint FROM terminals WHERE device_fingerprint='$FP';")" "$FP" "终端注册落库"
[ -n "$(sql "SELECT version FROM terminals WHERE device_fingerprint='$FP';")" ] || fail "心跳未回填版本号"
pass "心跳回填了终端版本号"

echo "== 6. 用 curl 换 token 并建任务（指纹与执行器一致，任务才会被本机拉走） =="
curl_call POST /v1/auth/token "{\"api_key\":\"$API_KEY\",\"device_fingerprint\":\"$FP\"}" no
expect_eq "$HTTP_CODE" "200" "auth/token"
TOKEN="$(jget token)"

CONTENT="w3 执行器打通：file_write 落盘成功"
B64="$(b64_of "$CONTENT")"
STEPS_OK="$("$PY" - "$B64" <<'PY'
import json, sys
b64 = sys.argv[1]
print(json.dumps([
    {"tool": "file_write", "args": {"path": "notes/hello.txt", "content_b64": b64}},
    {"tool": "file_read", "args": {"path": "notes/hello.txt"}},
    {"tool": "cmd", "args": {"command": "/bin/echo", "args": ["w3-ok"]}},
]))
PY
)"
curl_call POST /v1/tasks "$(make_task_body "w3-smoke-ok-$$" "W3 冒烟：写文件+读回+跑命令" "$STEPS_OK")"
expect_eq "$HTTP_CODE" "201" "建任务（成功链）"
T1="$(jget id)"
pass "任务 A = $T1"
expect_eq "$(jget status)" "queued" "新任务初始状态"

echo "== 7. 等执行器拉活：任务 A 应变 done =="
wait_task_status "$T1" done 30 || fail "任务 A 未在 30s 内 done（当前 $(sql "SELECT status FROM tasks WHERE id='$T1';")）"
pass "任务 A 状态 done"

echo "== 8. 文件真的写到授权目录里 =="
[ -f "$DATA/notes/hello.txt" ] || fail "文件没落盘: $DATA/notes/hello.txt"
printf '%s' "$CONTENT" >"$WORK/expected.txt"
cmp -s "$WORK/expected.txt" "$DATA/notes/hello.txt" || fail "文件内容不一致: $(cat "$DATA/notes/hello.txt")"
pass "文件内容与下发一致"
expect_eq "$(file_mode "$DATA/notes/hello.txt")" "640" "文件权限"

echo "== 9. 步骤真落库（顺序/工具/成败） =="
got_steps="$(sql "SELECT seq||':'||tool||':'||ok FROM task_steps WHERE task_id='$T1' ORDER BY seq;")"
expect_eq "$got_steps" "$(printf '1:file_write:1\n2:file_read:1\n3:cmd:1')" "任务 A 三步全成功"
expect_eq "$(sql "SELECT COUNT(*) FROM task_steps WHERE task_id='$T1' AND started_at IS NOT NULL AND finished_at IS NOT NULL;")" "3" "步骤带起止时间（契约必填）"
expect_eq "$(sql "SELECT COUNT(*) FROM audit_logs WHERE target='POST /v1/tasks/$T1/steps' AND status_code=200;")" "1" "上报步骤审计 200"

echo "== 10. 路径硬边界：越界写必须失败且不落盘 =="
STEPS_ESCAPE='[{"tool":"file_write","args":{"path":"../escape.txt","content_b64":"'$(b64_of "escape")'"}}]'
curl_call POST /v1/tasks "$(make_task_body "w3-smoke-escape-$$" "W3 冒烟：路径越界" "$STEPS_ESCAPE")"
expect_eq "$HTTP_CODE" "201" "建任务（越界）"
T2="$(jget id)"
wait_task_status "$T2" failed 30 || fail "任务 B 越界写竟然没失败"
pass "任务 B 状态 failed"
expect_eq "$(sql "SELECT ok FROM task_steps WHERE task_id='$T2';")" "0" "越界步骤 ok=0"
expect_contains "$(sql "SELECT error FROM task_steps WHERE task_id='$T2';")" "escapes authorized dir" "越界错误原因透出"
if [ -f "$WORK/escape.txt" ] || [ -f "$DATA/../escape.txt" ]; then
  fail "越界文件竟然落盘了"
fi
pass "授权目录外无残留文件"

echo "== 11. 命令非零退出：ok=0 且原因里带退出码 =="
STEPS_FAIL='[{"tool":"cmd","args":{"command":"ls","args":["/definitely/not/here"]}}]'
curl_call POST /v1/tasks "$(make_task_body "w3-smoke-cmd-$$" "W3 冒烟：命令非零退出" "$STEPS_FAIL")"
expect_eq "$HTTP_CODE" "201" "建任务（命令失败）"
T3="$(jget id)"
wait_task_status "$T3" failed 30 || fail "任务 C 未失败"
pass "任务 C 状态 failed"
expect_contains "$(sql "SELECT error FROM task_steps WHERE task_id='$T3';")" "退出码" "步骤错误含退出码"

echo "== 12. curl 查任务状态（看板端点） =="
curl_call GET "/v1/tasks?limit=50"
if [ "$HTTP_CODE" = "200" ]; then
  found="$("$PY" - "$BODY_FILE" "$T1" "$T2" "$T3" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
want = {sys.argv[2]: "done", sys.argv[3]: "failed", sys.argv[4]: "failed"}
have = {item["task_id"]: item["status"] for item in body["items"]}
missing = {k: v for k, v in want.items() if have.get(k) != v}
print("OK" if not missing else f"不符: {missing} / 实际: {have}")
PY
)"
  expect_eq "$found" "OK" "GET /v1/tasks 三个任务状态一致"
else
  # 列表端点属服务端并行开发范围，W3 断言不依赖它：状态已由库直查确认。
  echo "SKIP: GET /v1/tasks 返回 ${HTTP_CODE}（状态已由 task_steps/tasks 直查确认）"
fi

echo "== 13. 执行器日志（拉活→执行→上报 全链路留痕） =="
expect_contains "$(cat "$EXEC_LOG")" "已上报 status=done" "执行器上报成功日志"
expect_contains "$(cat "$EXEC_LOG")" "已上报 status=failed" "执行器上报失败日志"
if kill -0 "$EXEC_PID" 2>/dev/null; then
  pass "执行器全程存活（无 panic 退出）"
else
  fail "执行器已退出"
fi

echo
echo "全部通过：W3 执行器 ↔ 服务端 真连通（3 任务 / 5 步骤 / 文件落盘 / 审计留痕）"
