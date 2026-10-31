package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dspool/internal/config"
)

var errDaemonNotRunning = errors.New("dspool is not running")

var daemonClient = &http.Client{
	Timeout:   2 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

func daemonLogPath(cfg *config.Config) string {
	if cfg.LogFile != "" {
		return cfg.LogFile
	}
	return filepath.Join(filepath.Dir(cfg.StateFile), "dspool.log")
}

// ensureDaemon returns a reachable daemon URL, starting a detached serve
// process if nothing is listening yet.
func daemonBase(cfg *config.Config) (string, error) {
	base, err := baseURLFromListen(cfg.Listen)
	if err != nil {
		return "", fmt.Errorf("the background daemon needs a fixed listen port (set listen in config.yaml): %w", err)
	}
	if len(cfg.ClientKeys) == 0 && !urlIsLoopback(base) {
		return "", fmt.Errorf("client_keys is empty but the daemon URL %s is not loopback; add client_keys to config.yaml or listen on 127.0.0.1", base)
	}
	return base, nil
}

func ensureDaemon(cfg *config.Config, logger *slog.Logger) (base, logPath string, started bool, err error) {
	base, err = daemonBase(cfg)
	if err != nil {
		return "", "", false, err
	}
	logPath = daemonLogPath(cfg)
	if proxyRunning(base) {
		return base, logPath, false, nil
	}

	logger.Info("starting background dspool daemon", "listen", cfg.Listen, "log", logPath)
	waitCh, err := launchDaemon(cfg, logPath)
	if err != nil {
		return "", logPath, false, err
	}
	if err := waitForDaemon(base, cfg, logPath, waitCh); err != nil {
		return "", logPath, false, err
	}
	return base, logPath, true, nil
}

func launchDaemon(cfg *config.Config, logPath string) (<-chan error, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate dspool executable: %w", err)
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}

	// Always pass -config, even when empty: an explicit flag prevents the
	// child from discovering a different config.yaml than this process used.
	args := []string{"-config", cfg.ConfigPath, "serve"}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("create daemon log directory: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon log %s: %w", logPath, err)
	}

	cmd := exec.Command(exe, args...)
	if cfg.BaseDir != "" {
		cmd.Dir = cfg.BaseDir
	}
	cmd.Env = daemonEnv(cfg, logPath)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start background daemon: %w", err)
	}
	_ = logFile.Close()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	return waitCh, nil
}

func daemonEnv(cfg *config.Config, logPath string) []string {
	env := os.Environ()
	env = setEnv(env, "DSPOOL_LISTEN", cfg.Listen, true)
	env = setEnv(env, "DSPOOL_LOG_LEVEL", cfg.LogLevel, true)
	env = setEnv(env, "DSPOOL_STATE_FILE", cfg.StateFile, true)
	env = setEnv(env, "DSPOOL_LOG_FILE", logPath, true)
	return env
}

