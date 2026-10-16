"""路由器软件中心（fnOS 移植版）服务端。

职责（与原版一一对应，只是入口从「小米插件 + 自渲染 nginx」换成「fnOS 统一网关 + Unix Socket」）：

  1. 页面前缀（/app/unificenter/）返回本地化后的软件中心页面（CDN 本地副本 + 令牌注入）；
  2. /api/…        转发路由器软件中心的接口（Authorization 按规则取值）；
  3. /api/github/… 转发页面里对 GitHub 的直连（桌面窗口访问不了外网）；
  4. /ctl/…        本应用自己的接口：状态、设置、令牌、日志、设置面板；
  5. /assets/…     本地资源（Vue / Tailwind / Lucide / 图标 / 前端脚本）。

监听：$TRIM_APPDEST/app.sock（统一网关的 gatewaySocket）。
调试：设 UNIFICENTER_DEV_PORT=18101 可同时（或单独，无 TRIM_APPDEST 时）监听 127.0.0.1 TCP 端口。

写操作只有一处：保存设置（目标地址、AdminToken）。绝不改动路由器上的任何东西，
所有对路由器的请求都是只读探测或用户自己发起的转发。
"""

from __future__ import annotations

import argparse
import json
import os
import posixpath
import socket
import socketserver
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import engine                                                              # noqa: E402

CONTENT_TYPES = {
    '.html': 'text/html; charset=utf-8',
    '.js': 'text/javascript; charset=utf-8',
    '.css': 'text/css; charset=utf-8',
    '.json': 'application/json; charset=utf-8',
    '.png': 'image/png',
    '.jpg': 'image/jpeg',
    '.svg': 'image/svg+xml',
    '.ico': 'image/x-icon',
    '.woff2': 'font/woff2',
    '.txt': 'text/plain; charset=utf-8',
}

#: 页面里注入的脚本（关闭按钮 + 右上角设置入口）
OVERLAY_SCRIPTS = (
    '<script src="{base}/assets/close.js" data-mode="close" defer></script>'
    '<script src="{base}/assets/overlay.js" defer></script>'
)

#: 软件中心是 REST + 长轮询，转发超时放长一些；GitHub 转发单独用较短超时。
ROUTER_TIMEOUT = float(os.environ.get('ROUTER_TIMEOUT', '3600'))
GITHUB_TIMEOUT = float(os.environ.get('GITHUB_TIMEOUT', '30'))


class AppState:
    """进程级共享状态。"""

    def __init__(self, dev_port: int | None = None) -> None:
        self.started = time.time()
        self.dev_port = dev_port
        self.ui_dir = engine.ui_dir()


