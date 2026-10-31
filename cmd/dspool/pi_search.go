package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	dspooltools "dspool/integrations/pi/dspool-tools"
)

const piSearchExtensionDir = "dspool-tools"

// ensurePiSearchExtension writes the embedded pi extension into pi's
// extensions directory. pi discovers it automatically; the extension only
// registers its tools when dspool's environment variables are present.
//
// Set DSPOOL_PI_SEARCH=0 to opt out.
func ensurePiSearchExtension(log *slog.Logger) (string, error) {
	if strings.TrimSpace(os.Getenv("DSPOOL_PI_SEARCH")) == "0" {
		return "", nil
	}
	dir, err := piAgentDir()
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, "extensions", piSearchExtensionDir, "index.ts")

	current, err := os.ReadFile(target)
	if err == nil && bytes.Equal(current, dspooltools.Source) {
		return target, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(target, dspooltools.Source, 0o644); err != nil {
		return "", err
	}
	log.Debug("installed pi search extension", "path", target)
	return target, nil
}
