package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dspool/internal/config"
)

type envSetting struct {
	key       string
	value     string
	overwrite bool
}

type agentSpec struct {
	binary string
	model  string
	env    []envSetting
	unset  []string
	args   []string
}

func runAgent(cfg *config.Config, logger *slog.Logger, command string, args []string, noStart bool) (int, error) {
	switch command {
	case "claude", "codex", "pi":
	default:
		return 1, fmt.Errorf("unknown agent %q", command)
	}
	binary, err := exec.LookPath(command)
	if err != nil {
		return 1, fmt.Errorf("%s not found in PATH; install it first", command)
	}

	base, err := daemonBase(cfg)
	if err != nil {
		return 1, err
	}

	if noStart {
		if !proxyRunning(base) {
			return 1, fmt.Errorf("no dspool daemon is running at %s; run `dspool start` first or drop -no-start/-no-embed", base)
		}
		logger.Info("using existing dspool daemon", "base_url", base)
	} else {
		var started bool
		base, _, started, err = ensureDaemon(cfg, logger)
		if err != nil {
			return 1, err
		}
		if started {
			logger.Info("started background dspool daemon", "base_url", base)
		} else {
			logger.Info("using existing dspool daemon", "base_url", base)
		}
	}

	token := clientToken(cfg)
	if err := checkDaemonToken(base, token); err != nil {
		return 1, err
	}
	var spec agentSpec
	switch command {
	case "claude":
		spec = claudeSpec(cfg, base, token, args)
	case "codex":
		spec = codexSpec(cfg, base, token, args, binary, logger)
	case "pi":
		var specErr error
		spec, specErr = piSpec(cfg, base, token, args, logger)
		if specErr != nil {
			return 1, specErr
		}
	}

	cmd := exec.Command(binary, spec.args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := os.Environ()
	for _, key := range spec.unset {
		env = removeEnv(env, key)
	}
	for _, setting := range spec.env {
		env = setEnv(env, setting.key, setting.value, setting.overwrite)
	}
	// Many coding agents honour HTTP(S)_PROXY but do not automatically bypass
	// loopback. Without this, requests to the local dspool can be sent to the
	// user's forward proxy and fail with a 502.
	// Keep this list free of bare IPv6 literals: some Node-based agents
	// (e.g. Claude Code) mis-parse NO_PROXY when "::1" is present. Add the
	// IPv6 loopback only when the proxy itself is addressed that way.
	noProxyHosts := []string{"127.0.0.1", "localhost"}
	if parsed, parseErr := url.Parse(base); parseErr == nil && parsed.Hostname() != "" {
		noProxyHosts = append(noProxyHosts, parsed.Hostname())
	}
	env = addNoProxy(env, noProxyHosts...)
	cmd.Env = env

	logger.Info("starting agent",
		"agent", command,
		"binary", binary,
		"base_url", base,
		"model", spec.model,
		"proxy", "daemon",
	)

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("start %s: %w", spec.binary, err)
	}

	// In an interactive terminal Ctrl-C goes to the whole foreground process
	// group, so the child already receives it. Forward signals that Target only
	// this process (and non-interactive SIGINT) explicitly.
	interactive := stdinIsTerminal()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	sigDone := make(chan struct{})
	go func() {
		defer close(sigDone)
		for sig := range sigCh {
			if cmd.Process == nil {
				continue
			}
			if sig == os.Interrupt && interactive {
				continue
			}
			_ = cmd.Process.Signal(sig)
		}
	}()

	waitErr := cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)
	<-sigDone

	code := commandExitCode(waitErr)
	logger.Info("agent exited",
		"agent", command,
		"code", code,
		"duration", time.Since(started).Round(time.Second).String(),
	)
	return code, nil
}

