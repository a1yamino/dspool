package pool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"dspool/internal/balance"
	"dspool/internal/keyutil"
)

type memStones struct {
	mu      sync.Mutex
	invalid map[string]bool
}

func newMemStones() *memStones {
	return &memStones{invalid: map[string]bool{}}
}

func (m *memStones) MarkInvalid(id, label, mask, reason string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invalid[id] = true
}

func (m *memStones) ClearInvalid(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.invalid, id)
}

func (m *memStones) IsInvalid(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.invalid[id]
}

func newTestPool(t *testing.T, strategy Strategy, maxInflight int, balances map[string]float64) (*Pool, map[string]string, *memStones) {
	t.Helper()
	var specs []Spec
	ids := map[string]string{}
	for secret := range balances {
		specs = append(specs, Spec{Secret: secret})
		ids[secret] = keyutil.Fingerprint(secret)
	}
	stones := newMemStones()
	p := New(Options{
		Strategy:          strategy,
		MaxInflightPerKey: maxInflight,
		InflightPenalty:   1,
		BalanceInterval:   time.Minute,
		ColdCheckInterval: time.Minute,
		RateLimitCooldown: 10 * time.Millisecond,
		ErrorCooldown:     10 * time.Millisecond,
	}, specs, stones)
	for secret, bal := range balances {
		p.ApplyProbe(ids[secret], balance.Result{
			Available: true, Known: true, Total: bal, Currency: "CNY",
		}, nil, time.Now())
	}
	return p, ids, stones
}

func TestAuthFailureRemovesKeyAndRestoreRevivesIt(t *testing.T) {
	p, ids, stones := newTestPool(t, StrategySpread, 0, map[string]float64{"sk-a": 10})
	lease, err := p.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.Fail(FailAuth, 401, "HTTP 401: Authentication Fails", 0)

	snap, _ := p.Snapshot(ids["sk-a"])
	if snap.State != StateInvalid {
		t.Fatalf("state = %s, want invalid", snap.State)
	}
	if !stones.IsInvalid(ids["sk-a"]) {
		t.Fatal("tombstone not recorded")
	}
	if _, err := p.Acquire(nil); err == nil {
		t.Fatal("invalid key should not be acquired")
	}

	if _, ok := p.Restore(ids["sk-a"]); !ok {
		t.Fatal("restore failed")
	}
	if stones.IsInvalid(ids["sk-a"]) {
		t.Fatal("restore did not clear tombstone")
	}
	tr := p.ApplyProbe(ids["sk-a"], balance.Result{Available: true, Known: true, Total: 3, Currency: "CNY"}, nil, time.Now())
	if tr.To != StateHot {
		t.Fatalf("transition = %+v", tr)
	}
	if _, err := p.Acquire(nil); err != nil {
		t.Fatalf("acquire after restore: %v", err)
	}
}

func TestProbeAuthErrorRemovesKey(t *testing.T) {
	p, ids, stones := newTestPool(t, StrategySpread, 0, map[string]float64{"sk-a": 10})
	tr := p.ApplyProbe(ids["sk-a"], balance.Result{}, fmt.Errorf("%w: 401", balance.ErrAuth), time.Now())
	if tr.To != StateInvalid {
		t.Fatalf("transition = %+v", tr)
	}
	if !stones.IsInvalid(ids["sk-a"]) {
		t.Fatal("tombstone not recorded")
	}
}

