package engine

import (
	"math"
	"sync"
)

// Exploitability inside the card abstraction.
//
// Exploitability (bestresponse.go) lets the responder see real cards. This
// file asks the other question: how much can a responder win who, like the
// strategy under test, sees only buckets? That is the quantity the solver
// is actually driving down, so it is the check that the solver is doing
// its job when the real-card number stops falling.
//
// With imperfect-recall buckets there is no efficient exact best response
// in bucket strategies, so this is a lower bound: a responder is trained
// by the same MCCFR against the fixed strategy, and its winnings are
// measured. On a perfect-recall game (Leduc) the bound closes on the exact
// value; TestBucketedResponseApproachesExactBestResponse.

// BucketedResponse trains seat `seat` against strat, held fixed in the
// other seat, and returns the responder's strategy.
func BucketedResponse(g *Game, strat Strategy, seat int, iters int64, cfg TrainConfig) (Strategy, error) {
	cfg.Fixed = [2]Strategy{}
	cfg.Fixed[1-seat] = strat
	s, err := NewSolver(g, cfg)
	if err != nil {
		return nil, err
	}
	s.Run(iters)
	return MostLikely{s.AverageStrategy()}, nil
}

// MostLikely plays the single most probable action of another strategy
// (the lowest-numbered on ties). A best response never needs to mix, so
// rounding a trained responder's average strategy this way removes the
// residue of its early exploration.
type MostLikely struct{ Of Strategy }

func (m MostLikely) Probs(n *Node, bucket int, out []float64) {
	m.Of.Probs(n, bucket, out)
	best := 0
	for a, p := range out {
		if p > out[best] {
			best = a
		}
	}
	clear(out)
	out[best] = 1
}

// BucketedExploitability is a lower bound on what bucket-seeing responders
// win per hand against strat, averaged over the two seats, with its
// standard error, and each seat's responder's winnings. Each responder is
// trained for iters iterations; then both are measured on the same `deals`
// random deals, which cancels most of the luck of the cards out of the
// bound.
func BucketedExploitability(g *Game, strat Strategy, iters, deals int64, cfg TrainConfig) (bound, stderr float64, value [2]float64, err error) {
	var profiles [2][2]Strategy
	for seat := range profiles {
		resp, err := BucketedResponse(g, strat, seat, iters, cfg)
		if err != nil {
			return 0, 0, value, err
		}
		profiles[seat] = [2]Strategy{strat, strat}
		profiles[seat][seat] = resp
	}
	means, stderrs := sampledMeans(g, deals, cfg.Seed+1000, cfg.Workers, 3, func(seat0Value func([2]Strategy) float64, out []float64) {
		out[0] = seat0Value(profiles[0])  // seat 0 responds
		out[1] = -seat0Value(profiles[1]) // seat 1 responds
		out[2] = (out[0] + out[1]) / 2
	})
	return means[2], stderrs[2], [2]float64{means[0], means[1]}, nil
}

// SampledValue estimates seat 0's expected winnings when seat p plays
// strats[p], from random deals, and the estimate's standard error. Only
// the cards are sampled: for each deal the expectation over both players'
// actions is computed exactly by walking the whole betting tree.
func SampledValue(g *Game, strats [2]Strategy, deals int64, seed uint64, workers int) (mean, stderr float64) {
	means, stderrs := sampledMeans(g, deals, seed, workers, 1, func(seat0Value func([2]Strategy) float64, out []float64) {
		out[0] = seat0Value(strats)
	})
	return means[0], stderrs[0]
}

// sampledMeans averages k per-deal quantities over random deals and
// returns each one's mean and standard error. perDeal computes the
// quantities for the current deal into out; seat0Value gives it seat 0's
// exact expected winnings on that deal under any strategy pair.
func sampledMeans(g *Game, deals int64, seed uint64, workers, k int,
	perDeal func(seat0Value func([2]Strategy) float64, out []float64)) (means, stderrs []float64) {
	workers = max(workers, 1)
	sums := make([][]float64, workers) // per worker: k sums, then k sums of squares
	depth := g.Tree.Depth()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		share := deals / int64(workers)
		if int64(w) < deals%int64(workers) {
			share++
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rnd := newRNG(seed, uint64(w))
			var deal Deal
			var strats [2]Strategy
			frames := make([][MaxActions]float64, depth)
			var walk func(ni int32, d int) float64
			walk = func(ni int32, d int) float64 {
				n := &g.Tree.Nodes[ni]
				switch n.Kind {
				case Fold:
					if n.Player == 0 {
						return -float64(n.Stake)
					}
					return float64(n.Stake)
				case Showdown:
					return float64(deal.Sign) * float64(n.Stake)
				}
				probs := frames[d][:n.NumActions]
				strats[n.Player].Probs(n, int(deal.Bucket[n.Player][n.Round]), probs)
				var v float64
				for a, p := range probs {
					if p != 0 {
						v += p * walk(g.Tree.Children[int(n.FirstChild)+a], d+1)
					}
				}
				return v
			}
			seat0Value := func(s [2]Strategy) float64 {
				strats = s
				return walk(0, 0)
			}
			acc := make([]float64, 2*k)
			out := make([]float64, k)
			for i := int64(0); i < share; i++ {
				g.sampleDeal(rnd, &deal)
				perDeal(seat0Value, out)
				for j, v := range out {
					acc[j] += v
					acc[k+j] += v * v
				}
			}
			sums[w] = acc
		}()
	}
	wg.Wait()
	means, stderrs = make([]float64, k), make([]float64, k)
	n := float64(deals)
	for j := 0; j < k; j++ {
		var sum, squares float64
		for _, acc := range sums {
			sum += acc[j]
			squares += acc[k+j]
		}
		means[j] = sum / n
		stderrs[j] = math.Sqrt(max(squares/n-means[j]*means[j], 0) / n)
	}
	return means, stderrs
}
