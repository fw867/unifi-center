#!/usr/bin/env python3
"""打包 .fpk（fnOS 应用包）—— 用 Python 标准库完成，不依赖 tar 的实现细节。

为什么不用系统的 tar：Windows 自带的是 bsdtar，它既不认 `--mode`（GNU 选项），
又默认把权限记成 0666，打出来的包里 `bin/unifi-center` 和 `cmd/*` 没有可执行位，
装到设备上会直接 `Permission denied`。用 Python 自己写 tar 头就能精确控制每个条目的
权限（mtime/uid/gid 也固定，产物可复现）。

产物结构与官方 fnpack 一致：
    manifest  ICON.PNG  ICON_256.PNG  app.tgz  cmd/  config/  wizard/
其中 app.tgz 是 app/ 的 tar.gz，manifest 里带一行 `checksum = <app.tgz 的 md5>`。

用法：
    python3 tools/pack_fpk.py --root . --out dist/unificenter-1.0.0-x86.fpk \
        --arch x86 --app-version 1.0.0
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
import re
import sys
import tarfile
from pathlib import Path

FILE_MODE = 0o755          # cmd/*、二进制、UI 文件一律可执行位（最稳，也方便排障）
DIR_MODE = 0o755
FIXED_MTIME = 1700000000   # 固定时间戳，让产物可复现

ARCH_PLATFORM = {'x86': 'x86', 'arm': 'arm'}
SKIP_NAMES = {'__pycache__', '.DS_Store', '.dev', 'dist', '.git'}
SKIP_SUFFIXES = ('.pyc', '.tmp')


def log(message: str) -> None:
    print(f'[pack] {message}')


def want(path: Path, root: Path) -> bool:
    relative = path.relative_to(root)
    if any(part in SKIP_NAMES for part in relative.parts):
        return False
    if path.name.endswith(SKIP_SUFFIXES):
        return False
    return True


def add_tree(archive: tarfile.TarFile, source: Path, arc_prefix: str) -> int:
    """把 source 目录（含目录项本身）按 arc_prefix 写进 archive，权限固定。"""
    count = 0
    info = archive.gettarinfo(str(source), arcname=arc_prefix)
    info.mode = DIR_MODE
    info.mtime = FIXED_MTIME
    info.uid = info.gid = 0
    info.uname = info.gname = 'root'
    archive.addfile(info)
    count += 1

    for current, dirs, files in os.walk(source):
        dirs[:] = sorted(name for name in dirs if name not in SKIP_NAMES)
        files = sorted(files)
        current_path = Path(current)
        for name in dirs + files:
            path = current_path / name
            if not want(path, source):
                continue
            relative = path.relative_to(source)
            arcname = f'{arc_prefix}/{relative.as_posix()}'
            if path.is_dir():
                info = archive.gettarinfo(str(path), arcname=arcname)
                info.mode = DIR_MODE
            else:
                info = archive.gettarinfo(str(path), arcname=arcname)
                info.mode = FILE_MODE
            info.mtime = FIXED_MTIME
            info.uid = info.gid = 0
            info.uname = info.gname = 'root'
            if info.isreg():
                with path.open('rb') as handle:
                    archive.addfile(info, handle)
            else:
                archive.addfile(info)
            count += 1
    return count


def add_entry(archive: tarfile.TarFile, source: Path, arcname: str) -> int:
    """把单个文件或一整棵目录按 arcname 写进 archive（权限固定 0755）。"""
    if source.is_dir():
        return add_tree(archive, source, arcname)
    info = archive.gettarinfo(str(source), arcname=arcname)
    info.mode = FILE_MODE
    info.mtime = FIXED_MTIME
    info.uid = info.gid = 0
    info.uname = info.gname = 'root'
    with source.open('rb') as handle:
        archive.addfile(info, handle)
    return 1


def build_tar_gz(target: Path, entries: list[tuple[Path, str]]) -> int:
    """把若干 (路径, 归档内名字) 打成 tar.gz，返回条目数。"""
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open('wb') as raw:
        with gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=FIXED_MTIME) as gz:
            with tarfile.open(fileobj=gz, mode='w', format=tarfile.PAX_FORMAT) as archive:
                total = 0
                for source, arcname in entries:
                    total += add_entry(archive, source, arcname)
                return total


def build_app_tgz(target: Path, root: Path) -> int:
    """把 app/ 的**内容**（不带 app/ 这一层）打成 app.tgz。

    这是与官方 fnpack 产物的关键差异，踩过：官方 app.tgz 的顶层直接是
    `bin/`、`ui/`（以及 `config/`），安装器解到 target 后就是 target/bin、target/ui；
    如果多套一层 `app/`，装完只有 target/app/bin/...，于是 cmd/main 找不到二进制、
    入口也没有 ui/config —— 表现为"应用无法启用：找不到可执行文件"。
    """
    app_dir = root / 'app'
    entries: list[tuple[Path, str]] = []
    for item in sorted(app_dir.iterdir(), key=lambda path: path.name):
        if item.name in SKIP_NAMES:
            continue
        entries.append((item, item.name))
    # 官方还会把 config/ 整个放进 app.tgz（安装后 target/config 里有 privilege/resource，
    # 安装器据此决定运行用户），所以这里连目录条目一起补上
    config_dir = root / 'config'
    if config_dir.is_dir():
        entries.append((config_dir, 'config'))
    return build_tar_gz(target, entries)


def archive_mode(target: Path) -> bool:
    """自检：确认打出来的包里可执行位真的在（避免又退回 0666）。"""
    with tarfile.open(target, 'r:gz') as archive:
        for member in archive.getmembers():
            if member.name.endswith('bin/unifi-center') or member.name == 'cmd/main':
                if not member.mode & 0o111:
                    return False
    return True


def check_app_layout(app_tgz: Path) -> list[str]:
    """自检 app.tgz 的顶层结构：必须是 bin/ 与 ui/，不能多套一层 app/。

    官方的 app.tgz 顶层就是 bin/、ui/，安装器把它们解到 target 下；多一层 app/
    会导致装完只有 target/app/...，应用直接起不来（踩过）。
    同时确认前端资源齐全（缺一个就可能白屏或按钮失灵）。
    """
    problems = []
    with tarfile.open(app_tgz, 'r:gz') as archive:
        names = [member.name.rstrip('/') for member in archive.getmembers()]
    if any(name == 'app' or name.startswith('app/') for name in names):
        problems.append('app.tgz 里多了一层 app/ 目录（安装后会变成 target/app/...）')
    tops = {name.split('/')[0] for name in names if name}
    for required in ('bin', 'ui'):
        if required not in tops:
            problems.append(f'app.tgz 顶层缺少 {required}/')
    for required in ('bin/unifi-center', 'ui/config', 'config/privilege', 'config/resource',
                     'ui/panel.html', 'ui/panel.js', 'ui/panel.css',
                     'ui/assets/close.js', 'ui/assets/overlay.js', 'ui/assets/fullscreen.js',
                     'ui/assets/vue.js', 'ui/assets/lucide.js', 'ui/assets/tailwind.js',
                     'ui/assets/icon.png',
                     'ui/images/icon_64.png', 'ui/images/icon_256.png'):
        if required not in names:
            problems.append(f'app.tgz 里缺少 {required}')
    return problems


def read_manifest(path: Path) -> list[str]:
    text = path.read_text(encoding='utf-8').lstrip('\ufeff')
    return [line for line in text.splitlines() if line.strip()]


def write_manifest(path: Path, lines: list[str]) -> None:
    text = '\n'.join(lines) + '\n'
    path.write_text(text, encoding='utf-8')     # 无 BOM


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description='打包 fnOS .fpk')
    parser.add_argument('--root', default='.', help='项目根目录')
    parser.add_argument('--out', required=True, help='输出的 .fpk 路径')
    parser.add_argument('--arch', default='x86', choices=sorted(ARCH_PLATFORM), help='目标架构')
    parser.add_argument('--work', default='', help='临时目录（默认用系统临时目录）')
    args = parser.parse_args(argv)

    root = Path(args.root).resolve()
    out = Path(args.out)
    if not out.is_absolute():
        out = (Path.cwd() / out).resolve()
    platform = ARCH_PLATFORM[args.arch]

    manifest_path = root / 'manifest'
    if not manifest_path.is_file():
        raise SystemExit('缺少 manifest')
    lines = read_manifest(manifest_path)
    fields = {}
    for line in lines:
        if '=' in line:
            key, value = line.split('=', 1)
            fields[key.strip()] = value.strip()
    appname = fields.get('appname')
    version = fields.get('version')
    if not appname or not version:
        raise SystemExit('manifest 里缺 appname 或 version')

    # 二进制必须已经放在 app/bin/ 下（由 build.sh / build.ps1 交叉编译产出）
    binary = root / 'app' / 'bin' / 'unifi-center'
    if not binary.is_file():
        raise SystemExit(f'缺少 {binary}（先交叉编译 Go 二进制）')

    work = Path(args.work).resolve() if args.work else None
    if work is None:
        import tempfile
        work = Path(tempfile.mkdtemp(prefix='unificenter-pack-'))
    work.mkdir(parents=True, exist_ok=True)
    log(f'工作目录 {work}')

    log(f'打包 app/ → app.tgz（{appname} {version} {args.arch}）')
    app_tgz = work / 'app.tgz'
    entries = build_app_tgz(app_tgz, root)
    checksum = hashlib.md5(app_tgz.read_bytes()).hexdigest()
    log(f'app.tgz 条目 {entries} 个，md5 = {checksum}')
    if not archive_mode(app_tgz):
        raise SystemExit('app.tgz 里缺少可执行位，打包中止')
    layout_problems = check_app_layout(app_tgz)
    if layout_problems:
        raise SystemExit('app.tgz 结构不对：' + '；'.join(layout_problems))

    # manifest：本架构的 platform，去掉旧 checksum，补上新的
    new_lines = [line for line in lines if not re.match(r'^\s*checksum\s*=', line)]
    replaced = False
    for index, line in enumerate(new_lines):
        if re.match(r'^\s*platform\s*=', line):
            new_lines[index] = f'platform={platform}'
            replaced = True
    if not replaced:
        new_lines.append(f'platform={platform}')
    new_lines.append(f'checksum = {checksum}')
    local_manifest = work / 'manifest'
    write_manifest(local_manifest, new_lines)

    log('打包 .fpk')
    # 包根条目：与官方 fnpack 产物的顺序保持一致（app.tgz 在最前，manifest 在最后）
    staging = [
        (app_tgz, 'app.tgz'),
        (root / 'cmd', 'cmd'),
        (root / 'config', 'config'),
        (root / 'wizard', 'wizard'),
        (root / 'ICON.PNG', 'ICON.PNG'),
        (root / 'ICON_256.PNG', 'ICON_256.PNG'),
        (local_manifest, 'manifest'),
    ]
    for source, _ in staging:
        if not source.exists():
            raise SystemExit(f'缺少 {source}')
    build_tar_gz(out, staging)
    if not archive_mode(out):
        raise SystemExit('.fpk 里缺少可执行位，打包中止')

    size_kb = round(out.stat().st_size / 1024, 1)
    log(f'完成：{out}（{size_kb} KB，platform={platform}）')
    return 0


if __name__ == '__main__':
    raise SystemExit(main(sys.argv[1:]))
