package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/atticus6/vless-small/internal/cert"
	"github.com/atticus6/vless-small/internal/tunnel"
)

// 响应缺少 devices 时必须报错，而不是当成“没有设备”清空用户表：
// 自动休眠运行时没有后台轮询，清空后可能一直无法恢复。
func TestSyncDevicesRejectsMissingDevices(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 只有隧道字段，没有 devices。
		_, _ = w.Write([]byte(`{"tunnel":null}`))
	}))
	defer ts.Close()

	staticID := mustTestUUID(t)
	srv := NewServer(map[[16]byte]struct{}{staticID: {}}, "/ws")
	srv.setUsers(map[[16]byte]struct{}{staticID: {}})

	_, err := syncDevices(
		NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"},
		srv, ":8080", "/ws",
		map[[16]byte]struct{}{staticID: {}},
		nodeInfo{runtime: "local"},
		tunnel.Report{}, cert.Report{},
	)
	if err == nil {
		t.Fatal("expected error for missing devices array")
	}
	if !srv.hasUser(staticID) {
		t.Fatal("static user must survive a malformed response")
	}
}

// 显式空数组仍是合法的“清空动态用户”指令。
func TestSyncDevicesAcceptsEmptyDevices(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"devices":[]}`))
	}))
	defer ts.Close()

	staticID := mustTestUUID(t)
	dynamicID, err := parseUUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(map[[16]byte]struct{}{staticID: {}}, "/ws")
	srv.setUsers(map[[16]byte]struct{}{staticID: {}, dynamicID: {}})

	if _, err := syncDevices(
		NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"},
		srv, ":8080", "/ws",
		map[[16]byte]struct{}{staticID: {}},
		nodeInfo{runtime: "local"},
		tunnel.Report{}, cert.Report{},
	); err != nil {
		t.Fatalf("syncDevices: %v", err)
	}

	if srv.hasUser(dynamicID) {
		t.Fatal("explicit empty array should clear dynamic users")
	}
	if !srv.hasUser(staticID) {
		t.Fatal("static user must survive an empty sync")
	}
}
