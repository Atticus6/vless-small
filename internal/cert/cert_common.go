package cert

import "strings"

// Signal 是主控在节点上报响应里返回的证书域名。
//
// 空字符串表示关闭自动证书；节点只在纯 Linux、非 Docker 环境启用。
type Signal struct {
	Domain string `json:"domain"`
}

// Report 是节点自动证书状态，随上报和 /<token>/config 返回。
type Report struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Domain    string `json:"domain,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Issuer    string `json:"issuer,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Controller 抽象 ACME 证书生命周期；Vercel/非 Linux 构建使用空实现。
type Controller interface {
	Sync(domain string)
	Status() Report
	// SetReporter 在状态变化时回调主控，便于立即写入证书状态。
	SetReporter(func(Report))
	Close()
}

// certRuntimeAllowed 判断平台运行时是否允许直接绑定 80/443。
//
// 只有显式识别为 local 的纯 Linux 环境允许；所有托管容器/PaaS
// （Northflank、Vercel、Railway 等）即使是常驻容器也不启用 ACME。
func certRuntimeAllowed(runtime string) bool {
	return strings.ToLower(strings.TrimSpace(runtime)) == "local"
}
