#!/usr/bin/env python3
"""冒烟验证：把构建好的 .fpk 解开来，按 fnOS 安装后的布局真跑一遍服务。

模拟的安装布局（与官方文档一致）：
    <tmp>/target/          ← app.tgz 解开 AppDEST（$TRIM_APPDEST）
    <tmp>/etc/             ← $TRIM_PKGETC
    <tmp>/var/             ← $TRIM_PKGVAR
    <tmp>/fpk/             ← .fpk 解开的包根（manifest / ICON / cmd / config / wizard）

验证内容：
  · 包结构（manifest 字段、网关入口、图标、生命周期脚本、向导）；
  · 从 target 目录启动服务（Windows 上用 TCP 调试端口），Socket 路径按 TRIM_APPDEST 计算；
  · /healthz、/ctl/status、页面前缀的本地化与令牌注入、/api 转发带上令牌；
  · /ctl/settings 对管理员给明文、对普通用户只给提示。

用法：python tools/smoke.py [dist/unificenter.fpk]
"""

from __future__ import annotations

import http.server
import io
import json
import os
import socket
import socketserver
import struct
import sys
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
TOKEN = 'smoke-token-123456'

PAGE = """<!doctype html><html><head><title>UniFi SoftCenter</title>
<script src="https://cdn.jsdelivr.net/npm/vue@3.4.21/dist/vue.global.prod.js"></script>
</head><body>soft center
<script>fetch('https://api.github.com/repos/x/y/releases');</script>
</body></html>"""


class FakeRouter(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    token = TOKEN
    seen: list[str] = []

    def log_message(self, fmt, *args):                                     # noqa: A003
        return

    def do_GET(self) -> None:                                              # noqa: N802
        FakeRouter.seen.append(self.headers.get('Authorization') or '')
        if self.path.startswith('/api/system/info'):
            if (self.headers.get('Authorization') or '') != self.token:
                self._json(401, {'error': '令牌不正确'})
                return
            self._json(200, {'Device': 'R619AC', 'Version': '3.0.1'})
            return
        body = PAGE.encode('utf-8')
        self.send_response(200)
        self.send_header('Content-Type', 'text/html; charset=utf-8')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _json(self, status: int, payload: object) -> None:
        body = json.dumps(payload).encode('utf-8')
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class FakeRouterServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    daemon_threads = True
    allow_reuse_address = True


def free_port() -> int:
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 0))
        return probe.getsockname()[1]


def request(url: str, headers: dict[str, str] | None = None) -> tuple[int, bytes]:
    req = urllib.request.Request(url)
    for name, value in (headers or {}).items():
        req.add_header(name, value)
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def check(label: str, condition: bool, detail: str = '') -> None:
    mark = 'OK  ' if condition else 'FAIL'
    print(f'  [{mark}] {label}' + (f' —— {detail}' if detail and not condition else ''))
    if not condition:
        raise SystemExit(f'冒烟失败：{label} {detail}')


