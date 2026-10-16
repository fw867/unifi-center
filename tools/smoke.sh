#!/usr/bin/env bash
# 端到端冒烟：把构建好的 .fpk 解开，按 fnOS 安装后的布局真跑一遍静态二进制。
#
# 这是**构建产物**的验证，不是在开发机上跑源码：
#   1. 解开 .fpk（manifest / ICON / cmd / config / wizard / app.tgz）
#   2. 把 app.tgz 摊平成 target/（app/bin → target/bin、app/ui → target/ui）
#   3. 起一个假路由器（用 python3 写几个响应）
#   4. 用 target/bin/unifi-center 起服务，监听 target/app.sock（真实 Unix Socket）
#   5. 通过 socket 打接口，检查：软件中心页面本地化与令牌注入、/api 转发带令牌、
#      管理员能看明文/能改设置、普通用户只能读、非管理员改设置被 403、路径穿越被拒
#
# 用法（Linux / WSL / 设备上）：
#     bash tools/smoke.sh dist/unificenter-1.0.0-x86.fpk
#
# 依赖：python3、curl（仅用于本测试，产品本身不依赖它们）
set -euo pipefail

FPK="${1:?用法: bash tools/smoke.sh <某个架构的 .fpk>}"
FPK="$(cd "$(dirname "$FPK")" && pwd)/$(basename "$FPK")"
[ -f "$FPK" ] || { echo "找不到 $FPK" >&2; exit 1; }

