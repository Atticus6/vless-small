package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/atticus6/vless-small/internal/cert"
	"github.com/atticus6/vless-small/internal/tunnel"
)

// Version 由 main 在启动时注入 Release 版本；main 里的 -ldflags -X 只作用于
// main.version，因此由 main 复制到 app。
var Version = "dev"

// NodeConfig 是 CONFIG=<baseURL>_<name>_<token> 的解析结果.
// Name 与 Token 中不要包含下划线.
type NodeConfig struct {
	BaseURL string
	Name    string
	Token   string
}

func ParseNodeConfig(raw string) (NodeConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return NodeConfig{}, nil
	}

	last := strings.LastIndex(raw, "_")
	if last < 0 {
		return NodeConfig{}, fmt.Errorf("expected <baseURL>_<name>_<token>")
	}
	token := strings.TrimSpace(raw[last+1:])
	rest := raw[:last]

	sep := strings.LastIndex(rest, "_")
	if sep < 0 {
		return NodeConfig{}, fmt.Errorf("expected <baseURL>_<name>_<token>")
	}
	name := strings.TrimSpace(rest[sep+1:])
	baseURL := strings.TrimRight(strings.TrimSpace(rest[:sep]), "/")

	if baseURL == "" || name == "" || token == "" {
		return NodeConfig{}, fmt.Errorf("CONFIG requires baseURL, name and token")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return NodeConfig{}, fmt.Errorf("baseURL must start with http:// or https://")
	}

	return NodeConfig{BaseURL: baseURL, Name: name, Token: token}, nil
}

type nodeDevice struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type nodeReportResponse struct {
	Devices []nodeDevice `json:"devices"`
	// Update 只在主控为该节点设置了目标版本、且节点版本落后时出现。
	Update *nodeUpdate `json:"update,omitempty"`
	// Tunnel 是主控下发的临时隧道开关；nil 视为不改变。
	Tunnel *tunnel.Signal `json:"tunnel,omitempty"`
	// Cert 是主控下发的证书域名；nil 视为不改变，空字符串表示关闭。
	Cert *cert.Signal `json:"cert,omitempty"`
	// Egress 是主控下发的出口选择开关；nil 视为不改变。
	Egress *egressSignal `json:"egress,omitempty"`
}

// egressSignal 是主控对“按 /ipN 选择出口”的总开关，默认关闭。
type egressSignal struct {
	Enabled bool `json:"enabled"`
}

// egressReport 是节点上报的出口地址列表和当前开关状态。
type egressReport struct {
	Enabled   bool            `json:"enabled"`
	Addresses []egressAddress `json:"addresses"`
}

// nodeSyncResult 汇总一次上报拿到的控制指令。
type nodeSyncResult struct {
	Update *nodeUpdate
	Tunnel *tunnel.Signal
	Cert   *cert.Signal
	Egress *egressSignal
}

var syncHTTPClient = &http.Client{Timeout: 10 * time.Second}

type nodeReport struct {
	Name    string        `json:"name"`
	Runtime string        `json:"runtime"`
	Version string        `json:"version,omitempty"`
	OS      string        `json:"os,omitempty"`
	Arch    string        `json:"arch,omitempty"`
	Tunnel  tunnel.Report `json:"tunnel"`
	Cert    cert.Report   `json:"cert"`
	Listen  string        `json:"listen,omitempty"`
	Path    string        `json:"path,omitempty"`
	// Egress 是本地出口地址列表和开关状态；非 local 运行时 addresses 为空。
	Egress      *egressReport `json:"egress,omitempty"`
	Hosts       []string      `json:"hosts,omitempty"`
	IPv4        string        `json:"ipv4,omitempty"`
	IPv6        string        `json:"ipv6,omitempty"`
	CountryCode string        `json:"countryCode,omitempty"`
	Latitude    *float64      `json:"latitude,omitempty"`
	Longitude   *float64      `json:"longitude,omitempty"`
}

// collectNodeInfo 汇总平台信息与本机公网 IP/地理位置.
func collectNodeInfo() nodeInfo {
	info := nodeInfo{
		runtime: detectRuntime(),
		hosts:   detectHosts(),
	}

	found, err := lookupIPInfo()
	if err != nil {
		log.Printf("ip lookup failed: %v", err)
	} else {
		info.ipv4 = found.ipv4
		info.ipv6 = found.ipv6
		info.countryCode = found.countryCode
		info.latitude = found.latitude
		info.longitude = found.longitude
	}

	if v := strings.TrimSpace(os.Getenv("NODE_IPV4")); v != "" {
		info.ipv4 = v
	}
	if v := strings.TrimSpace(os.Getenv("NODE_IPV6")); v != "" {
		info.ipv6 = v
	}
	return info
}

