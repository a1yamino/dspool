package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMergesDefaultsAndReadsKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keys.txt"), []byte("primary = sk-aaaa\nsk-bbbb\n# ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgYAML := `
listen: 127.0.0.1:9090
state_file: state.json
keys_file: keys.txt
pool:
  strategy: spread
  balance_interval: 45s
  max_attempts: 6
models:
  default: deepseek-v4-pro
  small: deepseek-flash
  map:
    "claude-opus*": deepseek-v4-pro
keys:
  - label: inline
    key: sk-inline
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := Load(path, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected at least the dashboard warning")
	}
	if cfg.Listen != "127.0.0.1:9090" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.StateFile != filepath.Join(dir, "state.json") {
		t.Errorf("state file = %q", cfg.StateFile)
	}
	if got := cfg.Pool.BalanceInterval.D(); got != 45*time.Second {
		t.Errorf("balance interval = %v", got)
	}
	// These were not present in YAML and must keep their defaults even though
	// the pool mapping was present.
	if got := cfg.Pool.ColdCheckInterval.D(); got != 2*time.Minute {
		t.Errorf("cold check interval default lost: %v", got)
	}
	if cfg.Pool.MaxAttempts != 6 {
		t.Errorf("max attempts = %d", cfg.Pool.MaxAttempts)
	}
	if cfg.Models.Default != "deepseek-v4-pro" || cfg.Models.Small != "deepseek-flash" {
		t.Errorf("models = %+v", cfg.Models)
	}
	if cfg.Models.Map["claude-opus*"] != "deepseek-v4-pro" {
		t.Errorf("model map = %+v", cfg.Models.Map)
	}

	if len(cfg.Keys) != 3 {
		t.Fatalf("keys = %d, want 3: %+v", len(cfg.Keys), cfg.Keys)
	}
	if cfg.Keys[0].Label != "inline" || cfg.Keys[0].Key != "sk-inline" {
		t.Errorf("inline key = %+v", cfg.Keys[0])
	}
	if cfg.Keys[1].Label != "primary" || cfg.Keys[1].Key != "sk-aaaa" {
		t.Errorf("keys file labeled key = %+v", cfg.Keys[1])
	}
	if cfg.Keys[2].Key != "sk-bbbb" || cfg.Keys[2].Label == "" {
		t.Errorf("keys file bare key = %+v", cfg.Keys[2])
	}
}

func TestLoadDeduplicatesKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
keys:
  - sk-same
  - sk-same
  - sk-other
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Keys) != 2 {
		t.Fatalf("keys = %d, want 2", len(cfg.Keys))
	}
	found := false
	for _, w := range warnings {
		if w != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected duplicate warning")
	}
}

func TestLoadExplicitMissingFile(t *testing.T) {
	if _, _, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), true); err == nil {
		t.Fatal("expected error for missing explicit config")
	}
}

func TestLoadRejectsInvalidStrategy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("pool:\n  strategy: nope\nkeys: [sk-a]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path, true); err == nil {
		t.Fatal("expected invalid strategy error")
	}
}

func TestLoadFromEnvironment(t *testing.T) {
	t.Setenv("DSPOOL_KEYS", "sk-env-one,sk-env-two")
	t.Setenv("DSPOOL_CLIENT_KEYS", "local-token")
	t.Setenv("DSPOOL_STRATEGY", "weighted")
	cfg, _, err := Load("", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Keys) != 2 {
		t.Fatalf("keys = %d", len(cfg.Keys))
	}
	if len(cfg.ClientKeys) != 1 || cfg.ClientKeys[0] != "local-token" {
		t.Fatalf("client keys = %+v", cfg.ClientKeys)
	}
	if cfg.Pool.Strategy != "weighted" {
		t.Fatalf("strategy = %q", cfg.Pool.Strategy)
	}
}

func TestListenIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8787": true,
		"localhost:8787": true,
		"[::1]:8787":     true,
		"0.0.0.0:8787":   false,
		":8787":          false,
		"192.168.1.2:88": false,
	}
	for addr, want := range cases {
		if got := ListenIsLoopback(addr); got != want {
			t.Errorf("ListenIsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
