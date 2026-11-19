package app

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrafficCounterTakeAndRestore(t *testing.T) {
	id, err := parseUUID("14725836-1234-5678-9abc-def012345678")
	if err != nil {
		t.Fatal(err)
	}

	counter := newTrafficCounter()
	counter.add(id, 10, 20)
	counter.add(id, 1, 2)

	taken := counter.take()
	if delta := taken[id]; delta.Up != 11 || delta.Down != 22 {
		t.Fatalf("taken = %+v, want 11/22", delta)
	}
	if again := counter.take(); len(again) != 0 {
		t.Fatalf("second take = %+v, want empty", again)
	}

	// take 之后的新流量与 restore 的旧流量应合并。
	counter.add(id, 5, 0)
	counter.restore(taken)
	restored := counter.take()
	if delta := restored[id]; delta.Up != 16 || delta.Down != 22 {
		t.Fatalf("restored = %+v, want 16/22", delta)
	}
}

func TestReportTraffic(t *testing.T) {
	var got nodeTrafficReport
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/nodes/traffic" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode traffic: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	id, err := parseUUID("14725836-1234-5678-9abc-def012345678")
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(nil, "")
	srv.addTraffic(id, 1024, 2048)
	if err := reportTraffic(
		NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"},
		srv,
	); err != nil {
		t.Fatalf("reportTraffic: %v", err)
	}

	if got.Name != "hk" || len(got.Traffic) != 1 {
		t.Fatalf("report = %+v", got)
	}
	entry := got.Traffic[0]
	if entry.ID != "14725836-1234-5678-9abc-def012345678" ||
		entry.Up != 1024 || entry.Down != 2048 {
		t.Fatalf("entry = %+v", entry)
	}
	if left := srv.takeTraffic(); len(left) != 0 {
		t.Fatalf("traffic not cleared after success: %+v", left)
	}
}

func TestReportTrafficRestoresOnFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	id, err := parseUUID("14725836-1234-5678-9abc-def012345678")
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(nil, "")
	srv.addTraffic(id, 7, 9)
	if err := reportTraffic(
		NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"},
		srv,
	); err == nil {
		t.Fatal("expected report error")
	}

	restored := srv.takeTraffic()
	if delta := restored[id]; delta.Up != 7 || delta.Down != 9 {
		t.Fatalf("restored = %+v, want 7/9", delta)
	}
}

func TestReportTrafficChunks(t *testing.T) {
	requests := 0
	entries := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report nodeTrafficReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Errorf("decode traffic: %v", err)
		}
		requests++
		entries += len(report.Traffic)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	srv := NewServer(nil, "")
	for index := 0; index < trafficReportBatchSize+1; index++ {
		var id [16]byte
		binary.BigEndian.PutUint64(id[:8], uint64(index+1))
		srv.addTraffic(id, 1, 1)
	}

	if err := reportTraffic(
		NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"},
		srv,
	); err != nil {
		t.Fatalf("reportTraffic: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if entries != trafficReportBatchSize+1 {
		t.Fatalf("entries = %d, want %d", entries, trafficReportBatchSize+1)
	}
}

func TestReportTrafficRestoresOnlyFailedChunks(t *testing.T) {
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	srv := NewServer(nil, "")
	for index := 0; index < trafficReportBatchSize+1; index++ {
		var id [16]byte
		binary.BigEndian.PutUint64(id[:8], uint64(index+1))
		srv.addTraffic(id, 1, 1)
	}

	if err := reportTraffic(
		NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"},
		srv,
	); err == nil {
		t.Fatal("expected report error")
	}

	restored := srv.takeTraffic()
	if len(restored) != 1 {
		t.Fatalf("restored %d entries, want only the failed chunk", len(restored))
	}
}
