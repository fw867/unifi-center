package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const validToken = "abcdef1234567890"

const fakePage = `<!doctype html>
<html><head><title>UniFi SoftCenter - 安全管理中心</title>
<script src="https://cdn.tailwindcss.com"></script>
<script src="https://cdn.jsdelivr.net/npm/vue@3.4.21/dist/vue.global.prod.js"></script>
<script src="https://cdn.jsdelivr.net/npm/lucide@0.363.0/dist/umd/lucide.js"></script>
<script src="https://cdn.example.com/other.js"></script>
</head><body><div id="app">soft center</div>
<script>fetch('https://api.github.com/repos/x/y/releases');</script>
</body></html>`

// ---------------------------------------------------------------- 假路由器

type fakeRouter struct {
	*httptest.Server
	token    string
	mu       sync.Mutex
	requests []routerHit
}

type routerHit struct {
	method        string
	path          string
	authorization string
}

func newFakeRouter(t *testing.T) *fakeRouter {
	t.Helper()
	router := &fakeRouter{token: validToken}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/system/info", func(w http.ResponseWriter, r *http.Request) {
		router.record(r)
		// 与真实软件中心一致：Authorization 里带的就是整串令牌（原插件运行时也是整串）
		if r.Header.Get("Authorization") != router.token {
			writeJSONResponse(w, http.StatusUnauthorized, map[string]any{"error": "令牌不正确"})
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"Device": "R619AC", "Version": "3.0.1"})
	})
	mux.HandleFunc("/repos/x/y/releases", func(w http.ResponseWriter, r *http.Request) {
		router.record(r)
		writeJSONResponse(w, http.StatusOK, map[string]any{"proxied": true, "path": r.URL.Path})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		router.record(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, fakePage)
	})
	router.Server = httptest.NewServer(mux)
	t.Cleanup(router.Close)
	return router
}

// rewriteHost 把假路由器的地址换成另一个（用于模拟"页面里的 GitHub 上游"）。
func (f *fakeRouter) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, routerHit{
		method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization")})
}

func (f *fakeRouter) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *fakeRouter) last() routerHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return routerHit{}
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeRouter) hits() []routerHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]routerHit(nil), f.requests...)
}

func writeJSONResponse(w http.ResponseWriter, status int, payload any) {
	data, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// listenUnix 只在支持 Unix Socket 的平台上可用（Windows 上相关用例会被跳过）。
func listenUnix(path string) (io.Closer, error) {
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return listener, nil
}

// ---------------------------------------------------------------- 测试环境

type harness struct {
	t       *testing.T
	env     Env
	store   *Store
	service *Service
	logger  *Logger
	router  *fakeRouter
	server  *httptest.Server
}

func setupHarness(t *testing.T) *harness {
	t.Helper()
	configDir := t.TempDir()
	uiDir, err := filepath.Abs(filepath.Join("..", "app", "ui"))
	if err != nil {
		t.Fatalf("解析 UI 目录失败：%v", err)
	}
	router := newFakeRouter(t)

	logger := NewLogger(filepath.Join(configDir, logFileName))
	env := Env{
		AppName:     "unificenter",
		ConfigDir:   configDir,
		UIDir:       uiDir,
		SocketPath:  filepath.Join(configDir, "app.sock"),
		DefaultTarg: "http://192.168.1.1:9958/",
		GatewayPfx:  "/app/unificenter",
		ProbeSec:    3,
		GitHubSec:   10,
		RouterSec:   30,
	}
	store := NewStore(configDir)
	service := NewService(env, store, logger)
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)

	h := &harness{t: t, env: env, store: store, service: service, logger: logger,
		router: router, server: server}
	h.reset()
	return h
}

func (h *harness) reset() {
	h.router.reset()
	if err := h.store.SaveToken(""); err != nil {
		h.t.Fatalf("清除令牌失败：%v", err)
	}
	_ = os.Remove(h.store.SettingsPath())
	if _, err := h.store.SaveTarget(h.router.URL, h.env.DefaultTarg); err != nil {
		h.t.Fatalf("写入地址失败：%v", err)
	}
}

func (h *harness) saveToken(token string) {
	if err := h.store.SaveToken(token); err != nil {
		h.t.Fatalf("保存令牌失败：%v", err)
	}
}

var (
	adminHeaders = map[string]string{
		"X-Trim-Userid": "1000", "X-Trim-Username": "admin", "X-Trim-Isadmin": "true"}
	userHeaders = map[string]string{
		"X-Trim-Userid": "1001", "X-Trim-Username": "bob", "X-Trim-Isadmin": "false"}
)

// do 发一个请求（相对被测服务的路径），返回状态码与正文。
func (h *harness) do(path, method string, body string, headers map[string]string) (int, []byte, http.Header) {
	h.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("构造请求失败：%v", err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		h.t.Fatalf("请求 %s 失败：%v", path, err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	return response.StatusCode, payload, response.Header
}

func (h *harness) doJSON(path, method, body string, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	status, payload, _ := h.do(path, method, body, headers)
	parsed := map[string]any{}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &parsed)
	}
	return status, parsed
}

// ---------------------------------------------------------------- 目标地址

