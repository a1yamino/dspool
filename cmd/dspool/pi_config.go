package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

const piProviderID = "dspool"

// piAgentDir mirrors pi's own resolution: $PI_CODING_AGENT_DIR, falling back
// to ~/.pi/agent.
func piAgentDir() (string, error) {
	dir := os.Getenv("PI_CODING_AGENT_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		return filepath.Join(home, ".pi", "agent"), nil
	}
	expanded, err := expandHome(dir)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		abs, err := filepath.Abs(expanded)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	return expanded, nil
}

func piModelsPath() (string, error) {
	dir, err := piAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "models.json"), nil
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// ensurePiProvider adds or updates a "dspool" provider in pi's models.json and
// returns the provider id and the model to pass on the CLI. Other providers and
// top-level fields are preserved byte-for-byte.
func ensurePiProvider(path, baseURL, defaultModel, smallModel string, log *slog.Logger) (string, string, error) {
	defaultModel = strings.TrimSpace(defaultModel)
	if defaultModel == "" {
		defaultModel = "deepseek-flash"
	}
	smallModel = strings.TrimSpace(smallModel)
	if smallModel == "" {
		smallModel = defaultModel
	}
	apiBase := strings.TrimRight(baseURL, "/") + "/v1"

	root := map[string]json.RawMessage{}
	providers := map[string]json.RawMessage{}

	data, readErr := os.ReadFile(path)
	switch {
	case readErr == nil && len(bytes.TrimSpace(data)) > 0:
		if err := json.Unmarshal(data, &root); err != nil {
			return "", "", fmt.Errorf("parse %s: %w (auto-setup needs strict JSON; remove comments or add providers.%s manually)", path, err, piProviderID)
		}
		if raw, ok := root["providers"]; ok {
			if err := json.Unmarshal(raw, &providers); err != nil {
				return "", "", fmt.Errorf("parse providers in %s: %w", path, err)
			}
		}
	case readErr != nil && !errors.Is(readErr, os.ErrNotExist):
		return "", "", fmt.Errorf("read %s: %w", path, readErr)
	}
	if root == nil {
		root = map[string]json.RawMessage{}
	}
	if providers == nil {
		providers = map[string]json.RawMessage{}
	}

	provider := map[string]any{}
	existingRaw, providerExists := providers[piProviderID]
	if providerExists {
		if err := json.Unmarshal(existingRaw, &provider); err != nil {
			return "", "", fmt.Errorf("parse providers.%s in %s: %w", piProviderID, path, err)
		}
	}
	if provider == nil {
		provider = map[string]any{}
	}
	original := cloneJSONMap(provider)

	provider["name"] = "dspool (DeepSeek pool)"
	provider["baseUrl"] = apiBase
	provider["api"] = "openai-completions"
	provider["apiKey"] = "$DSPOOL_API_KEY"
	provider["authHeader"] = true

	compat, _ := provider["compat"].(map[string]any)
	if compat == nil {
		compat = map[string]any{}
	}
	compatDefaults := map[string]any{
		"supportsDeveloperRole":   false,
		"supportsReasoningEffort": true,
		"supportsStore":           false,
		"maxTokensField":          "max_tokens",
		"thinkingFormat":          "deepseek",
	}
	for k, v := range compatDefaults {
		if _, ok := compat[k]; !ok {
			compat[k] = v
		}
	}
	provider["compat"] = compat

	models, _ := provider["models"].([]any)
	seen := map[string]bool{}
	for _, entry := range models {
		if m, ok := entry.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				seen[id] = true
			}
		}
	}
	for _, id := range []string{defaultModel, smallModel, "deepseek-flash", "deepseek-v4-pro"} {
		if id == "" || seen[id] {
			continue
		}
		models = append(models, piModelEntry(id))
		seen[id] = true
	}
	provider["models"] = models

	changed := !providerExists || !reflect.DeepEqual(original, provider)
	if !changed {
		return piProviderID, defaultModel, nil
	}

	encodedProvider, err := json.MarshalIndent(provider, "", "  ")
	if err != nil {
		return "", "", err
	}
	providers[piProviderID] = encodedProvider
	encodedProviders, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		return "", "", err
	}
	root["providers"] = encodedProviders
	if _, ok := root["$schema"]; !ok {
		root["$schema"] = json.RawMessage(`"https://pi.dev/schemas/models.schema.json"`)
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", "", err
	}
	out = append(out, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", fmt.Errorf("create pi config directory: %w", err)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".models-*.json")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", "", err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", "", err
	}
	if log != nil {
		log.Info("updated pi provider", "path", path, "provider", piProviderID, "base_url", apiBase, "model", defaultModel)
	}
	return piProviderID, defaultModel, nil
}

func cloneJSONMap(in map[string]any) map[string]any {
	data, err := json.Marshal(in)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return map[string]any{}
	}
	return out
}

func piModelEntry(id string) map[string]any {
	entry := map[string]any{
		"id":            id,
		"name":          id,
		"reasoning":     true,
		"input":         []any{"text", "image"},
		"contextWindow": modelContextWindow,
		"maxTokens":     65536,
	}
	switch id {
	case "deepseek-flash":
		entry["name"] = "DeepSeek Flash"
		entry["cost"] = map[string]any{
			"input": 0.15, "output": 0.60, "cacheRead": 0.003, "cacheWrite": 0.15,
		}
	case "deepseek-v4-pro":
		entry["name"] = "DeepSeek V4 Pro"
		entry["cost"] = map[string]any{
			"input": 0.66, "output": 1.98, "cacheRead": 0.022, "cacheWrite": 0.66,
		}
	default:
		entry["input"] = []any{"text"}
	}
	return entry
}
