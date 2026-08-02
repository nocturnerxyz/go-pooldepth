package pooldepth

import (
	"errors"
	"math/big"
	"testing"
)

func bigOf(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad literal %q", s)
	}
	return v
}

// The canonical Uniswap v2 formula, checked against a hand-computed value.
// reserves 1000/1000, fee 0.30%, input 100:
//
//	inAfterFee = 100 * 997000/1000000 = 99
//	out        = 1000 * 99 / (1000 + 99) = 99000/1099 = 90
func TestV2Quote_KnownValue(t *testing.T) {
	p, err := NewV2Pool(big.NewInt(1000), big.NewInt(1000), DefaultV2FeePips)
	if err != nil {
		t.Fatal(err)
	}

	q, err := p.Quote(big.NewInt(100), true)
	if err != nil {
		t.Fatal(err)
	}
	if q.AmountOut.Int64() != 90 {
		t.Errorf("AmountOut = %s, want 90", q.AmountOut)
	}
	if q.FeeAmount.Int64() != 1 {
		t.Errorf("FeeAmount = %s, want 1", q.FeeAmount)
	}
	if q.Partial {
		t.Error("constant product can never be exhausted, so a quote must never be partial")
	}
}

// The invariant that defines the curve: reserve0 * reserve1 never decreases.
func TestV2Quote_PreservesInvariant(t *testing.T) {
	r0 := bigOf(t, "1000000000000000000000")
	r1 := bigOf(t, "2500000000000000000000")
	p, err := NewV2Pool(r0, r1, DefaultV2FeePips)
	if err != nil {
		t.Fatal(err)
	}

	kBefore := new(big.Int).Mul(r0, r1)

	for _, size := range []string{"1000000000000000000", "50000000000000000000", "400000000000000000000"} {
		amountIn := bigOf(t, size)
		q, err := p.Quote(amountIn, true)
		if err != nil {
			t.Fatal(err)
		}
		newR0 := new(big.Int).Add(r0, amountIn)
		newR1 := new(big.Int).Sub(r1, q.AmountOut)
		kAfter := new(big.Int).Mul(newR0, newR1)

		if kAfter.Cmp(kBefore) < 0 {
			t.Errorf("size %s: k decreased from %s to %s", size, kBefore, kAfter)
		}
	}
}

// Bigger orders must cost strictly more per unit. A pricing function that is
// not monotonic here is broken in a way that unit values alone would miss.
func TestV2Quote_CostIsMonotonic(t *testing.T) {
	p, err := NewV2Pool(bigOf(t, "1000000000000000000000"), bigOf(t, "1000000000000000000000"), DefaultV2FeePips)
	if err != nil {
		t.Fatal(err)
	}

	prev := -1
	for _, size := range []string{
		"1000000000000000000",
		"10000000000000000000",
		"100000000000000000000",
		"500000000000000000000",
	} {
		q, err := p.Quote(bigOf(t, size), true)
		if err != nil {
			t.Fatal(err)
		}
		if q.ExecutionCostBps <= prev {
			t.Errorf("size %s cost %d bps, not above the previous %d", size, q.ExecutionCostBps, prev)
		}
		prev = q.ExecutionCostBps
	}
}

