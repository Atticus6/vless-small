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

// shouldSyncCert 判断本次 Sync 是否可以跳过。
//
// 域名相同且证书已启用时不需要重启服务；关闭证书（空域名）且当前也没有
// 域名时同样无需处理。缺少后一条时，上报循环每 60 秒都会执行一次
// stop/notify，向主控发送没有变化的状态回调。非空域名启动失败（例如
// 80/443 被占用）仍要每次重试，因此不能只要域名相同就直接返回。
func shouldSyncCert(currentDomain string, enabled bool, nextDomain string) bool {
	if nextDomain != currentDomain {
		return false
	}
	return enabled || nextDomain == ""
}
