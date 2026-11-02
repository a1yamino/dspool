package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"dspool/internal/config"
)

// Codex picks reasoning levels from its model metadata. A model that is not in
// that metadata gets fallback metadata with no supported levels, so the TUI
// cannot offer a reasoning picker for it. To expose DeepSeek models there, the
// codex shortcut generates a catalog that replaces Codex's bundled one.
//
// Codex rejects catalog entries that carry no instructions, so every DeepSeek
// entry is cloned from Codex's bundled GPT-6 Astra entry and keeps its prompt.
const codexCatalogTemplateSlug = "gpt-6-astra"

// These are DeepSeek's canonical thinking levels. The API also accepts aliases
// (minimal, medium, xhigh, ultra), but they collapse onto low, high or max, so
// exposing them here would only add duplicate choices.
var codexReasoningLevels = []any{
	map[string]any{"effort": "low", "description": "Low thinking effort"},
	map[string]any{"effort": "high", "description": "High thinking effort (DeepSeek default)"},
	map[string]any{"effort": "max", "description": "Maximum thinking effort"},
}

// ensureCodexModelCatalog writes <state_dir>/codex-model-catalog.json and
// returns its absolute path.
func ensureCodexModelCatalog(codexBin string, cfg *config.Config, logger *slog.Logger) (string, error) {
	bundled, err := exec.Command(codexBin, "debug", "models", "--bundled").Output()
	if err != nil {
		return "", fmt.Errorf("read Codex bundled model catalog: %w", err)
	}
	data, err := buildCodexModelCatalog(bundled, codexCatalogModelIDs(cfg))
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(cfg.StateFile), "codex-model-catalog.json")
	if abs, absErr := filepath.Abs(path); absErr == nil {
		path = abs
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create catalog directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "codex-model-catalog-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary model catalog: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("write model catalog: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("close model catalog: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("replace model catalog: %w", err)
	}
	logger.Debug("wrote Codex model catalog", "path", path)
	return path, nil
}

// codexCatalogModelIDs lists the models the proxy routes to. They are the ones
// worth showing in Codex's /model picker.
func codexCatalogModelIDs(cfg *config.Config) []string {
	candidates := []string{
		cfg.Models.Default,
		cfg.Models.Small,
		"deepseek-flash",
		"deepseek-v4-pro",
	}
	patterns := make([]string, 0, len(cfg.Models.Map))
	for pattern := range cfg.Models.Map {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	for _, pattern := range patterns {
		candidates = append(candidates, cfg.Models.Map[pattern])
	}
	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, id := range candidates {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// buildCodexModelCatalog takes the output of `codex debug models --bundled`
// and returns a catalog containing DeepSeek entries cloned from the template
// model's metadata and instructions.
func buildCodexModelCatalog(bundled []byte, ids []string) ([]byte, error) {
	var source struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(bundled, &source); err != nil {
		return nil, fmt.Errorf("parse Codex bundled model catalog: %w", err)
	}
	template, err := codexCatalogTemplate(source.Models)
	if err != nil {
		return nil, err
	}

	models := make([]map[string]any, 0, len(ids))
	for i, id := range ids {
		entry, err := codexCatalogEntry(template, id, i+1)
		if err != nil {
			return nil, err
		}
		models = append(models, entry)
	}
	return json.Marshal(map[string]any{"models": models})
}

func codexCatalogTemplate(models []map[string]any) (map[string]any, error) {
	var nonLite map[string]any
	for _, model := range models {
		if slug, _ := model["slug"].(string); slug == codexCatalogTemplateSlug {
			return model, nil
		}
		lite, _ := model["use_responses_lite"].(bool)
		if !lite && nonLite == nil {
			nonLite = model
		}
	}
	if nonLite != nil {
		return nonLite, nil
	}
	if len(models) > 0 {
		return models[0], nil
	}
	return nil, fmt.Errorf("Codex bundled model catalog is empty")
}

func codexCatalogEntry(template map[string]any, id string, priority int) (map[string]any, error) {
	raw, err := json.Marshal(template)
	if err != nil {
		return nil, err
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, err
	}

	entry["slug"] = id
	entry["display_name"] = codexDisplayName(id)
	entry["description"] = "DeepSeek model served through dspool"
	entry["default_reasoning_level"] = "high"
	entry["supported_reasoning_levels"] = codexReasoningLevels
	entry["visibility"] = "list"
	entry["supported_in_api"] = true
	entry["priority"] = priority
	entry["context_window"] = modelContextWindow
	entry["max_context_window"] = modelContextWindow
	entry["effective_context_window_percent"] = 95
	// Codex would otherwise send mid-conversation changes as configuration_update
	// items, which dspool (Chat Completions upstream) cannot represent. With
	// this false, Codex updates the request-level reasoning.effort instead.
	entry["supports_reasoning_effort_updates"] = false
	entry["use_responses_lite"] = false
	entry["input_modalities"] = []any{"text"}
	entry["supports_search_tool"] = false
	entry["web_search_tool_type"] = "text"
	entry["supports_image_detail_original"] = false
	entry["default_reasoning_summary"] = "none"
	entry["support_verbosity"] = false
	entry["include_skills_usage_instructions"] = false
	entry["include_plugin_usage_instructions"] = false
	entry["include_apps_usage_instructions"] = false
	entry["experimental_supported_tools"] = []any{}

	// OpenAI-specific tier and migration metadata does not apply to DeepSeek.
	for _, key := range []string{
		"additional_speed_tiers", "availability_nux", "comp_hash",
		"default_verbosity", "multi_agent_reasoning_effort",
		"multi_agent_version", "node_repl_auto_review_required",
		"node_repl_disabled", "service_tiers", "tool_mode", "upgrade",
	} {
		delete(entry, key)
	}

	instructions, _ := entry["base_instructions"].(string)
	if instructions == "" {
		if messages, ok := entry["model_messages"].(map[string]any); ok {
			instructions, _ = messages["instructions_template"].(string)
		}
	}
	if instructions == "" {
		return nil, fmt.Errorf("Codex template model %q has no instructions", codexCatalogTemplateSlug)
	}
	instructions = codexIdentityPrefix.ReplaceAllString(instructions,
		"You are Codex, an AI coding agent running in the Codex CLI.")
	entry["base_instructions"] = instructions
	if messages, ok := entry["model_messages"].(map[string]any); ok {
		messages["instructions_template"] = instructions
	}
	return entry, nil
}

// codexIdentityPrefix drops the OpenAI model identity from the cloned prompt.
var codexIdentityPrefix = regexp.MustCompile(`^You are Codex, (?:a coding agent|an agent) based on GPT-[^.]+\.`)

func codexDisplayName(id string) string {
	switch id {
	case "deepseek-flash":
		return "DeepSeek Flash"
	case "deepseek-v4-pro":
		return "DeepSeek V4 Pro"
	}
	return id
}
