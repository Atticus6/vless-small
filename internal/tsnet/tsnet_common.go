package tsnet

import "strings"

// Config 是主控 CONFIG 中 tsnet 需要的字段。
type Config struct {
	BaseURL string
	Name    string
	Token   string
}

// Signal 是主控在节点上报响应里返回的 tsnet 开关。
//
// 节点只在开关为 true 时启动内嵌 Tailscale；关闭后停止客户端，但
// Tailscale 后台的设备记录会保留（非 ephemeral）。
type Signal struct {
	Enabled bool `json:"enabled"`
}

// tsnetConfig 是节点用通信密钥从主控拉取的 tsnet 启动配置。
//
// State / LogConf 是 base64 编码的文件内容：
//   - tailscaled.state 保存节点身份和网络状态，是跨重启保持身份的关键；
//   - tailscaled.log.conf 是日志策略，可能不存在。
type tsnetConfig struct {
	Enabled  bool   `json:"enabled"`
	Hostname string `json:"hostname"`
	AuthKey  string `json:"authKey"`
	State    string `json:"state"`
	LogConf  string `json:"logConf"`
}

// Controller 抽象 tsnet 生命周期，Vercel 构建使用空实现。
//
// Sync 必须幂等：上报循环每 60 秒调用一次，只有状态变化时才启动/停止。
type Controller interface {
	Sync(enabled bool)
	// IP 返回当前 tailnet IPv4；未连接时为空字符串。
	IP() string
	Close()
}

// tsnet 在 Dir 下使用的文件名，与 tailscaled 保持一致。
const (
	tsnetStateFile   = "tailscaled.state"
	tsnetLogConfFile = "tailscaled.log.conf"
	// 在 tailnet 上暴露与本地服务相同的 HTTP handler，便于通过 Tailscale
	// IP 访问健康检查和 WS（不影响公网监听端口）。
	tsnetListenAddr = ":8080"
)

// sanitizeTSNetHostname 把 vless 节点名转换成 Tailscale 可接受的 DNS label。
//
// 与 Web 端 Clash tailscale 出站的清洗规则一致：小写、非法字符折叠为
// "-"、去掉首尾连字符、最长 63 字符。清洗后为空时由调用方使用备用名。
func sanitizeTSNetHostname(value string) string {
	var builder strings.Builder
	lastHyphen := false

	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen && builder.Len() > 0 {
			builder.WriteByte('-')
			lastHyphen = true
		}
	}

	result := strings.Trim(builder.String(), "-")
	if len(result) > 63 {
		result = strings.TrimRight(result[:63], "-")
	}
	return result
}
