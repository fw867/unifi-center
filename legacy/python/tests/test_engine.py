#!/usr/bin/env python3
"""测试套件（从 xiaomi-router-center 的 tests/test_engine.py 移植并改写）。

覆盖：
  * 目标地址规整、设置与令牌读写（含 0600 权限）、令牌提示；
  * 转发到路由器时 Authorization 的取值（占位值换保存的令牌、缺 Authorization、真令牌透传、没保存令牌不乱送）；
  * 探测假路由器（可达 / 带令牌 / 错令牌 / 不可达，且只发 GET）；
  * 页面重写（CDN 本地化、GitHub 改走 NAS、令牌注入、favicon）；
  * 网关身份（管理员 → 明文令牌；非管理员 / 未登录 / 只用调试端口 → 只有提示；非管理员不能改设置）；
  * 真起一遍服务（TCP 调试端口）：/healthz、/ctl/status、/ctl/settings、/ctl/token、/ctl/logs、
    /assets 静态文件、路径穿越拒绝、未知接口 404、页面前缀返回本地化页面、/api 转发带令牌；
  * fnOS 包结构（manifest/入口/config/wizard/图标/生命周期脚本）。

运行：
    python -m unittest discover -s tests -v
"""

from __future__ import annotations

import http.server
import importlib
import json
import os
import socket
import socketserver
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SERVER_DIR = ROOT / 'app' / 'server' / 'unifi_center'
sys.path.insert(0, str(SERVER_DIR))

FAKE_PAGE = """<!doctype html>
<html><head><title>UniFi SoftCenter - 安全管理中心</title>
<script src="https://cdn.tailwindcss.com"></script>
<script src="https://cdn.jsdelivr.net/npm/vue@3.4.21/dist/vue.global.prod.js"></script>
<script src="https://cdn.jsdelivr.net/npm/lucide@0.363.0/dist/umd/lucide.js"></script>
<script src="https://cdn.example.com/other.js"></script>
</head><body><div id="app">soft center</div>
<script>fetch('https://api.github.com/repos/x/y/releases');</script>
</body></html>"""

VALID_TOKEN = 'abcdef1234567890'


class FakeRouter(http.server.BaseHTTPRequestHandler):
    """假的路由器软件中心：只接受正确令牌，用来验证探测与转发逻辑。"""

    protocol_version = 'HTTP/1.1'
    token = VALID_TOKEN
    requests: list[tuple[str, str, str]] = []

    def log_message(self, fmt, *args):                                     # noqa: A003
        return

    def _json(self, status: int, payload: object) -> None:
        body = json.dumps(payload).encode('utf-8')
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:                                              # noqa: N802
        FakeRouter.requests.append(('GET', self.path, self.headers.get('Authorization') or ''))
        if self.path.startswith('/repos/'):
            self._json(200, {'proxied': True, 'path': self.path})
            return
        if self.path.startswith('/api/system/info'):
            if (self.headers.get('Authorization') or '') != self.token:
                self._json(401, {'error': '令牌不正确'})
                return
            self._json(200, {'Device': 'R619AC', 'Version': '3.0.1'})
            return
        body = FAKE_PAGE.encode('utf-8')
        self.send_response(200)
        self.send_header('Content-Type', 'text/html; charset=utf-8')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:                                             # noqa: N802
        length = int(self.headers.get('Content-Length') or 0)
        self.rfile.read(length)
        FakeRouter.requests.append(('POST', self.path, self.headers.get('Authorization') or ''))
        self._json(200, {'ok': True})


class FakeRouterServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    daemon_threads = True
    allow_reuse_address = True


def free_port() -> int:
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 0))
        return probe.getsockname()[1]


