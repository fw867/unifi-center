"""路由器软件中心（fnOS 移植版）：把局域网里路由器上的软件中心反代到飞牛 fnOS 的桌面入口。

移植来源：xiaomi-router-center（小米智能存储插件），保留的能力：
  * 软件中心页面取回后本地化（Vue / Tailwind / Lucide 三个 CDN 换成本地副本）；
  * 页面里对 GitHub 的直连改走 NAS 转发（fnOS 桌面窗口里同样访问不到外网）；
  * AdminToken 存在 NAS 上（0600），打开软件中心时自动注入 localStorage，不必每次输入；
  * 转发到路由器时按规则决定 Authorization（真实令牌原样透传、占位值换成保存的令牌）。

相对原版的改动（fnOS 运行模型）：
  * 不再自己渲染 nginx 配置文件：入口交给 fnOS「统一网关」，服务监听 target/app.sock，
    路由 /app/unificenter/… ⇒ 本进程（见 docs/core-concepts/gateway-registration）；
  * 身份不再来自小米客户端证书，而是网关透传的 X-Trim-Userid / X-Trim-Isadmin / X-Trim-Username：
    网关只保证「已登录」，因此明文 AdminToken 只对管理员开放（engine.can_reveal_token）；
  * 路径全部走 TRIM_* 环境变量，不硬编码卷与安装目录；
  * 纯标准库实现，目标机不需要 pip（fnOS 自带 python3）。

不信任任何客户端输入：网关用户头只由 fnOS 注入，请求里的同名头会被忽略（见 server 侧只读
网关值的方式 —— 服务只读自己收到的头，且只在网关转发路径上使用，独立端口调试模式下降级为
「非管理员」）。
"""

from __future__ import annotations

import json
import os
import re
import socket
import stat
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

# ---------------------------------------------------------------------------
# 常量与路径（全部可被环境变量覆盖，便于开发和测试）
# ---------------------------------------------------------------------------

APP_NAME = os.environ.get('TRIM_APPNAME') or 'unificenter'
APP_DIR = Path(__file__).resolve().parent            # app/server/unifi_center
DEFAULT_UI_DIR = APP_DIR.parent.parent / 'ui'        # app/ui

GATEWAY_PREFIX = os.environ.get('GATEWAY_PREFIX', f'/app/{APP_NAME}')
SOCKET_NAME = os.environ.get('GATEWAY_SOCKET', 'app.sock')

#: 开发/测试时的兜底目录（目标机上 TRIM_PKGETC 一定存在）
DEV_CONFIG_DIR = APP_DIR.parent.parent.parent / '.dev' / 'config'


def _config_dir() -> Path:
    """应用配置目录：优先 TRIM_PKGETC，其次 TRIM_PKGVAR，最后开发用兜底。"""
    for name in ('TRIM_PKGETC', 'TRIM_PKGVAR'):
        value = (os.environ.get(name) or '').strip()
        if value:
            return Path(value)
    return DEV_CONFIG_DIR


CONFIG_DIR = _config_dir()
STATE_FILE = Path(os.environ.get('STATE_FILE') or (CONFIG_DIR / 'settings.json'))
TOKEN_FILE = Path(os.environ.get('TOKEN_FILE') or (CONFIG_DIR / 'router-token'))
LOG_FILE = Path(os.environ.get('LOG_FILE') or (CONFIG_DIR / 'unifi-center.log'))
DEFAULT_TARGET = os.environ.get('ROUTER_TARGET', 'http://192.168.1.1:9958/')
PROBE_TIMEOUT = float(os.environ.get('PROBE_TIMEOUT', '6'))
MAX_LOG_BYTES = int(os.environ.get('MAX_LOG_BYTES', str(512 * 1024)))

_toggle_lock = threading.Lock()
_log_lock = threading.Lock()


# ---------------------------------------------------------------------------
# 日志：写应用自己的日志文件（stdout 也会输出一份，便于 appcenter-cli 观察）
# ---------------------------------------------------------------------------

def log(message: str) -> None:
    stamp = time.strftime('%Y-%m-%d %H:%M:%S')
    line = f'[{stamp}] {message}'
    print(line, flush=True)
    try:
        with _log_lock:
            LOG_FILE.parent.mkdir(parents=True, exist_ok=True)
            if LOG_FILE.is_file() and LOG_FILE.stat().st_size > MAX_LOG_BYTES:
                LOG_FILE.replace(LOG_FILE.with_suffix('.log.1'))
            with LOG_FILE.open('a', encoding='utf-8') as handle:
                handle.write(line + '\n')
    except OSError:
        pass


