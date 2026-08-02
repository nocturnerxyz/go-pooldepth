package pooldepth

import (
	"errors"
	"math/big"
	"testing"
)

func TestSplit_AllocatesWholeOrder(t *testing.T) {
	pools := []Pool{
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
		mustV2(t, "2000000000000000000000", "2000000000000000000000", 3000),
	}
	amount := bigOf(t, "5000000000000000000")

	res, err := Split(pools, amount, true)
	if err != nil {
		t.Fatal(err)
	}

	if !res.Complete() {
		t.Errorf("order should fill completely, unfilled %s", res.Unfilled)
	}
	if res.Filled.Cmp(amount) != 0 {
		t.Errorf("Filled = %s, want exactly %s", res.Filled, amount)
	}

	// The parts must sum to the whole — no units invented, none lost.
	sum := new(big.Int)
	for _, a := range res.Allocations {
		sum.Add(sum, a.Amount)
	}
	if sum.Cmp(amount) != 0 {
		t.Errorf("allocations sum to %s, want %s", sum, amount)
	}
	if res.TotalOut.Sign() <= 0 {
		t.Errorf("TotalOut = %s", res.TotalOut)
	}
}

// The deeper pool must take the larger share, and it must fall out of the
// depths rather than being hand-tuned.
func TestSplit_DeeperPoolTakesMore(t *testing.T) {
	shallow := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)
	deep := mustV2(t, "5000000000000000000000", "5000000000000000000000", 3000)

	res, err := Split([]Pool{shallow, deep}, bigOf(t, "10000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Allocations) != 2 {
		t.Fatalf("got %d allocations, want 2", len(res.Allocations))
	}

	byIndex := map[int]*big.Int{}
	for _, a := range res.Allocations {
		byIndex[a.PoolIndex] = a.Amount
	}
	if byIndex[1].Cmp(byIndex[0]) <= 0 {
		t.Errorf("the 5x pool took %s, not more than the shallow pool's %s", byIndex[1], byIndex[0])
	}

	// Roughly 5:1, since both pools have the same shape and fee.
	ratio := new(big.Int).Mul(byIndex[1], big.NewInt(100))
	ratio.Div(ratio, byIndex[0])
	if ratio.Int64() < 450 || ratio.Int64() > 550 {
		t.Errorf("split ratio %v/100, want roughly 500", ratio)
	}
}

// Splitting must beat routing the whole order to any single pool — that is the
// entire justification for doing it.
func TestSplit_BeatsSinglePoolRouting(t *testing.T) {
	pools := []Pool{
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
	}
	amount := bigOf(t, "30000000000000000000")

	res, err := Split(pools, amount, true)
	if err != nil {
		t.Fatal(err)
	}

	solo, err := pools[0].Quote(amount, true)
	if err != nil {
		t.Fatal(err)
	}

	if res.BlendedCostBps >= solo.ExecutionCostBps {
		t.Errorf("split cost %d bps is not better than routing it all to one pool at %d bps",
			res.BlendedCostBps, solo.ExecutionCostBps)
	}
	if res.TotalOut.Cmp(solo.AmountOut) <= 0 {
		t.Errorf("split returned %s, not more than the single-pool %s", res.TotalOut, solo.AmountOut)
	}
}

