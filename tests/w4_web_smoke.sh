#!/usr/bin/env bash
# W4 冒烟：Web 工作台骨架（登录页 + 看板页）。
#
# 验的是三件事：
#   1. 两个页面真能被静态服务吐出来（不是只存在于磁盘上）；
#   2. 页面里的 JS 真能解析、真在调契约里的端点、且零外链（内网可用）；
#   3. 页面依赖的服务端行为（Key 换 token、无 token/坏 token 401）真成立。
#
# 已知缺口不掩盖：服务端不发 CORS 头，静态站与 API 分开端口时浏览器会拦跨域请求。
# 该断言只打印 WARN 不失败（web/ 无权改 server/，需服务端加中间件或同源托管 web/）。
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WEB_DIR="$ROOT/web"
PY="$ROOT/server/.venv/bin/python"
PORT="${TAP_W4_API_PORT:-8127}"
WEB_PORT="${TAP_W4_WEB_PORT:-8128}"
BASE="http://127.0.0.1:${PORT}"
WEB_BASE="http://127.0.0.1:${WEB_PORT}"
API_KEY="w4-smoke-key-0001"

WORK="$(mktemp -d -t tap_w4_XXXXXX)"
DB="$WORK/w4.db"
SERVER_LOG="$WORK/server.log"
WEB_LOG="$WORK/web.log"
BODY="$WORK/body.json"
SERVER_PID=""
WEB_PID=""
PASS=0
FAIL=0
WARN=0