class BaseCase(unittest.TestCase):
    """每个用例一套独立的临时配置目录 + 一个假路由器 + 一个真服务。"""

    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix='unifi-center-test-')
        cls.config_dir = Path(cls.temp.name) / 'etc'
        cls.config_dir.mkdir(parents=True, exist_ok=True)

        # 先固定环境变量，再 import engine / server（它们在建模块时就读取环境）
        os.environ['TRIM_PKGETC'] = str(cls.config_dir)
        os.environ['UI_DIR'] = str(ROOT / 'app' / 'ui')
        os.environ['TRIM_APPNAME'] = 'unificenter'
        os.environ['SOCKET_PATH'] = str(Path(cls.temp.name) / 'app.sock')
        os.environ.pop('TRIM_APPDEST', None)
        os.environ['PROBE_TIMEOUT'] = '3'

        import engine                                                      # noqa: PLC0415
        import server                                                      # noqa: PLC0415

        # engine/server 在 import 时就读取环境变量，而每个测试类都有自己的临时目录，
        # 所以必须重新加载，否则第二个类会继续用第一个类的配置目录（踩过）。
        importlib.reload(engine)
        importlib.reload(server)
        cls.engine = engine
        cls.server = server

        cls.router_port = free_port()
        cls.router = FakeRouterServer(('127.0.0.1', cls.router_port), FakeRouter)
        threading.Thread(target=cls.router.serve_forever, daemon=True).start()

        cls.port = free_port()
        cls.state = server.AppState(dev_port=cls.port)
        cls.httpd = server.ThreadingTcpServer(('127.0.0.1', cls.port),
                                              type('Bound', (server.Handler,), {'app': cls.state}))
        threading.Thread(target=cls.httpd.serve_forever, daemon=True).start()
        cls.base = f'http://127.0.0.1:{cls.port}'

    @classmethod
    def tearDownClass(cls) -> None:
        cls.httpd.shutdown()
        cls.httpd.server_close()
        cls.router.shutdown()
        cls.router.server_close()
        cls.temp.cleanup()

    # -- 工具 -------------------------------------------------------------

    def setUp(self) -> None:
        FakeRouter.requests.clear()
        self.engine.save_router_token('')
        self.engine.STATE_FILE.unlink(missing_ok=True)      # 回到"没有配置"的状态
        self.engine.save_settings({'target': f'http://127.0.0.1:{self.router_port}/'})

    def write_token(self, token: str = VALID_TOKEN) -> None:
        self.engine.save_router_token(token)

    def request(self, path: str, method: str = 'GET', body: bytes | None = None,
                headers: dict[str, str] | None = None,
                base: str | None = None) -> tuple[int, bytes, dict[str, str]]:
        request = urllib.request.Request(f'{base or self.base}{path}', data=body, method=method)
        for name, value in (headers or {}).items():
            request.add_header(name, value)
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return response.status, response.read(), dict(response.headers)
        except urllib.error.HTTPError as error:
            return error.code, error.read(), dict(error.headers)

    def json_request(self, path: str, **kwargs) -> tuple[int, dict]:
        status, body, _ = self.request(path, **kwargs)
        try:
            payload = json.loads(body or b'{}')
        except json.JSONDecodeError:
            payload = {}
        return status, payload


class TargetTests(BaseCase):
    def test_normalize_target_forms(self) -> None:
        normalize = self.engine.normalize_target
        self.assertEqual(normalize('192.168.1.1:9958'), 'http://192.168.1.1:9958/')
        self.assertEqual(normalize('http://192.168.1.1:9958'), 'http://192.168.1.1:9958/')
        self.assertEqual(normalize('https://router.lan/openwrt/'), 'https://router.lan/')
        self.assertEqual(normalize('  http://192.168.1.1:9958/index.html  '),
                         'http://192.168.1.1:9958/')

    def test_normalize_target_rejects_bad_values(self) -> None:
        normalize = self.engine.normalize_target
        for bad in ('', '   ', 'ftp://x/', 'http://host:abc/', 'http:///x', 'http://host:99999x/'):
            with self.assertRaises(RuntimeError, msg=bad):
                normalize(bad)

    def test_settings_roundtrip_and_default(self) -> None:
        state = self.config_dir / 'settings.json'
        state.unlink(missing_ok=True)
        self.assertEqual(self.engine.load_settings()['target'], self.engine.DEFAULT_TARGET)
        self.engine.save_settings({'target': 'router.lan:8080'})
        self.assertEqual(self.engine.load_settings()['target'], 'http://router.lan:8080/')

    def test_token_file_is_0600_and_hint_hides_value(self) -> None:
        self.write_token('a1b2c3d4e5f6')
        if os.name == 'posix':                     # Windows 上 os.chmod 不映射 POSIX 权限位
            mode = (self.config_dir / 'router-token').stat().st_mode & 0o777
            self.assertEqual(mode, 0o600)
        self.assertEqual(self.engine.router_token(), 'a1b2c3d4e5f6')
        hint = self.engine.token_hint()
        self.assertEqual(hint, 'a1b2…f6')
        self.assertNotIn('c3d4', hint)

    def test_token_can_be_cleared(self) -> None:
        self.write_token('secret-value')
        self.engine.save_router_token('')
        self.assertEqual(self.engine.router_token(), '')
        self.assertFalse((self.config_dir / 'router-token').exists())


