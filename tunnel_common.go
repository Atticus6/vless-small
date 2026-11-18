package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// tunnelSignal 是主控在节点上报响应里返回的临时隧道开关。
type tunnelSignal struct {
	Enabled bool `json:"enabled"`
}

// tunnelReport 是节点上报的 cloudflared 临时隧道状态。
//
// Supported 由构建标签决定：Vercel 构建为 false；URL 是 cloudflared 分配的
// *.trycloudflare.com 地址，重启后会变化。
type tunnelReport struct {
	Supported bool   `json:"supported"`
	URL       string `json:"url,omitempty"`
	Error     string `json:"error,omitempty"`
}

// tunnelController 抽象临时隧道生命周期，Vercel 构建使用空实现。
//
// Sync 必须幂等：上报循环每 60 秒调用一次，只在开关变化时启动/停止；
// 启动失败时保留错误，后续 Sync 会重试。
type tunnelController interface {
	Sync(enabled bool)
	Status() tunnelReport
	Close()
}

// localTunnelPort 从监听地址中提取本地端口，供 cloudflared ingress 使用.
//
// ":8080"、"0.0.0.0:8080" 和 "[::]:8080" 都会得到 8080；cloudflared 固定
// 回源到 localhost，因此只需要端口。
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
