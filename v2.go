package pooldepth

import (
	"fmt"
	"math/big"

	"github.com/nocturnerxyz/go-pooldepth/internal/uniswapmath"
)

// DefaultV2FeePips is the 0.30% fee charged by canonical Uniswap v2 pools.
const DefaultV2FeePips uint32 = 3000

// V2Pool is a constant-product pool: reserve0 * reserve1 = k.
//
// A V2Pool is immutable once constructed and safe for concurrent use.
type V2Pool struct {
	reserve0 *big.Int
	reserve1 *big.Int
	feePips  uint32
}

// NewV2Pool returns a constant-product pool from its reserves.
//
// feePips is in millionths: 3000 is 0.30%. Pass 0 for a fee-free pool.
func NewV2Pool(reserve0, reserve1 *big.Int, feePips uint32) (*V2Pool, error) {
	if !uniswapmath.IsPositive(reserve0) || !uniswapmath.IsPositive(reserve1) {
		return nil, fmt.Errorf("%w: reserves must both be positive, got %v and %v",
			ErrInvalidPool, reserve0, reserve1)
	}
	if feePips >= FeeDenominator {
		return nil, fmt.Errorf("%w: fee %d pips is not below 100%%", ErrInvalidPool, feePips)
	}
	return &V2Pool{
		reserve0: clone(reserve0),
		reserve1: clone(reserve1),
		feePips:  feePips,
	}, nil
}

// Reserves returns copies of the pool reserves.
func (p *V2Pool) Reserves() (reserve0, reserve1 *big.Int) {
	return clone(p.reserve0), clone(p.reserve1)
}

// Fee returns the pool fee in pips.
func (p *V2Pool) Fee() uint32 { return p.feePips }

// SpotPriceX96 returns the marginal price as token1 per token0, in Q96.
func (p *V2Pool) SpotPriceX96() (*big.Int, error) {
	return uniswapmath.MulDiv(p.reserve1, uniswapmath.Q96, p.reserve0), nil
}

// Quote simulates a swap of amountIn.
//
// A constant-product pool can never be fully drained — the curve is asymptotic
// — so a quote here is never partial. That is a real difference from
// concentrated liquidity, where liquidity genuinely runs out at the edge of the
// provisioned range.
func (p *V2Pool) Quote(amountIn *big.Int, zeroForOne bool) (*Quote, error) {
	if err := validateAmount(amountIn); err != nil {
		return nil, err
	}

	reserveIn, reserveOut := p.reserve0, p.reserve1
	if !zeroForOne {
		reserveIn, reserveOut = p.reserve1, p.reserve0
	}

	// amountInAfterFee = amountIn * (1e6 - fee) / 1e6, floored — the fee is
	// rounded in the pool's favour, as on-chain.
	feeFactor := big.NewInt(int64(FeeDenominator - p.feePips))
	amountInAfterFee := new(big.Int).Mul(amountIn, feeFactor)
	amountInAfterFee.Div(amountInAfterFee, big.NewInt(FeeDenominator))
	feeAmount := new(big.Int).Sub(amountIn, amountInAfterFee)

	// amountOut = reserveOut * amountInAfterFee / (reserveIn + amountInAfterFee)
	numerator := new(big.Int).Mul(amountInAfterFee, reserveOut)
	denominator := new(big.Int).Add(reserveIn, amountInAfterFee)
	amountOut := numerator.Div(numerator, denominator)

	if amountOut.Sign() <= 0 {
		return nil, fmt.Errorf("%w: %s rounds to zero output", ErrInvalidAmount, amountIn)
	}

	spotBefore, _ := p.SpotPriceX96()

	// Post-trade reserves. The whole input enters the pool, including the fee:
	// in v2 the fee stays in the reserves rather than being skimmed off.
	newReserve0 := new(big.Int)
	newReserve1 := new(big.Int)
	if zeroForOne {
		newReserve0.Add(p.reserve0, amountIn)
		newReserve1.Sub(p.reserve1, amountOut)
	} else {
		newReserve1.Add(p.reserve1, amountIn)
		newReserve0.Sub(p.reserve0, amountOut)
	}
	spotAfter := uniswapmath.MulDiv(newReserve1, uniswapmath.Q96, newReserve0)

	q := &Quote{
		AmountIn:           clone(amountIn),
		AmountOut:          amountOut,
		FeeAmount:          feeAmount,
		SpotPriceX96Before: spotBefore,
		SpotPriceX96After:  spotAfter,
		PriceImpactBps:     bpsBetween(spotAfter, spotBefore),
	}
	q.ExecutionPriceX96 = executionPriceX96(amountIn, amountOut, zeroForOne)
	q.ExecutionCostBps = bpsBetween(q.ExecutionPriceX96, spotBefore)

	return q, nil
}

// DepthWithinBps returns the largest input whose execution cost stays within
// the given budget.
//
// Constant product has a closed form, so this is exact rather than searched:
//
//	depth = reserveIn * (bps*100 - feePips) * 10000 / ((1e6 - feePips) * (10000 - bps))
//
// The numerator makes the fee floor explicit. When the budget is at or below
// the fee rate — a 10 bps budget in a 30 bps pool — the term goes non-positive
// and the answer is zero: no size at all satisfies that budget, because the fee
// is charged before a single unit of price impact accrues. That is a real and
// frequently surprising property of AMMs, and it is worth returning honestly
// instead of quietly reporting some small non-zero number.
func (p *V2Pool) DepthWithinBps(bps int, zeroForOne bool) (*big.Int, error) {
	if err := validateBps(bps); err != nil {
		return nil, err
	}

	reserveIn := p.reserve0
	if !zeroForOne {
		reserveIn = p.reserve1
	}

	// bps*100 converts basis points onto the pip scale so the fee subtracts
	// directly.
	budgetPips := int64(bps)*100 - int64(p.feePips)
	if budgetPips <= 0 {
		return big.NewInt(0), nil
	}

	numerator := new(big.Int).Mul(reserveIn, big.NewInt(budgetPips))
	numerator.Mul(numerator, big.NewInt(BpsDenominator))

	denominator := new(big.Int).Mul(
		big.NewInt(int64(FeeDenominator-p.feePips)),
		big.NewInt(int64(BpsDenominator-bps)),
	)

	return numerator.Div(numerator, denominator), nil
}

// executionPriceX96 expresses the realised price as token1 per token0, in Q96,
// regardless of direction.
func executionPriceX96(amountIn, amountOut *big.Int, zeroForOne bool) *big.Int {
	if amountIn.Sign() == 0 || amountOut.Sign() == 0 {
		return big.NewInt(0)
	}
	if zeroForOne {
		// token0 in, token1 out: price = out/in.
		return uniswapmath.MulDiv(amountOut, uniswapmath.Q96, amountIn)
	}
	// token1 in, token0 out: price = in/out.
	return uniswapmath.MulDiv(amountIn, uniswapmath.Q96, amountOut)
}
