// Package config loads and validates dspool's YAML configuration.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dspool/internal/keyutil"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that understands YAML strings like "30s".
// Bare integers are interpreted as seconds.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string like \"30s\" or a number of seconds")
	}
	if value.Tag == "!!int" || value.Tag == "!!float" {
		var seconds float64
		if err := value.Decode(&seconds); err != nil {
			return err
		}
		*d = Duration(time.Duration(seconds * float64(time.Second)))
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// KeySpec is one upstream DeepSeek key. In YAML it may be written either as a
// plain string or as {label: ..., key: ...}.
type KeySpec struct {
	Label string `yaml:"label"`
	Key   string `yaml:"key"`
}

func (k *KeySpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&k.Key)
	}
	type plain struct {
		Label string `yaml:"label"`
		Key   string `yaml:"key"`
	}
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	k.Label, k.Key = p.Label, p.Key
	return nil
}

type Upstream struct {
	AnthropicBase        string   `yaml:"anthropic_base"`
	OpenAIBase           string   `yaml:"openai_base"`
	BalanceURL           string   `yaml:"balance_url"`
	RequestHeaderTimeout Duration `yaml:"request_header_timeout"`
	ProbeTimeout         Duration `yaml:"probe_timeout"`
	MaxTokensCap         int      `yaml:"max_tokens_cap"`
	AnthropicVersion     string   `yaml:"anthropic_version"`
}

type Pool struct {
	// Strategy is weighted, spread, highest, drain or round_robin.
	Strategy string `yaml:"strategy"`
	// MaxAttempts is the total number of upstream attempts per client request.
	MaxAttempts int `yaml:"max_attempts"`
	// MaxInflightPerKey caps concurrent requests per upstream key (0 = no cap).
	MaxInflightPerKey int `yaml:"max_inflight_per_key"`
	// InflightPenalty damps the weight of a key as its in-flight count grows.
	InflightPenalty float64 `yaml:"inflight_penalty"`
	// MinBalance keeps keys below this balance in the cold pool.
	MinBalance float64 `yaml:"min_balance"`
	// BalanceInterval controls how often hot keys are re-checked.
	BalanceInterval Duration `yaml:"balance_interval"`
	// ColdCheckInterval controls how often the cold pool is checked for top-ups.
	ColdCheckInterval Duration `yaml:"cold_check_interval"`
	// StartupProbeTimeout bounds the blocking probe done before serving.
	StartupProbeTimeout Duration `yaml:"startup_probe_timeout"`
	// RateLimitCooldown is the minimum cooldown after a 429 (Retry-After wins).
	RateLimitCooldown Duration `yaml:"rate_limit_cooldown"`
	// ErrorCooldown is the cooldown after network errors and 5xx responses.
	ErrorCooldown Duration `yaml:"error_cooldown"`
	// RefreshColdOnExhaustion synchronously re-checks cold keys when no hot key
	// is available, so a fresh top-up is picked up immediately.
	RefreshColdOnExhaustion bool `yaml:"refresh_cold_on_exhaustion"`
	// ExhaustionRefreshCooldown rate-limits that synchronous refresh.
	ExhaustionRefreshCooldown Duration `yaml:"exhaustion_refresh_cooldown"`
}

type Models struct {
	// Default is used for any model name that is not a DeepSeek model and does
	// not match a pattern in Map.
	Default string `yaml:"default"`
	// Small is used for Claude "haiku" style background requests.
	Small string `yaml:"small"`
	// Map supports exact names and glob patterns (for example "claude-opus*").
	Map map[string]string `yaml:"map"`
}

type Responses struct {
	// Thinking is auto, enabled or disabled. auto follows OpenAI's reasoning
	// effort field (effort=none disables thinking).
	Thinking string `yaml:"thinking"`
}