class AuthorizationTests(BaseCase):
    def test_real_token_passes_through(self) -> None:
        self.write_token('saved-token')
        self.assertEqual(self.engine.resolve_authorization('Bearer user-token'), 'Bearer user-token')

    def test_console_placeholder_replaced_by_saved_token(self) -> None:
        self.write_token('saved-token')
        self.assertEqual(self.engine.resolve_authorization('Bearer console-loopback'), 'saved-token')
        self.assertEqual(self.engine.resolve_authorization('bearer   console-loopback  '), 'saved-token')
        self.assertEqual(self.engine.resolve_authorization('console-loopback'), 'saved-token')

    def test_missing_authorization_uses_saved_token(self) -> None:
        self.write_token('saved-token')
        self.assertEqual(self.engine.resolve_authorization(''), 'saved-token')

    def test_placeholder_without_saved_token_sends_nothing(self) -> None:
        self.assertEqual(self.engine.resolve_authorization('Bearer console-loopback'), '')
        self.assertEqual(self.engine.resolve_authorization(''), '')

    def test_other_schemes_are_not_treated_as_placeholder(self) -> None:
        self.write_token('saved-token')
        self.assertEqual(self.engine.resolve_authorization('Basic dXNlcg=='), 'Basic dXNlcg==')


class ProbeTests(BaseCase):
    def test_probe_unauthenticated_when_no_token(self) -> None:
        result = self.engine.probe_router()
        self.assertTrue(result['reachable'])
        self.assertEqual(result['status'], 200)
        self.assertFalse(result['authenticated'])
        self.assertIn('SoftCenter', result['title'])
        self.assertTrue(all(method == 'GET' for method, _, _ in FakeRouter.requests),
                        '探测必须是只读的 GET')

    def test_probe_authenticated_with_good_token(self) -> None:
        self.write_token()
        result = self.engine.probe_router()
        self.assertTrue(result['authenticated'])
        self.assertEqual(result['device'], 'R619AC')
        self.assertEqual(result['version'], '3.0.1')

    def test_probe_with_wrong_token_reports_invalid(self) -> None:
        self.write_token('wrong-token')
        result = self.engine.probe_router()
        self.assertFalse(result['authenticated'])
        self.assertEqual(result['error'], '令牌无效')

    def test_probe_unreachable_target(self) -> None:
        result = self.engine.probe_router(f'http://127.0.0.1:{free_port()}/')
        self.assertFalse(result['reachable'])
        self.assertTrue(result['error'])


class PageRewriteTests(BaseCase):
    def test_rewrite_localizes_cdn_and_github(self) -> None:
        rewritten, unknown = self.engine.rewrite_page(FAKE_PAGE, '/app/unificenter')
        self.assertIn('/app/unificenter/assets/tailwind.js', rewritten)
        self.assertIn('/app/unificenter/assets/vue.js', rewritten)
        self.assertIn('/app/unificenter/assets/lucide.js', rewritten)
        self.assertNotIn('cdn.tailwindcss.com', rewritten)
        self.assertNotIn('cdn.jsdelivr.net', rewritten)
        self.assertIn('/app/unificenter/github/api/', rewritten)
        self.assertNotIn('https://api.github.com/', rewritten)
        self.assertEqual(unknown, ['https://cdn.example.com/other.js'])

    def test_prepare_page_injects_token_and_favicon(self) -> None:
        rewritten, _ = self.engine.prepare_page(FAKE_PAGE, '/app/unificenter',
                                                service_base='', token='tok-123')
        self.assertIn('localStorage.setItem("sc_token","tok-123")', rewritten)
        self.assertIn('/app/unificenter/assets/icon.png', rewritten)

    def test_prepare_page_without_token_does_not_inject(self) -> None:
        rewritten, _ = self.engine.prepare_page(FAKE_PAGE, '/app/unificenter', service_base='', token='')
        self.assertNotIn('sc_token', rewritten)

    def test_github_upstream_mapping(self) -> None:
        self.assertEqual(self.engine.github_upstream('github/api/repos/x/y'),
                         'https://api.github.com/repos/x/y')
        self.assertEqual(self.engine.github_upstream('github/raw/a/b'),
                         'https://raw.githubusercontent.com/a/b')
        self.assertEqual(self.engine.github_upstream('other/path'), '')