def main(argv: list[str]) -> int:
    fpk = Path(argv[1]) if len(argv) > 1 else ROOT / 'dist' / 'unificenter.fpk'
    if not fpk.is_file():
        raise SystemExit(f'找不到 {fpk}，先跑 deploy/build.ps1 或 deploy/build.sh')

    work = Path(tempfile.mkdtemp(prefix='unifi-center-smoke-'))
    target = work / 'target'
    pkgetc = work / 'etc'
    pkgvar = work / 'var'
    fpk_dir = work / 'fpk'
    for path in (target, pkgetc, pkgvar, fpk_dir):
        path.mkdir(parents=True, exist_ok=True)

    print(f'解包 {fpk.name} → {work}')
    with tarfile.open(fpk, 'r:gz') as archive:
        archive.extractall(fpk_dir)
        app_tgz = fpk_dir / 'app.tgz'
        with tarfile.open(app_tgz, 'r:gz') as inner:
            inner.extractall(target)
    # .fpk 里的 app.tgz 顶层是 app/ 目录，安装时它的**内容**落到 target/，
    # 这里按同样语义摊平（app/ui → target/ui、app/server → target/server）
    nested = target / 'app'
    if nested.is_dir():
        for item in nested.iterdir():
            item.rename(target / item.name)
        nested.rmdir()

    print('检查包结构')
    manifest = (fpk_dir / 'manifest').read_text(encoding='utf-8')
    fields = dict(line.split('=', 1) for line in manifest.strip().splitlines() if '=' in line)
    check('manifest 无 BOM', not manifest.startswith('\ufeff'))
    check('appname=unificenter', fields.get('appname') == 'unificenter', fields.get('appname', ''))
    check('install_dep_apps=python312', fields.get('install_dep_apps') == 'python312')
    entry = json.loads((target / 'ui' / 'config').read_text(encoding='utf-8'))['.url']['unificenter.main']
    check('网关入口已注册', entry['gatewayPrefix'] == '/app/unificenter'
          and entry['gatewaySocket'] == 'app.sock', json.dumps(entry, ensure_ascii=False))
    for name in ('main', 'install_init', 'install_callback', 'upgrade_init', 'upgrade_callback',
                 'uninstall_init', 'uninstall_callback', 'config_init', 'config_callback'):
        check(f'cmd/{name} 存在', (fpk_dir / 'cmd' / name).is_file())
    for name in ('install', 'upgrade', 'uninstall', 'config'):
        json.loads((fpk_dir / 'wizard' / name).read_text(encoding='utf-8'))
    check('向导 4 个文件都是合法 JSON', True)
    for icon, size in ((fpk_dir / 'ICON.PNG', 64), (fpk_dir / 'ICON_256.PNG', 256)):
        data = icon.read_bytes()
        width, height = struct.unpack('>II', data[16:24])
        check(f'{icon.name} 是 {size}x{size}', (width, height) == (size, size), f'{width}x{height}')
    for name in ('vue.js', 'lucide.js', 'tailwind.js', 'close.js', 'overlay.js', 'icon.png'):
        check(f'本地资源 ui/assets/{name}', (target / 'ui' / 'assets' / name).is_file())

    print('启动假路由器与真实服务')
    router_port = free_port()
    router = FakeRouterServer(('127.0.0.1', router_port), FakeRouter)
    threading.Thread(target=router.serve_forever, daemon=True).start()

    os.environ['TRIM_APPDEST'] = str(target)
    os.environ['TRIM_PKGETC'] = str(pkgetc)
    os.environ['TRIM_PKGVAR'] = str(pkgvar)
    os.environ['TRIM_TMP'] = str(work / 'tmp')
    os.environ['TRIM_APPNAME'] = 'unificenter'
    os.environ.pop('UI_DIR', None)

    sys.path.insert(0, str(target / 'server' / 'unifi_center'))
    import engine                                                          # noqa: PLC0415
    import server                                                          # noqa: PLC0415

    check('UI 目录按 TRIM_APPDEST 解析', engine.ui_dir() == target / 'ui', str(engine.ui_dir()))
    check('Socket 路径按 TRIM_APPDEST 解析',
          engine.socket_path() == target / 'app.sock', str(engine.socket_path()))
    check('版本号读得到', bool(engine.VERSION), engine.VERSION)

    engine.save_settings({'target': f'http://127.0.0.1:{router_port}/'})
    engine.save_router_token(TOKEN)
    check('令牌文件写在 TRIM_PKGETC', (pkgetc / 'router-token').is_file())

    port = free_port()
    state = server.AppState(dev_port=port)
    httpd = server.ThreadingTcpServer(('127.0.0.1', port),
                                     type('Bound', (server.Handler,), {'app': state}))
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    time.sleep(0.3)
    base = f'http://127.0.0.1:{port}'
    admin = {'X-Trim-Userid': '1000', 'X-Trim-Username': 'admin', 'X-Trim-Isadmin': 'true'}
    user = {'X-Trim-Userid': '1001', 'X-Trim-Username': 'bob', 'X-Trim-Isadmin': 'false'}

    print('打接口')
    status, body = request(f'{base}/healthz')
    check('/healthz 200', status == 200 and json.loads(body)['ok'])

    status, body = request(f'{base}/ctl/status', admin)
    payload = json.loads(body)
    check('/ctl/status 探测到路由器', status == 200 and payload['router']['reachable']
          and payload['router']['device'] == 'R619AC', body[:200].decode('utf-8', 'replace'))
    check('/ctl/status 令牌已保存且有效', payload['token_set'] and payload['router']['authenticated'])

    status, body = request(f'{base}/ctl/settings', admin)
    check('管理员能看到明文令牌', json.loads(body)['token'] == TOKEN)
    status, body = request(f'{base}/ctl/settings', user)
    settings = json.loads(body)
    check('普通用户看不到明文令牌', settings['token'] == '' and settings['token_visible'] is False
          and TOKEN not in body.decode('utf-8'), body[:200].decode('utf-8', 'replace'))
    check('普通用户不能改设置', settings['can_edit'] is False)

    status, body = request(f'{base}/')
    text = body.decode('utf-8')
    check('页面前缀返回本地化页面', status == 200 and '/app/unificenter/assets/vue.js' in text)
    check('页面已注入令牌', f'localStorage.setItem("sc_token","{TOKEN}")' in text)
    check('GitHub 直连已改走 NAS', '/app/unificenter/github/api/' in text and 'api.github.com' not in text)
    check('已注入关闭/设置按钮', 'assets/close.js' in text and 'assets/overlay.js' in text)

    FakeRouter.seen.clear()
    status, body = request(f'{base}/api/system/info')
    check('/api 转发带上了保存的令牌', status == 200 and FakeRouter.seen[-1] == TOKEN, str(FakeRouter.seen))

    status, body = request(f'{base}/ctl/panel.html', admin)
    check('设置面板可打开', status == 200 and b'targetInput' in body)

    status, body = request(f'{base}/assets/../../manifest')
    check('路径穿越被拒绝', status in (400, 403, 404), str(status))

    httpd.shutdown()
    httpd.server_close()
    router.shutdown()
    router.server_close()
    print('\n全部冒烟检查通过 ✓')
    return 0


if __name__ == '__main__':
    raise SystemExit(main(sys.argv))
