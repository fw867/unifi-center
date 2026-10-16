// Package main 是「路由器软件中心」的飞牛 fnOS 服务端：把局域网里路由器上的
// 软件中心（UniFi SoftCenter / KoolCenter 类插件中心）反代到 fnOS 的桌面入口。
//
// 本文件（engine.go）负责不碰网络协议的那一半：配置与令牌的读写、目标地址规整、
// 转发时的 Authorization 决策、页面重写、网关身份判定、路由器只读探测。
// 它由 server.go 调用，也是单元测试的主要对象。
//
// 设计约束（沿用原插件与官方规范）：
//   - 编译成静态二进制，目标机不需要任何解释器/运行时/依赖应用（CGO 关闭）；
//   - 路径全部来自 TRIM_* 环境变量，不硬编码卷与安装目录；
//   - 不信任客户端输入：身份只认 fnOS 网关注入的 X-Trim-* 头。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	appNameDefault = "unificenter"
	appVersion     = "1.0.0"
	binaryName     = "unifi-center"
	socketName     = "app.sock"

	settingsFileName = "settings.json"
	tokenFileName    = "router-token"
	logFileName      = "unifi-center.log"
)

// 控制台那一跳塞进来的占位 Authorization（与 xiaomi-nas-console 的 nginx 配置一致）。
const (
	consoleLoopbackToken = "console-loopback"
	localEntryLabel      = "本机调试"
)

var (
	// 目标地址：http(s)://主机[:端口]/ —— 与 Python 版、原插件保持同一套规则
	targetPattern  = regexp.MustCompile(`(?i)^(https?)://([^/]+)(/.*)?$`)
	authorityOK    = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:\d{1,5})?$`)
	titlePattern   = regexp.MustCompile(`(?is)<title>(.*?)</title>`)
	scriptSrcRegex = regexp.MustCompile(`(?is)(<script[^>]*\bsrc=")(https?://[^"]+)(")`)

	// 页面里依赖的外部 CDN → 本应用自带的本地副本
	assetRoutes = []struct{ marker, local string }{
		{"cdn.tailwindcss.com", "assets/tailwind.js"},
		{"cdn.jsdelivr.net/npm/vue@", "assets/vue.js"},
		{"cdn.jsdelivr.net/npm/lucide@", "assets/lucide.js"},
	}

	localAssets = []string{"assets/vue.js", "assets/lucide.js", "assets/tailwind.js"}
)

// ---------------------------------------------------------------------------
// 运行环境：全部路径与开关都从环境变量/参数来
// ---------------------------------------------------------------------------

// Env 是一次运行的全部外部输入（便于测试注入）。
type Env struct {
	AppName     string
	ConfigDir   string // TRIM_PKGETC（其次 TRIM_PKGVAR）
	VarDir      string // TRIM_PKGVAR
	UIDir       string // <应用目录>/ui（其次包内 app/ui）
	AppDest     string // TRIM_APPDEST 原值（网关按它找 Socket，位置不能动）
	AppRoot     string // 解析出来的"应用文件目录"（可能等于 AppDest，也可能是 AppDest/target）
	SocketPath  string // <TRIM_APPDEST>/app.sock
	SocketOnly  bool   // 目标机模式：只监听 Unix Socket（TRIM_APPDEST 存在）
	DevPort     int    // 本机调试用的 TCP 端口（0 表示不监听）
	DefaultTarg string // ROUTER_TARGET
	GatewayPfx  string // GATEWAY_PREFIX，默认 /app/<appname>
	ProbeSec    float64
	GitHubSec   float64
	RouterSec   float64
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// AppDestCandidates 返回"应用文件所在目录"的候选，按优先级排列。
//
// 为什么要有候选：官方文档写 TRIM_APPDEST 是"已安装的 target 目录"，但实测有设备把它
// 指到应用根目录（即 target 的上一级），于是 APP/bin/unifi-center 与 APP/target/bin/unifi-center
// 都可能才是正确路径。这里不猜，逐个看哪个目录里真有 server 端要用的东西
// （bin/unifi-center、ui/config、app.sock），并且把过程写进日志，方便对着真实布局排障。
func AppDestCandidates() []string {
	seen := map[string]bool{}
	candidates := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		candidates = append(candidates, value)
	}

	appDest := strings.TrimSpace(os.Getenv("TRIM_APPDEST"))
	add(appDest)
	if appDest != "" {
		add(filepath.Join(appDest, "target"))
	}
	appName := firstEnv("TRIM_APPNAME")
	if appName == "" {
		appName = appNameDefault
	}
	add(filepath.Join("/var/apps", appName, "target"))
	add(filepath.Join("/var/apps", appName))
	return candidates
}