func TestInsufficientBalanceMovesToColdThenBack(t *testing.T) {
	p, ids, _ := newTestPool(t, StrategySpread, 0, map[string]float64{"sk-a": 10})
	lease, err := p.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.Fail(FailInsufficient, 402, "HTTP 402: Insufficient Balance", 0)
	snap, _ := p.Snapshot(ids["sk-a"])
	if snap.State != StateCold || snap.Balance != 0 || snap.BalanceKnown != true {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, err := p.Acquire(nil); err == nil {
		t.Fatal("cold key should not be acquired")
	}

	tr := p.ApplyProbe(ids["sk-a"], balance.Result{Available: true, Known: true, Total: 5, Currency: "CNY"}, nil, time.Now())
	if tr.From != StateCold || tr.To != StateHot {
		t.Fatalf("transition = %+v", tr)
	}
	if _, err := p.Acquire(nil); err != nil {
		t.Fatalf("acquire after top-up: %v", err)
	}
}

func TestRateLimitCooldown(t *testing.T) {
	p, ids, _ := newTestPool(t, StrategySpread, 0, map[string]float64{"sk-a": 10})
	lease, err := p.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.Fail(FailRateLimit, 429, "429", 10*time.Millisecond)
	if _, err := p.Acquire(nil); err == nil {
		t.Fatal("key should be cooling down")
	} else {
		var nk *NoKeysError
		if !errors.As(err, &nk) || nk.Cooling != 1 {
			t.Fatalf("error = %#v", err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := p.Acquire(nil); err != nil {
		t.Fatalf("acquire after cooldown: %v", err)
	}
	snap, _ := p.Snapshot(ids["sk-a"])
	if snap.Stats.RateLimited != 1 {
		t.Fatalf("stats = %+v", snap.Stats)
	}
}

func TestStrategies(t *testing.T) {
	t.Run("highest", func(t *testing.T) {
		p, ids, _ := newTestPool(t, StrategyHighest, 0, map[string]float64{"sk-low": 10, "sk-high": 100})
		lease, err := p.Acquire(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Cancel()
		if lease.KeyID() != ids["sk-high"] {
			t.Fatalf("picked %s, want high balance key", lease.Label())
		}
	})

	t.Run("drain", func(t *testing.T) {
		p, ids, _ := newTestPool(t, StrategyDrain, 0, map[string]float64{"sk-low": 10, "sk-high": 100})
		lease, err := p.Acquire(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Cancel()
		if lease.KeyID() != ids["sk-low"] {
			t.Fatalf("picked %s, want low balance key", lease.Label())
		}
	})

	t.Run("spread_uses_idle_keys", func(t *testing.T) {
		p, _, _ := newTestPool(t, StrategySpread, 0, map[string]float64{"sk-a": 10, "sk-b": 10})
		one, err := p.Acquire(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer one.Cancel()
		two, err := p.Acquire(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer two.Cancel()
		if one.KeyID() == two.KeyID() {
			t.Fatalf("spread reused loaded key %s", one.Label())
		}
	})

	t.Run("spread_prefers_low_balance_among_idle", func(t *testing.T) {
		p, ids, _ := newTestPool(t, StrategySpread, 0, map[string]float64{"sk-low": 10, "sk-high": 100})
		lease, err := p.Acquire(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Cancel()
		if lease.KeyID() != ids["sk-low"] {
			t.Fatalf("spread picked %s, want the low balance key", lease.Label())
		}
	})

	t.Run("round_robin", func(t *testing.T) {
		p, _, _ := newTestPool(t, StrategyRoundRobin, 0, map[string]float64{"sk-a": 10, "sk-b": 10})
		var picked []string
		for i := 0; i < 4; i++ {
			lease, err := p.Acquire(nil)
			if err != nil {
				t.Fatal(err)
			}
			picked = append(picked, lease.KeyID())
			lease.Cancel()
		}
		for i := 1; i < len(picked); i++ {
			if picked[i] == picked[i-1] {
				t.Fatalf("round robin repeated a key: %v", picked)
			}
		}
	})

	t.Run("weighted_prefers_balance", func(t *testing.T) {
		p, ids, _ := newTestPool(t, StrategyWeighted, 0, map[string]float64{"sk-low": 1, "sk-high": 100})
		counts := map[string]int{}
		for i := 0; i < 300; i++ {
			lease, err := p.Acquire(nil)
			if err != nil {
				t.Fatal(err)
			}
			counts[lease.KeyID()]++
			lease.Cancel()
		}
		if counts[ids["sk-high"]] <= counts[ids["sk-low"]] {
			t.Fatalf("weighted routing did not prefer high balance: %v", counts)
		}
	})
}

func TestMaxInflight(t *testing.T) {
	p, _, _ := newTestPool(t, StrategySpread, 1, map[string]float64{"sk-a": 10})
	lease, err := p.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Acquire(nil)
	var nk *NoKeysError
	if !errors.As(err, &nk) || nk.Busy != 1 {
		t.Fatalf("err = %#v, want busy", err)
	}
	lease.Cancel()
	if _, err := p.Acquire(nil); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestExcludePreviouslyTriedKeys(t *testing.T) {
	p, ids, _ := newTestPool(t, StrategyHighest, 0, map[string]float64{"sk-a": 10, "sk-b": 10})
	exclude := map[string]struct{}{ids["sk-a"]: {}}
	lease, err := p.Acquire(exclude)
	if err != nil {
		t.Fatal(err)
	}
	lease.Cancel()
	if lease.KeyID() != ids["sk-b"] {
		t.Fatalf("picked excluded key")
	}
}

func TestSyncKeys(t *testing.T) {
	p, _, _ := newTestPool(t, StrategySpread, 0, map[string]float64{"sk-a": 10})
	diff := p.SyncKeys([]Spec{{Secret: "sk-a"}, {Secret: "sk-b"}})
	if len(diff.Added) != 1 || len(diff.Removed) != 0 {
		t.Fatalf("diff = %+v", diff)
	}
	if c := p.Counts(); c.Total != 2 {
		t.Fatalf("counts = %+v", c)
	}
	diff = p.SyncKeys([]Spec{{Secret: "sk-b"}})
	if len(diff.Added) != 0 || len(diff.Removed) != 1 {
		t.Fatalf("diff = %+v", diff)
	}
	if c := p.Counts(); c.Total != 1 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestNewRespectsTombstones(t *testing.T) {
	stones := newMemStones()
	id := keyutil.Fingerprint("sk-dead")
	stones.MarkInvalid(id, "", "", "401", time.Now())
	p := New(Options{Strategy: StrategySpread}, []Spec{{Secret: "sk-dead"}}, stones)
	snap, ok := p.Snapshot(id)
	if !ok || snap.State != StateInvalid {
		t.Fatalf("snapshot = %+v, ok=%v", snap, ok)
	}
}
