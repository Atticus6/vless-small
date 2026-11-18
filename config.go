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
func parseFlags() config {
	defaultListen := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		defaultListen = ":" + port
	}

	listen := flag.String("listen", defaultListen, "HTTP listen address")
	uuidList := flag.String("uuid", os.Getenv("UUID"), "comma-separated VLESS UUIDs")
	path := flag.String("path", os.Getenv("WS_PATH"), "WebSocket path to accept (empty = any)")
	controlPlane := flag.String("config", os.Getenv("CONFIG"), "control plane: <baseURL>_<name>_<token>")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

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
