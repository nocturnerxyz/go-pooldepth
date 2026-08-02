package pooldepth

import (
	"errors"
	"math/big"
	"testing"

	"github.com/nocturnerxyz/go-pooldepth/internal/uniswapmath"
)

// sqrtAt is the sqrt price at a tick, for building test fixtures.
func sqrtAt(t *testing.T, tick int) *big.Int {
	t.Helper()
	r, err := uniswapmath.SqrtRatioAtTick(tick)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// symmetricPool builds a well-formed pool: liquidity L provisioned across
// [-width, +width] around tick 0, with liquidityNet summing to zero.
func symmetricPool(t *testing.T, width int, liquidity string, feePips uint32) *V3Pool {
	t.Helper()

	l := bigOf(t, liquidity)
	p, err := NewV3Pool(sqrtAt(t, 0), l, 60, feePips, []Tick{
		{Index: -width, LiquidityNet: new(big.Int).Set(l)},
		{Index: width, LiquidityNet: new(big.Int).Neg(l)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestV3Pool_DerivesTickFromPrice(t *testing.T) {
	for _, tick := range []int{-887220, -60000, -600, 0, 600, 60000, 887220} {
		p, err := NewV3Pool(sqrtAt(t, tick), big.NewInt(1_000_000), 60, 3000, nil)
		if err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if p.Tick() != tick {
			t.Errorf("pool at tick %d reports %d", tick, p.Tick())
		}
	}
}

func TestV3Quote_BasicSwap(t *testing.T) {
	p := symmetricPool(t, 600, "1000000000000000000", 3000)

	q, err := p.Quote(bigOf(t, "10000000000000000"), true) // ~1/3 of the range
	if err != nil {
		t.Fatal(err)
	}

	if q.AmountOut.Sign() <= 0 {
		t.Fatalf("AmountOut = %s", q.AmountOut)
	}
	if q.Partial {
		t.Error("this size should fit inside the provisioned range")
	}
	if q.FeeAmount.Sign() <= 0 {
		t.Error("a 30 bps pool should charge a fee")
	}
	if q.SpotPriceX96After.Cmp(q.SpotPriceX96Before) >= 0 {
		t.Error("selling token0 must lower the price")
	}
	if q.TickAfter >= 0 {
		t.Errorf("TickAfter = %d, expected a move below tick 0", q.TickAfter)
	}
	if q.ExecutionCostBps < 30 {
		t.Errorf("cost %d bps is below the 30 bps fee, which is impossible", q.ExecutionCostBps)
	}
}

func TestV3Quote_DirectionSymmetry(t *testing.T) {
	p := symmetricPool(t, 600, "1000000000000000000", 3000)
	amount := bigOf(t, "5000000000000000")

	down, err := p.Quote(amount, true)
	if err != nil {
		t.Fatal(err)
	}
	up, err := p.Quote(amount, false)
	if err != nil {
		t.Fatal(err)
	}

	if down.SpotPriceX96After.Cmp(down.SpotPriceX96Before) >= 0 {
		t.Error("token0 in must lower the price")
	}
	if up.SpotPriceX96After.Cmp(up.SpotPriceX96Before) <= 0 {
		t.Error("token1 in must raise the price")
	}
	// The pool is symmetric around tick 0, so equal-sized swaps should cost
	// about the same either way.
	if diff := down.ExecutionCostBps - up.ExecutionCostBps; diff > 2 || diff < -2 {
		t.Errorf("symmetric pool gave asymmetric costs: down=%d up=%d bps",
			down.ExecutionCostBps, up.ExecutionCostBps)
	}
}

// Quoting must not mutate the pool. This is the bug that makes a cached pool
// drift a little further from reality with every quote taken against it.
func TestV3Quote_IsPure(t *testing.T) {
	p := symmetricPool(t, 600, "1000000000000000000", 3000)
	amount := bigOf(t, "10000000000000000")

	first, err := p.Quote(amount, true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := p.Quote(amount, true)
		if err != nil {
			t.Fatal(err)
		}
		if again.AmountOut.Cmp(first.AmountOut) != 0 {
			t.Fatalf("quote %d returned %s, first returned %s — the pool is mutating",
				i, again.AmountOut, first.AmountOut)
		}
	}
	if p.Tick() != 0 {
		t.Errorf("pool tick moved to %d after quoting", p.Tick())
	}
}

// Concavity: doubling the input must return less than double the output.
// A linear result means the tick engine is not actually walking the curve.
func TestV3Quote_IsConcave(t *testing.T) {
	p := symmetricPool(t, 6000, "1000000000000000000", 3000)

	x := bigOf(t, "10000000000000000")
	q1, err := p.Quote(x, true)
	if err != nil {
		t.Fatal(err)
	}
	q2, err := p.Quote(new(big.Int).Lsh(x, 1), true)
	if err != nil {
		t.Fatal(err)
	}

	doubled := new(big.Int).Lsh(q1.AmountOut, 1)
	if q2.AmountOut.Cmp(doubled) >= 0 {
		t.Errorf("2x input returned %s, not less than 2x the output %s", q2.AmountOut, doubled)
	}
	if q2.ExecutionCostBps <= q1.ExecutionCostBps {
		t.Errorf("2x input cost %d bps, not more than %d", q2.ExecutionCostBps, q1.ExecutionCostBps)
	}
}

// Liquidity runs out at the edge of the provisioned range. Reporting how much
// *was* fillable is the useful part: "this pool can take 40k of your 200k".
func TestV3Quote_PartialFillAtRangeEdge(t *testing.T) {
	p := symmetricPool(t, 600, "1000000000000000000", 3000)

	// Far more than the range can absorb.
	huge := bigOf(t, "100000000000000000000")
	q, err := p.Quote(huge, true)
	if err != nil {
		t.Fatal(err)
	}

	if !q.Partial {
		t.Fatal("an order far exceeding the range should be partial")
	}
	if q.AmountIn.Cmp(huge) >= 0 {
		t.Errorf("AmountIn %s should be below the requested %s", q.AmountIn, huge)
	}
	if q.AmountIn.Sign() <= 0 {
		t.Error("a partial fill should still report what was fillable")
	}
	if q.AmountOut.Sign() <= 0 {
		t.Error("a partial fill should still return output")
	}
}

func TestV3Quote_NoLiquidityInDirection(t *testing.T) {
	// Liquidity only above the current price: selling token0 has nothing to
	// trade against.
	l := bigOf(t, "1000000000000000000")
	p, err := NewV3Pool(sqrtAt(t, 0), big.NewInt(0), 60, 3000, []Tick{
		{Index: 600, LiquidityNet: new(big.Int).Set(l)},
		{Index: 1200, LiquidityNet: new(big.Int).Neg(l)},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := p.Quote(bigOf(t, "1000000000000000"), true); !errors.Is(err, ErrInsufficientLiquidity) {
		t.Errorf("got %v, want ErrInsufficientLiquidity", err)
	}
}

// Crossing a tick must change the liquidity in force. A pool with extra
// liquidity waiting below must fill a large sell better than one without.
func TestV3Quote_CrossingTickAppliesLiquidityNet(t *testing.T) {
	l := bigOf(t, "1000000000000000000")
	extra := bigOf(t, "9000000000000000000")

	thin, err := NewV3Pool(sqrtAt(t, 0), l, 60, 3000, []Tick{
		{Index: -6000, LiquidityNet: new(big.Int).Set(l)},
		{Index: 6000, LiquidityNet: new(big.Int).Neg(l)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Identical, except a deep block of liquidity opens at tick -600.
	deep, err := NewV3Pool(sqrtAt(t, 0), l, 60, 3000, []Tick{
		{Index: -6000, LiquidityNet: new(big.Int).Add(l, extra)},
		{Index: -600, LiquidityNet: new(big.Int).Neg(extra)},
		{Index: 6000, LiquidityNet: new(big.Int).Neg(l)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Large enough to push past tick -600 and into the deeper band.
	amount := bigOf(t, "60000000000000000")

	thinQ, err := thin.Quote(amount, true)
	if err != nil {
		t.Fatal(err)
	}
	deepQ, err := deep.Quote(amount, true)
	if err != nil {
		t.Fatal(err)
	}

	if deepQ.AmountOut.Cmp(thinQ.AmountOut) <= 0 {
		t.Errorf("extra liquidity below the price should improve the fill: deep=%s thin=%s",
			deepQ.AmountOut, thinQ.AmountOut)
	}
	if deepQ.ExecutionCostBps >= thinQ.ExecutionCostBps {
		t.Errorf("extra liquidity should reduce cost: deep=%d thin=%d bps",
			deepQ.ExecutionCostBps, thinQ.ExecutionCostBps)
	}
	if deepQ.PriceImpactBps >= thinQ.PriceImpactBps {
		t.Errorf("extra liquidity should reduce impact: deep=%d thin=%d bps",
			deepQ.PriceImpactBps, thinQ.PriceImpactBps)
	}
}

// A gap in the provisioned range must be skipped without consuming input.
func TestV3Quote_SkipsLiquidityGap(t *testing.T) {
	l := bigOf(t, "1000000000000000000")

	// Liquidity in [-600, 0) and [-6000, -3000), with nothing in between.
	p, err := NewV3Pool(sqrtAt(t, -60), l, 60, 3000, []Tick{
		{Index: -6000, LiquidityNet: new(big.Int).Set(l)},
		{Index: -3000, LiquidityNet: new(big.Int).Neg(l)},
		{Index: -600, LiquidityNet: new(big.Int).Set(l)},
		{Index: 600, LiquidityNet: new(big.Int).Neg(l)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Enough to clear the first band, cross the empty gap, and reach the second.
	q, err := p.Quote(bigOf(t, "40000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	if q.AmountOut.Sign() <= 0 {
		t.Fatal("the swap should reach the far liquidity band")
	}
	if q.TickAfter > -3000 {
		t.Errorf("TickAfter = %d; the swap should have crossed the gap below -3000", q.TickAfter)
	}
}

func TestV3DepthWithinBps(t *testing.T) {
	p := symmetricPool(t, 6000, "1000000000000000000", 3000)

	for _, bps := range []int{50, 100, 300} {
		depth, err := p.DepthWithinBps(bps, true)
		if err != nil {
			t.Fatal(err)
		}
		if depth.Sign() <= 0 {
			t.Fatalf("bps %d: depth is %s", bps, depth)
		}

		at, err := p.Quote(depth, true)
		if err != nil {
			t.Fatal(err)
		}
		if at.ExecutionCostBps > bps {
			t.Errorf("bps %d: depth %s costs %d bps, over budget", bps, depth, at.ExecutionCostBps)
		}

		// Twice the reported depth must break the budget, or it was not the
		// maximum.
		beyond, err := p.Quote(new(big.Int).Lsh(depth, 1), true)
		if err == nil && !beyond.Partial && beyond.ExecutionCostBps <= bps {
			t.Errorf("bps %d: twice the reported depth still costs only %d bps", bps, beyond.ExecutionCostBps)
		}
	}
}

func TestV3DepthWithinBps_GrowsWithBudget(t *testing.T) {
	p := symmetricPool(t, 6000, "1000000000000000000", 3000)

	prev := big.NewInt(0)
	for _, bps := range []int{40, 60, 100, 200, 400} {
		depth, err := p.DepthWithinBps(bps, true)
		if err != nil {
			t.Fatal(err)
		}
		if depth.Cmp(prev) < 0 {
			t.Errorf("bps %d gave depth %s, below the previous %s", bps, depth, prev)
		}
		prev = depth
	}
}

// The fee floor applies to concentrated liquidity too.
func TestV3DepthWithinBps_FeeFloor(t *testing.T) {
	p := symmetricPool(t, 6000, "1000000000000000000", 3000)

	depth, err := p.DepthWithinBps(20, true) // below the 30 bps fee
	if err != nil {
		t.Fatal(err)
	}
	if depth.Sign() != 0 {
		t.Errorf("a 20 bps budget in a 30 bps pool should give zero depth, got %s", depth)
	}
}

func TestV3Validate(t *testing.T) {
	// A window rather than a complete pool: liquidityNet does not sum to zero.
	l := bigOf(t, "1000000000000000000")
	window, err := NewV3Pool(sqrtAt(t, 0), l, 60, 3000, []Tick{
		{Index: -600, LiquidityNet: new(big.Int).Set(l)},
	})
	if err != nil {
		t.Fatal(err)
	}
	problems := window.Validate()
	if len(problems) == 0 {
		t.Error("a tick window should be reported as such")
	}

	// A complete, healthy pool has nothing to report.
	if got := symmetricPool(t, 600, "1000000000000000000", 3000).Validate(); len(got) != 0 {
		t.Errorf("a well-formed pool reported problems: %v", got)
	}

	// Zero in-range liquidity is worth flagging: every quote will be partial.
	empty, err := NewV3Pool(sqrtAt(t, 0), big.NewInt(0), 60, 3000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Validate()) == 0 {
		t.Error("a pool with no liquidity and no ticks should report problems")
	}
}

func TestNewV3Pool_Validation(t *testing.T) {
	l := big.NewInt(1_000_000)
	good := sqrtAt(t, 0)

	tests := []struct {
		name    string
		sqrt    *big.Int
		liq     *big.Int
		spacing int
		fee     uint32
		ticks   []Tick
	}{
		{"nil sqrt price", nil, l, 60, 3000, nil},
		{"sqrt price below range", big.NewInt(1), l, 60, 3000, nil},
		{"sqrt price at upper bound", uniswapmath.MaxSqrtRatio, l, 60, 3000, nil},
		{"nil liquidity", good, nil, 60, 3000, nil},
		{"negative liquidity", good, big.NewInt(-1), 60, 3000, nil},
		{"zero tick spacing", good, l, 0, 3000, nil},
		{"negative tick spacing", good, l, -60, 3000, nil},
		{"fee at 100%", good, l, 60, FeeDenominator, nil},
		{"tick not aligned to spacing", good, l, 60, 3000, []Tick{{Index: 61, LiquidityNet: l}}},
		{"tick out of range", good, l, 60, 3000, []Tick{{Index: MaxTick + 60, LiquidityNet: l}}},
		{"nil liquidityNet", good, l, 60, 3000, []Tick{{Index: 60, LiquidityNet: nil}}},
		{"duplicate ticks", good, l, 60, 3000, []Tick{
			{Index: 60, LiquidityNet: l}, {Index: 60, LiquidityNet: l},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewV3Pool(tc.sqrt, tc.liq, tc.spacing, tc.fee, tc.ticks); !errors.Is(err, ErrInvalidPool) {
				t.Errorf("got %v, want ErrInvalidPool", err)
			}
		})
	}
}

// Ticks may be supplied in any order; the pool sorts them.
func TestNewV3Pool_SortsTicks(t *testing.T) {
	l := bigOf(t, "1000000000000000000")
	p, err := NewV3Pool(sqrtAt(t, 0), l, 60, 3000, []Tick{
		{Index: 600, LiquidityNet: new(big.Int).Neg(l)},
		{Index: -600, LiquidityNet: new(big.Int).Set(l)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Validate(); len(got) != 0 {
		t.Errorf("unsorted input should be accepted and sorted, got %v", got)
	}
	if _, err := p.Quote(bigOf(t, "1000000000000000"), true); err != nil {
		t.Errorf("quoting after sorting failed: %v", err)
	}
}

func TestNewV3Pool_CopiesInputs(t *testing.T) {
	l := bigOf(t, "1000000000000000000")
	sqrt := sqrtAt(t, 0)
	ticks := []Tick{
		{Index: -600, LiquidityNet: new(big.Int).Set(l)},
		{Index: 600, LiquidityNet: new(big.Int).Neg(l)},
	}

	p, err := NewV3Pool(sqrt, l, 60, 3000, ticks)
	if err != nil {
		t.Fatal(err)
	}
	before, err := p.Quote(bigOf(t, "1000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}

	// The caller reuses every value they passed in.
	l.SetInt64(1)
	sqrt.SetInt64(1)
	ticks[0].LiquidityNet.SetInt64(1)

	after, err := p.Quote(bigOf(t, "1000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	if before.AmountOut.Cmp(after.AmountOut) != 0 {
		t.Errorf("mutating caller values changed the pool: %s then %s", before.AmountOut, after.AmountOut)
	}
}

func TestV3Accessors_ReturnCopies(t *testing.T) {
	p := symmetricPool(t, 600, "1000000000000000000", 3000)

	l := p.Liquidity()
	l.SetInt64(1)
	if p.Liquidity().Cmp(bigOf(t, "1000000000000000000")) != 0 {
		t.Error("Liquidity leaked a mutable reference")
	}

	s := p.SqrtPriceX96()
	s.SetInt64(1)
	if p.SqrtPriceX96().Cmp(sqrtAt(t, 0)) != 0 {
		t.Error("SqrtPriceX96 leaked a mutable reference")
	}

	if p.TickSpacing() != 60 || p.Fee() != 3000 {
		t.Errorf("TickSpacing=%d Fee=%d", p.TickSpacing(), p.Fee())
	}
}

// Both pool types satisfy the same interface, so callers can hold either
// without caring which AMM they are looking at.
func TestPoolInterface_BothImplementations(t *testing.T) {
	v2, err := NewV2Pool(bigOf(t, "1000000000000000000000"), bigOf(t, "1000000000000000000000"), 3000)
	if err != nil {
		t.Fatal(err)
	}
	v3 := symmetricPool(t, 6000, "1000000000000000000", 3000)

	for _, p := range []Pool{v2, v3} {
		if _, err := p.SpotPriceX96(); err != nil {
			t.Errorf("SpotPriceX96: %v", err)
		}
		q, err := p.Quote(bigOf(t, "1000000000000000"), true)
		if err != nil {
			t.Errorf("Quote: %v", err)
			continue
		}
		if q.AmountOut.Sign() <= 0 {
			t.Error("no output")
		}
		if _, err := p.DepthWithinBps(100, true); err != nil {
			t.Errorf("DepthWithinBps: %v", err)
		}
		if p.Fee() != 3000 {
			t.Errorf("Fee = %d", p.Fee())
		}
	}
}

func BenchmarkV3Quote(b *testing.B) {
	l, _ := new(big.Int).SetString("1000000000000000000", 10)
	sqrt, _ := uniswapmath.SqrtRatioAtTick(0)

	var ticks []Tick
	for i := -6000; i <= 6000; i += 60 {
		if i == 0 {
			continue
		}
		net := big.NewInt(1_000_000_000)
		if i > 0 {
			net.Neg(net)
		}
		ticks = append(ticks, Tick{Index: i, LiquidityNet: net})
	}

	p, err := NewV3Pool(sqrt, l, 60, 3000, ticks)
	if err != nil {
		b.Fatal(err)
	}
	amount, _ := new(big.Int).SetString("10000000000000000", 10)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Quote(amount, true); err != nil {
			b.Fatal(err)
		}
	}
}

// Regression: when a swap exhausts the provisioned range, the reported
// post-trade price must be where liquidity actually ran out — not the floor of
// the representable price range.
//
// Walking on to the price limit through empty space inflated measured impact
// enormously and made an exhausted pool look infinitely movable, which in turn
// made AmountToMovePrice report that any target was reachable.
func TestV3Quote_ExhaustedSwapStopsWhereLiquidityEnds(t *testing.T) {
	// Liquidity only within roughly 6% of the current price.
	p := symmetricPool(t, 600, "1000000000000000000", 3000)

	q, err := p.Quote(bigOf(t, "100000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Partial {
		t.Fatal("an order far beyond the range should be partial")
	}

	// The range bottoms out around tick -600, which is about 5.8% below spot.
	// Anything much larger than that means the price walked through empty space.
	if q.PriceImpactBps > 700 {
		t.Errorf("impact %d bps exceeds what the provisioned range allows; "+
			"the price walked past where liquidity ended", q.PriceImpactBps)
	}
	if q.PriceImpactBps < 400 {
		t.Errorf("impact %d bps is below the range floor, so the swap stopped early", q.PriceImpactBps)
	}

	// And the post-trade price must sit at the edge of the range, not at the
	// bottom of the representable one.
	edge := sqrtAt(t, -600)
	edgeSpot := new(big.Int).Mul(edge, edge)
	edgeSpot.Rsh(edgeSpot, 96)
	if q.SpotPriceX96After.Cmp(edgeSpot) < 0 {
		t.Errorf("post-trade price %s is below the range edge %s", q.SpotPriceX96After, edgeSpot)
	}
}