def read_tail(path: Path | None = None, limit: int = 200) -> list[str]:
    """读日志尾部若干行（设置面板里的"最近日志"）。"""
    target = Path(path or LOG_FILE)
    try:
        lines = target.read_text(encoding='utf-8', errors='replace').splitlines()
    except OSError:
        return []
    return lines[-limit:]


# ---------------------------------------------------------------------------
# 版本号：VERSION 文件 > 已安装的 manifest
# ---------------------------------------------------------------------------

def _read_version_file() -> str:
    for candidate in (APP_DIR / 'VERSION', APP_DIR.parent / 'VERSION'):
        try:
            value = candidate.read_text(encoding='utf-8').strip()
        except OSError:
            continue
        if value:
            return value
    return ''


def _read_manifest_version() -> str:
    """从已安装的 manifest 里取 version（target 旁边还有一份 /var/apps/<appname>/manifest）。"""
    for candidate in (APP_DIR.parent / 'manifest',
                      Path('/var/apps') / APP_NAME / 'manifest'):
        try:
            text = candidate.read_text(encoding='utf-8', errors='replace')
        except OSError:
            continue
        match = re.search(r'^version=(.+)$', text, re.MULTILINE)
        if match:
            return match.group(1).strip()
    return ''


VERSION = _read_version_file() or _read_manifest_version()


# ---------------------------------------------------------------------------
# 设置（目标地址 + 路由器令牌）
# ---------------------------------------------------------------------------


def atomic_write(path: Path, text: str, mode: int = 0o644) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.tmp')
    temporary.write_text(text, encoding='utf-8')
    try:
        os.chmod(temporary, mode)
    except OSError:
        pass
    temporary.replace(path)
    try:
        os.chmod(path, mode)
    except OSError:
        pass


def normalize_target(raw: str) -> str:
    """把用户填的地址规整成 http://host:port/ 形式（必须以 / 结尾，反代才好拼路径）。"""
    value = (raw or '').strip()
    if not value:
        raise RuntimeError('目标地址不能为空')
    if '://' not in value:
        value = 'http://' + value
    match = re.match(r'^(https?)://([^/]+)(/.*)?$', value, re.IGNORECASE)
    if not match:
        raise RuntimeError('目标地址格式不对，示例：http://192.168.1.1:9958/')
    scheme, authority, _path = match.groups()
    if not re.match(r'^[A-Za-z0-9.\-]+(:\d{1,5})?$', authority):
        raise RuntimeError('目标地址的主机名/端口不合法')
    if authority.endswith(':') or (':' in authority and not re.match(r'^[^:]+:\d{1,5}$', authority)):
        raise RuntimeError('目标地址的端口不合法')
    return f'{scheme.lower()}://{authority}/'


def load_settings() -> dict[str, Any]:
    try:
        payload = json.loads(STATE_FILE.read_text(encoding='utf-8'))
    except (OSError, json.JSONDecodeError):
        payload = {}
    settings = payload if isinstance(payload, dict) else {}
    raw_target = str(settings.get('target') or '').strip() or DEFAULT_TARGET
    try:
        target = normalize_target(raw_target)
    except RuntimeError:
        target = normalize_target(DEFAULT_TARGET)
    return {'target': target}


def save_settings(values: dict[str, Any]) -> dict[str, Any]:
    settings = load_settings()
    if 'target' in values:
        settings['target'] = normalize_target(str(values.get('target') or ''))
    atomic_write(STATE_FILE, json.dumps(settings, ensure_ascii=False, indent=2) + '\n')
    return settings


def apply_target(target: str) -> dict[str, Any]:
    """保存目标地址（路由器接口由本服务转发，改地址不涉及任何系统配置改动）。"""
    normalized = normalize_target(target)
    with _toggle_lock:
        settings = save_settings({'target': normalized})
    log(f'目标地址已更新为 {normalized}')
    return settings


def router_token() -> str:
    try:
        return TOKEN_FILE.read_text(encoding='utf-8').strip()
    except OSError:
        return ''