ok()   { PASS=$((PASS + 1)); printf '  PASS  %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; }
warn() { WARN=$((WARN + 1)); printf '  WARN  %s\n' "$1"; }

cleanup() {
  for pid in "${WEB_PID:-}" "${SERVER_PID:-}"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== W4 Web 工作台冒烟 =="

# ---------- 0. 前置 ----------
[ -x "$PY" ] || { echo "缺少 venv：${PY}（见 AGENTS.md）" >&2; exit 1; }
for page in index.html dashboard.html; do
  [ -f "$WEB_DIR/$page" ] || { echo "缺少页面：web/$page" >&2; exit 1; }
done

# ---------- 1. 起控制面 ----------
echo "-- 启动控制面 127.0.0.1:$PORT (db=$DB)"
cd "$ROOT/server" || exit 1
env -u VIRTUAL_ENV -u PYTHONPATH \
  TAP_DB="$DB" TAP_SEED_API_KEY="$API_KEY" TAP_SECRET="w4-smoke-secret" \
  "$PY" -m uvicorn app.main:app --host 127.0.0.1 --port "$PORT" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
cd "$ROOT" || exit 1

up=""
for _ in $(seq 1 60); do
  if curl -s -o /dev/null "$BASE/v1/inbox/pull" 2>/dev/null; then up=1; break; fi
  sleep 0.25
done
if [ -z "$up" ]; then
  echo "服务端没起来，日志：" >&2
  cat "$SERVER_LOG" >&2
  exit 1
fi
ok "控制面就绪 $BASE"

# ---------- 2. 起静态服务，页面真可达 ----------
echo "-- 启动静态服务 127.0.0.1:$WEB_PORT (root=$WEB_DIR)"
python3 -m http.server "$WEB_PORT" --bind 127.0.0.1 --directory "$WEB_DIR" >"$WEB_LOG" 2>&1 &
WEB_PID=$!

webup=""
for _ in $(seq 1 40); do
  if curl -s -o /dev/null "$WEB_BASE/index.html" 2>/dev/null; then webup=1; break; fi
  sleep 0.25
done
if [ -z "$webup" ]; then
  echo "静态服务没起来，日志：" >&2
  cat "$WEB_LOG" >&2
  exit 1
fi

for page in index.html dashboard.html; do
  code="$(curl -s -o "$BODY" -w '%{http_code}' "$WEB_BASE/$page")"
  if [ "$code" = "200" ] && grep -q '团队 AI 工作台' "$BODY"; then
    ok "GET /$page → 200 且含中文标题"
  else
    bad "GET /$page → ${code}（期望 200 且含「团队 AI 工作台」）"
  fi
done

# ---------- 3. 静态检查：零外链 / 端点对得上契约 / JS 可解析 ----------
echo "-- 静态检查（零外链、端点、JS 语法）"
STATIC_RC=0
"$PY" - "$ROOT" <<'PYEOF' || STATIC_RC=$?
import pathlib
import re
import subprocess
import sys
import tempfile

root = pathlib.Path(sys.argv[1])
web = root / "web"
fails = []


def check(cond, msg):
    if cond:
        print("  PASS  " + msg)
    else:
        print("  FAIL  " + msg)
        fails.append(msg)


# 契约里的路径集合：页面调用的端点必须都在里面，防止前端自创接口
contract = (root / "contracts" / "openapi.yaml").read_text(encoding="utf-8")
paths = set(re.findall(r"^  (/\S+):", contract, re.MULTILINE))
check(bool(paths), "读到契约路径 %d 条" % len(paths))

ENDPOINT_RE = re.compile(r"(?:fetch|api)\(\s*(?:[A-Za-z_$][\w$]*\s*\+\s*)?\"([^\"]+)\"")
SCRIPT_RE = re.compile(r"<script[^>]*>(.*?)</script>", re.S)

expect = {
    "index.html": ["/auth/token"],
    "dashboard.html": ["/heartbeat", "/tasks"],
}

for name, wanted in expect.items():
    path = web / name
    check(path.is_file(), "%s 存在" % name)
    if not path.is_file():
        continue
    html = path.read_text(encoding="utf-8")

    # --- 零外链：内网可离线跑，任何 CDN/远程字体/远程脚本都不允许 ---
    check("cdn" not in html.lower(), "%s 无 cdn 字样" % name)
    check(not re.search(r"https?://", html), "%s 无 http(s) 绝对地址" % name)
    check("<script src" not in html and "<link" not in html, "%s 无外部 script/link 引用" % name)
    check(not re.search(r"@import", html), "%s 无 @import" % name)

    # --- 中文 + 编码 ---
    check('lang="zh-CN"' in html and 'charset="utf-8"' in html, "%s 声明 zh-CN / utf-8" % name)

    # --- 无 console.log 残留（排错用 console.error） ---
    check(not re.search(r"console\.log", html), "%s 无 console.log" % name)

    # --- 不写 innerHTML：任何来源的字符串都不该进 HTML 解析器 ---
    check(not re.search(r"\.innerHTML\s*=", html), "%s 无 innerHTML 赋值" % name)

    # --- 内联 CSS 变量主题 ---
    for token in ("--bg", "--primary", "--gold"):
        check(token in html, "%s 定义 CSS 变量 %s" % (name, token))

    # --- 端点：非空、且都在契约里 ---
    calls = ENDPOINT_RE.findall(html)
    check(len(calls) > 0, "%s 提取到 %d 个 fetch 端点" % (name, len(calls)))
    for call in calls:
        clean = call.split("?")[0]
        check(clean in paths, "%s 端点 %s 在契约中" % (name, clean))
    for w in wanted:
        check(any(c.split("?")[0] == w for c in calls), "%s 调用了 %s" % (name, w))

    # --- JS 语法：抽出来交给 node --check，浏览器怎么解析它就怎么解析 ---
    scripts = SCRIPT_RE.findall(html)
    check(len(scripts) >= 1, "%s 内联脚本段 %d 个" % (name, len(scripts)))
    if scripts:
        with tempfile.NamedTemporaryFile("w", suffix=".js", delete=False, encoding="utf-8") as fh:
            fh.write("\n".join(scripts))
            js_path = fh.name
        try:
            probe = subprocess.run(["node", "--check", js_path], capture_output=True, text=True)
            if probe.returncode == 0:
                check(True, "%s JS 语法通过 node --check" % name)
            else:
                check(False, "%s JS 语法错误: %s" % (name, probe.stderr.strip().splitlines()[:3]))
        except FileNotFoundError:
            print("  SKIP  node 不可用，跳过 %s 的语法检查" % name)
        finally:
            pathlib.Path(js_path).unlink(missing_ok=True)

# --- 页面职责：登录页存 token 并跳看板；看板页 401 回登录、10 秒刷新 ---
index = (web / "index.html").read_text(encoding="utf-8")
dash = (web / "dashboard.html").read_text(encoding="utf-8")

check("tap.apiKey" in index and "localStorage.setItem" in index, "index 把 Key 存进 localStorage")
check('location.replace("dashboard.html")' in index, "index 登录成功跳 dashboard.html")
check("#0D1A2A" in index and "#00B4D8" in index and "#FFB703" in index, "index 用对齐 PPT 的三色（深蓝/青/金）")

check("REFRESH_MS = 10000" in dash and "setInterval" in dash, "dashboard 10 秒自动刷新")
check('location.replace("index.html"' in dash, "dashboard 401 跳回登录页")
check("Bearer " in dash, "dashboard 请求带 Bearer token")
check("405" in dash, "dashboard 对 GET /tasks 的 405 有降级提示（兼容旧版服务端）")
check("console.error" in dash and "console.error" in index, "两页均用 console.error 报错")

sys.exit(1 if fails else 0)
PYEOF

if [ "$STATIC_RC" -eq 0 ]; then
  ok "静态检查全过"
else
  bad "静态检查有失败项"
fi

# ---------- 4. 服务端行为：登录能换 token，坏凭证必须 401 ----------
echo "-- 鉴权契约（页面依赖的行为）"

code="$(curl -s -o "$BODY" -w '%{http_code}' -X POST "$BASE/v1/auth/token" \
  -H 'Content-Type: application/json' \
  -d "{\"api_key\":\"$API_KEY\",\"device_fingerprint\":\"w4-smoke-web-001\",\"os\":\"smoke\"}")"
TOKEN="$("$PY" -c 'import json,sys;print(json.load(open(sys.argv[1])).get("token",""))' "$BODY" 2>/dev/null)"
if [ "$code" = "200" ] && [ -n "$TOKEN" ]; then
  ok "POST /auth/token 正确 Key → 200 且返回 token"
else
  bad "POST /auth/token 正确 Key → ${code}（token 空则为空）"
fi

code="$(curl -s -o "$BODY" -w '%{http_code}' -X POST "$BASE/v1/auth/token" \
  -H 'Content-Type: application/json' \
  -d '{"api_key":"definitely-wrong","device_fingerprint":"w4-smoke-web-001"}')"
[ "$code" = "401" ] && ok "POST /auth/token 错误 Key → 401" || bad "POST /auth/token 错误 Key → ${code}（期望 401）"

# 坏 token 必须 401：看板页靠这个跳回登录页
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/heartbeat" \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer bad-token' -d '{}')"
[ "$code" = "401" ] && ok "POST /heartbeat 坏 token → 401" || bad "POST /heartbeat 坏 token → ${code}（期望 401）"

# 看板靠 GET /tasks 拉列表：坏 token 必须 401，页面据此清 token 跳回登录页
code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/tasks" -H 'Authorization: Bearer bad')"
[ "$code" = "401" ] && ok "GET /tasks 坏 token → 401（看板据此回登录页）" || bad "GET /tasks 坏 token → ${code}（期望 401）"

# 看板第二个依赖：带好 token 的心跳（状态卡一级探针）
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/heartbeat" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" -d '{"version":"web-1.0"}')"
[ "$code" = "200" ] && ok "POST /heartbeat 好 token → 200（状态卡可用）" || bad "POST /heartbeat 好 token → $code"

# ---------- 5. 看板数据通路：建一个任务，列表里必须能拿到并渲染 ----------
echo "-- 看板数据通路 GET /tasks"
CREATE_CODE="$(curl -s -o "$BODY" -w '%{http_code}' -X POST "$BASE/v1/tasks" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" \
  -d '{"request_id":"w4-smoke-task-001","title":"W4 冒烟任务","type":"local","source":"web"}')"
