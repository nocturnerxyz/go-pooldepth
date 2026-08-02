# go-pooldepth

AMM price impact and depth in exact integer math — "how much can I trade before it costs more than 50 bps?"

[![CI](https://github.com/nocturnerxyz/go-pooldepth/actions/workflows/ci.yml/badge.svg)](https://github.com/nocturnerxyz/go-pooldepth/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/nocturnerxyz/go-pooldepth.svg)](https://pkg.go.dev/github.com/nocturnerxyz/go-pooldepth)

**Zero dependencies.** Nothing outside the standard library. No RPC, no I/O.

```bash
go get github.com/nocturnerxyz/go-pooldepth
```

Supports **both** AMM shapes behind one interface:

- **Constant product** (`V2Pool`) — Uniswap v2 and its forks
- **Concentrated liquidity** (`V3Pool`) — Uniswap v3, with full tick traversal

## Usage

```go
pool, err := pooldepth.NewV3Pool(sqrtPriceX96, liquidity, tickSpacing, feePips, ticks)

q, err := pool.Quote(amountIn, true) // token0 in, token1 out
fmt.Println(q.AmountOut, q.ExecutionCostBps, q.PriceImpactBps)

if q.Partial {
    // the pool could only absorb q.AmountIn of the order —
    // usually a more useful signal than an error
}

depth, err := pool.DepthWithinBps(50, true) // largest size fitting in 50 bps
```

Constructors copy every `big.Int` argument, so reusing your own values can't silently change a pool's answers. Pools are immutable and safe for concurrent use — quoting simulates against a copy, so a cached pool never drifts from repeated use.

## Two numbers people conflate

`Quote` reports both, because they are not the same thing:

| Field | Fees | Means |
|---|---|---|
| `ExecutionCostBps` | **included** | What the trade costs vs pre-trade spot. What a *trader* means. |
| `PriceImpactBps` | excluded | How far the pool's marginal price moved. What an *LP or risk model* means. |

They differ by roughly the fee rate. Reporting one where the other was meant gives a slippage figure wrong by exactly one fee tier — close enough to look right, wrong in the direction that matters.

## Depth curves

```go
curve, err := pooldepth.DepthCurve(pool, true, 10, 50, 100, 500)
total, err := pooldepth.TotalDepth([]pooldepth.Pool{poolA, poolB}, 100, true)
```

A single depth number can't distinguish a pool that takes $2M at 10 bps and $2.1M at 500 bps from one that takes $2M at 10 bps and $40M at 500 bps. Those are completely different risks.

Depth is monotonic in the budget, so `DepthCurve` sweeps upward and threads each level's answer forward as a floor for the next — skipping the probe phase for every level after the first. With concentrated liquidity, where each probe is a full tick traversal, that's the difference between one search and N. Pools with a closed form (v2) keep using it rather than being routed through the search.

## The fee floor

`DepthWithinBps` returns **zero** when the budget is at or below the pool fee.

That is not a bug or a rounding artifact. The fee is charged before a single unit of price impact accrues, so a **10 bps budget is unreachable in a 30 bps pool at any size**. This surprises people regularly. Returning zero honestly beats returning a small number that implies the budget is achievable.

For constant product there's a closed form that makes it explicit:

```
depth = reserveIn * (bps*100 - feePips) * 10000 / ((1e6 - feePips) * (10000 - bps))
```

When `bps*100 <= feePips`, the numerator goes non-positive. That term *is* the fee floor.

## Cost to move the price

```go
cost, quote, err := pooldepth.AmountToMovePrice(pool, 200, true) // what moves the mark 2%?
m, err := pooldepth.AssessManipulability(pool, 200, typicalOrderSize, true)
if m.Manipulable() { /* an ordinary order can move this mark */ }
```

A different question from `DepthWithinBps`. Depth asks what a trade costs **you**; this asks what it costs to move the **price**. A pool where $3,000 moves the mark 5% is manipulable no matter how tight its quotes look at size — and that gap is widest exactly when liquidity thins out overnight.

It measures price impact, so the fee is excluded: a fee is paid to the pool and doesn't move the mark. This also means it has **no fee floor** — a 10 bps price move is reachable in a 30 bps pool, even though `DepthWithinBps(10)` correctly returns zero.

`AssessManipulability` divides that cost by a reference order size, because "$8,000 to move it 2%" means nothing without knowing what normal flow looks like.

## Splitting an order

```go
res, err := pooldepth.Split([]pooldepth.Pool{poolA, poolB, poolC}, amountIn, true)
// res.Allocations, res.BlendedCostBps, res.WorstCostBps, res.Unfilled
```

Finds the smallest cost budget at which the pools together absorb the order, then gives each pool what it can take within it. That's the equal-marginal-cost allocation — the condition for total cost to be minimised — so the "send more to the deep pool" behaviour falls out of the depths rather than being hand-tuned.

`WorstCostBps` is reported separately from `BlendedCostBps` because a split can look cheap on average while one leg is terrible. `Unfilled` is a risk signal, not a failure: "this name can take 40k of your 200k".

This is a static plan computed against pre-trade state. Real execution is sequential and moves each pool as it goes, so realised cost is somewhat higher.

## Exactness

Everything is integer arithmetic on `math/big`, mirroring the rounding direction of the Solidity libraries: **inputs round up, outputs round down**, always in the pool's favour.

No floating point anywhere. Float silently diverges from on-chain results on exactly the large orders where a quote matters most.

The v3 tick math is a direct port of `TickMath` and `SwapMath`, verified against the on-chain anchors: `sqrtRatioAtTick(0) == 2^96`, `sqrtRatioAtTick(MIN_TICK) == 4295128739`, and the corresponding maximum. `TickAtSqrtRatio` is a **binary search over `SqrtRatioAtTick`** rather than a port of the on-chain log2 approximation — off-chain there's no gas budget to optimise against, and inverting the real function beats approximating it with a second table of magic numbers to get subtly wrong.

## Getting pool state

This package does no I/O by design. Fetch state however you already do — RPC, subgraph, indexer, archive node — and pass it in:

- **v2**: `reserve0`, `reserve1` from `getReserves()`, plus the fee
- **v3**: `sqrtPriceX96` and `liquidity` from `slot0()`/`liquidity()`, plus initialized ticks

The current tick is **derived** from `sqrtPriceX96` rather than accepted as a parameter. `slot0` reports both and they agree by definition; taking only one removes a whole class of stale-tick bug that's otherwise invisible until a quote is subtly wrong.

Tick data need not cover the whole range — a window around the current price is normal. Quotes stay exact until the price would leave the window, at which point the result is `Partial` rather than silently extrapolated. `Validate()` tells you whether you supplied a window or a complete pool.

## Scope

Out of scope: exact-**output** swaps, multi-hop routing, **Uniswap v4 hooks** (which can alter swap behaviour arbitrarily and can't be modelled without the hook's own code), and fee-on-transfer tokens.

## Contributing

CI runs `gofmt`, `go vet`, `staticcheck`, and `go test -race` on the two most recent Go releases.

The highest-value contribution here is **differential testing against real on-chain quotes**: capture pool state at a block, quote against the deployed Quoter, and assert this library matches exactly. Discrepancies are bugs and we want them.

## License

Apache-2.0 — see [LICENSE](LICENSE).
