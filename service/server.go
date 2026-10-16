// server.go：路由、Unix Socket 监听、转发与静态资源。
//
// 路由（前缀 /app/unificenter 可带可不带，网关会原样转发）：
//
//	/                    软件中心页面（取回后本地化 + 令牌注入）
//	/api/…               转发到路由器软件中心的接口（Authorization 按规则取值）
//	/github/api|raw/…    转发页面里对 GitHub 的直连（不带 Authorization）
//	/ctl/…               本应用自己的接口（状态、设置、令牌、日志、设置面板）
//	/assets/ /images/…   本地资源
//	/view/…              兼容入口（对应原插件的老路径）
//	/healthz             健康检查
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 每类请求的超时：软件中心是 REST + 长轮询，转发要放很长；GitHub 与探测短一些。
const (
	maxSettingsBody = 32 * 1024
	maxProxyBody    = 8 * 1024 * 1024
	panelScriptName = "panel.js"
)

//: 软件中心是 REST + 长轮询，转发超时放长一些；GitHub 转发单独用较短超时。
var (
	routerTimeout = floatDefault(firstEnv("ROUTER_TIMEOUT"), 3600)
	githubTimeout = floatDefault(firstEnv("GITHUB_TIMEOUT"), 30)
)

var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".svg":   "image/svg+xml",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// Service 是进程级共享状态。
type Service struct {
	Env       Env
	Store     *Store
	Logger    *Logger
	Started   time.Time
	ProbeHTTP *http.Client
	ProxyHTTP *http.Client
	GitHub    *http.Client
}

func NewService(env Env, store *Store, logger *Logger) *Service {
	return &Service{
		Env:       env,
		Store:     store,
		Logger:    logger,
		Started:   time.Now(),
		ProbeHTTP: NewHTTPClient(time.Duration(env.ProbeSec*float64(time.Second)), false),
		ProxyHTTP: NewHTTPClient(time.Duration(env.RouterSec*float64(time.Second)), true),
		GitHub:    NewHTTPClient(time.Duration(env.GitHubSec*float64(time.Second)), true),
	}
}

// prefix 是对外路径前缀，例如 /app/unificenter。
func (s *Service) prefix() string { return "/" + strings.Trim(s.Env.GatewayPfx, "/") }

// ---------------------------------------------------------------- 页面与状态

// StatusPayload 是 /ctl/status 的返回。
func (s *Service) StatusPayload(user GatewayUser) map[string]any {
	target := s.Store.LoadSettings(s.Env.DefaultTarg)
	token := s.Store.Token()
	return map[string]any{
		"app":            s.Env.AppName,
		"version":        appVersion,
		"target":         target,
		"default_target": s.Env.DefaultTarg,
		"token_set":      token != "",
		"token_hint":     TokenHint(token),
		"router":         ProbeRouter(s.ProbeHTTP, target, token),
		"gateway_prefix": s.prefix(),
		"socket_path":    s.Env.SocketPath,
		// OwnedSocket：自己 bind 成功才算就绪（不能看文件在不在，残留文件会骗人）
		"socket_ready":   s.Env.OwnedSocket,
		"socket_present": socketFilePresent(s.Env.SocketPath),
		"app_dest":       s.Env.AppDest,
		"app_root":       s.Env.AppRoot,
		"ui_dir":         s.Env.UIDir,
		"assets":         AssetsPresent(s.Env.UIDir),
		"config_dir":     s.Env.ConfigDir,
		"user":           user,
		"port":           s.Env.DevPort,
		"at":             time.Now().Unix(),
	}
}

// SettingsPayload 是 /ctl/settings 的返回：明文令牌只给已登录管理员。
func (s *Service) SettingsPayload(user GatewayUser) map[string]any {
	token := s.Store.Token()
	trusted := CanRevealToken(user)
	visible := ""
	if trusted {
		visible = token
	}
	return map[string]any{
		"ok":             true,
		"target":         s.Store.LoadSettings(s.Env.DefaultTarg),
		"default_target": s.Env.DefaultTarg,
		"token_set":      token != "",
		"token":          visible,
		"token_hint":     TokenHint(token),
		"token_visible":  trusted,
		"can_edit":       CanChangeSettings(user),
		"user":           user,
		"gateway_prefix": s.prefix(),
		"assets":         AssetsPresent(s.Env.UIDir),
		"socket_path":    s.Env.SocketPath,
	}
}

