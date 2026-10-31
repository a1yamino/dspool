package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDaemonLogPath(t *testing.T) {
	cfg := testConfig()
	cfg.StateFile = "/tmp/dspool/data/state.json"
	cfg.LogFile = ""
	if got := daemonLogPath(cfg); got != "/tmp/dspool/data/dspool.log" {
		t.Fatalf("daemon log path = %q", got)
	}
	cfg.LogFile = "/var/log/dspool.log"
	if got := daemonLogPath(cfg); got != "/var/log/dspool.log" {
		t.Fatalf("configured daemon log path = %q", got)
	}
}

func TestDaemonPIDFromData(t *testing.T) {
	if got := daemonPIDFromData(map[string]any{"pid": float64(123)}); got != 123 {
		t.Fatalf("pid = %d", got)
	}
	if got := daemonPIDFromData(map[string]any{}); got != 0 {
		t.Fatalf("pid = %d", got)
	}
}

func TestFormatUptime(t *testing.T) {
	cases := map[int64]string{
		5:    "5s",
		65:   "1m",
		3661: "1h1m",
	}
	for seconds, want := range cases {
		if got := formatUptime(seconds); got != want {
			t.Errorf("formatUptime(%d) = %q, want %q", seconds, got, want)
		}
	}
}

func TestDaemonBaseRejectsNonLoopbackWithoutKeys(t *testing.T) {
	cfg := testConfig()
	cfg.ClientKeys = nil
	cfg.Listen = "192.168.1.5:8787"
	if _, err := daemonBase(cfg); err == nil {
		t.Fatal("expected an error for non-loopback daemon without client_keys")
	}
	cfg.Listen = "127.0.0.1:8787"
	if _, err := daemonBase(cfg); err != nil {
		t.Fatalf("loopback daemon base: %v", err)
	}
	cfg.Listen = "127.0.0.1:0"
	if _, err := daemonBase(cfg); err == nil {
		t.Fatal("expected an error for an auto-assigned port")
	}
}

func TestCheckDaemonToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := checkDaemonToken(srv.URL, "good"); err != nil {
		t.Fatalf("good token: %v", err)
	}
	if err := checkDaemonToken(srv.URL, "bad"); err == nil {
		t.Fatal("expected an error for a rejected token")
	}
}