// ResolveAppDest 选出一个候选作为应用文件目录：优先挑真正有服务文件的，
// 都没有就退回第一个候选（此时报错信息里会列出所有试过的路径）。
func ResolveAppDest() string {
	candidates := AppDestCandidates()
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, "bin", binaryName)); err == nil && !info.IsDir() {
			return candidate
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, "ui", "config")); err == nil && !info.IsDir() {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

// LoadEnv 按 fnOS 规范解析环境变量。
func LoadEnv() Env {
	appName := firstEnv("TRIM_APPNAME")
	if appName == "" {
		appName = appNameDefault
	}
	configDir := firstEnv("TRIM_PKGETC", "TRIM_PKGVAR")
	if configDir == "" {
		configDir = ".dev/config" // 开发机兜底
	}
	appDest := firstEnv("TRIM_APPDEST")
	appRoot := ResolveAppDest()
	if appRoot == "" {
		appRoot = appDest
	}
	uiDir := firstEnv("UI_DIR")
	if uiDir == "" && appRoot != "" {
		uiDir = filepath.Join(appRoot, "ui")
	}
	if uiDir == "" {
		uiDir = "app/ui"
	}
	socket := firstEnv("SOCKET_PATH")
	if socket == "" && appDest != "" {
		// 网关按 TRIM_APPDEST 找 Socket，所以这里必须跟着 TRIM_APPDEST 走，不做候选回退：
		// 放到别处网关就找不到（会和入口配置的 gatewaySocket 对不上）。
		socket = filepath.Join(appDest, socketName)
	}
	if socket == "" {
		socket = filepath.Join(".dev", socketName)
	}
	defaultTarget := firstEnv("ROUTER_TARGET")
	if defaultTarget == "" {
		defaultTarget = "http://192.168.1.1:9958/"
	}
	prefix := firstEnv("GATEWAY_PREFIX")
	if prefix == "" {
		prefix = "/app/" + appName
	}
	return Env{
		AppName:     appName,
		ConfigDir:   configDir,
		VarDir:      firstEnv("TRIM_PKGVAR"),
		UIDir:       uiDir,
		AppDest:     appDest,
		AppRoot:     appRoot,
		SocketPath:  socket,
		SocketOnly:  appDest != "",
		DevPort:     atoiDefault(firstEnv("UNIFICENTER_DEV_PORT"), 0),
		DefaultTarg: defaultTarget,
		GatewayPfx:  "/" + strings.Trim(prefix, "/"),
		ProbeSec:    floatDefault(firstEnv("PROBE_TIMEOUT"), 6),
		GitHubSec:   floatDefault(firstEnv("GITHUB_TIMEOUT"), 30),
		RouterSec:   floatDefault(firstEnv("ROUTER_TIMEOUT"), 3600),
	}
}

// ---------------------------------------------------------------------------
// 日志：stdout 一份（appcenter-cli 可见），应用配置目录一份（带大小上限）
// ---------------------------------------------------------------------------

// Logger 是带轮转的简单日志器。
type Logger struct {
	mu   sync.Mutex
	file string
	max  int64
	out  *log.Logger
}

func NewLogger(file string) *Logger {
	return &Logger{file: file, max: 512 * 1024, out: log.New(os.Stdout, "", 0)}
}

func (l *Logger) Printf(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	l.out.Println(line)
	if l.file == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if info, err := os.Stat(l.file); err == nil && info.Size() > l.max {
		_ = os.Rename(l.file, l.file+".1")
	}
	if err := os.MkdirAll(filepath.Dir(l.file), 0o700); err != nil {
		return
	}
	handle, err := os.OpenFile(l.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer handle.Close()
	_, _ = handle.WriteString(line + "\n")
}

// Tail 返回日志尾部若干行（设置面板的"最近日志"）。
func (l *Logger) Tail(limit int) ([]string, error) {
	handle, err := os.Open(l.file)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, l.max+1))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}, nil
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
}

// ---------------------------------------------------------------------------
// 配置与令牌
// ---------------------------------------------------------------------------

// Store 管理 settings.json 与 router-token。
type Store struct {
	ConfigDir string
	mu        sync.Mutex
}