func claudeSpec(cfg *config.Config, base, token string, args []string) agentSpec {
	mainModel := strings.TrimSpace(cfg.Models.Default)
	if mainModel == "" {
		mainModel = "deepseek-flash"
	}
	mainWithWindow := withContextSuffix(mainModel)
	small := strings.TrimSpace(cfg.Models.Small)
	if small == "" {
		small = "deepseek-flash"
	}

	return agentSpec{
		binary: "claude",
		model:  mainWithWindow,
		args:   args,
		unset: []string{
			"CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
			"CLAUDE_CODE_USE_BEDROCK",
			"CLAUDE_CODE_USE_VERTEX",
			"CLAUDE_CODE_USE_FOUNDRY",
		},
		env: []envSetting{
			{key: "ANTHROPIC_BASE_URL", value: base, overwrite: true},
			{key: "ANTHROPIC_AUTH_TOKEN", value: token, overwrite: true},
			{key: "ANTHROPIC_API_KEY", value: token, overwrite: true},
			{key: "ANTHROPIC_MODEL", value: mainWithWindow},
			{key: "ANTHROPIC_DEFAULT_OPUS_MODEL", value: mainWithWindow},
			{key: "ANTHROPIC_DEFAULT_SONNET_MODEL", value: mainWithWindow},
			{key: "ANTHROPIC_DEFAULT_HAIKU_MODEL", value: small},
			{key: "ANTHROPIC_SMALL_FAST_MODEL", value: small},
			{key: "CLAUDE_CODE_SUBAGENT_MODEL", value: small},
		},
	}
}

// modelContextWindow is the context window advertised to clients that need a
// number instead of a model-name marker (Codex, pi). DeepSeek models accept
// 1M tokens.
const modelContextWindow = 1000000

func withContextSuffix(model string) string {
	if model == "" || strings.ContainsAny(model, "[]") {
		return model
	}
	return model + "[1m]"
}

func codexSpec(cfg *config.Config, base, token string, args []string, codexBin string, logger *slog.Logger) agentSpec {
	model := strings.TrimSpace(cfg.Models.Default)
	if model == "" {
		model = "deepseek-flash"
	}
	baseV1 := strings.TrimRight(base, "/") + "/v1"
	quoted := func(v string) string { return strconv.Quote(v) }

	configArgs := []string{
		"-c", "model=" + quoted(model),
		"-c", "model_provider=" + quoted("dspool"),
		"-c", "model_providers.dspool.name=" + quoted("dspool"),
		"-c", "model_providers.dspool.base_url=" + quoted(baseV1),
		"-c", "model_providers.dspool.env_key=" + quoted("DSPOOL_API_KEY"),
		"-c", "model_providers.dspool.wire_api=" + quoted("responses"),
		// Codex has no metadata for DeepSeek model IDs and falls back to a
		// 272k window. Without this, compaction kicks in far too early.
		"-c", "model_context_window=" + strconv.Itoa(modelContextWindow),
	}
	// A catalog entry is what gives the TUI its /model list and reasoning
	// picker; an unknown model gets neither.
	if codexBin != "" {
		if catalogPath, err := ensureCodexModelCatalog(codexBin, cfg, logger); err != nil {
			logger.Warn("could not build Codex model catalog; /model will not list DeepSeek models", "error", err)
		} else {
			configArgs = append(configArgs, "-c", "model_catalog_json="+quoted(catalogPath))
		}
	}
	configArgs = append(configArgs,
		// Resolve the system proxy per destination (like native Codex) so a
		// loopback dspool URL is not sent through the user's forward proxy.
		"-c", "features.respect_system_proxy=true",
	)
	return agentSpec{
		binary: "codex",
		model:  model,
		env: []envSetting{
			{key: "DSPOOL_API_KEY", value: token, overwrite: true},
		},
		args: append(configArgs, args...),
	}
}