WORK="$(mktemp -d)"
ROUTER_PID=""
SERVICE_PID=""
cleanup() {
  [ -n "$SERVICE_PID" ] && kill "$SERVICE_PID" 2>/dev/null || true
  [ -n "$ROUTER_PID" ] && kill "$ROUTER_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

PASS=0
FAIL=0
check() {
  local label="$1" condition="$2" detail="${3:-}"
  if [ "$condition" = "1" ]; then
    printf '  [OK  ] %s\n' "$label"
    PASS=$((PASS + 1))
  else
    printf '  [FAIL] %s %s\n' "$label" "$detail"
    FAIL=$((FAIL + 1))
  fi
}

echo "解包 $(basename "$FPK") → $WORK"
mkdir -p "$WORK/fpk" "$WORK/target" "$WORK/etc" "$WORK/var" "$WORK/tmp"
tar -xzf "$FPK" -C "$WORK/fpk"

echo "检查包结构"
grep -q '^appname=unificenter' "$WORK/fpk/manifest" \
  && check "manifest 里 appname 正确且无 BOM" 1 || check "manifest 里 appname 正确且无 BOM" 0
! grep -q '^install_dep_apps=' "$WORK/fpk/manifest" \
  && check "没有声明任何运行时依赖（自包含）" 1 || check "没有声明任何运行时依赖（自包含）" 0
grep -q '^platform=' "$WORK/fpk/manifest" \
  && check "manifest 声明了 platform" 1 || check "manifest 声明了 platform" 0
for name in main install_init install_callback upgrade_init upgrade_callback \
            uninstall_init uninstall_callback config_init config_callback; do
  [ -f "$WORK/fpk/cmd/$name" ] || check "缺少 cmd/$name" 0
done
check "生命周期脚本齐全" 1
for name in install upgrade uninstall config; do
  python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$WORK/fpk/wizard/$name"
done
check "向导 4 个文件都是合法 JSON" 1

# 摊平 app/：app.tgz 顶层是 app/，安装时其内容落到 target/
tar -xzf "$WORK/fpk/app.tgz" -C "$WORK/target"
if [ -d "$WORK/target/app" ]; then
  for item in "$WORK/target/app"/*; do mv "$item" "$WORK/target/"; done
  rmdir "$WORK/target/app"
fi

[ -x "$WORK/target/bin/unifi-center" ] \
  && check "静态二进制在 target/bin/unifi-center 且可执行" 1 \
  || check "静态二进制在 target/bin/unifi-center 且可执行" 0
[ -x "$WORK/fpk/cmd/main" ] \
  && check "cmd/main 在包里带可执行位" 1 || check "cmd/main 在包里带可执行位" 0
grep -q '"gatewaySocket": "app.sock"' "$WORK/target/ui/config" \
  && check "入口配置 gatewaySocket=app.sock" 1 || check "入口配置 gatewaySocket=app.sock" 0
grep -q '"gatewayPrefix": "/app/unificenter"' "$WORK/target/ui/config" \
  && check "入口配置 gatewayPrefix=/app/unificenter" 1 || check "入口配置 gatewayPrefix=/app/unificenter" 0

echo "二进制信息"
BIN_SIZE="$(du -h "$WORK/target/bin/unifi-center" | awk '{print $1}')"
if command -v file >/dev/null 2>&1; then
  file "$WORK/target/bin/unifi-center" | sed 's/^/  /'
else
  echo "  size = $BIN_SIZE"
fi
if command -v ldd >/dev/null 2>&1; then
  LDD_OUT="$(ldd "$WORK/target/bin/unifi-center" 2>&1 || true)"
  case "$LDD_OUT" in
    *"not a dynamic executable"*|*"statically linked"*)
      check "二进制是静态链接（无动态库依赖）" 1 ;;
    *) check "二进制是静态链接（无动态库依赖）" 0 "$LDD_OUT" ;;
  esac
fi

echo "自检子命令"
if TRIM_APPDEST="$WORK/target" TRIM_PKGETC="$WORK/etc" TRIM_PKGVAR="$WORK/var" \
   TRIM_APPNAME=unificenter \
   "$WORK/target/bin/unifi-center" selftest >"$WORK/selftest.json" 2>&1; then
  check "selftest 通过" 1
else
  check "selftest 通过" 0 "$(cat "$WORK/selftest.json")"
fi

echo "启动假路由器"
PORT_FILE="$WORK/port"
python3 - "$PORT_FILE" >"$WORK/router.log" 2>&1 <<'PY' &
import http.server, json, socketserver, sys

TOKEN = "smoke-token-123456"
PAGE = """<!doctype html><html><head><title>UniFi SoftCenter</title>
<script src="https://cdn.jsdelivr.net/npm/vue@3.4.21/dist/vue.global.prod.js"></script>
</head><body>soft center
<script>fetch('https://api.github.com/repos/x/y/releases');</script>
</body></html>"""

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *args): pass
    def _json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        if self.path.startswith("/api/system/info"):
            if self.headers.get("Authorization") != TOKEN:
                self._json(401, {"error": "令牌不正确"})
            else:
                self._json(200, {"Device": "R619AC", "Version": "3.0.1"})
            return
        if self.path.startswith("/repos/"):
            self._json(200, {"proxied": True, "path": self.path})
            return
        body = PAGE.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

class Server(socketserver.ThreadingMixIn, socketserver.TCPServer):
    daemon_threads = True
    allow_reuse_address = True

with Server(("127.0.0.1", 0), Handler) as httpd:
    with open(sys.argv[1], "w") as handle:
        handle.write(str(httpd.server_address[1]))
    httpd.serve_forever()
PY
ROUTER_PID=$!
for _ in $(seq 1 50); do [ -s "$PORT_FILE" ] && break; sleep 0.1; done
ROUTER_PORT="$(cat "$PORT_FILE")"
check "假路由器已启动（:$ROUTER_PORT）" "$([ -n "$ROUTER_PORT" ] && echo 1 || echo 0)"

echo "启动服务（真实 Unix Socket）"
SOCKET="$WORK/target/app.sock"
# GITHUB_API_BASE 指向假路由器：让 GitHub 转发这条链路可离线、可确定地验证
TRIM_APPDEST="$WORK/target" \
TRIM_PKGETC="$WORK/etc" \
TRIM_PKGVAR="$WORK/var" \
TRIM_PKGTMP="$WORK/tmp" \
TRIM_APPNAME=unificenter \
ROUTER_TARGET="http://127.0.0.1:$ROUTER_PORT/" \
GITHUB_API_BASE="http://127.0.0.1:$ROUTER_PORT" \
"$WORK/target/bin/unifi-center" serve >"$WORK/service.log" 2>&1 &
SERVICE_PID=$!
for _ in $(seq 1 50); do [ -S "$SOCKET" ] && break; sleep 0.1; done
[ -S "$SOCKET" ] && check "已监听 $SOCKET" 1 || check "已监听 $SOCKET" 0 "$(cat "$WORK/service.log")"

# 通过 Unix Socket 打请求（这是网关实际使用的路径）
sock() { # sock <path> [curl args...]
  local path="$1"; shift
  curl -sS --unix-socket "$SOCKET" -o "$WORK/body" -w '%{http_code}' "http://localhost$path" "$@" 2>/dev/null || echo 000
}
ADMIN=(-H 'X-Trim-Userid: 1000' -H 'X-Trim-Username: admin' -H 'X-Trim-Isadmin: true')
USER=(-H 'X-Trim-Userid: 1001' -H 'X-Trim-Username: bob' -H 'X-Trim-Isadmin: false')

echo "打接口（经 Unix Socket）"
CODE="$(sock /healthz)"
check "/healthz = 200" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
grep -q '"ok":true' "$WORK/body" && check "/healthz 返回 ok" 1 || check "/healthz 返回 ok" 0

CODE="$(sock /ctl/status "${ADMIN[@]}")"
check "/ctl/status = 200" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
grep -q '"reachable":true' "$WORK/body" && check "探测到路由器可达" 1 || check "探测到路由器可达" 0 "$(head -c 200 "$WORK/body")"

echo "设置权限"
CODE="$(sock /ctl/settings "${ADMIN[@]}")"
check "管理员读设置 = 200" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
grep -q '"can_edit":true' "$WORK/body" && check "管理员 can_edit=true" 1 || check "管理员 can_edit=true" 0 "$(head -c 200 "$WORK/body")"

CODE="$(sock /ctl/settings "${USER[@]}")"
check "普通用户读设置 = 200（只读）" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
grep -q '"can_edit":false' "$WORK/body" && check "普通用户 can_edit=false" 1 || check "普通用户 can_edit=false" 0
grep -q 'smoke-token' "$WORK/body" && check "普通用户看不到明文令牌" 0 || check "普通用户看不到明文令牌" 1

CODE="$(sock /ctl/settings "${USER[@]}" -X POST -H 'Content-Type: application/json' \
        -d "{\"target\":\"http://127.0.0.1:$ROUTER_PORT/\"}")"
check "普通用户改设置被 403" "$([ "$CODE" = "403" ] && echo 1 || echo 0)" "$CODE"

CODE="$(sock /ctl/settings "${ADMIN[@]}" -X POST -H 'Content-Type: application/json' \
        -d "{\"target\":\"http://127.0.0.1:$ROUTER_PORT/\",\"token\":\"smoke-token-123456\"}")"
check "管理员改设置 = 200" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"

CODE="$(sock /ctl/token "${ADMIN[@]}")"
grep -q 'smoke-token-123456' "$WORK/body" && check "管理员能取到明文令牌" 1 || check "管理员能取到明文令牌" 0
CODE="$(sock /ctl/token "${USER[@]}")"
check "普通用户取明文令牌被 403" "$([ "$CODE" = "403" ] && echo 1 || echo 0)" "$CODE"

echo "软件中心页面与转发"
CODE="$(sock / "${ADMIN[@]}")"
check "页面前缀 = 200" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
grep -q '/app/unificenter/assets/vue.js' "$WORK/body" \
  && check "CDN 已换成本地副本" 1 || check "CDN 已换成本地副本" 0
grep -q 'localStorage.setItem("sc_token","smoke-token-123456")' "$WORK/body" \
  && check "已注入 AdminToken" 1 || check "已注入 AdminToken" 0
grep -q 'api.github.com' "$WORK/body" \
  && check "GitHub 直连已改走 NAS" 0 || check "GitHub 直连已走 NAS" 1

CODE="$(sock /api/system/info)"
check "/api 转发 = 200（用保存的令牌）" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
grep -q 'R619AC' "$WORK/body" && check "转发拿到了路由器设备信息" 1 || check "转发拿到了路由器设备信息" 0

CODE="$(sock /github/api/repos/x/y/releases)"
check "GitHub 转发 = 200" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
# 假路由器的 JSON 带空格（Python json.dumps），匹配时不要写死冒号后的空格
grep -q '"proxied": *true' "$WORK/body" \
  && check "GitHub 转发到达上游" 1 \
  || check "GitHub 转发到达上游" 0 "响应体：$(head -c 200 "$WORK/body")"

echo "静态资源与安全"
CODE="$(sock /ctl/panel.html)"
check "设置面板可打开" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
CODE="$(sock /assets/vue.js)"
check "本地 Vue 可取出" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
CODE="$(sock /assets/../../manifest)"
check "路径穿越被拒绝" "$([ "$CODE" != "200" ] && echo 1 || echo 0)" "$CODE"
CODE="$(sock /ctl/nope)"
check "未知接口 404" "$([ "$CODE" = "404" ] && echo 1 || echo 0)" "$CODE"

echo "生命周期脚本"
export TRIM_APPNAME=unificenter TRIM_APPDEST="$WORK/target" TRIM_PKGETC="$WORK/etc" \
       TRIM_PKGVAR="$WORK/var" TRIM_PKGTMP="$WORK/tmp" TRIM_TEMP_LOGFILE="$WORK/tmp/err.log" \
       ROUTER_TARGET="http://127.0.0.1:$ROUTER_PORT/" GITHUB_API_BASE="http://127.0.0.1:$ROUTER_PORT"

# 先停掉手工起的进程，改由 cmd/main 管理（它要自己写 PID 文件、等 Socket 就绪）
kill "$SERVICE_PID" 2>/dev/null || true
SERVICE_PID=""
for _ in $(seq 1 30); do [ -S "$SOCKET" ] || break; sleep 0.1; done

"$WORK/fpk/cmd/main" start >/dev/null 2>&1 \
  && check "cmd/main start" 1 || check "cmd/main start" 0 "$(cat "$WORK/service.log")"
"$WORK/fpk/cmd/main" status && STATUS_RUN=0 || STATUS_RUN=$?
check "cmd/main status = 0（运行中）" "$([ "$STATUS_RUN" = "0" ] && echo 1 || echo 0)" "exit=$STATUS_RUN"
[ -S "$SOCKET" ] && check "cmd/main start 后 Socket 就绪" 1 || check "cmd/main start 后 Socket 就绪" 0

CODE="$(sock /healthz)"
check "由 cmd/main 拉起的服务可访问" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"

"$WORK/fpk/cmd/main" stop >/dev/null 2>&1
"$WORK/fpk/cmd/main" status >/dev/null 2>&1 && STATUS_STOP=0 || STATUS_STOP=$?
check "cmd/main stop 后 status = 3（未运行）" "$([ "$STATUS_STOP" = "3" ] && echo 1 || echo 0)" "exit=$STATUS_STOP"
[ -S "$SOCKET" ] && check "stop 后 Socket 已清理" 0 || check "stop 后 Socket 已清理" 1

"$WORK/fpk/cmd/main" start >/dev/null 2>&1 && check "再次 start 幂等可用" 1 || check "再次 start 幂等可用" 0
for _ in $(seq 1 50); do [ -S "$SOCKET" ] && break; sleep 0.1; done
[ -S "$SOCKET" ] && check "重启后 Socket 又在了" 1 || check "重启后 Socket 又在了" 0
"$WORK/fpk/cmd/main" stop >/dev/null 2>&1 || true

echo "第二种布局：TRIM_APPDEST 直接就是应用目录（实测踩到的布局）"
# 有设备把 TRIM_APPDEST 指到应用根目录而不是它的 target 子目录，
# 于是二进制在 APP/bin/unifi-center、Socket 在 APP/app.sock。
ROOT2="$WORK/root-layout"
rm -rf "$ROOT2"
mkdir -p "$ROOT2/bin" "$ROOT2/ui"
cp "$WORK/target/bin/unifi-center" "$ROOT2/bin/unifi-center"
cp -R "$WORK/target/ui/." "$ROOT2/ui/"

run2() { # run2 <cmd/main 参数...>
  TRIM_APPNAME=unificenter TRIM_APPDEST="$ROOT2" TRIM_PKGETC="$WORK/etc" \
  TRIM_PKGVAR="$WORK/var" TRIM_PKGTMP="$WORK/tmp" TRIM_TEMP_LOGFILE="$WORK/tmp/err2.log" \
  ROUTER_TARGET="http://127.0.0.1:$ROUTER_PORT/" GITHUB_API_BASE="http://127.0.0.1:$ROUTER_PORT" \
  "$WORK/fpk/cmd/main" "$@"
}

if run2 doctor >"$WORK/doctor.txt" 2>&1; then
  check "cmd/main doctor 可运行" 1
else
  check "cmd/main doctor 可运行" 0 "$(head -c 200 "$WORK/doctor.txt")"
fi
grep -q "解析出的应用目录：$ROOT2$" "$WORK/doctor.txt" \
  && check "doctor 解析出应用目录（根目录布局）" 1 \
  || check "doctor 解析出应用目录（根目录布局）" 0 "$(grep '解析出的应用目录' "$WORK/doctor.txt")"

SOCKET2="$ROOT2/app.sock"
if run2 start >"$WORK/start2.log" 2>&1; then
  check "根目录布局下 cmd/main start" 1
else
  check "根目录布局下 cmd/main start" 0 "$(cat "$WORK/start2.log" "$WORK/tmp/err2.log" 2>/dev/null | head -c 300)"
fi
for _ in $(seq 1 50); do [ -S "$SOCKET2" ] && break; sleep 0.1; done
[ -S "$SOCKET2" ] && check 'Socket 建在 $TRIM_APPDEST/app.sock' 1 \
  || check 'Socket 建在 $TRIM_APPDEST/app.sock' 0 "见 $WORK/var/unifi-center.out"
CODE="$(curl -sS --unix-socket "$SOCKET2" -o "$WORK/body2" -w '%{http_code}' http://localhost/healthz 2>/dev/null || echo 000)"
check "根目录布局下 /healthz = 200" "$([ "$CODE" = "200" ] && echo 1 || echo 0)" "$CODE"
run2 stop >/dev/null 2>&1 || true

printf '\n通过 %d 项，失败 %d 项\n' "$PASS" "$FAIL"
[ "$FAIL" = "0" ] || { echo '冒烟失败'; exit 1; }
echo '全部冒烟检查通过 ✓'
