package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dspool/internal/config"
)

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.Models.Default = "deepseek-flash"
	cfg.Models.Small = "deepseek-flash"
	cfg.ClientKeys = []string{"local-token"}
	return &cfg
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSetAndRemoveEnv(t *testing.T) {
	env := []string{"A=1", "B=2"}
	env = setEnv(env, "B", "3", true)
	env = setEnv(env, "C", "4", false)
	env = setEnv(env, "C", "5", false)
	if got := envValue(env, "B"); got != "3" {
		t.Fatalf("B = %q", got)
	}
	if got := envValue(env, "C"); got != "4" {
		t.Fatalf("C = %q", got)
	}
	env = removeEnv(env, "A")
	if got := envValue(env, "A"); got != "" {
		t.Fatalf("A should have been removed, got %q", got)
	}
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if ok && k == key {
			return v
		}
	}
	return ""
}

func TestBaseURLs(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8787": "http://127.0.0.1:8787",
		"0.0.0.0:8787":   "http://127.0.0.1:8787",
		"[::]:8787":      "http://[::1]:8787",
		"localhost:9000": "http://localhost:9000",
	}
	for listen, want := range cases {
		got, err := baseURLFromListen(listen)
		if err != nil {
			t.Fatalf("baseURLFromListen(%q): %v", listen, err)
		}
		if got != want {
			t.Errorf("baseURLFromListen(%q) = %q, want %q", listen, got, want)
		}
	}
	if _, err := baseURLFromListen("127.0.0.1:0"); !errors.Is(err, errAutoPort) {
		t.Fatalf("port 0 err = %v", err)
	}
	got, err := baseURLFromAddr("0.0.0.0:1234")
	if err != nil || got != "http://127.0.0.1:1234" {
		t.Fatalf("baseURLFromAddr = %q, %v", got, err)
	}
}

func TestAddNoProxy(t *testing.T) {
	env := []string{"NO_PROXY=corp.example"}
	env = addNoProxy(env, "127.0.0.1", "localhost")
	if got := envValue(env, "NO_PROXY"); got != "corp.example,127.0.0.1,localhost" {
		t.Fatalf("NO_PROXY = %q", got)
	}
	if got := envValue(env, "no_proxy"); got != "corp.example,127.0.0.1,localhost" {
		t.Fatalf("no_proxy = %q", got)
	}
	env = []string{"NO_PROXY=*"}
	env = addNoProxy(env, "127.0.0.1")
	if got := envValue(env, "NO_PROXY"); got != "*" {
		t.Fatalf("wildcard NO_PROXY changed: %q", got)
	}
	env = []string{"NO_PROXY=::1,corp.example"}
	env = addNoProxy(env, "127.0.0.1")
	if got := envValue(env, "NO_PROXY"); got != "corp.example,127.0.0.1" {
		t.Fatalf("IPv6 literal not filtered: %q", got)
	}
}