type Config struct {
	Listen       string   `yaml:"listen"`
	StateFile    string   `yaml:"state_file"`
	LogLevel     string   `yaml:"log_level"`
	LogFile      string   `yaml:"log_file"`
	ClientKeys   []string `yaml:"client_keys"`
	AdminToken   string   `yaml:"admin_token"`
	MaxBodyBytes int64    `yaml:"max_body_bytes"`

	Upstream  Upstream  `yaml:"upstream"`
	Pool      Pool      `yaml:"pool"`
	Models    Models    `yaml:"models"`
	Responses Responses `yaml:"responses"`

	KeysFile string    `yaml:"keys_file"`
	Keys     []KeySpec `yaml:"keys"`

	// Derived, not serialized.
	ConfigPath string `yaml:"-"`
	BaseDir    string `yaml:"-"`
}

func Default() Config {
	return Config{
		Listen:       "127.0.0.1:8787",
		StateFile:    "data/state.json",
		LogLevel:     "info",
		MaxBodyBytes: 64 << 20,
		Upstream: Upstream{
			AnthropicBase:        "https://api.deepseek.com/anthropic",
			OpenAIBase:           "https://api.deepseek.com/v1",
			BalanceURL:           "https://api.deepseek.com/user/balance",
			RequestHeaderTimeout: Duration(180 * time.Second),
			ProbeTimeout:         Duration(15 * time.Second),
			AnthropicVersion:     "2023-06-01",
		},
		Pool: Pool{
			// spread uses the whole pool concurrently, which maximises aggregate
			// throughput when the same keys are also being consumed elsewhere.
			Strategy:                  "spread",
			MaxAttempts:               4,
			MaxInflightPerKey:         0,
			InflightPenalty:           1.0,
			MinBalance:                0,
			BalanceInterval:           Duration(30 * time.Second),
			ColdCheckInterval:         Duration(2 * time.Minute),
			StartupProbeTimeout:       Duration(30 * time.Second),
			RateLimitCooldown:         Duration(30 * time.Second),
			ErrorCooldown:             Duration(10 * time.Second),
			RefreshColdOnExhaustion:   true,
			ExhaustionRefreshCooldown: Duration(10 * time.Second),
		},
		Models: Models{
			Default: "deepseek-flash",
			Small:   "deepseek-flash",
			Map:     map[string]string{},
		},
		Responses: Responses{Thinking: "auto"},
	}
}

// Load reads configPath (if it exists), applies environment overrides, validates
// the result and loads API keys from keys_file. When explicit is false and the
// file does not exist, defaults plus environment variables are used.
func Load(configPath string, explicit bool) (*Config, []string, error) {
	cfg := Default()
	var warnings []string

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return nil, nil, fmt.Errorf("parse %s: %w", configPath, err)
			}
			abs, absErr := filepath.Abs(configPath)
			if absErr != nil {
				return nil, nil, absErr
			}
			cfg.ConfigPath = abs
			cfg.BaseDir = filepath.Dir(abs)
		case errors.Is(err, os.ErrNotExist) && !explicit:
			// Local quick start without a config file.
		case errors.Is(err, os.ErrNotExist):
			return nil, nil, fmt.Errorf("config file %s does not exist", configPath)
		default:
			return nil, nil, fmt.Errorf("read %s: %w", configPath, err)
		}
	}

	if cfg.BaseDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, nil, err
		}
		cfg.BaseDir = wd
	}

	applyEnv(&cfg)
	if cfg.StateFile == "" {
		cfg.StateFile = "data/state.json"
	}
	cfg.StateFile = resolvePath(cfg.BaseDir, cfg.StateFile)
	if cfg.LogFile != "" {
		cfg.LogFile = resolvePath(cfg.BaseDir, cfg.LogFile)
	}

	if err := validate(&cfg); err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, securityWarnings(&cfg)...)

	loaded, keyWarnings, err := loadKeys(&cfg)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, keyWarnings...)
	cfg.Keys = loaded

	return &cfg, warnings, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("DSPOOL_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("DSPOOL_STATE_FILE"); v != "" {
		cfg.StateFile = v
	}
	if v := os.Getenv("DSPOOL_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("DSPOOL_LOG_FILE"); v != "" {
		cfg.LogFile = v
	}
	if v := os.Getenv("DSPOOL_ADMIN_TOKEN"); v != "" {
		cfg.AdminToken = v
	}
	if v := os.Getenv("DSPOOL_STRATEGY"); v != "" {
		cfg.Pool.Strategy = v
	}
	if v := os.Getenv("DSPOOL_KEYS_FILE"); v != "" {
		cfg.KeysFile = v
	}
	if v := os.Getenv("DSPOOL_CLIENT_KEYS"); v != "" {
		cfg.ClientKeys = append(cfg.ClientKeys, splitList(v)...)
	}
	if v := os.Getenv("DSPOOL_KEYS"); v != "" {
		for _, k := range splitList(v) {
			cfg.Keys = append(cfg.Keys, KeySpec{Key: k})
		}
	}
}

