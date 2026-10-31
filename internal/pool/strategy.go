package pool

import (
	"math"
	"math/rand/v2"
)

type Strategy string

const (
	// StrategySpread sends each request to the least-loaded key. Among equally
	// idle keys it prefers the smallest remaining balance, draining remnants
	// before somebody else does. It uses every key concurrently, maximising
	// aggregate throughput when the keys are also consumed by other users.
	StrategySpread Strategy = "spread"
	// StrategyWeighted picks keys randomly, weighted by remaining balance and
	// damped by in-flight requests.
	StrategyWeighted Strategy = "weighted"
	// StrategyHighest always prefers the richest key (longest runway).
	StrategyHighest Strategy = "highest"
	// StrategyDrain spends the smallest usable balances first so remnants are
	// not stranded when somebody else drains the account.
	StrategyDrain Strategy = "drain"
	// StrategyRoundRobin cycles through eligible keys in configuration order.
	StrategyRoundRobin Strategy = "round_robin"
)

func ParseStrategy(s string) Strategy {
	switch Strategy(s) {
	case StrategyWeighted, StrategySpread, StrategyHighest, StrategyDrain, StrategyRoundRobin:
		return Strategy(s)
	default:
		return StrategySpread
	}
}

func balanceWeight(k *Key, penalty float64) float64 {
	b := 1.0
	if k.BalanceKnown {
		b = math.Max(k.Balance, 0.01)
	}
	return b / (1 + penalty*float64(k.InFlight))
}

func (p *Pool) chooseLocked(cands []*Key) *Key {
	if len(cands) == 1 {
		return cands[0]
	}
	switch p.strategy {
	case StrategyWeighted:
		total := 0.0
		for _, k := range cands {
			total += balanceWeight(k, p.opts.InflightPenalty)
		}
		if total <= 0 {
			return cands[rand.IntN(len(cands))]
		}
		r := rand.Float64() * total
		for _, k := range cands {
			r -= balanceWeight(k, p.opts.InflightPenalty)
			if r <= 0 {
				return k
			}
		}
		return cands[len(cands)-1]

	case StrategyHighest:
		best := cands[0]
		for _, k := range cands[1:] {
			if betterBalance(k, best) || (sameBalance(k, best) && k.InFlight < best.InFlight) {
				best = k
			}
		}
		return best

	case StrategyDrain:
		best := cands[0]
		for _, k := range cands[1:] {
			// Lower known balance wins. Unknown balances are treated as very
			// large, so they are drained last.
			if lowerBalanceForDrain(k, best) || (sameBalance(k, best) && k.InFlight < best.InFlight) {
				best = k
			}
		}
		return best

	case StrategyRoundRobin:
		idx := int(p.rr % uint64(len(cands)))
		p.rr++
		return cands[idx]

	case StrategySpread:
		fallthrough
	default:
		min := cands[0].InFlight
		for _, k := range cands[1:] {
			if k.InFlight < min {
				min = k.InFlight
			}
		}
		// Use all keys concurrently (least in-flight first), then drain the
		// smallest known balance first so external users cannot strand it.
		best := cands[0]
		found := false
		for _, k := range cands {
			if k.InFlight != min {
				continue
			}
			if !found {
				best = k
				found = true
				continue
			}
			if lowerBalanceForDrain(k, best) {
				best = k
			}
		}
		return best
	}
}

func betterBalance(a, b *Key) bool {
	if a.BalanceKnown != b.BalanceKnown {
		return a.BalanceKnown
	}
	if !a.BalanceKnown {
		return false
	}
	return a.Balance > b.Balance
}

func sameBalance(a, b *Key) bool {
	if a.BalanceKnown != b.BalanceKnown {
		return false
	}
	if !a.BalanceKnown {
		return true
	}
	return math.Abs(a.Balance-b.Balance) < 1e-9
}

func lowerBalanceForDrain(a, b *Key) bool {
	if a.BalanceKnown != b.BalanceKnown {
		return a.BalanceKnown // known balances drain before unknown ones
	}
	if !a.BalanceKnown {
		return false
	}
	return a.Balance < b.Balance
}
