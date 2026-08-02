package pooldepth

import (
	"errors"
	"math/big"
	"testing"
)

func TestAmountToMovePrice(t *testing.T) {
	p := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)

	for _, bps := range []int{50, 100, 500, 1000} {
		cost, quote, err := AmountToMovePrice(p, bps, true)
		if err != nil {
			t.Fatalf("%d bps: %v", bps, err)
		}
		if cost.Sign() <= 0 {
			t.Fatalf("%d bps: cost is %s", bps, cost)
		}
		if quote.PriceImpactBps < bps {
			t.Errorf("%d bps: the reported cost only moves the price %d bps", bps, quote.PriceImpactBps)
		}

		// One unit less must fall short, or it was not the smallest size.
		smaller := new(big.Int).Sub(cost, big.NewInt(1))
		if smaller.Sign() > 0 {
			q, err := p.Quote(smaller, true)
			if err == nil && q.PriceImpactBps >= bps {
				t.Errorf("%d bps: a smaller size also reaches the target, so %s is not minimal", bps, cost)
			}
		}
	}
}

// Moving further must cost more. A non-monotonic result would mean the search
// is finding local answers rather than the boundary.
func TestAmountToMovePrice_IsMonotonic(t *testing.T) {
	p := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)

	prev := big.NewInt(0)
	for _, bps := range []int{10, 50, 100, 250, 500, 1000, 2000} {
		cost, _, err := AmountToMovePrice(p, bps, true)
		if err != nil {
			t.Fatalf("%d bps: %v", bps, err)
		}
		if cost.Cmp(prev) <= 0 {
			t.Errorf("moving %d bps costs %s, not more than the previous %s", bps, cost, prev)
		}
		prev = cost
	}
}

// This is what makes it a different question from DepthWithinBps: price impact
// excludes the fee, so a pool has a non-zero answer at budgets that Depth
// reports as zero.
func TestAmountToMovePrice_UnaffectedByTheFeeFloor(t *testing.T) {
	p := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000) // 30 bps fee

	depth, err := p.DepthWithinBps(10, true)
	if err != nil {
		t.Fatal(err)
	}
	if depth.Sign() != 0 {
		t.Fatalf("fixture wrong: a 10 bps budget in a 30 bps pool should give zero depth, got %s", depth)
	}

	// The price can still be moved 10 bps — the fee is paid to the pool and
	// does not move the mark.
	cost, quote, err := AmountToMovePrice(p, 10, true)
	if err != nil {
		t.Fatalf("moving the price 10 bps should be possible: %v", err)
	}
	if cost.Sign() <= 0 {
		t.Errorf("cost = %s", cost)
	}
	if quote.PriceImpactBps < 10 {
		t.Errorf("PriceImpactBps = %d, want at least 10", quote.PriceImpactBps)
	}
	// And it costs materially less than the execution-cost framing implies.
	if quote.ExecutionCostBps <= quote.PriceImpactBps {
		t.Error("execution cost should exceed price impact: it includes the fee")
	}
}

// A deeper pool must be harder to move — that is the whole signal.
func TestAmountToMovePrice_DeeperPoolCostsMore(t *testing.T) {
	thin := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)
	deep := mustV2(t, "10000000000000000000000", "10000000000000000000000", 3000)

	thinCost, _, err := AmountToMovePrice(thin, 200, true)
	if err != nil {
		t.Fatal(err)
	}
	deepCost, _, err := AmountToMovePrice(deep, 200, true)
	if err != nil {
		t.Fatal(err)
	}

	if deepCost.Cmp(thinCost) <= 0 {
		t.Errorf("the 10x pool costs %s to move, not more than the thin pool's %s", deepCost, thinCost)
	}

	// Roughly 10x, since the pools are the same shape.
	ratio := new(big.Int).Mul(deepCost, big.NewInt(100))
	ratio.Div(ratio, thinCost)
	if ratio.Int64() < 900 || ratio.Int64() > 1100 {
		t.Errorf("cost ratio %v/100, want roughly 1000", ratio)
	}
}

