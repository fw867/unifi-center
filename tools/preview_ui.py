"""用无头 Edge 截图设置面板与软件中心页面，检查不同窗口尺寸下的排版。

用法：python tools/preview_ui.py [服务端口]
前提前：服务已在 127.0.0.1:<端口> 上以调试模式运行（UNIFICENTER_DEV_PORT）。
"""

import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

EDGE = Path(r'C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe')
OUT = Path(__file__).resolve().parent / 'ui-preview'

SIZES = (
    ('small', 900, 560),      # 接近 fnOS 桌面窗口的默认大小
    ('medium', 1280, 800),
    ('narrow', 420, 700),     # 极窄（手机/半屏）
)


def shoot(url: str, name: str, width: int, height: int) -> Path:
    OUT.mkdir(parents=True, exist_ok=True)
    target = OUT / f'{name}.png'
    profile = Path(tempfile.mkdtemp(prefix='edge-shot-'))
    command = [
        str(EDGE), '--headless=new', '--disable-gpu', '--hide-scrollbars',
        '--no-first-run', '--no-default-browser-check',
        f'--user-data-dir={profile}',
        f'--window-size={width},{height}',
        f'--screenshot={target}',
        url,
    ]
    try:
        subprocess.run(command, capture_output=True, timeout=90, check=False)
    finally:
        shutil.rmtree(profile, ignore_errors=True)
    if not target.is_file():
        raise SystemExit(f'截图失败：{target}')
    print(f'  {target.name}  {width}x{height}  {target.stat().st_size} 字节')
    return target


def main(argv: list[str]) -> int:
    port = argv[1] if len(argv) > 1 else '18101'
    base = f'http://127.0.0.1:{port}'
    print(f'截图 {base}')
    for label, width, height in SIZES:
        shoot(f'{base}/ctl/panel.html', f'panel-{label}', width, height)
    shoot(f'{base}/', 'softcenter-small', 900, 560)
    print(f'输出目录：{OUT}')
    return 0


if __name__ == '__main__':
    raise SystemExit(main(sys.argv))
