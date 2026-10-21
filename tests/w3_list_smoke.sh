#!/usr/bin/env bash
# W3 冒烟：换 token → 建 3 个任务 → 列表全量 → limit=2 翻页 → 游标续取 → 乱码游标容错 → 无 token 401。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${TAP_SMOKE_PORT:-8125}"
BASE="http://127.0.0.1:${PORT}"
PY="$ROOT/server/.venv/bin/python"
SMOKE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/tap_w3_list.XXXXXX")"
DB="$SMOKE_DIR/tap_w3.db"
LOG="$SMOKE_DIR/tap_w3.log"
API_KEY="smoke-key-w3"
BODY_FILE="$SMOKE_DIR/body.json"
TOKEN=""
HTTP_CODE=""
BODY=""
SERVER_PID=""

cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$SMOKE_DIR"
}
trap cleanup EXIT

if [ ! -x "$PY" ]; then
  echo "缺少 venv：先跑 cd server && /usr/bin/python3 -m venv .venv && .venv/bin/pip install -r requirements.txt" >&2
  exit 1
fi

# 所有 Python/服务进程都不继承外层 venv 与 PYTHONPATH，避免跑成别的解释器上的依赖。
pyrun() { env -u VIRTUAL_ENV -u PYTHONPATH "$PY" "$@"; }

echo "== 启动控制面 (db=$DB, port=$PORT) =="
cd "$ROOT/server"
env -u VIRTUAL_ENV -u PYTHONPATH TAP_DB="$DB" TAP_SEED_API_KEY="$API_KEY" TAP_SECRET="smoke-secret" \
  "$PY" -m uvicorn app.main:app --host 127.0.0.1 --port "$PORT" >"$LOG" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 40); do
  if curl -s -o /dev/null "$BASE/v1/tasks"; then
    break
  fi
  sleep 0.25
done
if ! curl -s -o /dev/null "$BASE/v1/tasks"; then
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

jget() { # 点号取 JSON 字段，数组用数字下标；null 打印 None
  printf '%s' "$BODY" | pyrun -c '
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

expect_eq() { # 实际 期望 说明
  if [ "$1" != "$2" ]; then
    echo "FAIL: $3 — 期望 [$2] 实际 [$1]"
    echo "body: $BODY"
    exit 1
  fi
  echo "PASS: $3 = $1"
}

echo "== 1. 换 token =="
curl_call POST /v1/auth/token "{\"api_key\":\"$API_KEY\",\"device_fingerprint\":\"smoke-mac-w3\",\"os\":\"macOS\"}" no
expect_code 200 "auth/token 正确 key"
TOKEN="$(jget token)"
echo "     token=${TOKEN:0:24}… terminal=$(jget terminal_id)"

echo "== 2. 建 3 个任务（不同 request_id）=="
CREATED_IDS=""
for n in 1 2 3; do
  curl_call POST /v1/tasks "{\"request_id\":\"w3-list-req-000$n\",\"title\":\"W3 列表冒烟任务 $n\",\"type\":\"standard\",\"source\":\"web\",\"summary\":\"第 $n 条，用于翻页\"}"
  expect_code 201 "tasks 创建第 $n 条"
  CREATED_IDS="$CREATED_IDS $(jget id)"
done
echo "     已建：$CREATED_IDS"

echo "== 3. GET /v1/tasks 全量 =="
curl_call GET /v1/tasks
expect_code 200 "tasks 列表全量"
expect_eq "$(jget count)" "3" "列表条数"
expect_eq "$(jget next_cursor)" "None" "只有 3 条时不给 next_cursor"
ITEMS_JSON="$(cat "$BODY_FILE")"
pyrun - "$ITEMS_JSON" "$CREATED_IDS" <<'PY'
import json, sys
body = json.loads(sys.argv[1])
created = set(sys.argv[2].split())
fields = {"task_id", "title", "type", "status", "request_id", "created_at", "updated_at"}
got = {item["task_id"] for item in body["items"]}
assert got == created, f"列表任务集合不对: {got} != {created}"
for item in body["items"]:
    missing = fields - set(item)
    assert not missing, f"卡片缺字段 {sorted(missing)}: {item}"
    assert item["type"] == "standard" and item["status"] == "queued", f"字段值不对: {item}"
    assert item["request_id"].startswith("w3-list-req-"), f"request_id 不对: {item}"
print(f"PASS: 3 条卡片字段齐全（{', '.join(sorted(fields))}）")
PY

echo "== 4. limit=2 只取两条 =="
curl_call GET "/v1/tasks?limit=2"
expect_code 200 "tasks 列表 limit=2"
expect_eq "$(jget count)" "2" "第一页条数"
CURSOR="$(jget next_cursor)"
if [ -z "$CURSOR" ] || [ "$CURSOR" = "None" ]; then
  echo "FAIL: 还有剩余任务却没给 next_cursor —— body=$BODY"
  exit 1
fi
echo "PASS: next_cursor=$CURSOR"
PAGE1_JSON="$(cat "$BODY_FILE")"

echo "== 5. 带 next_cursor 续取，应剩 1 条 =="
curl_call GET "/v1/tasks?limit=2&cursor=$CURSOR"
expect_code 200 "tasks 列表 cursor 续页"
expect_eq "$(jget count)" "1" "第二页条数"
expect_eq "$(jget next_cursor)" "None" "末页不再给 next_cursor"
PAGE2_JSON="$(cat "$BODY_FILE")"

pyrun - "$PAGE1_JSON" "$PAGE2_JSON" <<'PY'
import json, sys
page1 = json.loads(sys.argv[1])
page2 = json.loads(sys.argv[2])
ids1 = [item["task_id"] for item in page1["items"]]
ids2 = [item["task_id"] for item in page2["items"]]
assert len(set(ids1 + ids2)) == 3, f"两页加起来不是 3 条: {ids1 + ids2}"
assert not set(ids1) & set(ids2), f"翻页出现重复: {ids1 + ids2}"
print("PASS: 两页拼起来 3 条、无重复无遗漏")
PY

echo "== 6. 乱码游标不炸（200 空页或容错，不得 500）=="
curl_call GET "/v1/tasks?cursor=%E4%B9%B1%E7%A0%81not-a-task-id"
expect_code 200 "tasks 列表 乱码 cursor"
expect_eq "$(jget count)" "0" "乱码游标返回空页"
expect_eq "$(jget next_cursor)" "None" "乱码游标不给 next_cursor"

echo "== 7. 越界 limit 被拒（契约写 1..200）=="
curl_call GET "/v1/tasks?limit=201"
expect_code 422 "tasks 列表 limit=201"
curl_call GET "/v1/tasks?limit=0"
expect_code 422 "tasks 列表 limit=0"

echo "== 8. 无 token 一律 401 =="
curl_call GET /v1/tasks "" no
expect_code 401 "无 token 拉 tasks 列表"
expect_eq "$(jget detail.code)" "40101" "缺 token 错误码"

echo
echo "全部通过：W3 GET /v1/tasks 列表+游标分页 已跑通"