func waitForDaemon(base string, cfg *config.Config, logPath string, waitCh <-chan error) error {
	timeout := cfg.Pool.StartupProbeTimeout.D() + 20*time.Second
	timeout = max(timeout, 20*time.Second)
	timeout = min(timeout, 2*time.Minute)
	deadline := time.Now().Add(timeout)

	for {
		if proxyRunning(base) {
			return nil
		}
		select {
		case waitErr := <-waitCh:
			// A second concurrent shortcut may have won the port race. Give
			// it a moment before reporting our child's failure.
			time.Sleep(300 * time.Millisecond)
			if proxyRunning(base) {
				return nil
			}
			if waitErr == nil {
				waitErr = errors.New("process exited before becoming ready")
			}
			return fmt.Errorf("background dspool failed to start: %w (log: %s)", waitErr, logPath)
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dspool did not become ready at %s within %s (log: %s)", base, timeout.Round(time.Second), logPath)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func fetchDaemonData(base string) (map[string]any, error) {
	resp, err := daemonClient.Get(strings.TrimRight(base, "/") + "/dashboard/data")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("dashboard data returned HTTP %d", resp.StatusCode)
	}
	var data map[string]any
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

// checkDaemonToken catches the common case where a daemon is still running
// with an older client_keys configuration (or a different config file).
func checkDaemonToken(base, token string) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := daemonClient.Do(req)
	if err != nil {
		// The health probe already succeeded; do not fail merely because the
		// models endpoint was slow.
		return nil
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("daemon at %s rejected the client token; restart it with `dspool stop && dspool start` or align client_keys", base)
	}
	return nil
}

func daemonPIDFromData(data map[string]any) int {
	if pid, ok := data["pid"].(float64); ok && pid > 0 {
		return int(pid)
	}
	return 0
}

func runStart(cfg *config.Config, logger *slog.Logger) error {
	base, err := daemonBase(cfg)
	if err != nil {
		return err
	}
	already := proxyRunning(base)

	base, logPath, _, err := ensureDaemon(cfg, logger)
	if err != nil {
		return err
	}

	data, _ := fetchDaemonData(base)
	pid := daemonPIDFromData(data)
	if already {
		fmt.Println("dspool is already running")
	} else {
		fmt.Println("dspool started in the background")
	}
	fmt.Printf("  url: %s/\n", base)
	if pid > 0 {
		fmt.Printf("  pid: %d\n", pid)
	} else {
		fmt.Println("  pid: unknown (older daemon; restart it to enable management)")
	}
	reportedLog := logPath
	if data != nil {
		if lf, ok := data["log_file"].(string); ok && lf == "" && already {
			reportedLog = "stderr (foreground serve)"
		} else if ok && lf != "" {
			reportedLog = lf
		}
	}
	fmt.Printf("  log: %s\n", reportedLog)
	return nil
}

func runStop(cfg *config.Config) error {
	base, err := daemonBase(cfg)
	if err != nil {
		return err
	}
	if !proxyRunning(base) {
		fmt.Println("dspool is not running")
		return nil
	}

	data, err := fetchDaemonData(base)
	if err != nil {
		return fmt.Errorf("dspool is running at %s but its PID is unavailable: %w; stop it manually and start a new daemon", base, err)
	}
	pid := daemonPIDFromData(data)
	if pid <= 0 {
		return fmt.Errorf("dspool is running at %s but did not report a PID (it may be an older build); stop it manually (Ctrl-C in its terminal, or kill the listener) and run `dspool start`", base)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find dspool process %d: %w", pid, err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("send SIGTERM to dspool pid %d: %w", pid, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !proxyRunning(base) {
			fmt.Printf("dspool stopped (pid %d)\n", pid)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("sent SIGTERM to dspool pid %d, but it is still responding at %s", pid, base)
}

func runStatus(cfg *config.Config) error {
	base, err := daemonBase(cfg)
	if err != nil {
		return err
	}
	if !proxyRunning(base) {
		return errDaemonNotRunning
	}

	data, fetchErr := fetchDaemonData(base)
	fmt.Println("dspool is running")
	fmt.Printf("  url: %s/\n", base)
	if fetchErr != nil {
		fmt.Printf("  log: %s\n", daemonLogPath(cfg))
		return nil
	}
	if pid := daemonPIDFromData(data); pid > 0 {
		fmt.Printf("  pid: %d\n", pid)
	} else {
		fmt.Println("  pid: unknown (older daemon; restart it to enable management)")
	}
	if strategy, ok := data["strategy"].(string); ok && strategy != "" {
		fmt.Printf("  strategy: %s\n", strategy)
	}
	if counts, ok := data["counts"].(map[string]any); ok {
		fmt.Printf("  keys: hot=%v cold=%v invalid=%v cooling=%v\n",
			counts["hot"], counts["cold"], counts["invalid"], counts["cooling"])
	}
	if seconds, ok := data["uptime_seconds"].(float64); ok {
		fmt.Printf("  uptime: %s\n", formatUptime(int64(seconds)))
	}
	logLine := daemonLogPath(cfg)
	if lf, ok := data["log_file"].(string); ok {
		if lf == "" {
			logLine = "stderr (foreground serve)"
		} else {
			logLine = lf
		}
	}
	fmt.Printf("  log: %s\n", logLine)
	return nil
}

func formatUptime(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
}
