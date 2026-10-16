package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 全屏按钮：fnOS 的窗口大小由桌面外壳决定（应用包里没有窗口尺寸字段），
// 所以应用侧只能给出"一键放大"的能力。这里守住这条链路别被改没了：
// 软中心页面注入的按钮列要有 ⚙，并且会加载 fullscreen.js；面板也要有全屏按钮。
func TestFullscreenControlsWired(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "app", "ui", name))
		if err != nil {
			t.Fatalf("读 %s 失败：%v", name, err)
		}
		return string(data)
	}

	fullscreen := read("assets/fullscreen.js")
	// 走标准 API，并对老内核留 webkit 兜底
	for _, want := range []string{"requestFullscreen", "webkitRequestFullscreen",
		"exitFullscreen", "data-fs-target", "currentScript", "全屏"} {
		if !strings.Contains(fullscreen, want) {
			t.Errorf("fullscreen.js 里缺少 %q", want)
		}
	}
	// 不猜宿主方法名（猜错了没意义，还会误导用户）
	if strings.Contains(fullscreen, "hs_webCallAppHandler") {
		t.Error("fullscreen.js 不该去猜宿主桥的方法名")
	}

	overlay := read("assets/overlay.js")
	for _, want := range []string{"unifi-fab-col", "assets/fullscreen.js", "ctl/panel.html"} {
		if !strings.Contains(overlay, want) {
			t.Errorf("overlay.js 里缺少 %q", want)
		}
	}

	panel := read("panel.js")
	for _, want := range []string{"#panelActions", "fsInline", "fullscreen.js"} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel.js 里缺少 %q", want)
		}
	}
	panelHTML := read("panel.html")
	if !strings.Contains(panelHTML, `id="panelActions"`) {
		t.Error("panel.html 里没有 #panelActions（全屏按钮的落点）")
	}

	// 面板要被 Static 白名单放行（否则浏览器取不到，按钮就没了）
	if !strings.Contains(panel, "loadFullscreenButton();") {
		t.Error("panel.js 没有在初始化时挂载全屏按钮")
	}
}

// 设置面板要能适应小窗口（外壳大小不可控）：必须有紧凑布局的媒体查询。
func TestPanelAdaptsToSmallWindows(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "app", "ui", "panel.css"))
	if err != nil {
		t.Fatalf("读 panel.css 失败：%v", err)
	}
	css := string(data)
	for _, want := range []string{"max-height: 760px", "max-width: 560px", "max-width: 420px"} {
		if !strings.Contains(css, want) {
			t.Errorf("panel.css 里缺少 %q（小窗口/窄窗口自适应）", want)
		}
	}
	// 面板里的全屏按钮样式在 fullscreen.js 里（.unifi-fs.is-panel 跟随面板配色）
	fullscreen, err := os.ReadFile(filepath.Join("..", "app", "ui", "assets", "fullscreen.js"))
	if err != nil {
		t.Fatalf("读 fullscreen.js 失败：%v", err)
	}
	if !strings.Contains(string(fullscreen), ".unifi-fs.is-panel") {
		t.Error("fullscreen.js 里缺少 .unifi-fs.is-panel 样式")
	}
}

// 应用显示名：manifest 的 display_name 是唯一来源，页面与兜底页都要跟着它变。
// 注意 appname 不能动（官方要求升级间保持稳定，否则等于换了个应用、不能覆盖安装）。
func TestDisplayNameFromManifest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "manifest"))
	if err != nil {
		t.Fatalf("读 manifest 失败：%v", err)
	}
	want := ""
	for _, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(key) == "display_name" {
			want = strings.TrimSpace(value)
		}
	}
	if want == "" {
		t.Fatal("manifest 里没有 display_name")
	}
	if want != "unifi 软件中心" {
		t.Errorf("显示名 = %q，期望 \"unifi 软件中心\"", want)
	}
	if got := displayName(); got != want {
		t.Errorf("displayName() = %q，期望 %q", got, want)
	}

	// 用户可见的几个位置都得是新名字
	for _, item := range []struct{ path, needle string }{
		{"app/ui/panel.html", "<title>unifi 软件中心 · 设置</title>"},
		{"app/ui/panel.html", `<strong id="brandTitle">unifi 软件中心</strong>`},
		{"app/ui/config", `"title": "unifi 软件中心"`},
		{"app/ui/assets/overlay.js", "unifi 软件中心 · 设置"},
		{"wizard/install", `"stepTitle": "unifi 软件中心"`},
	} {
		body, err := os.ReadFile(filepath.Join("..", item.path))
		if err != nil {
			t.Fatalf("读 %s 失败：%v", item.path, err)
		}
		if !strings.Contains(string(body), item.needle) {
			t.Errorf("%s 里没有 %q", item.path, item.needle)
		}
	}

	// appname 与网关前缀必须保持不变（改了就不能覆盖安装）
	if !strings.Contains(string(data), "appname=unificenter") {
		t.Error("appname 被改动了（官方要求升级间保持稳定）")
	}
	entry, err := os.ReadFile(filepath.Join("..", "app", "ui", "config"))
	if err != nil {
		t.Fatalf("读入口配置失败：%v", err)
	}
	if !strings.Contains(string(entry), `"gatewayPrefix": "/app/unificenter"`) {
		t.Error("网关前缀被改动了")
	}
}
