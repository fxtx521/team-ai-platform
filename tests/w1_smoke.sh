#!/usr/bin/env bash
# W1 冒烟：换 token → 建任务 → 拉 inbox → 报步骤 → 登记产物 → 再拉 inbox，顺带验幂等与鉴权。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${TAP_SMOKE_PORT:-8123}"
BASE="http://127.0.0.1:${PORT}"
PY="$ROOT/server/.venv/bin/python"
DB="$ROOT/server/tap_smoke.db"
LOG="$ROOT/server/tap_smoke.log"
API_KEY="smoke-key-0001"
BODY_FILE="$(mktemp -t tap_body.XXXXXX)"
TOKEN=""
HTTP_CODE=""
BODY=""

cleanup() {
  if [ -n "${SERVER_PID:-}" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -f "$BODY_FILE"
}
trap cleanup EXIT

if [ ! -x "$PY" ]; then
  echo "缺少 venv：先跑 cd server && /usr/bin/python3 -m venv .venv && .venv/bin/pip install -r requirements.txt" >&2
  exit 1
fi

rm -f "$DB" "$DB-wal" "$DB-shm" "$LOG"

echo "== 启动控制面 (db=$DB, port=$PORT) =="
cd "$ROOT/server"
env -u PYTHONPATH TAP_DB="$DB" TAP_SEED_API_KEY="$API_KEY" TAP_SECRET="smoke-secret" \
  "$PY" -m uvicorn app.main:app --host 127.0.0.1 --port "$PORT" >"$LOG" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 40); do
  if curl -s -o /dev/null "$BASE/v1/inbox/pull"; then
    break
  fi
  sleep 0.25
done
if ! curl -s -o /dev/null "$BASE/v1/inbox/pull"; then
  echo "服务未起来，日志：" >&2
  cat "$LOG" >&2
  exit 1
fi

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

expect_code() { # 期望码 说明
  if [ "$HTTP_CODE" != "$1" ]; then
    echo "FAIL: $2 — 期望 $1 实际 $HTTP_CODE"
    echo "body: $BODY"
    exit 1
  fi
  echo "PASS: $2 → $HTTP_CODE"
}

echo "== 1. 换 token =="
curl_call POST /v1/auth/token "{\"api_key\":\"$API_KEY\",\"device_fingerprint\":\"smoke-mac-aa11\",\"os\":\"macOS\"}" no
expect_code 200 "auth/token 正确 key"
TOKEN="$(jget token)"
echo "     token=${TOKEN:0:24}… terminal=$(jget terminal_id)"

echo "== 2. 心跳 =="
curl_call POST /v1/heartbeat '{"version":"0.1.0-smoke","queue_len":0}'
expect_code 200 "heartbeat"

echo "== 3. 建任务 =="
curl_call POST /v1/tasks '{"request_id":"smoke-req-0001","title":"冒烟：整理本周周报","type":"standard","source":"web","summary":"读取周报模板并生成初稿","prompt":"这段原文不应落库"}'
expect_code 201 "tasks 首次创建"
TASK_ID="$(jget id)"
echo "     task_id=$TASK_ID status=$(jget status)"

echo "== 4. 同一 request_id 重放 =="
curl_call POST /v1/tasks '{"request_id":"smoke-req-0001","title":"冒烟：整理本周周报","type":"standard","source":"web"}'
expect_code 200 "tasks 幂等重放"
if [ "$(jget id)" != "$TASK_ID" ] || [ "$(jget replay)" != "True" ] || [ "$(jget code)" != "40901" ]; then
  echo "FAIL: 重放没有返回原任务/请求码 —— body=$BODY"
  exit 1
fi
echo "PASS: 重放返回原任务且 code=40901"

echo "== 5. inbox 拉取 =="
curl_call GET /v1/inbox/pull
expect_code 200 "inbox/pull 首次"
if [ "$(jget count)" != "1" ] || [ "$(jget tasks.0.id)" != "$TASK_ID" ]; then
  echo "FAIL: inbox 未返回刚建的任务 —— body=$BODY"
  exit 1
fi
echo "PASS: inbox 拉到任务且状态=$(jget tasks.0.status)"

echo "== 6. 上报步骤 =="
STEPS='{"request_id":"smoke-steps-0001","status":"done","steps":[{"tool":"file.read","ok":true,"started_at":"2026-10-22T10:00:00Z","finished_at":"2026-10-22T10:00:01Z","seq":1},{"tool":"file.write","ok":true,"started_at":"2026-10-22T10:00:02Z","finished_at":"2026-10-22T10:00:03Z","seq":2}]}'
curl_call POST "/v1/tasks/$TASK_ID/steps" "$STEPS"
expect_code 200 "tasks/{id}/steps 上报"
if [ "$(jget count)" != "2" ] || [ "$(jget status)" != "done" ]; then
  echo "FAIL: 步骤或任务状态不对 —— body=$BODY"
  exit 1
