// Package uniswapmath implements the fixed-point arithmetic Uniswap performs
// on-chain, in exact integer form.
//
// Every function here mirrors a Solidity library. The rounding direction of
// each is part of its contract, not an implementation detail: on-chain, inputs
// round up and outputs round down so the pool can never be drained by
// accumulated dust. A port that rounds the other way produces quotes that drift
// from reality by a wei here and there — which is invisible on small orders and
// materially wrong on large ones, exactly where quotes matter.
package uniswapmath

import "math/big"

// Q96 is 2^96, the fixed-point scale Uniswap v3 uses for sqrt prices.
var Q96 = new(big.Int).Lsh(big.NewInt(1), 96)

// Q128 is 2^128.
var Q128 = new(big.Int).Lsh(big.NewInt(1), 128)

// MaxUint256 is 2^256 - 1.
var MaxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// MulDiv computes floor(a * b / denominator).
//
// Solidity needs FullMath.mulDiv to survive a 512-bit intermediate; big.Int has
// no such limit, so the intermediate is simply exact.
func MulDiv(a, b, denominator *big.Int) *big.Int {
	if denominator.Sign() == 0 {
		panic("uniswapmath: division by zero")
	}
	product := new(big.Int).Mul(a, b)
	return product.Div(product, denominator)
}

// MulDivRoundingUp computes ceil(a * b / denominator).
func MulDivRoundingUp(a, b, denominator *big.Int) *big.Int {
	if denominator.Sign() == 0 {
		panic("uniswapmath: division by zero")
	}
	product := new(big.Int).Mul(a, b)
	quotient, remainder := new(big.Int).QuoRem(product, denominator, new(big.Int))
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

// DivRoundingUp computes ceil(a / b).
func DivRoundingUp(a, b *big.Int) *big.Int {
	quotient, remainder := new(big.Int).QuoRem(a, b, new(big.Int))
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

// Clone returns a copy, so callers cannot mutate values we hold.
//
// Every exported entry point in this module clones its *big.Int inputs before
// storing them. A pool struct that aliases a caller's big.Int will silently
// change its own quotes when that caller reuses the value — a bug that only
// appears under reuse and is miserable to find.
func Clone(x *big.Int) *big.Int {
	if x == nil {
		return nil
	}
	return new(big.Int).Set(x)
}

// IsPositive reports whether x is non-nil and greater than zero.
func IsPositive(x *big.Int) bool {
	return x != nil && x.Sign() > 0
}