NEW_ID="$("$PY" -c 'import json,sys;print(json.load(open(sys.argv[1])).get("id",""))' "$BODY" 2>/dev/null)"
if [ "$CREATE_CODE" = "201" ] && [ -n "$NEW_ID" ]; then
  ok "POST /tasks → 201，任务 ${NEW_ID}"
else
  bad "POST /tasks → ${CREATE_CODE}（建不出任务，看板无从验证）"
fi

code="$(curl -s -o "$BODY" -w '%{http_code}' "$BASE/v1/tasks?limit=50" \
  -H "Authorization: Bearer $TOKEN")"
if [ "$code" != "200" ]; then
  case "$code" in
    405|404) warn "GET /tasks → ${code}：契约已定义但服务端未实现，看板显示降级态（web/ 改不了）" ;;
    401)     bad "GET /tasks → 401（好 token 不该被拒）" ;;
    *)       bad "GET /tasks → ${code}（意外状态码）" ;;
  esac
else
  # 校验的正是看板要渲染的字段：字段名对不上，页面就是一片「—」
  LIST_RC=0
  "$PY" - "$BODY" "$NEW_ID" <<'PYEOF' || LIST_RC=$?
import json, sys
body = json.load(open(sys.argv[1]))
new_id = sys.argv[2]
items = body.get("items")
assert isinstance(items, list), "响应缺 items 数组: %r" % list(body)[:6]
print("  PASS  GET /tasks → 200，items %d 条" % len(items))
if new_id:
    hit = [t for t in items if t.get("task_id") == new_id]
    assert hit, "列表里找不到刚建的任务 %s" % new_id
    print("  PASS  刚建的任务出现在列表首屏（看板能拿到真实数据）")
    need = {"task_id", "title", "type", "status", "created_at"}
    miss = sorted(need - set(hit[0]))
    assert not miss, "任务卡片缺字段 %s（看板列会显示占位符）" % miss
    print("  PASS  卡片字段齐全 %s" % ",".join(sorted(need)))
