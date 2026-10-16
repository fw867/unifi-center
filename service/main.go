// main.go：命令行入口与启动流程。
//
// 子命令：
//
//	serve（默认）    启动服务：监听 $TRIM_APPDEST/app.sock（可另加调试端口）
//	seed             把向导环境变量（wizard_router_target / wizard_router_token）写入配置
//	migrate-legacy   从旧的小米插件目录搬一次设置（幂等，只读旧目录）
//	status           打印一份状态 JSON（排障用，不启动服务）
//	version          打印版本号
//	selftest         本机自检（配置目录可写、UI 资源齐全、Socket 路径可用）
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	command := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	env := LoadEnv()
	switch command {
	case "version":
		fmt.Println(appVersion)
		return
	case "status":
		printStatus(env)
		return
	case "seed":
		os.Exit(runSeed(env, false))
	case "migrate-legacy":
		os.Exit(runSeed(env, true))
	case "selftest":
		os.Exit(runSelfTest(env))
	case "serve":
		os.Exit(runServe(env))
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q（可用：serve / seed / migrate-legacy / status / version / selftest）\n", command)
		os.Exit(1)
	}
}

func runServe(env Env) int {
	logger := NewLogger(filepath.Join(env.ConfigDir, logFileName))
	store := NewStore(env.ConfigDir)
	service := NewService(env, store, logger)

	listeners, err := service.BuildListeners()
	if err != nil {
		fail(logger, fmt.Sprintf("启动失败：%v", err))
		return 1
	}

	logger.Printf("路由器软件中心服务已启动：目标 %s，前端 %s，网关前缀 %s，版本 %s",
		store.LoadSettings(env.DefaultTarg), env.UIDir, env.GatewayPfx, appVersion)
	logger.Printf("路径：TRIM_APPDEST=%q 应用目录=%q Socket=%q 配置=%q",
		env.AppDest, env.AppRoot, env.SocketPath, env.ConfigDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := service.Serve(ctx, listeners); err != nil {
		fail(logger, fmt.Sprintf("服务异常退出：%v", err))
		return 1
	}
	logger.Printf("服务已停止")
	if !env.SocketOnly {
		return 0
	}
	return 0
}

// fail 把用户可见的错误写进 TRIM_TEMP_LOGFILE（fnOS 会在应用中心展示它）。
func fail(logger *Logger, message string) {
	logger.Printf("%s", message)
	if path := strings.TrimSpace(os.Getenv("TRIM_TEMP_LOGFILE")); path != "" {
		_ = os.WriteFile(path, []byte(message+"\n"), 0o644)
	}
}

func printStatus(env Env) {
	logger := NewLogger("")
	store := NewStore(env.ConfigDir)
	service := NewService(env, store, logger)
	payload := service.StatusPayload(GatewayUser{})
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(payload)
}

// runSeed 把向导值落盘（幂等）；legacy 为真时改为从旧插件目录迁移。
//
// 向导字段是环境变量（不带 TRIM_ 前缀）：
//
//	wizard_router_target  路由器软件中心地址
//	wizard_router_token   AdminToken（留空表示保持原值不变）
func runSeed(env Env, legacy bool) int {
	logger := NewLogger(filepath.Join(env.ConfigDir, logFileName))
	store := NewStore(env.ConfigDir)
	if legacy {
		return migrateLegacy(store, logger)
	}

	rawTarget := strings.TrimSpace(os.Getenv("wizard_router_target"))
	rawToken := strings.TrimSpace(os.Getenv("wizard_router_token"))
	if rawTarget == "" && rawToken == "" {
		logger.Printf("向导没有提供地址或令牌，保持现有配置不变")
		return 0
	}
	if rawTarget != "" {
		saved, err := store.SaveTarget(rawTarget, env.DefaultTarg)
		if err != nil {
			fail(logger, "地址不合法："+err.Error())
			return 1
		}
		logger.Printf("已写入目标地址：%s", saved)
	}
	if rawToken != "" {
		if err := store.SaveToken(rawToken); err != nil {
			fail(logger, "写入令牌失败："+err.Error())
			return 1
		}
		logger.Printf("已写入 AdminToken（%s，文件权限 0600）", TokenHint(rawToken))
	} else {
		logger.Printf("令牌留空，保持原值不变")
	}
	return 0
}

// migrateLegacy 从旧的小米插件目录搬一次设置（本应用已有配置就不动）。
func migrateLegacy(store *Store, logger *Logger) int {
	if _, err := os.Stat(store.SettingsPath()); err == nil {
		logger.Printf("本应用已有配置，跳过旧数据迁移")
		return 0
	}
	legacyDir := strings.TrimSpace(os.Getenv("LEGACY_PLUGIN_DIR"))
	if legacyDir == "" {
		legacyDir = "/data/plugin/router-center"
	}
	legacySettings := filepath.Join(legacyDir, "settings.json")
	legacyToken := filepath.Join(legacyDir, "router-token")
	if _, err := os.Stat(legacySettings); err != nil {
		if _, tokenErr := os.Stat(legacyToken); tokenErr != nil {
			logger.Printf("没有找到可迁移的旧插件配置（%s）", legacyDir)
			return 0
		}
	}
	if data, err := os.ReadFile(legacySettings); err == nil {
		var payload struct {
			Target string `json:"target"`
		}
		if json.Unmarshal(data, &payload) == nil && payload.Target != "" {
			if _, err := store.SaveTarget(payload.Target, ""); err != nil {
				logger.Printf("旧地址不可用，跳过：%v", err)
			}
		}
	}
	if data, err := os.ReadFile(legacyToken); err == nil {
		if err := store.SaveToken(string(data)); err != nil {
			logger.Printf("旧令牌迁移失败：%v", err)
		}
	}
	logger.Printf("已从旧插件目录迁移设置：%s（旧目录保持不动）", legacyDir)
	return 0
}

// runSelfTest 是不需要网络的启动自检：路径能不能解析、目录可写、UI 资源齐全。
func runSelfTest(env Env) int {
	logger := NewLogger("")
	problems := []string{}

	// 路径解析情况（排障时最有用的信息：真实布局与文档不一致时一眼看出来）
	type layout struct {
		Path string `json:"path"`
		Bin  bool   `json:"has_bin"`
		UIC  bool   `json:"has_ui_config"`
		Sock bool   `json:"has_socket"`
	}
	layouts := []layout{}
	for _, candidate := range AppDestCandidates() {
		item := layout{Path: candidate}
		if info, err := os.Stat(filepath.Join(candidate, "bin", binaryName)); err == nil && !info.IsDir() {
			item.Bin = true
		}
		if info, err := os.Stat(filepath.Join(candidate, "ui", "config")); err == nil && !info.IsDir() {
			item.UIC = true
		}
		if info, err := os.Stat(filepath.Join(candidate, socketName)); err == nil && !info.IsDir() {
			item.Sock = true
		}
		layouts = append(layouts, item)
	}
	logger.Printf("布局候选：%+v", layouts)
	logger.Printf("解析结果：TRIM_APPDEST=%q 应用目录=%q UI=%q Socket=%q",
		env.AppDest, env.AppRoot, env.UIDir, env.SocketPath)

	if info, err := os.Stat(filepath.Join(env.AppRoot, "bin", binaryName)); err != nil || info.IsDir() {
		problems = append(problems, "应用目录里没有 bin/"+binaryName+"："+env.AppRoot)
	}

	if err := os.MkdirAll(env.ConfigDir, 0o700); err != nil {
		problems = append(problems, "配置目录不可写："+env.ConfigDir)
	}
	probe := filepath.Join(env.ConfigDir, ".selftest")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err == nil {
		_ = os.Remove(probe)
	} else {
		problems = append(problems, "配置目录不可写："+env.ConfigDir)
	}
	for name, present := range AssetsPresent(env.UIDir) {
		if !present {
			problems = append(problems, "缺少前端资源："+filepath.Join(env.UIDir, name))
		}
	}
	for _, name := range []string{"panel.html", "panel.js", "panel.css", "config"} {
		if _, err := os.Stat(filepath.Join(env.UIDir, name)); err != nil {
			problems = append(problems, "缺少前端文件："+filepath.Join(env.UIDir, name))
		}
	}
	if env.SocketOnly {
		if err := os.MkdirAll(filepath.Dir(env.SocketPath), 0o755); err != nil {
			problems = append(problems, "Socket 目录不可用："+env.SocketPath)
		}
	}

	report := map[string]any{
		"ok":          len(problems) == 0,
		"version":     appVersion,
		"appname":     env.AppName,
		"config_dir":  env.ConfigDir,
		"app_dest":    env.AppDest,
		"app_root":    env.AppRoot,
		"ui_dir":      env.UIDir,
		"socket":      env.SocketPath,
		"socket_only": env.SocketOnly,
		"layouts":     layouts,
		"problems":    problems,
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(report)
	if len(problems) > 0 {
		logger.Printf("自检未通过：%v", problems)
		return 1
	}
	logger.Printf("自检通过（版本 %s）", appVersion)
	return 0
}