class Handler(BaseHTTPRequestHandler):
    server_version = 'FnOsUnifiCenter/' + (engine.VERSION or 'unknown')
    protocol_version = 'HTTP/1.1'
    app: AppState

    # -- 基础工具 ---------------------------------------------------------

    def log_message(self, fmt: str, *args) -> None:                        # noqa: A003
        """默认的 stderr 访问日志太吵，转成自己的日志（只记异常与写操作）。"""
        return

    def log_event(self, message: str) -> None:
        engine.log(f'{self.client_address[0] if self.client_address else "-"} {message}')

    def gateway_user(self) -> engine.GatewayUser:
        return engine.gateway_user(self.headers)

    def base_prefix(self) -> str:
        """页面/接口对外前缀：网关注册的 /app/unificenter（去掉结尾斜杠）。"""
        return '/' + engine.GATEWAY_PREFIX.strip('/')

    def _send(self, status: HTTPStatus | int, body: bytes, content_type: str,
              extra: dict[str, str] | None = None) -> None:
        self.send_response(int(status))
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Cache-Control', 'no-store')
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.send_header('Referrer-Policy', 'no-referrer')
        for name, value in (extra or {}).items():
            self.send_header(name, value)
        self.end_headers()
        if self.command == 'HEAD':
            return
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError, OSError):
            pass

    def _json(self, status: HTTPStatus | int, payload: object) -> None:
        self._send(status, json.dumps(payload, ensure_ascii=False).encode('utf-8'),
                   'application/json; charset=utf-8')

    def _error(self, status: HTTPStatus | int, message: str) -> None:
        self._json(status, {'ok': False, 'error': message})

    def _html(self, status: HTTPStatus | int, text: str) -> None:
        self._send(status, text.encode('utf-8'), 'text/html; charset=utf-8')

    def _read_body(self, limit: int = 32768) -> bytes | None:
        length = int(self.headers.get('Content-Length') or 0)
        if length > limit:
            self._error(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, '请求体过大')
            return None
        return self.rfile.read(length) if length else b''

    def _redirect(self, location: str) -> None:
        self.send_response(HTTPStatus.MOVED_PERMANENTLY)
        self.send_header('Location', location)
        self.send_header('Content-Length', '0')
        self.end_headers()

    # -- 路由 -------------------------------------------------------------

    def do_GET(self) -> None:                                              # noqa: N802
        self._dispatch('GET')

    def do_HEAD(self) -> None:                                             # noqa: N802
        self._dispatch('HEAD')

    def do_POST(self) -> None:                                             # noqa: N802
        self._dispatch('POST')

    def _dispatch(self, method: str) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = urllib.parse.unquote(parsed.path)
        if not path.startswith('/'):
            self._error(HTTPStatus.BAD_REQUEST, '请求路径不合法')
            return
        if '..' in path.split('/'):
            self._error(HTTPStatus.BAD_REQUEST, '请求路径不合法')
            return

        # 网关把 /app/unificenter/... 原样转给本服务；这里接受带前缀与不带前缀两种写法。
        prefix = self.base_prefix()
        if path == prefix:
            self._redirect(prefix + '/')
            return
        if path.startswith(prefix + '/'):
            path = path[len(prefix):]

        if path == '/healthz':
            self._healthz()
            return
        if path == '/ctl' or path.startswith('/ctl/'):
            self._control(method, path[len('/ctl'):] or '/', parsed)
            return
        if path.startswith('/view'):
            self._view(method, path[len('/view'):], parsed)
            return
        if path == '/' or path in ('/index.html', '/control.html', '/control'):
            if method not in ('GET', 'HEAD'):
                self._error(HTTPStatus.METHOD_NOT_ALLOWED, '方法不被支持')
                return
            self._serve_softcenter(parsed)
            return
        if path.startswith('/github/'):
            # github_upstream 认的是 `github/api/…` 这种形状，把前缀补回去
            self._proxy_github('github/' + path[len('/github/'):], parsed, method)
            return
        if path.startswith('/api/github/'):
            self._proxy_github(path[len('/api/'):], parsed, method)
            return
        if path.startswith('/api/'):
            self._proxy_router(path.lstrip('/'), parsed, method)
            return
        if method not in ('GET', 'HEAD'):
            self._error(HTTPStatus.METHOD_NOT_ALLOWED, '方法不被支持')
            return
        self._serve_static(path)

    # -- 软件中心页面 -----------------------------------------------------

    def _serve_softcenter(self, parsed: urllib.parse.ParseResult) -> None:
        """页面前缀：直接返回本地化后的软件中心页面（不套 iframe）。

        已保存的 AdminToken 由本服务无条件注入 localStorage.sc_token：
        注入不挑入口，这样哪个用户/哪个入口打开都不会再弹软件中心那层"配置门"。
        """
        try:
            html = engine.fetch_router_page()
        except Exception as error:                                        # noqa: BLE001
            self.log_event(f'取软件中心页面失败：{error}')
            self._html(HTTPStatus.BAD_GATEWAY, self._offline_page(str(error)))
            return
        base = self.base_prefix()
        # service_base 留空 ⇒ 页面里的 GitHub 直连改走 {base}/github/…（本服务自己转发）
        rewritten, unknown = engine.prepare_page(
            html, base, service_base='', token=engine.router_token())
        scripts = OVERLAY_SCRIPTS.format(base=base)
        if '</body>' in rewritten:
            rewritten = rewritten.replace('</body>', scripts + '</body>', 1)
        else:
            rewritten += scripts
        if unknown:
            self.log_event(f'页面里仍有外部脚本未本地化：{unknown[:3]}')
        missing = [name for name, present in engine.assets_present(self.app.ui_dir).items() if not present]
        if missing:
            self.log_event(f'本地资源缺失 {missing}，页面可能显示不全')
        self._html(HTTPStatus.OK, rewritten)

    def _offline_page(self, detail: str) -> str:
        """路由器连不上时的兜底页面：说明原因 + 进设置面板的入口。"""
        base = self.base_prefix()
        target = engine.load_settings()['target']
        return f"""<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>路由器软件中心</title>
<link rel="stylesheet" href="{base}/assets/panel.css">
</head><body class="rc-body">
<main class="rc-panel">
  <section class="rc-card">
    <h2>连不上路由器软件中心</h2>
    <p class="rc-muted">目标地址：<code>{target}</code></p>
    <p class="rc-error">{detail}</p>
    <p class="rc-muted">请确认路由器已开机、软件中心正在运行，并检查下面的设置。</p>
    <div class="rc-card-actions">
      <a class="rc-btn is-primary" href="{base}/ctl/panel.html">打开设置</a>
      <a class="rc-btn" href="{base}/">重新加载</a>
    </div>
  </section>
</main>
<script src="{base}/assets/close.js" data-mode="close" defer></script>
</body></html>"""

    # -- 本应用自己的接口 -------------------------------------------------

    def _healthz(self) -> None:
        self._json(HTTPStatus.OK, {
            'ok': True,
            'app': engine.APP_NAME,
            'version': engine.VERSION,
            'target': engine.load_settings()['target'],
            'uptime': int(time.time() - self.app.started),
            'socket': str(engine.socket_path()),
            'assets': engine.assets_present(self.app.ui_dir),
        })

    def _control(self, method: str, rest: str, parsed: urllib.parse.ParseResult) -> None:
        route = rest.split('?', 1)[0]
        user = self.gateway_user()

        if route in ('/status', '/status/'):
            if method not in ('GET', 'HEAD'):
                self._error(HTTPStatus.METHOD_NOT_ALLOWED, '方法不被支持')
                return
            self._json(HTTPStatus.OK, engine.status_payload(
                port=self.app.dev_port, user=user, static_dir=self.app.ui_dir))
            return

        if route in ('/settings', '/settings/'):
            if method == 'GET':
                self._get_settings(user)
                return
            if method == 'POST':
                self._post_settings(user)
                return
            self._error(HTTPStatus.METHOD_NOT_ALLOWED, '方法不被支持')
            return

        if route in ('/token', '/token/'):
            if method not in ('GET', 'HEAD'):
                self._error(HTTPStatus.METHOD_NOT_ALLOWED, '方法不被支持')
                return
            if not engine.can_reveal_token(user):
                self._error(HTTPStatus.FORBIDDEN, '只有管理员可以查看 AdminToken 明文')
                return
            self._json(HTTPStatus.OK, {'ok': True, 'token': engine.router_token(),
                                       'token_set': bool(engine.router_token())})
            return

        if route in ('/logs', '/logs/'):
            if method not in ('GET', 'HEAD'):
                self._error(HTTPStatus.METHOD_NOT_ALLOWED, '方法不被支持')
                return
            self._json(HTTPStatus.OK, {'ok': True, 'lines': engine.read_tail(limit=200),
                                       'file': str(engine.LOG_FILE)})
            return

        if route in ('/probe', '/probe/'):
            if method not in ('GET', 'HEAD'):
                self._error(HTTPStatus.METHOD_NOT_ALLOWED, '方法不被支持')
                return
            self._json(HTTPStatus.OK, {'ok': True, 'router': engine.probe_router()})
            return

        if route in ('', '/'):
            self._redirect(self.base_prefix() + '/ctl/panel.html')
            return

        # 设置面板与它自己的静态资源，从本服务的 UI 目录提供（不复用打包进 fpk 的静态文件服务）
        if method not in ('GET', 'HEAD'):
            self._error(HTTPStatus.METHOD_NOT_ALLOWED, '方法不被支持')
            return
        self._serve_ui_file(rest)

    def _get_settings(self, user: engine.GatewayUser) -> None:
        """GET /ctl/settings：明文令牌只给已登录的管理员，其他人只给提示。

        这是从原版「设备所有者的小米客户端才看得到明文」改写来的：fnOS 上没有客户端证书，
        但统一网关会给出可信的 X-Trim-Isadmin，因此判据换成「网关 + 管理员」。
        """
        token = engine.router_token()
        trusted = engine.can_reveal_token(user)
        self._json(HTTPStatus.OK, {
            'ok': True,
            'target': engine.load_settings()['target'],
            'default_target': engine.DEFAULT_TARGET,
            'token_set': bool(token),
            'token': token if trusted else '',
            'token_hint': engine.token_hint(token),
            'token_visible': trusted,
            'can_edit': engine.can_change_settings(user),
            'user': user.as_dict(),
            'gateway_prefix': self.base_prefix(),
            'assets': engine.assets_present(self.app.ui_dir),
            'socket_path': str(engine.socket_path()),
        })

    def _post_settings(self, user: engine.GatewayUser) -> None:
        if not engine.can_change_settings(user):
            self._error(HTTPStatus.FORBIDDEN, '只有管理员可以修改连接设置')
            return
        body = self._read_body()
        if body is None:
            return
        try:
            payload = json.loads(body.decode('utf-8')) if body else {}
        except (UnicodeDecodeError, json.JSONDecodeError):
            self._error(HTTPStatus.BAD_REQUEST, '请求体不是合法 JSON')
            return
        if not isinstance(payload, dict):
            self._error(HTTPStatus.BAD_REQUEST, '请求体必须是 JSON 对象')
            return
        target = str(payload.get('target') or engine.load_settings()['target'])
        try:
            if 'token' in payload:
                engine.save_router_token(str(payload.get('token') or ''))
            engine.apply_target(target)
        except RuntimeError as error:
            self._json(HTTPStatus.CONFLICT, {'ok': False, 'error': str(error),
                                             **engine.status_payload(
                                                 port=self.app.dev_port, user=user,
                                                 static_dir=self.app.ui_dir)})
            return
        except OSError as error:
            self.log_event(f'保存设置失败：{error}')
            self._error(HTTPStatus.INTERNAL_SERVER_ERROR, f'写入失败：{error}')
            return
        self.log_event(f'设置已更新：目标 {engine.load_settings()["target"]}（{user.label}）')
        self._json(HTTPStatus.OK, engine.status_payload(
            port=self.app.dev_port, user=user, static_dir=self.app.ui_dir))

    # -- 转发：软件中心的接口 ---------------------------------------------

    def _proxy_router(self, suffix: str, parsed: urllib.parse.ParseResult, method: str) -> None:
        """把 /api/… 转发到路由器软件中心。

        转发用的 Authorization 不是无脑透传：控制台那一跳会把客户端凭据覆盖成占位值
        （Bearer console-loopback），那个值送到路由器必然是"令牌不正确"。
        取值规则见 engine.resolve_authorization —— 占位值换成保存的令牌，真实令牌原样过。
        """
        target = engine.load_settings()['target'].rstrip('/')
        body = self._read_body(limit=8 * 1024 * 1024) if method in ('POST', 'PUT', 'PATCH', 'DELETE') else None
        if body is None and method in ('POST', 'PUT', 'PATCH', 'DELETE'):
            return
        url = f'{target}/{suffix}'
        if parsed.query:
            url += f'?{parsed.query}'
        headers = {
            'User-Agent': self.headers.get('User-Agent') or 'FnOsUnifiCenter',
            'Accept': self.headers.get('Accept') or '*/*',
        }
        authorization = engine.resolve_authorization(self.headers.get('Authorization') or '')
        if authorization:
            headers['Authorization'] = authorization
        for name in ('Content-Type', 'Accept-Language', 'Content-Length'):
            value = self.headers.get(name)
            if value and name != 'Content-Length':
                headers[name] = value
        if body:
            headers['Content-Length'] = str(len(body))
        self._forward(url, headers, body, method, label=suffix)

    def _proxy_github(self, rest: str, parsed: urllib.parse.ParseResult, method: str) -> None:
        """把页面里对 GitHub 的直连改为经 NAS 转发（桌面窗口访问不了外网）。

        注意：绝不能把客户端的 Authorization 带过去，否则 GitHub 会判成无效凭据返回 401。
        """
        upstream = engine.github_upstream(rest)
        if not upstream:
            self._error(HTTPStatus.NOT_FOUND, '不是 GitHub 转发路径')
            return
        if method not in ('GET', 'HEAD'):
            self._error(HTTPStatus.METHOD_NOT_ALLOWED, 'GitHub 转发只支持 GET')
            return
        url = upstream + (f'?{parsed.query}' if parsed.query else '')
        headers = {
            'User-Agent': self.headers.get('User-Agent') or 'FnOsUnifiCenter',
            'Accept': self.headers.get('Accept') or 'application/json',
        }
        self._forward(url, headers, None, method, label=f'github {rest}', timeout=GITHUB_TIMEOUT)

    def _forward(self, url: str, headers: dict[str, str], body: bytes | None,
                 method: str, label: str, timeout: float = ROUTER_TIMEOUT) -> None:
        request = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                payload = response.read() if method != 'HEAD' else b''
                content_type = response.headers.get('Content-Type') or 'application/octet-stream'
                self._send(HTTPStatus(response.status), payload, content_type)
        except urllib.error.HTTPError as error:
            # 401/404 这些要原样回给页面（软件中心靠 401 判断令牌错误）
            payload = error.read() or b''
            self._send(HTTPStatus(error.code), payload,
                       error.headers.get('Content-Type') or 'application/json; charset=utf-8')
        except Exception as error:                                        # noqa: BLE001
            self.log_event(f'转发失败 {label}: {error}')
            self._error(HTTPStatus.BAD_GATEWAY, f'无法访问目标：{error}')

    # -- 兼容入口 /view/… -------------------------------------------------

    def _view(self, method: str, rest: str, parsed: urllib.parse.ParseResult) -> None:
        """兼容入口（原版的小米客户端路径用 /view/…，这里保留，便于老书签与对照测试）。"""
        suffix = rest.lstrip('/')
        if suffix.startswith('github/'):
            self._proxy_github(suffix, parsed, method)
            return
        if suffix in ('', 'index.html') and method in ('GET', 'HEAD'):
            self._serve_softcenter(parsed)
            return
        if method == 'GET' and suffix in ('', 'panel.html', 'panel.js', 'panel.css'):
            self._serve_ui_file(suffix or 'panel.html')
            return
        self._proxy_router(suffix, parsed, method)

    # -- 静态资源 ---------------------------------------------------------

    def _serve_ui_file(self, rest: str) -> None:
        """从 UI 目录提供文件（只允许白名单内的文件，设置面板用）。"""
        relative = rest.lstrip('/')
        allowed = {'panel.html', 'panel.js', 'panel.css', 'index.html'}
        if relative not in allowed:
            self._error(HTTPStatus.NOT_FOUND, '文件不存在')
            return
        self._send_file(self.app.ui_dir / relative)

    def _serve_static(self, path: str) -> None:
        """提供 /assets/… 与 /images/…（本地 CDN 副本、图标、前端脚本）。"""
        relative = posixpath.normpath(path.lstrip('/'))
        if relative.startswith('../') or relative in ('..', '.'):
            self._error(HTTPStatus.FORBIDDEN, '拒绝访问')
            return
        if not (relative.startswith('assets/') or relative.startswith('images/')
                or relative in ('panel.html', 'panel.js', 'panel.css', 'index.html',
                                'favicon.ico', 'icon.png')):
            self._error(HTTPStatus.NOT_FOUND, '文件不存在')
            return
        root = self.app.ui_dir
        candidate = (root / relative).resolve()
        try:
            candidate.relative_to(root.resolve())
        except ValueError:
            self._error(HTTPStatus.FORBIDDEN, '拒绝访问')
            return
        if candidate.is_dir():
            candidate = candidate / 'index.html'
        self._send_file(candidate)

    def _send_file(self, candidate: Path) -> None:
        if not candidate.is_file():
            self._error(HTTPStatus.NOT_FOUND, '文件不存在')
            return
        content_type = CONTENT_TYPES.get(candidate.suffix.lower(), 'application/octet-stream')
        try:
            payload = candidate.read_bytes()
        except OSError as error:
            self._error(HTTPStatus.INTERNAL_SERVER_ERROR, f'读取失败：{error}')
            return
        self._send(HTTPStatus.OK, payload, content_type,
                   extra={'Cache-Control': 'no-cache, no-store, must-revalidate'})


