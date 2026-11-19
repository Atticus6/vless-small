package tunnel

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Signal 是主控在节点上报响应里返回的隧道配置。
//
// Token 是 Cloudflare 命名隧道 token（base64 JSON），Hostname 是已分配并
// 写入 DNS 的固定域名，例如 hk-tunnel.atticus.eu.org。Enabled=false 时
// 节点停止 cloudflared。
//
// 固定隧道默认开启但可能尚未分配：此时主控只下发 Enabled=true，Token 与
// Hostname 为空，节点使用 cloudflared quick tunnel（随机
// *.trycloudflare.com）兜底，保证节点在分配完成前仍可访问。
type Signal struct {
	Enabled  bool   `json:"enabled"`
	Token    string `json:"token,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// Report 是节点隧道状态。
//
// Supported 由构建标签决定：Vercel 构建为 false。URL 在固定隧道下是分配
// 的 hostname，在未分配时是 quick tunnel 的随机地址；临时地址在进程重启
// 后会变化，主控每次上报都会同步最新值。
type Report struct {
	Supported bool   `json:"supported"`
	URL       string `json:"url,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Controller 抽象隧道生命周期，Vercel 构建使用空实现。
//
// Sync 必须幂等：上报循环每 60 秒调用一次，只在开关/token/hostname 变化时
// 重启；固定隧道未分配时切换到临时隧道，分配完成后自动切回固定隧道。
// 启动失败时保留错误，后续 Sync 会重试。
type Controller interface {
	Sync(signal Signal)
	Status() Report
	Close()
}

// localTunnelPort 从监听地址中提取本地端口，供本地回源地址使用.
//
// ":8080"、"0.0.0.0:8080" 和 "[::]:8080" 都会得到 8080；cloudflared 固定
// 回源到 127.0.0.1，因此只需要端口。
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
