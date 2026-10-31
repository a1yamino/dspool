package monitor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"dspool/internal/balance"
	"dspool/internal/keyutil"
	"dspool/internal/pool"
)

func TestProbeMovesKeyColdAndBack(t *testing.T) {
	var available atomic.Bool
	available.Store(true)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		if key == "Bearer sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if available.Load() {
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.50"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"0.00"}]}`))
		}
	}))
	defer upstream.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := pool.New(pool.Options{
		Strategy:          pool.StrategySpread,
		BalanceInterval:   time.Second,
		ColdCheckInterval: time.Second,
		RateLimitCooldown: time.Second,
		ErrorCooldown:     time.Second,
	}, []pool.Spec{{Secret: "sk-good"}, {Secret: "sk-bad"}}, nil)
	m := New(p, balance.NewClient(upstream.URL, 2*time.Second), Options{
		ProbeTimeout: time.Second,
		Concurrency:  2,
		Tick:         time.Second,
	}, log)

	ctx := context.Background()
	if n := m.ProbeAll(ctx); n != 2 {
		t.Fatalf("probed %d, want 2", n)
	}

	goodID := keyutil.Fingerprint("sk-good")
	badID := keyutil.Fingerprint("sk-bad")
	good, _ := p.Snapshot(goodID)
	if good.State != pool.StateHot {
		t.Fatalf("good key state = %s", good.State)
	}
	if good.Balance != 12.5 {
		t.Fatalf("balance = %v", good.Balance)
	}
	bad, _ := p.Snapshot(badID)
	if bad.State != pool.StateInvalid {
		t.Fatalf("bad key state = %s", bad.State)
	}

	available.Store(false)
	m.ProbeAll(ctx)
	good, _ = p.Snapshot(goodID)
	if good.State != pool.StateCold {
		t.Fatalf("good key state = %s, want cold", good.State)
	}

	available.Store(true)
	m.ProbeAll(ctx)
	good, _ = p.Snapshot(goodID)
	if good.State != pool.StateHot {
		t.Fatalf("good key state = %s, want hot", good.State)
	}
}

func TestMaybeRefreshColdRespectsCooldown(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"0"}]}`))
	}))
	defer upstream.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := pool.New(pool.Options{Strategy: pool.StrategySpread}, []pool.Spec{{Secret: "sk-a"}}, nil)
	// Put the key in the cold pool first.
	p.ApplyProbe(keyutil.Fingerprint("sk-a"), balance.Result{Available: false, Known: true, Total: 0, Currency: "CNY"}, nil, time.Now())
	m := New(p, balance.NewClient(upstream.URL, time.Second), Options{
		ProbeTimeout:        time.Second,
		ColdRefreshCooldown: time.Minute,
	}, log)

	m.MaybeRefreshCold(context.Background())
	m.MaybeRefreshCold(context.Background())
	if got := hits.Load(); got != 1 {
		t.Fatalf("balance hits = %d, want 1", got)
	}
}
