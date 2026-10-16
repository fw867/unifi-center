#!/usr/bin/env bash
# 路由器软件中心（fnOS 移植版）· 构建 .fpk（静态二进制，零运行时依赖）
#
# 产出两个架构的安装包（默认）：
#     dist/unificenter-<版本>-x86.fpk    platform=x86   linux/amd64
#     dist/unificenter-<版本>-arm.fpk    platform=arm   linux/arm64
#
# 包结构（与官方 fnpack 产物一致）：
#     manifest + ICON.PNG + ICON_256.PNG + app.tgz + cmd/ + config/ + wizard/
# 其中 app.tgz 是 app/ 目录的 tar.gz（本脚本会先把对应架构的二进制放进 app/bin/），
# manifest 里补一行 checksum = <app.tgz 的 md5>。
#
# 用法：
#     bash deploy/build.sh                  # 两个架构都出
#     bash deploy/build.sh x86              # 只出 x86
#     bash deploy/build.sh arm /tmp/out     # 只出 arm，输出到指定目录
#
# 依赖：go（交叉编译）、python3（打包由 tools/pack_fpk.py 完成 —— 用 Python 标准库
# 精确控制包内权限，避免 Windows bsdtar 把 bin/unifi-center、cmd/* 记成 0666）
set -euo pipefail

ROOT="$(cd "${2:-$(dirname "$0")/..}" && pwd)"
ARCHES="${1:-x86,arm}"
OUT_DIR="${3:-$ROOT/dist}"

say() { printf '[build] %s\n' "$*"; }
fail() { printf '[build] 错误：%s\n' "$*" >&2; exit 1; }

[ -f "$ROOT/manifest" ] || fail '缺少 manifest'
command -v go >/dev/null 2>&1 || fail '找不到 go（需要用它交叉编译静态二进制）'
command -v python3 >/dev/null 2>&1 || fail '找不到 python3（打包脚本 tools/pack_fpk.py 需要）'
[ -f "$ROOT/tools/pack_fpk.py" ] || fail '缺少 tools/pack_fpk.py'

APPNAME="$(sed -n 's/^appname[[:space:]]*=[[:space:]]*//p' "$ROOT/manifest" | head -n1 | tr -d '\r')"
VERSION="$(sed -n 's/^version[[:space:]]*=[[:space:]]*//p' "$ROOT/manifest" | head -n1 | tr -d '\r')"
[ -n "$APPNAME" ] || fail 'manifest 里没有 appname'
[ -n "$VERSION" ] || fail 'manifest 里没有 version'

# --- 预检 -------------------------------------------------------------------
for item in config/privilege config/resource cmd/main ICON.PNG ICON_256.PNG \
            app/ui/config app/ui/panel.html app/ui/panel.js app/ui/panel.css \
            app/ui/assets/vue.js app/ui/assets/lucide.js app/ui/assets/tailwind.js \
            wizard/install wizard/upgrade wizard/uninstall wizard/config service/go.mod \
            tools/make_icon.py tools/ui-apple-touch-icon.png; do
  [ -e "$ROOT/$item" ] || fail "缺少 $item"
done

# 图标：每次构建都从官方图源重新生成，避免包里留着过期图标
say '从 tools/ui-apple-touch-icon.png 重新生成图标'
python3 "$ROOT/tools/make_icon.py" "$ROOT" || fail '图标生成失败'

for json in config/privilege config/resource app/ui/config wizard/install wizard/upgrade wizard/uninstall wizard/config; do
  python3 -c 'import json,sys; json.load(open(sys.argv[1], encoding="utf-8"))' "$ROOT/$json" \
    || fail "$json 不是合法 JSON"
done

chmod 755 "$ROOT"/cmd/* 2>/dev/null || true
find "$ROOT" -name '.DS_Store' -delete 2>/dev/null || true
rm -rf "$ROOT/.dev"

mkdir -p "$OUT_DIR"

# --- 逐架构构建 -------------------------------------------------------------
build_arch() {
  local arch="$1" goarch
  case "$arch" in
    x86) goarch=amd64 ;;
    arm) goarch=arm64 ;;
    *) fail "不支持的架构 $arch（可用：x86、arm）" ;;
  esac

  say "编译 linux/$goarch（CGO 关闭，静态链接）"
  rm -f "$ROOT/app/bin/unifi-center"
  ( cd "$ROOT/service" && \
    CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
    go build -trimpath -ldflags '-s -w' -o "$ROOT/app/bin/unifi-center" . )
  say "二进制大小：$(du -h "$ROOT/app/bin/unifi-center" | awk '{print $1}')"

  say "打包 .fpk"
  python3 "$ROOT/tools/pack_fpk.py" \
    --root "$ROOT" \
    --out "$OUT_DIR/$APPNAME-$VERSION-$arch.fpk" \
    --arch "$arch"

  say "完成：$OUT_DIR/$APPNAME-$VERSION-$arch.fpk"
}

IFS=',' read -r -a wanted <<< "$ARCHES"
for arch in "${wanted[@]}"; do
  [ -n "$arch" ] && build_arch "$arch"
done

say '安装：appcenter-cli install-fpk <对应架构的 .fpk>（或在应用中心里手动安装）'
say '注意：x86 与 arm 的包只能装到对应架构的设备上。'
