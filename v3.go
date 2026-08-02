package pooldepth

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/nocturnerxyz/go-pooldepth/internal/uniswapmath"
)

// Tick bounds, re-exported so callers can validate without reaching into
// internals.
const (
	MinTick = uniswapmath.MinTick
	MaxTick = uniswapmath.MaxTick
)

// Tick is an initialized tick and the liquidity change that occurs when the
// price crosses it moving upward.
//
// LiquidityNet is signed: positive where a position begins, negative where one
// ends. Crossing downward applies the negation.
type Tick struct {
	Index        int
	LiquidityNet *big.Int
}

// V3Pool is a concentrated-liquidity pool.
//
// A V3Pool is immutable once constructed and safe for concurrent use: quoting
// simulates against a copy of the state rather than mutating it.
type V3Pool struct {
	sqrtPriceX96 *big.Int
	liquidity    *big.Int
	tick         int
	tickSpacing  int
	feePips      uint32
	ticks        []Tick
}

// NewV3Pool builds a pool from its slot0 state and the initialized ticks
// around the current price.
//
// The current tick is derived from sqrtPriceX96 rather than accepted as a
// parameter. slot0 reports both, and they must agree by definition; taking only
// one removes an entire class of "the caller passed a stale tick" bug that is
// otherwise invisible until a quote is subtly wrong.
//
// ticks need not cover the whole range. Supplying a window around the current
// price is normal and correct — quotes stay exact until the price would leave
// the window, at which point the result is reported as partial rather than
// silently extrapolated.
func NewV3Pool(sqrtPriceX96, liquidity *big.Int, tickSpacing int, feePips uint32, ticks []Tick) (*V3Pool, error) {
	if sqrtPriceX96 == nil || sqrtPriceX96.Cmp(uniswapmath.MinSqrtRatio) < 0 ||
		sqrtPriceX96.Cmp(uniswapmath.MaxSqrtRatio) >= 0 {
		return nil, fmt.Errorf("%w: sqrtPriceX96 %v outside the representable range", ErrInvalidPool, sqrtPriceX96)
	}
	if liquidity == nil || liquidity.Sign() < 0 {
		return nil, fmt.Errorf("%w: liquidity %v is negative or nil", ErrInvalidPool, liquidity)
	}
	if tickSpacing <= 0 {
		return nil, fmt.Errorf("%w: tick spacing %d must be positive", ErrInvalidPool, tickSpacing)
	}
	if feePips >= FeeDenominator {
		return nil, fmt.Errorf("%w: fee %d pips is not below 100%%", ErrInvalidPool, feePips)
	}

	tick, err := uniswapmath.TickAtSqrtRatio(sqrtPriceX96)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPool, err)
	}

	copied := make([]Tick, len(ticks))
	for i, t := range ticks {
		if t.Index < MinTick || t.Index > MaxTick {
			return nil, fmt.Errorf("%w: tick %d outside [%d, %d]", ErrInvalidPool, t.Index, MinTick, MaxTick)
		}
		if t.Index%tickSpacing != 0 {
			return nil, fmt.Errorf("%w: tick %d is not a multiple of spacing %d", ErrInvalidPool, t.Index, tickSpacing)
		}
		if t.LiquidityNet == nil {
			return nil, fmt.Errorf("%w: tick %d has nil LiquidityNet", ErrInvalidPool, t.Index)
		}
		copied[i] = Tick{Index: t.Index, LiquidityNet: clone(t.LiquidityNet)}
	}
	sort.Slice(copied, func(i, j int) bool { return copied[i].Index < copied[j].Index })

	for i := 1; i < len(copied); i++ {
		if copied[i].Index == copied[i-1].Index {
			return nil, fmt.Errorf("%w: duplicate tick %d", ErrInvalidPool, copied[i].Index)
		}
	}

	return &V3Pool{
		sqrtPriceX96: clone(sqrtPriceX96),
		liquidity:    clone(liquidity),
		tick:         tick,
		tickSpacing:  tickSpacing,
		feePips:      feePips,
		ticks:        copied,
	}, nil
}

// Tick returns the current tick, derived from the sqrt price.
func (p *V3Pool) Tick() int { return p.tick }

// TickSpacing returns the pool's tick spacing.
func (p *V3Pool) TickSpacing() int { return p.tickSpacing }

// Liquidity returns the in-range liquidity at the current price.
func (p *V3Pool) Liquidity() *big.Int { return clone(p.liquidity) }

// Fee returns the pool fee in pips.
func (p *V3Pool) Fee() uint32 { return p.feePips }

