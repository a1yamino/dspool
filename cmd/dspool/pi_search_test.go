package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	dspooltools "dspool/integrations/pi/dspool-tools"
)

func TestEnsurePiSearchExtension(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	t.Setenv("DSPOOL_PI_SEARCH", "")

	path, err := ensurePiSearchExtension(discardLog())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "extensions", "dspool-tools", "index.ts")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(dspooltools.Source) {
		t.Fatal("provisioned extension does not match the embedded source")
	}

	// A stale or edited copy is restored.
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePiSearchExtension(discardLog()); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `name: "web_search"`) {
		t.Fatal("extension was not restored")
	}
}

func TestEnsurePiSearchExtensionOptOut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	t.Setenv("DSPOOL_PI_SEARCH", "0")

	path, err := ensurePiSearchExtension(discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty", path)
	}
	if _, err := os.Stat(filepath.Join(dir, "extensions")); !os.IsNotExist(err) {
		t.Fatalf("extensions directory was created: %v", err)
	}
}
