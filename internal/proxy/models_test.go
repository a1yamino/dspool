package proxy

import (
	"testing"

	"dspool/internal/config"
)

func TestModelMapper(t *testing.T) {
	m, err := NewModelMapper(config.Models{
		Default: "deepseek-flash",
		Small:   "deepseek-flash-small",
		Map: map[string]string{
			"claude-opus*": "deepseek-v4-pro",
			"gpt-5*":       "deepseek-v4-pro",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"":                         "deepseek-flash",
		"claude-sonnet-4-5":        "deepseek-flash",
		"claude-opus-4-1-20250805": "deepseek-v4-pro",
		"claude-3-5-haiku":         "deepseek-flash-small",
		"gpt-5-codex":              "deepseek-v4-pro",
		"deepseek-flash[1m]":       "deepseek-flash",
		"deepseek-v4-pro":          "deepseek-v4-pro",
	}
	for input, want := range cases {
		if got := m.Map(input); got != want {
			t.Errorf("Map(%q) = %q, want %q", input, got, want)
		}
	}
	if got := m.KnownModels(); len(got) == 0 {
		t.Fatal("KnownModels is empty")
	}
}

func TestStripContextSuffix(t *testing.T) {
	cases := map[string]string{
		"deepseek-flash[1m]": "deepseek-flash",
		"model[1m]":          "model",
		"model[not-marker]":  "model[not-marker]",
		"model]":             "model]",
	}
	for input, want := range cases {
		if got := stripContextSuffix(input); got != want {
			t.Errorf("stripContextSuffix(%q) = %q, want %q", input, got, want)
		}
	}
}