// SqrtPriceX96 returns the current sqrt price.
func (p *V3Pool) SqrtPriceX96() *big.Int { return clone(p.sqrtPriceX96) }

// SpotPriceX96 returns the marginal price as token1 per token0, in Q96.
//
// The sqrt price squared is a Q192 quantity, so it is shifted back down by 96
// rather than divided — the difference matters at the extremes of the range.
func (p *V3Pool) SpotPriceX96() (*big.Int, error) {
	sq := new(big.Int).Mul(p.sqrtPriceX96, p.sqrtPriceX96)
	return sq.Rsh(sq, 96), nil
}

// Validate reports structural problems that are not fatal to quoting but
// usually mean the tick data was assembled incorrectly.
//
// The main one is that liquidityNet across every initialized tick in a complete
// pool sums to zero: every position that opens also closes. A non-zero sum
// means the tick set is a window rather than the whole pool — which is fine and
// common, but worth knowing when you thought you had everything.
func (p *V3Pool) Validate() []string {
	var problems []string

	sum := new(big.Int)
	for _, t := range p.ticks {
		sum.Add(sum, t.LiquidityNet)
	}
	if sum.Sign() != 0 {
		problems = append(problems, fmt.Sprintf(
			"liquidityNet sums to %s rather than 0; the tick set is a window, not the complete pool", sum))
	}

	if p.liquidity.Sign() == 0 {
		problems = append(problems, "in-range liquidity is zero; every quote will be partial")
	}

	if len(p.ticks) == 0 {
		problems = append(problems, "no initialized ticks supplied; quotes cannot cross out of the current range")
	} else {
		if p.ticks[0].Index > p.tick {
			problems = append(problems, "no initialized tick at or below the current price; downward quotes will exhaust immediately")
		}
		if p.ticks[len(p.ticks)-1].Index <= p.tick {
			problems = append(problems, "no initialized tick above the current price; upward quotes will exhaust immediately")
		}
	}

	return problems
}

// swapState is the mutable state of a simulated swap.
type swapState struct {
	sqrtPriceX96    *big.Int
	tick            int
	liquidity       *big.Int
	amountRemaining *big.Int
	amountInUsed    *big.Int
	amountOut       *big.Int
	feePaid         *big.Int
}