// Concentrated liquidity genuinely runs out, so a large target can be
// unreachable — and saying so is better than returning the biggest size that
// happened to fit.
func TestAmountToMovePrice_ExhaustedPool(t *testing.T) {
	// Liquidity only within about 6% of the current price.
	p := symmetricPool(t, 600, "1000000000000000000", 3000)

	if _, _, err := AmountToMovePrice(p, 9000, true); !errors.Is(err, ErrInsufficientLiquidity) {
		t.Errorf("got %v, want ErrInsufficientLiquidity for an unreachable target", err)
	}

	// A target inside the provisioned range is fine.
	if _, _, err := AmountToMovePrice(p, 100, true); err != nil {
		t.Errorf("a reachable target failed: %v", err)
	}
}

func TestAmountToMovePrice_BothAMMTypes(t *testing.T) {
	for _, p := range []Pool{
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
		symmetricPool(t, 6000, "1000000000000000000", 3000),
	} {
		for _, dir := range []bool{true, false} {
			cost, quote, err := AmountToMovePrice(p, 100, dir)
			if err != nil {
				t.Errorf("zeroForOne=%v: %v", dir, err)
				continue
			}
			if cost.Sign() <= 0 || quote.PriceImpactBps < 100 {
				t.Errorf("zeroForOne=%v: cost=%s impact=%d", dir, cost, quote.PriceImpactBps)
			}
		}
	}
}

func TestAmountToMovePrice_Validation(t *testing.T) {
	p := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)

	for _, bps := range []int{0, -1, BpsDenominator, BpsDenominator + 1} {
		if _, _, err := AmountToMovePrice(p, bps, true); !errors.Is(err, ErrInvalidBps) {
			t.Errorf("bps %d: got %v, want ErrInvalidBps", bps, err)
		}
	}
}

// A ratio is what makes the cost comparable across pools: "$8,000 to move it
// 2%" means nothing without knowing what a normal order looks like.
func TestAssessManipulability(t *testing.T) {
	thin := mustV2(t, "100000000000000000000", "100000000000000000000", 3000)
	deep := mustV2(t, "100000000000000000000000", "100000000000000000000000", 3000)
	reference := bigOf(t, "2000000000000000000") // a typical order for this venue

	thinAssessment, err := AssessManipulability(thin, 200, reference, true)
	if err != nil {
		t.Fatal(err)
	}
	deepAssessment, err := AssessManipulability(deep, 200, reference, true)
	if err != nil {
		t.Fatal(err)
	}

	if thinAssessment.RatioBps >= deepAssessment.RatioBps {
		t.Errorf("the thin pool should have the lower ratio: thin=%d deep=%d",
			thinAssessment.RatioBps, deepAssessment.RatioBps)
	}
	if !thinAssessment.Manipulable() {
		t.Errorf("a reference order moves the thin pool 2%%, ratio %d bps — that is manipulable",
			thinAssessment.RatioBps)
	}
	if deepAssessment.Manipulable() {
		t.Errorf("the deep pool should not be movable by one reference order, ratio %d bps",
			deepAssessment.RatioBps)
	}

	if thinAssessment.TargetBps != 200 {
		t.Errorf("TargetBps = %d", thinAssessment.TargetBps)
	}
	if thinAssessment.Quote == nil {
		t.Error("the quote at the cost should be attached")
	}
}

// The reference must be copied, or a caller reusing their value silently
// changes a stored assessment.
func TestAssessManipulability_CopiesReference(t *testing.T) {
	p := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)
	reference := bigOf(t, "1000000000000000000")

	m, err := AssessManipulability(p, 100, reference, true)
	if err != nil {
		t.Fatal(err)
	}
	reference.SetInt64(1)

	if m.Reference.Cmp(bigOf(t, "1000000000000000000")) != 0 {
		t.Errorf("Reference was aliased: %s", m.Reference)
	}
}

func TestAssessManipulability_Validation(t *testing.T) {
	p := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)

	for _, ref := range []*big.Int{nil, big.NewInt(0), big.NewInt(-1)} {
		if _, err := AssessManipulability(p, 100, ref, true); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("reference %v: got %v, want ErrInvalidAmount", ref, err)
		}
	}
}
