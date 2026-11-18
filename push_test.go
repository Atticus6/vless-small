package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDevicePush(t *testing.T) {
	staticID := mustTestUUID(t)
	dynamicID, err := parseUUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}
	disabledID, err := parseUUID("11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatal(err)
	}

	srv := newServer(map[[16]byte]struct{}{staticID: {}}, "/ws")
	srv.controlToken = "secret"

	body, err := json.Marshal(map[string]any{
		"devices": []map[string]any{
			{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "name": "phone", "enabled": true},
			{"id": "11111111-2222-3333-4444-555555555555", "name": "disabled", "enabled": false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://example.com/ws", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.handleHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !srv.hasUser(staticID) {
		t.Fatal("static user missing after push")
	}
	if !srv.hasUser(dynamicID) {
		t.Fatal("pushed user missing")
	}
	if srv.hasUser(disabledID) {
		t.Fatal("disabled device should be skipped")
	}
	if srv.userCount() != 2 {
		t.Fatalf("userCount = %d, want 2", srv.userCount())
	}
}

func TestDevicePushRejectsWrongToken(t *testing.T) {
	srv := newServer(nil, "/ws")
	srv.controlToken = "secret"

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/ws",
		bytes.NewReader([]byte(`{"devices":[]}`)),
	)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()

	srv.handleHTTP(rec, req)

	// 不是有效推送时继续走 WS 处理，普通 HTTP 请求返回 426。
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", rec.Code)
	}
}
