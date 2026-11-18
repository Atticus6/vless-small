package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func TestHealthEndpointIsMinimal(t *testing.T) {
	srv := newServer(map[[16]byte]struct{}{mustTestUUID(t): {}}, "/ws")
	// 让运行时长稳定大于 1 秒，避免 uptimeSeconds 因调度抖动变成 0。
	srv.startedAt = time.Now().Add(-2 * time.Second)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	if !srv.handleHealth(rec, req) {
		t.Fatal("handleHealth returned false for /healthz")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// 公开探活只应包含 ok/version/uptime/uptimeSeconds 四个字段。
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if len(raw) != 4 {
		t.Fatalf("/healthz fields = %v, want only 4 basic fields", raw)
	}

	var got healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if !got.OK {
		t.Fatal("ok = false")
	}
	if got.Version != version {
		t.Fatalf("version = %q, want %q", got.Version, version)
	}
	if got.UptimeSeconds < 2 {
		t.Fatalf("uptimeSeconds = %d, want >= 2", got.UptimeSeconds)
	}
}

func TestConfigEndpointIncludesRuntimeStats(t *testing.T) {
	srv := newServer(map[[16]byte]struct{}{mustTestUUID(t): {}}, "/ws")
	srv.controlToken = "secret"
	srv.startedAt = time.Now().Add(-2 * time.Second)

	req := httptest.NewRequest(http.MethodGet, "/secret/config", nil)
	rec := httptest.NewRecorder()

	if !srv.handleHealth(rec, req) {
		t.Fatal("handleHealth returned false for /<token>/config")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode config response: %v", err)
	}

	if !got.OK || got.Version != version {
		t.Fatalf("response = %+v, want ok with version %q", got, version)
	}
	if got.UptimeSeconds < 2 {
		t.Fatalf("uptimeSeconds = %d, want >= 2", got.UptimeSeconds)
	}
	if got.Devices != 1 {
		t.Fatalf("devices = %d, want 1", got.Devices)
	}
	if got.ActiveConnections != 0 {
		t.Fatalf("activeConnections = %d, want 0", got.ActiveConnections)
	}
	if got.Goroutines <= 0 {
		t.Fatalf("goroutines = %d, want > 0", got.Goroutines)
	}
	if got.CPU.Cores <= 0 {
		t.Fatalf("cpu.cores = %d, want > 0", got.CPU.Cores)
	}
	if got.CPU.Percent < 0 || got.CPU.ProcessSeconds < 0 {
		t.Fatalf("cpu = %+v, want non-negative", got.CPU)
	}
	if got.Memory.AllocBytes == 0 || got.Memory.SysBytes == 0 {
		t.Fatalf("memory = %+v, want non-zero alloc/sys", got.Memory)
	}
	if got.GoVersion == "" || got.OS == "" || got.Arch == "" {
		t.Fatalf("runtime info incomplete: %+v", got)
	}

	// 系统级指标只在 Linux 采集；CI 与生产环境都会覆盖到这里。
	if runtime.GOOS == "linux" {
		if got.System == nil {
			t.Fatal("system stats missing on Linux")
		}
		if got.System.Memory.TotalBytes == 0 || got.System.Memory.UsedBytes == 0 {
			t.Fatalf("system memory = %+v, want non-zero", got.System.Memory)
		}
		if got.System.CPU.Cores <= 0 {
			t.Fatalf("system cpu.cores = %d, want > 0", got.System.CPU.Cores)
		}
		if got.System.CPU.Percent < 0 {
			t.Fatalf("system cpu.percent = %v, want >= 0", got.System.CPU.Percent)
		}
		if len(got.System.Disk) == 0 {
			t.Fatal("system disk list is empty")
		}
	}
}

func TestConfigEndpointRejectsBadToken(t *testing.T) {
	tests := []struct {
		name         string
		controlToken string
		path         string
	}{
		{name: "错误 token", controlToken: "secret", path: "/wrong/config"},
		{name: "未配置 CONFIG", controlToken: "", path: "/secret/config"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(nil, "/ws")
			srv.controlToken = tt.controlToken

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			if !srv.handleHealth(rec, req) {
				t.Fatal("config path was not recognized")
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestInvalidConfigPathFallsThrough(t *testing.T) {
	srv := newServer(nil, "/ws")
	srv.controlToken = "secret"

	for _, path := range []string{"//config", "/a/secret/config", "/secret/config/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		if srv.handleHealth(rec, req) {
			t.Fatalf("handleHealth(%q) = true, want false", path)
		}
	}
}

func TestConfigPathToken(t *testing.T) {
	tests := []struct {
		path  string
		token string
		ok    bool
	}{
		{path: "/abc/config", token: "abc", ok: true},
		{path: "/abc", ok: false},
		{path: "/config", ok: false},
		{path: "/a/b/config", ok: false},
		{path: "/abc/config/", ok: false},
		{path: "/abc/configs", ok: false},
	}

	for _, tt := range tests {
		token, ok := configPathToken(tt.path)
		if ok != tt.ok || token != tt.token {
			t.Fatalf("configPathToken(%q) = (%q, %v), want (%q, %v)", tt.path, token, ok, tt.token, tt.ok)
		}
	}
}

func TestActiveUUIDTracking(t *testing.T) {
	srv := newServer(nil, "/ws")
	id := mustTestUUID(t)

	srv.addActiveUUID(id)
	srv.addActiveUUID(id)

	list := srv.activeUUIDList()
	if len(list) != 1 {
		t.Fatalf("active UUIDs = %d, want 1", len(list))
	}
	if list[0].UUID != formatUUID(id) || list[0].Connections != 2 {
		t.Fatalf("active UUID = %+v, want uuid %q with 2 connections", list[0], formatUUID(id))
	}
	if got := srv.activeConnectionCount(); got != 2 {
		t.Fatalf("activeConnectionCount() = %d, want 2", got)
	}

	srv.removeActiveUUID(id)
	list = srv.activeUUIDList()
	if len(list) != 1 || list[0].Connections != 1 {
		t.Fatalf("after one remove: %+v", list)
	}

	srv.removeActiveUUID(id)
	if list = srv.activeUUIDList(); len(list) != 0 {
		t.Fatalf("after all removed: %+v", list)
	}
}

func TestHealthRejectsNonGet(t *testing.T) {
	srv := newServer(nil, "/ws")
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()

	if srv.handleHealth(rec, req) {
		t.Fatal("handleHealth handled POST, want false")
	}
}

func TestCPUPercentSince(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name        string
		prevSeconds float64
		prevAt      time.Time
		previous    bool
		seconds     float64
		want        float64
	}{
		{
			name:     "首次采样按累计时间除以运行时长",
			previous: false,
			seconds:  5,
			prevAt:   now.Add(-10 * time.Second),
			want:     50,
		},
		{
			name:        "后续采样按差值计算",
			previous:    true,
			prevSeconds: 5,
			prevAt:      now.Add(-2 * time.Second),
			seconds:     5.4,
			want:        20,
		},
		{
			name:        "时间倒流返回 0",
			previous:    true,
			prevSeconds: 5,
			prevAt:      now.Add(2 * time.Second),
			seconds:     5.4,
			want:        0,
		},
		{
			name:        "CPU 计数回退返回 0",
			previous:    true,
			prevSeconds: 6,
			prevAt:      now.Add(-2 * time.Second),
			seconds:     5.4,
			want:        0,
		},
		{
			name:     "首次采样没有 CPU 时间返回 0",
			seconds:  0,
			prevAt:   now.Add(-10 * time.Second),
			previous: false,
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cpuPercentSince(
				tt.prevSeconds,
				tt.prevAt,
				tt.previous,
				tt.seconds,
				now,
				now.Add(-10*time.Second),
			)
			if got != tt.want {
				t.Fatalf("cpuPercentSince() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRound2(t *testing.T) {
	if got := round2(12.345); got != 12.35 {
		t.Fatalf("round2(12.345) = %v, want 12.35", got)
	}
	if got := round2(-1); got != 0 {
		t.Fatalf("round2(-1) = %v, want 0", got)
	}
}
