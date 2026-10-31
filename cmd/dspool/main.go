package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"dspool/internal/config"
)

const version = "0.1.0"

func main() {
	fs := flag.NewFlagSet("dspool", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	configPath := fs.String("config", "config.yaml", "path to the YAML config file (optional)")
	listenFlag := fs.String("listen", "", "override listen address, e.g. 127.0.0.1:8787")
	logLevelFlag := fs.String("log-level", "", "override log level: debug, info, warn, error")
	noStart := fs.Bool("no-start", false, "for agent commands: require an already-running daemon")
	noEmbed := fs.Bool("no-embed", false, "deprecated alias for -no-start")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() { printUsage(fs) }

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}
	if *showVersion {
		fmt.Printf("dspool %s\n", version)
		return
	}

	explicitConfig := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})

	args := fs.Args()
	command := "serve"
	var passthrough []string
	if len(args) > 0 {
		switch args[0] {
		case "serve", "start", "stop", "status":
			command = args[0]
			passthrough = args[1:]
		case "claude", "codex", "pi":
			command = args[0]
			passthrough = args[1:]
		case "help":
			printUsage(fs)
			return
		default:
			fmt.Fprintf(os.Stderr, "dspool: unknown command %q\n\n", args[0])
			printUsage(fs)
			os.Exit(2)
		}
	}

	effectiveConfigPath := *configPath
	usedFallbackConfig := false
	if !explicitConfig {
		if _, statErr := os.Stat(effectiveConfigPath); errors.Is(statErr, os.ErrNotExist) {
			if fallback := findDefaultConfig(); fallback != "" {
				effectiveConfigPath = fallback
				usedFallbackConfig = true
			}
		}
	}

	cfg, warnings, err := config.Load(effectiveConfigPath, explicitConfig || usedFallbackConfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dspool:", err)
		os.Exit(1)
	}
	if *listenFlag != "" {
		cfg.Listen = *listenFlag
	}
	if *logLevelFlag != "" {
		cfg.LogLevel = *logLevelFlag
	}

	logPath := resolveLogPath(cfg, command)
	logger, closeLog, logErr := newLogger(cfg.LogLevel, logPath)
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "dspool: cannot open log file %s: %v; logging to stderr\n", logPath, logErr)
		logger, closeLog, _ = newLogger(cfg.LogLevel, "")
	}
	defer closeLog()
	slog.SetDefault(logger)
	if usedFallbackConfig {
		logger.Info("using fallback config file", "path", effectiveConfigPath)
	}
	for _, warning := range warnings {
		logger.Warn("config warning", "detail", warning)
	}

	switch command {
	case "serve":
		if len(passthrough) > 0 {
			logger.Error("serve does not take positional arguments", "args", passthrough)
			fmt.Fprintln(os.Stderr, "dspool: serve does not take positional arguments")
			os.Exit(2)
		}
		if err := runServe(cfg, logger, effectiveConfigPath, explicitConfig || usedFallbackConfig); err != nil {
			logger.Error("proxy stopped with an error", "error", err)
			fmt.Fprintln(os.Stderr, "dspool:", err)
			os.Exit(1)
		}
		return

	case "start", "stop", "status":
		if len(passthrough) > 0 {
			fmt.Fprintf(os.Stderr, "dspool: %s does not take positional arguments\n", command)
			os.Exit(2)
		}
		var cmdErr error
		switch command {
		case "start":
			cmdErr = runStart(cfg, logger)
		case "stop":
			cmdErr = runStop(cfg)
		case "status":
			cmdErr = runStatus(cfg)
			if errors.Is(cmdErr, errDaemonNotRunning) {
				fmt.Println("dspool is not running")
				os.Exit(1)
			}
		}
		if cmdErr != nil {
			logger.Error("command failed", "command", command, "error", cmdErr)
			fmt.Fprintln(os.Stderr, "dspool:", cmdErr)
			os.Exit(1)
		}
		return
	}

	requireDaemon := *noStart || *noEmbed
	code, err := runAgent(cfg, logger, command, passthrough, requireDaemon)
	if err != nil {
		logger.Error("could not start agent", "agent", command, "error", err)
		fmt.Fprintln(os.Stderr, "dspool:", err)
		os.Exit(1)
	}
	if code != 0 {
		os.Exit(code)
	}
}

// findDefaultConfig makes a PATH-installed dspool usable outside the project
// directory. It checks a user config first, then the directory containing the
// real executable (following symlinks).
func findDefaultConfig() string {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".config", "dspool", "config.yaml"),
			filepath.Join(home, ".dspool", "config.yaml"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config.yaml"))
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func printUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `dspool %s — DeepSeek API key pool proxy

Usage:
  dspool [flags]                Start the proxy in the foreground (same as "serve")
  dspool serve [flags]          Start the proxy in the foreground
  dspool start                  Start the proxy as a background daemon
  dspool stop                   Stop the background daemon
  dspool status                 Show daemon status
  dspool claude [args...]       Start Claude Code (starts the daemon if needed)
  dspool codex [args...]        Start Codex CLI (starts the daemon if needed)
  dspool pi [args...]           Start pi (starts the daemon if needed)
  dspool help                   Show this help

Flags must come before the subcommand. Everything after an agent name is passed
through unchanged, e.g.:
  dspool claude --resume
  dspool codex exec "fix the tests"
  dspool -config other.yaml pi -p "hello"

Flags:
  -config <path>      Config file (default config.yaml; may be absent for env-only use)
  -listen <addr>      Override listen address
  -log-level <level>  debug, info, warn, error
  -no-start           Agent commands: require an already-running daemon
  -no-embed           Deprecated alias for -no-start
  -version            Print version
`, version)
}

// resolveLogPath decides where dspool's own logs go. Agent shortcuts default to
// a file so the agent's TUI is not polluted; serve keeps logging to stderr
// unless log_file is configured explicitly.
func resolveLogPath(cfg *config.Config, command string) string {
	if cfg.LogFile != "" {
		return cfg.LogFile
	}
	if command == "serve" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.StateFile), "dspool-agent.log")
}

func newLogger(level, path string) (*slog.Logger, func(), error) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	writer := io.Writer(os.Stderr)
	closeFn := func() {}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}
		writer = file
		closeFn = func() { _ = file.Close() }
	}
	return slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: lvl})), closeFn, nil
}
