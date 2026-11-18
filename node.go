package main

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
)

// nodeConfig 是 CONFIG=<baseURL>_<name>_<token> 的解析结果.
// name 与 token 中不要包含下划线.
type nodeConfig struct {
	baseURL string
	name    string
	token   string
}

func parseNodeConfig(raw string) (nodeConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nodeConfig{}, nil
	}

	last := strings.LastIndex(raw, "_")
	if last < 0 {
		return nodeConfig{}, fmt.Errorf("expected <baseURL>_<name>_<token>")
	}
	token := strings.TrimSpace(raw[last+1:])
	rest := raw[:last]

	sep := strings.LastIndex(rest, "_")
	if sep < 0 {
		return nodeConfig{}, fmt.Errorf("expected <baseURL>_<name>_<token>")
	}
	name := strings.TrimSpace(rest[sep+1:])
	baseURL := strings.TrimRight(strings.TrimSpace(rest[:sep]), "/")

	if baseURL == "" || name == "" || token == "" {
		return nodeConfig{}, fmt.Errorf("CONFIG requires baseURL, name and token")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nodeConfig{}, fmt.Errorf("baseURL must start with http:// or https://")
	}

	return nodeConfig{baseURL: baseURL, name: name, token: token}, nil
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
	// Tsnet 是主控下发的内嵌 Tailscale 开关；nil 视为关闭。
	Tsnet *tsnetSignal `json:"tsnet,omitempty"`
	// Tunnel 是主控下发的临时 cloudflared 隧道开关；nil 视为不改变。
	Tunnel *tunnelSignal `json:"tunnel,omitempty"`
}

// nodeSyncResult 汇总一次上报拿到的控制指令。
type nodeSyncResult struct {
	Update *nodeUpdate
	Tsnet  *tsnetSignal
	Tunnel *tunnelSignal
}

var syncHTTPClient = &http.Client{Timeout: 10 * time.Second}

type nodeReport struct {
	Name           string       `json:"name"`
	Runtime        string       `json:"runtime"`
	Version        string       `json:"version,omitempty"`
	OS             string       `json:"os,omitempty"`
	Arch           string       `json:"arch,omitempty"`
	TsnetSupported bool         `json:"tsnetSupported"`
	TsnetIP        string       `json:"tsnetIp"`
	Tunnel         tunnelReport `json:"tunnel"`
	Listen         string       `json:"listen,omitempty"`
	Path           string       `json:"path,omitempty"`
	Hosts          []string     `json:"hosts,omitempty"`
	IPv4           string       `json:"ipv4,omitempty"`
	IPv6           string       `json:"ipv6,omitempty"`
	CountryCode    string       `json:"countryCode,omitempty"`
	Latitude       *float64     `json:"latitude,omitempty"`
	Longitude      *float64     `json:"longitude,omitempty"`
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

func syncLoop(
	cfg nodeConfig,
	srv *server,
	listen, path string,
	static map[[16]byte]struct{},
	info nodeInfo,
	tsnetCtl tsnetController,
	tunnelCtl tunnelController,
) {
	ticker := time.NewTicker(nodeSyncInterval)
	defer ticker.Stop()

	for range ticker.C {
		result, err := syncDevices(
			cfg, srv, listen, path, static, info, tsnetCtl.IP(), tunnelCtl.Status(),
		)
		if err != nil {
			log.Printf("device sync failed: %v", err)
			continue
		}
		if result == nil {
			continue
		}

		tsnetCtl.Sync(result.Tsnet != nil && result.Tsnet.Enabled)
		if result.Tunnel != nil {
			tunnelCtl.Sync(result.Tunnel.Enabled)
		}
		handleNodeUpdate(cfg, result.Update)
	}
}

// syncDevices 向主控上报节点信息, 把返回的所有启用设备设为可认证 UUID,
// 并返回主控下发的更新指令、tsnet 与 tunnel 开关.
func syncDevices(
	cfg nodeConfig,
	srv *server,
	listen, path string,
	static map[[16]byte]struct{},
	info nodeInfo,
	tsnetIP string,
	tunnel tunnelReport,
) (*nodeSyncResult, error) {
	payload, err := json.Marshal(nodeReport{
		Name:           cfg.name,
		Runtime:        info.runtime,
		Version:        version,
		OS:             runtime.GOOS,
		Arch:           runtimeAssetArch(),
		TsnetSupported: tsnetSupported(),
		TsnetIP:        tsnetIP,
		Tunnel:         tunnel,
		Listen:         listen,
		Path:           path,
		Hosts:          info.hosts,
		IPv4:           info.ipv4,
		IPv6:           info.ipv6,
		CountryCode:    info.countryCode,
		Latitude:       info.latitude,
		Longitude:      info.longitude,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, cfg.baseURL+"/api/nodes/report", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.token)

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
	log.Printf("synced %d devices from %s", len(users), cfg.baseURL)
	return &nodeSyncResult{Update: result.Update, Tsnet: result.Tsnet, Tunnel: result.Tunnel}, nil
}
