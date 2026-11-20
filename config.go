package main

import (
	"flag"
	"os"
	"strconv"
	"strings"
)

// config 是命令行/环境变量解析后的启动配置.
type config struct {
	listen       string
	uuidList     string
	wsPath       string
	controlPlane string
	showVersion  bool

	// Hysteria 2 入口；Listen 为空表示不启动。
	//
	// 端口固定为 hy2Port（9500），主控订阅里也按同一常量生成，节点不需要
	// 单独上报端口。HY2_LISTEN 只作为开关使用：任何非空值都启用。
	hy2Listen   string
	hy2Password string
	hy2Cert     string
	hy2Key      string
	// hy2Domain 是自签证书的 CN/SAN；未配置证书时使用，默认 bing.com。
	hy2Domain string

	// Telegram Desktop 内置 web-proxy。
	//
	// TG_SECRET 是 32 hex（可带 dd/ee/ef 前缀）或 UUID 形式的密钥；为空表示
	// 不启用。启用后节点在 HTTPS 端口上提供 /api/v1/session、/api/v1/ws 和
	// ?bridge= 签名页面，Telegram Desktop 通过它连到 TG WebK。
	tgSecret   string
	bridgeHost string // 非空时启动后打印 bridge URL
}

// hy2Port 是 Hysteria 2 入口固定的 UDP 端口。
//
// 与主控 apps/web 的 HY2_PORT 保持一致：订阅生成时直接写这个端口，
// 不依赖节点上报。改这里必须同步改 apps/web/worker/lib/subscription.ts。
const hy2Port = 9500

// parseFlags 读取命令行参数与环境变量，并规范化路径.
//
// 使用独立的 FlagSet：依赖库可能在 init 时向全局 flag.CommandLine 注册
// 参数，复用全局集合容易触发 "flag redefined" panic，也不利于测试重复解析。
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
	hy2Listen := flags.String("hy2-listen", os.Getenv("HY2_LISTEN"), "enable Hysteria 2 on UDP "+strconv.Itoa(hy2Port)+" (empty = disabled)")
	hy2Password := flags.String("hy2-password", os.Getenv("HY2_PASSWORD"), "optional shared Hysteria 2 password (clients normally use their device UUID)")
	hy2Cert := flags.String("hy2-cert", os.Getenv("HY2_CERT"), "Hysteria 2 TLS certificate file (empty = self-signed)")
	hy2Key := flags.String("hy2-key", os.Getenv("HY2_KEY"), "Hysteria 2 TLS key file (empty = self-signed)")
	hy2Domain := flags.String("hy2-domain", os.Getenv("HY2_DOMAIN"), "Hysteria 2 self-signed certificate CN/SAN (default bing.com)")
	tgSecret := flags.String("tg-secret", os.Getenv("TG_SECRET"), "Telegram web-proxy secret (empty = disabled)")
	bridgeHost := flags.String("tg-bridge-host", os.Getenv("TG_BRIDGE_HOST"), "public HTTPS host to print the Telegram bridge URL for")
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
		hy2Listen:    strings.TrimSpace(*hy2Listen),
		hy2Password:  *hy2Password,
		hy2Cert:      strings.TrimSpace(*hy2Cert),
		hy2Key:       strings.TrimSpace(*hy2Key),
		hy2Domain:    strings.TrimSpace(*hy2Domain),
		tgSecret:     strings.TrimSpace(*tgSecret),
		bridgeHost:   strings.TrimSpace(*bridgeHost),
	}
}
