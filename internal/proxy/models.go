package proxy

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"dspool/internal/config"
)

type modelPattern struct {
	pattern string
	target  string
}

// ModelMapper rewrites client model names (Claude Code, Codex, pi, ...) to the
// DeepSeek models that actually exist today.
type ModelMapper struct {
	def      string
	small    string
	exact    map[string]string
	patterns []modelPattern
}

func NewModelMapper(cfg config.Models) (*ModelMapper, error) {
	m := &ModelMapper{
		def:   cfg.Default,
		small: cfg.Small,
		exact: map[string]string{},
	}
	if m.small == "" {
		m.small = m.def
	}
	for key, target := range cfg.Map {
		if strings.ContainsAny(key, "*?[") {
			if _, err := path.Match(key, "probe"); err != nil {
				return nil, fmt.Errorf("models.map pattern %q: %w", key, err)
			}
			m.patterns = append(m.patterns, modelPattern{pattern: key, target: target})
		} else {
			m.exact[key] = target
		}
	}
	// Most specific patterns first. Exact matches are handled separately.
	sort.SliceStable(m.patterns, func(i, j int) bool {
		if len(m.patterns[i].pattern) != len(m.patterns[j].pattern) {
			return len(m.patterns[i].pattern) > len(m.patterns[j].pattern)
		}
		return m.patterns[i].pattern < m.patterns[j].pattern
	})
	return m, nil
}

// Map returns the upstream model name for a client-supplied model.
func (m *ModelMapper) Map(requested string) string {
	requested = stripContextSuffix(strings.TrimSpace(requested))
	if requested == "" {
		return m.def
	}
	if strings.HasPrefix(strings.ToLower(requested), "deepseek") {
		return requested
	}
	if target, ok := m.exact[requested]; ok {
		return target
	}
	for _, p := range m.patterns {
		if ok, _ := path.Match(p.pattern, requested); ok {
			return p.target
		}
	}
	if strings.Contains(strings.ToLower(requested), "haiku") {
		return m.small
	}
	return m.def
}

// KnownModels lists the upstream model IDs advertised by /v1/models.
func (m *ModelMapper) KnownModels() []string {
	set := map[string]struct{}{
		"deepseek-flash":  {},
		"deepseek-v4-pro": {},
		m.def:             {},
		m.small:           {},
	}
	for _, target := range m.exact {
		set[target] = struct{}{}
	}
	for _, p := range m.patterns {
		set[p.target] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		if strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// stripContextSuffix removes client-side suffixes such as "deepseek-flash[1m]".
func stripContextSuffix(s string) string {
	if !strings.HasSuffix(s, "]") {
		return s
	}
	i := strings.LastIndex(s, "[")
	if i <= 0 {
		return s
	}
	inner := s[i+1 : len(s)-1]
	if inner == "" {
		return s
	}
	for _, r := range inner {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return s
		}
	}
	return strings.TrimSpace(s[:i])
}