func (s *Service) fetchRouterPage() (string, error) {
	target := ensureTrailingSlash(s.Store.LoadSettings(s.Env.DefaultTarg))
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	client := NewHTTPClient(time.Duration(s.Env.ProbeSec*2*float64(time.Second)), true)
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	_ = response.Body.Close()
	if readErr != nil {
		return "", readErr
	}
	return string(body), nil
}

// ---------------------------------------------------------------- HTTP 入口

// Handler 返回服务的 HTTP 处理器（Socket 与调试端口共用）。
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(s.route)
}

func (s *Service) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求路径不合法"})
		return
	}

	prefix := s.prefix()
	if path == prefix {
		http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
		return
	}
	if strings.HasPrefix(path, prefix+"/") {
		path = strings.TrimPrefix(path, prefix)
	}

	switch {
	case path == "/healthz":
		s.handleHealthz(w)
	case path == "/ctl" || strings.HasPrefix(path, "/ctl/"):
		s.handleControl(w, r, strings.TrimPrefix(path, "/ctl"))
	case strings.HasPrefix(path, "/view"):
		s.handleView(w, r, strings.TrimPrefix(path, "/view"))
	case path == "/" || path == "/index.html" || path == "/control" || path == "/control.html":
		s.handleSoftCenter(w, r)
	case strings.HasPrefix(path, "/github/"):
		// GitHubUpstream 认的是 `github/api/…` 这种形状，把前缀补回去
		s.handleGitHub(w, r, "github/"+strings.TrimPrefix(path, "/github/"))
	case strings.HasPrefix(path, "/api/github/"):
		s.handleGitHub(w, r, strings.TrimPrefix(path, "/api/"))
	case strings.HasPrefix(path, "/api/"):
		s.handleProxy(w, r, strings.TrimPrefix(path, "/"))
	default:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "方法不被支持"})
			return
		}
		s.serveStatic(w, r, path)
	}
}

func (s *Service) handleHealthz(w http.ResponseWriter) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"app":     s.Env.AppName,
		"version": appVersion,
		"target":  s.Store.LoadSettings(s.Env.DefaultTarg),
		"uptime":  int(time.Since(s.Started).Seconds()),
		"socket":  s.Env.SocketPath,
		"assets":  AssetsPresent(s.Env.UIDir),
	})
}

// handleSoftCenter 返回本地化后的软件中心页面（不套 iframe）。
// 已保存的 AdminToken 无条件注入 localStorage.sc_token：注入不挑入口，
// 这样哪个用户/入口打开都不会再弹软件中心那层"配置门"。
func (s *Service) handleSoftCenter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "方法不被支持"})
		return
	}
	html, err := s.fetchRouterPage()
	if err != nil {
		s.Logger.Printf("取软件中心页面失败：%v", err)
		s.writeHTML(w, http.StatusBadGateway, s.offlinePage(err.Error()))
		return
	}
	base := s.prefix()
	rewritten, unknown := PreparePage(html, base, "", s.Store.Token(), GitHubRoutes())
	scripts := fmt.Sprintf(`<script src="%s/assets/close.js" data-mode="close" defer></script>`+
		`<script src="%s/assets/overlay.js" defer></script>`, base, base)
	if strings.Contains(rewritten, "</body>") {
		rewritten = strings.Replace(rewritten, "</body>", scripts+"</body>", 1)
	} else {
		rewritten += scripts
	}
	if len(unknown) > 0 {
		s.Logger.Printf("页面里仍有外部脚本未本地化：%v", unknown)
	}
	missing := []string{}
	for name, present := range AssetsPresent(s.Env.UIDir) {
		if !present {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		s.Logger.Printf("本地资源缺失 %v，页面可能显示不全", missing)
	}
	s.writeHTML(w, http.StatusOK, rewritten)
}