// reportCertState 在证书状态变化时立即回调主控。
//
// 上报循环仍会携带 cert 状态作为兜底；回调只是让“申请成功/失败/端口占用”
// 更快写入 D1，不必等下一轮 60 秒。
func reportCertState(cfg NodeConfig, status cert.Report) {
	if cfg.BaseURL == "" {
		return
	}

	payload, err := json.Marshal(struct {
		Name string      `json:"name"`
		Cert cert.Report `json:"cert"`
	}{
		Name: cfg.Name,
		Cert: status,
	})
	if err != nil {
		return
	}

	req, err := http.NewRequest(
		http.MethodPost,
		cfg.BaseURL+"/api/nodes/cert",
		bytes.NewReader(payload),
	)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err := syncHTTPClient.Do(req)
	if err != nil {
		log.Printf("cert callback failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		log.Printf("cert callback %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
}

// StartNodeSync 在配置了 CONFIG 时执行首次上报，并按平台决定是否后台轮询.
//
// Railway 无请求时会自动休眠；后台定时同步会产生持续请求，让实例
// 无法休眠，因此这两个平台只在冷启动时同步一次。其余平台（含 Northflank
// 这类常驻容器）保持 60 秒轮询，以便及时收到设备、隧道和证书指令。
func StartNodeSync(
	listen, path string,
	cfg NodeConfig,
	srv *Server,
	static map[[16]byte]struct{},
	tunnelCtl tunnel.Controller,
	certCtl cert.Controller,
) {
	if cfg.BaseURL == "" {
		return
	}

	// 证书状态变化时立即回调主控；上报循环里的 cert 字段作为兜底。
	certCtl.SetReporter(func(status cert.Report) {
		reportCertState(cfg, status)
	})

	info := collectNodeInfo()
	log.Printf(
		"node info runtime=%s hosts=%v ipv4=%s ipv6=%s country=%s tunnel=%v cert=%v",
		info.runtime, info.hosts, info.ipv4, info.ipv6, info.countryCode,
		tunnel.Supported(), certCtl.Status().Supported,
	)
	if addresses := srv.EgressAddresses(); len(addresses) > 0 {
		log.Printf(
			"egress candidates detected (enabled=%v): %v",
			srv.EgressEnabled(), addresses,
		)
	}

	result, err := syncDevices(
		cfg, srv, listen, path, static, info,
		tunnelCtl.Status(), certCtl.Status(),
	)
	if err != nil {
		log.Printf("initial device sync failed: %v", err)
	}
	if result != nil {
		if result.Tunnel != nil {
			tunnelCtl.Sync(*result.Tunnel)
		}
		if result.Cert != nil {
			certCtl.Sync(result.Cert.Domain)
		}
		if result.Egress != nil {
			srv.SetEgressEnabled(result.Egress.Enabled)
		}
		handleNodeUpdate(cfg, result.Update)
	}

	if isAutoSleepRuntime(info.runtime) {
		log.Printf("runtime %s: background sync disabled (auto-sleep)", info.runtime)
		return
	}
	go syncLoop(
		cfg, srv, listen, path, static, info,
		tunnelCtl, certCtl,
	)
	// 流量按设备 UUID 累计，每 30 分钟单独上报；自动休眠运行时没有常驻
	// 进程，无法可靠累计，跳过。
	go trafficLoop(cfg, srv)
}

func syncLoop(
	cfg NodeConfig,
	srv *Server,
	listen, path string,
	static map[[16]byte]struct{},
	info nodeInfo,
	tunnelCtl tunnel.Controller,
	certCtl cert.Controller,
) {
	ticker := time.NewTicker(nodeSyncInterval)
	defer ticker.Stop()

	for range ticker.C {
		result, err := syncDevices(
			cfg, srv, listen, path, static, info,
			tunnelCtl.Status(), certCtl.Status(),
		)
		if err != nil {
			log.Printf("device sync failed: %v", err)
			continue
		}
		if result == nil {
			continue
		}

		if result.Tunnel != nil {
			tunnelCtl.Sync(*result.Tunnel)
		}
		if result.Cert != nil {
			certCtl.Sync(result.Cert.Domain)
		}
		if result.Egress != nil {
			srv.SetEgressEnabled(result.Egress.Enabled)
		}
		handleNodeUpdate(cfg, result.Update)
	}
}

// syncDevices 向主控上报节点信息, 把返回的所有启用设备设为可认证 UUID,
// 并返回主控下发的更新指令、tunnel 开关与证书域名.
func syncDevices(
	cfg NodeConfig,
	srv *Server,
	listen, path string,
	static map[[16]byte]struct{},
	info nodeInfo,
	tunnel tunnel.Report,
	cert cert.Report,
) (*nodeSyncResult, error) {
	// 即使没有检测到地址也上报空数组，节点切换到非 local 运行时 Web 才能
	// 清掉旧的出口列表。
	addresses := srv.EgressAddresses()
	if addresses == nil {
		addresses = []egressAddress{}
	}
	egress := &egressReport{
		Enabled:   srv.EgressEnabled(),
		Addresses: addresses,
	}

	payload, err := json.Marshal(nodeReport{
		Name:        cfg.Name,
		Runtime:     info.runtime,
		Version:     Version,
		OS:          runtime.GOOS,
		Arch:        runtimeAssetArch(),
		Tunnel:      tunnel,
		Cert:        cert,
		Listen:      listen,
		Path:        path,
		Egress:      egress,
		Hosts:       info.hosts,
		IPv4:        info.ipv4,
		IPv6:        info.ipv6,
		CountryCode: info.countryCode,
		Latitude:    info.latitude,
		Longitude:   info.longitude,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, cfg.BaseURL+"/api/nodes/report", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err := syncHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("report %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var result nodeReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	users := make(map[[16]byte]struct{}, len(static)+len(result.Devices))
	for id := range static {
		users[id] = struct{}{}
	}
	for _, device := range result.Devices {
		if !device.Enabled {
			continue
		}
		id, err := parseUUID(device.ID)
		if err != nil {
			log.Printf("skip device %q: %v", device.Name, err)
			continue
		}
		users[id] = struct{}{}
	}

	srv.setUsers(users)
	log.Printf("synced %d devices from %s", len(users), cfg.BaseURL)
	return &nodeSyncResult{
		Update: result.Update,
		Tunnel: result.Tunnel,
		Cert:   result.Cert,
		Egress: result.Egress,
	}, nil
}
