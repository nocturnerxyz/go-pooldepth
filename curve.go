package pooldepth

import (
	"math/big"
	"sort"
)

// DepthPoint is the tradeable size at one cost budget.
type DepthPoint struct {
	// Bps is the execution-cost budget this point was computed for.
	Bps int
	// Amount is the largest input that stays within it. Zero means no size
	// qualifies — see the fee floor in DepthWithinBps.
	Amount *big.Int
	// Quote is the simulated swap at Amount, or nil when Amount is zero.
	Quote *Quote
}

// DepthCurve returns the tradeable size at each of several cost budgets.
//
// This is the shape a depth chart or an impact simulator actually needs: not
// "how much fits in 50 bps" but the whole profile, because a pool that takes
// $2M at 10 bps and $2.1M at 500 bps is a completely different risk than one
// that takes $2M at 10 bps and $40M at 500 bps, and a single number cannot tell
// them apart.
//
// Levels are deduplicated and returned in ascending order regardless of the
// order given. Levels outside (0, 10000) are rejected.
//
// # Why this is cheaper than calling DepthWithinBps in a loop
//
// Depth is monotonically non-decreasing in the budget, so the answer at each
// level is a valid lower bound for the next. Sweeping upward threads that
// bound forward, which skips the probe phase for every level after the first
// and starts each bracket from a far tighter position. With concentrated
// liquidity, where each probe is a full tick traversal, that is the difference
// between one search and N.
func DepthCurve(p Pool, zeroForOne bool, bpsLevels ...int) ([]DepthPoint, error) {
	levels, err := normalizeLevels(bpsLevels)
	if err != nil {
		return nil, err
	}
	if len(levels) == 0 {
		return nil, nil
	}

	// A pool with a closed form is left alone: it is already exact and O(1),
	// and routing it through the search would return a slightly different
	// answer. ExecutionCostBps is an integer, so a band of sizes just above the
	// exact solution still reports the same truncated cost — a search happily
	// returns the top of that band while the closed form solves the equation.
	// Both are defensible, but a curve that disagreed with DepthWithinBps on
	// the same pool would be indefensible.
	searcher, incremental := p.(depthSearcher)

	out := make([]DepthPoint, 0, len(levels))
	var carry *big.Int // the previous level's answer, a valid floor for this one

	for _, bps := range levels {
		var amount *big.Int
		var err error
		if incremental {
			amount, err = searcher.depthFrom(bps, zeroForOne, carry)
		} else {
			amount, err = p.DepthWithinBps(bps, zeroForOne)
		}
		if err != nil {
			return nil, err
		}

		point := DepthPoint{Bps: bps, Amount: amount}
		if amount.Sign() > 0 {
			// Errors here are not fatal: the size was already validated by the
			// search, and a nil Quote is more useful than losing the curve.
			if q, err := p.Quote(amount, zeroForOne); err == nil {
				point.Quote = q
			}
			carry = amount
		}
		out = append(out, point)
	}

	return out, nil
}

// TotalDepth sums the tradeable size across several pools at one budget.
//
// This is an upper bound, not a routing result. Each pool is measured
// independently against its own pre-trade price, so the total is what could be
// absorbed if the order were split perfectly and executed simultaneously. Real
// execution is sequential and moves each pool as it goes, so the achievable
// figure is lower. It is still the right number for "is there enough liquidity
// in this name at all", which is a different question from "what will this
// specific order fill at".
func TotalDepth(pools []Pool, bps int, zeroForOne bool) (*big.Int, error) {
	if err := validateBps(bps); err != nil {
		return nil, err
	}

	total := new(big.Int)
	for _, p := range pools {
		if p == nil {
			continue
		}
		d, err := p.DepthWithinBps(bps, zeroForOne)
		if err != nil {
			return nil, err
		}
		total.Add(total, d)
	}
	return total, nil
}

// depthSearcher is implemented by pools whose depth is found by search rather
// than by a closed form, and which therefore benefit from a known-good floor.
type depthSearcher interface {
	depthFrom(bps int, zeroForOne bool, lowerBound *big.Int) (*big.Int, error)
}

func normalizeLevels(levels []int) ([]int, error) {
	seen := make(map[int]bool, len(levels))
	out := make([]int, 0, len(levels))

	for _, bps := range levels {
		if err := validateBps(bps); err != nil {
			return nil, err
		}
		if seen[bps] {
			continue
		}
		seen[bps] = true
		out = append(out, bps)
	}

	sort.Ints(out)
	return out, nil
}
