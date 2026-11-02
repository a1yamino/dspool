package main

import (
	"encoding/json"
	"strings"
	"testing"

	"dspool/internal/config"
)

const testBundledCatalog = `{
  "models": [
    {
      "slug": "gpt-6-astra",
      "display_name": "GPT-6-Astra",
      "default_reasoning_level": "low",
      "supported_reasoning_levels": [{"effort": "low", "description": "low"}],
      "shell_type": "unified_exec",
      "visibility": "list",
      "supported_in_api": true,
      "priority": 2,
      "additional_speed_tiers": ["fast"],
      "service_tiers": [{"id": "priority"}],
      "context_window": 272000,
      "max_context_window": 872000,
      "effective_context_window_percent": 95,
      "use_responses_lite": true,
      "tool_mode": "code_mode_only",
      "multi_agent_version": "v2",
      "multi_agent_reasoning_effort": "xhigh",
      "node_repl_auto_review_required": true,
      "input_modalities": ["text", "image"],
      "supports_reasoning_effort_updates": true,
      "base_instructions": "You are Codex, an agent based on GPT-6. Astra marker.",
      "model_messages": {"instructions_template": "You are Codex, an agent based on GPT-6. Astra marker."}
    },
    {
      "slug": "gpt-6.1-sol",
      "use_responses_lite": true,
      "base_instructions": "lite model",
      "model_messages": {"instructions_template": "lite model"}
    },
    {
      "slug": "gpt-5.5",
      "display_name": "GPT-5.5",
      "default_reasoning_level": "medium",
      "supported_reasoning_levels": [{"effort": "low", "description": "low"}],
      "shell_type": "unified_exec",
      "visibility": "list",
      "supported_in_api": true,
      "priority": 13,
      "additional_speed_tiers": ["fast"],
      "service_tiers": [{"id": "priority"}],
      "upgrade": {"model": "gpt-6-sol"},
      "context_window": 272000,
      "max_context_window": 272000,
      "effective_context_window_percent": 95,
      "use_responses_lite": false,
      "input_modalities": ["text", "image"],
      "supports_reasoning_effort_updates": false,
      "base_instructions": "You are Codex, a coding agent based on GPT-5. Be useful.",
      "model_messages": {"instructions_template": "You are Codex, a coding agent based on GPT-5. Be useful."}
    }
  ]
}`

func TestBuildCodexModelCatalog(t *testing.T) {
	data, err := buildCodexModelCatalog([]byte(testBundledCatalog), []string{"deepseek-flash", "deepseek-v4-pro"})
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(catalog.Models))
	}
	for i, want := range []struct{ slug, name string }{
		{"deepseek-flash", "DeepSeek Flash"},
		{"deepseek-v4-pro", "DeepSeek V4 Pro"},
	} {
		model := catalog.Models[i]
		if model["slug"] != want.slug || model["display_name"] != want.name {
			t.Fatalf("model %d = %v / %v", i, model["slug"], model["display_name"])
		}
		if got := model["context_window"].(float64); got != modelContextWindow {
			t.Fatalf("%s context window = %v", want.slug, got)
		}
		if model["max_context_window"].(float64) != modelContextWindow {
			t.Fatalf("%s max context window = %v", want.slug, model["max_context_window"])
		}
		if model["use_responses_lite"] != false {
			t.Fatalf("%s should not use responses lite", want.slug)
		}
		if model["supports_reasoning_effort_updates"] != false {
			t.Fatalf("%s should not use configuration_update effort changes", want.slug)
		}
		levels, _ := model["supported_reasoning_levels"].([]any)
		if len(levels) != 3 {
			t.Fatalf("%s reasoning levels = %v", want.slug, levels)
		}
		for i, effort := range []string{"low", "high", "max"} {
			level, _ := levels[i].(map[string]any)
			if level["effort"] != effort {
				t.Fatalf("%s level %d = %v, want %s", want.slug, i, level["effort"], effort)
			}
		}
		if model["service_tiers"] != nil || model["upgrade"] != nil || model["additional_speed_tiers"] != nil {
			t.Fatalf("%s kept OpenAI tier metadata", want.slug)
		}
		if model["multi_agent_version"] != nil || model["tool_mode"] != nil {
			t.Fatalf("%s kept OpenAI agent/code-mode metadata", want.slug)
		}
		instructions, _ := model["base_instructions"].(string)
		if strings.Contains(instructions, "based on GPT") || !strings.Contains(instructions, "Codex CLI") {
			t.Fatalf("%s instructions were not rewritten: %q", want.slug, instructions)
		}
		if !strings.Contains(instructions, "Astra marker") {
			t.Fatalf("%s did not use the Astra template: %q", want.slug, instructions)
		}
		messages, _ := model["model_messages"].(map[string]any)
		if messages["instructions_template"] != instructions {
			t.Fatalf("%s template does not match base instructions", want.slug)
		}
	}
}

func TestBuildCodexModelCatalogFallsBackToNonLiteModel(t *testing.T) {
	fixture := `{"models":[
	  {"slug":"lite","use_responses_lite":true,"base_instructions":"lite"},
	  {"slug":"other","use_responses_lite":false,"base_instructions":"You are Codex, a coding agent based on GPT-5."}
	]}`
	if _, err := buildCodexModelCatalog([]byte(fixture), []string{"deepseek-flash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := buildCodexModelCatalog([]byte(`{"models":[]}`), []string{"deepseek-flash"}); err == nil {
		t.Fatal("expected an error for an empty bundled catalog")
	}
}

func TestCodexCatalogModelIDs(t *testing.T) {
	cfg := config.Default()
	cfg.Models.Map = map[string]string{"claude-opus*": "deepseek-v4-pro"}
	ids := codexCatalogModelIDs(&cfg)
	want := []string{"deepseek-flash", "deepseek-v4-pro"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}