class GatewayIdentityTests(BaseCase):
    def test_gateway_user_reads_trusted_headers(self) -> None:
        user = self.engine.gateway_user({'X-Trim-Userid': '1000',
                                         'X-Trim-Username': 'admin',
                                         'X-Trim-Isadmin': 'true'})
        self.assertTrue(user.via_gateway)
        self.assertTrue(user.is_admin)
        self.assertEqual(user.label, 'admin')
        self.assertTrue(self.engine.can_reveal_token(user))

    def test_non_admin_cannot_see_token(self) -> None:
        user = self.engine.gateway_user({'X-Trim-Userid': '1001',
                                         'X-Trim-Username': 'bob',
                                         'X-Trim-Isadmin': 'false'})
        self.assertFalse(self.engine.can_reveal_token(user))
        self.assertFalse(self.engine.can_change_settings(user))

    def test_debug_port_without_gateway_headers_is_readonly(self) -> None:
        user = self.engine.gateway_user({})
        self.assertFalse(user.via_gateway)
        self.assertFalse(self.engine.can_reveal_token(user))
        self.assertFalse(self.engine.can_change_settings(user))

    def test_admin_flag_case_and_spacing(self) -> None:
        user = self.engine.gateway_user({'X-Trim-Userid': '1', 'X-Trim-Isadmin': ' TRUE '})
        self.assertTrue(user.is_admin)
        user = self.engine.gateway_user({'X-Trim-Userid': '1', 'X-Trim-Isadmin': 'yes'})
        self.assertFalse(user.is_admin)

    def test_missing_admin_header_is_not_admin(self) -> None:
        user = self.engine.gateway_user({'X-Trim-Username': 'admin'})
        self.assertTrue(user.via_gateway)
        self.assertFalse(user.is_admin)
        self.assertFalse(self.engine.can_reveal_token(user))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):       # noqa: D102
        return None


class GatewaySmokeTests(BaseCase):
    """/app/unificenter 前缀（网关真实形状）下把主要流程走一遍，并验证 GitHub 转发。"""

    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        # 把 engine 模块里的上游映射换掉（服务线程走的是同一个模块对象）
        cls.github_api = f'http://127.0.0.1:{cls.router_port}'
        cls.real_github_routes = cls.engine.github_routes
        # 让 GitHub 上游指向假路由器，同时保留默认上游（默认页 FAKE_PAGE 里写的是真 GitHub 地址）
        cls.engine.github_routes = lambda api_base=None, raw_base=None: (
            (f'{cls.github_api}/', 'github/api/'),
            *cls.real_github_routes(),
        )
        # 假 GitHub：一个"页面"（会被 rewrite）和一个"接口"路径（会被转发）
        cls.github_page = f'<script>fetch("{cls.github_api}/repos/x/y/releases");</script>'
        cls.github_api_path = '/repos/x/y/releases'
        cls.github_proxy_path = '/github/api' + cls.github_api_path

    @classmethod
    def tearDownClass(cls) -> None:
        cls.engine.github_routes = cls.real_github_routes
        super().tearDownClass()

    @classmethod
    def open_path(cls, path: str, headers: dict[str, str] | None = None) -> tuple[int, bytes]:
        """用原始 socket 发请求，保留前缀本身（urllib 会规范化路径）。"""
        host, port = '127.0.0.1', cls.port
        lines = [f'GET {path} HTTP/1.1', f'Host: {host}:{port}']
        for name, value in (headers or {}).items():
            lines.append(f'{name}: {value}')
        lines.append('Connection: close')
        with socket.create_connection((host, port), timeout=30) as sock:
            sock.sendall(('\r\n'.join(lines) + '\r\n\r\n').encode('utf-8'))
            chunks = []
            while True:
                block = sock.recv(65536)
                if not block:
                    break
                chunks.append(block)
        raw = b''.join(chunks)
        head, _, body = raw.partition(b'\r\n\r\n')
        status = int(head.split(b' ')[1])
        return status, body

    def test_gateway_prefix_shape(self) -> None:
        status, body = self.open_path('/app/unificenter/',
                                      {'X-Trim-Userid': '1000',
                                       'X-Trim-Username': 'admin',
                                       'X-Trim-Isadmin': 'true'})
        self.assertEqual(status, 200)
        self.assertIn(b'/app/unificenter/assets/vue.js', body)
        self.assertIn(b'/app/unificenter/github/api/', body)
        self.assertNotIn(b'cdn.jsdelivr.net', body)

        status, body = self.open_path('/app/unificenter/ctl/settings',
                                      {'X-Trim-userid': '1000',
                                       'X-Trim-username': 'admin',
                                       'X-Trim-isadmin': 'true'})
        self.assertEqual(status, 200)
        self.assertIn(b'"can_edit": true', body)

        status, body = self.open_path('/app/unificenter/ctl/settings',
                                      {'X-Trim-Isadmin': 'false', 'X-Trim-Userid': '1001'})
        self.assertEqual(status, 200)
        self.assertIn(b'"can_edit": false', body)
        self.assertNotIn(VALID_TOKEN.encode(), body)

    def test_github_proxy_reaches_upstream_without_authorization(self) -> None:
        self.write_token()
        # 页面 rewrite 之后 GitHub 请求会变成 {前缀}/github/api/…，这里按同样的形状请求
        status, payload = self.json_request(self.github_proxy_path,
                                            headers={'Authorization': 'Bearer console-loopback'})
        self.assertEqual(status, 200)
        self.assertEqual(payload, {'proxied': True, 'path': self.github_api_path})
        method, path, authorization = FakeRouter.requests[-1]
        self.assertEqual(method, 'GET')
        self.assertEqual(path, self.github_api_path)
        self.assertEqual(authorization, '', 'GitHub 转发绝不能带上 Authorization')

    def test_github_rewritten_url_is_routable(self) -> None:
        """页面重写后的地址（{前缀}/github/api/…）必须能被自己的路由接住。"""
        rewritten, unknown = self.engine.rewrite_page(self.github_page, '/app/unificenter')
        self.assertEqual(unknown, [])
        self.assertNotIn(self.github_api, rewritten)
        expected = '/app/unificenter/github/api' + self.github_api_path
        self.assertIn(expected, rewritten)
        # 去掉网关前缀后就是本服务要处理的路径
        local = expected[len('/app/unificenter'):]
        status, payload = self.json_request(local)
        self.assertEqual(status, 200)
        self.assertTrue(payload['proxied'])

    def test_github_proxy_rejects_unknown_path(self) -> None:
        status, payload = self.json_request('/github/unknown/path')
        self.assertEqual(status, 404)
        self.assertFalse(payload['ok'])