// Quote simulates swapping amountIn across the pool's tick structure.
//
// Unlike constant product, concentrated liquidity genuinely runs out: past the
// last initialized tick there is nothing left to trade against. When that
// happens the quote is marked Partial and AmountIn reports how much was
// actually fillable, which is the number a risk system wants — "this pool can
// take 40k of your 200k order" is the signal, not the error.
func (p *V3Pool) Quote(amountIn *big.Int, zeroForOne bool) (*Quote, error) {
	if err := validateAmount(amountIn); err != nil {
		return nil, err
	}

	spotBefore, _ := p.SpotPriceX96()

	state := &swapState{
		sqrtPriceX96:    clone(p.sqrtPriceX96),
		tick:            p.tick,
		liquidity:       clone(p.liquidity),
		amountRemaining: clone(amountIn),
		amountInUsed:    new(big.Int),
		amountOut:       new(big.Int),
		feePaid:         new(big.Int),
	}

	limit := priceLimit(zeroForOne)

	// Every iteration either consumes input or crosses a tick, so the bound is
	// the tick count plus slack. It exists so malformed tick data cannot spin
	// the loop rather than because a healthy pool would approach it.
	maxSteps := 2*len(p.ticks) + 16

	for step := 0; state.amountRemaining.Sign() > 0 && step < maxSteps; step++ {
		if state.sqrtPriceX96.Cmp(limit) == 0 {
			break
		}

		nextTick, initialized := p.nextTick(state.tick, zeroForOne)
		sqrtTarget, err := uniswapmath.SqrtRatioAtTick(nextTick)
		if err != nil {
			return nil, fmt.Errorf("pooldepth: tick %d: %w", nextTick, err)
		}
		sqrtTarget = clampToLimit(sqrtTarget, limit, zeroForOne)

		if state.liquidity.Sign() == 0 {
			if !initialized {
				// Past the last provisioned tick with nothing left in range.
				// Walking on to the price limit would report the swap as having
				// pushed the price to the floor of the representable range,
				// when in truth trading simply stopped where the liquidity did.
				// That inflates measured impact enormously and makes an
				// exhausted pool look infinitely movable.
				break
			}
			// A gap between provisioned ranges: no trading happens here, so
			// jump to the boundary without consuming input.
			state.sqrtPriceX96 = sqrtTarget
		} else {
			sqrtNext, stepIn, stepOut, stepFee := computeSwapStep(
				state.sqrtPriceX96, sqrtTarget, state.liquidity,
				state.amountRemaining, p.feePips, zeroForOne,
			)

			consumed := new(big.Int).Add(stepIn, stepFee)
			state.amountRemaining.Sub(state.amountRemaining, consumed)
			state.amountInUsed.Add(state.amountInUsed, consumed)
			state.amountOut.Add(state.amountOut, stepOut)
			state.feePaid.Add(state.feePaid, stepFee)
			state.sqrtPriceX96 = sqrtNext

			// Nothing moved and nothing was consumed: without this the loop
			// would burn its whole step budget in place.
			if consumed.Sign() == 0 && sqrtNext.Cmp(sqrtTarget) != 0 {
				break
			}
		}

		if state.sqrtPriceX96.Cmp(sqrtTarget) == 0 {
			if initialized {
				p.cross(state, nextTick, zeroForOne)
			}
			// Step past the boundary so the next iteration looks at a new tick.
			if zeroForOne {
				state.tick = nextTick - 1
			} else {
				state.tick = nextTick
			}
		} else {
			t, err := uniswapmath.TickAtSqrtRatio(state.sqrtPriceX96)
			if err != nil {
				return nil, fmt.Errorf("pooldepth: %w", err)
			}
			state.tick = t
		}
	}

	if state.amountOut.Sign() <= 0 {
		if state.amountInUsed.Sign() == 0 {
			return nil, fmt.Errorf("%w: no liquidity available in this direction", ErrInsufficientLiquidity)
		}
		return nil, fmt.Errorf("%w: %s rounds to zero output", ErrInvalidAmount, amountIn)
	}

	spotAfter := new(big.Int).Mul(state.sqrtPriceX96, state.sqrtPriceX96)
	spotAfter.Rsh(spotAfter, 96)

	q := &Quote{
		AmountIn:           state.amountInUsed,
		AmountOut:          state.amountOut,
		FeeAmount:          state.feePaid,
		SpotPriceX96Before: spotBefore,
		SpotPriceX96After:  spotAfter,
		PriceImpactBps:     bpsBetween(spotAfter, spotBefore),
		TickAfter:          state.tick,
		Partial:            state.amountRemaining.Sign() > 0,
	}
	q.ExecutionPriceX96 = executionPriceX96(state.amountInUsed, state.amountOut, zeroForOne)
	q.ExecutionCostBps = bpsBetween(q.ExecutionPriceX96, spotBefore)

	return q, nil
}

// DepthWithinBps returns the largest input whose execution cost stays within
// the given budget.
//
// Concentrated liquidity has no closed form here: cost is piecewise, changing
// slope at every tick boundary the swap crosses. So this searches, and the
// search is deterministic and bounded — see searchDepth.
func (p *V3Pool) DepthWithinBps(bps int, zeroForOne bool) (*big.Int, error) {
	if err := validateBps(bps); err != nil {
		return nil, err
	}
	return searchDepth(p, bps, zeroForOne)
}

// depthFrom implements depthSearcher, letting DepthCurve thread each level's
// answer forward as a floor for the next.
func (p *V3Pool) depthFrom(bps int, zeroForOne bool, lowerBound *big.Int) (*big.Int, error) {
	if err := validateBps(bps); err != nil {
		return nil, err
	}
	return searchDepthFrom(p, bps, zeroForOne, lowerBound)
}

// nextTick returns the next initialized tick in the direction of travel, and
// whether it is a real initialized tick or the end of the supplied data.
func (p *V3Pool) nextTick(from int, zeroForOne bool) (tick int, initialized bool) {
	if zeroForOne {
		// Price falling: the greatest initialized tick at or below `from`.
		for i := len(p.ticks) - 1; i >= 0; i-- {
			if p.ticks[i].Index <= from {
				return p.ticks[i].Index, true
			}
		}
		return MinTick, false
	}

	// Price rising: the least initialized tick strictly above `from`.
	for i := 0; i < len(p.ticks); i++ {
		if p.ticks[i].Index > from {
			return p.ticks[i].Index, true
		}
	}
	return MaxTick, false
}

