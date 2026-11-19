package tunnel

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Signal 是主控在节点上报响应里返回的临时隧道开关。
//
// 启用后节点使用 cloudflared quick tunnel 申请随机 *.trycloudflare.com
// 地址；地址会随进程重启或重新申请变化，主控每次上报同步最新值。
type Signal struct {
	Enabled bool `json:"enabled"`
}

// Report 是节点临时隧道状态。
//
// Supported 当前构建固定为 true，保留字段用于兼容旧主控；URL 是
// Cloudflare 分配的 *.trycloudflare.com 地址，不可用时为空。
type Report struct {
	Supported bool   `json:"supported"`
	URL       string `json:"url,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Controller 抽象临时隧道生命周期。
//
// Sync 必须幂等：上报循环每 60 秒调用一次，只在开关变化时启动/停止；
// 启动失败时保留错误，后续 Sync 会重试。
type Controller interface {
	Sync(signal Signal)
	Status() Report
	Close()
}

// localTunnelPort 从监听地址中提取本地端口，供本地回源地址使用.
//
// ":8080"、"0.0.0.0:8080" 和 "[::]:8080" 都会得到 8080。
func localTunnelPort(listen string) (int, error) {
	_, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return 0, fmt.Errorf("listen %q: %w", listen, err)
	}

	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 || value > 65535 {
		return 0, fmt.Errorf("listen %q has invalid port", listen)
	}
	return value, nil
}

// localTunnelHost 从监听地址中提取 cloudflared 的回源主机.
//
// 通配地址（空、0.0.0.0、::）回源到 127.0.0.1；显式绑定某个地址时（例如
// 192.168.1.10:8080）必须回源到该地址——服务没有监听回环，固定用
// localhost 会让隧道的所有请求都连不上。
//
// 返回值已按 URL 要求处理 IPv6：`[::1]:8080` 得到 `[::1]`，拼进
// `http://host:port` 才不会解析失败。
func localTunnelHost(listen string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return "127.0.0.1"
	}
	host = strings.TrimSpace(host)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	// SplitHostPort 已经去掉方括号；IPv6 字面量必须重新加上。
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