func TestNormalizeTargetForms(t *testing.T) {
	cases := map[string]string{
		"192.168.1.1:9958":                   "http://192.168.1.1:9958/",
		"http://192.168.1.1:9958":            "http://192.168.1.1:9958/",
		"https://router.lan/openwrt/":        "https://router.lan/",
		"  http://192.168.1.1:9958/index.html  ": "http://192.168.1.1:9958/",
		"HTTP://Router.LAN:8080/x":           "http://Router.LAN:8080/",
	}
	for input, want := range cases {
		got, err := NormalizeTarget(input)
		if err != nil {
			t.Fatalf("NormalizeTarget(%q) 报错：%v", input, err)
		}
		if got != want {
			t.Errorf("NormalizeTarget(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestNormalizeTargetRejectsBadValues(t *testing.T) {
	for _, bad := range []string{"", "   ", "ftp://x/", "http://host:abc/", "http:///x",
		"http://host:99999x/", "http://host:/"} {
		if got, err := NormalizeTarget(bad); err == nil {
			t.Errorf("NormalizeTarget(%q) 应当报错，却返回 %q", bad, got)
		}
	}
}

func TestSettingsRoundTripAndDefault(t *testing.T) {
	h := setupHarness(t)
	if err := os.Remove(h.store.SettingsPath()); err != nil && !os.IsNotExist(err) {
		t.Fatalf("删除设置失败：%v", err)
	}
	if got := h.store.LoadSettings(h.env.DefaultTarg); got != h.env.DefaultTarg {
		t.Errorf("无配置时应回退默认值，得到 %q", got)
	}
	if _, err := h.store.SaveTarget("router.lan:8080", h.env.DefaultTarg); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if got := h.store.LoadSettings(h.env.DefaultTarg); got != "http://router.lan:8080/" {
		t.Errorf("保存后读回 = %q", got)
	}
	// 损坏的配置文件应回退默认值，而不是让服务崩掉
	if err := os.WriteFile(h.store.SettingsPath(), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("写入损坏配置失败：%v", err)
	}
	if got := h.store.LoadSettings(h.env.DefaultTarg); got != h.env.DefaultTarg {
		t.Errorf("损坏配置应回退默认值，得到 %q", got)
	}
}

func TestTokenFilePermissionsAndHint(t *testing.T) {
	h := setupHarness(t)
	h.saveToken("a1b2c3d4e5f6")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(h.store.TokenPath())
		if err != nil {
			t.Fatalf("令牌文件不存在：%v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("令牌文件权限 = %o，期望 600", mode)
		}
	}
	if got := h.store.Token(); got != "a1b2c3d4e5f6" {
		t.Errorf("读回令牌 = %q", got)
	}
	hint := TokenHint(h.store.Token())
	if hint != "a1b2…f6" {
		t.Errorf("令牌提示 = %q", hint)
	}
	if strings.Contains(hint, "c3d4") {
		t.Errorf("令牌提示泄露了令牌内容：%q", hint)
	}
	if TokenHint("短") != "已设置" {
		t.Errorf("短令牌提示应当只说已设置，得到 %q", TokenHint("短"))
	}
}

func TestTokenCanBeCleared(t *testing.T) {
	h := setupHarness(t)
	h.saveToken("secret-value")
	if err := h.store.SaveToken(""); err != nil {
		t.Fatalf("清除失败：%v", err)
	}
	if h.store.Token() != "" {
		t.Error("清除后仍能读到令牌")
	}
	if _, err := os.Stat(h.store.TokenPath()); !os.IsNotExist(err) {
		t.Error("清除后令牌文件仍然存在")
	}
}

// ---------------------------------------------------------------- Authorization

func TestAuthorizationDecisions(t *testing.T) {
	h := setupHarness(t)
	noop := func(string, ...any) {}
	h.saveToken("saved-token")

	cases := []struct{ input, want, why string }{
		{"Bearer user-token", "Bearer user-token", "真实令牌必须原样透传"},
		{"saved-token-2", "saved-token-2", "非 Bearer 方案也原样透传"},
		{"Bearer console-loopback", "saved-token", "控制台占位值换保存的令牌"},
		{"bearer   console-loopback  ", "saved-token", "大小写与空格容错"},
		{"console-loopback", "saved-token", "裸值（无方案）也认"},
		{"", "saved-token", "缺 Authorization 时用保存的令牌"},
	}
	for _, item := range cases {
		if got := ResolveAuthorization(item.input, "saved-token", noop); got != item.want {
			t.Errorf("ResolveAuthorization(%q) = %q，期望 %q（%s）", item.input, got, item.want, item.why)
		}
	}
	// 没保存令牌时，占位值与空值都不能变出令牌来
	if got := ResolveAuthorization("Bearer console-loopback", "", noop); got != "" {
		t.Errorf("没保存令牌时不应送出占位值，得到 %q", got)
	}
	if got := ResolveAuthorization("", "", noop); got != "" {
		t.Errorf("没保存令牌时应不带 Authorization，得到 %q", got)
	}
	// 占位值判定本身
	if !IsConsolePlaceholder("Bearer console-loopback") || !IsConsolePlaceholder("console-loopback") {
		t.Error("占位值判定漏判")
	}
	if IsConsolePlaceholder("Basic dXNlcg==") || IsConsolePlaceholder("Bearer real-token") {
		t.Error("占位值判定误判")
	}
	_ = h
}

// ---------------------------------------------------------------- 网关身份

func TestGatewayIdentity(t *testing.T) {
	admin := GatewayUserFrom(http.Header{
		"X-Trim-Userid": []string{"1000"}, "X-Trim-Username": []string{"admin"},
		"X-Trim-Isadmin": []string{"true"}})
	if !admin.ViaGateway || !admin.IsAdmin {
		t.Fatalf("管理员身份识别失败：%+v", admin)
	}
	if !CanRevealToken(admin) || !CanChangeSettings(admin) || admin.Label != "admin" {
		t.Errorf("管理员权限判定失败：%+v", admin)
	}

	bob := GatewayUserFrom(http.Header{
		"X-Trim-Userid": []string{"1001"}, "X-Trim-Username": []string{"bob"},
		"X-Trim-Isadmin": []string{"false"}})
	if bob.IsAdmin || CanRevealToken(bob) || CanChangeSettings(bob) {
		t.Errorf("普通用户不应有管理员权限：%+v", bob)
	}

	// 没有网关头（本机调试端口）：失败关闭，绝不能凭客户端自称管理员
	debug := GatewayUserFrom(http.Header{"X-Trim-Isadmin": []string{"true"}})
	if debug.ViaGateway || debug.IsAdmin || CanRevealToken(debug) {
		t.Errorf("无网关头时必须失败关闭：%+v", debug)
	}
	if debug.Label != localEntryLabel {
		t.Errorf("调试模式标签 = %q", debug.Label)
	}

	// 网关在、但没给 Isadmin（老版本或异常）：也不给管理员
	partial := GatewayUserFrom(http.Header{"X-Trim-Username": []string{"admin"}})
	if !partial.ViaGateway || partial.IsAdmin {
		t.Errorf("缺 Isadmin 头时不应视为管理员：%+v", partial)
	}

	cases := map[string]bool{"true": true, " TRUE ": true, "yes": false, "1": false, "": false}
	for value, want := range cases {
		got := GatewayUserFrom(http.Header{
			"X-Trim-Userid": []string{"1"}, "X-Trim-Isadmin": []string{value}}).IsAdmin
		if got != want {
			t.Errorf("Isadmin=%q 解析为 %v，期望 %v", value, got, want)
		}
	}
}

// ---------------------------------------------------------------- 探测

func TestProbeRouter(t *testing.T) {
	h := setupHarness(t)
	client := NewHTTPClient(3*time.Second, false)

	t.Run("无令牌时可达但未授权", func(t *testing.T) {
		h.reset()
		result := ProbeRouter(client, h.router.URL, "")
		if !result.Reachable || result.Status != 200 {
			t.Fatalf("应当可达：%+v", result)
		}
		if result.Authenticated {
			t.Error("没有令牌不应判定为已授权")
		}
		if !strings.Contains(result.Title, "SoftCenter") {
			t.Errorf("标题解析失败：%q", result.Title)
		}
		for _, hit := range h.router.hits() {
			if hit.method != http.MethodGet {
				t.Errorf("探测必须是只读 GET，出现 %s %s", hit.method, hit.path)
			}
		}
	})

	t.Run("正确令牌", func(t *testing.T) {
		h.reset()
		result := ProbeRouter(client, h.router.URL, validToken)
		if !result.Authenticated || result.Device != "R619AC" || result.Version != "3.0.1" {
			t.Errorf("带正确令牌的探测结果不对：%+v", result)
		}
	})

	t.Run("错误令牌", func(t *testing.T) {
		h.reset()
		result := ProbeRouter(client, h.router.URL, "wrong-token")
		if result.Authenticated {
			t.Error("错误令牌不应判定为已授权")
		}
		if result.Error != "令牌无效" {
			t.Errorf("错误信息 = %q", result.Error)
		}
	})

	t.Run("不可达目标", func(t *testing.T) {
		result := ProbeRouter(client, "http://127.0.0.1:1/", "")
		if result.Reachable || result.Error == "" {
			t.Errorf("不可达目标的结果不对：%+v", result)
		}
	})

	t.Run("带路径的目标也要能探测", func(t *testing.T) {
		// 用户填了 http://host:port/x 会被规整成 http://host:port/，这里验证请求确实打到根
		h.reset()
		result := ProbeRouter(client, h.router.URL+"/", validToken)
		if !result.Reachable {
			t.Errorf("带尾斜杠的目标应当可达：%+v", result)
		}
	})
}

// ---------------------------------------------------------------- 页面重写

func TestRewritePage(t *testing.T) {
	rewritten, unknown := RewritePage(fakePage, "/app/unificenter", "", GitHubRoutes())
	for _, want := range []string{
		"/app/unificenter/assets/tailwind.js",
		"/app/unificenter/assets/vue.js",
		"/app/unificenter/assets/lucide.js",
		"/app/unificenter/github/api/",
	} {
		if !strings.Contains(rewritten, want) {
			t.Errorf("重写结果缺少 %q\n%s", want, rewritten)
		}
	}
	for _, unwanted := range []string{"cdn.tailwindcss.com", "cdn.jsdelivr.net", "https://api.github.com/"} {
		if strings.Contains(rewritten, unwanted) {
			t.Errorf("重写结果仍含 %q", unwanted)
		}
	}
	if len(unknown) != 1 || unknown[0] != "https://cdn.example.com/other.js" {
		t.Errorf("未本地化的脚本列表 = %v", unknown)
	}
}

func TestPreparePageInjectsTokenAndFavicon(t *testing.T) {
	rewritten, _ := PreparePage(fakePage, "/app/unificenter", "", "tok-123", GitHubRoutes())
	if !strings.Contains(rewritten, `localStorage.setItem("sc_token","tok-123")`) {
		t.Error("没有注入令牌")
	}
	if !strings.Contains(rewritten, "/app/unificenter/assets/icon.png") {
		t.Error("没有注入 favicon")
	}
	without, _ := PreparePage(fakePage, "/app/unificenter", "", "", GitHubRoutes())
	if strings.Contains(without, "sc_token") {
		t.Error("没有令牌时不应注入")
	}
	// 令牌里的引号/反斜杠必须被 JSON 转义，不能拼出可注入的 HTML
	escaped, _ := PreparePage(fakePage, "/p", "", `a"b\c</script>`, GitHubRoutes())
	if strings.Contains(escaped, `"a"b\c</script>"`) {
		t.Errorf("令牌注入没有转义：%s", escaped)
	}
}

func TestGitHubUpstreamMapping(t *testing.T) {
	routes := GitHubRoutes()
	cases := map[string]string{
		"github/api/repos/x/y": "https://api.github.com/repos/x/y",
		"github/raw/a/b":       "https://raw.githubusercontent.com/a/b",
		"other/path":           "",
		"github/nope":          "",
	}
	for input, want := range cases {
		if got := GitHubUpstream(input, routes); got != want {
			t.Errorf("GitHubUpstream(%q) = %q，期望 %q", input, got, want)
		}
	}
	// 页面里两种写法（带/不带尾斜杠）都要能被替换
	html := `<script src="https://api.github.com"></script>` +
		`<script src="https://raw.githubusercontent.com/x"></script>`
	rewritten, _ := RewritePage(html, "/app/unificenter", "", routes)
	if strings.Contains(rewritten, "api.github.com") || strings.Contains(rewritten, "raw.githubusercontent.com") {
		t.Errorf("无尾斜杠的 GitHub 地址没有被替换：%s", rewritten)
	}
}

// ---------------------------------------------------------------- HTTP 接口

func TestHealthz(t *testing.T) {
	h := setupHarness(t)
	status, payload := h.doJSON("/healthz", http.MethodGet, "", nil)
	if status != 200 || payload["ok"] != true || payload["app"] != "unificenter" {
		t.Fatalf("健康检查返回不对：%d %v", status, payload)
	}
	if payload["version"] != appVersion {
		t.Errorf("版本号 = %v", payload["version"])
	}
}

func TestStatusEndpoint(t *testing.T) {
	h := setupHarness(t)
	h.saveToken(validToken)
	status, payload := h.doJSON("/ctl/status", http.MethodGet, "", adminHeaders)
	if status != 200 {
		t.Fatalf("状态接口 = %d", status)
	}
	router, _ := payload["router"].(map[string]any)
	if router["reachable"] != true || router["device"] != "R619AC" {
		t.Errorf("探测结果不对：%v", router)
	}
	if payload["token_set"] != true || payload["gateway_prefix"] != "/app/unificenter" {
		t.Errorf("状态字段不对：%v", payload)
	}
	assets, _ := payload["assets"].(map[string]any)
	if assets["assets/vue.js"] != true {
		t.Errorf("本地资源状态不对：%v", assets)
	}
}

func TestSettingsVisibility(t *testing.T) {
	h := setupHarness(t)
	h.saveToken(validToken)

	t.Run("管理员看到明文", func(t *testing.T) {
		status, payload := h.doJSON("/ctl/settings", http.MethodGet, "", adminHeaders)
		if status != 200 || payload["token"] != validToken || payload["token_visible"] != true {
			t.Fatalf("管理员应看到明文：%d %v", status, payload)
		}
		if payload["can_edit"] != true {
			t.Errorf("管理员应当可以编辑：%v", payload)
		}
	})

	t.Run("普通用户只看到提示", func(t *testing.T) {
		status, payload := h.doJSON("/ctl/settings", http.MethodGet, "", userHeaders)
		if status != 200 {
			t.Fatalf("状态 = %d", status)
		}
		if payload["token"] != "" || payload["token_visible"] != false {
			t.Errorf("普通用户不应看到明文：%v", payload)
		}
		hint, _ := payload["token_hint"].(string)
		if hint == "" || strings.Contains(hint, validToken) {
			t.Errorf("提示字段不对：%q", hint)
		}
		if payload["can_edit"] != false {
			t.Errorf("普通用户不应可以编辑：%v", payload)
		}
	})

	t.Run("没有网关头时看不到明文", func(t *testing.T) {
		_, payload := h.doJSON("/ctl/settings", http.MethodGet, "", nil)
		if payload["token"] != "" || payload["token_visible"] != false {
			t.Errorf("无网关头不应看到明文：%v", payload)
		}
	})

	t.Run("自己伪造 Isadmin 头也无效（本机调试口）", func(t *testing.T) {
		_, payload := h.doJSON("/ctl/settings", http.MethodGet, "",
			map[string]string{"X-Trim-Isadmin": "true"})
		if payload["token_visible"] != false || payload["can_edit"] != false {
			t.Errorf("伪造管理员头必须无效：%v", payload)
		}
	})
}

func TestSettingsPostRequiresAdmin(t *testing.T) {
	h := setupHarness(t)
	before := h.store.LoadSettings(h.env.DefaultTarg)

	status, payload := h.doJSON("/ctl/settings", http.MethodPost,
		`{"target":"http://10.0.0.9:9958/"}`, userHeaders)
	if status != 403 || payload["ok"] != false {
		t.Fatalf("普通用户改设置应 403：%d %v", status, payload)
	}
	status, _ = h.doJSON("/ctl/settings", http.MethodPost, `{"target":"http://10.0.0.9:9958/"}`, nil)
	if status != 403 {
		t.Fatalf("无身份改设置应 403，得到 %d", status)
	}
	if h.store.LoadSettings(h.env.DefaultTarg) != before {
		t.Error("被拒绝的请求不应改动设置")
	}
}

func TestSettingsPostAsAdmin(t *testing.T) {
	h := setupHarness(t)
	status, payload := h.doJSON("/ctl/settings", http.MethodPost,
		`{"target":"http://10.0.0.9:9958","token":"new-token"}`, adminHeaders)
	if status != 200 {
		t.Fatalf("管理员改设置 = %d %v", status, payload)
	}
	if payload["target"] != "http://10.0.0.9:9958/" {
		t.Errorf("地址没有被规整：%v", payload["target"])
	}
	if h.store.Token() != "new-token" {
		t.Errorf("令牌没有被保存：%q", h.store.Token())
	}
	// 显式传空令牌表示清除
	status, _ = h.doJSON("/ctl/settings", http.MethodPost, `{"token":""}`, adminHeaders)
	if status != 200 || h.store.Token() != "" {
		t.Errorf("空令牌应当清除：%d %q", status, h.store.Token())
	}
}

func TestSettingsPostValidation(t *testing.T) {
	h := setupHarness(t)
	if status, _ := h.doJSON("/ctl/settings", http.MethodPost, `{not-json`, adminHeaders); status != 400 {
		t.Errorf("非法 JSON 应 400，得到 %d", status)
	}
	if status, _ := h.doJSON("/ctl/settings", http.MethodPost, `{"target":"ftp://nope/"}`, adminHeaders); status != 409 {
		t.Errorf("非法地址应 409，得到 %d", status)
	}
	if status, _ := h.doJSON("/ctl/settings", http.MethodPost, `"just-a-string"`, adminHeaders); status != 400 {
		t.Errorf("非对象请求体应 400，得到 %d", status)
	}
	if status, _ := h.doJSON("/ctl/settings", http.MethodGet, "", adminHeaders); status != 200 {
		t.Errorf("GET 设置应 200，得到 %d", status)
	}
}

func TestTokenEndpointAdminOnly(t *testing.T) {
	h := setupHarness(t)
	h.saveToken(validToken)
	status, payload := h.doJSON("/ctl/token", http.MethodGet, "", adminHeaders)
	if status != 200 || payload["token"] != validToken {
		t.Fatalf("管理员取令牌失败：%d %v", status, payload)
	}
	for name, headers := range map[string]map[string]string{"普通用户": userHeaders, "无身份": nil} {
		status, payload := h.doJSON("/ctl/token", http.MethodGet, "", headers)
		if status != 403 {
			t.Errorf("%s 取令牌应 403，得到 %d", name, status)
		}
		raw, _ := json.Marshal(payload)
		if strings.Contains(string(raw), validToken) {
			t.Errorf("%s 的响应里出现了明文令牌", name)
		}
	}
}

func TestLogsEndpoint(t *testing.T) {
	h := setupHarness(t)
	h.logger.Printf("测试日志一行")
	status, payload := h.doJSON("/ctl/logs", http.MethodGet, "", nil)
	if status != 200 {
		t.Fatalf("日志接口 = %d", status)
	}
	lines, _ := payload["lines"].([]any)
	found := false
	for _, line := range lines {
		if text, ok := line.(string); ok && strings.Contains(text, "测试日志一行") {
			found = true
		}
	}
	if !found {
		t.Errorf("日志里没有刚写的内容：%v", lines)
	}
}

func TestPanelAndAssetsAreServed(t *testing.T) {
	h := setupHarness(t)
	cases := map[string]string{
		"/ctl/panel.html":    `id="targetInput"`,
		"/ctl/panel.js":      "loadSettings",
		"/ctl/panel.css":     ".rc-body",
		"/assets/close.js":   "normal_goback",
		"/assets/overlay.js": "unifi-fab",
		"/assets/vue.js":     "Vue",
		"/images/icon_64.png": "\x89PNG",
	}
	for path, needle := range cases {
		status, body, headers := h.do(path, http.MethodGet, "", nil)
		if status != 200 {
			t.Errorf("%s = %d", path, status)
			continue
		}
		if !strings.Contains(string(body), needle) {
			t.Errorf("%s 内容里没有 %q", path, needle)
		}
		if strings.HasSuffix(path, ".js") && !strings.Contains(headers.Get("Content-Type"), "javascript") {
			t.Errorf("%s 的 Content-Type = %q", path, headers.Get("Content-Type"))
		}
	}
}

func TestPanelUsesRelativePaths(t *testing.T) {
	h := setupHarness(t)
	_, body, _ := h.do("/ctl/panel.html", http.MethodGet, "", nil)
	text := string(body)
	for _, want := range []string{`href="panel.css"`, `src="panel.js"`, `src="../assets/close.js"`} {
		if !strings.Contains(text, want) {
			t.Errorf("面板里缺少 %q", want)
		}
	}
	_, script, _ := h.do("/ctl/panel.js", http.MethodGet, "", nil)
	if !strings.Contains(string(script), "const API = '../ctl'") {
		t.Error("面板脚本没有用相对接口前缀")
	}
}

func TestUnknownRoutes(t *testing.T) {
	h := setupHarness(t)
	if status, payload := h.doJSON("/ctl/nope", http.MethodGet, "", nil); status != 404 || payload["ok"] != false {
		t.Errorf("未知接口应 404：%d %v", status, payload)
	}
	if status, _, _ := h.do("/assets/nope.js", http.MethodGet, "", nil); status != 404 {
		t.Errorf("未知静态资源应 404，得到 %d", status)
	}
	if status, _, _ := h.do("/ctl/status", http.MethodPost, "", nil); status != 405 {
		t.Errorf("错误方法应 405，得到 %d", status)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	h := setupHarness(t)
	for _, path := range []string{
		"/assets/../../manifest",
		"/assets/%2e%2e/%2e%2e/manifest",
		"/ctl/../panel.js",
		"/assets/..%2f..%2fmanifest",
		"/images/../../../ICON.PNG",
	} {
		status, body, _ := h.do(path, http.MethodGet, "", nil)
		if status != 400 && status != 403 && status != 404 {
			t.Errorf("%s 应当被拒绝，得到 %d", path, status)
		}
		if strings.Contains(string(body), "appname=") {
			t.Errorf("%s 泄露了包内文件", path)
		}
	}
}

func TestSoftCenterPageLocalizedAndTokenInjected(t *testing.T) {
	h := setupHarness(t)
	h.saveToken(validToken)
	status, body, _ := h.do("/", http.MethodGet, "", nil)
	if status != 200 {
		t.Fatalf("页面 = %d", status)
	}
	text := string(body)
	for _, want := range []string{
		"/app/unificenter/assets/vue.js",
		`localStorage.setItem("sc_token","` + validToken + `")`,
		"/app/unificenter/github/api/",
		"/app/unificenter/assets/close.js",
		"/app/unificenter/assets/overlay.js",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("页面里缺少 %q", want)
		}
	}
	if strings.Contains(text, "cdn.jsdelivr.net") {
		t.Error("页面里仍引用外部 CDN")
	}
}

func TestProxyAuthorizationRules(t *testing.T) {
	h := setupHarness(t)

	t.Run("保存的令牌被带上", func(t *testing.T) {
		h.reset()
		h.saveToken(validToken)
		status, payload := h.doJSON("/api/system/info", http.MethodGet, "", nil)
		if status != 200 || payload["Device"] != "R619AC" {
			t.Fatalf("转发失败：%d %v", status, payload)
		}
		if hit := h.router.last(); hit.authorization != validToken {
			t.Errorf("转发用的 Authorization = %q", hit.authorization)
		}
	})

	t.Run("客户端真令牌原样透传", func(t *testing.T) {
		h.reset()
		h.saveToken("saved-token")
		status, _ := h.doJSON("/api/system/info", http.MethodGet, "",
			map[string]string{"Authorization": validToken})
		if status != 200 {
			t.Fatalf("转发失败：%d", status)
		}
		if hit := h.router.last(); hit.authorization != validToken {
			t.Errorf("真令牌被覆盖成 %q", hit.authorization)
		}
	})

	t.Run("控制台占位值被换成保存的令牌", func(t *testing.T) {
		h.reset()
		h.saveToken(validToken)
		status, _ := h.doJSON("/api/system/info", http.MethodGet, "",
			map[string]string{"Authorization": "Bearer console-loopback"})
		if status != 200 {
			t.Fatalf("转发失败：%d", status)
		}
		if hit := h.router.last(); hit.authorization != validToken {
			t.Errorf("占位值没有被替换：%q", hit.authorization)
		}
	})

	t.Run("没保存令牌时占位值不会被送出去", func(t *testing.T) {
		h.reset()
		status, _ := h.doJSON("/api/system/info", http.MethodGet, "",
			map[string]string{"Authorization": "Bearer console-loopback"})
		if status != 401 {
			t.Errorf("应当原样收到 401，得到 %d", status)
		}
		if hit := h.router.last(); hit.authorization != "" {
			t.Errorf("不应送出占位值：%q", hit.authorization)
		}
	})

	t.Run("上游 4xx 原样回给页面", func(t *testing.T) {
		h.reset()
		status, payload := h.doJSON("/api/system/info", http.MethodGet, "", nil)
		if status != 401 {
			t.Fatalf("应当 401，得到 %d", status)
		}
		if payload["error"] != "令牌不正确" {
			t.Errorf("上游正文字段丢了：%v", payload)
		}
	})

	t.Run("上游连不上回 502", func(t *testing.T) {
		h.reset()
		if _, err := h.store.SaveTarget("http://127.0.0.1:1/", h.env.DefaultTarg); err != nil {
			t.Fatalf("写入地址失败：%v", err)
		}
		status, _ := h.doJSON("/api/system/info", http.MethodGet, "", nil)
		if status != 502 {
			t.Errorf("应当 502，得到 %d", status)
		}
	})
}

func TestViewCompatEntry(t *testing.T) {
	h := setupHarness(t)
	h.saveToken(validToken)
	if status, _ := h.doJSON("/view/api/system/info", http.MethodGet, "", nil); status != 200 {
		t.Errorf("/view/api 兼容入口 = %d", status)
	}
	status, body, _ := h.do("/view/", http.MethodGet, "", nil)
	if status != 200 || !strings.Contains(string(body), "sc_token") {
		t.Errorf("/view/ 兼容入口 = %d", status)
	}
	if status, _, _ := h.do("/view/panel.html", http.MethodGet, "", nil); status != 200 {
		t.Errorf("/view/panel.html = %d", status)
	}
	// /view/<其它> 按设计转发到路由器（原插件的兼容行为），所以不要求 404；
	// 这里只要求它确实是"转发"，即含前缀的路径也能落到同一条转发链上。
	status, _, _ = h.do("/view/api/system/info", http.MethodGet, "", nil)
	if status != 200 {
		t.Errorf("/view 转发链 = %d", status)
	}
}

func TestOfflinePageWhenRouterUnreachable(t *testing.T) {
	h := setupHarness(t)
	if _, err := h.store.SaveTarget("http://127.0.0.1:1/", h.env.DefaultTarg); err != nil {
		t.Fatalf("写入地址失败：%v", err)
	}
	status, body, _ := h.do("/", http.MethodGet, "", nil)
	if status != 502 {
		t.Fatalf("应当 502，得到 %d", status)
	}
	text := string(body)
	if !strings.Contains(text, "连不上") || !strings.Contains(text, "/app/unificenter/ctl/panel.html") {
		t.Errorf("兜底页面内容不对：%s", text)
	}
	// 标题与提示里的应用名来自 manifest 的 display_name（改了应用名这里跟着变）
	if name := displayName(); !strings.Contains(text, name) {
		t.Errorf("兜底页面里没有应用显示名 %q：%s", name, text)
	}
	// 兜底页引用的样式与关闭按钮必须真的能取到（否则是一张没样式、没关闭按钮的裸页面）
	for _, asset := range []string{"/panel.css", "/assets/close.js"} {
		status, body, _ := h.do(asset, http.MethodGet, "", nil)
		if status != 200 || len(body) == 0 {
			t.Errorf("兜底页依赖的 %s 取不到（HTTP %d）", asset, status)
		}
	}
}

// ---------------------------------------------------------------- 网关前缀

// 回归测试：有设备把 TRIM_APPDEST 指到应用根目录（target 的上一级），
// 曾经导致 cmd/main 报"缺少可执行文件 …/unificenter/bin/unifi-center"。
func TestResolveAppDestCandidates(t *testing.T) {
	clear := func() {
		for _, name := range []string{"TRIM_APPDEST", "TRIM_APPNAME"} {
			_ = os.Unsetenv(name)
		}
	}
	clear()
	t.Cleanup(clear)

	// 情况一：TRIM_APPDEST 直接就是应用目录（实测踩到的布局）
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "bin", binaryName)
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ui"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ui", "config"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRIM_APPDEST", root)
	t.Setenv("TRIM_APPNAME", "unificenter")
	if got := ResolveAppDest(); got != root {
		t.Errorf("ResolveAppDest() = %q，期望 %q", got, root)
	}
	env := LoadEnv()
	if env.AppRoot != root {
		t.Errorf("AppRoot = %q，期望 %q", env.AppRoot, root)
	}
	if env.UIDir != filepath.Join(root, "ui") {
		t.Errorf("UIDir = %q", env.UIDir)
	}
	// Socket 必须严格跟着 TRIM_APPDEST（网关按它找 socket，不能跟着候选跑）
	if env.SocketPath != filepath.Join(root, socketName) {
		t.Errorf("SocketPath = %q，期望 %q", env.SocketPath, filepath.Join(root, socketName))
	}

	// 情况二：TRIM_APPDEST 是应用根，二进制在它的 target/ 下（官方文档的布局）
	docsRoot := t.TempDir()
	dest := filepath.Join(docsRoot, "target")
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "bin", binaryName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRIM_APPDEST", docsRoot)
	if got := ResolveAppDest(); got != dest {
		t.Errorf("ResolveAppDest() = %q，期望 %q（应回退到 target 子目录）", got, dest)
	}
	env = LoadEnv()
	if env.SocketPath != filepath.Join(docsRoot, socketName) {
		t.Errorf("SocketPath = %q，期望仍在 TRIM_APPDEST 下", env.SocketPath)
	}

	// 情况三：两者都不存在时退回第一个候选，并把候选列表暴露出来供排障
	t.Setenv("TRIM_APPDEST", filepath.Join(docsRoot, "nope"))
	if got := ResolveAppDest(); got != filepath.Join(docsRoot, "nope") {
		t.Errorf("ResolveAppDest() = %q，期望退回第一个候选", got)
	}
	candidates := AppDestCandidates()
	if len(candidates) < 3 {
		t.Errorf("候选列表太短：%v", candidates)
	}
	if candidates[0] != filepath.Join(docsRoot, "nope") {
		t.Errorf("第一个候选应是 TRIM_APPDEST，得到 %q", candidates[0])
	}
}

func TestMainScriptDetectsLayout(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "cmd", "main"))
	if err != nil {
		t.Fatalf("读 cmd/main 失败：%v", err)
	}
	text := string(script)
	// 必须真的做候选解析，而不是认死一种布局
	for _, want := range []string{"resolve_app_root", "app_candidates", "describe_layout", "doctor"} {
		if !strings.Contains(text, want) {
			t.Errorf("cmd/main 里缺少 %q", want)
		}
	}
	if strings.Contains(text, `BIN="$APPDEST/bin/unifi-center"`) {
		t.Error("cmd/main 又回到了只认一种布局的老写法")
	}
	if !strings.Contains(text, `SOCKET="$APPDEST/app.sock"`) {
		t.Error("Socket 必须跟着 TRIM_APPDEST 走")
	}
	for _, callback := range []string{"install_callback", "config_callback", "upgrade_callback"} {
		body, err := os.ReadFile(filepath.Join("..", "cmd", callback))
		if err != nil {
			t.Fatalf("读 cmd/%s 失败：%v", callback, err)
		}
		if !strings.Contains(string(body), `"$APPDEST/target"`) {
			t.Errorf("cmd/%s 没有考虑 target 子目录这种布局", callback)
		}
	}
}

