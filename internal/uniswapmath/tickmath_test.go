package uniswapmath

import (
	"math/big"
	"testing"
)

// The anchors that prove the magic constants were transcribed correctly. Tick 0
// exercises none of them, so it passes even with a corrupt table — the boundary
// ticks are the real check, since they set every bit.
func TestSqrtRatioAtTick_Anchors(t *testing.T) {
	tests := []struct {
		tick int
		want string
	}{
		{0, "79228162514264337593543950336"}, // 2^96
		{MinTick, "4295128739"},
		{MaxTick, "1461446703485210103287273052203988822378723970342"},
	}

	for _, tc := range tests {
		got, err := SqrtRatioAtTick(tc.tick)
		if err != nil {
			t.Fatalf("tick %d: %v", tc.tick, err)
		}
		want, _ := new(big.Int).SetString(tc.want, 10)
		if got.Cmp(want) != 0 {
			t.Errorf("SqrtRatioAtTick(%d) =\n  %s\nwant\n  %s", tc.tick, got, want)
		}
	}
}

func TestSqrtRatioAtTick_Monotonic(t *testing.T) {
	prev, err := SqrtRatioAtTick(MinTick)
	if err != nil {
		t.Fatal(err)
	}
	// Full sweep is 1.7M ticks; step through enough to catch a bad constant in
	// any bit position without making the suite slow.
	for tick := MinTick + 1; tick <= MaxTick; tick += 997 {
		got, err := SqrtRatioAtTick(tick)
		if err != nil {
			t.Fatal(err)
		}
		if got.Cmp(prev) <= 0 {
			t.Fatalf("sqrt ratio not increasing at tick %d: %s <= %s", tick, got, prev)
		}
		prev = got
	}
}

// Each tick is a 1.0001x price step, so the sqrt ratio should rise by
// sqrt(1.0001) ≈ 1.00005 per tick. This catches a constant that is right in
// magnitude but wrong in value.
func TestSqrtRatioAtTick_StepSize(t *testing.T) {
	for _, base := range []int{-100000, -1000, 0, 1000, 100000} {
		a, err := SqrtRatioAtTick(base)
		if err != nil {
			t.Fatal(err)
		}
		b, err := SqrtRatioAtTick(base + 1)
		if err != nil {
			t.Fatal(err)
		}

		// ratio = b/a, scaled by 1e9 to compare as integers.
		scaled := new(big.Int).Mul(b, big.NewInt(1e9))
		scaled.Div(scaled, a)

		const wantLow, wantHigh = 1000049998, 1000050002 // sqrt(1.0001) ≈ 1.000049998
		if scaled.Int64() < wantLow || scaled.Int64() > wantHigh {
			t.Errorf("tick %d -> %d step ratio %v/1e9, want ~1.000050", base, base+1, scaled)
		}
	}
}

func TestSqrtRatioAtTick_RejectsOutOfRange(t *testing.T) {
	for _, tick := range []int{MinTick - 1, MaxTick + 1, -1 << 30, 1 << 30} {
		if _, err := SqrtRatioAtTick(tick); err == nil {
			t.Errorf("tick %d should be rejected", tick)
		}
	}
}

// TickAtSqrtRatio must invert SqrtRatioAtTick exactly.
func TestTickAtSqrtRatio_RoundTrip(t *testing.T) {
	for tick := MinTick; tick <= MaxTick-1; tick += 641 {
		ratio, err := SqrtRatioAtTick(tick)
		if err != nil {
			t.Fatal(err)
		}
		got, err := TickAtSqrtRatio(ratio)
		if err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if got != tick {
			t.Fatalf("round trip: tick %d -> ratio %s -> tick %d", tick, ratio, got)
		}
	}
}

// A ratio between two ticks must resolve to the lower one: the contract is
// "greatest tick whose ratio does not exceed the input".
func TestTickAtSqrtRatio_RoundsDown(t *testing.T) {
	for _, tick := range []int{-50000, -100, 0, 100, 50000} {
		lo, err := SqrtRatioAtTick(tick)
		if err != nil {
			t.Fatal(err)
		}
		hi, err := SqrtRatioAtTick(tick + 1)
		if err != nil {
			t.Fatal(err)
		}
		mid := new(big.Int).Add(lo, hi)
		mid.Div(mid, big.NewInt(2))

		got, err := TickAtSqrtRatio(mid)
		if err != nil {
			t.Fatal(err)
		}
		if got != tick {
			t.Errorf("ratio between tick %d and %d resolved to %d, want %d", tick, tick+1, got, tick)
		}
	}
}

func TestTickAtSqrtRatio_RejectsOutOfRange(t *testing.T) {
	below := new(big.Int).Sub(MinSqrtRatio, big.NewInt(1))
	if _, err := TickAtSqrtRatio(below); err == nil {
		t.Error("a ratio below MinSqrtRatio should be rejected")
	}
	if _, err := TickAtSqrtRatio(MaxSqrtRatio); err == nil {
		t.Error("MaxSqrtRatio is exclusive and should be rejected")
	}
	if _, err := TickAtSqrtRatio(nil); err == nil {
		t.Error("nil should be rejected rather than panicking")
	}
}