func splitList(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func resolvePath(base, path string) string {
	if path == "" {
		return path
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

func validate(cfg *Config) error {
	switch cfg.Pool.Strategy {
	case "weighted", "spread", "highest", "drain", "round_robin":
	default:
		return fmt.Errorf("pool.strategy must be weighted, spread, highest, drain or round_robin (got %q)", cfg.Pool.Strategy)
	}
	switch cfg.Responses.Thinking {
	case "", "auto", "enabled", "disabled":
	default:
		return fmt.Errorf("responses.thinking must be auto, enabled or disabled (got %q)", cfg.Responses.Thinking)
	}

	if cfg.Pool.MaxAttempts < 1 {
		return fmt.Errorf("pool.max_attempts must be at least 1")
	}
	if cfg.Pool.MaxAttempts > 32 {
		return fmt.Errorf("pool.max_attempts must be at most 32")
	}
	if cfg.Pool.MaxInflightPerKey < 0 {
		return fmt.Errorf("pool.max_inflight_per_key must be >= 0")
	}
	if cfg.Pool.InflightPenalty < 0 {
		return fmt.Errorf("pool.inflight_penalty must be >= 0")
	}
	if cfg.Pool.MinBalance < 0 {
		return fmt.Errorf("pool.min_balance must be >= 0")
	}

	clamp := func(d *Duration, min time.Duration) {
		if d.D() <= 0 {
			*d = Duration(min)
		}
	}
	clamp(&cfg.Pool.BalanceInterval, 5*time.Second)
	clamp(&cfg.Pool.ColdCheckInterval, 10*time.Second)
	clamp(&cfg.Pool.StartupProbeTimeout, 5*time.Second)
	clamp(&cfg.Pool.RateLimitCooldown, time.Second)
	clamp(&cfg.Pool.ErrorCooldown, time.Second)
	clamp(&cfg.Pool.ExhaustionRefreshCooldown, time.Second)
	clamp(&cfg.Upstream.RequestHeaderTimeout, 5*time.Second)
	clamp(&cfg.Upstream.ProbeTimeout, 2*time.Second)

	if cfg.MaxBodyBytes < 1024 {
		return fmt.Errorf("max_body_bytes must be at least 1024")
	}
	if cfg.Upstream.MaxTokensCap < 0 {
		return fmt.Errorf("upstream.max_tokens_cap must be >= 0")
	}
	if err := validateURL("upstream.anthropic_base", cfg.Upstream.AnthropicBase); err != nil {
		return err
	}
	if err := validateURL("upstream.openai_base", cfg.Upstream.OpenAIBase); err != nil {
		return err
	}
	if err := validateURL("upstream.balance_url", cfg.Upstream.BalanceURL); err != nil {
		return err
	}
	if cfg.Upstream.AnthropicVersion == "" {
		cfg.Upstream.AnthropicVersion = "2023-06-01"
	}
	if cfg.Models.Default == "" {
		return fmt.Errorf("models.default must not be empty")
	}
	if cfg.Models.Small == "" {
		cfg.Models.Small = cfg.Models.Default
	}
	if cfg.Models.Map == nil {
		cfg.Models.Map = map[string]string{}
	}
	for pattern, target := range cfg.Models.Map {
		if strings.TrimSpace(pattern) == "" || strings.TrimSpace(target) == "" {
			return fmt.Errorf("models.map must not contain empty pattern or target")
		}
	}
	if _, err := net.ResolveTCPAddr("tcp", cfg.Listen); err != nil {
		return fmt.Errorf("listen %q: %w", cfg.Listen, err)
	}
	if cfg.Listen == "" {
		return fmt.Errorf("listen must not be empty")
	}

	seenClients := map[string]struct{}{}
	dedupedClients := cfg.ClientKeys[:0]
	for _, k := range cfg.ClientKeys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, ok := seenClients[k]; ok {
			continue
		}
		seenClients[k] = struct{}{}
		dedupedClients = append(dedupedClients, k)
	}
	cfg.ClientKeys = dedupedClients
	return nil
}

func validateURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must be an http(s) URL", name)
	}
	if u.Host == "" {
		return fmt.Errorf("%s must include a host", name)
	}
	return nil
}

