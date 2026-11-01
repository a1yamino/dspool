package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dspool/internal/balance"
	"dspool/internal/config"
	"dspool/internal/monitor"
	"dspool/internal/pool"
	"dspool/internal/proxy"
	"dspool/internal/state"
)

type core struct {
	cfg     *config.Config
	logger  *slog.Logger
	state   *state.Store
	pool    *pool.Pool
	monitor *monitor.Monitor
	server  *proxy.Server
}

func buildCore(cfg *config.Config, logger *slog.Logger) (*core, error) {
	st, err := state.Open(cfg.StateFile, logger)
	if err != nil {
		return nil, fmt.Errorf("open state file: %w", err)
	}

	specs := make([]pool.Spec, 0, len(cfg.Keys))
	for _, k := range cfg.Keys {
		specs = append(specs, pool.Spec{Label: k.Label, Secret: k.Key})
	}
	p := pool.New(pool.Options{
		Strategy:          pool.ParseStrategy(cfg.Pool.Strategy),
		MaxInflightPerKey: cfg.Pool.MaxInflightPerKey,
		InflightPenalty:   cfg.Pool.InflightPenalty,
		MinBalance:        cfg.Pool.MinBalance,
		BalanceInterval:   cfg.Pool.BalanceInterval.D(),
		ColdCheckInterval: cfg.Pool.ColdCheckInterval.D(),
		RateLimitCooldown: cfg.Pool.RateLimitCooldown.D(),
		ErrorCooldown:     cfg.Pool.ErrorCooldown.D(),
	}, specs, st)

	balanceClient := balance.NewClient(cfg.Upstream.BalanceURL, cfg.Upstream.ProbeTimeout.D())
	mon := monitor.New(p, balanceClient, monitor.Options{
		ProbeTimeout:        cfg.Upstream.ProbeTimeout.D(),
		ColdRefreshCooldown: cfg.Pool.ExhaustionRefreshCooldown.D(),
		Concurrency:         8,
		Tick:                2 * time.Second,
	}, logger)

	probeCtx, probeCancel := context.WithTimeout(context.Background(), cfg.Pool.StartupProbeTimeout.D())
	probed := mon.ProbeAll(probeCtx)
	probeCancel()
	counts := p.Counts()
	logger.Info("startup balance probe complete",
		"probed", probed,
		"hot", counts.Hot, "cold", counts.Cold,
		"invalid", counts.Invalid, "probing", counts.Probing,
	)

	srv, err := proxy.New(cfg, p, mon, st, version, logger)
	if err != nil {
		return nil, fmt.Errorf("build proxy: %w", err)
	}
	return &core{cfg: cfg, logger: logger, state: st, pool: p, monitor: mon, server: srv}, nil
}

type running struct {
	core       *core
	httpServer *http.Server
	listener   net.Listener
	monCancel  context.CancelFunc
	errCh      chan error
}

func (c *core) start() (*running, error) {
	ln, err := net.Listen("tcp", c.cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w (is another dspool or service already using it?)", c.cfg.Listen, err)
	}

	httpServer := &http.Server{
		Addr:              c.cfg.Listen,
		Handler:           c.server.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	monCtx, monCancel := context.WithCancel(context.Background())
	go c.monitor.Run(monCtx)

	errCh := make(chan error, 1)
	go func() {
		serveErr := httpServer.Serve(ln)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	base, _ := baseURLFromAddr(ln.Addr().String())
	c.logger.Info("dspool listening",
		"addr", ln.Addr().String(),
		"strategy", c.cfg.Pool.Strategy,
		"keys", len(c.cfg.Keys),
		"balance_interval", c.cfg.Pool.BalanceInterval.D().String(),
		"cold_check_interval", c.cfg.Pool.ColdCheckInterval.D().String(),
		"dashboard", base+"/",
		"version", version,
	)

	return &running{
		core:       c,
		httpServer: httpServer,
		listener:   ln,
		monCancel:  monCancel,
		errCh:      errCh,
	}, nil
}

func (r *running) baseURL() (string, error) {
	return baseURLFromAddr(r.listener.Addr().String())
}

func (r *running) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = r.httpServer.Shutdown(ctx)
	r.monCancel()
	if err := r.core.state.Flush(); err != nil {
		r.core.logger.Error("failed to flush state", "error", err)
	}
}

func runServe(cfg *config.Config, logger *slog.Logger, configPath string, explicit bool) error {
	c, err := buildCore(cfg, logger)
	if err != nil {
		return err
	}
	c.server.SetKeyReloader(func() (pool.KeyDiff, error) {
		return reloadKeys(configPath, explicit, c.pool, c.monitor, logger)
	})

	r, err := c.start()
	if err != nil {
		return err
	}
	defer r.shutdown()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				_, _ = c.server.ReloadKeys()
				continue
			}
			logger.Info("shutting down", "signal", sig.String())
			return nil
		case serveErr := <-r.errCh:
			if serveErr != nil {
				return serveErr
			}
			return nil
		}
	}
}

func reloadKeys(path string, explicit bool, p *pool.Pool, mon *monitor.Monitor, log *slog.Logger) (pool.KeyDiff, error) {
	newCfg, warnings, err := config.Load(path, explicit)
	if err != nil {
		log.Error("config reload failed; keeping current keys", "error", err)
		return pool.KeyDiff{}, err
	}
	for _, warning := range warnings {
		log.Warn("config warning", "detail", warning)
	}
	specs := make([]pool.Spec, 0, len(newCfg.Keys))
	for _, k := range newCfg.Keys {
		specs = append(specs, pool.Spec{Label: k.Label, Secret: k.Key})
	}
	diff := p.SyncKeys(specs)
	for _, added := range diff.Added {
		log.Info("key added by reload", "label", added.Label, "mask", added.Mask)
	}
	for _, removed := range diff.Removed {
		log.Info("key removed by reload", "label", removed.Label, "mask", removed.Mask)
	}
	log.Info("reloaded upstream keys",
		"added", len(diff.Added), "removed", len(diff.Removed),
		"note", "other config changes require a restart",
	)
	mon.Wake()
	return diff, nil
}

func baseURLFromAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parse listen address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

var errAutoPort = errors.New("listen port is 0; cannot infer an external proxy URL")

func baseURLFromListen(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("parse listen address %q: %w", listen, err)
	}
	if port == "" || port == "0" {
		return "", errAutoPort
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}