func NewStore(configDir string) *Store { return &Store{ConfigDir: configDir} }

func (s *Store) SettingsPath() string { return filepath.Join(s.ConfigDir, settingsFileName) }
func (s *Store) TokenPath() string    { return filepath.Join(s.ConfigDir, tokenFileName) }

// AtomicWrite 写文件：先写临时文件再改名，权限按 move 前的 chmod 设定。
// 这样任何一次写入中断都不会留下半个文件（配置与令牌都必须如此）。
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// NormalizeTarget 把用户填的地址规整成 http(s)://主机[:端口]/ 。
func NormalizeTarget(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("目标地址不能为空")
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	match := targetPattern.FindStringSubmatch(value)
	if match == nil {
		return "", errors.New("目标地址格式不对，示例：http://192.168.1.1:9958/")
	}
	scheme := strings.ToLower(match[1])
	authority := match[2]
	if !authorityOK.MatchString(authority) {
		return "", errors.New("目标地址的主机名/端口不合法")
	}
	if strings.HasSuffix(authority, ":") {
		return "", errors.New("目标地址的端口不合法")
	}
	return scheme + "://" + authority + "/", nil
}

// LoadSettings 读设置；文件缺失/损坏/地址非法都回退到默认目标。
func (s *Store) LoadSettings(defaultTarget string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(defaultTarget)
}

func (s *Store) loadLocked(defaultTarget string) string {
	raw := ""
	if data, err := os.ReadFile(s.SettingsPath()); err == nil {
		var payload struct {
			Target string `json:"target"`
		}
		if json.Unmarshal(data, &payload) == nil {
			raw = strings.TrimSpace(payload.Target)
		}
	}
	if raw == "" {
		raw = defaultTarget
	}
	target, err := NormalizeTarget(raw)
	if err != nil {
		fallback, fallbackErr := NormalizeTarget(defaultTarget)
		if fallbackErr != nil {
			return defaultTarget
		}
		return fallback
	}
	return target
}

// SaveTarget 保存目标地址（路由器接口由本服务转发，改地址不涉及任何系统改动）。
func (s *Store) SaveTarget(target, defaultTarget string) (string, error) {
	normalized, err := NormalizeTarget(target)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, _ := json.MarshalIndent(map[string]string{"target": normalized}, "", "  ")
	if err := AtomicWrite(s.SettingsPath(), append(payload, '\n'), 0o644); err != nil {
		return "", err
	}
	return normalized, nil
}

// Token 读保存的 AdminToken；不存在返回空串。
func (s *Store) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.TokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// SaveToken 保存/清除 AdminToken（明文只落在这里，权限 0600）。
func (s *Store) SaveToken(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = strings.TrimSpace(token)
	if token == "" {
		if err := os.Remove(s.TokenPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return AtomicWrite(s.TokenPath(), []byte(token+"\n"), 0o600)
}

// TokenHint 返回展示用提示（绝不等于令牌本身）。
func TokenHint(token string) string {
	if token == "" {
		return ""
	}
	runes := []rune(token)
	if len(runes) >= 6 {
		return string(runes[:4]) + "…" + string(runes[len(runes)-2:])
	}
	return "已设置"
}

// ---------------------------------------------------------------------------
// 转发时的 Authorization 决策（原插件踩过的坑，逐字保留这套规则）
//
// 背景：「控制台」电脑端那一跳会无条件写 `Authorization: Bearer console-loopback`，
// 把客户端真实凭据覆盖成占位值；那个值送到路由器必然是"令牌不正确"。
// 所以转发前要认得出占位值，换成本应用保存的令牌；认不出来（客户端自己带的真令牌）原样透传。
// ---------------------------------------------------------------------------

// IsConsolePlaceholder 判断 Authorization 是不是控制台那一跳塞的占位值。
// 容错：方案大小写、Bearer 前后多写的空格、只剩裸值（没有方案）都认。
func IsConsolePlaceholder(value string) bool {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return false
	}
	scheme, credentials, hasSep := strings.Cut(raw, " ")
	if hasSep {
		if !strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
			return false
		}
	} else {
		credentials = raw
	}
	return strings.TrimSpace(credentials) == consoleLoopbackToken
}