PYEOF
  if [ "$LIST_RC" -eq 0 ]; then
    ok "任务列表可用（看板可渲染真实任务）"
  else
    bad "任务列表结构不符合看板预期"
  fi
fi

# 无 token 访问：必须 401（鉴权），不能是 200 越权
code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/tasks")"
if [ "$code" = "200" ]; then
  bad "GET /tasks 无 token 竟然 → 200（越权）"
else
  ok "GET /tasks 无 token → ${code}（非 200，不越权）"
fi

# ---------- 6. CORS：跨域部署时浏览器能否直连 ----------
echo "-- CORS（静态站与 API 不同端口时必需）"
acao="$(curl -s -D - -o /dev/null -X OPTIONS "$BASE/v1/auth/token" \
  -H 'Origin: http://127.0.0.1:8128' \
  -H 'Access-Control-Request-Method: POST' | tr -d '\r' | awk 'tolower($1)=="access-control-allow-origin:"{print $2}')"
if [ -n "$acao" ]; then
  ok "OPTIONS /auth/token 回 Access-Control-Allow-Origin: $acao"
else
  warn "服务端无 CORS 头：浏览器直连跨域会被拦（web/ 改不了，需 server/ 加 CORSMiddleware 或同源托管 web/）"
fi

# ---------- 汇总 ----------
echo
echo "== 结果：PASS=$PASS FAIL=$FAIL WARN=$WARN =="
if [ "$FAIL" -ne 0 ]; then
  echo "W4 Web 冒烟未通过"
  exit 1
fi
echo "W4 Web 冒烟通过"
exit 0
