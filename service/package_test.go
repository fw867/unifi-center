package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 构建产物验证：检查 dist/ 下每个 .fpk 的架构、platform 声明与包内权限位。
// dist/ 不存在（还没构建过）时自动跳过，这样 go test ./... 在裸源码树上也能通过。
func TestBuiltPackages(t *testing.T) {
	dist := filepath.Join("..", "dist")
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Skipf("没有 dist/ 目录，跳过构建产物检查：%v", err)
	}

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".fpk") {
			continue
		}
		checked++
		t.Run(name, func(t *testing.T) {
			wantArch := ""
			switch {
			case strings.Contains(name, "-x86"):
				wantArch = "x86"
			case strings.Contains(name, "-arm"):
				wantArch = "arm"
			default:
				t.Fatalf("包名 %q 里看不出架构", name)
			}
			checkPackage(t, filepath.Join(dist, name), wantArch)
		})
	}
	if checked == 0 {
		t.Skip("dist/ 里没有 .fpk，跳过")
	}
}

func checkPackage(t *testing.T, path, arch string) {
	t.Helper()
	members := readTarGz(t, path)

	manifest, ok := members["manifest"]
	if !ok {
		t.Fatal("包里没有 manifest")
	}
	text := string(manifest.data)
	if strings.HasPrefix(text, "\ufeff") {
		t.Error("manifest 带 BOM")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if fields["platform"] != arch {
		t.Errorf("manifest platform = %q，期望 %q", fields["platform"], arch)
	}
	for _, required := range []string{"appname", "version", "source", "os_min_version",
		"desktop_uidir", "desktop_applaunchname", "checksum"} {
		if fields[required] == "" {
			t.Errorf("manifest 缺少 %s", required)
		}
	}
	if fields["install_dep_apps"] != "" {
		t.Errorf("自包含包不应声明运行时依赖，却写了 %q", fields["install_dep_apps"])
	}
	appTgz, ok := members["app.tgz"]
	if !ok {
		t.Fatal("包里没有 app.tgz")
	}
	if fields["checksum"] != md5Hex(appTgz.data) {
		t.Errorf("checksum 与 app.tgz 的 md5 不一致：%s vs %s",
			fields["checksum"], md5Hex(appTgz.data))
	}

	inner := readTarGzBytes(t, appTgz.data)
	// app.tgz 的顶层必须是 bin/、ui/、config/ —— 官方 fnpack 就是这么打的，
	// 安装器会把它们解到 target 下。多套一层 app/ 会导致装完只有 target/app/...，
	// 应用直接起不来（实测踩过：应用中心报"找不到可执行文件"）。
	for name := range inner {
		if name == "app" || strings.HasPrefix(name, "app/") {
			t.Fatalf("app.tgz 里多了一层 app/ 目录：%s（安装后会变成 target/app/...）", name)
		}
	}
	for _, required := range []string{"bin/unifi-center", "ui/config",
		"config/privilege", "config/resource", "ui/panel.html",
		"ui/assets/fullscreen.js", "ui/assets/overlay.js", "ui/assets/close.js"} {
		if _, ok := inner[required]; !ok {
			t.Errorf("app.tgz 里缺少 %s（官方包的顶层就是 bin/、ui/、config/）", required)
		}
	}

	binary, ok := inner["app/bin/unifi-center"]
	if !ok {
		binary, ok = inner["bin/unifi-center"]
	}
	if !ok {
		t.Fatal("app.tgz 里没有 bin/unifi-center")
	}
	if binary.mode&0o111 == 0 {
		t.Errorf("二进制没有可执行位：mode=%o", binary.mode)
	}
	// ELF 头：magic + class(2=64bit) + little endian + e_machine
	if len(binary.data) < 20 {
		t.Fatal("二进制太短，不是合法 ELF")
	}
	if string(binary.data[:4]) != "\x7fELF" {
		t.Fatalf("不是 ELF 文件，magic=%q", binary.data[:4])
	}
	if binary.data[4] != 2 || binary.data[5] != 1 {
		t.Errorf("期望 64 位小端 ELF，class/endian = %d/%d", binary.data[4], binary.data[5])
	}
	machine := binaryEndian(binary.data[18:20])
	want := map[string]uint16{"x86": 0x3E, "arm": 0xB7}[arch]
	if machine != want {
		t.Errorf("ELF e_machine = 0x%02X，期望 0x%02X（%s）", machine, want, arch)
	}
	// 静态链接：没有 PT_INTERP 段
	if hasInterp(binary.data) {
		t.Error("二进制带 PT_INTERP，不是静态链接")
	}

	mainScript, ok := members["cmd/main"]
	if !ok {
		t.Fatal("包里没有 cmd/main")
	}
	if mainScript.mode&0o111 == 0 {
		t.Errorf("cmd/main 没有可执行位：mode=%o", mainScript.mode)
	}
	if !strings.Contains(string(mainScript.data), "unifi-center") {
		t.Error("cmd/main 没有指向包内二进制")
	}
	for _, name := range []string{"ICON.PNG", "ICON_256.PNG",
		"config/privilege", "config/resource", "wizard/install", "wizard/config",
		"cmd/install_callback", "cmd/config_callback", "cmd/uninstall_callback"} {
		if _, ok := members[name]; !ok {
			t.Errorf("包里缺少 %s", name)
		}
	}
	if ui, ok := inner["ui/config"]; !ok {
		t.Error("app.tgz 里缺少 ui/config")
	} else if !strings.Contains(string(ui.data), "/app/unificenter") {
		t.Error("入口配置里没有网关前缀")
	}
}

type tarMember struct {
	data []byte
	mode int64
}

func readTarGz(t *testing.T, path string) map[string]tarMember {
	t.Helper()
	handle, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开 %s 失败：%v", path, err)
	}
	defer handle.Close()
	gz, err := gzip.NewReader(handle)
	if err != nil {
		t.Fatalf("%s 不是 gzip：%v", path, err)
	}
	defer gz.Close()
	return collect(t, gz)
}

func readTarGzBytes(t *testing.T, data []byte) map[string]tarMember {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("app.tgz 不是 gzip：%v", err)
	}
	defer gz.Close()
	return collect(t, gz)
}

func collect(t *testing.T, reader io.Reader) map[string]tarMember {
	t.Helper()
	members := map[string]tarMember{}
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读 tar 失败：%v", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		payload, err := io.ReadAll(archive)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", header.Name, err)
		}
		members[header.Name] = tarMember{data: payload, mode: header.Mode}
	}
	return members
}

func binaryEndian(pair []byte) uint16 { return binary.LittleEndian.Uint16(pair) }

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// hasInterp 解析 ELF 程序头，判断是否存在 PT_INTERP（静态链接的 Go 二进制没有）。
func hasInterp(data []byte) bool {
	if len(data) < 64 {
		return false
	}
	phoff := int(binary.LittleEndian.Uint64(data[32:40]))
	phentsize := int(binary.LittleEndian.Uint16(data[54:56]))
	phnum := int(binary.LittleEndian.Uint16(data[56:58]))
	for index := 0; index < phnum; index++ {
		offset := phoff + index*phentsize
		if offset+4 > len(data) {
			return false
		}
		if binary.LittleEndian.Uint32(data[offset:offset+4]) == 3 { // PT_INTERP
			return true
		}
	}
	return false
}