func TestGatewayPrefixShape(t *testing.T) {
	h := setupHarness(t)
	h.saveToken(validToken)
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	t.Run("带前缀的页面与前缀等价", func(t *testing.T) {
		response, err := client.Get(h.server.URL + "/app/unificenter/")
		if err != nil {
			t.Fatalf("请求失败：%v", err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 {
			t.Fatalf("状态 = %d", response.StatusCode)
		}
		if !strings.Contains(string(body), "/app/unificenter/assets/vue.js") {
			t.Error("带前缀的页面没有本地化资源")
		}
	})

	t.Run("前缀本身 301 到带斜杠", func(t *testing.T) {
		response, err := client.Get(h.server.URL + "/app/unificenter")
		if err != nil {
			t.Fatalf("请求失败：%v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != 301 {
			t.Fatalf("状态 = %d", response.StatusCode)
		}
		if location := response.Header.Get("Location"); location != "/app/unificenter/" {
			t.Errorf("Location = %q", location)
		}
	})

	t.Run("带前缀的接口按网关头判权限", func(t *testing.T) {
		for _, item := range []struct {
			headers map[string]string
			canEdit string
		}{{adminHeaders, "true"}, {userHeaders, "false"}} {
			request, _ := http.NewRequest(http.MethodGet, h.server.URL+"/app/unificenter/ctl/settings", nil)
			for name, value := range item.headers {
				request.Header.Set(name, value)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("请求失败：%v", err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if !strings.Contains(string(body), `"can_edit":`+item.canEdit) {
				t.Errorf("权限判定不对：%s", body)
			}
			if item.headers["X-Trim-Isadmin"] != "true" && strings.Contains(string(body), validToken) {
				t.Error("普通用户的响应里出现了明文令牌")
			}
		}
	})
}

// ---------------------------------------------------------------- GitHub 转发

func TestGitHubProxy(t *testing.T) {
	h := setupHarness(t)
	// 把 GitHub 上游指向假路由器（页面里的 GitHub 地址由它提供）
	original := os.Getenv("GITHUB_API_BASE")
	if err := os.Setenv("GITHUB_API_BASE", h.router.URL); err != nil {
		t.Fatalf("设置环境变量失败：%v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("GITHUB_API_BASE", original) })
	if original == "" {
		t.Cleanup(func() { _ = os.Unsetenv("GITHUB_API_BASE") })
	}

	t.Run("重写后的地址能被自己接住", func(t *testing.T) {
		page := fmt.Sprintf(`<script>fetch("%s/repos/x/y/releases");</script>`, h.router.URL)
		rewritten, unknown := RewritePage(page, "/app/unificenter", "", GitHubRoutes())
		if len(unknown) != 0 {
			t.Fatalf("应当被本地化：%v", unknown)
		}
		if strings.Contains(rewritten, h.router.URL) {
			t.Fatalf("上游地址没有被替换：%s", rewritten)
		}
		start := strings.Index(rewritten, "/app/unificenter/github/")
		if start < 0 {
			t.Fatalf("重写结果里没有本应用的转发地址：%s", rewritten)
		}
		end := strings.Index(rewritten[start:], `"`)
		if end < 0 {
			t.Fatalf("重写结果格式不对：%s", rewritten)
		}
		local := strings.TrimPrefix(rewritten[start:start+end], "/app/unificenter")
		status, payload := h.doJSON(local, http.MethodGet, "", nil)
		if status != 200 || payload["proxied"] != true {
			t.Fatalf("转发失败：%d %v（路径 %s）", status, payload, local)
		}
	})

	t.Run("转发不带 Authorization", func(t *testing.T) {
		h.reset()
		h.saveToken(validToken)
		status, payload := h.doJSON("/github/api/repos/x/y/releases", http.MethodGet, "",
			map[string]string{"Authorization": "Bearer console-loopback"})
		if status != 200 || payload["proxied"] != true {
			t.Fatalf("转发失败：%d %v", status, payload)
		}
		hit := h.router.last()
		if hit.authorization != "" {
			t.Errorf("GitHub 转发带上了 Authorization：%q", hit.authorization)
		}
		if hit.method != http.MethodGet || hit.path != "/repos/x/y/releases" {
			t.Errorf("转发到了错误的位置：%s %s", hit.method, hit.path)
		}
	})

	t.Run("未知路径 404", func(t *testing.T) {
		if status, _ := h.doJSON("/github/unknown/path", http.MethodGet, "", nil); status != 404 {
			t.Errorf("状态 = %d", status)
		}
	})

	t.Run("POST 被拒绝", func(t *testing.T) {
		if status, _ := h.doJSON("/github/api/repos/x/y/releases", http.MethodPost, "{}", nil); status != 405 {
			t.Errorf("状态 = %d", status)
		}
	})
}

// ---------------------------------------------------------------- 环境变量

func TestLoadEnv(t *testing.T) {
	clear := func() {
		for _, name := range []string{"TRIM_APPNAME", "TRIM_PKGETC", "TRIM_PKGVAR", "TRIM_APPDEST",
			"UI_DIR", "SOCKET_PATH", "ROUTER_TARGET", "GATEWAY_PREFIX", "UNIFICENTER_DEV_PORT"} {
			_ = os.Unsetenv(name)
		}
	}
	clear()
	t.Cleanup(clear)

	t.Setenv("TRIM_APPNAME", "unificenter")
	t.Setenv("TRIM_PKGETC", "/vol1/@appconf/unificenter")
	t.Setenv("TRIM_APPDEST", "/vol1/@appcenter/unificenter/target")
	env := LoadEnv()
	if env.ConfigDir != "/vol1/@appconf/unificenter" {
		t.Errorf("ConfigDir = %q", env.ConfigDir)
	}
	// 路径断言统一用 filepath.Join，避免 Windows/Linux 分隔符差异
	wantUIDir := filepath.Join("/vol1/@appcenter/unificenter/target", "ui")
	if env.UIDir != wantUIDir {
		t.Errorf("UIDir = %q，期望 %q", env.UIDir, wantUIDir)
	}
	wantSocket := filepath.Join("/vol1/@appcenter/unificenter/target", "app.sock")
	if env.SocketPath != wantSocket {
		t.Errorf("SocketPath = %q，期望 %q", env.SocketPath, wantSocket)
	}
	if !env.SocketOnly {
		t.Error("有 TRIM_APPDEST 时应为 Socket 模式")
	}
	if env.GatewayPfx != "/app/unificenter" {
		t.Errorf("GatewayPfx = %q", env.GatewayPfx)
	}
	if env.DefaultTarg != "http://192.168.1.1:9958/" {
		t.Errorf("默认目标 = %q", env.DefaultTarg)
	}
}

func TestSocketReadyAndStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.sock")
	if SocketReady(path) {
		t.Error("不存在的 Socket 不应判定为就绪")
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		listener, err := listenUnix(path)
		if err != nil {
			t.Fatalf("创建测试 Socket 失败：%v", err)
		}
		defer listener.Close()
		if !SocketReady(path) {
			t.Error("正在监听的 Socket 应当判定为就绪")
		}
		// 有活着的服务时不能删
		if UnlinkStaleSocket(path) {
			t.Error("活着服务的 Socket 不应被判定为陈旧并删除")
		}
		listener.Close()
		// 关掉之后就是陈旧的，应能清理
		if !UnlinkStaleSocket(path) {
			t.Error("陈旧 Socket 应当被清理")
		}
	}
}

// ---------------------------------------------------------------- 包结构

func TestPackageStructure(t *testing.T) {
	root := ".."
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest"))
	if err != nil {
		t.Fatalf("读 manifest 失败：%v", err)
	}
	if strings.HasPrefix(string(manifestBytes), "\ufeff") {
		t.Error("manifest 不能带 BOM")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(manifestBytes)), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if fields["appname"] != "unificenter" || fields["source"] != "thirdparty" {
		t.Errorf("manifest 基础字段不对：%v", fields)
	}
	if fields["desktop_applaunchname"] != "unificenter.main" {
		t.Errorf("desktop_applaunchname = %q", fields["desktop_applaunchname"])
	}
	if fields["maintainer"] != "fw867" {
		t.Errorf("maintainer = %q，期望 \"fw867\"", fields["maintainer"])
	}
	// Go 版是自包含的静态二进制：不能再声明 python312 之类的运行时依赖
	if fields["install_dep_apps"] != "" {
		t.Errorf("自包含包不应声明运行时依赖，却写了 %q", fields["install_dep_apps"])
	}
	if fields["platform"] != "x86" && fields["platform"] != "arm" {
		t.Errorf("含架构二进制时必须按架构声明 platform，得到 %q", fields["platform"])
	}
	if fields["version"] != appVersion {
		t.Errorf("manifest 版本 %q 与二进制版本 %q 不一致", fields["version"], appVersion)
	}

	entryBytes, err := os.ReadFile(filepath.Join(root, "app", "ui", "config"))
	if err != nil {
		t.Fatalf("读入口配置失败：%v", err)
	}
	var generic map[string]map[string]map[string]any
	if err := json.Unmarshal(entryBytes, &generic); err != nil {
		t.Fatalf("入口配置不是合法 JSON：%v", err)
	}
	entry := generic[".url"]["unificenter.main"]
	if entry["gatewayPrefix"] != "/app/unificenter" || entry["gatewaySocket"] != "app.sock" {
		t.Errorf("网关入口配置不对：%v", entry)
	}
	if entry["type"] != "iframe" || entry["url"] != "/app/unificenter/" {
		t.Errorf("入口类型/地址不对：%v", entry)
	}
	if entry["icon"] != "images/icon_{0}.png" {
		t.Errorf("入口图标不对：%v", entry["icon"])
	}

	for _, name := range []string{"main", "install_init", "install_callback", "upgrade_init",
		"upgrade_callback", "uninstall_init", "uninstall_callback", "config_init", "config_callback"} {
		path := filepath.Join(root, "cmd", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("缺少 %s：%v", path, err)
			continue
		}
		if !strings.HasPrefix(string(data), "#!/bin/bash") {
			t.Errorf("%s 缺少 shebang", name)
		}
	}
	mainScript, _ := os.ReadFile(filepath.Join(root, "cmd", "main"))
	for _, want := range []string{"start)", "stop)", "status)", "exit 3", "app.sock", "unifi-center"} {
		if !strings.Contains(string(mainScript), want) {
			t.Errorf("cmd/main 里缺少 %q", want)
		}
	}
	// 静态二进制：脚本里不能出现解释器依赖
	for _, unwanted := range []string{"python3 ", "python ", "node ", "systemctl"} {
		if strings.Contains(string(mainScript), unwanted) {
			t.Errorf("cmd/main 里不该出现 %q（服务端是自包含二进制）", unwanted)
		}
	}

	for _, name := range []string{"install", "upgrade", "uninstall", "config"} {
		data, err := os.ReadFile(filepath.Join(root, "wizard", name))
		if err != nil {
			t.Errorf("缺少 wizard/%s：%v", name, err)
			continue
		}
		var steps []map[string]any
		if err := json.Unmarshal(data, &steps); err != nil {
			t.Errorf("wizard/%s 不是合法 JSON：%v", name, err)
		}
	}

	for _, name := range []string{"ICON.PNG", "ICON_256.PNG",
		"app/ui/images/icon_64.png", "app/ui/images/icon_256.png",
		"app/ui/assets/vue.js", "app/ui/assets/lucide.js", "app/ui/assets/tailwind.js",
		"app/ui/panel.html", "app/ui/panel.js", "app/ui/panel.css"} {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || info.IsDir() {
			t.Errorf("缺少 %s", name)
		}
	}
}

func TestFrontendNeverShowsHintAsToken(t *testing.T) {	panel, err := os.ReadFile(filepath.Join("..", "app", "ui", "panel.js"))
	if err != nil {
		t.Fatalf("读面板脚本失败：%v", err)
	}
	text := string(panel)
	if !strings.Contains(text, "api('token')") {
		t.Error("面板应当通过 /ctl/token 显式取明文")
	}
	if strings.Contains(text, "$('tokenInput').value = hint") {
		t.Error("面板不能把 token_hint 当令牌填进输入框")
	}
	if !strings.Contains(text, "can_edit") {
		t.Error("面板应当按 can_edit 决定可编辑性")
	}
}

func TestBuildURL(t *testing.T) {
	cases := []struct {
		base, path, query, want string
	}{
		{"http://host:1/", "repos/x", "", "http://host:1/repos/x"},
		{"http://host:1", "repos/x", "", "http://host:1/repos/x"},
		{"http://host:1/", "/repos/x", "", "http://host:1/repos/x"},
		// 空 path 不能补尾斜杠：否则上游会 301，转发链跟着跳走
		{"http://host:1/repos/x/y/releases", "", "", "http://host:1/repos/x/y/releases"},
		{"http://host:1/", "api/system/info", "a=1&b=2", "http://host:1/api/system/info?a=1&b=2"},
	}
	for _, item := range cases {
		got, err := BuildURL(item.base, item.path, item.query)
		if err != nil {
			t.Fatalf("BuildURL(%q,%q) 报错：%v", item.base, item.path, err)
		}
		if got != item.want {
			t.Errorf("BuildURL(%q,%q,%q) = %q，期望 %q", item.base, item.path, item.query, got, item.want)
		}
	}
}

func TestEnsureTrailingSlash(t *testing.T) {
	cases := map[string]string{
		"http://host:1":     "http://host:1/",
		"http://host:1/":    "http://host:1/",
		"http://host:1/sub": "http://host:1/sub/",
	}
	for input, want := range cases {
		if got := ensureTrailingSlash(input); got != want {
			t.Errorf("ensureTrailingSlash(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestReadBodyLimited(t *testing.T) {	if _, err := ReadBodyLimited(strings.NewReader("hello"), 10); err != nil {
		t.Errorf("正常大小不应报错：%v", err)
	}
	if _, err := ReadBodyLimited(strings.NewReader(strings.Repeat("x", 100)), 10); err == nil {
		t.Error("超限应当报错")
	}
}

func TestLoggerTailAndRotation(t *testing.T) {
	dir := t.TempDir()
	logger := NewLogger(filepath.Join(dir, "test.log"))
	for index := 0; index < 5; index++ {
		logger.Printf("第 %d 行", index)
	}
	lines, err := logger.Tail(3)
	if err != nil {
		t.Fatalf("读日志失败：%v", err)
	}
	if len(lines) != 3 || !strings.Contains(lines[2], "第 4 行") {
		t.Errorf("日志尾部不对：%v", lines)
	}
	missing := NewLogger(filepath.Join(dir, "nope.log"))
	if lines, err := missing.Tail(10); err != nil || len(lines) != 0 {
		t.Errorf("不存在的日志应返回空列表：%v %v", lines, err)
	}
}
