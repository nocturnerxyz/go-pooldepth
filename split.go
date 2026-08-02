package pooldepth

import (
	"fmt"
	"math/big"
)

// Allocation is one pool's share of a split order.
type Allocation struct {
	// PoolIndex is the position in the slice handed to Split, so the caller can
	// map back to whatever they know the pool by.
	PoolIndex int
	Amount    *big.Int
	Quote     *Quote
}

// SplitResult describes how an order was divided across pools.
type SplitResult struct {
	// Allocations covers only the pools that received a non-zero share.
	Allocations []Allocation
	// Filled and Unfilled sum to the requested amount. Unfilled is non-zero
	// when the pools together cannot absorb the order at any cost.
	Filled   *big.Int
	Unfilled *big.Int
	// TotalOut is the sum of every allocation's output.
	TotalOut *big.Int

	// BlendedCostBps is the amount-weighted mean execution cost across
	// allocations. It is an average of per-pool costs rather than a single
	// price comparison, because the pools need not share a reference price —
	// two venues for the same pair routinely quote slightly differently, and
	// pretending otherwise would bake one venue's price in as truth.
	BlendedCostBps int
	// WorstCostBps is the highest cost any single allocation paid. A split can
	// look cheap on average while one leg is terrible.
	WorstCostBps int
	// MarginalBps is the cost budget at which the split was struck: every pool
	// contributed what it could absorb within it. It is the natural answer to
	// "what did this order cost at the margin".
	MarginalBps int
}

// Split allocates an order across several pools.
//
// Liquidity for a tokenized name is routinely spread over several venues, and
// TotalDepth answers whether there is enough in aggregate without answering how
// to actually use it. This does the second part.
//
// # How it splits
//
// It finds the smallest cost budget at which the pools together can absorb the
// order, then gives each pool exactly what it can take within that budget. That
// is the equal-marginal-cost allocation: at the chosen budget no pool is being
// pushed harder than any other, which is precisely the condition for the total
// cost to be minimised. Sending more to a deep pool and less to a shallow one
// falls out of the depths themselves rather than being hand-tuned.
//
// # What it is not
//
// This is a static allocation computed against each pool's pre-trade state.
// Real execution is sequential and moves each pool as it goes, so the realised
// cost is somewhat higher than the figures here. Treat the result as a plan and
// a bound, not as a settled fill.
func Split(pools []Pool, amountIn *big.Int, zeroForOne bool) (*SplitResult, error) {
	if err := validateAmount(amountIn); err != nil {
		return nil, err
	}

	live := make([]int, 0, len(pools))
	for i, p := range pools {
		if p != nil {
			live = append(live, i)
		}
	}
	if len(live) == 0 {
		return nil, fmt.Errorf("%w: no pools supplied", ErrInvalidPool)
	}

	totalAt := func(bps int) (*big.Int, []*big.Int, error) {
		depths := make([]*big.Int, len(live))
		total := new(big.Int)
		for i, idx := range live {
			d, err := pools[idx].DepthWithinBps(bps, zeroForOne)
			if err != nil {
				return nil, nil, err
			}
			depths[i] = d
			total.Add(total, d)
		}
		return total, depths, nil
	}

	// The pools cannot absorb the order at any price: allocate everything they
	// have and report the shortfall. That shortfall is the interesting number —
	// "this name can take 40k of your 200k" is a risk signal, not a failure.
	maxTotal, maxDepths, err := totalAt(BpsDenominator - 1)
	if err != nil {
		return nil, err
	}
	if maxTotal.Cmp(amountIn) < 0 {
		return buildResult(pools, live, maxDepths, maxTotal, amountIn, BpsDenominator-1, zeroForOne)
	}

	// Binary search for the smallest budget that fits the whole order. Depth is
	// monotone in the budget, so the predicate is monotone too.
	lo, hi := 1, BpsDenominator-1
	var chosenDepths []*big.Int
	var chosenTotal *big.Int

	for lo < hi {
		mid := lo + (hi-lo)/2
		total, depths, err := totalAt(mid)
		if err != nil {
			return nil, err
		}
		if total.Cmp(amountIn) >= 0 {
			hi = mid
			chosenDepths, chosenTotal = depths, total
		} else {
			lo = mid + 1
		}
	}

	if chosenDepths == nil {
		chosenTotal, chosenDepths, err = totalAt(lo)
		if err != nil {
			return nil, err
		}
	}

	return buildResult(pools, live, chosenDepths, chosenTotal, amountIn, lo, zeroForOne)
}

