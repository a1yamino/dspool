// Package pool owns the upstream key states and the balance-aware routing
// strategies.
package pool

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"dspool/internal/balance"
	"dspool/internal/keyutil"
)

type State string

const (
	StateProbing State = "probing"
	StateHot     State = "hot"
	StateCold    State = "cold"
	StateInvalid State = "invalid"
)

type FailKind int

const (
	FailAuth FailKind = iota
	FailInsufficient
	FailRateLimit
	FailServer
	FailNetwork
)

type KeyStats struct {
	Attempts            int64     `json:"attempts"`
	Successes           int64     `json:"successes"`
	AuthErrors          int64     `json:"auth_errors"`
	InsufficientBalance int64     `json:"insufficient_balance"`
	RateLimited         int64     `json:"rate_limited"`
	ServerErrors        int64     `json:"server_errors"`
	NetworkErrors       int64     `json:"network_errors"`
	LastUsed            time.Time `json:"last_used"`
	LastStatus          int       `json:"last_status"`
	LastError           string    `json:"last_error,omitempty"`
}

type Key struct {
	ID     string
	Label  string
	Mask   string
	Secret string

	State         State
	Balance       float64
	Currency      string
	BalanceKnown  bool
	Available     bool
	LastChecked   time.Time
	LastError     string
	CooldownUntil time.Time
	NextCheck     time.Time
	Checking      bool
	InFlight      int
	AddedAt       time.Time
	Stats         KeyStats
}

type Spec struct {
	Label  string
	Secret string
}

type Options struct {
	Strategy          Strategy
	MaxInflightPerKey int
	InflightPenalty   float64
	MinBalance        float64
	BalanceInterval   time.Duration
	ColdCheckInterval time.Duration
	RateLimitCooldown time.Duration
	ErrorCooldown     time.Duration
}

type Tombstones interface {
	MarkInvalid(id, label, mask, reason string, at time.Time)
	ClearInvalid(id string)
	IsInvalid(id string) bool
}

type Pool struct {
	mu       sync.Mutex
	opts     Options
	strategy Strategy
	keys     map[string]*Key
	order    []string
	rr       uint64
	stones   Tombstones
}

func New(opts Options, specs []Spec, stones Tombstones) *Pool {
	if opts.MaxInflightPerKey < 0 {
		opts.MaxInflightPerKey = 0
	}
	if opts.InflightPenalty < 0 {
		opts.InflightPenalty = 0
	}
	p := &Pool{
		opts:     opts,
		strategy: opts.Strategy,
		keys:     map[string]*Key{},
		stones:   stones,
	}
	if p.strategy == "" {
		p.strategy = StrategySpread
	}
	now := time.Now()
	for _, spec := range specs {
		spec.Secret = trimSpace(spec.Secret)
		if spec.Secret == "" {
			continue
		}
		id := keyutil.Fingerprint(spec.Secret)
		if _, exists := p.keys[id]; exists {
			continue
		}
		k := p.newKeyLocked(id, spec, now)
		p.keys[id] = k
		p.order = append(p.order, id)
	}
	return p
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func (p *Pool) newKeyLocked(id string, spec Spec, now time.Time) *Key {
	label := trimSpace(spec.Label)
	if label == "" {
		label = "key-" + id[:6]
	}
	k := &Key{
		ID:        id,
		Label:     label,
		Mask:      keyutil.Mask(spec.Secret),
		Secret:    spec.Secret,
		State:     StateProbing,
		AddedAt:   now,
		NextCheck: now,
	}
	if p.stones != nil && p.stones.IsInvalid(id) {
		k.State = StateInvalid
		k.LastError = "previously rejected by DeepSeek; use /admin/keys/{id}/restore to retry"
		k.NextCheck = time.Time{}
	}
	return k
}

// NoKeysError is returned when no eligible hot key exists.
type NoKeysError struct {
	Total    int
	Hot      int
	Cold     int
	Invalid  int
	Probing  int
	Cooling  int
	Busy     int
	Excluded int
}

func (e *NoKeysError) Error() string {
	return fmt.Sprintf(
		"no eligible upstream key (total=%d hot=%d cooling=%d busy=%d excluded=%d cold=%d invalid=%d probing=%d)",
		e.Total, e.Hot, e.Cooling, e.Busy, e.Excluded, e.Cold, e.Invalid, e.Probing,
	)
}

func (e *NoKeysError) HasCold() bool { return e.Cold > 0 }

type Lease struct {
	p      *Pool
	key    *Key
	secret string
	label  string
	mask   string
	once   sync.Once
}

func (l *Lease) KeyID() string  { return l.key.ID }
func (l *Lease) Label() string  { return l.label }
func (l *Lease) Mask() string   { return l.mask }
func (l *Lease) Secret() string { return l.secret }

// Acquire selects an eligible hot key and reserves an in-flight slot. Excluded
// keys are keys already attempted for the current client request.
func (p *Pool) Acquire(exclude map[string]struct{}) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	counts := NoKeysError{Total: len(p.keys)}
	cap := p.opts.MaxInflightPerKey
	cands := make([]*Key, 0, len(p.keys))
	for _, id := range p.order {
		k := p.keys[id]
		switch k.State {
		case StateProbing:
			counts.Probing++
			continue
		case StateCold:
			counts.Cold++
			continue
		case StateInvalid:
			counts.Invalid++
			continue
		case StateHot:
			counts.Hot++
		default:
			continue
		}
		if now.Before(k.CooldownUntil) {
			counts.Cooling++
			continue
		}
		if cap > 0 && k.InFlight >= cap {
			counts.Busy++
			continue
		}
		if _, skip := exclude[id]; skip {
			counts.Excluded++
			continue
		}
		cands = append(cands, k)
	}
	if len(cands) == 0 {
		return nil, &counts
	}

	k := p.chooseLocked(cands)
	k.InFlight++
	k.Stats.Attempts++
	return &Lease{
		p:      p,
		key:    k,
		secret: k.Secret,
		label:  k.Label,
		mask:   k.Mask,
	}, nil
}