// Execution cost includes the fee; price impact does not. They should differ by
// roughly the fee rate on a small order, where impact is negligible.
func TestV2Quote_CostAndImpactDifferByTheFee(t *testing.T) {
	p, err := NewV2Pool(bigOf(t, "1000000000000000000000000"), bigOf(t, "1000000000000000000000000"), DefaultV2FeePips)
	if err != nil {
		t.Fatal(err)
	}

	// 0.0001% of the pool: impact is essentially nil, so cost ≈ the 30 bps fee.
	q, err := p.Quote(bigOf(t, "1000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}

	if q.ExecutionCostBps < 29 || q.ExecutionCostBps > 32 {
		t.Errorf("execution cost = %d bps, want ~30 (the fee) on a negligible order", q.ExecutionCostBps)
	}
	if q.PriceImpactBps > 2 {
		t.Errorf("price impact = %d bps, want ~0 on a negligible order", q.PriceImpactBps)
	}
	if q.ExecutionCostBps <= q.PriceImpactBps {
		t.Error("execution cost must exceed price impact: it includes the fee")
	}
}

func TestV2Quote_BothDirections(t *testing.T) {
	p, err := NewV2Pool(bigOf(t, "1000000000000000000000"), bigOf(t, "4000000000000000000000"), DefaultV2FeePips)
	if err != nil {
		t.Fatal(err)
	}

	amount := bigOf(t, "1000000000000000000")

	zeroForOne, err := p.Quote(amount, true)
	if err != nil {
		t.Fatal(err)
	}
	oneForZero, err := p.Quote(amount, false)
	if err != nil {
		t.Fatal(err)
	}

	// Selling token0 lowers the price; selling token1 raises it.
	if zeroForOne.SpotPriceX96After.Cmp(zeroForOne.SpotPriceX96Before) >= 0 {
		t.Error("selling token0 should lower the token1-per-token0 price")
	}
	if oneForZero.SpotPriceX96After.Cmp(oneForZero.SpotPriceX96Before) <= 0 {
		t.Error("selling token1 should raise the token1-per-token0 price")
	}

	// Costs are quoted in the same orientation, so both are positive.
	if zeroForOne.ExecutionCostBps <= 0 || oneForZero.ExecutionCostBps <= 0 {
		t.Errorf("costs should be positive in both directions: %d and %d",
			zeroForOne.ExecutionCostBps, oneForZero.ExecutionCostBps)
	}
}

// The closed form must agree with what Quote actually reports.
func TestV2DepthWithinBps_MatchesQuote(t *testing.T) {
	p, err := NewV2Pool(bigOf(t, "1000000000000000000000"), bigOf(t, "1000000000000000000000"), DefaultV2FeePips)
	if err != nil {
		t.Fatal(err)
	}

	for _, bps := range []int{50, 100, 250, 500} {
		depth, err := p.DepthWithinBps(bps, true)
		if err != nil {
			t.Fatal(err)
		}
		if depth.Sign() <= 0 {
			t.Fatalf("bps %d: depth is %s", bps, depth)
		}

		atDepth, err := p.Quote(depth, true)
		if err != nil {
			t.Fatal(err)
		}
		if atDepth.ExecutionCostBps > bps {
			t.Errorf("bps %d: depth %s costs %d bps, over budget", bps, depth, atDepth.ExecutionCostBps)
		}

		// Ten percent more must break the budget, or the answer was not the
		// maximum.
		bigger := new(big.Int).Mul(depth, big.NewInt(110))
		bigger.Div(bigger, big.NewInt(100))
		beyond, err := p.Quote(bigger, true)
		if err != nil {
			t.Fatal(err)
		}
		if beyond.ExecutionCostBps <= bps {
			t.Errorf("bps %d: 10%% above the reported depth still costs only %d bps", bps, beyond.ExecutionCostBps)
		}
	}
}

// The fee floor: a budget at or below the fee rate is unreachable at any size,
// because the fee is charged before a single unit of impact accrues. Reporting
// zero is the honest answer, and it surprises people.
func TestV2DepthWithinBps_FeeFloor(t *testing.T) {
	p, err := NewV2Pool(bigOf(t, "1000000000000000000000"), bigOf(t, "1000000000000000000000"), DefaultV2FeePips)
	if err != nil {
		t.Fatal(err)
	}

	for _, bps := range []int{1, 10, 29, 30} {
		depth, err := p.DepthWithinBps(bps, true)
		if err != nil {
			t.Fatal(err)
		}
		if depth.Sign() != 0 {
			t.Errorf("bps %d is at or below the 30 bps fee; depth should be 0, got %s", bps, depth)
		}
	}

	// One basis point above the fee, some size becomes possible.
	depth, err := p.DepthWithinBps(31, true)
	if err != nil {
		t.Fatal(err)
	}
	if depth.Sign() <= 0 {
		t.Errorf("31 bps is above the fee; depth should be positive, got %s", depth)
	}
}

// A fee-free pool has no floor at all.
func TestV2DepthWithinBps_ZeroFeePool(t *testing.T) {
	p, err := NewV2Pool(bigOf(t, "1000000000000000000000"), bigOf(t, "1000000000000000000000"), 0)
	if err != nil {
		t.Fatal(err)
	}

	depth, err := p.DepthWithinBps(1, true)
	if err != nil {
		t.Fatal(err)
	}
	if depth.Sign() <= 0 {
		t.Errorf("a fee-free pool should have positive depth even at 1 bps, got %s", depth)
	}
}

func TestV2DepthWithinBps_ScalesWithReserves(t *testing.T) {
	small, _ := NewV2Pool(bigOf(t, "1000000000000000000000"), bigOf(t, "1000000000000000000000"), DefaultV2FeePips)
	large, _ := NewV2Pool(bigOf(t, "10000000000000000000000"), bigOf(t, "10000000000000000000000"), DefaultV2FeePips)

	ds, err := small.DepthWithinBps(100, true)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := large.DepthWithinBps(100, true)
	if err != nil {
		t.Fatal(err)
	}

	// Ten times the reserves should give ten times the depth.
	want := new(big.Int).Mul(ds, big.NewInt(10))
	diff := new(big.Int).Sub(dl, want)
	diff.Abs(diff)
	tolerance := new(big.Int).Div(want, big.NewInt(1000))
	if diff.Cmp(tolerance) > 0 {
		t.Errorf("depth did not scale: small=%s large=%s, want ~%s", ds, dl, want)
	}
}

func TestNewV2Pool_Validation(t *testing.T) {
	tests := []struct {
		name   string
		r0, r1 *big.Int
		fee    uint32
	}{
		{"nil reserve0", nil, big.NewInt(1000), 3000},
		{"nil reserve1", big.NewInt(1000), nil, 3000},
		{"zero reserve", big.NewInt(0), big.NewInt(1000), 3000},
		{"negative reserve", big.NewInt(-1), big.NewInt(1000), 3000},
		{"fee at 100%", big.NewInt(1000), big.NewInt(1000), FeeDenominator},
		{"fee above 100%", big.NewInt(1000), big.NewInt(1000), FeeDenominator + 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewV2Pool(tc.r0, tc.r1, tc.fee); !errors.Is(err, ErrInvalidPool) {
				t.Errorf("got %v, want ErrInvalidPool", err)
			}
		})
	}
}

// A pool must never alias a caller's big.Int: reusing that value elsewhere
// would silently change the pool's own quotes.
func TestNewV2Pool_CopiesInputs(t *testing.T) {
	r0 := bigOf(t, "1000000000000000000000")
	r1 := bigOf(t, "1000000000000000000000")

	p, err := NewV2Pool(r0, r1, DefaultV2FeePips)
	if err != nil {
		t.Fatal(err)
	}
	before, err := p.Quote(bigOf(t, "1000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}

	r0.SetInt64(1) // the caller reuses their value
	after, err := p.Quote(bigOf(t, "1000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}

	if before.AmountOut.Cmp(after.AmountOut) != 0 {
		t.Errorf("mutating the caller's reserve changed the pool: %s then %s", before.AmountOut, after.AmountOut)
	}
}

func TestReserves_ReturnsCopies(t *testing.T) {
	p, _ := NewV2Pool(big.NewInt(1000), big.NewInt(2000), 0)
	r0, _ := p.Reserves()
	r0.SetInt64(1)

	again, _ := p.Reserves()
	if again.Int64() != 1000 {
		t.Errorf("Reserves leaked a mutable reference: %s", again)
	}
}

func TestQuote_RejectsBadAmounts(t *testing.T) {
	p, _ := NewV2Pool(big.NewInt(1000), big.NewInt(1000), 0)

	for _, amount := range []*big.Int{nil, big.NewInt(0), big.NewInt(-5)} {
		if _, err := p.Quote(amount, true); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("amount %v: got %v, want ErrInvalidAmount", amount, err)
		}
	}
}

func TestDepthWithinBps_RejectsBadBps(t *testing.T) {
	p, _ := NewV2Pool(big.NewInt(1000), big.NewInt(1000), 0)

	for _, bps := range []int{0, -1, BpsDenominator, BpsDenominator + 1} {
		if _, err := p.DepthWithinBps(bps, true); !errors.Is(err, ErrInvalidBps) {
			t.Errorf("bps %d: got %v, want ErrInvalidBps", bps, err)
		}
	}
}

func TestQuote_String(t *testing.T) {
	p, _ := NewV2Pool(big.NewInt(1000), big.NewInt(1000), DefaultV2FeePips)
	q, err := p.Quote(big.NewInt(100), true)
	if err != nil {
		t.Fatal(err)
	}
	if s := q.String(); s == "" {
		t.Error("String should render something")
	}
}
