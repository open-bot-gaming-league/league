package engine

import (
	"math"
	"testing"
)

// The uniform-random strategy has a known best-response gain in both small
// games: OpenSpiel's exploitability tests pin NashConv (the sum over seats)
// at 11/12 for Kuhn and 4.747222... for Leduc. Matching them checks the
// tree, the showdown order, card removal and the chance weights at once,
// against numbers that did not come from this code.
func TestBestResponseToUniformMatchesKnownValues(t *testing.T) {
	cases := []struct {
		name     string
		g        *Game
		nashConv float64
	}{
		{"kuhn", NewKuhn(), 11.0 / 12},
		{"leduc", NewLeduc(), 4.747222222222222},
	}
	for _, c := range cases {
		expl, br := Exploitability(c.g, UniformStrategy{}, 1)
		// Both seats play uniform, so NashConv = br0 + br1 - (v0 + v1) and
		// the profile values cancel.
		if got := br[0] + br[1]; math.Abs(got-c.nashConv) > 1e-9 {
			t.Errorf("%s: NashConv of uniform = %.9f, want %.9f", c.name, got, c.nashConv)
		}
		if math.Abs(expl-c.nashConv/2) > 1e-9 {
			t.Errorf("%s: exploitability %.9f, want %.9f", c.name, expl, c.nashConv/2)
		}
	}
}

// A best response can never do worse than playing the strategy itself.
func TestBestResponseAtLeastProfileValue(t *testing.T) {
	for _, g := range []*Game{NewKuhn(), NewLeduc()} {
		v := ProfileValue(g, [2]Strategy{UniformStrategy{}, UniformStrategy{}}, 1)
		_, br := Exploitability(g, UniformStrategy{}, 1)
		if br[0] < v-1e-12 || br[1] < -v-1e-12 {
			t.Errorf("%s: profile value %.6f, best responses %.6f / %.6f", g.Rules.Name, v, br[0], br[1])
		}
	}
}

// hashStrategy is an arbitrary fixed strategy that differs at every
// infoset, so a symmetry bug cannot hide behind uniform play.
type hashStrategy struct{}

func (hashStrategy) Probs(n *Node, bucket int, out []float64) {
	x := uint64(n.Offset)*0x9E3779B97F4A7C15 + uint64(bucket)*0xBF58476D1CE4E5B9
	var total float64
	for a := range out {
		x ^= x >> 29
		x *= 0x94D049BB133111EB
		out[a] = float64(x>>40) + 1
		total += out[a]
	}
	for a := range out {
		out[a] /= total
	}
}

