package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// nodeInfo 是节点上报所需的运行环境与网络信息.
type nodeInfo struct {
	runtime     string
	hosts       []string
	ipv4        string
	ipv6        string
	countryCode string
	latitude    *float64
	longitude   *float64
}

// detectRuntime 通过环境变量判断当前 SaaS 平台.
func detectRuntime() string {
	if v := firstNonEmpty(os.Getenv("NODE_RUNTIME"), os.Getenv("RUNTIME")); v != "" {
		return v
	}

	switch {
	case envSet("VERCEL") || envSet("VERCEL_URL"):
		return "vercel"
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
	case isNetlify():
		return "netlify"
	case envPrefixSet("NF_"):
		return "northflank"
	default:
		return "local"
	}
}

func envSet(key string) bool {
	return strings.TrimSpace(os.Getenv(key)) != ""
}

// isNetlify 识别 Netlify Functions/Server 运行时。
// NETLIFY=true 是 Netlify 注入的主要标记，NETLIFY_DEV 用于本地 dev.
func isNetlify() bool {
	return envSet("NETLIFY") || envSet("NETLIFY_DEV")
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

// isAutoSleepRuntime 判断平台是否会在没有请求时自动休眠。
// 这类平台不能跑后台轮询，否则持续请求会阻止实例休眠。
// Northflank 可以常驻，因此保持后台同步。
func isAutoSleepRuntime(runtime string) bool {
	return runtime == "vercel" || runtime == "railway" || runtime == "netlify"
}

// detectHosts 获取平台分配的公网地址.
func detectHosts() []string {
	if raw := os.Getenv("PUBLIC_HOSTS"); strings.TrimSpace(raw) != "" {
		return normalizeHosts(strings.Split(raw, ","))
	}
	if raw := os.Getenv("PUBLIC_HOST"); strings.TrimSpace(raw) != "" {
		return normalizeHosts([]string{raw})
	}

	// Northflank 注入的 NF_HOSTS 可能包含多个地址，用逗号或空白分隔。
	if raw := strings.TrimSpace(os.Getenv("NF_HOSTS")); raw != "" {
		return normalizeHosts(strings.Fields(strings.ReplaceAll(raw, ",", " ")))
	}

	// Netlify 的 URL 是稳定主站地址；DEPLOY_URL / DEPLOY_PRIME_URL 会随
	// 每次部署或分支预览变化，不能作为长期节点地址。
	if isNetlify() {
		if raw := strings.TrimSpace(os.Getenv("URL")); raw != "" {
			return normalizeHosts([]string{raw})
		}
		if site := strings.TrimSpace(os.Getenv("SITE_NAME")); site != "" {
			return []string{site + ".netlify.app"}
		}
	}

	// 只使用稳定的公网地址：
	// - 不用 VERCEL_URL：它是每次部署都会变化的 deployment URL；
	// - 不用 CF_PAGES_URL：它同样指向单次部署，不是稳定生产域名。
	candidates := []string{
		os.Getenv("VERCEL_PROJECT_PRODUCTION_URL"),
		os.Getenv("RAILWAY_PUBLIC_DOMAIN"),
		os.Getenv("RENDER_EXTERNAL_HOSTNAME"),
		os.Getenv("RENDER_EXTERNAL_URL"),
		os.Getenv("KOYEB_PUBLIC_DOMAIN"),
		os.Getenv("ZEABUR_WEB_URL"),
		os.Getenv("SERVICE_URL"),
		os.Getenv("PUBLIC_URL"),
	}
	if app := strings.TrimSpace(os.Getenv("FLY_APP_NAME")); app != "" {
		candidates = append(candidates, app+".fly.dev")
	}
	return normalizeHosts(candidates)
}

func normalizeHosts(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	hosts := make([]string, 0, len(raw))

	for _, item := range raw {
		host := normalizeHost(item)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	return hosts
}

func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(u.Host)
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

type ipLookupResponse struct {
	IP          string   `json:"ip"`
	IPv4        string   `json:"ipv4"`
	IPv6        string   `json:"ipv6"`
	Type        string   `json:"type"`
	Country     string   `json:"country"`
	CountryCode string   `json:"country_code"`
	Loc         string   `json:"loc"`
	Latitude    *float64 `json:"latitude"`
	Longitude   *float64 `json:"longitude"`
	Success     *bool    `json:"success"`
}

func lookupIPInfo() (nodeInfo, error) {
	endpoint := strings.TrimSpace(os.Getenv("IP_LOOKUP_URL"))
	if endpoint == "" {
		endpoint = "https://ipinfo.io/json"
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nodeInfo{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vless-small")

	resp, err := syncHTTPClient.Do(req)
	if err != nil {
		return nodeInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nodeInfo{}, fmt.Errorf("lookup %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var data ipLookupResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&data); err != nil {
		return nodeInfo{}, fmt.Errorf("decode lookup: %w", err)
	}
	if data.Success != nil && !*data.Success {
		return nodeInfo{}, fmt.Errorf("lookup returned success=false")
	}

	info := nodeInfo{
		ipv4:        strings.TrimSpace(data.IPv4),
		ipv6:        strings.TrimSpace(data.IPv6),
		countryCode: strings.ToUpper(firstNonEmpty(data.CountryCode, data.Country)),
		latitude:    data.Latitude,
		longitude:   data.Longitude,
	}

	ip := strings.TrimSpace(data.IP)
	if ip != "" {
		parsed := net.ParseIP(ip)
		switch {
		case parsed == nil:
		case parsed.To4() != nil:
			if info.ipv4 == "" {
				info.ipv4 = ip
			}
		default:
			if info.ipv6 == "" {
				info.ipv6 = ip
			}
		}
	}
	if strings.EqualFold(data.Type, "ipv4") && info.ipv4 == "" {
		info.ipv4 = ip
	}
	if strings.EqualFold(data.Type, "ipv6") && info.ipv6 == "" {
		info.ipv6 = ip
	}

	if info.latitude == nil || info.longitude == nil {
		if lat, lon, ok := parseLoc(data.Loc); ok {
			info.latitude, info.longitude = &lat, &lon
		}
	}
	return info, nil
}

func parseLoc(raw string) (float64, float64, bool) {
	parts := strings.Split(raw, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, false
	}
	lon, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, false
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false
	}
	return lat, lon, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