func TestMulDiv(t *testing.T) {
	tests := []struct {
		a, b, d int64
		want    int64
		wantUp  int64
	}{
		{10, 10, 3, 33, 34},
		{10, 10, 5, 20, 20}, // exact: rounding up changes nothing
		{0, 5, 3, 0, 0},
		{7, 1, 2, 3, 4},
	}

	for _, tc := range tests {
		got := MulDiv(big.NewInt(tc.a), big.NewInt(tc.b), big.NewInt(tc.d))
		if got.Int64() != tc.want {
			t.Errorf("MulDiv(%d,%d,%d) = %s, want %d", tc.a, tc.b, tc.d, got, tc.want)
		}
		gotUp := MulDivRoundingUp(big.NewInt(tc.a), big.NewInt(tc.b), big.NewInt(tc.d))
		if gotUp.Int64() != tc.wantUp {
			t.Errorf("MulDivRoundingUp(%d,%d,%d) = %s, want %d", tc.a, tc.b, tc.d, gotUp, tc.wantUp)
		}
	}
}

// The 512-bit intermediate that forces Solidity into FullMath must not lose
// precision here.
func TestMulDiv_LargeIntermediate(t *testing.T) {
	big1 := new(big.Int).Lsh(big.NewInt(1), 200)
	big2 := new(big.Int).Lsh(big.NewInt(1), 200)
	d := new(big.Int).Lsh(big.NewInt(1), 200)

	got := MulDiv(big1, big2, d)
	if got.Cmp(big1) != 0 {
		t.Errorf("MulDiv(2^200, 2^200, 2^200) = %s, want 2^200", got)
	}
}

func TestMulDiv_PanicsOnZeroDenominator(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected a panic on a zero denominator")
		}
	}()
	MulDiv(big.NewInt(1), big.NewInt(1), big.NewInt(0))
}

// Amount deltas must round in the direction that favours the pool: inputs up,
// outputs down.
func TestAmountDeltas_RoundingDirection(t *testing.T) {
	a, _ := SqrtRatioAtTick(0)
	b, _ := SqrtRatioAtTick(60)
	liquidity := big.NewInt(1_000_000_007) // prime, to force a remainder

	up0 := Amount0Delta(a, b, liquidity, true)
	down0 := Amount0Delta(a, b, liquidity, false)
	if up0.Cmp(down0) < 0 {
		t.Errorf("Amount0Delta rounding up (%s) is below rounding down (%s)", up0, down0)
	}
	if new(big.Int).Sub(up0, down0).Cmp(big.NewInt(1)) > 0 {
		t.Errorf("rounding should differ by at most 1: up=%s down=%s", up0, down0)
	}

	up1 := Amount1Delta(a, b, liquidity, true)
	down1 := Amount1Delta(a, b, liquidity, false)
	if up1.Cmp(down1) < 0 {
		t.Errorf("Amount1Delta rounding up (%s) is below rounding down (%s)", up1, down1)
	}
}

func TestAmountDeltas_OrderIndependent(t *testing.T) {
	a, _ := SqrtRatioAtTick(-120)
	b, _ := SqrtRatioAtTick(120)
	l := big.NewInt(1e18)

	if Amount0Delta(a, b, l, false).Cmp(Amount0Delta(b, a, l, false)) != 0 {
		t.Error("Amount0Delta should not depend on argument order")
	}
	if Amount1Delta(a, b, l, false).Cmp(Amount1Delta(b, a, l, false)) != 0 {
		t.Error("Amount1Delta should not depend on argument order")
	}
}

// Adding token0 lowers the price; adding token1 raises it. Getting these
// backwards inverts every quote.
func TestNextSqrtPrice_Direction(t *testing.T) {
	p, _ := SqrtRatioAtTick(0)
	l := big.NewInt(1e18)
	amount := big.NewInt(1e15)

	lower := NextSqrtPriceFromAmount0(p, l, amount)
	if lower.Cmp(p) >= 0 {
		t.Errorf("adding token0 should lower the price: %s -> %s", p, lower)
	}

	higher := NextSqrtPriceFromAmount1(p, l, amount)
	if higher.Cmp(p) <= 0 {
		t.Errorf("adding token1 should raise the price: %s -> %s", p, higher)
	}
}

func TestNextSqrtPrice_ZeroAmountIsIdentity(t *testing.T) {
	p, _ := SqrtRatioAtTick(1000)
	l := big.NewInt(1e18)

	if got := NextSqrtPriceFromAmount0(p, l, big.NewInt(0)); got.Cmp(p) != 0 {
		t.Errorf("zero token0 moved the price to %s", got)
	}
	if got := NextSqrtPriceFromAmount1(p, l, big.NewInt(0)); got.Cmp(p) != 0 {
		t.Errorf("zero token1 moved the price to %s", got)
	}
}

// Moving the price with an amount, then measuring the amount needed for that
// move, must agree to within rounding.
func TestNextSqrtPrice_ConsistentWithAmountDelta(t *testing.T) {
	p, _ := SqrtRatioAtTick(0)
	l, _ := new(big.Int).SetString("100000000000000000000", 10)
	amount := big.NewInt(10_000_000_000_000_000)

	next := NextSqrtPriceFromAmount0(p, l, amount)
	needed := Amount0Delta(next, p, l, true)

	diff := new(big.Int).Sub(needed, amount)
	diff.Abs(diff)
	if diff.Cmp(big.NewInt(2)) > 0 {
		t.Errorf("round trip off by %s: amount=%s needed=%s", diff, amount, needed)
	}
}

func TestClone(t *testing.T) {
	orig := big.NewInt(42)
	c := Clone(orig)
	c.SetInt64(99)
	if orig.Int64() != 42 {
		t.Error("Clone returned an alias, not a copy")
	}
	if Clone(nil) != nil {
		t.Error("Clone(nil) should be nil")
	}
}