def token_hint(token: str | None = None) -> str:
    """令牌的展示提示（绝不等于令牌本身）：够长就"前 4 位…后 2 位"，否则只说"已设置"。"""
    value = router_token() if token is None else (token or '')
    if not value:
        return ''
    return (value[:4] + '…' + value[-2:]) if len(value) >= 6 else '已设置'


def save_router_token(token: str) -> None:
    token = (token or '').strip()
    if not token:
        TOKEN_FILE.unlink(missing_ok=True)
        log('已清除保存的 AdminToken')
        return
    atomic_write(TOKEN_FILE, token + '\n', mode=0o600)
    log(f'已保存 AdminToken（{token_hint(token)}，文件权限 0600）')


# ---------------------------------------------------------------------------
# 网关身份：网关注入的 X-Trim-* 是唯一可信来源
#   注意：本服务只读「自己收到的头」。fnOS 网关在转发前会覆写这三个头，
#   客户端伪造的 X-Trim-Isadmin 到不了这里（网关先校验会话再转发）。
# ---------------------------------------------------------------------------


class GatewayUser:
    __slots__ = ('uid', 'username', 'is_admin', 'via_gateway')

    def __init__(self, uid: str = '', username: str = '', is_admin: bool = False,
                 via_gateway: bool = False) -> None:
        self.uid = uid
        self.username = username
        self.is_admin = is_admin
        self.via_gateway = via_gateway

    @property
    def label(self) -> str:
        if not self.via_gateway:
            return '本机调试'
        return self.username or (f'uid {self.uid}' if self.uid else '未知用户')

    def as_dict(self) -> dict[str, Any]:
        return {'uid': self.uid, 'username': self.username,
                'is_admin': self.is_admin, 'via_gateway': self.via_gateway,
                'label': self.label}


def gateway_user(headers: Any) -> GatewayUser:
    """从网关头构造可信用户上下文。

    独立端口（本机调试）模式下网关头不存在，一律当成「非管理员 + 未登录」，
    这样调试模式也拿不到明文令牌，行为与线上一致。
    """
    get = headers.get
    uid = (get('X-Trim-Userid') or '').strip()
    username = (get('X-Trim-Username') or '').strip()
    is_admin = (get('X-Trim-Isadmin') or '').strip().lower() == 'true'
    # 网关转发时这三个头一定有一份（uid / username 至少其一）；独立端口调试时全都没有。
    via_gateway = bool(uid or username)
    return GatewayUser(uid=uid, username=username, is_admin=is_admin, via_gateway=via_gateway)


def can_reveal_token(user: GatewayUser) -> bool:
    """明文 AdminToken 只给「已登录的管理员」；其他情况只给提示（失败关闭）。

    原版（小米客户端）用客户端证书判定"设备所有者"；fnOS 上没有那套证书，
    改用统一网关的 X-Trim-Isadmin —— 网关保证这个头不可伪造。
    """
    return bool(user.via_gateway and user.is_admin)


def can_change_settings(user: GatewayUser) -> bool:
    """改地址/换令牌：同样只允许管理员（普通用户只读）。"""
    return can_reveal_token(user)


# ---------------------------------------------------------------------------
# 转发时的 Authorization 决策
#
# 背景（沿用原版，实测报障）：电脑端「控制台」那一跳会无条件写
# `proxy_set_header Authorization "Bearer console-loopback"`，把客户端真实凭据覆盖成占位值；
# 那个值送到路由器必然是"令牌不正确"。所以转发前要认得出占位值，换成插件自己保存的令牌；
# 认不出来（客户端自己带的真实令牌）必须原样透传，绝不覆盖。
# ---------------------------------------------------------------------------

CONSOLE_LOOPBACK_TOKEN = 'console-loopback'
CONSOLE_PLACEHOLDER_AUTHORIZATION = f'Bearer {CONSOLE_LOOPBACK_TOKEN}'


def is_console_placeholder_authorization(value: str) -> bool:
    """判断 Authorization 是不是控制台那一跳塞进来的占位值。

    容错：授权方案大小写、Bearer 前后多写的空格、只剩裸值（没有方案）都认。
    """
    raw = (value or '').strip()
    if not raw:
        return False
    scheme, separator, credentials = raw.partition(' ')
    if separator and scheme.strip().lower() != 'bearer':
        return False
    return (credentials if separator else raw).strip() == CONSOLE_LOOPBACK_TOKEN