// buildResult scales the per-pool depths down to the requested amount and
// quotes each leg.
func buildResult(
	pools []Pool, live []int, depths []*big.Int, total, amountIn *big.Int,
	marginalBps int, zeroForOne bool,
) (*SplitResult, error) {
	target := new(big.Int).Set(amountIn)
	if total.Cmp(target) < 0 {
		target = new(big.Int).Set(total)
	}

	res := &SplitResult{
		Filled:      new(big.Int),
		TotalOut:    new(big.Int),
		MarginalBps: marginalBps,
	}

	// Scale each pool's capacity down proportionally so the parts sum to the
	// order rather than to the pools' combined capacity.
	assigned := make([]*big.Int, len(live))
	for i, d := range depths {
		if total.Sign() == 0 {
			assigned[i] = new(big.Int)
			continue
		}
		share := new(big.Int).Mul(d, target)
		assigned[i] = share.Div(share, total)
	}

	// Integer division loses a few units. Give the remainder to the pool with
	// the most capacity, which is the one best able to absorb it without
	// changing its cost.
	remainder := new(big.Int).Set(target)
	for _, a := range assigned {
		remainder.Sub(remainder, a)
	}
	if remainder.Sign() > 0 {
		deepest := 0
		for i := range depths {
			if depths[i].Cmp(depths[deepest]) > 0 {
				deepest = i
			}
		}
		assigned[deepest].Add(assigned[deepest], remainder)
	}

	weightedCost := new(big.Int)
	for i, idx := range live {
		amount := assigned[i]
		if amount.Sign() <= 0 {
			continue
		}

		alloc := Allocation{PoolIndex: idx, Amount: amount}
		q, err := pools[idx].Quote(amount, zeroForOne)
		if err != nil {
			// A leg that will not quote is dropped rather than failing the
			// whole plan: the rest of the split is still usable, and the
			// shortfall shows up in Unfilled.
			continue
		}

		alloc.Quote = q
		res.Allocations = append(res.Allocations, alloc)
		res.Filled.Add(res.Filled, amount)
		res.TotalOut.Add(res.TotalOut, q.AmountOut)
		weightedCost.Add(weightedCost, new(big.Int).Mul(amount, big.NewInt(int64(q.ExecutionCostBps))))

		if q.ExecutionCostBps > res.WorstCostBps {
			res.WorstCostBps = q.ExecutionCostBps
		}
	}

	if res.Filled.Sign() > 0 {
		blended := new(big.Int).Div(weightedCost, res.Filled)
		if blended.IsInt64() {
			res.BlendedCostBps = int(blended.Int64())
		}
	}

	res.Unfilled = new(big.Int).Sub(amountIn, res.Filled)
	if res.Unfilled.Sign() < 0 {
		res.Unfilled = new(big.Int)
	}

	return res, nil
}

// Complete reports whether the whole order was allocated.
func (r *SplitResult) Complete() bool {
	return r.Unfilled != nil && r.Unfilled.Sign() == 0
}

// String renders a split for logs.
func (r *SplitResult) String() string {
	s := fmt.Sprintf("split across %d pools: filled=%s out=%s blended=%dbps worst=%dbps margin=%dbps",
		len(r.Allocations), r.Filled, r.TotalOut, r.BlendedCostBps, r.WorstCostBps, r.MarginalBps)
	if !r.Complete() {
		s += fmt.Sprintf(" unfilled=%s", r.Unfilled)
	}
	return s
}
