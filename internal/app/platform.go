package app

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/atticus6/vless-small/internal/platform"
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

// detectRuntime 通过环境变量判断当前 SaaS 平台，规则见 internal/platform。
func detectRuntime() string {
	return platform.DetectRuntime()
}

// isAutoSleepRuntime 判断平台是否会在没有请求时自动休眠。
// 这类平台不能跑后台轮询，否则持续请求会阻止实例休眠。
// Northflank 可以常驻，因此保持后台同步。
func isAutoSleepRuntime(runtime string) bool {
	return platform.IsAutoSleepRuntime(runtime)
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

	// 只使用稳定的公网地址：
	// - 不用 CF_PAGES_URL：它指向单次部署，不是稳定生产域名。
	candidates := []string{
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
	// NaN 能通过 ParseFloat，且与任何数字比较都为 false，范围检查拦不住它。
	// 一旦写进 info 会导致后续 json.Marshal 失败，节点再也发不出上报，
	// 而 syncLoop 复用启动时的 info，重启前都无法恢复。这里按缺失处理。
	if math.IsNaN(lat) || math.IsNaN(lon) ||
		math.IsInf(lat, 0) || math.IsInf(lon, 0) {
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

// PublicIPv4 返回本机公网 IPv4；NODE_IPV4 显式覆盖优先。
//
// 供其他协议入口（如 Hysteria 2 的端口可达性探测）复用同一套 IP 查询逻辑，
// 避免重复实现或把整个 nodeInfo 暴露出去。查询失败时返回空字符串。
func PublicIPv4() string {
	if v := strings.TrimSpace(os.Getenv("NODE_IPV4")); v != "" {
		return v
	}
	info, err := lookupIPInfo()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(info.ipv4)
}