// ResolveAuthorization 返回真正要转发给路由器的 Authorization；空串表示这一跳不带。
//
//   - 客户端自己带了真凭据 → 原样返回；
//   - 没带凭据 / 带的是占位值 → 用本应用保存的令牌顶上；
//   - 该顶上却没保存令牌 → 返回空串（宁可不带，也别把占位值当令牌送去）。
func ResolveAuthorization(clientValue, savedToken string, logf func(string, ...any)) string {
	value := strings.TrimSpace(clientValue)
	if value != "" && !IsConsolePlaceholder(value) {
		return value
	}
	if savedToken != "" {
		if value != "" {
			logf("收到控制台占位 Authorization，改用本应用保存的路由器令牌转发")
		} else {
			logf("请求里没有 Authorization，改用本应用保存的路由器令牌转发")
		}
		return savedToken
	}
	if value != "" {
		logf("收到控制台占位 Authorization（Bearer %s），但还没保存路由器令牌："+
			"本次转发不带 Authorization，路由器会回 401；请在设置面板填入 AdminToken 后保存",
			consoleLoopbackToken)
	} else {
		logf("请求里没有 Authorization，也没保存路由器令牌：本次转发不带 Authorization，" +
			"路由器会回 401；请在设置面板填入 AdminToken 后保存")
	}
	return ""
}

// ---------------------------------------------------------------------------
// 网关身份：只认 fnOS 网关注入的 X-Trim-* 头
// ---------------------------------------------------------------------------

// GatewayUser 是网关给出的可信用户上下文。
type GatewayUser struct {
	UID        string `json:"uid"`
	Username   string `json:"username"`
	IsAdmin    bool   `json:"is_admin"`
	ViaGateway bool   `json:"via_gateway"`
	Label      string `json:"label"`
}

// GatewayUserFrom 从请求头构造身份。独立端口（调试）模式下三个头都没有，
// 一律当成"未登录 + 非管理员"，因此调试口拿不到明文令牌、也改不了设置。
func GatewayUserFrom(headers http.Header) GatewayUser {
	uid := strings.TrimSpace(headers.Get("X-Trim-Userid"))
	username := strings.TrimSpace(headers.Get("X-Trim-Username"))
	isAdmin := strings.EqualFold(strings.TrimSpace(headers.Get("X-Trim-Isadmin")), "true")
	viaGateway := uid != "" || username != ""
	label := localEntryLabel
	if viaGateway {
		switch {
		case username != "":
			label = username
		case uid != "":
			label = "uid " + uid
		default:
			label = "未知用户"
		}
	}
	// 客户端自己塞的 X-Trim-Isadmin 在经网关时会被覆写；调试模式下 ViaGateway=false，
	// 因此下面的权限判定会直接失败关闭。
	if !viaGateway {
		isAdmin = false
	}
	return GatewayUser{UID: uid, Username: username, IsAdmin: isAdmin, ViaGateway: viaGateway, Label: label}
}

// CanRevealToken 决定能否拿到明文 AdminToken：只有经网关登录的管理员。
func CanRevealToken(user GatewayUser) bool { return user.ViaGateway && user.IsAdmin }

// CanChangeSettings 决定能否改地址/换令牌：同样只允许管理员。
func CanChangeSettings(user GatewayUser) bool { return CanRevealToken(user) }

// ---------------------------------------------------------------------------
// 路由器只读探测
// ---------------------------------------------------------------------------

// RouterProbe 是一次探测的结果（全部来自只读 GET）。
type RouterProbe struct {
	Target        string  `json:"target"`
	Reachable     bool    `json:"reachable"`
	Status        int     `json:"status"`
	LatencyMS     float64 `json:"latency_ms"`
	Title         string  `json:"title"`
	Server        string  `json:"server"`
	Authenticated bool    `json:"authenticated"`
	Device        string  `json:"device"`
	Version       string  `json:"version"`
	Error         string  `json:"error"`
	At            int64   `json:"at"`
}

