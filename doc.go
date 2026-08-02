// Package pooldepth computes AMM price impact and depth in exact integer
// arithmetic.
//
// It answers the question every execution-risk system needs and few libraries
// provide: how much can be traded through this pool right now before it costs
// more than a given number of basis points.
//
// Both AMM shapes are supported behind one interface — constant product
// (Uniswap v2 and its many forks) and concentrated liquidity (Uniswap v3) —
// so callers do not have to care which one a venue runs, and a system spanning
// several venues does not need two code paths.
//
// # Cost versus impact
//
// Quote reports two numbers that are routinely conflated:
//
//   - ExecutionCostBps is what the trade costs against the pre-trade spot
//     price, fees included. This is what a trader means.
//   - PriceImpactBps is how far the pool's marginal price moved, fees
//     excluded. This is what an LP or a risk model means.
//
// They differ by roughly the fee rate. Reporting one where the other was meant
// produces a slippage figure that is wrong by exactly one fee tier — close
// enough to look right, and wrong in the direction that matters.
//
// # The fee floor
//
// DepthWithinBps returns zero when the requested budget is at or below the pool
// fee. This is not an error or a degenerate case: the fee is charged before a
// single unit of price impact accrues, so a 10 bps budget is unreachable in a
// 30 bps pool at any size. It surprises people, and returning zero honestly is
// better than returning a small number that implies otherwise.
//
// # Exactness
//
// Every calculation is integer arithmetic on math/big, mirroring the rounding
// direction the Solidity libraries use: inputs round up, outputs round down,
// always in the pool's favour. There is no floating point anywhere, because
// float silently diverges from on-chain results on exactly the large orders
// where a quote matters most.
//
// # Usage
//
//	pool, err := pooldepth.NewV3Pool(sqrtPriceX96, liquidity, tickSpacing, feePips, ticks)
//	if err != nil {
//	    return err
//	}
//
//	q, err := pool.Quote(amountIn, true) // token0 in, token1 out
//	if q.Partial {
//	    // the pool could only absorb q.AmountIn of the order
//	}
//
//	depth, err := pool.DepthWithinBps(50, true) // how much fits in 50 bps
//
// Pools are immutable once constructed and safe for concurrent use. Quoting
// simulates against a copy, so a cached pool never drifts from repeated use.
// Constructors defensively copy every big.Int argument, so a caller reusing
// their own values cannot silently change a pool's answers.
//
// # Scope
//
// This package does no I/O. The caller fetches pool state however they
// already do — RPC, subgraph, indexer, archive — and this computes against it.
// That keeps the package dependency-free and usable regardless of how chain
// data reaches you.
//
// Also out of scope: exact-output swaps, multi-hop routing, Uniswap v4 hooks
// (which can alter swap behaviour arbitrarily and cannot be modelled without
// the hook's own code), and fee-on-transfer tokens.
package pooldepth
