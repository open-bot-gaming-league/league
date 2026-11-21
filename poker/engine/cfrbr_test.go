package engine

import (
	"math"
	"os"
	"testing"
)

// leducCoarse is a lossy Leduc abstraction: before the public card a
// player knows only whether it holds a king; after it, whether it has a
// pair, and otherwise again only king or not. Jacks and queens are never
// told apart unless paired.
type leducCoarse struct{}

func (leducCoarse) ID() string { return "leduc-coarse" }

func (leducCoarse) NumBuckets(round int) int {
	if round == 0 {
		return 2
	}
	return 3
}

func (leducCoarse) Bucket(round int, hole, board []Card) int {
	king := 0
	if hole[0]>>1 == 2 {
		king = 1
	}
	if round == 1 && hole[0]>>1 == board[0]>>1 {
		return 2
	}
	return king
}

func coarseLeduc() *Game {
	return mustGame(LeducRules(), BuildLimitTree(LimitConfig{Ante: 1, BetSize: []int{2, 4}, MaxBets: 2}), leducCoarse{})
}

func newCFRBR(t *testing.T, g *Game, cfg CFRBRConfig) *CFRBR {
	t.Helper()
	c, err := NewCFRBR(g, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The two passes of an iteration compute the same quantity from opposite
// sides: what R wins (pass 1, R's values against A's reach) and what A
// wins (pass 2, A's values against R's reach, replaying R's recorded
// choices). They must cancel on every iteration, whatever the strategies
// have become. On the 12-card hold'em deck this also covers the suit
// symmetry inside a subgame, which pass 2 may use only because R's trunk
// classes are suit orbits.
func TestCFRBRPassesCancel(t *testing.T) {
	// The last game has decision nodes with five and six actions, so its
	// recorded choices take four bits; the others' take one or two.
	menu := NLConfig{SB: 1, BB: 2, StackBB: 12, Opening: []float64{0.5, 1, 2}, RaiseMenu: []float64{0.5, 1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true}
	wide := mustGame(HoldemRules(3), BuildNLTree(menu), CategoryAbstraction{Ranks: 3})
	games := map[string]*Game{"leduc": NewLeduc(), "coarse leduc": coarseLeduc(), "12-card hold'em": shortDeckGame(t, 3),
		"12-card hold'em, wide menu": wide}
	widths := map[uint8]bool{}
	for i := range wide.Tree.Nodes {
		if n := &wide.Tree.Nodes[i]; n.Kind == Decision && n.Round > 0 {
			widths[choiceBits(int(n.NumActions))] = true
		}
	}
	if !widths[1] || !widths[2] || !widths[4] {
		t.Fatalf("the wide menu's postflop nodes use choice widths %v, want 1, 2 and 4 all present", widths)
	}
	for name, g := range games {
		c := newCFRBR(t, g, DefaultCFRBRConfig())
		w := c.workers[0]
		var worst float64
		iterations := 300
		if g == wide {
			iterations = 30
		}
		for i := 0; i < iterations; i++ {
			real, abstract := w.iterate()
			for seat := range real {
				worst = max(worst, math.Abs(real[seat]+abstract[seat]))
			}
		}
		t.Logf("%s: largest |R's value + A's value| over %d iterations: %.2e", name, iterations, worst)
		if worst > 1e-12 {
			t.Errorf("%s: the passes disagree by %.3e", name, worst)
		}
	}
}

// Recorded choices survive packing at every width and at lengths that do
// not fill the last byte.
func TestChoicePackingRoundTrips(t *testing.T) {
	rnd := newRNG(7, 0)
	for _, actions := range []int{2, 3, 4, 5, 6, 16, 17} {
		w := choiceBits(actions)
		for _, hands := range []int{1, 7, 8, 9, 1081, 1128, 1176} {
			src, back := make([]uint8, hands), make([]uint8, hands)
			for i := range src {
				src[i] = uint8(rnd.intn(actions))
				back[i] = 0xFF
			}
			packed := make([]byte, packedBytes(hands, w))
			packChoices(packed, src, w)
			unpackChoices(back, packed, w)
			for i := range src {
				if back[i] != src[i] {
					t.Fatalf("%d actions (%d bits), %d hands: choice %d came back as %d, was %d", actions, w, hands, i, back[i], src[i])
				}
			}
		}
	}
}

// R's trunk infosets in hold'em are the 169 preflop classes.
func TestTrunkClassesAreSuitOrbits(t *testing.T) {
	r := HoldemRules(13)
	class, classes := r.trunkClasses()
	if classes != preflopCount {
		t.Fatalf("%d classes, want %d", classes, preflopCount)
	}
	byPreflop := map[int]int32{}
	for h, hand := range r.Hands() {
		p := preflopClass(13, hand[0], hand[1])
		if c, seen := byPreflop[p]; seen && c != class[h] {
			t.Fatalf("hand %v: class %d, but its preflop class %d was numbered %d", hand, class[h], p, c)
		}
		byPreflop[p] = class[h]
	}
	if len(byPreflop) != classes {
		t.Fatalf("%d preflop classes map onto %d trunk classes", len(byPreflop), classes)
	}
}

// With lossless buckets the least exploitable abstract strategy is an
// equilibrium, so CFR-BR must drive exploitability to zero. This is the
// end-to-end check on the weights: the sampled deal against the exact
// trunk, and regrets against strategy sums.
func TestCFRBRSolvesLeduc(t *testing.T) {
	g := NewLeduc()
	c := newCFRBR(t, g, DefaultCFRBRConfig())
	prev, _ := Exploitability(g, c.AverageStrategy(), 1)
	for _, upTo := range []int64{100, 1000, 10000, 30000} {
		c.Run(upTo - c.Iterations())
		expl, _ := Exploitability(g, c.AverageStrategy(), 1)
		current, _ := Exploitability(g, c.CurrentStrategy(), 1)
		t.Logf("leduc: %6d iterations, exploitability %.5f average, %.5f current", upTo, expl, current)
		if expl >= prev {
			t.Errorf("exploitability rose from %.5f to %.5f by %d iterations", prev, expl, upTo)
		}
		prev = expl
	}
	if prev > 0.02 {
		t.Errorf("exploitability %.5f after 30,000 iterations, want < 0.02", prev)
	}
}

// The point of CFR-BR: on a lossy abstraction it ends at or below what
// plain CFR reaches on the same buckets, measured in the real game.
func TestCFRBRBeatsCFROnCoarseLeduc(t *testing.T) {
	g := coarseLeduc()
	plain := trained(t, g, DefaultTrainConfig(), 4_000_000)
	cfr, _ := Exploitability(g, plain.AverageStrategy(), 1)

	c := newCFRBR(t, g, DefaultCFRBRConfig())
	for _, upTo := range []int64{100, 1000, 10000} {
		real, _ := c.Run(upTo - c.Iterations())
		average, _ := Exploitability(g, c.AverageStrategy(), 1)
		current, _ := Exploitability(g, c.CurrentStrategy(), 1)
		t.Logf("coarse leduc: %6d iterations, exploitability %.5f average, %.5f current; R won %.5f while training",
			upTo, average, current, (real[0]+real[1])/2)
	}
	cfrbr, _ := Exploitability(g, c.AverageStrategy(), 1)
	t.Logf("coarse leduc: plain CFR %.5f, CFR-BR %.5f", cfr, cfrbr)
	if cfrbr > cfr {
		t.Errorf("CFR-BR ended at %.5f, above plain CFR's %.5f", cfrbr, cfr)
	}
}

// One worker is a pure function of the seed.
func TestCFRBRSingleWorkerIsDeterministic(t *testing.T) {
	run := func() float64 {
		g := coarseLeduc()
		c := newCFRBR(t, g, DefaultCFRBRConfig())
		c.Run(200)
		expl, _ := Exploitability(g, c.AverageStrategy(), 1)
		return expl
	}
	if a, b := run(), run(); a != b {
		t.Errorf("two runs with one seed: %.15f and %.15f", a, b)
	}
}

// The table of recorded choices is the one allocation that grows out of
// hand with the tree; NewCFRBR must refuse rather than allocate it.
func TestCFRBRRefusesAnOversizedChoiceTable(t *testing.T) {
	cfg := DefaultCFRBRConfig()
	g := shortDeckGame(t, 3)
	c := newCFRBR(t, g, cfg)
	cfg.MaxChoiceBytes = c.ChoiceBytes() - 1
	if _, err := NewCFRBR(g, cfg); err == nil {
		t.Errorf("NewCFRBR accepted a %d-byte table with a limit of %d", c.ChoiceBytes(), cfg.MaxChoiceBytes)
	}
	cfg.MaxChoiceBytes = c.ChoiceBytes()
	if _, err := NewCFRBR(g, cfg); err != nil {
		t.Errorf("NewCFRBR refused a table exactly at the limit: %v", err)
	}
}

// Kuhn has no board deal to sample.
func TestCFRBRNeedsABoardDeal(t *testing.T) {
	if _, err := NewCFRBR(NewKuhn(), DefaultCFRBRConfig()); err == nil {
		t.Error("NewCFRBR accepted Kuhn")
	}
}

// BenchmarkBestResponseOneTurnBotPoker prices CFR-BR on the Bot Poker
// tree, where an iteration cannot be run (TestCFRBRRefusesAnOversizedChoiceTable
// and EVAL.md): it times the best-response walk under one turn card of one
// flop, every river included, against uniform play or a blueprint. A pass
// over a flop is 49 of these when the cards have no suit symmetry, as
// here, and an iteration is four passes.
//
//	BOTPOKER_ABS=data/abs-10-10-10.bin [BOTPOKER_BLUEPRINT=runs/botpoker-k10/blueprint-300000000.bp] \
//	    go test -run '^$' -bench OneTurnBotPoker -benchtime 1x
func BenchmarkBestResponseOneTurnBotPoker(b *testing.B) {
	absPath := os.Getenv("BOTPOKER_ABS")
	if absPath == "" {
		b.Skip("set BOTPOKER_ABS to the bucket file")
	}
	g, closeGame, err := OpenGame("holdem", "configs/botpoker_100bb.json", absPath)
	if err != nil {
		b.Fatal(err)
	}
	defer closeGame()
	var strat Strategy = UniformStrategy{}
	if path := os.Getenv("BOTPOKER_BLUEPRINT"); path != "" {
		bp, err := OpenBlueprint(path, g)
		if err != nil {
			b.Fatal(err)
		}
		defer bp.Close()
		strat = bp
	}
	e := newEvaluator(g, against(strat, 0), 0, true, 1)
	ar := e.newArena()
	root := ar.pub[0]
	e.deal(root, nil, nil)
	reach := ar.scratch(len(root.hands))
	for i := range reach {
		reach[i] = 1
	}
	var preflop, leaves []entry
	e.walk(0, root, reach, nil, &preflop, nil, ar)
	flop := ar.pub[1]
	e.deal(flop, root, hand("7c", "9d", "Kh"))
	for _, leaf := range preflop {
		in := ar.scratch(len(flop.hands))
		for i, j := range flop.parent {
			in[i] = leaf.reach[j]
		}
		e.walk(leaf.node, flop, in, nil, &leaves, nil, ar)
	}
	sums := make([][]float64, len(leaves))
	for li := range sums {
		sums[li] = make([]float64, len(flop.hands))
	}
	var turn outcome
	turn.count = copy(turn.cards[:], hand("2s"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.chanceOutcome(flop, leaves, turn, sums, 1, ar)
	}
	b.ReportMetric(float64(len(leaves)), "lines-into-the-turn")
	b.ReportMetric(float64(e.boards[3].Load())/float64(b.N), "rivers")
}

// A run stopped between two checkpoints and resumed from the earlier one
// ends with the blueprints of a run that was never stopped, byte for byte
// (one worker). Checked on a lossy Leduc and on 12-card hold'em, where the
// trunk has suit classes and the subgame has three rounds.
func TestCFRBRResumeMatchesUninterrupted(t *testing.T) {
	for name, build := range map[string]func() *Game{"coarse leduc": coarseLeduc, "12-card hold'em": func() *Game { return shortDeckGame(t, 3) }} {
		path := t.TempDir() + "/state.cfrbr"
		whole := newCFRBR(t, build(), DefaultCFRBRConfig())
		whole.Run(60)

		stopped := newCFRBR(t, build(), DefaultCFRBRConfig())
		stopped.Run(25)
		if err := stopped.SaveState(path); err != nil {
			t.Fatal(err)
		}
		stopped.Run(20) // work past the checkpoint, lost when the run is killed

		resumed := newCFRBR(t, build(), DefaultCFRBRConfig())
		if err := resumed.LoadState(path); err != nil {
			t.Fatal(err)
		}
		if resumed.Iterations() != 25 {
			t.Fatalf("%s: resumed at %d iterations, saved at 25", name, resumed.Iterations())
		}
		resumed.Run(35)
		for which, pair := range map[string][2]*Blueprint{
			"average": {whole.Blueprint(whole.AverageStrategy()), resumed.Blueprint(resumed.AverageStrategy())},
			"current": {whole.Blueprint(whole.CurrentStrategy()), resumed.Blueprint(resumed.CurrentStrategy())},
		} {
			if !pair[0].Equal(pair[1]) || pair[0].Header.Iterations != pair[1].Header.Iterations {
				t.Errorf("%s: the resumed run's %s blueprint differs from the uninterrupted run's", name, which)
			}
		}
		// Not only to the blueprint's byte: the state itself is the same.
		for i, a := range whole.arrays() {
			if string(a) != string(resumed.arrays()[i]) {
				t.Errorf("%s: array %d of the resumed run differs from the uninterrupted run's", name, i)
			}
		}
		if whole.workers[0].rnd.s != resumed.workers[0].rnd.s {
			t.Errorf("%s: the generators differ", name)
		}
	}
}

// A state is refused by a trainer it does not belong to, by one that has
// already run, and when the file is cut short; a refusal leaves the
// trainer as it was.
func TestCFRBRStateRefusals(t *testing.T) {
	path := t.TempDir() + "/state.cfrbr"
	a := newCFRBR(t, coarseLeduc(), DefaultCFRBRConfig())
	a.Run(10)
	if err := a.SaveState(path); err != nil {
		t.Fatal(err)
	}
	if err := a.LoadState(path); err == nil {
		t.Error("a trainer that had run loaded a state")
	}
	if err := newCFRBR(t, NewLeduc(), DefaultCFRBRConfig()).LoadState(path); err == nil {
		t.Error("a trainer on another abstraction loaded the state")
	}
	other := DefaultCFRBRConfig()
	other.Seed = 2
	if err := newCFRBR(t, coarseLeduc(), other).LoadState(path); err == nil {
		t.Error("a trainer with another seed loaded the state")
	}
	other = DefaultCFRBRConfig()
	other.RegretFloor = 0
	if err := newCFRBR(t, coarseLeduc(), other).LoadState(path); err == nil {
		t.Error("a trainer with another regret floor loaded the state")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, cut := range map[string][]byte{"cut short": data[:len(data)-8], "too long": append(append([]byte{}, data...), 0), "not a state": []byte("PKRBP001 and then some")} {
		if err := os.WriteFile(path, cut, 0o644); err != nil {
			t.Fatal(err)
		}
		fresh := newCFRBR(t, coarseLeduc(), DefaultCFRBRConfig())
		if err := fresh.LoadState(path); err == nil {
			t.Errorf("%s: loaded", name)
		}
		if fresh.Iterations() != 0 {
			t.Errorf("%s: a refused load left the trainer at %d iterations", name, fresh.Iterations())
		}
		for i, arr := range fresh.arrays() {
			for _, b := range arr {
				if b != 0 {
					t.Fatalf("%s: a refused load wrote into array %d", name, i)
				}
			}
		}
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("SaveState left its temporary file behind")
	}
}