// cross applies a tick's liquidity change. Moving up adds liquidityNet; moving
// down subtracts it. Inverting this is the classic concentrated-liquidity bug:
// quotes stay plausible but drift further from reality with every tick crossed.
func (p *V3Pool) cross(state *swapState, tickIndex int, zeroForOne bool) {
	for _, t := range p.ticks {
		if t.Index != tickIndex {
			continue
		}
		if zeroForOne {
			state.liquidity.Sub(state.liquidity, t.LiquidityNet)
		} else {
			state.liquidity.Add(state.liquidity, t.LiquidityNet)
		}
		if state.liquidity.Sign() < 0 {
			// Malformed tick data can drive this negative. Clamping keeps the
			// simulation meaningful — the quote becomes partial rather than
			// nonsensical.
			state.liquidity.SetInt64(0)
		}
		return
	}
}

// computeSwapStep prices one segment of a swap, between the current price and
// either the next tick boundary or wherever the remaining input runs out.
//
// This mirrors SwapMath.computeSwapStep for the exact-input case, including its
// rounding: inputs round up and outputs round down, always in the pool's
// favour.
func computeSwapStep(
	sqrtCurrent, sqrtTarget, liquidity, amountRemaining *big.Int,
	feePips uint32, zeroForOne bool,
) (sqrtNext, amountIn, amountOut, feeAmount *big.Int) {
	feeComplement := big.NewInt(int64(FeeDenominator - feePips))

	// The fee is charged on the input, so only the remainder actually moves the
	// price.
	amountRemainingLessFee := new(big.Int).Mul(amountRemaining, feeComplement)
	amountRemainingLessFee.Div(amountRemainingLessFee, big.NewInt(FeeDenominator))

	var amountInToTarget *big.Int
	if zeroForOne {
		amountInToTarget = uniswapmath.Amount0Delta(sqrtTarget, sqrtCurrent, liquidity, true)
	} else {
		amountInToTarget = uniswapmath.Amount1Delta(sqrtCurrent, sqrtTarget, liquidity, true)
	}

	if amountRemainingLessFee.Cmp(amountInToTarget) >= 0 {
		// The input is enough to reach the boundary: the step ends there.
		sqrtNext = new(big.Int).Set(sqrtTarget)
		amountIn = amountInToTarget
		feeAmount = uniswapmath.MulDivRoundingUp(amountIn, big.NewInt(int64(feePips)), feeComplement)
	} else {
		// The input runs out first: the step ends mid-range.
		if zeroForOne {
			sqrtNext = uniswapmath.NextSqrtPriceFromAmount0(sqrtCurrent, liquidity, amountRemainingLessFee)
			if sqrtNext.Cmp(sqrtTarget) < 0 {
				sqrtNext = new(big.Int).Set(sqrtTarget)
			}
			amountIn = uniswapmath.Amount0Delta(sqrtNext, sqrtCurrent, liquidity, true)
		} else {
			sqrtNext = uniswapmath.NextSqrtPriceFromAmount1(sqrtCurrent, liquidity, amountRemainingLessFee)
			if sqrtNext.Cmp(sqrtTarget) > 0 {
				sqrtNext = new(big.Int).Set(sqrtTarget)
			}
			amountIn = uniswapmath.Amount1Delta(sqrtCurrent, sqrtNext, liquidity, true)
		}

		// Everything left over is fee. Rounding up amountIn can in principle
		// push it past what remains, so clamp rather than emit a negative fee.
		feeAmount = new(big.Int).Sub(amountRemaining, amountIn)
		if feeAmount.Sign() < 0 {
			amountIn = new(big.Int).Set(amountRemaining)
			feeAmount = new(big.Int)
		}
	}

	if zeroForOne {
		amountOut = uniswapmath.Amount1Delta(sqrtNext, sqrtCurrent, liquidity, false)
	} else {
		amountOut = uniswapmath.Amount0Delta(sqrtCurrent, sqrtNext, liquidity, false)
	}

	return sqrtNext, amountIn, amountOut, feeAmount
}

// priceLimit returns the furthest representable price in the direction of the
// swap. Uniswap requires a strict inequality against the bound, hence the ±1.
func priceLimit(zeroForOne bool) *big.Int {
	if zeroForOne {
		return new(big.Int).Add(uniswapmath.MinSqrtRatio, big.NewInt(1))
	}
	return new(big.Int).Sub(uniswapmath.MaxSqrtRatio, big.NewInt(1))
}

func clampToLimit(target, limit *big.Int, zeroForOne bool) *big.Int {
	if zeroForOne && target.Cmp(limit) < 0 {
		return new(big.Int).Set(limit)
	}
	if !zeroForOne && target.Cmp(limit) > 0 {
		return new(big.Int).Set(limit)
	}
	return target
}