fi
echo "PASS: 步骤 2 条，任务状态 done"

echo "== 7. 步骤整批重放 =="
curl_call POST "/v1/tasks/$TASK_ID/steps" "$STEPS"
expect_code 200 "steps 幂等重放"
if [ "$(jget replay)" != "True" ] || [ "$(jget count)" != "2" ]; then
  echo "FAIL: 步骤重放没有返回原结果 —— body=$BODY"
  exit 1
fi
echo "PASS: 步骤重放返回原 2 条且 code=$(jget code)"

echo "== 8. 登记产物 =="
SHA="$(printf '%s' "weekly-report-2026w43" | shasum -a 256 | awk '{print $1}')"
curl_call POST "/v1/tasks/$TASK_ID/artifacts" "{\"request_id\":\"smoke-art-0001\",\"sha256\":\"$SHA\",\"visibility\":\"group\",\"name\":\"周报.md\",\"size_bytes\":2048,\"provenance\":{\"producer\":\"$TASK_ID\",\"inputs\":[\"template.docx\"]}}"
expect_code 201 "artifacts 登记"
UPLOAD_URL="$(jget upload_url)"
if [ -z "$UPLOAD_URL" ]; then
  echo "FAIL: 没返回上传地址 —— body=$BODY"
  exit 1
fi
echo "PASS: 产物 $(jget artifact.id) visibility=$(jget artifact.visibility) upload=$UPLOAD_URL"

echo "== 9. 产物重放 =="
curl_call POST "/v1/tasks/$TASK_ID/artifacts" "{\"request_id\":\"smoke-art-0001\",\"sha256\":\"$SHA\",\"visibility\":\"group\"}"
expect_code 200 "artifacts 幂等重放"
if [ "$(jget replay)" != "True" ]; then
  echo "FAIL: 产物重放没有标记 replay —— body=$BODY"
  exit 1
fi
echo "PASS: 产物重放返回原记录"

echo "== 10. 二次拉 inbox 应为空 =="
curl_call GET /v1/inbox/pull
expect_code 200 "inbox/pull 第二次"
if [ "$(jget count)" != "0" ]; then
  echo "FAIL: 已执行任务又被拉一次 —— body=$BODY"
  exit 1
fi
echo "PASS: 已拉走的任务不再重复下发"

echo "== 11. 鉴权失败路径 =="
curl_call POST /v1/auth/token '{"api_key":"wrong-key","device_fingerprint":"smoke-mac-aa11"}' no
expect_code 401 "auth/token 错误 key"
if [ "$(jget detail.code)" != "40101" ]; then
  echo "FAIL: 错误码不是 40101 —— body=$BODY"
  exit 1
fi
curl_call GET /v1/inbox/pull "" no
expect_code 401 "无 token 拉 inbox"

echo "== 12. 审计留痕 =="
AUDIT_TOTAL="$("$PY" -c '
import sqlite3, sys
print(sqlite3.connect(sys.argv[1]).execute("SELECT COUNT(*) FROM audit_logs").fetchone()[0])
' "$DB")"
AUDIT_ACTOR="$("$PY" -c '
import sqlite3, sys
print(sqlite3.connect(sys.argv[1]).execute("SELECT COUNT(*) FROM audit_logs WHERE actor = ?", ("u_dev",)).fetchone()[0])
' "$DB")"
if [ "${AUDIT_TOTAL:-0}" -lt 11 ] || [ "${AUDIT_ACTOR:-0}" -lt 1 ]; then
  echo "FAIL: 审计日志不足（总 ${AUDIT_TOTAL:-0}，带 actor 的 ${AUDIT_ACTOR:-0}）"
  exit 1
fi
echo "PASS: audit_logs 共 $AUDIT_TOTAL 条，其中 $AUDIT_ACTOR 条能定位到用户"

echo "== 13. 原文未落盘 =="
if "$PY" -c '
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
rows = conn.execute("SELECT title, summary FROM tasks").fetchall()
sys.exit(0 if all("不应落库" not in (t or "") + (s or "") for t, s in rows) else 1)
' "$DB"; then
  echo "PASS: 任务原文（prompt）没有进库"
else
  echo "FAIL: 任务原文被落盘了，违反硬禁令 4"
  exit 1
fi

echo
echo "全部通过：W1 任务闭环 已跑通"
