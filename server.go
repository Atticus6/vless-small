package main

import (
	"crypto/subtle"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// server 持有可热更新的用户表与 WS 配置, 可被多个连接并发使用.
type server struct {
	users     atomic.Pointer[map[[16]byte]struct{}]
	path      string // WebSocket 路径；为空表示任意路径都接受
	upgrader  websocket.Upgrader
	active    atomic.Int64
	startedAt time.Time

	// controlToken 是 CONFIG 里的节点 token，用于主控推送设备列表。
	controlToken string
	// staticUsers 是 -uuid/环境变量提供的固定 UUID；推送时始终保留。
	staticUsers map[[16]byte]struct{}

	// /healthz 的 CPU 采样状态：保存上一次进程 CPU 时间，用两次差值算瞬时占用。
	cpuMu            sync.Mutex
	lastCPUSeconds   float64
	lastCPUSampledAt time.Time
	cpuSampled       bool

	// 系统 CPU 采样状态：/proc/stat 的累计总时间和空闲时间。
	sysCPUMu      sync.Mutex
	sysCPUTotal   uint64
	sysCPUIdle    uint64
	sysCPUSampled bool

	// 当前已认证的 WS 连接按 UUID 计数，用于 /<token>/config 展示。
	activeIDsMu sync.Mutex
	activeIDs   map[[16]byte]int
}

// healthResponse 是 /healthz 的响应，只保留探活必需的信息。
//
// 详细的 CPU/内存/磁盘信息放在 /<token>/config，避免公开探活接口泄露版本
// 之外的运行状态。
type healthResponse struct {
	OK            bool   `json:"ok"`
	Version       string `json:"version"`
	Uptime        string `json:"uptime"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}

// configResponse 是 /<token>/config 的响应，包含完整运行状态.
//
// 路径里的 token 就是节点 CONFIG 的最后一段，和 /api/nodes 通信使用同一个
// 密钥；错误 token 返回 404，不暴露该接口是否存在。
type configResponse struct {
	OK            bool   `json:"ok"`
	Version       string `json:"version"`
	Uptime        string `json:"uptime"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	GoVersion     string `json:"goVersion"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Hostname      string `json:"hostname,omitempty"`
	PID           int    `json:"pid"`
	// Devices 是当前进程接受的 UUID 数量（静态 UUID + 启用设备）。
	Devices           int          `json:"devices"`
	ActiveConnections int64        `json:"activeConnections"`
	ActiveUUIDs       []activeUUID `json:"activeUUIDs,omitempty"`
	Goroutines        int          `json:"goroutines"`
	CPU               cpuHealth    `json:"cpu"`
	Memory            memoryHealth `json:"memory"`
	Disk              *diskHealth  `json:"disk,omitempty"`
	// System 是宿主机/操作系统的 CPU、内存和磁盘占用（仅 Linux）。
	System *systemHealth `json:"system,omitempty"`
}

// activeUUID 是当前已认证连接的一个 UUID，Connections 是并发连接数.
type activeUUID struct {
	UUID        string `json:"uuid"`
	Connections int    `json:"connections"`
}

// systemHealth 是操作系统级资源占用.
//
// 容器里 /proc 通常反映宿主机；如果平台注入 lxcfs，则是容器配额。
type systemHealth struct {
	UptimeSeconds int64        `json:"uptimeSeconds"`
	CPU           systemCPU    `json:"cpu"`
	Memory        systemMemory `json:"memory"`
	Disk          []diskHealth `json:"disk,omitempty"`
}

// systemCPU 是整个系统的 CPU 占用，Percent 为所有核心的平均值。
type systemCPU struct {
	Percent float64 `json:"percent"`
	Cores   int     `json:"cores"`
	Load1   float64 `json:"load1"`
	Load5   float64 `json:"load5"`
	Load15  float64 `json:"load15"`
}

// systemMemory 来自 /proc/meminfo；UsedBytes = TotalBytes - AvailableBytes.
type systemMemory struct {
	TotalBytes     uint64  `json:"totalBytes"`
	UsedBytes      uint64  `json:"usedBytes"`
	FreeBytes      uint64  `json:"freeBytes"`
	AvailableBytes uint64  `json:"availableBytes"`
	UsedPercent    float64 `json:"usedPercent"`
	SwapTotalBytes uint64  `json:"swapTotalBytes"`
	SwapUsedBytes  uint64  `json:"swapUsedBytes"`
}

// cpuHealth 中的 Percent 是进程 CPU 占用（相对单核，可能超过 100）。
// 第一次请求按“累计 CPU 时间 / 运行时长”给出平均值，之后按两次采样差值计算。
type cpuHealth struct {
	Percent        float64 `json:"percent"`
	Cores          int     `json:"cores"`
	ProcessSeconds float64 `json:"processSeconds"`
}

// memoryHealth 同时给出 Go 堆信息和进程 RSS（RSS 仅 Linux 提供）.
type memoryHealth struct {
	AllocBytes     uint64 `json:"allocBytes"`
	HeapInuseBytes uint64 `json:"heapInuseBytes"`
	SysBytes       uint64 `json:"sysBytes"`
	RSSBytes       uint64 `json:"rssBytes,omitempty"`
	NumGC          uint32 `json:"numGC"`
}

// diskHealth 是文件系统或二进制所在目录的使用情况.
type diskHealth struct {
	Path        string  `json:"path"`
	Device      string  `json:"device,omitempty"`
	FSType      string  `json:"fsType,omitempty"`
	TotalBytes  uint64  `json:"totalBytes"`
	UsedBytes   uint64  `json:"usedBytes"`
	FreeBytes   uint64  `json:"freeBytes"`
	UsedPercent float64 `json:"usedPercent"`
}

func newServer(users map[[16]byte]struct{}, path string) *server {
	s := &server{
		path:        path,
		startedAt:   time.Now(),
		staticUsers: make(map[[16]byte]struct{}, len(users)),
		activeIDs:   make(map[[16]byte]int),
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(*http.Request) bool { return true },
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
	}
	for id := range users {
		s.staticUsers[id] = struct{}{}
	}
	s.setUsers(users)
	return s
}

func (s *server) setUsers(users map[[16]byte]struct{}) {
	if users == nil {
		users = map[[16]byte]struct{}{}
	}
	s.users.Store(&users)
}

func (s *server) hasUser(id [16]byte) bool {
	users := s.users.Load()
	if users == nil {
		return false
	}
	_, ok := (*users)[id]
	return ok
}

func (s *server) userCount() int {
	users := s.users.Load()
	if users == nil {
		return 0
	}
	return len(*users)
}

// addActiveUUID 记录一条已认证的 WS 连接.
func (s *server) addActiveUUID(id [16]byte) {
	s.activeIDsMu.Lock()
	defer s.activeIDsMu.Unlock()

	if s.activeIDs == nil {
		s.activeIDs = make(map[[16]byte]int)
	}
	s.activeIDs[id]++
}

// removeActiveUUID 在连接结束时递减；计数归零后删除，避免 map 持续膨胀.
func (s *server) removeActiveUUID(id [16]byte) {
	s.activeIDsMu.Lock()
	defer s.activeIDsMu.Unlock()

	count := s.activeIDs[id]
	if count <= 1 {
		delete(s.activeIDs, id)
		return
	}
	s.activeIDs[id] = count - 1
}

// activeConnectionCount 返回已认证的 WS 连接总数.
//
// 不用 s.active：它包含健康检查等普通 HTTP 请求，用在这里会把探活请求
// 也算成 VLESS 连接。
func (s *server) activeConnectionCount() int64 {
	s.activeIDsMu.Lock()
	defer s.activeIDsMu.Unlock()

	var total int64
	for _, count := range s.activeIDs {
		total += int64(count)
	}
	return total
}

// activeUUIDList 返回按 UUID 排序的活动连接列表，供 /<token>/config 使用.
func (s *server) activeUUIDList() []activeUUID {
	s.activeIDsMu.Lock()
	list := make([]activeUUID, 0, len(s.activeIDs))
	for id, count := range s.activeIDs {
		list = append(list, activeUUID{
			UUID:        formatUUID(id),
			Connections: count,
		})
	}
	s.activeIDsMu.Unlock()

	sort.Slice(list, func(i, j int) bool {
		return list[i].UUID < list[j].UUID
	})
	return list
}

// handleHTTP 处理 WebSocket 请求；POST WS 路径也用于主控设备推送.
func (s *server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if s.handleDevicePush(w, r) {
		return
	}
	s.handleWS(w, r)
}

// handleHealth 处理普通 HTTP 探活与节点配置查询请求.
//
// 浏览器/探活请求走普通 HTTP：根路径和 /healthz 返回健康信息，避免直接
// 看到 426/400；`/<token>/config` 在 token 正确时返回完整运行状态。
// 真正的 WebSocket 路径仍要求 Upgrade。返回 false 表示该请求不是这些
// 固定路径，由调用方继续返回 426.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}

	switch r.URL.Path {
	case "/healthz":
		// 公开探活只暴露存活状态与版本；详细资源信息走 /<token>/config。
		uptime := time.Since(s.startedAt)
		writeJSON(w, healthResponse{
			OK:            true,
			Version:       version,
			Uptime:        uptime.Truncate(time.Second).String(),
			UptimeSeconds: int64(uptime.Seconds()),
		})
		return true
	case "/":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("vless-small ok\n"))
		return true
	default:
		token, ok := configPathToken(r.URL.Path)
		if !ok {
			return false
		}
		// token 放在路径里会进入反向代理访问日志，这里只做等值校验；
		// 错误 token 统一返回 404，不确认接口是否存在。
		if !s.validControlToken(token) {
			http.NotFound(w, r)
			return true
		}

		now := time.Now()
		uptime := now.Sub(s.startedAt)
		cpuPercent, cpuSeconds := s.sampleCPU(now)
		hostname, _ := os.Hostname()

		var disk *diskHealth
		if usage, ok := readDiskUsage(executableDir()); ok {
			disk = usage
		}

		writeJSON(w, configResponse{
			OK:                true,
			Version:           version,
			Uptime:            uptime.Truncate(time.Second).String(),
			UptimeSeconds:     int64(uptime.Seconds()),
			GoVersion:         runtime.Version(),
			OS:                runtime.GOOS,
			Arch:              runtime.GOARCH,
			Hostname:          hostname,
			PID:               os.Getpid(),
			Devices:           s.userCount(),
			ActiveConnections: s.activeConnectionCount(),
			ActiveUUIDs:       s.activeUUIDList(),
			Goroutines:        runtime.NumGoroutine(),
			CPU: cpuHealth{
				Percent:        cpuPercent,
				Cores:          runtime.NumCPU(),
				ProcessSeconds: cpuSeconds,
			},
			Memory: readMemoryStats(),
			Disk:   disk,
			System: s.systemHealth(),
		})
		return true
	}
}

// configPathToken 从 `/<token>/config` 中提取 token；token 不能包含斜杠。
func configPathToken(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return "", false
	}
	token, ok := strings.CutSuffix(rest, "/config")
	if !ok || token == "" || strings.Contains(token, "/") {
		return "", false
	}
	return token, true
}

// validControlToken 对比路径 token 与 CONFIG token；未配置 CONFIG 时不可用。
func (s *server) validControlToken(token string) bool {
	if s.controlToken == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.controlToken)) == 1
}

// writeJSON 输出不缓存的 JSON 响应.
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

// sampleCPU 返回进程 CPU 占用（相对单核）和累计 CPU 秒数.
//
// 第一次调用没有上一个采样点，用“累计 CPU 时间 / 进程运行时长”估算；
// 之后的调用用两次采样的差值和实际间隔计算。读取失败时返回 0。
func (s *server) sampleCPU(now time.Time) (percent, seconds float64) {
	seconds, ok := processCPUSeconds()
	if !ok {
		return 0, 0
	}

	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()

	percent = cpuPercentSince(
		s.lastCPUSeconds,
		s.lastCPUSampledAt,
		s.cpuSampled,
		seconds,
		now,
		s.startedAt,
	)

	s.lastCPUSeconds = seconds
	s.lastCPUSampledAt = now
	s.cpuSampled = true

	return percent, round2(seconds)
}

// systemHealth 汇总操作系统级 CPU、内存和磁盘信息；非 Linux 返回 nil.
func (s *server) systemHealth() *systemHealth {
	info, ok := readSystemInfo()
	if !ok {
		return nil
	}
	if percent, ok := s.sampleSystemCPU(); ok {
		info.CPU.Percent = percent
	}
	return info
}

// sampleSystemCPU 返回系统 CPU 平均占用（所有核心）.
//
// 第一次调用用 /proc/stat 启动以来的累计值；之后按两次采样的差值计算。
func (s *server) sampleSystemCPU() (float64, bool) {
	total, idle, ok := readSystemCPUCounters()
	if !ok {
		return 0, false
	}

	s.sysCPUMu.Lock()
	defer s.sysCPUMu.Unlock()

	percent := systemCPUPercent(
		s.sysCPUTotal,
		s.sysCPUIdle,
		total,
		idle,
		s.sysCPUSampled,
	)
	s.sysCPUTotal = total
	s.sysCPUIdle = idle
	s.sysCPUSampled = true

	return percent, true
}

// cpuPercentSince 用累计 CPU 秒数计算进程 CPU 占用（相对单核）。
//
// previous=false 表示第一次采样，此时用累计 CPU 时间除以进程运行时长；
// 否则用两次采样的差值除以实际间隔。时间倒流或计数回退时返回 0。
func cpuPercentSince(
	prevSeconds float64,
	prevAt time.Time,
	previous bool,
	seconds float64,
	now time.Time,
	startedAt time.Time,
) float64 {
	if previous {
		elapsed := now.Sub(prevAt).Seconds()
		delta := seconds - prevSeconds
		if elapsed <= 0 || delta <= 0 {
			return 0
		}
		return round2(delta / elapsed * 100)
	}

	uptime := now.Sub(startedAt).Seconds()
	if uptime <= 0 || seconds <= 0 {
		return 0
	}
	return round2(seconds / uptime * 100)
}

// round2 保留两位小数，避免 JSON 里出现一长串浮点尾数.
func round2(value float64) float64 {
	if value < 0 {
		return 0
	}
	return math.Round(value*100) / 100
}

// readMemoryStats 汇总 Go 运行时内存和进程 RSS.
func readMemoryStats() memoryHealth {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	memory := memoryHealth{
		AllocBytes:     stats.Alloc,
		HeapInuseBytes: stats.HeapInuse,
		SysBytes:       stats.Sys,
		NumGC:          stats.NumGC,
	}
	if rss, ok := processRSSBytes(); ok {
		memory.RSSBytes = rss
	}
	return memory
}

// executableDir 返回二进制所在目录；磁盘统计以该目录所在文件系统为准。
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// waitDrain 等待已升级的 WebSocket 会话结束, 最多等 timeout.
func (s *server) waitDrain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for s.active.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}
