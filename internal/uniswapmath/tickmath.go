package uniswapmath

import (
	"fmt"
	"math/big"
)

// Tick bounds, matching TickMath.sol.
const (
	MinTick = -887272
	MaxTick = 887272
)

// Sqrt-ratio bounds corresponding to MinTick and MaxTick.
var (
	MinSqrtRatio = big.NewInt(4295128739)
	MaxSqrtRatio = mustParse("1461446703485210103287273052203988822378723970342")
)

func mustParse(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("uniswapmath: bad constant " + s)
	}
	return v
}

func mustParseHex(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("uniswapmath: bad hex constant " + s)
	}
	return v
}

// The magic constants of TickMath.getSqrtRatioAtTick. Each is
// 2^128 / sqrt(1.0001)^(2^i), so multiplying the ones selected by the set bits
// of |tick| composes sqrt(1.0001)^-|tick| in a single pass — a binary
// exponentiation with the powers precomputed.
var tickRatios = []*big.Int{
	mustParseHex("fffcb933bd6fad37aa2d162d1a594001"), // bit 0
	mustParseHex("fff97272373d413259a46990580e213a"), // bit 1
	mustParseHex("fff2e50f5f656932ef12357cf3c7fdcc"),
	mustParseHex("ffe5caca7e10e4e61c3624eaa0941cd0"),
	mustParseHex("ffcb9843d60f6159c9db58835c926644"),
	mustParseHex("ff973b41fa98c081472e6896dfb254c0"),
	mustParseHex("ff2ea16466c96a3843ec78b326b52861"),
	mustParseHex("fe5dee046a99a2a811c461f1969c3053"),
	mustParseHex("fcbe86c7900a88aedcffc83b479aa3a4"),
	mustParseHex("f987a7253ac413176f2b074cf7815e54"),
	mustParseHex("f3392b0822b70005940c7a398e4b70f3"),
	mustParseHex("e7159475a2c29b7443b29c7fa6e889d9"),
	mustParseHex("d097f3bdfd2022b8845ad8f792aa5825"),
	mustParseHex("a9f746462d870fdf8a65dc1f90e061e5"),
	mustParseHex("70d869a156d2a1b890bb3df62baf32f7"),
	mustParseHex("31be135f97d08fd981231505542fcfa6"),
	mustParseHex("9aa508b5b7a84e1c677de54f3e99bc9"),
	mustParseHex("5d6af8dedb81196699c329225ee604"),
	mustParseHex("2216e584f5fa1ea926041bedfe98"),
	mustParseHex("48a170391f7dc42444e8fa2"), // bit 19
}

// SqrtRatioAtTick returns sqrt(1.0001^tick) * 2^96, matching
// TickMath.getSqrtRatioAtTick exactly.
func SqrtRatioAtTick(tick int) (*big.Int, error) {
	if tick < MinTick || tick > MaxTick {
		return nil, fmt.Errorf("uniswapmath: tick %d outside [%d, %d]", tick, MinTick, MaxTick)
	}

	abs := tick
	if abs < 0 {
		abs = -abs
	}

	// Start at 2^128, or at the bit-0 constant when |tick| is odd.
	ratio := new(big.Int)
	if abs&0x1 != 0 {
		ratio.Set(tickRatios[0])
	} else {
		ratio.Set(Q128)
	}

	for i := 1; i < len(tickRatios); i++ {
		if abs&(1<<uint(i)) != 0 {
			ratio.Mul(ratio, tickRatios[i])
			ratio.Rsh(ratio, 128)
		}
	}

	// Positive ticks are the reciprocal: the constants above encode negative
	// powers.
	if tick > 0 {
		ratio = new(big.Int).Div(MaxUint256, ratio)
	}

	// Downshift from Q128 to Q96, rounding up so the result is never below the
	// true ratio — the same direction TickMath takes.
	shifted := new(big.Int).Rsh(ratio, 32)
	if new(big.Int).And(ratio, big.NewInt(0xffffffff)).Sign() != 0 {
		shifted.Add(shifted, big.NewInt(1))
	}
	return shifted, nil
}