func (s *Service) offlinePage(detail string) string {
	base := s.prefix()
	target := s.Store.LoadSettings(s.Env.DefaultTarg)
	name := displayName()
	// 离线页的短标题用应用名去掉"系统"后缀（"unifi 软件中心" → "unifi 软件"），
	// 和"连不上…"这句话连起来更顺；它是从 manifest 读的，改了应用名这里自动跟着变
	short := strings.TrimSuffix(strings.TrimSuffix(name, "系统"), "系统")
	return fmt.Sprintf(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<link rel="stylesheet" href="%s/panel.css">
</head><body class="rc-body">
<main class="rc-panel">
  <section class="rc-card">
    <h2>连不上 %s</h2>
    <p class="rc-muted">目标地址：<code>%s</code></p>
    <p class="rc-error">%s</p>
    <p class="rc-muted">请确认路由器已开机、软件中心正在运行，并检查下面的设置。</p>
    <div class="rc-card-actions">
      <a class="rc-btn is-primary" href="%s/ctl/panel.html">打开设置</a>
      <a class="rc-btn" href="%s/">重新加载</a>
    </div>
  </section>
</main>
<script src="%s/assets/close.js" data-mode="close" defer></script>
</body></html>`, name, base, short, htmlEscape(target), htmlEscape(detail), base, base, base)
}

// ---------------------------------------------------------------- /ctl/…

func (s *Service) handleControl(w http.ResponseWriter, r *http.Request, rest string) {
	route := rest
	if index := strings.IndexByte(route, '?'); index >= 0 {
		route = route[:index]
	}
	user := GatewayUserFrom(r.Header)
	readOnly := r.Method != http.MethodGet && r.Method != http.MethodHead

	switch route {
	case "/status", "/status/":
		if readOnly {
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "方法不被支持"})
			return
		}
		s.writeJSON(w, http.StatusOK, s.StatusPayload(user))
		return

	case "/settings", "/settings/":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.writeJSON(w, http.StatusOK, s.SettingsPayload(user))
		case http.MethodPost:
			s.postSettings(w, r, user)
		default:
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "方法不被支持"})
		}
		return

	case "/token", "/token/":
		if readOnly {
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "方法不被支持"})
			return
		}
		if !CanRevealToken(user) {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"ok": false,
				"error": "只有管理员可以查看 AdminToken 明文"})
			return
		}
		token := s.Store.Token()
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "token_set": token != ""})
		return

	case "/logs", "/logs/":
		if readOnly {
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "方法不被支持"})
			return
		}
		lines, err := s.Logger.Tail(200)
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lines": lines, "file": s.Logger.file})
		return

	case "/probe", "/probe/":
		if readOnly {
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "方法不被支持"})
			return
		}
		target := s.Store.LoadSettings(s.Env.DefaultTarg)
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "router": ProbeRouter(s.ProbeHTTP, target, s.Store.Token())})
		return

	case "", "/":
		http.Redirect(w, r, s.prefix()+"/ctl/panel.html", http.StatusMovedPermanently)
		return
	}

	if readOnly {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "方法不被支持"})
		return
	}
	// 设置面板与它自己的静态资源
	s.serveUIFile(w, r, strings.TrimPrefix(rest, "/"))
}

func (s *Service) postSettings(w http.ResponseWriter, r *http.Request, user GatewayUser) {
	if !CanChangeSettings(user) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "只有管理员可以修改连接设置"})
		return
	}
	body, err := ReadBodyLimited(r.Body, maxSettingsBody)
	if err != nil {
		s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	payload := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
			return
		}
	}
	if rawToken, present := payload["token"]; present {
		text, _ := rawToken.(string)
		if err := s.Store.SaveToken(text); err != nil {
			s.Logger.Printf("保存令牌失败：%v", err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false,
				"error": "写入失败：" + err.Error()})
			return
		}
		if strings.TrimSpace(text) == "" {
			s.Logger.Printf("已清除保存的 AdminToken（%s）", user.Label)
		} else {
			s.Logger.Printf("已保存 AdminToken（%s，文件权限 0600，%s）", TokenHint(text), user.Label)
		}
	}
	target := s.Store.LoadSettings(s.Env.DefaultTarg)
	if rawTarget, present := payload["target"]; present {
		text, _ := rawTarget.(string)
		target = text
	}
	saved, err := s.Store.SaveTarget(target, s.Env.DefaultTarg)
	if err != nil {
		s.writeJSON(w, http.StatusConflict, mergeMaps(map[string]any{"ok": false, "error": err.Error()},
			s.StatusPayload(user)))
		return
	}
	s.Logger.Printf("设置已更新：目标 %s（%s）", saved, user.Label)
	s.writeJSON(w, http.StatusOK, s.StatusPayload(user))
}

// ---------------------------------------------------------------- 转发

// handleProxy 把 /api/… 转发到路由器软件中心。
//
// 转发用的 Authorization 不是无脑透传：控制台那一跳会把客户端凭据覆盖成占位值，
// 那个值送到路由器必然是"令牌不正确"。取值规则见 ResolveAuthorization。
func (s *Service) handleProxy(w http.ResponseWriter, r *http.Request, suffix string) {
	target := ensureTrailingSlash(s.Store.LoadSettings(s.Env.DefaultTarg))
	url, err := BuildURL(target, suffix, r.URL.RawQuery)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "转发地址不合法"})
		return
	}
	var body io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		data, readErr := ReadBodyLimited(r.Body, maxProxyBody)
		if readErr != nil {
			s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": readErr.Error()})
			return
		}
		body = strings.NewReader(string(data))
	}
	request, err := http.NewRequest(r.Method, url, body)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	copyHeader(request.Header, r.Header, "Accept", "Content-Type", "Accept-Language", "User-Agent")
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", "FnOsUnifiCenter/"+appVersion)
	}
	if authorization := ResolveAuthorization(r.Header.Get("Authorization"), s.Store.Token(),
		s.Logger.Printf); authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	s.forward(w, request, s.ProxyHTTP, suffix)
}

// handleGitHub 转发页面里对 GitHub 的直连。
// 注意：绝不能把客户端的 Authorization 带过去，否则 GitHub 会判成无效凭据返回 401。
func (s *Service) handleGitHub(w http.ResponseWriter, r *http.Request, rest string) {
	upstream := GitHubUpstream(rest, GitHubRoutes())
	if upstream == "" {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "不是 GitHub 转发路径"})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GitHub 转发只支持 GET"})
		return
	}
	url, err := BuildURL(upstream, "", r.URL.RawQuery)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	request, err := http.NewRequest(r.Method, url, nil)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		accept = "application/json"
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", "FnOsUnifiCenter/"+appVersion)
	s.forward(w, request, s.GitHub, "github "+rest)
}

// forward 执行一次上游请求并把结果原样写回（含 4xx/5xx 状态与正文）。
func (s *Service) forward(w http.ResponseWriter, request *http.Request, client *http.Client, label string) {
	response, err := client.Do(request)
	if err != nil {
		s.Logger.Printf("转发失败 %s: %v", label, err)
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false,
			"error": "无法访问目标：" + err.Error()})
		return
	}
	defer response.Body.Close()
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(w, response.Body)
	}
}

// handleView 是兼容入口（对应原插件在小米客户端里的 /view/…）。
func (s *Service) handleView(w http.ResponseWriter, r *http.Request, rest string) {
	suffix := strings.TrimLeft(rest, "/")
	switch {
	case strings.HasPrefix(suffix, "github/"):
		s.handleGitHub(w, r, suffix)
	case suffix == "" || suffix == "index.html":
		s.handleSoftCenter(w, r)
	case suffix == "panel.html" || suffix == panelScriptName || suffix == "panel.css":
		s.serveUIFile(w, r, suffix)
	default:
		s.handleProxy(w, r, suffix)
	}
}

// ---------------------------------------------------------------- 静态资源

var allowedUIFiles = map[string]bool{
	"panel.html": true, "panel.js": true, "panel.css": true, "index.html": true,
}

// directUIFiles 是"在根路径下也能直接按文件名取"的 UI 文件。
// 离线兜底页就是在根路径渲染的，它引用的 ref 是 {prefix}/panel.css 与
// {prefix}/assets/close.js —— 少了这条放行，兜底页会变成没样式、没关闭按钮的裸 HTML（踩过）。
var directUIFiles = map[string]string{
	"panel.html": "panel.html",
	"panel.js":   "panel.js",
	"panel.css":  "panel.css",
	"close.js":   filepath.Join("assets", "close.js"),
	"overlay.js": filepath.Join("assets", "overlay.js"),
}

func (s *Service) serveUIFile(w http.ResponseWriter, r *http.Request, name string) {
	if !allowedUIFiles[name] {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "文件不存在"})
		return
	}
	s.sendFile(w, filepath.Join(s.Env.UIDir, name))
}

func (s *Service) serveStatic(w http.ResponseWriter, r *http.Request, path string) {
	relative := filepath.Clean(strings.TrimPrefix(path, "/"))
	if relative == "." || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "拒绝访问"})
		return
	}
	allowed := strings.HasPrefix(relative, "assets"+string(filepath.Separator)) ||
		strings.HasPrefix(relative, "images"+string(filepath.Separator)) ||
		allowedUIFiles[relative] || relative == "favicon.ico" || relative == "icon.png"
	if !allowed {
		// 根路径下也放行几个固定的 UI 文件（离线兜底页要用）
		if mapped, ok := directUIFiles[relative]; ok {
			s.sendFile(w, filepath.Join(s.Env.UIDir, mapped))
			return
		}
		s.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "文件不存在"})
		return
	}
	root, err := filepath.Abs(s.Env.UIDir)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "读取失败"})
		return
	}
	candidate, err := filepath.Abs(filepath.Join(root, relative))
	if err != nil || !strings.HasPrefix(candidate, root+string(filepath.Separator)) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "拒绝访问"})
		return
	}
	// 符号链接也要挡在 UI 目录内（避免用软链读出目录外的文件）
	if resolved, evalErr := filepath.EvalSymlinks(candidate); evalErr == nil {
		if !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "拒绝访问"})
			return
		}
	}
	if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
		candidate = filepath.Join(candidate, "index.html")
	}
	s.sendFile(w, candidate)
}

func (s *Service) sendFile(w http.ResponseWriter, candidate string) {
	data, err := os.ReadFile(candidate)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "文件不存在"})
		return
	}
	contentType := contentTypes[strings.ToLower(filepath.Ext(candidate))]
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------- 响应工具

func (s *Service) writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte(`{"ok":false,"error":"序列化失败"}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (s *Service) writeHTML(w http.ResponseWriter, status int, body string) {
	data := []byte(body)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func copyHeader(dst, src http.Header, names ...string) {
	for _, name := range names {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

func mergeMaps(base, extra map[string]any) map[string]any {
	for key, value := range extra {
		base[key] = value
	}
	return base
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(value)
}

// ---------------------------------------------------------------- 监听与启动

// UnlinkStaleSocket 清理崩掉留下的 Socket 文件（不清理的话 bind 会 EADDRINUSE）。
//
// 判据用"能不能重新 bind"，**不用 dial**：Unix Socket 的 backlog 里可能还留着没被
// accept 掉的残留连接，监听者早已关闭时 dial 仍会成功、连 write 都可能成功，于是把
// 陈旧 Socket 误判成"服务还活着"而不清理（Linux 上的 CI 抓到了这个竞态）。
// bind 探测是确定性的：
//   - 文件不存在 → 无需清理；
//   - 能 bind 成功  → 确实没人在听（内核已释放），删掉文件；
//   - bind 失败     → 有活着的进程占着（或在重启中），绝不删。
//
// 进程被 kill -9 时内核会释放绑定、只留下文件，所以这个判据对"崩溃残留"也成立。
func UnlinkStaleSocket(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	for attempt := 0; attempt < 10; attempt++ {
		probe, err := net.Listen("unix", path)
		if err == nil {
			_ = probe.Close()
			return os.Remove(path) == nil
		}
		// 可能是内核刚释放绑定，稍等一下再试
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// BuildListeners 按约定返回要监听的入口：
// 目标机上监听 $TRIM_APPDEST/app.sock；本机调试时可另开 TCP 端口。
func (s *Service) BuildListeners() ([]net.Listener, error) {
	listeners := []net.Listener{}
	if s.Env.SocketOnly || s.Env.DevPort <= 0 {
		if err := os.MkdirAll(filepath.Dir(s.Env.SocketPath), 0o755); err != nil {
			return nil, err
		}
		UnlinkStaleSocket(s.Env.SocketPath)
		listener, err := net.Listen("unix", s.Env.SocketPath)
		if err != nil {
			return nil, fmt.Errorf("监听 %s 失败：%w", s.Env.SocketPath, err)
		}
		// 网关以系统身份连接，Socket 需要可写
		_ = os.Chmod(s.Env.SocketPath, 0o666)
		s.Env.OwnedSocket = true
		s.Logger.Printf("已监听 Unix Socket %s（fnOS 统一网关目标）", s.Env.SocketPath)
		listeners = append(listeners, listener)
	}
	if s.Env.DevPort > 0 {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.Env.DevPort))
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("监听调试端口失败：%w", err)
		}
		s.Logger.Printf("已监听 http://127.0.0.1:%d（本机调试端口）", s.Env.DevPort)
		listeners = append(listeners, listener)
	}
	if len(listeners) == 0 {
		return nil, errors.New("没有可监听的入口：请设置 TRIM_APPDEST 或 UNIFICENTER_DEV_PORT")
	}
	return listeners, nil
}

// Serve 阻塞运行，直到 ctx 取消。
func (s *Service) Serve(ctx context.Context, listeners []net.Listener) error {
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func(l net.Listener) {
			if err := server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(listener)
	}
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		_ = server.Close()
		return err
	}
}
