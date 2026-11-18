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
	xhttpPath    string
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
	xhttpPathFlag := flag.String(
		"xhttp-path",
		os.Getenv("XHTTP_PATH"),
		"XHTTP stream-one path (default /xhttp; use none to disable)",
	)
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
		xhttpPath:    resolveXHTTPPath(*xhttpPathFlag),
		controlPlane: *controlPlane,
		showVersion:  *showVersion,
	}
}

// resolveXHTTPPath 解析 XHTTP 路径。
//
// 空值使用默认 /xhttp（主控订阅默认包含 XHTTP 节点）；
// none/off/- 用于显式禁用，其他值按路径规范化。
func resolveXHTTPPath(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "":
		return defaultXHTTPPath
	case xhttpDisabledValue, "off", "-":
		return ""
	default:
		return normalizeHTTPPath(raw)
	}
}

// normalizeHTTPPath 统一路径格式：确保以 / 开头，去掉尾部斜杠.
func normalizeHTTPPath(raw string) string {
	path := strings.TrimSpace(raw)
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	if path == "" {
		return "/"
	}
	return path
}
