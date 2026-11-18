package main

import "strings"

// certSignal 是主控在节点上报响应里返回的证书域名。
//
// 空字符串表示关闭自动证书；节点只在纯 Linux、非 Docker 环境启用。
type certSignal struct {
	Domain string `json:"domain"`
}

// certReport 是节点自动证书状态，随上报和 /<token>/config 返回。
type certReport struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Domain    string `json:"domain,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Issuer    string `json:"issuer,omitempty"`
	Error     string `json:"error,omitempty"`
}

// certController 抽象 ACME 证书生命周期；Vercel/非 Linux 构建使用空实现。
type certController interface {
	Sync(domain string)
	Status() certReport
	// SetReporter 在状态变化时回调主控，便于立即写入证书状态。
	SetReporter(func(certReport))
	Close()
}

// certRuntimeAllowed 判断平台运行时是否允许直接绑定 80/443。
//
// 托管容器/PaaS 的端口由平台转发，节点进程通常不能自行监听低端口；
// 即使容器检测漏判，也不在这些运行时上启动 ACME。
func certRuntimeAllowed(runtime string) bool {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "vercel", "railway", "render", "fly", "koyeb", "cloudflare",
		"zeabur", "northflank":
		return false
	default:
		return true
	}
}
