#!/usr/bin/env python3
"""把安装/配置向导里填的值写入应用配置目录（幂等，可重复执行）。

字段来源是向导同名的环境变量（不带 TRIM_ 前缀）：
    wizard_router_target   路由器软件中心地址，例如 http://192.168.1.1:9958/
    wizard_router_token    AdminToken；留空表示"保持原值不变"
    wizard_purge_data      仅卸载向导使用，本脚本不处理

另外支持 --migrate-legacy：从旧的小米插件目录（/data/plugin/router-center）搬一次设置，
只读旧目录、不删旧数据，方便从 xiaomi-router-center 平滑迁移过来。

退出码：0 成功；1 值非法或写入失败（调用方会把错误写进 TRIM_TEMP_LOGFILE）。
"""

from __future__ import annotations

import json
import os
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import engine                                                              # noqa: E402

LEGACY_DIRS = (
    Path(os.environ.get('LEGACY_PLUGIN_DIR') or '/data/plugin/router-center'),
)


def config_dir() -> Path:
    for name in ('TRIM_PKGETC', 'TRIM_PKGVAR'):
        value = (os.environ.get(name) or '').strip()
        if value:
            return Path(value)
    return engine.CONFIG_DIR


def write_line(message: str) -> None:
    print(f'[unifi-center] {message}')
    logfile = (os.environ.get('TRIM_TEMP_LOGFILE') or '').strip()
    if logfile:
        try:
            with Path(logfile).open('a', encoding='utf-8') as handle:
                handle.write(message + '\n')
        except OSError:
            pass


def seed(directory: Path) -> int:
    raw_target = (os.environ.get('wizard_router_target') or '').strip()
    raw_token = (os.environ.get('wizard_router_token') or '').strip()
    if not raw_target and not raw_token:
        write_line('向导没有提供地址或令牌，保持现有配置不变')
        return 0

    target = ''
    if raw_target:
        try:
            target = engine.normalize_target(raw_target)
        except RuntimeError as error:
            write_line(f'地址不合法：{error}')
            return 1

    directory.mkdir(parents=True, exist_ok=True)
    try:
        os.chmod(directory, 0o700)
    except OSError:
        pass

    if target:
        state_file = directory / 'settings.json'
        payload: dict[str, object] = {}
        if state_file.is_file():
            try:
                loaded = json.loads(state_file.read_text(encoding='utf-8'))
                if isinstance(loaded, dict):
                    payload = loaded
            except (OSError, json.JSONDecodeError):
                payload = {}
        payload['target'] = target
        engine.atomic_write(state_file, json.dumps(payload, ensure_ascii=False, indent=2) + '\n')
        write_line(f'已写入目标地址：{target}')

    if raw_token:
        engine.atomic_write(directory / 'router-token', raw_token + '\n', mode=0o600)
        write_line(f'已写入 AdminToken（{engine.token_hint(raw_token)}，权限 0600）')
    else:
        write_line('令牌留空，保持原值不变')
    return 0


def migrate_legacy(directory: Path) -> int:
    """从旧的小米插件目录搬一次设置（旧目录有、本应用还没有时才搬）。"""
    state_file = directory / 'settings.json'
    token_file = directory / 'router-token'
    if state_file.is_file() or token_file.is_file():
        write_line('本应用已有配置，跳过旧数据迁移')
        return 0
    for legacy in LEGACY_DIRS:
        legacy_state = legacy / 'settings.json'
        legacy_token = legacy / 'router-token'
        if not (legacy_state.is_file() or legacy_token.is_file()):
            continue
        directory.mkdir(parents=True, exist_ok=True)
        try:
            os.chmod(directory, 0o700)
        except OSError:
            pass
        if legacy_state.is_file():
            shutil.copyfile(legacy_state, state_file)
        if legacy_token.is_file():
            shutil.copyfile(legacy_token, token_file)
            try:
                os.chmod(token_file, 0o600)
            except OSError:
                pass
        write_line(f'已从旧插件目录迁移设置：{legacy}（旧目录保持不动）')
        return 0
    write_line('没有找到可迁移的旧插件配置')
    return 0


def main(argv: list[str]) -> int:
    directory = config_dir()
    if '--migrate-legacy' in argv:
        return migrate_legacy(directory)
    return seed(directory)


if __name__ == '__main__':
    raise SystemExit(main(sys.argv[1:]))
