package main

import (
	"flag"
	"os"
	"strings"
)

// config 是命令行/环境变量解析后的启动配置.
type config struct {
	listen       string
	uuidList     string
	wsPath       string
	controlPlane string
	showVersion  bool
}

// parseFlags 读取命令行参数与环境变量，并规范化路径.
//
// 使用独立的 FlagSet：cloudflared 依赖的 CoreDNS 会在 init 时向全局
// flag.CommandLine 注册 version/conf 等参数，复用全局集合会触发
// "flag redefined" panic。
func parseFlags() config {
	defaultListen := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		defaultListen = ":" + port
	}

	flags := flag.NewFlagSet("vless-small", flag.ExitOnError)
	listen := flags.String("listen", defaultListen, "HTTP listen address")
	uuidList := flags.String("uuid", os.Getenv("UUID"), "comma-separated VLESS UUIDs")
	path := flags.String("path", os.Getenv("WS_PATH"), "WebSocket path to accept (empty = any)")
	controlPlane := flags.String("config", os.Getenv("CONFIG"), "control plane: <baseURL>_<name>_<token>")
	showVersion := flags.Bool("version", false, "print version and exit")
	_ = flags.Parse(os.Args[1:])

	wsPath := strings.TrimSpace(*path)
	if wsPath != "" && !strings.HasPrefix(wsPath, "/") {
		wsPath = "/" + wsPath
	}

	return config{
		listen:       *listen,
		uuidList:     *uuidList,
		wsPath:       wsPath,
		controlPlane: *controlPlane,
		showVersion:  *showVersion,
	}
}
