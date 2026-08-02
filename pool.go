package pooldepth

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/nocturnerxyz/go-pooldepth/internal/uniswapmath"
)

// FeeDenominator is the scale fee rates are expressed in. Uniswap calls these
// "pips": 3000 is 0.30%, 500 is 0.05%, 10000 is 1.00%.
const FeeDenominator = 1_000_000

// BpsDenominator is the scale basis points are expressed in.
const BpsDenominator = 10_000

// Errors returned by this package. Match them with errors.Is.
var (
	// ErrInvalidPool means the pool state is internally inconsistent — a
	// negative reserve, an unsorted tick list, a zero sqrt price.
	ErrInvalidPool = errors.New("pooldepth: invalid pool state")

	// ErrInvalidAmount means the requested amount is nil, zero or negative.
	ErrInvalidAmount = errors.New("pooldepth: invalid amount")

	// ErrInsufficientLiquidity means the pool cannot absorb the whole order.
	// The quote returned alongside it describes how much *was* fillable, which
	// is usually the more interesting number: "this pool can only take $40k of
	// your $200k" is a risk signal, not just an error.
	ErrInsufficientLiquidity = errors.New("pooldepth: insufficient liquidity")

	// ErrInvalidBps means the requested basis-point budget is out of range.
	ErrInvalidBps = errors.New("pooldepth: basis points must be in (0, 10000)")
)

// Quote is the result of simulating a swap.
//
// Prices are expressed as token1 per token0 in Q96 fixed point, regardless of
// swap direction. Fixing one orientation means callers never have to invert
// anything to compare a buy against a sell.
type Quote struct {
	// AmountIn is the input actually consumed. On a partial fill it is less
	// than the amount requested.
	AmountIn *big.Int
	// AmountOut is the output received, net of fees.
	AmountOut *big.Int
	// FeeAmount is the portion of AmountIn taken as fees.
	FeeAmount *big.Int

	// SpotPriceX96Before and After are the pool's marginal prices either side
	// of the swap, as token1 per token0 in Q96.
	SpotPriceX96Before *big.Int
	SpotPriceX96After  *big.Int
	// ExecutionPriceX96 is the average price actually paid, same orientation.
	ExecutionPriceX96 *big.Int

	// ExecutionCostBps is the total cost against the pre-trade spot price,
	// fees included. This is what a trader means by "what does this cost me".
	ExecutionCostBps int
	// PriceImpactBps is how far the pool's marginal price moved, fees
	// excluded. This is what an LP or a risk model means by "impact".
	//
	// The two differ by roughly the fee rate, and conflating them is the most
	// common way a slippage number ends up wrong by exactly one fee tier.
	PriceImpactBps int

	// TickAfter is the pool tick after the swap. Zero for constant-product
	// pools, which have no ticks.
	TickAfter int
	// Partial reports that liquidity ran out before the full input was
	// consumed. AmountIn holds what was fillable.
	Partial bool
}

// String renders a quote for logs.
func (q *Quote) String() string {
	s := fmt.Sprintf("in=%s out=%s fee=%s cost=%dbps impact=%dbps",
		q.AmountIn, q.AmountOut, q.FeeAmount, q.ExecutionCostBps, q.PriceImpactBps)
	if q.Partial {
		s += " (partial)"
	}
	return s
}

// Pool is anything that can price a swap.
//
// Both constant-product (v2-style) and concentrated-liquidity (v3-style) pools
// implement it, so callers that only need depth and impact do not have to care
// which AMM they are looking at — which matters when the venue is not settled,
// or when a system has to span both.
type Pool interface {
	// Quote simulates swapping amountIn. zeroForOne means token0 in, token1 out.
	Quote(amountIn *big.Int, zeroForOne bool) (*Quote, error)

	// SpotPriceX96 returns the current marginal price as token1 per token0.
	SpotPriceX96() (*big.Int, error)

	// DepthWithinBps returns the largest input whose total execution cost,
	// fees included, stays within the given basis-point budget.
	DepthWithinBps(bps int, zeroForOne bool) (*big.Int, error)

	// Fee returns the pool fee in pips.
	Fee() uint32
}

// Compile-time confirmation that both pool types satisfy the interface.
var (
	_ Pool = (*V2Pool)(nil)
	_ Pool = (*V3Pool)(nil)
)

// bpsBetween returns |a - b| * 10000 / b, the difference from b to a in basis
// points. It returns 0 when b is zero rather than dividing by it: a pool with
// no price is not a pool with infinite slippage.
func bpsBetween(a, b *big.Int) int {
	if b == nil || b.Sign() == 0 || a == nil {
		return 0
	}
	diff := new(big.Int).Sub(a, b)
	diff.Abs(diff)
	diff.Mul(diff, big.NewInt(BpsDenominator))
	diff.Div(diff, b)

	// Clamp rather than overflow: a 3000% move is reported as the maximum, and
	// callers comparing against a budget behave correctly either way.
	if !diff.IsInt64() || diff.Int64() > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(diff.Int64())
}

// validateAmount rejects the inputs that would otherwise produce a meaningless
// quote.
func validateAmount(amount *big.Int) error {
	if amount == nil {
		return fmt.Errorf("%w: nil", ErrInvalidAmount)
	}
	if amount.Sign() <= 0 {
		return fmt.Errorf("%w: %s is not positive", ErrInvalidAmount, amount)
	}
	return nil
}