def resolve_authorization(value: str) -> str:
    """返回真正要转发给路由器的 Authorization；空串表示这一跳不带 Authorization。

    规则：
      * 客户端自己带了真实凭据（页面里手输的、App 注入的）→ 原样返回，不动；
      * 没带凭据 → 用本应用保存的令牌顶上（本地服务用自己的凭据，就不再 401）；
      * 带的是控制台那一跳的占位值 → 也当"没带"，换成保存的令牌；
      * 该顶上却没有保存令牌时返回空串（宁可不带，也别把占位值当令牌送去，否则软件中心
        只能报"令牌不正确"，看不出是哪一环的问题），并写日志说明下一步该做什么。
    """
    client_value = (value or '').strip()
    if client_value and not is_console_placeholder_authorization(client_value):
        return client_value
    saved = router_token()
    if saved:
        if client_value:
            log('收到控制台占位 Authorization，改用本应用保存的路由器令牌转发')
        else:
            log('请求里没有 Authorization，改用本应用保存的路由器令牌转发')
        return saved
    if client_value:
        log(f'收到控制台占位 Authorization（{CONSOLE_PLACEHOLDER_AUTHORIZATION}），'
            '但还没保存路由器令牌：本次转发不带 Authorization，路由器会回 401；'
            '请在设置面板填入 AdminToken 后保存')
    else:
        log('请求里没有 Authorization，也没保存路由器令牌：本次转发不带 Authorization，'
            '路由器会回 401；请在设置面板填入 AdminToken 后保存')
    return ''


# ---------------------------------------------------------------------------
# 探测路由器软件中心（只读：两个 GET，不改动路由器上任何东西）
# ---------------------------------------------------------------------------


def _user_agent() -> str:
    return f'FnOsUnifiCenter/{VERSION or "?"}'


def probe_router(target: str | None = None, token: str | None = None) -> dict[str, Any]:
    base = normalize_target(target or load_settings()['target'])
    token = router_token() if token is None else token
    result: dict[str, Any] = {
        'target': base,
        'reachable': False,
        'status': 0,
        'latency_ms': None,
        'title': '',
        'server': '',
        'authenticated': False,
        'device': '',
        'version': '',
        'error': '',
        'at': int(time.time()),
    }
    started = time.time()
    body = ''
    try:
        request = urllib.request.Request(base, headers={'User-Agent': _user_agent()})
        with urllib.request.urlopen(request, timeout=PROBE_TIMEOUT) as response:
            body = response.read(4096).decode('utf-8', 'replace')
            result['status'] = response.status
            result['reachable'] = response.status == 200
            result['server'] = (response.headers.get('Server') or '')
    except urllib.error.HTTPError as error:
        result['status'] = error.code
        result['reachable'] = error.code < 500
        result['error'] = f'HTTP {error.code}'
    except Exception as error:                                     # noqa: BLE001
        result['error'] = str(error) or error.__class__.__name__
    result['latency_ms'] = round((time.time() - started) * 1000, 1)

    match = re.search(r'<title>(.*?)</title>', body, re.IGNORECASE | re.DOTALL)
    if match:
        result['title'] = match.group(1).strip()[:80]

    if result['reachable'] and token:
        try:
            info_request = urllib.request.Request(
                base + 'api/system/info',
                headers={'Authorization': token, 'User-Agent': _user_agent()})
            with urllib.request.urlopen(info_request, timeout=PROBE_TIMEOUT) as response:
                payload = json.loads(response.read() or b'{}')
            result['authenticated'] = True
            if isinstance(payload, dict):
                result['device'] = str(payload.get('Device') or payload.get('device') or '')
                result['version'] = str(payload.get('Version') or payload.get('version') or '')
        except urllib.error.HTTPError as error:
            result['authenticated'] = error.code != 401
            if error.code == 401:
                result['error'] = result['error'] or '令牌无效'
        except Exception as error:                                 # noqa: BLE001
            result['error'] = result['error'] or str(error)
    return result