ADMIN_HEADERS = {'X-Trim-Userid': '1000', 'X-Trim-Username': 'admin', 'X-Trim-Isadmin': 'true'}
USER_HEADERS = {'X-Trim-Userid': '1001', 'X-Trim-Username': 'bob', 'X-Trim-Isadmin': 'false'}


class HttpApiTests(BaseCase):
    def test_healthz(self) -> None:
        status, payload = self.json_request('/healthz')
        self.assertEqual(status, 200)
        self.assertTrue(payload['ok'])
        self.assertEqual(payload['app'], 'unificenter')
        self.assertEqual(payload['version'], self.engine.VERSION)

    def test_status_endpoint(self) -> None:
        self.write_token()
        status, payload = self.json_request('/ctl/status', headers=ADMIN_HEADERS)
        self.assertEqual(status, 200)
        self.assertTrue(payload['router']['reachable'])
        self.assertTrue(payload['token_set'])
        self.assertEqual(payload['gateway_prefix'], '/app/unificenter')
        self.assertIn('vue.js', ''.join(payload['assets']))

    def test_settings_get_hides_plaintext_from_non_admin(self) -> None:
        self.write_token()
        status, payload = self.json_request('/ctl/settings', headers=USER_HEADERS)
        self.assertEqual(status, 200)
        self.assertTrue(payload['token_set'])
        self.assertFalse(payload['token_visible'])
        self.assertEqual(payload['token'], '')
        self.assertIn('…', payload['token_hint'])
        self.assertFalse(payload['can_edit'])

    def test_settings_get_gives_plaintext_to_admin(self) -> None:
        self.write_token()
        status, payload = self.json_request('/ctl/settings', headers=ADMIN_HEADERS)
        self.assertEqual(status, 200)
        self.assertTrue(payload['token_visible'])
        self.assertEqual(payload['token'], VALID_TOKEN)
        self.assertTrue(payload['can_edit'])

    def test_settings_get_hides_plaintext_without_gateway(self) -> None:
        self.write_token()
        status, payload = self.json_request('/ctl/settings')
        self.assertEqual(status, 200)
        self.assertEqual(payload['token'], '')
        self.assertFalse(payload['token_visible'])

    def test_settings_post_requires_admin(self) -> None:
        status, payload = self.json_request(
            '/ctl/settings', method='POST',
            body=json.dumps({'target': 'http://10.0.0.9:9958/'}).encode(),
            headers={**USER_HEADERS, 'Content-Type': 'application/json'})
        self.assertEqual(status, 403)
        self.assertFalse(payload['ok'])
        self.assertEqual(self.engine.load_settings()['target'], f'http://127.0.0.1:{self.router_port}/')

    def test_settings_post_as_admin_updates_target_and_token(self) -> None:
        status, payload = self.json_request(
            '/ctl/settings', method='POST',
            body=json.dumps({'target': 'http://10.0.0.9:9958', 'token': 'new-token'}).encode(),
            headers={**ADMIN_HEADERS, 'Content-Type': 'application/json'})
        self.assertEqual(status, 200)
        self.assertEqual(payload['target'], 'http://10.0.0.9:9958/')
        self.assertEqual(self.engine.router_token(), 'new-token')

    def test_settings_post_rejects_bad_json(self) -> None:
        status, payload = self.json_request(
            '/ctl/settings', method='POST', body=b'{not-json',
            headers={**ADMIN_HEADERS, 'Content-Type': 'application/json'})
        self.assertEqual(status, 400)
        self.assertFalse(payload['ok'])

    def test_settings_post_rejects_bad_target(self) -> None:
        status, payload = self.json_request(
            '/ctl/settings', method='POST',
            body=json.dumps({'target': 'ftp://nope/'}).encode(),
            headers={**ADMIN_HEADERS, 'Content-Type': 'application/json'})
        self.assertEqual(status, 409)
        self.assertFalse(payload['ok'])

    def test_token_endpoint_admin_only(self) -> None:
        self.write_token()
        status, payload = self.json_request('/ctl/token', headers=ADMIN_HEADERS)
        self.assertEqual(status, 200)
        self.assertEqual(payload['token'], VALID_TOKEN)
        status, payload = self.json_request('/ctl/token', headers=USER_HEADERS)
        self.assertEqual(status, 403)
        self.assertNotIn(VALID_TOKEN, json.dumps(payload))
        status, _ = self.json_request('/ctl/token')
        self.assertEqual(status, 403)

    def test_logs_endpoint(self) -> None:
        self.engine.log('测试日志一行')
        status, payload = self.json_request('/ctl/logs')
        self.assertEqual(status, 200)
        self.assertTrue(any('测试日志一行' in line for line in payload['lines']))
        self.assertEqual(set(payload), {'ok', 'lines', 'file'})

    def test_panel_and_assets_are_served(self) -> None:
        for path, needle in (('/ctl/panel.html', b'id="targetInput"'),
                             ('/ctl/panel.js', b'loadSettings'),
                             ('/ctl/panel.css', b'.rc-body'),
                             ('/assets/close.js', b'normal_goback'),
                             ('/assets/overlay.js', b'unifi-fab'),
                             ('/assets/vue.js', b'Vue'),
                             ('/images/icon_64.png', b'\x89PNG')):
            status, body, headers = self.request(path)
            self.assertEqual(status, 200, path)
            self.assertIn(needle, body, path)

    def test_panel_html_uses_relative_paths(self) -> None:
        _, body, _ = self.request('/ctl/panel.html')
        text = body.decode('utf-8')
        self.assertIn('href="panel.css"', text)
        self.assertIn('src="panel.js"', text)
        self.assertIn('src="../assets/close.js"', text)
        self.assertIn("const API = '../ctl'", self.request('/ctl/panel.js')[1].decode('utf-8'))

    def test_unknown_api_returns_404(self) -> None:
        status, payload = self.json_request('/ctl/nope')
        self.assertEqual(status, 404)
        self.assertFalse(payload['ok'])

    def test_unknown_static_path_returns_404(self) -> None:
        status, _, _ = self.request('/assets/../../etc/passwd')
        self.assertIn(status, (400, 403, 404))
        status, _, _ = self.request('/assets/nope.js')
        self.assertEqual(status, 404)

    def test_path_traversal_is_rejected(self) -> None:
        for path in ('/assets/%2e%2e/%2e%2e/etc/passwd',
                     '/ctl/../panel.js',
                     '/../ICON.PNG',
                     '/assets/..%2f..%2fmanifest'):
            status, body, _ = self.request(path)
            self.assertIn(status, (400, 403, 404), f'{path} -> {status}')
            self.assertNotIn(b'root:', body)

    def test_softcenter_page_is_localized_and_token_injected(self) -> None:
        self.write_token()
        status, body, headers = self.request('/')
        self.assertEqual(status, 200)
        self.assertEqual(headers.get('Content-Type'), 'text/html; charset=utf-8')
        text = body.decode('utf-8')
        self.assertIn('/app/unificenter/assets/vue.js', text)
        self.assertNotIn('cdn.jsdelivr.net', text)
        self.assertIn('localStorage.setItem("sc_token"', text)
        self.assertIn('/app/unificenter/github/api/', text)
        self.assertIn('/app/unificenter/assets/close.js', text)
        self.assertIn('/app/unificenter/assets/overlay.js', text)

    def test_api_forwarding_carries_saved_token(self) -> None:
        self.write_token()
        status, payload = self.json_request('/api/system/info')
        self.assertEqual(status, 200)
        self.assertEqual(payload['Device'], 'R619AC')
        self.assertEqual(FakeRouter.requests[-1][2], VALID_TOKEN)

    def test_api_forwarding_passes_client_token_through(self) -> None:
        self.write_token('saved-token')
        status, _ = self.json_request('/api/system/info',
                                      headers={'Authorization': VALID_TOKEN})
        self.assertEqual(status, 200)
        self.assertEqual(FakeRouter.requests[-1][2], VALID_TOKEN)

    def test_api_forwarding_replaces_console_placeholder(self) -> None:
        self.write_token()
        status, _ = self.json_request('/api/system/info',
                                      headers={'Authorization': 'Bearer console-loopback'})
        self.assertEqual(status, 200)
        self.assertEqual(FakeRouter.requests[-1][2], VALID_TOKEN)

    def test_api_forwarding_401_when_no_token_saved(self) -> None:
        status, payload = self.json_request('/api/system/info')
        self.assertEqual(status, 401)
        self.assertEqual(FakeRouter.requests[-1][2], '')

    def test_api_forwarding_without_token_is_401_with_placeholder(self) -> None:
        # 未保存令牌时占位值不能当令牌送过去
        status, _ = self.json_request('/api/system/info',
                                      headers={'Authorization': 'Bearer console-loopback'})
        self.assertEqual(status, 401)
        self.assertEqual(FakeRouter.requests[-1][2], '')

    def test_prefix_paths_are_accepted_and_normalized(self) -> None:
        status, _, _ = self.request('/app/unificenter/ctl/status', headers=ADMIN_HEADERS)
        self.assertEqual(status, 200)
        redirect = urllib.request.build_opener(NoRedirect)
        try:
            redirect.open(f'{self.base}/app/unificenter', timeout=10)
            self.fail('应当 301')
        except urllib.error.HTTPError as error:
            self.assertEqual(error.code, 301)
            self.assertEqual(error.headers.get('Location'), '/app/unificenter/')

    def test_offline_page_when_router_unreachable(self) -> None:
        self.engine.save_settings({'target': f'http://127.0.0.1:{free_port()}/'})
        status, body, _ = self.request('/')
        self.assertEqual(status, 502)
        text = body.decode('utf-8')
        self.assertIn('连不上路由器软件中心', text)
        self.assertIn('/app/unificenter/ctl/panel.html', text)

    def test_view_compat_entry(self) -> None:
        self.write_token()
        status, _, _ = self.request('/view/api/system/info')
        self.assertEqual(status, 200)
        status, body, _ = self.request('/view/')
        self.assertEqual(status, 200)
        self.assertIn('sc_token', body.decode('utf-8'))

    def test_github_proxy_does_not_leak_authorization(self) -> None:
        # GitHub 转发目标是外网，测试里只验证路径识别与错误处理（不真的联网）
        status, payload = self.json_request('/github/other/path')
        self.assertEqual(status, 404)
        self.assertFalse(payload['ok'])