func validateBps(bps int) error {
	if bps <= 0 || bps >= BpsDenominator {
		return fmt.Errorf("%w: got %d", ErrInvalidBps, bps)
	}
	return nil
}

// Search bounds. These are limits on pathological input, not values a healthy
// pool approaches: 192 doublings covers any token supply that fits in 256 bits.
const (
	maxProbeDoublings = 192
	maxBisections     = 128
)

// searchDepth finds the largest input whose execution cost stays within bps.
//
// Concentrated liquidity has no closed form for this — cost is piecewise,
// changing slope at every tick boundary the swap crosses — so a search is the
// honest approach. It runs in two phases.
//
// Phase one finds the smallest size that produces any output at all. This is
// not a formality: with 18-decimal tokens, a one-wei probe rounds to zero
// output, and a naive search reads that as "infinitely expensive" and reports
// zero depth for a perfectly healthy pool. Starting from the smallest
// economically meaningful size avoids that whole failure mode.
//
// Phase two doubles to bracket the answer, then bisects. Both phases are
// bounded, so malformed pool data cannot make this spin.
func searchDepth(p Pool, bps int, zeroForOne bool) (*big.Int, error) {
	// within reports whether a size fits the budget. A size that exhausts the
	// pool never fits, regardless of its quoted cost.
	within := func(amount *big.Int) (ok bool, degenerate bool, err error) {
		q, err := p.Quote(amount, zeroForOne)
		if err != nil {
			switch {
			case errors.Is(err, ErrInsufficientLiquidity):
				return false, false, nil
			case errors.Is(err, ErrInvalidAmount):
				// Too small to produce output.
				return false, true, nil
			default:
				return false, false, err
			}
		}
		if q.AmountOut == nil || q.AmountOut.Sign() == 0 {
			return false, true, nil
		}
		if q.Partial {
			return false, false, nil
		}
		return q.ExecutionCostBps <= bps, false, nil
	}

	// Phase one: find a size out of the rounding-dominated regime.
	//
	// "Produces non-zero output" is not enough. At the smallest tradeable size
	// the output is a handful of units, so integer truncation of a single unit
	// is a large fraction of the trade, and the measured cost is dominated by
	// rounding rather than by the pool. Seeding there makes a healthy pool look
	// prohibitively expensive and reports zero depth.
	//
	// roundingFloor is the output size at which one unit of truncation is under
	// 0.01 bps of the trade — small enough that the measured cost is the pool's,
	// not the arithmetic's.
	roundingFloor := new(big.Int).Lsh(big.NewInt(1), 20)

	probe := big.NewInt(1)
	var seed *big.Int
	for i := 0; i < maxProbeDoublings; i++ {
		q, err := p.Quote(probe, zeroForOne)
		if err != nil {
			if !errors.Is(err, ErrInvalidAmount) && !errors.Is(err, ErrInsufficientLiquidity) {
				return nil, err
			}
			probe = new(big.Int).Lsh(probe, 1)
			continue
		}
		if q.AmountOut.Sign() == 0 {
			probe = new(big.Int).Lsh(probe, 1)
			continue
		}
		if q.Partial {
			// The pool is exhausted before the trade is even meaningful. If an
			// earlier probe worked, use it; otherwise there is no depth here.
			break
		}
		seed = new(big.Int).Set(probe)
		if q.AmountOut.Cmp(roundingFloor) >= 0 {
			break
		}
		probe = new(big.Int).Lsh(probe, 1)
	}
	if seed == nil {
		return big.NewInt(0), nil
	}

	ok, _, err := within(seed)
	if err != nil {
		return nil, err
	}
	if !ok {
		// The smallest economically meaningful size already blows the budget.
		// Usually this is the fee floor — a 10 bps budget in a 30 bps pool —
		// and zero is the correct, and genuinely useful, answer: no size at all
		// satisfies that budget.
		return big.NewInt(0), nil
	}
	probe = seed

	// Phase two: bracket by doubling.
	lo := new(big.Int).Set(probe)
	var hi *big.Int
	for i := 0; i < maxProbeDoublings; i++ {
		next := new(big.Int).Lsh(lo, 1)
		ok, degenerate, err := within(next)
		if err != nil {
			return nil, err
		}
		if !ok && !degenerate {
			hi = next
			break
		}
		lo = next
	}
	if hi == nil {
		// Nothing in reach exceeded the budget.
		return lo, nil
	}

	// Bisect. Invariant: lo is within budget, hi is not.
	one := big.NewInt(1)
	for i := 0; i < maxBisections; i++ {
		gap := new(big.Int).Sub(hi, lo)
		if gap.Cmp(one) <= 0 {
			break
		}
		mid := new(big.Int).Add(lo, gap.Rsh(gap, 1))
		ok, degenerate, err := within(mid)
		if err != nil {
			return nil, err
		}
		if ok && !degenerate {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// clone is re-exported from the internal package so pool constructors can
// defensively copy caller-supplied big.Ints.
func clone(x *big.Int) *big.Int { return uniswapmath.Clone(x) }
