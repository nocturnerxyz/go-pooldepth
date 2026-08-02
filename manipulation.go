package pooldepth

import (
	"errors"
	"math/big"
)

// AmountToMovePrice returns the smallest input that moves the pool's marginal
// price by at least bps, along with the quote at that size.
//
// This is the cost of manipulation, and it is a different question from
// DepthWithinBps. Depth asks what a trade costs *you*; this asks what it costs
// to move the *price* — the number that says whether a pool can be pushed
// around. A pool where $3,000 moves the mark 5% is manipulable regardless of
// how tight its quotes look at size, and that gap is widest exactly when
// liquidity thins out overnight.
//
// It measures price impact, not execution cost, so the fee is excluded: a fee
// is paid to the pool and does not move the mark. Using execution cost here
// would make every pool look harder to move than it is, by exactly one fee tier.
//
// ErrInsufficientLiquidity is returned when the pool cannot be moved that far
// at all — for concentrated liquidity, the price can run out of provisioned
// range before reaching the target.
func AmountToMovePrice(p Pool, bps int, zeroForOne bool) (*big.Int, *Quote, error) {
	if err := validateBps(bps); err != nil {
		return nil, nil, err
	}

	// moves reports whether an amount shifts the marginal price far enough.
	// A partial fill means the pool ran out before getting there, which is the
	// same answer as "not far enough" for bracketing purposes but is tracked
	// separately so exhaustion can be reported honestly.
	type probeResult struct {
		reached   bool
		exhausted bool
		quote     *Quote
	}
	probe := func(amount *big.Int) (probeResult, error) {
		q, err := p.Quote(amount, zeroForOne)
		if err != nil {
			if errors.Is(err, ErrInsufficientLiquidity) {
				return probeResult{exhausted: true}, nil
			}
			if errors.Is(err, ErrInvalidAmount) {
				return probeResult{}, nil // too small to price
			}
			return probeResult{}, err
		}
		return probeResult{
			reached:   q.PriceImpactBps >= bps,
			exhausted: q.Partial,
			quote:     q,
		}, nil
	}

	// Grow until the target is reached or the pool is exhausted.
	amount := big.NewInt(1)
	var lo *big.Int // largest size known not to reach the target
	var hit *probeResult

	for i := 0; i < maxProbeDoublings; i++ {
		res, err := probe(amount)
		if err != nil {
			return nil, nil, err
		}
		if res.reached {
			hit = &res
			break
		}
		if res.exhausted {
			// The pool gave out before the price got there.
			return nil, nil, ErrInsufficientLiquidity
		}
		if res.quote != nil {
			lo = new(big.Int).Set(amount)
		}
		amount = new(big.Int).Lsh(amount, 1)
	}
	if hit == nil {
		return nil, nil, ErrInsufficientLiquidity
	}

	hi := new(big.Int).Set(amount)
	if lo == nil {
		lo = big.NewInt(0)
	}

	// Bisect for the smallest size that still reaches the target. Invariant:
	// lo does not reach it, hi does.
	one := big.NewInt(1)
	best := hit.quote

	for i := 0; i < maxBisections; i++ {
		gap := new(big.Int).Sub(hi, lo)
		if gap.Cmp(one) <= 0 {
			break
		}
		mid := new(big.Int).Add(lo, gap.Rsh(gap, 1))

		res, err := probe(mid)
		if err != nil {
			return nil, nil, err
		}
		if res.reached {
			hi = mid
			best = res.quote
		} else {
			lo = mid
		}
	}

	return hi, best, nil
}

// Manipulability compares the cost of moving a pool against a reference order
// size.
//
// The ratio is what makes the number comparable across pools and across time:
// "it costs $8,000 to move this 2%" means nothing without knowing what normal
// flow looks like, whereas "moving it 2% costs a fifth of a typical order"
// means quite a lot. Feeding that comparison a size drawn from the venue's own
// recent activity is what turns it into a signal.
//
// A ratio at or below 1 means an ordinary order can move the mark by the target
// amount, which is the shape of a pool worth being suspicious of.
type Manipulability struct {
	// TargetBps is the price move being priced.
	TargetBps int
	// Cost is the input required to achieve it.
	Cost *big.Int
	// Reference is the order size Cost is compared against.
	Reference *big.Int
	// RatioBps is Cost divided by Reference, in basis points. 10000 means
	// moving the price that far costs exactly one reference order.
	RatioBps int
	// Quote is the swap at Cost.
	Quote *Quote
}

// Manipulable reports whether a reference-sized order can move the price by the
// target amount.
func (m Manipulability) Manipulable() bool {
	return m.RatioBps > 0 && m.RatioBps <= BpsDenominator
}

// AssessManipulability prices a target price move against a reference order
// size.
func AssessManipulability(p Pool, targetBps int, reference *big.Int, zeroForOne bool) (*Manipulability, error) {
	if err := validateAmount(reference); err != nil {
		return nil, err
	}

	cost, quote, err := AmountToMovePrice(p, targetBps, zeroForOne)
	if err != nil {
		return nil, err
	}

	ratio := new(big.Int).Mul(cost, big.NewInt(BpsDenominator))
	ratio.Div(ratio, reference)

	m := &Manipulability{
		TargetBps: targetBps,
		Cost:      cost,
		Reference: new(big.Int).Set(reference),
		Quote:     quote,
	}
	if ratio.IsInt64() && ratio.Int64() <= int64(^uint(0)>>1) {
		m.RatioBps = int(ratio.Int64())
	} else {
		m.RatioBps = int(^uint(0) >> 1)
	}
	return m, nil
}
