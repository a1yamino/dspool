package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
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
	noEmbed := fs.Bool("no-embed", false, "for agent commands: require an already-running proxy instead of starting one")
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
		case "serve":
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

	cfg, warnings, err := config.Load(*configPath, explicitConfig)
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

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	for _, warning := range warnings {
		logger.Warn("config warning", "detail", warning)
	}

	if command == "serve" {
		if len(passthrough) > 0 {
			logger.Error("serve does not take positional arguments", "args", passthrough)
			os.Exit(2)
		}
		if err := runServe(cfg, logger, *configPath, explicitConfig); err != nil {
			logger.Error("proxy stopped with an error", "error", err)
			os.Exit(1)
		}
		return
	}

	code, err := runAgent(cfg, logger, command, passthrough, *noEmbed)
	if err != nil {
		logger.Error("could not start agent", "agent", command, "error", err)
		os.Exit(1)
	}
	if code != 0 {
		os.Exit(code)
	}
}

func printUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `dspool %s — DeepSeek API key pool proxy

Usage:
  dspool [flags]                Start the proxy (same as "serve")
  dspool serve [flags]          Start the proxy
  dspool claude [args...]       Start Claude Code with dspool env + args pass-through
  dspool codex [args...]        Start Codex CLI with dspool env + args pass-through
  dspool pi [args...]           Start pi with dspool env + args pass-through
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
  -no-embed           Agent commands: do not auto-start a proxy
  -version            Print version
`, version)
}

func newLogger(level string) *slog.Logger {
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
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