func (l *Lease) releaseLocked() {
	if l.key.InFlight > 0 {
		l.key.InFlight--
	}
	l.key.Stats.LastUsed = time.Now()
}

// Success releases the lease and records a 2xx response.
func (l *Lease) Success(status int) {
	l.once.Do(func() {
		l.p.mu.Lock()
		defer l.p.mu.Unlock()
		l.releaseLocked()
		l.key.Stats.Successes++
		l.key.Stats.LastStatus = status
	})
}

// Neutral releases the lease for a response that is not the key's fault, such
// as a 400 caused by the client's request body.
func (l *Lease) Neutral(status int) {
	l.once.Do(func() {
		l.p.mu.Lock()
		defer l.p.mu.Unlock()
		l.releaseLocked()
		l.key.Stats.LastStatus = status
	})
}

// Cancel releases the lease without attributing an outcome (client went away).
func (l *Lease) Cancel() {
	l.once.Do(func() {
		l.p.mu.Lock()
		defer l.p.mu.Unlock()
		l.releaseLocked()
	})
}

// Fail releases the lease and updates the key's pool state.
func (l *Lease) Fail(kind FailKind, status int, reason string, retryAfter time.Duration) {
	l.once.Do(func() {
		now := time.Now()
		p := l.p
		k := l.key
		p.mu.Lock()
		l.releaseLocked()
		k.Stats.LastStatus = status
		k.Stats.LastError = reason
		markInvalid := false
		switch kind {
		case FailAuth:
			k.State = StateInvalid
			k.Available = false
			k.NextCheck = time.Time{}
			k.Checking = false
			k.Stats.AuthErrors++
			markInvalid = true
		case FailInsufficient:
			k.State = StateCold
			k.Balance = 0
			k.BalanceKnown = true
			k.Available = false
			k.Stats.InsufficientBalance++
			k.LastError = reason
			k.NextCheck = now.Add(jitter(p.opts.ColdCheckInterval, 0.2))
		case FailRateLimit:
			k.Stats.RateLimited++
			cooldown := p.opts.RateLimitCooldown
			if retryAfter > cooldown {
				cooldown = retryAfter
			}
			if cooldown > 10*time.Minute {
				cooldown = 10 * time.Minute
			}
			k.CooldownUntil = now.Add(cooldown)
			k.LastError = reason
		case FailServer:
			k.Stats.ServerErrors++
			k.CooldownUntil = now.Add(jitter(p.opts.ErrorCooldown, 0.2))
			k.LastError = reason
		case FailNetwork:
			k.Stats.NetworkErrors++
			k.CooldownUntil = now.Add(jitter(p.opts.ErrorCooldown, 0.2))
			k.LastError = reason
		}
		label, mask, id := k.Label, k.Mask, k.ID
		p.mu.Unlock()
		if markInvalid && p.stones != nil {
			p.stones.MarkInvalid(id, label, mask, reason, now)
		}
	})
}

// Transition describes a probe result that changed a key's pool state.
type Transition struct {
	KeyID  string
	Label  string
	Mask   string
	From   State
	To     State
	Reason string
}

