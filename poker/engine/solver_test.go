package engine

import (
	"math"
	"testing"
)

func trained(t *testing.T, g *Game, cfg TrainConfig, iters int64) *Solver {
	t.Helper()
	s, err := NewSolver(g, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Run(iters)
	return s
}

// Check (1) of the step-2 brief: the same engine that solves hold'em
// solves Kuhn and Leduc to their known solutions. Exploitability must
// approach 0 and the value of the game to seat 0 its known value
// (-1/18 for Kuhn, -0.0856 for Leduc).
func TestKuhnSolvedToKnownValue(t *testing.T) {
	g := NewKuhn()
	s := trained(t, g, DefaultTrainConfig(), 2_000_000)
	expl, _ := Exploitability(g, s.AverageStrategy(), 1)
	value := ProfileValue(g, [2]Strategy{s.AverageStrategy(), s.AverageStrategy()}, 1)
	t.Logf("kuhn: exploitability %.5f, value %.5f", expl, value)
	if expl > 0.003 {
		t.Errorf("exploitability %.5f, want < 0.003", expl)
	}
	if math.Abs(value-(-1.0/18)) > 0.003 {
		t.Errorf("game value %.5f, want -1/18 = %.5f", value, -1.0/18)
	}
}

func TestLeducSolvedToKnownValue(t *testing.T) {
	g := NewLeduc()
	s := trained(t, g, DefaultTrainConfig(), 4_000_000)
	expl, _ := Exploitability(g, s.AverageStrategy(), 1)
	value := ProfileValue(g, [2]Strategy{s.AverageStrategy(), s.AverageStrategy()}, 1)
	t.Logf("leduc: exploitability %.5f, value %.5f", expl, value)
	if expl > 0.02 {
		t.Errorf("exploitability %.5f, want < 0.02", expl)
	}
	if math.Abs(value-(-0.0856)) > 0.01 {
		t.Errorf("game value %.5f, want -0.0856", value)
	}
}

// Exploitability must fall as training goes on (checked at 10x steps,
// which is far outside sampling noise).
func TestLeducExploitabilityDecreases(t *testing.T) {
	g := NewLeduc()
	s := trained(t, g, DefaultTrainConfig(), 0)
	prev, _ := Exploitability(g, s.AverageStrategy(), 1)
	for _, upTo := range []int64{10_000, 100_000, 1_000_000} {
		s.Run(upTo - s.Iterations())
		expl, _ := Exploitability(g, s.AverageStrategy(), 1)
		t.Logf("leduc: %8d iterations, exploitability %.5f", upTo, expl)
		if expl >= prev {
			t.Errorf("exploitability rose from %.5f to %.5f by %d iterations", prev, expl, upTo)
		}
		prev = expl
	}
}

// Training with one worker is a pure function of the seed.
func TestSingleWorkerTrainingIsDeterministic(t *testing.T) {
	run := func() float64 {
		g := NewLeduc()
		s := trained(t, g, DefaultTrainConfig(), 50_000)
		s.Run(50_000) // chunked the same way both times
		expl, _ := Exploitability(g, s.AverageStrategy(), 1)
		return expl
	}
	if a, b := run(), run(); a != b {
		t.Errorf("same seed, different result: %v vs %v", a, b)
	}
}

// exactCards is a lossless abstraction for a short deck: the bucket is the
// hole cards and the visible board themselves.
type exactCards struct{ rules *Rules }

func (exactCards) ID() string { return "exact-cards" }
func (a exactCards) NumBuckets(round int) int {
	return binom[a.rules.Deck][a.rules.Hole] * binom[a.rules.Deck][a.rules.BoardSize(round)]
}
func (a exactCards) Bucket(round int, hole, board []Card) int {
	return combIndex(hole)*binom[a.rules.Deck][len(board)] + combIndex(board)
}

// Hold'em itself (two hole cards, flop, turn, river, a no-limit tree with
// all-ins and run-outs) must also be solved toward zero exploitability
// when the card abstraction loses nothing. A 12-card deck makes that
// checkable. This is the control for the bucketed hold'em curve in
// EVAL.md, which stops at the abstraction's floor: here, with the same
// rules, tree builder, solver and best response and no abstraction,
// exploitability keeps falling.
func TestShortDeckHoldemLosslessConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("trains 16M iterations")
	}
	rules := HoldemRules(3)
	rules.SuitSymmetric = false // raw cards are not suit-invariant buckets
	cfg := NLConfig{SB: 1, BB: 2, StackBB: 3, Opening: []float64{1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true}
	g, err := NewGame(rules, BuildNLTree(cfg), exactCards{rules})
	if err != nil {
		t.Fatal(err)
	}
	train := DefaultTrainConfig()
	train.Workers = 1
	s := trained(t, g, train, 0)
	prev, _ := Exploitability(g, s.AverageStrategy(), 4)
	t.Logf("short-deck hold'em, %d infosets: untrained exploitability %.4f bb/hand", g.Tree.Infosets, prev)
	for _, upTo := range []int64{1_000_000, 4_000_000, 16_000_000} {
		s.Run(upTo - s.Iterations())
		expl, _ := Exploitability(g, s.AverageStrategy(), 4)
		t.Logf("%9d iterations: exploitability %.4f bb/hand", upTo, expl)
		if expl >= prev {
			t.Errorf("exploitability rose from %.4f to %.4f by %d iterations", prev, expl, upTo)
		}
		prev = expl
	}
	if prev > 0.02 {
		t.Errorf("exploitability %.4f bb/hand after 16M iterations, want < 0.02", prev)
	}
}

// A training iteration must not allocate: at scale, per-visit garbage is
// what decides the process size (see worker).
func TestTrainingDoesNotAllocate(t *testing.T) {
	g, err := NewHoldem(NLConfig{SB: 1, BB: 2, StackBB: 4, Opening: []float64{1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true},
		CategoryAbstraction{Ranks: 13})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []StoreKind{Float32, Float16, BFloat16} {
		cfg := DefaultTrainConfig()
		cfg.RegretStorage, cfg.SumStorage = kind, kind
		s := trained(t, g, cfg, 1000)
		// Run itself allocates a fixed handful of objects per call; a
		// per-iteration allocation would add thousands.
		if allocs := testing.AllocsPerRun(5, func() { s.Run(2000) }); allocs > 20 {
			t.Errorf("%s: %.0f allocations in a 2000-iteration Run, want a fixed handful", kind, allocs)
		}
	}
}