func loadKeys(cfg *Config) ([]KeySpec, []string, error) {
	var warnings []string
	keys := make([]KeySpec, 0, len(cfg.Keys))
	keys = append(keys, cfg.Keys...)

	if cfg.KeysFile != "" {
		path := resolvePath(cfg.BaseDir, cfg.KeysFile)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read keys_file %s: %w", path, err)
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			spec := KeySpec{Key: line}
			// Optional "label = key" form.
			if i := strings.Index(line, "="); i > 0 {
				label := strings.TrimSpace(line[:i])
				key := strings.TrimSpace(line[i+1:])
				if label != "" && key != "" {
					spec.Label, spec.Key = label, key
				}
			}
			spec.Key = strings.TrimSpace(spec.Key)
			if spec.Key == "" {
				warnings = append(warnings, fmt.Sprintf("keys_file line %d is empty after parsing, skipped", lineNo+1))
				continue
			}
			keys = append(keys, spec)
		}
	}

	seen := map[string]struct{}{}
	out := make([]KeySpec, 0, len(keys))
	for _, spec := range keys {
		spec.Key = strings.TrimSpace(spec.Key)
		if spec.Key == "" {
			continue
		}
		id := keyutil.Fingerprint(spec.Key)
		if _, ok := seen[id]; ok {
			warnings = append(warnings, fmt.Sprintf("duplicate upstream key %s skipped", keyutil.Mask(spec.Key)))
			continue
		}
		seen[id] = struct{}{}
		spec.Label = strings.TrimSpace(spec.Label)
		if spec.Label == "" {
			spec.Label = "key-" + id[:6]
		}
		out = append(out, spec)
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("no upstream keys configured; add keys to config.yaml, keys_file, or DSPOOL_KEYS")
	}
	return out, warnings, nil
}

func securityWarnings(cfg *Config) []string {
	var out []string
	if len(cfg.ClientKeys) == 0 {
		out = append(out, "client_keys is empty: the proxy accepts any client token (fine for localhost, unsafe on a public interface)")
	}
	if cfg.AdminToken == "" {
		out = append(out, "admin_token is empty: admin endpoints are only available on loopback")
	}
	out = append(out, "the dashboard at / is unauthenticated; keep the listener on loopback or put it behind your own access control")
	if !listenIsLoopback(cfg.Listen) && len(cfg.ClientKeys) == 0 {
		out = append(out, "listening on a non-loopback interface without client_keys configured")
	}
	return out
}

// ListenIsLoopback reports whether addr binds only to the loopback interface.
func ListenIsLoopback(addr string) bool { return listenIsLoopback(addr) }

func listenIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