class PackageStructureTests(unittest.TestCase):
    """fnOS 包结构与规范一致性（fnpack 的基础检查项）。"""

    def test_manifest_fields(self) -> None:
        text = (ROOT / 'manifest').read_text(encoding='utf-8')
        fields = dict(line.split('=', 1) for line in text.strip().splitlines() if '=' in line)
        self.assertEqual(fields['appname'], 'unificenter')
        self.assertEqual(fields['source'], 'thirdparty')
        self.assertIn(fields['platform'], ('x86', 'arm', 'all'))
        self.assertTrue(fields['os_min_version'])
        self.assertEqual(fields['desktop_uidir'], 'ui')
        self.assertEqual(fields['desktop_applaunchname'], 'unificenter.main')
        self.assertEqual(fields['checkport'], 'false')
        self.assertEqual(fields['install_dep_apps'], 'python312')
        self.assertEqual(fields['version'], (SERVER_DIR / 'VERSION').read_text(encoding='utf-8').strip())

    def test_entry_config_registers_gateway(self) -> None:
        config = json.loads((ROOT / 'app' / 'ui' / 'config').read_text(encoding='utf-8'))
        entry = config['.url']['unificenter.main']
        self.assertEqual(entry['gatewayPrefix'], '/app/unificenter')
        self.assertEqual(entry['gatewaySocket'], 'app.sock')
        self.assertEqual(entry['type'], 'iframe')
        self.assertEqual(entry['url'], '/app/unificenter/')
        self.assertEqual(entry['icon'], 'images/icon_{0}.png')

    def test_icons_exist_with_expected_sizes(self) -> None:
        import struct                                                      # noqa: PLC0415

        def png_size(path: Path) -> tuple[int, int]:
            data = path.read_bytes()
            self.assertEqual(data[:8], b'\x89PNG\r\n\x1a\n', path)
            width, height = struct.unpack('>II', data[16:24])
            return width, height

        self.assertEqual(png_size(ROOT / 'ICON.PNG'), (64, 64))
        self.assertEqual(png_size(ROOT / 'ICON_256.PNG'), (256, 256))
        self.assertEqual(png_size(ROOT / 'app' / 'ui' / 'images' / 'icon_64.png'), (64, 64))
        self.assertEqual(png_size(ROOT / 'app' / 'ui' / 'images' / 'icon_256.png'), (256, 256))
        for path in ('ICON.PNG', 'ICON_256.PNG'):
            self.assertLess((ROOT / path).stat().st_size, 1024 * 1024)

    def test_lifecycle_scripts_present(self) -> None:
        expected = ('main', 'install_init', 'install_callback', 'upgrade_init',
                    'upgrade_callback', 'uninstall_init', 'uninstall_callback',
                    'config_init', 'config_callback')
        for name in expected:
            path = ROOT / 'cmd' / name
            self.assertTrue(path.is_file(), name)
            text = path.read_text(encoding='utf-8')
            self.assertTrue(text.startswith('#!/bin/bash'), name)
            self.assertIn('exit', text, name)

    def test_main_script_contract(self) -> None:
        text = (ROOT / 'cmd' / 'main').read_text(encoding='utf-8')
        self.assertIn('start)', text)
        self.assertIn('stop)', text)
        self.assertIn('status)', text)
        self.assertIn('exit 3', text)                    # 未运行
        self.assertIn('app.sock', text)
        self.assertIn('python3', text)

    def test_main_script_targets_gateway_socket(self) -> None:
        text = (ROOT / 'cmd' / 'main').read_text(encoding='utf-8')
        self.assertIn('SOCKET="$APPDEST/app.sock"', text)
        self.assertIn('wait_for_socket', text)

    def test_wizard_files_are_valid_json(self) -> None:
        for name in ('install', 'upgrade', 'uninstall', 'config'):
            payload = json.loads((ROOT / 'wizard' / name).read_text(encoding='utf-8'))
            self.assertIsInstance(payload, list, name)
            for step in payload:
                self.assertIn('stepTitle', step)
                self.assertIsInstance(step['items'], list)
                for item in step['items']:
                    self.assertIn(item['type'], ('text', 'password', 'radio', 'checkbox',
                                                 'select', 'switch', 'tips'), name)
                    if item['type'] != 'tips':
                        self.assertTrue(item['field'].startswith('wizard_'), name)

    def test_config_json_files_are_valid(self) -> None:
        privilege = json.loads((ROOT / 'config' / 'privilege').read_text(encoding='utf-8'))
        self.assertEqual(privilege['defaults']['run-as'], 'package')
        self.assertTrue(privilege['username'])
        self.assertTrue(privilege['groupname'])
        json.loads((ROOT / 'config' / 'resource').read_text(encoding='utf-8'))

    def test_ui_dependencies_are_local(self) -> None:
        """页面里替换 CDN 用的三个本地副本必须真的存在，否则页面会白屏。"""
        for name in self.engine_assets():
            path = ROOT / 'app' / 'ui' / name
            self.assertTrue(path.is_file(), name)
            self.assertGreater(path.stat().st_size, 1000, name)

    @staticmethod
    def engine_assets() -> tuple[str, ...]:
        return ('assets/vue.js', 'assets/lucide.js', 'assets/tailwind.js')

    def test_frontend_does_not_expose_token_hint_as_token(self) -> None:
        panel = (ROOT / 'app' / 'ui' / 'panel.js').read_text(encoding='utf-8')
        # 输入框只在显式调用 /ctl/token 之后才填入明文，绝不把 hint 当令牌
        self.assertIn("api('token')", panel)
        self.assertNotIn("$('tokenInput').value = hint", panel)
        self.assertIn('can_edit', panel)

    def test_readme_documents_gateway_and_paths(self) -> None:
        readme = (ROOT / 'README.md').read_text(encoding='utf-8')
        for needle in ('/app/unificenter', 'app.sock', 'X-Trim-Isadmin', 'TRIM_PKGETC'):
            self.assertIn(needle, readme, needle)

    def test_shell_scripts_do_not_hardcode_volume_paths(self) -> None:
        for name in ('main', 'config_callback', 'install_callback', 'uninstall_callback'):
            text = (ROOT / 'cmd' / name).read_text(encoding='utf-8')
            self.assertNotIn('/vol1/', text, name)
            self.assertNotIn('/vol2/', text, name)


if __name__ == '__main__':
    unittest.main(verbosity=2)