def fetch_router_page(target: str | None = None) -> str:
    """取软件中心首页 HTML（只读）。"""
    base = normalize_target(target or load_settings()['target'])
    request = urllib.request.Request(base, headers={
        'User-Agent': _user_agent(),
        'Accept': 'text/html,application/xhtml+xml',
        'Accept-Language': 'zh-CN,zh;q=0.9',
    })
    with urllib.request.urlopen(request, timeout=PROBE_TIMEOUT * 2) as response:
        return response.read().decode('utf-8', 'replace')


# ---------------------------------------------------------------------------
# 页面重写
#
# 为什么需要（原版踩过的坑，fnOS 桌面窗口里同样成立）：软件中心前端从 jsdelivr /
# tailwindcss.com 加载 Vue、Tailwind、Lucide，桌面窗口的内嵌 webview 常常只允许访问
# NAS 自己的域名，外部资源被拦就是一片空白。所以把页面取回来、把三个 CDN 换成本地副本，
# 再把直连 GitHub 的地址改走 NAS 转发，整条链路就只剩 NAS 一个域名。
# ---------------------------------------------------------------------------

ASSET_ROUTES: tuple[tuple[str, str], ...] = (
    ('cdn.tailwindcss.com', 'assets/tailwind.js'),
    ('cdn.jsdelivr.net/npm/vue@', 'assets/vue.js'),
    ('cdn.jsdelivr.net/npm/lucide@', 'assets/lucide.js'),
)
SCRIPT_SRC_PATTERN = re.compile(r'(<script[^>]*\bsrc=")(https?://[^"]+)(")', re.IGNORECASE)
LOCAL_ASSETS = ('assets/vue.js', 'assets/lucide.js', 'assets/tailwind.js')


def ui_dir() -> Path:
    """前端目录：优先 TRIM_APPDEST/ui（安装后），其次包内 app/ui（开发）。"""
    explicit = (os.environ.get('UI_DIR') or '').strip()
    candidates: list[Path] = []
    if explicit:
        candidates.append(Path(explicit))
    dest = (os.environ.get('TRIM_APPDEST') or '').strip()
    if dest:
        candidates.append(Path(dest) / 'ui')
    candidates.append(DEFAULT_UI_DIR)
    for candidate in candidates:
        if (candidate / 'panel.html').is_file():
            return candidate
    return candidates[-1]


def assets_present(static_dir: Path | None = None) -> dict[str, bool]:
    """本地 CDN 副本是否齐全（缺失时页面会退回直连模式，不至于白屏）。"""
    root = Path(static_dir or ui_dir())
    return {name: (root / name).is_file() for name in LOCAL_ASSETS}


def rewrite_page(html: str, prefix: str, service_base: str = '') -> tuple[str, list[str]]:
    """把页面里的 CDN 脚本与 GitHub 直连地址换成本应用的路径。

    * 本地资源（Vue/Tailwind/Lucide）由本服务的 /assets/ 提供 → {prefix}/assets/…
    * GitHub 转发也走本服务 → {service_base}/github/…
    """
    base = '/' + prefix.strip('/') if prefix.strip('/') else ''
    service = '/' + service_base.strip('/') if service_base.strip('/') else base
    unknown: list[str] = []

    def replace(match: re.Match[str]) -> str:
        head, url, tail = match.groups()
        for marker, local in ASSET_ROUTES:
            if marker in url:
                return f'{head}{base}/{local}{tail}'
        unknown.append(url)
        return match.group(0)

    rewritten = SCRIPT_SRC_PATTERN.sub(replace, html)
    for upstream, local in github_routes():
        rewritten = rewritten.replace(upstream, f'{service}/{local}')
    return rewritten, unknown


def prepare_page(html: str, prefix: str, service_base: str, token: str = '') -> tuple[str, list[str]]:
    """把软件中心页面加工成"能直接在 fnOS 桌面里打开"的版本：

    1. 外部 CDN（Vue/Tailwind/Lucide）换成本地副本；
    2. 页面里直连 GitHub 的地址改走本服务的转发；
    3. 已保存的 AdminToken 直接注入 localStorage（同源，软件中心一加载就通过鉴权）；
    4. 补一个本地 favicon（否则桌面窗口会去请求站点根目录的 /favicon.ico 报 404）。
    """
    rewritten, unknown = rewrite_page(html, prefix, service_base)
    head = f'<link rel="icon" href="{prefix}/assets/icon.png">'
    rewritten = rewritten.replace('</head>', head + '</head>', 1) if '</head>' in rewritten else head + rewritten
    if token:
        payload = json.dumps(token)
        rewritten = rewritten.replace(
            '</head>',
            f'<script>try{{localStorage.setItem("sc_token",{payload});}}catch(e){{}}</script></head>', 1)
    return rewritten, unknown


