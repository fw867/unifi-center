package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// 图标检查：尺寸符合 fnOS 要求（64/256）、大小不超过阈值，且按图标规范做了圆角
// （四角透明 —— 规范要求"主体使用圆角矩形，不要直角满铺"）。
func TestIconsAreRoundedAndCorrectSize(t *testing.T) {
	cases := map[string]int{
		"ICON.PNG":                   64,
		"ICON_256.PNG":               256,
		"app/ui/images/icon_64.png":  64,
		"app/ui/images/icon_256.png": 256,
		"app/ui/assets/icon.png":     256,
	}
	for name, size := range cases {
		path := filepath.Join("..", name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("缺少 %s：%v", name, err)
			continue
		}
		if info.Size() > 1024*1024 {
			t.Errorf("%s 超过 1024 KB（%d 字节）", name, info.Size())
		}
		width, height, pixels := decodePNGForTest(t, path)
		if width != size || height != size {
			t.Errorf("%s 是 %dx%d，期望 %dx%d", name, width, height, size, size)
			continue
		}
		if alpha := pixels[3]; alpha > 40 {
			t.Errorf("%s 左上角 alpha=%d，期望接近 0（需要圆角矩形主体）", name, alpha)
		}
		center := ((size/2)*size + size/2) * 4
		if alpha := pixels[center+3]; alpha < 200 {
			t.Errorf("%s 中心 alpha=%d，图标主体不该透明", name, alpha)
		}
	}
}

// PNG 源（图标与 ICO 都入库，便于重新生成）也一起检查一下在不在。
func TestIconSourcesPresent(t *testing.T) {
	for _, name := range []string{"tools/ui-apple-touch-icon.png", "tools/ui-favicon.ico",
		"tools/make_icon.py"} {
		if _, err := os.Stat(filepath.Join("..", name)); err != nil {
			t.Errorf("缺少图标源 %s：%v", name, err)
		}
	}
}

// decodePNGForTest 解 8 位 RGB/RGBA PNG（图标就是这个格式），返回原始像素。
func decodePNGForTest(t *testing.T, path string) (int, int, []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", path, err)
	}
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("%s 不是 PNG", path)
	}
	var width, height, depth, colorType int
	var idat []byte
	for offset := 8; offset+8 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		tag := string(data[offset+4 : offset+8])
		if offset+8+length > len(data) {
			break
		}
		body := data[offset+8 : offset+8+length]
		switch tag {
		case "IHDR":
			width = int(binary.BigEndian.Uint32(body[0:4]))
			height = int(binary.BigEndian.Uint32(body[4:8]))
			depth = int(body[8])
			colorType = int(body[9])
		case "IDAT":
			idat = append(idat, body...)
		}
		offset += 12 + length
	}
	if depth != 8 || (colorType != 6 && colorType != 2) {
		t.Fatalf("%s：只支持 8 位 RGB/RGBA，实际 depth=%d colorType=%d", path, depth, colorType)
	}
	channels := 4
	if colorType == 2 {
		channels = 3
	}
	reader, err := zlib.NewReader(bytes.NewReader(idat))
	if err != nil {
		t.Fatalf("%s：IDAT 解压失败：%v", path, err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("%s：读 IDAT 失败：%v", path, err)
	}
	_ = reader.Close()

	stride := width * channels
	pixels := make([]byte, width*height*4)
	previous := make([]byte, stride)
	for row := 0; row < height; row++ {
		base := row * (stride + 1)
		if base+1+stride > len(raw) {
			t.Fatalf("%s：IDAT 数据不完整（第 %d 行）", path, row)
		}
		filterType := raw[base]
		line := append([]byte(nil), raw[base+1:base+1+stride]...)
		for index := range line {
			var left, upLeft byte
			if index >= channels {
				left = line[index-channels]
				upLeft = previous[index-channels]
			}
			up := previous[index]
			switch filterType {
			case 1:
				line[index] += left
			case 2:
				line[index] += up
			case 3:
				line[index] += byte((int(left) + int(up)) / 2)
			case 4:
				estimate := int(left) + int(up) - int(upLeft)
				pa := absInt(estimate - int(left))
				pb := absInt(estimate - int(up))
				pc := absInt(estimate - int(upLeft))
				predictor := left
				if pa > pb || pa > pc {
					if pb <= pc {
						predictor = up
					} else {
						predictor = upLeft
					}
				}
				line[index] += predictor
			}
		}
		for column := 0; column < width; column++ {
			target := (row*width + column) * 4
			if channels == 4 {
				copy(pixels[target:target+4], line[column*4:column*4+4])
			} else {
				copy(pixels[target:target+3], line[column*3:column*3+3])
				pixels[target+3] = 255
			}
		}
		previous = line
	}
	return width, height, pixels
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