func piSpec(cfg *config.Config, base, token string, args []string, logger *slog.Logger) (agentSpec, error) {
	if piIsMetaOrSubcommand(args) {
		return agentSpec{
			binary: "pi",
			model:  "as requested",
			env: []envSetting{
				{key: "DSPOOL_API_KEY", value: token, overwrite: true},
				{key: "DSPOOL_BASE_URL", value: base, overwrite: true},
			},
			args: args,
		}, nil
	}
	modelsPath, err := piModelsPath()
	if err != nil {
		return agentSpec{}, err
	}
	provider, model, err := ensurePiProvider(modelsPath, base, cfg.Models.Default, cfg.Models.Small, logger)
	if err != nil {
		return agentSpec{}, err
	}
	if _, err := ensurePiSearchExtension(logger); err != nil {
		logger.Warn("could not install pi search extension", "error", err)
	}

	injected := make([]string, 0, 8)
	if !hasFlag(args, "--provider") {
		injected = append(injected, "--provider", provider)
	}
	if !hasFlag(args, "--model") {
		injected = append(injected, "--model", model)
	}
	if !hasFlag(args, "--api-key") {
		injected = append(injected, "--api-key", token)
	}

	return agentSpec{
		binary: "pi",
		model:  model,
		env: []envSetting{
			{key: "DSPOOL_API_KEY", value: token, overwrite: true},
			{key: "DSPOOL_BASE_URL", value: base, overwrite: true},
		},
		args: append(injected, args...),
	}, nil
}

func piIsMetaOrSubcommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "install", "remove", "uninstall", "update", "list", "config", "auth", "mcp",
		"--help", "-h", "help", "--version", "-v":
		return true
	}
	return false
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func clientToken(cfg *config.Config) string {
	for _, key := range cfg.ClientKeys {
		if key = strings.TrimSpace(key); key != "" {
			return key
		}
	}
	return "dspool-local"
}

var directProbeClient = &http.Client{
	Timeout:   900 * time.Millisecond,
	Transport: &http.Transport{Proxy: nil},
}

func proxyRunning(base string) bool {
	resp, err := directProbeClient.Get(strings.TrimRight(base, "/") + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.Header.Get("X-Dspool-Request-Id") == "" {
		return false
	}
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable
}

func urlIsLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func addNoProxy(env []string, hosts ...string) []string {
	existing := []string{}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		if value := envLookup(env, key); value != "" {
			existing = append(existing, value)
		}
	}
	joined := strings.Join(existing, ",")
	if strings.Contains(joined, "*") {
		return env
	}
	seen := map[string]bool{}
	var parts []string
	for _, part := range strings.Split(joined, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[strings.ToLower(part)] {
			continue
		}
		// Work around agents whose NO_PROXY parser breaks on bare IPv6
		// literals. The default listener is IPv4; a user who truly needs ::1
		// can set it explicitly for the agent invocation.
		if part == "::1" || part == "[::1]" {
			continue
		}
		seen[strings.ToLower(part)] = true
		parts = append(parts, part)
	}
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" || seen[strings.ToLower(host)] {
			continue
		}
		seen[strings.ToLower(host)] = true
		parts = append(parts, host)
	}
	merged := strings.Join(parts, ",")
	env = setEnv(env, "NO_PROXY", merged, true)
	env = setEnv(env, "no_proxy", merged, true)
	return env
}

func envLookup(env []string, key string) string {
	for _, entry := range env {
		k, value, ok := strings.Cut(entry, "=")
		if ok && envKeyEqual(k, key) {
			return value
		}
	}
	return ""
}

func envKeyEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func removeEnv(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		k, _, _ := strings.Cut(entry, "=")
		if envKeyEqual(k, key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func setEnv(env []string, key, value string, overwrite bool) []string {
	found := false
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		k, _, _ := strings.Cut(entry, "=")
		if envKeyEqual(k, key) {
			found = true
			if overwrite {
				out = append(out, key+"="+value)
			} else {
				out = append(out, entry)
			}
			continue
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, key+"="+value)
	}
	return out
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		if code := exitErr.ExitCode(); code > 0 {
			return code
		}
	}
	return 1
}