GITHUB_API_DEFAULT = 'https://api.github.com'
GITHUB_RAW_DEFAULT = 'https://raw.githubusercontent.com'


def github_routes(api_base: str | None = None, raw_base: str | None = None) -> tuple[tuple[str, str], ...]:
    """GitHub 上游地址 → 本应用内路径的映射。

    上游可用 GITHUB_API_BASE / GITHUB_RAW_BASE 覆盖（测试、私有镜像）；无论用哪一种，
    另一套默认地址也会保留，这样页面里写死的两种写法都能被替换掉。
    每个上游给出"带尾斜杠""不带尾斜杠"两条，因为页面里两种写法都出现过。
    """
    bases = {
        (api_base if api_base is not None
         else os.environ.get('GITHUB_API_BASE') or GITHUB_API_DEFAULT).rstrip('/'): 'github/api/',
        (raw_base if raw_base is not None
         else os.environ.get('GITHUB_RAW_BASE') or GITHUB_RAW_DEFAULT).rstrip('/'): 'github/raw/',
    }
    bases.setdefault(GITHUB_API_DEFAULT, 'github/api/')
    bases.setdefault(GITHUB_RAW_DEFAULT, 'github/raw/')
    routes: list[tuple[str, str]] = []
    for base, local in bases.items():
        if not base:
            continue
        routes.append((base + '/', local))
        routes.append((base, local))
    return tuple(routes)


def github_upstream(rest: str) -> str:
    """把 `github/raw/…`、`github/api/…` 映射回真正的 GitHub 地址（不认识就返回空串）。"""
    path = rest.lstrip('/')
    for upstream, local in github_routes():
        if path.startswith(local):
            return upstream + path[len(local):]
    return ''


# ---------------------------------------------------------------------------
# Unix Socket（统一网关目标）
# ---------------------------------------------------------------------------


def socket_path() -> Path:
    """网关转发目标：$TRIM_APPDEST/app.sock（与 ui/config 的 gatewaySocket 一致）。"""
    explicit = (os.environ.get('SOCKET_PATH') or '').strip()
    if explicit:
        return Path(explicit)
    dest = (os.environ.get('TRIM_APPDEST') or '').strip()
    if dest:
        return Path(dest) / SOCKET_NAME
    return APP_DIR.parent.parent.parent / '.dev' / SOCKET_NAME


def socket_ready() -> bool:
    path = socket_path()
    try:
        return stat.S_ISSOCK(path.stat().st_mode)
    except OSError:
        return False


def unlink_stale_socket(path: Path | None = None) -> bool:
    """清理陈旧的 socket 文件（进程崩了会留下，bind 会 EADDRINUSE）。"""
    target = Path(path or socket_path())
    if not target.exists():
        return False
    try:
        probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            probe.settimeout(0.4)
            probe.connect(str(target))
            return False                       # 还连得上，说明有活着的服务
        except OSError:
            target.unlink(missing_ok=True)
            return True
        finally:
            probe.close()
    except OSError:
        return False


def status_payload(port: int | None = None, user: GatewayUser | None = None,
                   static_dir: Path | None = None) -> dict[str, Any]:
    settings = load_settings()
    token = router_token()
    root = Path(static_dir or ui_dir())
    return {
        'app': APP_NAME,
        'version': VERSION,
        'target': settings['target'],
        'default_target': DEFAULT_TARGET,
        'token_set': bool(token),
        'token_hint': token_hint(token),
        'router': probe_router(settings['target']),
        'gateway_prefix': GATEWAY_PREFIX,
        'socket_path': str(socket_path()),
        'socket_ready': socket_ready(),
        'ui_dir': str(root),
        'assets': assets_present(root),
        'config_dir': str(CONFIG_DIR),
        'user': (user or GatewayUser()).as_dict(),
        'port': port,
        'at': int(time.time()),
    }


if __name__ == '__main__':                                          # 手工自检用
    print(json.dumps(status_payload(), ensure_ascii=False, indent=2))
    sys.exit(0)
