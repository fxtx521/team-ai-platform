#!/usr/bin/env bash
# W2 冒烟：换 token → 插技能 → 全量/增量拉技能 → 模型路由（无配置/有配置/坏配置）→ 鉴权失败。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${TAP_SMOKE_PORT:-8124}"
BASE="http://127.0.0.1:${PORT}"
PY="$ROOT/server/.venv/bin/python"
DB="$ROOT/server/tap_w2_smoke.db"
LOG="$ROOT/server/tap_w2_smoke.log"
API_KEY="smoke-key-w2"
BODY_FILE="$(mktemp -t tap_w2_body.XXXXXX)"
TOKEN=""
HTTP_CODE=""
BODY=""
SERVER_PID=""

cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
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
# shellcheck disable=SC1091
source "$ROOT/server/.venv/bin/activate"
cd "$ROOT/server"

rm -f "$DB" "$DB-wal" "$DB-shm" "$LOG"

start_server() { # [额外 TAP_* 赋值...]；路由环境变量默认清空，保证"未配置"场景真未配置
  env -u PYTHONPATH -u TAP_MODEL_ROUTE -u TAP_PROXY_BASE_URL \
    TAP_DB="$DB" TAP_SEED_API_KEY="$API_KEY" TAP_SECRET="smoke-secret" "$@" \
    "$PY" -m uvicorn app.main:app --host 127.0.0.1 --port "$PORT" >"$LOG" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 40); do
    if curl -s -o /dev/null "$BASE/v1/skills"; then
      return 0
    fi
    sleep 0.25
  done
  echo "服务未起来，日志：" >&2
  cat "$LOG" >&2
  return 1
}

stop_server() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  SERVER_PID=""
  for _ in $(seq 1 20); do
    if ! curl -s -o /dev/null "$BASE/v1/skills"; then
      return 0
    fi
    sleep 0.25
  done
  echo "旧进程没停干净（port=$PORT）" >&2
  return 1
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
    exit 1
  fi
  echo "PASS: $3 = $2"
}

echo "== 1. 启动控制面 (db=$DB, port=$PORT, 无模型路由配置) =="
start_server
echo "PASS: 服务就绪"

echo "== 2. 换 token =="
curl_call POST /v1/auth/token "{\"api_key\":\"$API_KEY\",\"device_fingerprint\":\"smoke-w2-mac\",\"os\":\"macOS\"}" no
expect_code 200 "auth/token 正确 key"
TOKEN="$(jget token)"
echo "     token=${TOKEN:0:24}… terminal=$(jget terminal_id)"

echo "== 3. 直插 2 条技能（技能写入 W2 不开接口，先验读路径） =="
sqlite3 "$DB" <<'SQL'
INSERT OR REPLACE INTO skills (id, name, version, content, visibility, updated_at) VALUES
  ('skill_smoke_1', '冒烟技能一', 1, '把会议记录整理成周报', 'company', '2026-10-20T09:00:00Z'),
  ('skill_smoke_2', '冒烟技能二', 3, '从聊天记录提取待办', 'group', '2026-10-21T09:00:00Z');
INSERT OR REPLACE INTO skill_versions (skill_id, version, content, updated_at) VALUES
  ('skill_smoke_2', 3, '技能二 v3 内容', '2026-10-21T09:00:00Z');
SQL
expect_eq "$(sqlite3 "$DB" 'SELECT COUNT(*) FROM skills;')" "2" "skills 表落库行数"
expect_eq "$(sqlite3 "$DB" 'SELECT COUNT(*) FROM skill_versions;')" "1" "skill_versions 表落库行数"

echo "== 4. GET /v1/skills 无 since → 全量 =="
curl_call GET /v1/skills
expect_code 200 "skills 全量"
expect_eq "$(jget count)" "2" "skills 全量条数"
expect_eq "$(jget items.0.id)" "skill_smoke_1" "首条按 updated_at 升序"
expect_eq "$(jget items.1.version)" "3" "技能版本号透出"
expect_eq "$(jget cursor)" "2026-10-21T09:00:00Z" "cursor 取最大 updated_at"

