// Package monitor periodically checks DeepSeek balances and moves keys between
// the hot and cold pools.
package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"dspool/internal/balance"
	"dspool/internal/pool"
)

type Options struct {
	ProbeTimeout        time.Duration
	ColdRefreshCooldown time.Duration
	Concurrency         int
	Tick                time.Duration
}

type Monitor struct {
	p      *pool.Pool
	client *balance.Client
	log    *slog.Logger
	opts   Options

	wake chan struct{}

	refreshMu       sync.Mutex
	lastColdRefresh time.Time
}

func New(p *pool.Pool, client *balance.Client, opts Options, log *slog.Logger) *Monitor {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.Tick <= 0 {
		opts.Tick = 2 * time.Second
	}
	if opts.ColdRefreshCooldown <= 0 {
		opts.ColdRefreshCooldown = 10 * time.Second
	}
	return &Monitor{
		p:      p,
		client: client,
		log:    log,
		opts:   opts,
		wake:   make(chan struct{}, 1),
	}
}

func (m *Monitor) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run probes due keys until ctx is canceled.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.opts.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
		jobs := m.p.ClaimDue(time.Now())
		if len(jobs) > 0 {
			m.probe(ctx, jobs)
		}
	}
}

// ProbeAll forces a check of every non-invalid key.
func (m *Monitor) ProbeAll(ctx context.Context) int {
	jobs := m.p.ClaimAll(time.Now())
	m.probe(ctx, jobs)
	return len(jobs)
}

// ProbeColdNow checks only the cold pool. Used when the hot pool is exhausted
// so a recent top-up is discovered immediately.
func (m *Monitor) ProbeColdNow(ctx context.Context) int {
	jobs := m.p.ClaimCold(time.Now())
	m.probe(ctx, jobs)
	return len(jobs)
}

// MaybeRefreshCold probes the cold pool at most once per cooldown window.
func (m *Monitor) MaybeRefreshCold(ctx context.Context) {
	m.refreshMu.Lock()
	if time.Since(m.lastColdRefresh) < m.opts.ColdRefreshCooldown {
		m.refreshMu.Unlock()
		return
	}
	m.lastColdRefresh = time.Now()
	m.refreshMu.Unlock()
	m.ProbeColdNow(ctx)
}

func (m *Monitor) probe(ctx context.Context, jobs []pool.ProbeJob) {
	if len(jobs) == 0 {
		return
	}
	sem := make(chan struct{}, m.opts.Concurrency)
	var wg sync.WaitGroup
	for _, job := range jobs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			m.p.AbortProbe(job.ID, time.Now())
			continue
		}
		wg.Add(1)
		go func(job pool.ProbeJob) {
			defer wg.Done()
			defer func() { <-sem }()
			m.probeOne(ctx, job)
		}(job)
	}
	wg.Wait()
}

func (m *Monitor) probeOne(ctx context.Context, job pool.ProbeJob) {
	defer func() {
		if rec := recover(); rec != nil {
			m.log.Error("balance probe panicked", "key", job.Label, "mask", job.Mask, "panic", rec)
			m.p.AbortProbe(job.ID, time.Now())
		}
	}()

	pctx, cancel := context.WithTimeout(ctx, m.opts.ProbeTimeout)
	res, err := m.client.Fetch(pctx, job.Secret)
	cancel()

	tr := m.p.ApplyProbe(job.ID, res, err, time.Now())
	if tr.From == "" {
		return
	}
	if err != nil {
		if tr.From != tr.To {
			m.log.Warn("balance probe failed; key state changed",
				"key", tr.Label, "mask", tr.Mask,
				"from", string(tr.From), "to", string(tr.To),
				"error", err)
		} else {
			m.log.Debug("balance probe failed",
				"key", tr.Label, "mask", tr.Mask, "error", err)
		}
		return
	}
	switch {
	case tr.From != tr.To && tr.To == pool.StateHot:
		m.log.Info("key entered hot pool",
			"key", tr.Label, "mask", tr.Mask,
			"balance", res.Total, "currency", res.Currency,
			"previous", string(tr.From))
	case tr.From != tr.To && tr.To == pool.StateCold:
		m.log.Warn("key moved to cold pool",
			"key", tr.Label, "mask", tr.Mask,
			"balance", res.Total, "currency", res.Currency,
			"reason", tr.Reason)
	case tr.From != tr.To && tr.To == pool.StateInvalid:
		m.log.Warn("key removed from pool",
			"key", tr.Label, "mask", tr.Mask,
			"reason", tr.Reason)
	}
}
