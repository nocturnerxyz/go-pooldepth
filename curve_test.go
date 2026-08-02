package pooldepth

import (
	"errors"
	"math/big"
	"testing"
)

func TestDepthCurve(t *testing.T) {
	p := symmetricPool(t, 6000, "1000000000000000000", 3000)

	curve, err := DepthCurve(p, true, 300, 50, 100, 200)
	if err != nil {
		t.Fatal(err)
	}

	if len(curve) != 4 {
		t.Fatalf("got %d points, want 4", len(curve))
	}

	// Returned ascending regardless of the order given.
	want := []int{50, 100, 200, 300}
	for i, w := range want {
		if curve[i].Bps != w {
			t.Errorf("point %d has bps %d, want %d", i, curve[i].Bps, w)
		}
	}

	// Depth is monotonically non-decreasing in the budget.
	for i := 1; i < len(curve); i++ {
		if curve[i].Amount.Cmp(curve[i-1].Amount) < 0 {
			t.Errorf("depth fell from %s at %d bps to %s at %d bps",
				curve[i-1].Amount, curve[i-1].Bps, curve[i].Amount, curve[i].Bps)
		}
	}

	// Each point's quote must actually sit inside its own budget.
	for _, pt := range curve {
		if pt.Amount.Sign() == 0 {
			continue
		}
		if pt.Quote == nil {
			t.Errorf("%d bps: no quote attached", pt.Bps)
			continue
		}
		if pt.Quote.ExecutionCostBps > pt.Bps {
			t.Errorf("%d bps: quote costs %d bps, over budget", pt.Bps, pt.Quote.ExecutionCostBps)
		}
	}
}

// The sweep must not change the answer — only how fast it is reached.
func TestDepthCurve_MatchesIndividualCalls(t *testing.T) {
	for _, p := range []Pool{
		symmetricPool(t, 6000, "1000000000000000000", 3000),
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
	} {
		levels := []int{40, 60, 100, 250, 500, 1000}

		curve, err := DepthCurve(p, true, levels...)
		if err != nil {
			t.Fatal(err)
		}

		for _, pt := range curve {
			solo, err := p.DepthWithinBps(pt.Bps, true)
			if err != nil {
				t.Fatal(err)
			}
			// The bracket differs, so allow a hair of search tolerance; the two
			// must agree to well within a tenth of a percent.
			if !closeEnough(pt.Amount, solo, 1000) {
				t.Errorf("%d bps: curve gave %s, individual call gave %s", pt.Bps, pt.Amount, solo)
			}
		}
	}
}

func TestDepthCurve_DeduplicatesLevels(t *testing.T) {
	p := symmetricPool(t, 6000, "1000000000000000000", 3000)

	curve, err := DepthCurve(p, true, 100, 100, 50, 100, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(curve) != 2 {
		t.Fatalf("got %d points, want 2 after dedupe: %+v", len(curve), curve)
	}
}

func TestDepthCurve_FeeFloorPoints(t *testing.T) {
	p := symmetricPool(t, 6000, "1000000000000000000", 3000) // 30 bps fee

	curve, err := DepthCurve(p, true, 10, 20, 100)
	if err != nil {
		t.Fatal(err)
	}

	// Budgets below the fee are unreachable at any size.
	for _, pt := range curve[:2] {
		if pt.Amount.Sign() != 0 {
			t.Errorf("%d bps is below the 30 bps fee but reports depth %s", pt.Bps, pt.Amount)
		}
		if pt.Quote != nil {
			t.Errorf("%d bps: a zero-depth point should carry no quote", pt.Bps)
		}
	}
	// A zero point early in the sweep must not poison the levels after it.
	if curve[2].Amount.Sign() <= 0 {
		t.Error("100 bps should still have depth after two zero points")
	}
}

func TestDepthCurve_Validation(t *testing.T) {
	p := symmetricPool(t, 600, "1000000000000000000", 3000)

	for _, bad := range [][]int{{0}, {-1}, {10000}, {100, 0}} {
		if _, err := DepthCurve(p, true, bad...); !errors.Is(err, ErrInvalidBps) {
			t.Errorf("levels %v: got %v, want ErrInvalidBps", bad, err)
		}
	}

	curve, err := DepthCurve(p, true)
	if err != nil {
		t.Errorf("no levels should not be an error: %v", err)
	}
	if curve != nil {
		t.Errorf("no levels should give no points, got %+v", curve)
	}
}

func TestTotalDepth(t *testing.T) {
	a := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)
	b := mustV2(t, "2000000000000000000000", "2000000000000000000000", 3000)

	total, err := TotalDepth([]Pool{a, b}, 100, true)
	if err != nil {
		t.Fatal(err)
	}

	da, _ := a.DepthWithinBps(100, true)
	db, _ := b.DepthWithinBps(100, true)
	want := new(big.Int).Add(da, db)

	if total.Cmp(want) != 0 {
		t.Errorf("TotalDepth = %s, want %s", total, want)
	}
	// The deeper pool must contribute more, or the pools were not measured
	// independently.
	if db.Cmp(da) <= 0 {
		t.Errorf("the 2x pool contributed %s, not more than %s", db, da)
	}
}

func TestTotalDepth_MixedPoolTypes(t *testing.T) {
	v2 := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)
	v3 := symmetricPool(t, 6000, "1000000000000000000", 3000)

	total, err := TotalDepth([]Pool{v2, v3}, 200, true)
	if err != nil {
		t.Fatal(err)
	}
	if total.Sign() <= 0 {
		t.Errorf("TotalDepth across both AMM types = %s", total)
	}
}

func TestTotalDepth_SkipsNilAndValidates(t *testing.T) {
	p := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)

	total, err := TotalDepth([]Pool{p, nil}, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	solo, _ := p.DepthWithinBps(100, true)
	if total.Cmp(solo) != 0 {
		t.Errorf("a nil pool should contribute nothing: %s vs %s", total, solo)
	}

	if _, err := TotalDepth([]Pool{p}, 0, true); !errors.Is(err, ErrInvalidBps) {
		t.Errorf("got %v, want ErrInvalidBps", err)
	}

	empty, err := TotalDepth(nil, 100, true)
	if err != nil || empty.Sign() != 0 {
		t.Errorf("no pools should give zero, got %s (%v)", empty, err)
	}
}

func mustV2(t *testing.T, r0, r1 string, fee uint32) *V2Pool {
	t.Helper()
	p, err := NewV2Pool(bigOf(t, r0), bigOf(t, r1), fee)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// closeEnough reports whether a and b differ by less than 1/tolerance.
func closeEnough(a, b *big.Int, tolerance int64) bool {
	if a.Cmp(b) == 0 {
		return true
	}
	diff := new(big.Int).Sub(a, b)
	diff.Abs(diff)

	larger := a
	if b.Cmp(a) > 0 {
		larger = b
	}
	if larger.Sign() == 0 {
		return diff.Sign() == 0
	}
	diff.Mul(diff, big.NewInt(tolerance))
	return diff.Cmp(larger) <= 0
}

func BenchmarkDepthCurve(b *testing.B) {
	l, _ := new(big.Int).SetString("1000000000000000000", 10)
	sqrt, _ := new(big.Int).SetString("79228162514264337593543950336", 10)

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

	levels := []int{50, 100, 200, 300, 500, 1000}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DepthCurve(p, true, levels...); err != nil {
			b.Fatal(err)
		}
	}
}