if hasattr(socketserver, 'UnixStreamServer'):        # Windows 上只在调试端口模式跑
    class ThreadingUnixServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
        daemon_threads = True
        allow_reuse_address = True
else:                                                # pragma: no cover - 仅开发机
    ThreadingUnixServer = None                       # type: ignore[assignment]


class ThreadingTcpServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    daemon_threads = True
    allow_reuse_address = True


def build_server(host: str, port: int, state: AppState) -> socketserver.BaseServer:
    handler = type('BoundHandler', (Handler,), {'app': state})
    use_socket = bool((os.environ.get('TRIM_APPDEST') or '').strip()) or port <= 0
    if use_socket:
        if ThreadingUnixServer is None:
            raise OSError('本平台不支持 Unix Socket，请用 UNIFICENTER_DEV_PORT 指定调试端口')
        path = engine.socket_path()
        path.parent.mkdir(parents=True, exist_ok=True)
        engine.unlink_stale_socket(path)
        server: socketserver.BaseServer = ThreadingUnixServer(str(path), handler)
        try:
            os.chmod(path, 0o666)          # 网关以系统身份连接，socket 需要可写
        except OSError:
            pass
        engine.log(f'已监听 Unix Socket {path}（fnOS 统一网关目标）')
        return server
    server = ThreadingTcpServer((host, port), handler)
    engine.log(f'已监听 http://{host}:{port}（本机调试端口）')
    return server