// ProbeRouter 先取首页（不需要令牌），再带令牌取一次 /api/system/info。
// 只读，不会改动路由器上任何东西。target 若不带尾斜杠会先被规整。
func ProbeRouter(client *http.Client, target, token string) RouterProbe {
	base := ensureTrailingSlash(target)
	result := RouterProbe{Target: base, LatencyMS: 0, At: time.Now().Unix()}
	started := time.Now()
	body := ""
	status, server, chunk, err := getFirstBytes(client, base, 4096)
	switch {
	case err != nil:
		result.Error = err.Error()
	default:
		result.Status = status
		result.Reachable = status == http.StatusOK
		result.Server = server
		body = string(chunk)
	}
	result.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
	if match := titlePattern.FindStringSubmatch(body); match != nil {
		result.Title = truncateRunes(strings.TrimSpace(match[1]), 80)
	}

	if result.Reachable && token != "" {
		request, reqErr := http.NewRequest(http.MethodGet, base+"api/system/info", nil)
		if reqErr == nil {
			request.Header.Set("Authorization", token)
			response, infoErr := client.Do(request)
			if infoErr != nil {
				if result.Error == "" {
					result.Error = infoErr.Error()
				}
			} else {
				// 必须读完并关闭：否则 keep-alive 连接会一直占着，后面的请求用不了它
				payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					result.Authenticated = true
					var parsed map[string]any
					if json.Unmarshal(payload, &parsed) == nil {
						result.Device = stringOf(parsed["Device"], parsed["device"])
						result.Version = stringOf(parsed["Version"], parsed["version"])
					}
				} else if response.StatusCode == http.StatusUnauthorized {
					result.Error = "令牌无效"
				}
			}
		}
	}
	return result
}

// ensureTrailingSlash 保证基址以 / 结尾：上游地址是按"目录"拼接的，
// 少了这个斜杠就会拼出 http://host:8410api/... 这种非法地址（踩过）。
func ensureTrailingSlash(base string) string {
	if strings.HasSuffix(base, "/") {
		return base
	}
	return base + "/"
}

// getFirstBytes 发一次 GET，只读前 limit 个字节就关闭响应体（探测首页用）。
func getFirstBytes(client *http.Client, url string, limit int64) (int, string, []byte, error) {
	response, err := client.Get(url)
	if err != nil {
		return 0, "", nil, err
	}
	chunk, readErr := io.ReadAll(io.LimitReader(response.Body, limit))
	_ = response.Body.Close()
	if readErr != nil {
		return response.StatusCode, response.Header.Get("Server"), chunk, readErr
	}
	return response.StatusCode, response.Header.Get("Server"), chunk, nil
}

// ---------------------------------------------------------------------------
// 页面重写：把页面依赖的外部 CDN 与 GitHub 直连改成本应用自己的路径
// ---------------------------------------------------------------------------

// GitHubRoutes 返回 GitHub 上游地址 → 本应用内路径的映射。
// 上游可用 GITHUB_API_BASE / GITHUB_RAW_BASE 覆盖（测试、私有镜像）；
// 无论覆盖成什么，默认地址也会保留，这样页面里两种写法都能被替换。
func GitHubRoutes() [][2]string {
	api := strings.TrimRight(firstEnv("GITHUB_API_BASE"), "/")
	if api == "" {
		api = "https://api.github.com"
	}
	raw := strings.TrimRight(firstEnv("GITHUB_RAW_BASE"), "/")
	if raw == "" {
		raw = "https://raw.githubusercontent.com"
	}
	seen := map[string]string{}
	order := []string{}
	for _, pair := range [][2]string{{api, "github/api/"}, {raw, "github/raw/"},
		{"https://api.github.com", "github/api/"}, {"https://raw.githubusercontent.com", "github/raw/"}} {
		if _, ok := seen[pair[0]]; !ok {
			seen[pair[0]] = pair[1]
			order = append(order, pair[0])
		}
	}
	routes := make([][2]string, 0, len(order)*2)
	for _, base := range order {
		local := seen[base]
		routes = append(routes, [2]string{base + "/", local}, [2]string{base, local})
	}
	return routes
}

// RewritePage 把页面里的 CDN 脚本与 GitHub 直连地址换成本应用的路径。
//
// 返回：改写后的 HTML、以及未能本地化的外部脚本地址（写日志提醒）。
func RewritePage(html, prefix, serviceBase string, routes [][2]string) (string, []string) {
	base := ""
	if trimmed := strings.Trim(prefix, "/"); trimmed != "" {
		base = "/" + trimmed
	}
	service := base
	if trimmed := strings.Trim(serviceBase, "/"); trimmed != "" {
		service = "/" + trimmed
	}
	unknown := []string{}
	rewritten := scriptSrcRegex.ReplaceAllStringFunc(html, func(match string) string {
		groups := scriptSrcRegex.FindStringSubmatch(match)
		for _, route := range assetRoutes {
			if strings.Contains(groups[2], route.marker) {
				return groups[1] + base + "/" + route.local + groups[3]
			}
		}
		unknown = append(unknown, groups[2])
		return match
	})
	for _, route := range routes {
		rewritten = strings.ReplaceAll(rewritten, route[0], service+"/"+route[1])
	}
	return rewritten, unknown
}

