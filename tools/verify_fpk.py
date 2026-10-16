#!/usr/bin/env python3
"""校验一个已经打好的 .fpk（发布前最后一道闸）。

为什么要单独有这个：`go test` 验的是构建过程的中间产物，而这里验的是**真正要发出去的
那个文件**——包结构、checksum、可执行位、ELF 架构、manifest 字段。CI 里对每个 .fpk 跑一遍。

用法：
    python3 tools/verify_fpk.py dist/*.fpk
    python3 tools/verify_fpk.py --arch x86 dist/unificenter-1.0.0-x86.fpk   # 额外核对架构

退出码：0 全部通过；1 有任一检查失败。
"""

from __future__ import annotations

import argparse
import hashlib
import io
import struct
import sys
import tarfile
from pathlib import Path

REQUIRED_TOP = ('manifest', 'ICON.PNG', 'ICON_256.PNG', 'app.tgz', 'cmd', 'config', 'wizard')
REQUIRED_ARGS = ('bin/unifi-center', 'ui/config', 'config/privilege', 'config/resource',
                 'ui/panel.html', 'ui/panel.js', 'ui/panel.css',
                 'ui/assets/close.js', 'ui/assets/overlay.js', 'ui/assets/fullscreen.js',
                 'ui/assets/vue.js', 'ui/assets/lucide.js', 'ui/assets/tailwind.js',
                 'ui/assets/icon.png', 'ui/images/icon_64.png', 'ui/images/icon_256.png')
REQUIRED_MANIFEST = ('appname', 'version', 'display_name', 'source', 'platform',
                     'desktop_uidir', 'desktop_applaunchname', 'checksum')
LIFECYCLE = ('main', 'install_init', 'install_callback', 'upgrade_init', 'upgrade_callback',
             'uninstall_init', 'uninstall_callback', 'config_init', 'config_callback')
ELF_MACHINE = {'x86': 0x3E, 'arm': 0xB7}          # x86-64 / AArch64


class Report:
    def __init__(self, path: Path) -> None:
        self.path = path
        self.problems: list[str] = []

    def check(self, label: str, ok: bool, detail: str = '') -> bool:
        print(f'  [{"OK  " if ok else "FAIL"}] {label}' + (f' —— {detail}' if detail and not ok else ''))
        if not ok:
            self.problems.append(label + (f' ({detail})' if detail else ''))
        return ok


def read_tar_gz(blob: bytes) -> dict[str, tarfile.TarInfo]:
    members: dict[str, tarfile.TarInfo] = {}
    with tarfile.open(fileobj=io.BytesIO(blob), mode='r:gz') as archive:
        for member in archive.getmembers():
            members[member.name.rstrip('/')] = member
    return members


def read_member(blob: bytes, name: str) -> bytes:
    with tarfile.open(fileobj=io.BytesIO(blob), mode='r:gz') as archive:
        handle = archive.extractfile(name)
        return handle.read() if handle else b''