// Identical pools must receive identical shares.
func TestSplit_IdenticalPoolsSplitEvenly(t *testing.T) {
	pools := []Pool{
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
	}

	res, err := Split(pools, bigOf(t, "8000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Allocations) != 2 {
		t.Fatalf("got %d allocations", len(res.Allocations))
	}

	diff := new(big.Int).Sub(res.Allocations[0].Amount, res.Allocations[1].Amount)
	diff.Abs(diff)
	// Integer rounding can leave a handful of units on one side.
	if diff.Cmp(big.NewInt(1000)) > 0 {
		t.Errorf("identical pools got %s and %s", res.Allocations[0].Amount, res.Allocations[1].Amount)
	}
}

// "This name can take 40k of your 200k" is a risk signal, not a failure.
func TestSplit_ReportsShortfall(t *testing.T) {
	// Concentrated liquidity genuinely runs out, unlike constant product.
	pools := []Pool{
		symmetricPool(t, 600, "1000000000000000000", 3000),
		symmetricPool(t, 600, "1000000000000000000", 3000),
	}
	huge := bigOf(t, "1000000000000000000000")

	res, err := Split(pools, huge, true)
	if err != nil {
		t.Fatal(err)
	}

	if res.Complete() {
		t.Error("an order far beyond the pools should not fill completely")
	}
	if res.Unfilled.Sign() <= 0 {
		t.Errorf("Unfilled = %s, want a positive shortfall", res.Unfilled)
	}
	if res.Filled.Sign() <= 0 {
		t.Error("a partial split should still allocate what it can")
	}
	if sum := new(big.Int).Add(res.Filled, res.Unfilled); sum.Cmp(huge) != 0 {
		t.Errorf("filled + unfilled = %s, want %s", sum, huge)
	}
}

// A split can look cheap on average while one leg is terrible.
func TestSplit_ReportsWorstLegSeparately(t *testing.T) {
	pools := []Pool{
		mustV2(t, "10000000000000000000000", "10000000000000000000000", 3000),
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
	}

	res, err := Split(pools, bigOf(t, "20000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.WorstCostBps < res.BlendedCostBps {
		t.Errorf("worst leg %d bps is below the blended %d bps", res.WorstCostBps, res.BlendedCostBps)
	}
	if res.MarginalBps <= 0 || res.MarginalBps >= BpsDenominator {
		t.Errorf("MarginalBps = %d, outside the valid range", res.MarginalBps)
	}
	// Every leg must respect the marginal budget it was struck at.
	for _, a := range res.Allocations {
		if a.Quote.ExecutionCostBps > res.MarginalBps {
			t.Errorf("pool %d cost %d bps, above the %d bps margin",
				a.PoolIndex, a.Quote.ExecutionCostBps, res.MarginalBps)
		}
	}
}

func TestSplit_MixedPoolTypes(t *testing.T) {
	pools := []Pool{
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
		symmetricPool(t, 6000, "1000000000000000000", 3000),
	}

	res, err := Split(pools, bigOf(t, "2000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Filled.Sign() <= 0 {
		t.Error("a mixed-AMM split should allocate something")
	}
	if len(res.Allocations) == 0 {
		t.Error("no allocations")
	}
}

func TestSplit_SinglePool(t *testing.T) {
	p := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)
	amount := bigOf(t, "1000000000000000000")

	res, err := Split([]Pool{p}, amount, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Allocations) != 1 {
		t.Fatalf("got %d allocations, want 1", len(res.Allocations))
	}
	if res.Allocations[0].Amount.Cmp(amount) != 0 {
		t.Errorf("a single pool should take the whole order, got %s", res.Allocations[0].Amount)
	}
}

func TestSplit_SkipsNilPoolsAndKeepsIndices(t *testing.T) {
	pools := []Pool{
		nil,
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
		nil,
		mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000),
	}

	res, err := Split(pools, bigOf(t, "2000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range res.Allocations {
		if a.PoolIndex != 1 && a.PoolIndex != 3 {
			t.Errorf("allocation points at index %d, which is nil", a.PoolIndex)
		}
	}
}

func TestSplit_Validation(t *testing.T) {
	p := mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)

	for _, amount := range []*big.Int{nil, big.NewInt(0), big.NewInt(-1)} {
		if _, err := Split([]Pool{p}, amount, true); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("amount %v: got %v, want ErrInvalidAmount", amount, err)
		}
	}
	if _, err := Split(nil, big.NewInt(100), true); !errors.Is(err, ErrInvalidPool) {
		t.Errorf("no pools: got %v, want ErrInvalidPool", err)
	}
	if _, err := Split([]Pool{nil, nil}, big.NewInt(100), true); !errors.Is(err, ErrInvalidPool) {
		t.Errorf("only nil pools: got %v, want ErrInvalidPool", err)
	}
}

func TestSplitResult_String(t *testing.T) {
	pools := []Pool{mustV2(t, "1000000000000000000000", "1000000000000000000000", 3000)}
	res, err := Split(pools, bigOf(t, "1000000000000000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.String() == "" {
		t.Error("String rendered empty")
	}
}
