#!/usr/bin/env python3
"""生成应用图标（纯标准库，不依赖 Pillow / ImageMagick）。

图标源：https://ui.com/microsite/apple-touch-icon.png
        官方 256×256 PNG，就是 ui.com 的 UniFi 标志（蓝色渐变底 + 白色 "U"）。
        也支持读 ICO（--source xxx.ico 会自动挑里面最大的一张；注意
        https://ui.com/microsite/favicon.ico?v=2 里最大只有 48×48，放大会糊）。

生成：
    ICON.PNG                    64×64   fnOS 包图标
    ICON_256.PNG                256×256 fnOS 包图标
    app/ui/images/icon_64.png    64×64  桌面入口图标（入口配置里的 {0} 会替换成尺寸）
    app/ui/images/icon_256.png   256×256
    app/ui/assets/icon.png       256×256 页面 favicon

按 fnOS 图标规范（"主体使用圆角矩形，不要直角满铺"）默认给四角加抗锯齿圆角，
半径默认取边长的 21%（贴近系统图标观感）。想保留官方原样的直角方图：

    python3 tools/make_icon.py . --radius 0

用法：
    python3 tools/make_icon.py [项目根目录]
    python3 tools/make_icon.py . --source tools/ui-favicon.ico --radius 0.18
"""

from __future__ import annotations

import argparse
import struct
import sys
import zlib
from pathlib import Path

DEFAULT_SOURCE = Path(__file__).resolve().parent / 'ui-apple-touch-icon.png'
DEFAULT_RADIUS_RATIO = 0.21

# ---------------------------------------------------------------------------
# 解码
# ---------------------------------------------------------------------------


def decode_image(path: Path) -> tuple[int, int, bytearray]:
    """读 PNG 或 ICO，返回 (宽, 高, RGBA 像素)。"""
    blob = path.read_bytes()
    if blob[:8] == b'\x89PNG\r\n\x1a\n':
        width, height, pixels = decode_png(blob)
        print(f'源图 {path.name}：{width}x{height} PNG')
        return width, height, pixels
    return load_ico(blob)


def load_ico(blob: bytes) -> tuple[int, int, bytearray]:
    """从 ICO 里取出最大的一张图，返回 (宽, 高, RGBA 像素)。"""
    if len(blob) < 6:
        raise SystemExit('ICO 太短')
    _reserved, kind, count = struct.unpack('<HHH', blob[:6])
    if kind != 1:
        raise SystemExit(f'不是 ICO（type={kind}）')
    best: tuple[int, int, int, bytearray] | None = None
    for index in range(count):
        offset = 6 + index * 16
        width, height, _colors, _res, _planes, _bits, size, data_offset = struct.unpack(
            '<BBBBHHII', blob[offset:offset + 16])
        width = width or 256
        height = height or 256
        payload = blob[data_offset:data_offset + size]
        try:
            pixels = decode_payload(payload)
        except Exception as error:                                     # noqa: BLE001
            print(f'  ! 第 {index} 张（{width}x{height}）解码失败：{error}', file=sys.stderr)
            continue
        print(f'  · ICO 第 {index} 张：{width}x{height}')
        if best is None or width * height > best[0] * best[1]:
            best = (width, height, index, pixels)
    if best is None:
        raise SystemExit('ICO 里没有能解码的图像')
    print(f'  使用 ICO 第 {best[2]} 张（{best[0]}x{best[1]}）—— ICO 里没有更大尺寸时会被放大')
    return best[0], best[1], best[3]


def decode_payload(payload: bytes) -> bytearray:
    if payload[:8] == b'\x89PNG\r\n\x1a\n':
        _width, _height, pixels = decode_png(payload)
        return pixels
    return decode_bmp(payload)