def serve_forever(servers: list[socketserver.BaseServer]) -> None:
    threads = [threading.Thread(target=s.serve_forever, name=f'serve-{i}', daemon=True)
               for i, s in enumerate(servers)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description='飞牛 fnOS · 路由器软件中心')
    parser.add_argument('--host', default=os.environ.get('HOST', '127.0.0.1'))
    parser.add_argument('--port', type=int, default=int(os.environ.get('UNIFICENTER_DEV_PORT', '0')),
                        help='本机调试用 TCP 端口（0 表示只用 Unix Socket）')
    args = parser.parse_args(argv)

    state = AppState(dev_port=args.port or None)
    servers: list[socketserver.BaseServer] = []
    try:
        servers.append(build_server(args.host, args.port, state))
        if args.port > 0 and (os.environ.get('TRIM_APPDEST') or '').strip():
            # 目标机上也想临时用 TCP 调试时，Unix Socket 与端口同时提供
            handler = type('BoundHandler', (Handler,), {'app': state})
            servers.append(ThreadingTcpServer((args.host, args.port), handler))
            engine.log(f'并已监听 http://{args.host}:{args.port}（调试端口）')
    except OSError as error:
        message = f'启动失败（端口或 Socket 被占用）：{error}'
        engine.log(message)
        logfile = (os.environ.get('TRIM_TEMP_LOGFILE') or '').strip()
        if logfile:
            try:
                Path(logfile).write_text(message + '\n', encoding='utf-8')
            except OSError:
                pass
        return 1

    engine.log(f'路由器软件中心服务已启动：目标 {engine.load_settings()["target"]}，'
               f'前端 {state.ui_dir}，网关前缀 {engine.GATEWAY_PREFIX}')
    try:
        serve_forever(servers)
    except KeyboardInterrupt:
        pass
    finally:
        for item in servers:
            item.server_close()
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