// TickAtSqrtRatio returns the greatest tick whose sqrt ratio does not exceed
// sqrtPriceX96 — the inverse of SqrtRatioAtTick, and what
// TickMath.getTickAtSqrtRatio is defined to compute.
//
// On-chain this is a hand-tuned log2 approximation with its own table of
// constants, because a contract cannot afford twenty iterations of anything.
// Off-chain that constraint does not apply, so this is a binary search over
// SqrtRatioAtTick instead. The result is identical by construction — it inverts
// the same function rather than approximating it — and there is no second table
// of magic numbers to get subtly wrong.
func TickAtSqrtRatio(sqrtPriceX96 *big.Int) (int, error) {
	if sqrtPriceX96 == nil || sqrtPriceX96.Cmp(MinSqrtRatio) < 0 || sqrtPriceX96.Cmp(MaxSqrtRatio) >= 0 {
		return 0, fmt.Errorf("uniswapmath: sqrt ratio %v outside [%v, %v)", sqrtPriceX96, MinSqrtRatio, MaxSqrtRatio)
	}

	lo, hi := MinTick, MaxTick
	for lo < hi {
		// Bias upward so the loop cannot stall when hi == lo+1.
		mid := lo + (hi-lo+1)/2
		ratio, err := SqrtRatioAtTick(mid)
		if err != nil {
			return 0, err
		}
		if ratio.Cmp(sqrtPriceX96) <= 0 {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, nil
}

// Amount0Delta returns the token0 required to move between two sqrt prices at a
// given liquidity: L * (sqrtB - sqrtA) * 2^96 / (sqrtB * sqrtA).
func Amount0Delta(sqrtA, sqrtB, liquidity *big.Int, roundUp bool) *big.Int {
	if sqrtA.Cmp(sqrtB) > 0 {
		sqrtA, sqrtB = sqrtB, sqrtA
	}
	if sqrtA.Sign() <= 0 {
		panic("uniswapmath: sqrt price must be positive")
	}

	numerator1 := new(big.Int).Lsh(liquidity, 96)
	numerator2 := new(big.Int).Sub(sqrtB, sqrtA)

	if roundUp {
		inner := MulDivRoundingUp(numerator1, numerator2, sqrtB)
		return DivRoundingUp(inner, sqrtA)
	}
	inner := MulDiv(numerator1, numerator2, sqrtB)
	return inner.Div(inner, sqrtA)
}

// Amount1Delta returns the token1 required to move between two sqrt prices at a
// given liquidity: L * (sqrtB - sqrtA) / 2^96.
func Amount1Delta(sqrtA, sqrtB, liquidity *big.Int, roundUp bool) *big.Int {
	if sqrtA.Cmp(sqrtB) > 0 {
		sqrtA, sqrtB = sqrtB, sqrtA
	}
	diff := new(big.Int).Sub(sqrtB, sqrtA)
	if roundUp {
		return MulDivRoundingUp(liquidity, diff, Q96)
	}
	return MulDiv(liquidity, diff, Q96)
}

// NextSqrtPriceFromAmount0 returns the sqrt price after adding amount of token0
// to a position with the given liquidity. Adding token0 lowers the price.
func NextSqrtPriceFromAmount0(sqrtPX96, liquidity, amount *big.Int) *big.Int {
	if amount.Sign() == 0 {
		return new(big.Int).Set(sqrtPX96)
	}
	numerator1 := new(big.Int).Lsh(liquidity, 96)
	product := new(big.Int).Mul(amount, sqrtPX96)
	denominator := new(big.Int).Add(numerator1, product)

	// The overflow guard from SqrtPriceMath: when the denominator wraps
	// on-chain the contract falls back to a form that cannot. Here big.Int
	// never wraps, but the fallback is kept so the rounding matches.
	if denominator.Cmp(numerator1) >= 0 {
		return MulDivRoundingUp(numerator1, sqrtPX96, denominator)
	}
	div := new(big.Int).Div(numerator1, sqrtPX96)
	return DivRoundingUp(numerator1, div.Add(div, amount))
}

// NextSqrtPriceFromAmount1 returns the sqrt price after adding amount of token1
// to a position with the given liquidity. Adding token1 raises the price.
func NextSqrtPriceFromAmount1(sqrtPX96, liquidity, amount *big.Int) *big.Int {
	quotient := MulDiv(amount, Q96, liquidity)
	return quotient.Add(quotient, sqrtPX96)
}