def verify(path: Path, expect_arch: str | None) -> list[str]:
    print(f'== {path.name} ==')
    report = Report(path)
    if not report.check('文件存在且是 gzip', path.read_bytes()[:2] == b'\x1f\x8b',
                        path.read_bytes()[:2].hex()):
        return report.problems

    outer_blob = path.read_bytes()
    outer = read_tar_gz(outer_blob)

    # --- 顶层结构（与官方 fnpack 产物一致）
    for name in REQUIRED_TOP:
        report.check(f'顶层有 {name}', name in outer)

    # --- manifest
    manifest_raw = read_member(outer_blob, 'manifest')
    report.check('manifest 不带 BOM', not manifest_raw.startswith(b'\xef\xbb\xbf'))
    text = manifest_raw.decode('utf-8', 'replace')
    fields: dict[str, str] = {}
    for line in text.splitlines():
        # partition 返回 (前, 分隔符, 后)，值是第三项；写成 key, value, found 会拿到分隔符（踩过）
        key, _separator, value = line.partition('=')
        if _separator:
            fields[key.strip()] = value.strip()
    for key in REQUIRED_MANIFEST:
        report.check(f'manifest 有 {key}', bool(fields.get(key)), fields.get(key, ''))
    report.check('appname 未被改名', fields.get('appname') == 'unificenter', fields.get('appname', ''))
    report.check('没有运行时依赖声明', not fields.get('install_dep_apps'),
                 f"install_dep_apps={fields.get('install_dep_apps')}")
    platform = fields.get('platform', '')
    report.check('platform 是具体架构（含二进制不能用 all）', platform in ELF_MACHINE, platform)
    if expect_arch:
        report.check(f'platform 与期望架构一致（{expect_arch}）', platform == expect_arch, platform)
    report.check('生命周期脚本声明齐全（包根 cmd/）',
                 all(f'cmd/{name}' in outer for name in LIFECYCLE),
                 ','.join(n for n in LIFECYCLE if f'cmd/{n}' not in outer))
    for name in LIFECYCLE:
        member = outer.get(f'cmd/{name}')
        if member is not None and not member.mode & 0o111:
            report.check(f'cmd/{name} 有可执行位', False, oct(member.mode))

    # --- app.tgz
    if 'app.tgz' not in outer:
        report.problems.append('缺少 app.tgz')
        return report.problems
    app_blob = read_member(outer_blob, 'app.tgz')
    checksum = hashlib.md5(app_blob).hexdigest()
    report.check('manifest 里的 checksum 与 app.tgz 的 md5 一致',
                 fields.get('checksum') == checksum, f"{fields.get('checksum')} vs {checksum}")

    inner = read_tar_gz(app_blob)
    nested = [name for name in inner if name == 'app' or name.startswith('app/')]
    report.check('app.tgz 里没有多套一层 app/', not nested, ','.join(nested[:3]))
    for name in REQUIRED_ARGS:
        report.check(f'app.tgz 里有 {name}', name in inner)

    # --- 二进制
    member = inner.get('bin/unifi-center')
    if member is not None:
        report.check('二进制带可执行位', bool(member.mode & 0o111), oct(member.mode))
        binary = read_member(app_blob, 'bin/unifi-center')
        report.check('二进制是 ELF', binary[:4] == b'\x7fELF', binary[:4].hex())
        if len(binary) >= 20:
            report.check('ELF 是 64 位小端', binary[4] == 2 and binary[5] == 1,
                         f'{binary[4]}/{binary[5]}')
            machine = struct.unpack('<H', binary[18:20])[0]
            want = ELF_MACHINE.get(platform)
            report.check(f'ELF 架构与 platform 一致（{platform}）', machine == want,
                         f'0x{machine:02X} vs 0x{want:02X}' if want else '未知 platform')
            report.check('二进制是静态链接（无 PT_INTERP）', not has_interp(binary))
        report.check('二进制非空且像样（> 1MB）', len(binary) > 1024 * 1024, f'{len(binary)} 字节')

    # --- 图标
    for name in ('ICON.PNG', 'ICON_256.PNG'):
        blob = read_member(outer_blob, name)
        report.check(f'{name} 是 PNG', blob[:8] == b'\x89PNG\r\n\x1a\n')
        if len(blob) >= 24:
            width, height = struct.unpack('>II', blob[16:24])
            want = 64 if name == 'ICON.PNG' else 256
            report.check(f'{name} 是 {want}x{want}', (width, height) == (want, want),
                         f'{width}x{height}')
        report.check(f'{name} 小于 1024 KB', len(blob) > 0 and len(blob) < 1024 * 1024,
                     f'{len(blob)} 字节')

    # --- wizard
    for name in ('install', 'upgrade', 'uninstall', 'config'):
        blob = read_member(outer_blob, f'wizard/{name}')
        report.check(f'wizard/{name} 是合法 JSON', blob.strip()[:1] == b'[' and blob.strip()[-1:] == b']')
    return report.problems


def has_interp(data: bytes) -> bool:
    if len(data) < 64:
        return False
    phoff = struct.unpack('<Q', data[32:40])[0]
    phentsize = struct.unpack('<H', data[54:56])[0]
    phnum = struct.unpack('<H', data[56:58])[0]
    for index in range(phnum):
        offset = phoff + index * phentsize
        if offset + 4 > len(data):
            return False
        if struct.unpack('<I', data[offset:offset + 4])[0] == 3:      # PT_INTERP
            return True
    return False


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description='校验 .fpk 包')
    parser.add_argument('packages', nargs='+', help='要校验的 .fpk')
    parser.add_argument('--arch', default='', choices=['', 'x86', 'arm'],
                        help='额外核对包的 platform 与 ELF 架构')
    args = parser.parse_args(argv)

    failed = 0
    for item in args.packages:
        path = Path(item)
        if not path.is_file():
            print(f'== {item} ==\n  [FAIL] 文件不存在')
            failed += 1
            continue
        expect = args.arch or None
        if expect is None:                     # 从文件名推断（unificenter-1.0.0-x86.fpk）
            for arch in ELF_MACHINE:
                if f'-{arch}.fpk' in path.name:
                    expect = arch
        problems = verify(path, expect)
        if problems:
            failed += 1
            print(f'  → {len(problems)} 项失败')
        else:
            print('  → 全部通过')
    if failed:
        print(f'\n{failed} 个包未通过校验')
        return 1
    print('\n所有包校验通过 ✓')
    return 0


if __name__ == '__main__':
    raise SystemExit(main(sys.argv[1:]))