echo "== 5. GET /v1/skills?since=<过去> → 仍是全量 2 条 =="
curl_call GET "/v1/skills?since=2020-01-01T00:00:00Z"
expect_code 200 "skills 增量（过去游标）"
expect_eq "$(jget count)" "2" "过去游标返回全量"

echo "== 6. GET /v1/skills?since=<未来> → 空列表 =="
curl_call GET "/v1/skills?since=2099-01-01T00:00:00Z"
expect_code 200 "skills 增量（未来游标）"
expect_eq "$(jget count)" "0" "未来游标无新条目"
expect_eq "$(jget cursor)" "2099-01-01T00:00:00Z" "空结果时游标原样保留"

echo "== 7. GET /v1/model/route 未配置 → 空模型但 200 =="
curl_call GET /v1/model/route
expect_code 200 "model/route 无配置"
"$PY" - "$BODY_FILE" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
assert body["models"] == [], f"models 应为空: {body}"
assert body["proxy_base_url"] == "", f"proxy_base_url 应为空: {body}"
assert body["note"] == "keys are never sent to terminals", f"note 不符: {body}"
print("PASS: models=[] proxy_base_url='' note 正确")
PY

echo "== 8. 带 TAP_MODEL_ROUTE 重启 → 返回配置的模型 =="
stop_server
ROUTE_JSON='[{"id":"gpt-4o","provider":"openai","base_url":"https://gw.corp/v1"},{"id":"claude-sonnet","provider":"anthropic","base_url":"https://gw.corp/anthropic","enabled":false}]'
start_server TAP_MODEL_ROUTE="$ROUTE_JSON" TAP_PROXY_BASE_URL="https://proxy.corp:8443"
curl_call GET /v1/model/route
expect_code 200 "model/route 有配置"
"$PY" - "$BODY_FILE" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
models = body["models"]
assert len(models) == 2, f"模型条数不对: {body}"
assert models[0] == {
    "id": "gpt-4o",
    "provider": "openai",
    "base_url": "https://gw.corp/v1",
    "enabled": True,
}, models[0]
assert models[1]["enabled"] is False, models[1]
assert body["proxy_base_url"] == "https://proxy.corp:8443", body
assert "api_key" not in json.dumps(models), models
print("PASS: 两个模型按配置返回，enabled/proxy_base_url 正确，无 api_key 字段")
PY
expect_eq "$(jget models.0.id)" "gpt-4o" "首个模型 id"
expect_eq "$(jget proxy_base_url)" "https://proxy.corp:8443" "proxy_base_url 透出"

echo "== 9. 技能表跨重启仍在（表随模块加载建好） =="
curl_call GET /v1/skills
expect_code 200 "重启后 skills 读取"
expect_eq "$(jget count)" "2" "重启后技能条数不变"

echo "== 10. 鉴权失败路径 =="
curl_call GET /v1/skills "" no
expect_code 401 "无 token 拉 skills"
expect_eq "$(jget detail.code)" "40101" "缺 token 错误码"
curl_call GET /v1/model/route "" no
expect_code 401 "无 token 查 model/route"

echo "== 11. 坏配置不炸端点 =="
stop_server
start_server TAP_MODEL_ROUTE='{not-json'
curl_call GET /v1/model/route
expect_code 200 "model/route 坏 JSON"
expect_eq "$(jget note)" "keys are never sent to terminals" "坏配置仍返回 note"
"$PY" - "$BODY_FILE" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
assert body["models"] == [], f"坏配置应退化为空列表: {body}"
print("PASS: 坏 JSON 退化为 models=[] 而非 500")
PY

echo
echo "全部通过：W2 skills/model_route 双端点 已跑通"