// ApplyProbe applies a balance endpoint result (or error) to a key.
func (p *Pool) ApplyProbe(id string, res balance.Result, probeErr error, now time.Time) Transition {
	p.mu.Lock()
	k := p.keys[id]
	if k == nil {
		p.mu.Unlock()
		return Transition{}
	}
	k.Checking = false
	from := k.State
	markInvalid := false
	reason := k.LastError

	switch {
	case probeErr != nil:
		if errors.Is(probeErr, balance.ErrAuth) {
			if k.State != StateInvalid {
				k.State = StateInvalid
				k.NextCheck = time.Time{}
				k.Available = false
			}
			k.LastError = probeErr.Error()
			markInvalid = true
			reason = k.LastError
		} else {
			k.LastError = probeErr.Error()
			reason = k.LastError
			switch k.State {
			case StateProbing:
				// The balance endpoint is unreachable but the chat endpoint may
				// still work. Be optimistic and let a real request decide.
				k.State = StateHot
				k.BalanceKnown = false
				k.NextCheck = now.Add(jitter(minDuration(p.opts.BalanceInterval, 30*time.Second), 0.2))
			case StateHot:
				k.NextCheck = now.Add(jitter(minDuration(p.opts.BalanceInterval, 30*time.Second), 0.2))
			case StateCold:
				k.NextCheck = now.Add(jitter(p.opts.ColdCheckInterval, 0.2))
			}
		}

	default:
		if k.State == StateInvalid {
			// Invalid keys are only revived by an explicit admin restore.
			break
		}
		k.LastChecked = now
		k.BalanceKnown = res.Known
		k.Balance = res.Total
		k.Currency = res.Currency
		k.Available = res.Available
		usable := res.Available && (!res.Known || res.Total > p.opts.MinBalance)
		if usable {
			k.State = StateHot
			k.LastError = ""
			k.NextCheck = now.Add(jitter(p.opts.BalanceInterval, 0.1))
		} else {
			k.State = StateCold
			if res.Known {
				k.LastError = fmt.Sprintf("balance is %s %.4f (below usable threshold)", res.Currency, res.Total)
			} else {
				k.LastError = "balance reported as unavailable"
			}
			k.NextCheck = now.Add(jitter(p.opts.ColdCheckInterval, 0.2))
		}
		reason = k.LastError
	}

	to := k.State
	label, mask := k.Label, k.Mask
	p.mu.Unlock()

	if markInvalid && p.stones != nil {
		p.stones.MarkInvalid(id, label, mask, reason, now)
	}
	return Transition{KeyID: id, Label: label, Mask: mask, From: from, To: to, Reason: reason}
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}

func jitter(d time.Duration, fraction float64) time.Duration {
	if d <= 0 || fraction <= 0 {
		return d
	}
	mult := 1 + (rand.Float64()*2-1)*fraction
	if mult < 0.1 {
		mult = 0.1
	}
	return time.Duration(float64(d) * mult)
}

type ProbeJob struct {
	ID     string
	Label  string
	Mask   string
	Secret string
}

// ClaimDue marks keys whose next balance check is due and returns their probe
// jobs. The Checking flag prevents overlapping probes for the same key.
func (p *Pool) ClaimDue(now time.Time) []ProbeJob {
	p.mu.Lock()
	defer p.mu.Unlock()
	var jobs []ProbeJob
	for _, id := range p.order {
		k := p.keys[id]
		if k == nil || k.State == StateInvalid || k.Checking {
			continue
		}
		if k.NextCheck.After(now) {
			continue
		}
		k.Checking = true
		jobs = append(jobs, ProbeJob{ID: k.ID, Label: k.Label, Mask: k.Mask, Secret: k.Secret})
	}
	return jobs
}

// ClaimAll claims every non-invalid key for a forced refresh.
func (p *Pool) ClaimAll(now time.Time) []ProbeJob {
	p.mu.Lock()
	defer p.mu.Unlock()
	var jobs []ProbeJob
	for _, id := range p.order {
		k := p.keys[id]
		if k == nil || k.State == StateInvalid || k.Checking {
			continue
		}
		k.Checking = true
		jobs = append(jobs, ProbeJob{ID: k.ID, Label: k.Label, Mask: k.Mask, Secret: k.Secret})
	}
	return jobs
}

// ClaimCold claims only cold keys, used when the hot pool is exhausted.
func (p *Pool) ClaimCold(now time.Time) []ProbeJob {
	p.mu.Lock()
	defer p.mu.Unlock()
	var jobs []ProbeJob
	for _, id := range p.order {
		k := p.keys[id]
		if k == nil || k.State != StateCold || k.Checking {
			continue
		}
		k.Checking = true
		jobs = append(jobs, ProbeJob{ID: k.ID, Label: k.Label, Mask: k.Mask, Secret: k.Secret})
	}
	return jobs
}

// AbortProbe clears a Checking flag after a panic or cancellation that did not
// reach ApplyProbe.
func (p *Pool) AbortProbe(id string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.keys[id]
	if k == nil {
		return
	}
	k.Checking = false
	if k.State == StateProbing {
		k.State = StateHot
		k.BalanceKnown = false
		k.NextCheck = now.Add(jitter(minDuration(p.opts.BalanceInterval, 30*time.Second), 0.2))
	} else if k.State != StateInvalid {
		k.NextCheck = now.Add(jitter(minDuration(p.opts.ErrorCooldown, 30*time.Second), 0.2))
	}
}

