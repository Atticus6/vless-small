// Package platform 识别节点所在运行平台（SaaS/PaaS/本地）。
//
// vless-small 的多个子系统都需要判断平台：上报循环要决定是否常驻轮询，
// 证书管理只允许纯 Linux 的 local 环境。把识别逻辑放在独立包里，避免
// app 与 cert 之间产生循环依赖，也避免两处规则逐渐不一致。
package platform

import (
	"os"
	"strings"
)

// DetectRuntime 通过环境变量判断当前 SaaS/PaaS 平台。
//
// 显式设置的 NODE_RUNTIME / RUNTIME 优先，便于自托管环境覆盖自动识别；
// 无法匹配任何平台时返回 "local"。
func DetectRuntime() string {
	if v := firstNonEmpty(os.Getenv("NODE_RUNTIME"), os.Getenv("RUNTIME")); v != "" {
		// 覆盖值也统一小写，避免 RUNTIME=LOCAL/Railway 让后续精确匹配失效。
		return strings.ToLower(v)
	}

	switch {
	case envSet("RAILWAY_ENVIRONMENT") || envSet("RAILWAY_PUBLIC_DOMAIN"):
		return "railway"
	case envSet("RENDER") || envSet("RENDER_EXTERNAL_URL"):
		return "render"
	case envSet("FLY_APP_NAME"):
		return "fly"
	case envSet("KOYEB_APP_NAME") || envSet("KOYEB_PUBLIC_DOMAIN"):
		return "koyeb"
	case envSet("CF_PAGES") || envSet("CF_PAGES_URL"):
		return "cloudflare"
	case envSet("ZEABUR") || envSet("ZEABUR_WEB_URL"):
		return "zeabur"
	case envPrefixSet("NF_"):
		return "northflank"
	default:
		return "local"
	}
}

// IsAutoSleepRuntime 判断平台是否会在没有请求时自动休眠。
//
// 这类平台不能跑后台轮询，否则持续请求会阻止实例休眠；Northflank 等
// 常驻容器平台不受影响。
func IsAutoSleepRuntime(runtime string) bool {
	return runtime == "railway"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func envSet(key string) bool {
	return strings.TrimSpace(os.Getenv(key)) != ""
}

// envPrefixSet 检查是否存在以 prefix 开头的环境变量。
// Northflank 会注入 NF_* 变量（如 NF_HOSTS、NF_PLAN_ID）用于识别平台。
func envPrefixSet(prefix string) bool {
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
