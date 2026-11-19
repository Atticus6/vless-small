package app

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

	srv := NewServer(map[[16]byte]struct{}{staticID: {}}, "/ws")
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

	srv.HandleHTTP(rec, req)

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
	if srv.UserCount() != 2 {
		t.Fatalf("userCount = %d, want 2", srv.UserCount())
	}
}

func TestDevicePushRejectsMissingDevices(t *testing.T) {
	staticID := mustTestUUID(t)
	dynamicID, err := parseUUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}

	for name, payload := range map[string]string{
		"empty object": `{}`,
		"null body":    `null`,
		"null devices": `{"devices":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(map[[16]byte]struct{}{staticID: {}}, "/ws")
			srv.controlToken = "secret"

			// 先放一个动态用户，验证被拒绝的请求不会把它清掉。
			srv.setUsers(map[[16]byte]struct{}{staticID: {}, dynamicID: {}})

			req := httptest.NewRequest(
				http.MethodPost,
				"http://example.com/ws",
				bytes.NewReader([]byte(payload)),
			)
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()

			srv.HandleHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !srv.hasUser(dynamicID) {
				t.Fatal("rejected push must not drop existing users")
			}
		})
	}
}

func TestDevicePushAcceptsEmptyArray(t *testing.T) {
	staticID := mustTestUUID(t)
	dynamicID, err := parseUUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(map[[16]byte]struct{}{staticID: {}}, "/ws")
	srv.controlToken = "secret"
	srv.setUsers(map[[16]byte]struct{}{staticID: {}, dynamicID: {}})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/ws",
		bytes.NewReader([]byte(`{"devices":[]}`)),
	)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.HandleHTTP(rec, req)

	// 显式空数组是合法的“清空动态用户”操作，静态用户保留。
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if srv.hasUser(dynamicID) {
		t.Fatal("explicit empty array should clear dynamic users")
	}
	if !srv.hasUser(staticID) {
		t.Fatal("static user must survive an empty push")
	}
}

func TestDevicePushRejectsWrongToken(t *testing.T) {
	srv := NewServer(nil, "/ws")
	srv.controlToken = "secret"

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/ws",
		bytes.NewReader([]byte(`{"devices":[]}`)),
	)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()

	srv.HandleHTTP(rec, req)

	// 不是有效推送时继续走 WS 处理，普通 HTTP 请求返回 426。
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", rec.Code)
	}
}