// Restore clears an invalid/cold state and schedules an immediate probe.
func (p *Pool) Restore(id string) (Snapshot, bool) {
	p.mu.Lock()
	k := p.keys[id]
	if k == nil {
		p.mu.Unlock()
		return Snapshot{}, false
	}
	k.State = StateProbing
	k.LastError = "manually restored; probing"
	k.CooldownUntil = time.Time{}
	k.NextCheck = time.Now()
	k.Checking = false
	snap := snapshotOf(k)
	p.mu.Unlock()
	if p.stones != nil {
		p.stones.ClearInvalid(id)
	}
	return snap, true
}

type KeyDiff struct {
	Added   []Snapshot
	Removed []Snapshot
}

// SyncKeys reconciles the pool with a new configuration (used on SIGHUP).
func (p *Pool) SyncKeys(specs []Spec) KeyDiff {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	newSpecs := make(map[string]Spec, len(specs))
	newOrder := make([]string, 0, len(specs))
	for _, spec := range specs {
		spec.Secret = trimSpace(spec.Secret)
		if spec.Secret == "" {
			continue
		}
		id := keyutil.Fingerprint(spec.Secret)
		if _, dup := newSpecs[id]; dup {
			continue
		}
		newSpecs[id] = spec
		newOrder = append(newOrder, id)
	}

	var diff KeyDiff
	for _, id := range newOrder {
		spec := newSpecs[id]
		if k, ok := p.keys[id]; ok {
			k.Label = trimSpace(spec.Label)
			if k.Label == "" {
				k.Label = "key-" + id[:6]
			}
			k.Mask = keyutil.Mask(spec.Secret)
			continue
		}
		k := p.newKeyLocked(id, spec, now)
		p.keys[id] = k
		diff.Added = append(diff.Added, snapshotOf(k))
	}
	for _, id := range p.order {
		if _, keep := newSpecs[id]; keep {
			continue
		}
		if k, ok := p.keys[id]; ok {
			diff.Removed = append(diff.Removed, snapshotOf(k))
			delete(p.keys, id)
		}
	}
	p.order = newOrder
	return diff
}

type Counts struct {
	Total   int `json:"total"`
	Hot     int `json:"hot"`
	Cold    int `json:"cold"`
	Invalid int `json:"invalid"`
	Probing int `json:"probing"`
	Cooling int `json:"cooling"`
}

func (p *Pool) Counts() Counts {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var c Counts
	c.Total = len(p.keys)
	for _, k := range p.keys {
		switch k.State {
		case StateHot:
			c.Hot++
			if now.Before(k.CooldownUntil) {
				c.Cooling++
			}
		case StateCold:
			c.Cold++
		case StateInvalid:
			c.Invalid++
		case StateProbing:
			c.Probing++
		}
	}
	return c
}

type Snapshot struct {
	ID            string    `json:"id"`
	Label         string    `json:"label"`
	Mask          string    `json:"mask"`
	State         State     `json:"state"`
	Balance       float64   `json:"balance"`
	Currency      string    `json:"currency,omitempty"`
	BalanceKnown  bool      `json:"balance_known"`
	Available     bool      `json:"available"`
	LastChecked   time.Time `json:"last_checked"`
	NextCheck     time.Time `json:"next_check"`
	CooldownUntil time.Time `json:"cooldown_until"`
	Checking      bool      `json:"checking"`
	InFlight      int       `json:"in_flight"`
	LastError     string    `json:"last_error,omitempty"`
	AddedAt       time.Time `json:"added_at"`
	Stats         KeyStats  `json:"stats"`
}

func snapshotOf(k *Key) Snapshot {
	return Snapshot{
		ID:            k.ID,
		Label:         k.Label,
		Mask:          k.Mask,
		State:         k.State,
		Balance:       k.Balance,
		Currency:      k.Currency,
		BalanceKnown:  k.BalanceKnown,
		Available:     k.Available,
		LastChecked:   k.LastChecked,
		NextCheck:     k.NextCheck,
		CooldownUntil: k.CooldownUntil,
		Checking:      k.Checking,
		InFlight:      k.InFlight,
		LastError:     k.LastError,
		AddedAt:       k.AddedAt,
		Stats:         k.Stats,
	}
}

// Snapshots returns a copy of all key states in configuration order.
func (p *Pool) Snapshots() []Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Snapshot, 0, len(p.order))
	for _, id := range p.order {
		if k := p.keys[id]; k != nil {
			out = append(out, snapshotOf(k))
		}
	}
	return out
}

func (p *Pool) Snapshot(id string) (Snapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.keys[id]
	if k == nil {
		return Snapshot{}, false
	}
	return snapshotOf(k), true
}