func TestURLIsLoopback(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://127.0.0.1:8787":   true,
		"http://localhost:8787":   true,
		"http://[::1]:8787":       true,
		"http://192.168.1.5:8787": false,
		"http://evil.example":     false,
	} {
		if got := urlIsLoopback(raw); got != want {
			t.Errorf("urlIsLoopback(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestProxyRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Dspool-Request-Id", "req")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if !proxyRunning(srv.URL) {
		t.Fatal("expected proxyRunning for dspool health response")
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	if proxyRunning(other.URL) {
		t.Fatal("a random HTTP service must not be treated as dspool")
	}
}

func TestClaudeSpec(t *testing.T) {
	cfg := testConfig()
	spec := claudeSpec(cfg, "http://127.0.0.1:8787", "local-token", []string{"--resume"})
	if spec.binary != "claude" {
		t.Fatalf("binary = %q", spec.binary)
	}
	if len(spec.args) != 1 || spec.args[0] != "--resume" {
		t.Fatalf("args = %v", spec.args)
	}
	settings := map[string]envSetting{}
	for _, s := range spec.env {
		settings[s.key] = s
	}
	if got := settings["ANTHROPIC_BASE_URL"].value; got != "http://127.0.0.1:8787" {
		t.Fatalf("base url = %q", got)
	}
	if got := settings["ANTHROPIC_AUTH_TOKEN"].value; got != "local-token" {
		t.Fatalf("token = %q", got)
	}
	if got := settings["ANTHROPIC_MODEL"].value; got != "deepseek-flash[1m]" {
		t.Fatalf("model = %q", got)
	}
	if got := settings["ANTHROPIC_DEFAULT_HAIKU_MODEL"].value; got != "deepseek-flash" {
		t.Fatalf("haiku = %q", got)
	}
	if settings["ANTHROPIC_MODEL"].overwrite {
		t.Fatal("model env must not overwrite a user value")
	}
	if !settings["ANTHROPIC_BASE_URL"].overwrite {
		t.Fatal("base url must override")
	}
	foundUnset := false
	for _, key := range spec.unset {
		if key == "CLAUDE_CODE_OAUTH_TOKEN" {
			foundUnset = true
		}
	}
	if !foundUnset {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN should be unset for the shortcut")
	}
	if got := withContextSuffix("deepseek-v4-pro[1m]"); got != "deepseek-v4-pro[1m]" {
		t.Fatalf("double suffix: %q", got)
	}
}

func TestCodexSpec(t *testing.T) {
	cfg := testConfig()
	spec := codexSpec(cfg, "http://127.0.0.1:8787", "local-token", []string{"exec", "fix tests"})
	if spec.binary != "codex" || spec.model != "deepseek-flash" {
		t.Fatalf("spec = %+v", spec)
	}
	joined := strings.Join(spec.args, "\n")
	for _, want := range []string{
		`model="deepseek-flash"`,
		`model_provider="dspool"`,
		`model_providers.dspool.base_url="http://127.0.0.1:8787/v1"`,
		`model_providers.dspool.env_key="DSPOOL_API_KEY"`,
		`model_providers.dspool.wire_api="responses"`,
		`features.respect_system_proxy=true`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if spec.args[len(spec.args)-2] != "exec" || spec.args[len(spec.args)-1] != "fix tests" {
		t.Fatalf("pass-through args = %v", spec.args)
	}
	if len(spec.env) != 1 || spec.env[0].key != "DSPOOL_API_KEY" || spec.env[0].value != "local-token" {
		t.Fatalf("env = %+v", spec.env)
	}
}

func TestHasFlag(t *testing.T) {
	if !hasFlag([]string{"-p", "--model", "x"}, "--model") {
		t.Fatal("missing --model")
	}
	if !hasFlag([]string{"--provider=dspool"}, "--provider") {
		t.Fatal("missing --provider=")
	}
	if hasFlag([]string{"--", "--model", "x"}, "--model") {
		t.Fatal("flags after -- must be ignored")
	}
}

func TestPiSpecUpsertsProviderAndInjectsFlags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{
		"$schema": "https://pi.dev/schemas/models.schema.json",
		"providers": {
			"other": {
				"baseUrl": "https://example.com/v1",
				"api": "openai-completions",
				"models": [{"id": "other-model"}]
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	spec, err := piSpec(cfg, "http://127.0.0.1:8787", "local-token", []string{"-p", "hello"}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{"--provider", "dspool"},
		{"--model", "deepseek-flash"},
		{"--api-key", "local-token"},
	} {
		found := false
		for i := 0; i+1 < len(spec.args); i++ {
			if spec.args[i] == pair[0] && spec.args[i+1] == pair[1] {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %v in %v", pair, spec.args)
		}
	}
	if spec.args[len(spec.args)-2] != "-p" || spec.args[len(spec.args)-1] != "hello" {
		t.Fatalf("pass-through args = %v", spec.args)
	}

	var root struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var other struct {
		BaseURL string `json:"baseUrl"`
	}
	if err := json.Unmarshal(root.Providers["other"], &other); err != nil {
		t.Fatal(err)
	}
	if other.BaseURL != "https://example.com/v1" {
		t.Fatalf("other provider was changed: %+v", other)
	}
	var provider struct {
		BaseURL    string         `json:"baseUrl"`
		API        string         `json:"api"`
		APIKey     string         `json:"apiKey"`
		AuthHeader bool           `json:"authHeader"`
		Compat     map[string]any `json:"compat"`
		Models     []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(root.Providers["dspool"], &provider); err != nil {
		t.Fatal(err)
	}
	if provider.BaseURL != "http://127.0.0.1:8787/v1" || provider.API != "openai-completions" || provider.APIKey != "$DSPOOL_API_KEY" || !provider.AuthHeader {
		t.Fatalf("provider = %+v", provider)
	}
	if provider.Compat["thinkingFormat"] != "deepseek" {
		t.Fatalf("compat = %+v", provider.Compat)
	}
	ids := map[string]bool{}
	for _, m := range provider.Models {
		ids[m.ID] = true
	}
	if !ids["deepseek-flash"] || !ids["deepseek-v4-pro"] {
		t.Fatalf("models = %+v", provider.Models)
	}

	// A second invocation with the same configuration should be a no-op.
	before, _ := os.ReadFile(path)
	if _, err := piSpec(cfg, "http://127.0.0.1:8787", "local-token", []string{"-p", "hello"}, discardLog()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("models.json was rewritten even though nothing changed")
	}
}

func TestPiSpecRespectsUserFlags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	cfg := testConfig()

	spec, err := piSpec(cfg, "http://127.0.0.1:8787", "local-token", []string{"--model", "deepseek-v4-pro", "-p", "hi"}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if countToken(spec.args, "--provider") != 1 {
		t.Fatalf("provider should be injected once: %v", spec.args)
	}
	if countToken(spec.args, "--model") != 1 {
		t.Fatalf("user --model should be preserved without another: %v", spec.args)
	}

	spec, err = piSpec(cfg, "http://127.0.0.1:8787", "local-token", []string{"--provider", "other", "--model", "x", "--api-key", "mine"}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if countToken(spec.args, "--provider") != 1 || countToken(spec.args, "--model") != 1 || countToken(spec.args, "--api-key") != 1 {
		t.Fatalf("user flags should be preserved: %v", spec.args)
	}
}

func countToken(args []string, token string) int {
	n := 0
	for _, arg := range args {
		if arg == token {
			n++
		}
	}
	return n
}

func TestPiMetaDoesNotTouchConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	cfg := testConfig()
	for _, args := range [][]string{{"--help"}, {"install", "some-extension"}, {"update"}} {
		spec, err := piSpec(cfg, "http://127.0.0.1:8787", "local-token", args, discardLog())
		if err != nil {
			t.Fatal(err)
		}
		if len(spec.args) != len(args) {
			t.Fatalf("args changed: %v", spec.args)
		}
		if _, err := os.Stat(filepath.Join(dir, "models.json")); !os.IsNotExist(err) {
			t.Fatalf("%v should not create models.json", args)
		}
	}
}

func TestPiProviderRejectsJSONComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	const original = "// comment\n{\"providers\": {}}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensurePiProvider(path, "http://127.0.0.1:8787", "deepseek-flash", "deepseek-flash", discardLog()); err == nil {
		t.Fatal("expected an error for JSON comments")
	}
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Fatal("the file was overwritten on parse failure")
	}
}

func TestEnsurePiProviderCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "models.json")
	provider, model, err := ensurePiProvider(path, "http://127.0.0.1:8787/", "deepseek-v4-pro", "deepseek-flash", discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if provider != "dspool" || model != "deepseek-v4-pro" {
		t.Fatalf("provider/model = %q/%q", provider, model)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("models.json mode = %o, want private", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "https://pi.dev/schemas/models.schema.json") {
		t.Fatalf("schema missing: %s", data)
	}
}