def decode_bmp(payload: bytes) -> bytearray:
    """ICO 里的 BMP：BITMAPINFOHEADER + 自下而上的 BGRA 像素 + AND 掩码。"""
    header_size = struct.unpack('<I', payload[:4])[0]
    dib_width, dib_height = struct.unpack('<ii', payload[4:12])
    bits = struct.unpack('<H', payload[14:16])[0]
    if header_size < 40:
        raise SystemExit(f'不支持的 DIB 头（{header_size} 字节）')
    if bits not in (24, 32):
        raise SystemExit(f'不支持的位深：{bits}')
    width = abs(dib_width)
    # ICO 的 DIB 高度是"图像 + 掩码"的两倍
    height = abs(dib_height) // 2 if abs(dib_height) % 2 == 0 else abs(dib_height)
    stride = ((width * bits + 31) // 32) * 4
    pixels = bytearray(width * height * 4)
    base = header_size
    for row in range(height):
        source_row = height - 1 - row
        line = payload[base + source_row * stride: base + (source_row + 1) * stride]
        for column in range(width):
            if bits == 32:
                blue, green, red, alpha = line[column * 4:column * 4 + 4]
            else:
                offset = column * 3
                blue, green, red = line[offset:offset + 3]
                alpha = 255
            index = (row * width + column) * 4
            pixels[index:index + 4] = bytes((red, green, blue, alpha))
    return pixels


def decode_png(payload: bytes) -> tuple[int, int, bytearray]:
    """8 位 PNG 解码，支持灰度/RGB/调色板/带 alpha。"""
    position = 8
    width = height = 0
    bit_depth = color_type = 0
    idat = bytearray()
    palette = b''
    while position < len(payload):
        length = struct.unpack('>I', payload[position:position + 4])[0]
        tag = payload[position + 4:position + 8]
        body = payload[position + 8:position + 8 + length]
        if tag == b'IHDR':
            width, height, bit_depth, color_type = struct.unpack('>IIBB', body[:10])
        elif tag == b'PLTE':
            palette = body
        elif tag == b'IDAT':
            idat += body
        elif tag == b'IEND':
            break
        position += 12 + length
    if bit_depth != 8:
        raise SystemExit(f'只支持 8 位 PNG，实际 {bit_depth}')
    if color_type not in (0, 2, 3, 4, 6):
        raise SystemExit(f'不支持的色彩类型：{color_type}')
    channels = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[color_type]
    raw = zlib.decompress(bytes(idat))
    stride = width * channels
    out = bytearray(width * height * 4)
    previous = bytearray(stride)
    offset = 0
    for row in range(height):
        filter_type = raw[offset]
        offset += 1
        line = bytearray(raw[offset:offset + stride])
        offset += stride
        unfilter(line, previous, filter_type, channels)
        for column in range(width):
            sample = line[column * channels:(column + 1) * channels]
            if color_type == 6:
                red, green, blue, alpha = sample
            elif color_type == 2:
                red, green, blue = sample
                alpha = 255
            elif color_type == 0:
                red = green = blue = sample[0]
                alpha = 255
            elif color_type == 4:
                red = green = blue = sample[0]
                alpha = sample[1]
            else:                                    # 3 = 调色板
                index = sample[0] * 3
                red, green, blue = palette[index:index + 3]
                alpha = 255
            target = (row * width + column) * 4
            out[target:target + 4] = bytes((red, green, blue, alpha))
        previous = line
    return width, height, out


def unfilter(line: bytearray, previous: bytearray, filter_type: int, channels: int) -> None:
    for index in range(len(line)):
        left = line[index - channels] if index >= channels else 0
        up = previous[index]
        up_left = previous[index - channels] if index >= channels else 0
        if filter_type == 1:
            line[index] = (line[index] + left) & 0xFF
        elif filter_type == 2:
            line[index] = (line[index] + up) & 0xFF
        elif filter_type == 3:
            line[index] = (line[index] + ((left + up) >> 1)) & 0xFF
        elif filter_type == 4:
            estimate = left + up - up_left
            pa, pb, pc = abs(estimate - left), abs(estimate - up), abs(estimate - up_left)
            predictor = left if (pa <= pb and pa <= pc) else (up if pb <= pc else up_left)
            line[index] = (line[index] + predictor) & 0xFF


# ---------------------------------------------------------------------------
# 缩放与圆角
# ---------------------------------------------------------------------------


def _weights(source_size: int, target_size: int) -> list[list[tuple[int, float]]]:
    scale = target_size / source_size
    support = 1.0 if scale < 1 else scale        # 降采样时滤波核跟着放大
    table: list[list[tuple[int, float]]] = []
    for out_index in range(target_size):
        center = (out_index + 0.5) / scale - 0.5
        left = int(center - support + 0.5)
        right = int(center + support + 0.5)
        weights: list[tuple[int, float]] = []
        total = 0.0
        for source_index in range(left, right + 1):
            distance = abs(source_index - center) / support
            weight = (max(0.0, 1.0 - distance) if scale < 1
                      else max(0.0, 1.0 - distance * distance * 0.6))
            if weight <= 0:
                continue
            clamped = min(max(source_index, 0), source_size - 1)
            weights.append((clamped, weight))
            total += weight
        if total <= 0:
            clamped = min(max(int(center + 0.5), 0), source_size - 1)
            weights = [(clamped, 1.0)]
            total = 1.0
        table.append([(index, weight / total) for index, weight in weights])
    return table


def resize(pixels: bytearray, width: int, height: int,
           new_width: int, new_height: int) -> bytearray:
    if width == new_width and height == new_height:
        return bytearray(pixels)
    # 在预乘 alpha 空间缩放，避免透明边缘出现黑边/白边
    premultiplied = [0.0] * (width * height * 4)
    for index in range(width * height):
        alpha = pixels[index * 4 + 3] / 255.0
        for channel in range(3):
            premultiplied[index * 4 + channel] = pixels[index * 4 + channel] * alpha
        premultiplied[index * 4 + 3] = pixels[index * 4 + 3]

    horizontal = [0.0] * (new_width * height * 4)
    for row in range(height):
        for out_column, weights in enumerate(_weights(width, new_width)):
            accumulator = [0.0, 0.0, 0.0, 0.0]
            for source_column, weight in weights:
                base = (row * width + source_column) * 4
                for channel in range(4):
                    accumulator[channel] += premultiplied[base + channel] * weight
            target = (row * new_width + out_column) * 4
            horizontal[target:target + 4] = accumulator

    result = bytearray(new_width * new_height * 4)
    for out_row, weights in enumerate(_weights(height, new_height)):
        for column in range(new_width):
            accumulator = [0.0, 0.0, 0.0, 0.0]
            for source_row, weight in weights:
                base = (source_row * new_width + column) * 4
                for channel in range(4):
                    accumulator[channel] += horizontal[base + channel] * weight
            alpha = min(255.0, max(0.0, accumulator[3]))
            target = (out_row * new_width + column) * 4
            if alpha > 0:
                for channel in range(3):
                    value = accumulator[channel] * 255.0 / alpha
                    result[target + channel] = int(min(255.0, max(0.0, value + 0.5)))
            result[target + 3] = int(alpha + 0.5)
    return result


def round_corners(pixels: bytearray, size: int, radius_ratio: float) -> bytearray:
    """给正方形图标加抗锯齿圆角（fnOS 图标规范要求主体是圆角矩形）。

    只处理四角半径范围内的像素，图中间一个像素都不动。
    """
    if radius_ratio <= 0:
        return pixels
    radius = size * radius_ratio
    out = bytearray(pixels)
    limit = int(radius) + 2
    for row in range(limit):
        for column in range(limit):
            if column >= radius and column < size - radius:
                continue
            if row >= radius and row < size - radius:
                continue
            center_x = radius if column < radius else size - radius
            center_y = radius if row < radius else size - radius
            dx = column + 0.5 - center_x
            dy = row + 0.5 - center_y
            distance = (dx * dx + dy * dy) ** 0.5
            coverage = min(1.0, max(0.0, radius - distance + 0.5))
            for (x, y) in ((column, row), (size - 1 - column, row),
                           (column, size - 1 - row), (size - 1 - column, size - 1 - row)):
                index = (y * size + x) * 4
                out[index + 3] = int(out[index + 3] * coverage + 0.5)
    return out


# ---------------------------------------------------------------------------
# PNG 编码
# ---------------------------------------------------------------------------


def encode_png(width: int, height: int, pixels: bytearray) -> bytes:
    raw = bytearray()
    stride = width * 4
    for row in range(height):
        raw.append(0)                                  # filter: None
        raw += pixels[row * stride:(row + 1) * stride]

    def chunk(tag: bytes, payload: bytes) -> bytes:
        return (struct.pack('>I', len(payload)) + tag + payload
                + struct.pack('>I', zlib.crc32(tag + payload) & 0xFFFFFFFF))

    header = struct.pack('>IIBBBBB', width, height, 8, 6, 0, 0, 0)     # 8bit RGBA
    return (b'\x89PNG\r\n\x1a\n'
            + chunk(b'IHDR', header)
            + chunk(b'sRGB', b'\x00')                  # 渲染意图：perceptual
            + chunk(b'gAMA', struct.pack('>I', 45455))
            + chunk(b'IDAT', zlib.compress(bytes(raw), 9))
            + chunk(b'IEND', b''))


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description='生成 fnOS 应用图标')
    parser.add_argument('root', nargs='?', default='', help='项目根目录')
    parser.add_argument('--source', default=str(DEFAULT_SOURCE),
                        help='图标源（PNG 或 ICO），默认用官方 apple-touch-icon.png')
    parser.add_argument('--radius', type=float, default=DEFAULT_RADIUS_RATIO,
                        help='圆角半径 / 边长，0 表示保持直角方图（默认 0.21）')
    args = parser.parse_args(argv)

    source = Path(args.source)
    if not source.is_file():
        raise SystemExit(f'找不到图标源：{source}')
    root = Path(args.root).resolve() if args.root else Path(__file__).resolve().parent.parent

    width, height, pixels = decode_image(source)
    cache: dict[int, bytearray] = {}

    def at(size: int) -> bytearray:
        if size not in cache:
            scaled = resize(pixels, width, height, size, size)
            cache[size] = round_corners(scaled, size, args.radius)
        return cache[size]

    note = f'圆角 {args.radius:.0%}' if args.radius > 0 else '直角（官方原样）'
    targets = (
        (root / 'ICON.PNG', 64),
        (root / 'ICON_256.PNG', 256),
        (root / 'app' / 'ui' / 'images' / 'icon_64.png', 64),
        (root / 'app' / 'ui' / 'images' / 'icon_256.png', 256),
        (root / 'app' / 'ui' / 'assets' / 'icon.png', 256),
    )
    for path, size in targets:
        path.parent.mkdir(parents=True, exist_ok=True)
        payload = encode_png(size, size, at(size))
        path.write_bytes(payload)
        print(f'  已生成 {path.relative_to(root)}（{size}x{size}，{len(payload)} 字节，{note}）')
    return 0


if __name__ == '__main__':
    raise SystemExit(main(sys.argv[1:]))