// PreparePage 在 RewritePage 之上补 favicon，并在有令牌时注入 localStorage。
func PreparePage(html, prefix, serviceBase, token string, routes [][2]string) (string, []string) {
	rewritten, unknown := RewritePage(html, prefix, serviceBase, routes)
	head := `<link rel="icon" href="` + prefix + `/assets/icon.png">`
	if strings.Contains(rewritten, "</head>") {
		rewritten = strings.Replace(rewritten, "</head>", head+"</head>", 1)
	} else {
		rewritten = head + rewritten
	}
	if token != "" {
		payload, err := json.Marshal(token)
		if err == nil {
			inject := `<script>try{localStorage.setItem("sc_token",` + string(payload) +
				`);}catch(e){}</script></head>`
			if strings.Contains(rewritten, "</head>") {
				rewritten = strings.Replace(rewritten, "</head>", inject, 1)
			}
		}
	}
	return rewritten, unknown
}

// GitHubUpstream 把 `github/api/…`、`github/raw/…` 映射回真正的 GitHub 地址。
func GitHubUpstream(rest string, routes [][2]string) string {
	path := strings.TrimLeft(rest, "/")
	for _, route := range routes {
		if strings.HasPrefix(path, route[1]) {
			return route[0] + path[len(route[1]):]
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func AssetsPresent(uiDir string) map[string]bool {
	result := make(map[string]bool, len(localAssets))
	for _, name := range localAssets {
		info, err := os.Stat(filepath.Join(uiDir, name))
		result[name] = err == nil && !info.IsDir()
	}
	return result
}

func SocketReady(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func stringOf(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func atoiDefault(value string, fallback int) int {
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		return fallback
	}
	return parsed
}

func floatDefault(value string, fallback float64) float64 {
	var parsed float64
	if _, err := fmt.Sscanf(value, "%g", &parsed); err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// NewHTTPClient 造一个不自动跟跳转、超时可控的客户端（探测/转发用）。
func NewHTTPClient(timeout time.Duration, followRedirects bool) *http.Client {
	client := &http.Client{Timeout: timeout}
	if !followRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

// BuildURL 拼接上游地址与查询串（转发用）。
//
// 两个必须遵守的细节：
//   - base 少尾斜杠、path 无前导斜杠时只补一个斜杠（多一个或少一个都会打到错的路径）；
//   - path 为空时不能补尾斜杠：GitHub 的 /repos/x/y/releases 被加上 "/" 后会被上游 301，
//     转发链就跟着跳走了（踩过）。
func BuildURL(base, path, query string) (string, error) {
	joined := strings.TrimRight(base, "/")
	if trimmed := strings.TrimLeft(path, "/"); trimmed != "" {
		joined += "/" + trimmed
	}
	parsed, err := url.Parse(joined)
	if err != nil {
		return "", err
	}
	if query != "" {
		parsed.RawQuery = query
	}
	return parsed.String(), nil
}

// ReadBodyLimited 读取请求体并限制大小（防止超大请求体打爆内存）。
func ReadBodyLimited(body io.Reader, limit int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	reader := io.LimitReader(body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("请求体过大")
	}
	return bytes.Clone(data), nil
}

// displayNameFallback 是读不到 manifest 时用的中文默认名。
const displayNameFallback = "unifi 软件中心"

// displayName 读 manifest 的 display_name，用作页面标题与离线提示。
// 这样改了 manifest 里的应用名，这些地方自动跟着变，不用改代码。
//
// 两个候选：开发时从 service/ 跑（包根在上一级），安装后是 /var/apps/<appname>/manifest。
func displayName() string {
	candidates := []string{
		filepath.Join("..", "manifest"),
		filepath.Join("/var/apps", firstEnvOr("TRIM_APPNAME", appNameDefault), "manifest"),
	}
	for _, candidate := range candidates {
		text, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimPrefix(string(text), "\ufeff"), "\n") {
			key, value, found := strings.Cut(line, "=")
			if found && strings.TrimSpace(key) == "display_name" && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return displayNameFallback
}

func firstEnvOr(name, fallback string) string {
	if value := firstEnv(name); value != "" {
		return value
	}
	return fallback
}