func shortDeckGame(t *testing.T, ranks int) *Game {
	t.Helper()
	cfg := NLConfig{SB: 1, BB: 2, StackBB: 3, Opening: []float64{1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true}
	g, err := NewGame(HoldemRules(ranks), BuildNLTree(cfg), CategoryAbstraction{Ranks: ranks})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Hold'em's best response enumerates one flop per suit-isomorphism class.
// On a 12-card deck every flop can be enumerated too; the two must agree.
// The parallel fan-out must also agree with the serial walk.
func TestSuitSymmetryMatchesFullEnumeration(t *testing.T) {
	sym := shortDeckGame(t, 3)
	full := shortDeckGame(t, 3)
	full.Rules.SuitSymmetric = false
	if a, b := len(rootOutcomes(sym)), binom[12][3]; a >= b {
		t.Fatalf("symmetry found %d flop classes out of %d flops", a, b)
	}
	for seat := 0; seat < 2; seat++ {
		want := BestResponseValue(full, hashStrategy{}, seat, 1)
		for _, workers := range []int{1, 4} {
			if got := BestResponseValue(sym, hashStrategy{}, seat, workers); math.Abs(got-want) > 1e-12 {
				t.Errorf("seat %d, %d workers: with symmetry %.15f, full enumeration %.15f", seat, workers, got, want)
			}
		}
	}
	if got, want := ProfileValue(sym, [2]Strategy{hashStrategy{}, hashStrategy{}}, 4), ProfileValue(full, [2]Strategy{hashStrategy{}, hashStrategy{}}, 1); math.Abs(got-want) > 1e-12 {
		t.Errorf("profile value: with symmetry %.15f, full enumeration %.15f", got, want)
	}
}

// rootOutcomes lists the flops best response would deal for g.
func rootOutcomes(g *Game) []outcome {
	e := newEvaluator(g, against(UniformStrategy{}, 0), 0, true, 1)
	ar := e.newArena()
	e.deal(ar.pub[0], nil, nil)
	return e.outcomes(ar.pub[0], ar)
}

func TestFullDeckHasTheKnownFlopClasses(t *testing.T) {
	g, err := NewHoldem(NLConfig{SB: 1, BB: 2, StackBB: 4, Opening: []float64{1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true},
		CategoryAbstraction{Ranks: 13})
	if err != nil {
		t.Fatal(err)
	}
	outcomes := rootOutcomes(g)
	if len(outcomes) != 1755 {
		t.Fatalf("flop classes: got %d, want 1755", len(outcomes))
	}
	total := 0
	for _, o := range outcomes {
		total += len(o.perms)
	}
	if total != 22100 {
		t.Fatalf("flops covered: got %d, want 22100", total)
	}
}

// The fold and showdown sweeps handle card removal by inclusion-exclusion
// and ties by grouping. Check both against the obvious double loop over
// (hand, opponent hand) on real river boards, where two-card hands share
// cards and many hands tie.
func TestTerminalSweepsMatchPairwiseSums(t *testing.T) {
	g, err := NewHoldem(NLConfig{SB: 1, BB: 2, StackBB: 4, Opening: []float64{1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true},
		CategoryAbstraction{Ranks: 13})
	if err != nil {
		t.Fatal(err)
	}
	e := newEvaluator(g, against(UniformStrategy{}, 0), 0, true, 1)
	ar := e.newArena()
	rnd := newRNG(21, 0)
	boards := [][]Card{hand("2c", "2d", "2h", "2s", "3c"), hand("Tc", "Jc", "Qc", "Kc", "Ac")} // everyone ties, in different ways
	for i := 0; i < 4; i++ {
		boards = append(boards, randomCards(rnd, 5))
	}
	for _, board := range boards {
		sortCards(board)
		e.deal(ar.pub[0], nil, nil)
		e.deal(ar.pub[1], ar.pub[0], board[:3])
		e.deal(ar.pub[2], ar.pub[1], board[3:4])
		pub := ar.pub[3]
		e.deal(pub, ar.pub[2], board[4:])
		n := len(pub.hands)
		if n != 1081 {
			t.Fatalf("board %v: %d live hands, want 1081", board, n)
		}
		reach := make([]float64, n)
		for i := range reach {
			if rnd.intn(4) > 0 { // leave some hands with no reach
				reach[i] = float64(rnd.float32())
			}
		}
		fold, show := make([]float64, n), make([]float64, n)
		sums := e.newReachSums(pub, ar)
		sums.foldValues(pub, reach, fold, 1.5)
		sums.showdownValues(pub, reach, show, 2.5)
		for i := 0; i < n; i++ {
			var wantFold, wantShow float64
			mine := cardMask(e.hands[pub.hands[i]])
			for j := 0; j < n; j++ {
				if mine&cardMask(e.hands[pub.hands[j]]) != 0 {
					continue
				}
				wantFold += 1.5 * reach[j]
				switch {
				case pub.strength[i] > pub.strength[j]:
					wantShow += 2.5 * reach[j]
				case pub.strength[i] < pub.strength[j]:
					wantShow -= 2.5 * reach[j]
				}
			}
			if math.Abs(fold[i]-wantFold) > 1e-9 || math.Abs(show[i]-wantShow) > 1e-9 {
				t.Fatalf("board %v hand %v: fold %.9f (want %.9f), showdown %.9f (want %.9f)",
					board, e.hands[pub.hands[i]], fold[i], wantFold, show[i], wantShow)
			}
		}
	}
}

// BenchmarkBestResponseOneFlop times the unit of work best response
// repeats 1,755 times per seat on the tiny tree: one flop, all turns and
// rivers.
func BenchmarkBestResponseOneFlop(b *testing.B) {
	cfg, err := LoadNLConfig("configs/tiny_20bb.json")
	if err != nil {
		b.Fatal(err)
	}
	g, err := NewHoldem(cfg, CategoryAbstraction{Ranks: 13})
	if err != nil {
		b.Fatal(err)
	}
	e := newEvaluator(g, against(UniformStrategy{}, 0), 0, true, 1)
	ar := e.newArena()
	root := ar.pub[0]
	e.deal(root, nil, nil)
	size := len(root.hands)
	en := entry{node: 0, reach: ar.get(size), out: ar.get(size)}
	for i := range en.reach {
		en.reach[i] = 1
	}
	var leaves []entry
	e.walk(0, root, en.reach, nil, &leaves, nil, ar)
	var flop outcome
	flop.count = copy(flop.cards[:], hand("7c", "9d", "Kh"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sums := make([][]float64, len(leaves))
		for li := range sums {
			sums[li] = make([]float64, size)
		}
		e.chanceOutcome(root, leaves, flop, sums, 1, ar)
	}
}

// riverState deals a fixed river board for the terminal benchmarks.
func riverState(b *testing.B) (*evaluator, *arena, *pubState, []float64) {
	cfg, err := LoadNLConfig("configs/tiny_20bb.json")
	if err != nil {
		b.Fatal(err)
	}
	g, err := NewHoldem(cfg, CategoryAbstraction{Ranks: 13})
	if err != nil {
		b.Fatal(err)
	}
	e := newEvaluator(g, against(UniformStrategy{}, 0), 0, true, 1)
	ar := e.newArena()
	board := hand("7c", "9d", "Kh", "2s", "Jc")
	e.deal(ar.pub[0], nil, nil)
	e.deal(ar.pub[1], ar.pub[0], board[:3])
	e.deal(ar.pub[2], ar.pub[1], board[3:4])
	e.deal(ar.pub[3], ar.pub[2], board[4:])
	rnd := newRNG(2, 0)
	reach := make([]float64, len(ar.pub[3].hands))
	for i := range reach {
		reach[i] = float64(rnd.float32())
	}
	return e, ar, ar.pub[3], reach
}

func BenchmarkShowdownValues(b *testing.B) {
	e, ar, pub, reach := riverState(b)
	out := make([]float64, len(reach))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := ar.mark()
		sums := e.newReachSums(pub, ar)
		sums.showdownValues(pub, reach, out, 2.5)
		ar.release(m)
	}
}

func BenchmarkFoldValues(b *testing.B) {
	e, ar, pub, reach := riverState(b)
	out := make([]float64, len(reach))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := ar.mark()
		sums := e.newReachSums(pub, ar)
		sums.foldValues(pub, reach, out, 2.5)
		ar.release(m)
	}
}
