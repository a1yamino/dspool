package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLogPath(t *testing.T) {
	cfg := testConfig()
	cfg.StateFile = filepath.Join("/tmp", "dspool", "data", "state.json")
	cfg.LogFile = ""

	if got := resolveLogPath(cfg, "claude"); got != filepath.Join("/tmp", "dspool", "data", "dspool-agent.log") {
		t.Fatalf("agent log path = %q", got)
	}
	if got := resolveLogPath(cfg, "codex"); got != filepath.Join("/tmp", "dspool", "data", "dspool-agent.log") {
		t.Fatalf("codex log path = %q", got)
	}
	if got := resolveLogPath(cfg, "serve"); got != "" {
		t.Fatalf("serve log path = %q, want stderr", got)
	}

	cfg.LogFile = "/var/log/dspool.log"
	if got := resolveLogPath(cfg, "claude"); got != "/var/log/dspool.log" {
		t.Fatalf("configured agent log path = %q", got)
	}
	if got := resolveLogPath(cfg, "serve"); got != "/var/log/dspool.log" {
		t.Fatalf("configured serve log path = %q", got)
	}
}

func TestNewLoggerWritesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dspool.log")
	log, closeLog, err := newLogger("info", path)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello from test", "answer", 42)
	closeLog()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello from test") {
		t.Fatalf("log file = %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("log file mode = %o, want private", info.Mode().Perm())
	}
}
