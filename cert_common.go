package main

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
