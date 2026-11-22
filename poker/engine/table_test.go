package engine

import (
	"math"
	"math/rand/v2"
	"testing"
)

func tableGame(t *testing.T, config string) *Game {
	t.Helper()
	cfg, err := LoadNLConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return tableGameFor(t, cfg)
}

func tableGameFor(t *testing.T, cfg NLConfig) *Game {
	t.Helper()
	g, err := NewHoldem(cfg, CategoryAbstraction{Ranks: 13})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var tableDeal = [TableCards]Card{card("As"), card("Kd"), card("7c"), card("7h"), card("2c"), card("9d"), card("Kh"), card("3s"), card("7d")}

// Every line of play the tree allows, on both table configs, played by
// menu index at the table with a TreeView following: the table's options
// are the tree's actions, the view stays on the node the line reaches
// (menu sizes translate exactly, and the tree's actions play back as
// exactly the options), the hand's chips agree with the tree's
// payoff at every fold and showdown, nobody puts in more than a stack, and
// what one seat wins the other loses.
func TestTableChipsMatchTreeOnEveryLine(t *testing.T) {
	for _, config := range []string{"configs/tiny_20bb.json", "configs/table_50bb.json"} {
		g := tableGame(t, config)
		tree := g.Tree
		lines, folds, showdowns := 0, 0, 0
		never := func() uint32 { t.Fatal("a menu size drew a random translation"); return 0 }
		var walk func(h TableHand, v TreeView, node int32)
		walk = func(h TableHand, v TreeView, node int32) {
			n := &tree.Nodes[node]
			if h.Over() != (n.Kind != Decision) {
				t.Fatalf("%s: after %v the hand is over %v at a %v node", config, h.Log, h.Over(), n.Kind)
			}
			if h.Over() {
				lines++
				if h.Net(0)+h.Net(1) != 0 {
					t.Fatalf("%s: nets %d and %d after %v", config, h.Net(0), h.Net(1), h.Log)
				}
				lost := h.Put(0)
				if n.Kind == Fold {
					lost = h.Put(int(n.Player))
				}
				if want := int(math.Round(float64(n.Stake) * float64(g.Config.BB))); want != lost {
					t.Fatalf("%s: the tree pays %d chips, the table counted %d after %v", config, want, lost, h.Log)
				}
				if h.Showdown() {
					showdowns++
					if len(h.Board()) != 5 || h.Put(0) != h.Put(1) {
						t.Fatalf("%s: showdown with board %v, chips %d %d", config, h.Board(), h.Put(0), h.Put(1))
					}
					// Seat 1 holds three sevens against a pair of kings.
					if h.Net(1) != h.Put(0) {
						t.Fatalf("%s: seat 1 won %d with %d in each", config, h.Net(1), h.Put(0))
					}
				} else {
					folds++
					folder := int(n.Player)
					if h.Net(folder) != -h.Put(folder) || h.Put(folder) > h.Put(1-folder) {
						t.Fatalf("%s: seat %d folded with %d %d in and lost %d", config, folder, h.Put(0), h.Put(1), -h.Net(folder))
					}
				}
				return
			}
			v.Follow(&h, never)
			if v.Node() != n {
				t.Fatalf("%s: after %v the view is at %v, the line at node %d", config, h.Log, v.Node(), node)
			}
			opts := h.Options()
			if len(opts) != int(n.NumActions) || len(h.Board()) != g.Rules.BoardSize(h.Round()) || h.ToAct() != int(n.Player) || h.Round() != int(n.Round) {
				t.Fatalf("%s: %d options at a node with %d actions, board %v in round %d", config, len(opts), n.NumActions, h.Board(), h.Round())
			}
			me := h.ToAct()
			for a, o := range opts {
				if o.Action != tree.Actions[int(n.FirstChild)+a] {
					t.Fatalf("%s: option %d is %+v, the tree's %+v", config, a, o.Action, tree.Actions[int(n.FirstChild)+a])
				}
				if o.Cost < 0 || h.Put(me)+o.Cost > g.Stack() || (o.AllIn != (h.Put(me)+o.Cost == g.Stack())) {
					t.Fatalf("%s: option %+v with %d of %d in", config, o, h.Put(me), g.Stack())
				}
				next := h
				next.Log = append([]TableAction(nil), h.Log...)
				if err := next.Act(a); err != nil {
					t.Fatalf("%s: after %v, option %d: %v", config, h.Log, a, err)
				}
				// A bot on the tree plays the tree's action as exactly that option.
				byBot, bv := h, v
				byBot.Log = append([]TableAction(nil), h.Log...)
				if err := bv.Play(&byBot, a); err != nil || byBot.Log[len(h.Log)] != next.Log[len(h.Log)] {
					t.Fatalf("%s: after %v the bot played tree action %d as %+v (%v), the option is %+v", config, h.Log, a, byBot.Log[len(byBot.Log)-1], err, o)
				}
				walk(next, v, tree.Children[int(n.FirstChild)+a])
			}
		}
		h, err := NewTableHand(g, tableDeal)
		if err != nil {
			t.Fatal(err)
		}
		if h.Put(0) != g.Config.SB || h.Put(1) != g.Config.BB || h.ToAct() != 0 {
			t.Fatalf("%s: starts with %d %d in, seat %d to act", config, h.Put(0), h.Put(1), h.ToAct())
		}
		walk(*h, *NewTreeView(g), 0)
		t.Logf("%s: %d lines of play, %d end in a fold, %d in a showdown", config, lines, folds, showdowns)
		if folds == 0 || showdowns == 0 {
			t.Errorf("%s: no folds or no showdowns", config)
		}
	}
}

// The bucket a hand reports is the one training saw for the same cards.
func TestTableBucketMatchesTheDeal(t *testing.T) {
	g := tableGame(t, "configs/tiny_20bb.json")
	var d Deal
	copy(d.cards[:], tableDeal[:])
	sortCards(d.cards[0:2]) // as dealCards leaves them; tableDeal's are not in order on purpose
	sortCards(d.cards[2:4])
	g.fillDeal(&d)
	h, err := NewTableHand(g, tableDeal)
	if err != nil {
		t.Fatal(err)
	}
	// Check or call down to the river.
	for !h.Over() {
		for seat := 0; seat < 2; seat++ {
			if got, want := h.Bucket(seat), int(d.Bucket[seat][h.Round()]); got != want {
				t.Errorf("round %d, seat %d: bucket %d, the deal has %d", h.Round(), seat, got, want)
			}
		}
		for a, o := range h.Options() {
			if o.Kind == ActCheck || o.Kind == ActCall {
				if err := h.Act(a); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
	}
	if !h.Showdown() || h.Round() != 3 || h.HandName(0) != "a pair" || h.HandName(1) != "three of a kind" {
		t.Errorf("ended in round %d, showdown %v, %s against %s", h.Round(), h.Showdown(), h.HandName(0), h.HandName(1))
	}
}

func TestTableRefusesBadInput(t *testing.T) {
	g := tableGame(t, "configs/tiny_20bb.json")
	twice := tableDeal
	twice[8] = twice[0]
	if _, err := NewTableHand(g, twice); err == nil {
		t.Error("a deal with the same card twice was accepted")
	}
	if _, err := NewTableHand(NewLeduc(), tableDeal); err == nil {
		t.Error("a Leduc table was accepted")
	}
	h, _ := NewTableHand(g, tableDeal)
	if err := h.Act(len(h.Options())); err == nil {
		t.Error("an action past the last option was accepted")
	}
	if err := h.Act(0); err != nil || !h.Over() { // the small blind folds
		t.Fatalf("fold: %v, over %v", err, h.Over())
	}
	if err := h.Act(0); err == nil {
		t.Error("an action after the hand ended was accepted")
	}
	if err := h.Play(ActCall, 0); err == nil {
		t.Error("a call after the hand ended was accepted")
	}
	if l := h.Legal(); l != (TableLegal{}) {
		t.Errorf("a finished hand offers %+v", l)
	}
	if h.Net(0) != -g.Config.SB || h.Net(1) != g.Config.SB {
		t.Errorf("the small blind folded: nets %d and %d", h.Net(0), h.Net(1))
	}
}

// mustPlay plays a move that must be legal; refuse one that must not be.
func mustPlay(t *testing.T, h *TableHand, kind ActionKind, to int) {
	t.Helper()
	if err := h.Play(kind, to); err != nil {
		t.Fatalf("%v to %d: %v", kind, to, err)
	}
}

func refuse(t *testing.T, h *TableHand, kind ActionKind, to int) {
	t.Helper()
	before := *h
	if err := h.Play(kind, to); err == nil {
		t.Fatalf("%v to %d was accepted with %+v legal", kind, to, before.Legal())
	}
	if h.st != before.st || len(h.Log) != len(before.Log) {
		t.Fatalf("a refused %v changed the hand", kind)
	}
}

func wantLegal(t *testing.T, h *TableHand, want TableLegal) {
	t.Helper()
	if got := h.Legal(); got != want {
		t.Fatalf("after %d actions: legal %+v, want %+v", len(h.Log), got, want)
	}
}

// Legal amounts at a 50 bb table (blinds 50/100, 5,000 chips): the
// minimum raise before and after the flop, a re-raise's minimum, the
// limp and the big blind's option, an all-in for less than a full raise
// (which reopens nothing), and every move the rules forbid.
func TestTableLegalAmounts(t *testing.T) {
	g := tableGame(t, "configs/table_50bb.json")
	h, _ := NewTableHand(g, tableDeal)
	wantLegal(t, h, TableLegal{Fold: true, Call: 50, Wager: true, Raise: true, MinTo: 200, MaxTo: 5000})
	refuse(t, h, ActCheck, 0)
	refuse(t, h, ActBet, 199)  // under a min-raise to 2 bb
	refuse(t, h, ActBet, 5001) // over the stack
	mustPlay(t, h, ActBet, 200)
	// Raised by 100: the big blind may call 100 or re-raise by 100 or more.
	wantLegal(t, h, TableLegal{Fold: true, Call: 100, Wager: true, Raise: true, MinTo: 300, MaxTo: 5000})
	mustPlay(t, h, ActBet, 750) // a raise of 550
	wantLegal(t, h, TableLegal{Fold: true, Call: 550, Wager: true, Raise: true, MinTo: 1300, MaxTo: 5000})
	refuse(t, h, ActBet, 1299)
	mustPlay(t, h, ActCall, 0)
	if h.Round() != 1 || h.ToAct() != 1 || h.Put(0) != 750 || h.Put(1) != 750 || len(h.Board()) != 3 {
		t.Fatalf("after the call: round %d, seat %d to act, chips %d %d", h.Round(), h.ToAct(), h.Put(0), h.Put(1))
	}
	// The flop: nothing in yet this round, so a bet, of at least a big blind.
	wantLegal(t, h, TableLegal{Check: true, Wager: true, MinTo: 100, MaxTo: 4250})
	refuse(t, h, ActFold, 0) // nothing to fold to
	refuse(t, h, ActCall, 0) // nothing to call
	refuse(t, h, ActBet, 99)
	mustPlay(t, h, ActBet, 3000)
	// 1,250 behind after calling 3,000 is short of a full raise (to
	// 6,000): all-in is the only raise.
	wantLegal(t, h, TableLegal{Fold: true, Call: 3000, Wager: true, Raise: true, MinTo: 4250, MaxTo: 4250})
	refuse(t, h, ActBet, 4249)
	mustPlay(t, h, ActBet, 4250)
	if o := h.Log[len(h.Log)-1]; !o.AllIn || o.Amount != 1250 || o.Cost != 4250 {
		t.Fatalf("the all-in for less was logged as %+v", o)
	}
	// Facing an all-in: call or fold, never raise.
	wantLegal(t, h, TableLegal{Fold: true, Call: 1250, Raise: true})
	refuse(t, h, ActBet, 4250)
	mustPlay(t, h, ActCall, 0)
	if !h.Showdown() || len(h.Board()) != 5 || h.Put(0) != 5000 || h.Put(1) != 5000 || h.Net(1) != 5000 {
		t.Fatalf("after the all-in call: showdown %v, board %v, chips %d %d, seat 1 net %d", h.Showdown(), h.Board(), h.Put(0), h.Put(1), h.Net(1))
	}

	// The limp, the big blind's option, and a check-through round.
	h, _ = NewTableHand(g, tableDeal)
	mustPlay(t, h, ActCall, 0)
	wantLegal(t, h, TableLegal{Check: true, Wager: true, Raise: true, MinTo: 200, MaxTo: 5000})
	refuse(t, h, ActFold, 0)
	mustPlay(t, h, ActCheck, 0)
	if h.Round() != 1 || h.ToAct() != 1 {
		t.Fatalf("the big blind's check left round %d, seat %d to act", h.Round(), h.ToAct())
	}
	mustPlay(t, h, ActCheck, 0)
	mustPlay(t, h, ActBet, 137) // any size: 137 into 200
	if o := h.Log[len(h.Log)-1]; o.To != 137 || o.Cost != 137 || math.Abs(o.PotFraction-0.685) > 1e-9 {
		t.Fatalf("a bet of 137 into 200 was logged as %+v", o)
	}
	wantLegal(t, h, TableLegal{Fold: true, Call: 137, Wager: true, Raise: true, MinTo: 274, MaxTo: 4900})
	mustPlay(t, h, ActFold, 0)
	if h.Net(1) != -100 || h.Net(0) != 100 {
		t.Fatalf("the flop bettor won %d", h.Net(0))
	}
}

// Short stacks: at 2 bb the small blind's minimum raise is all-in; at
// 1 bb the big blind is all-in from its blind, and a call ends the
// betting.
func TestTableShortStacks(t *testing.T) {
	cfg := NLConfig{SB: 50, BB: 100, StackBB: 2, Opening: []float64{1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true}
	h, _ := NewTableHand(tableGameFor(t, cfg), tableDeal)
	wantLegal(t, h, TableLegal{Fold: true, Call: 50, Wager: true, Raise: true, MinTo: 200, MaxTo: 200})
	mustPlay(t, h, ActBet, 200)
	wantLegal(t, h, TableLegal{Fold: true, Call: 100, Raise: true})
	mustPlay(t, h, ActCall, 0)
	if !h.Showdown() || h.Net(1) != 200 {
		t.Fatalf("2 bb all-in and call: showdown %v, net %d", h.Showdown(), h.Net(1))
	}

	cfg.StackBB = 1
	h, _ = NewTableHand(tableGameFor(t, cfg), tableDeal)
	wantLegal(t, h, TableLegal{Fold: true, Call: 50, Raise: true})
	refuse(t, h, ActBet, 100)
	mustPlay(t, h, ActCall, 0)
	if !h.Showdown() || h.Put(0) != 100 || h.Net(1) != 100 {
		t.Fatalf("1 bb: after the call, showdown %v, chips %d, net %d", h.Showdown(), h.Put(0), h.Net(1))
	}

	// A config that forbids the small blind's fold and limp: raise or nothing.
	cfg.StackBB, cfg.AllowSBFold, cfg.AllowSBLimp = 20, false, false
	h, _ = NewTableHand(tableGameFor(t, cfg), tableDeal)
	wantLegal(t, h, TableLegal{Wager: true, Raise: true, MinTo: 200, MaxTo: 2000})
	refuse(t, h, ActFold, 0)
	refuse(t, h, ActCall, 0)
}

func TestPseudoHarmonicMapping(t *testing.T) {
	for _, c := range []struct{ A, B, x, want float64 }{
		{0.5, 1, 0.5, 1},
		{0.5, 1, 1, 0},
		{0.5, 1, 0.75, 0.375 / 0.875},    // (0.25)(1.5) / ((0.5)(1.75))
		{1, 2, 1.5, 0.5 * 2 / (1 * 2.5)}, // 0.4: a bet halfway between is read as the larger more often
		{0, 1, 0.5, 0.5 / 1.5},
	} {
		if got := pseudoHarmonic(c.A, c.B, c.x); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("f_{%g,%g}(%g) = %g, want %g", c.A, c.B, c.x, got, c.want)
		}
	}
	prev := 1.0
	for x := 0.5; x <= 2; x += 0.01 {
		f := pseudoHarmonic(0.5, 2, x)
		if f > prev+1e-12 || f < -1e-12 || f > 1+1e-12 {
			t.Fatalf("f_{0.5,2}(%g) = %g after %g: not monotone in [0, 1]", x, f, prev)
		}
		prev = f
	}
}

// translationHand is a 50 bb hand at the flop after a limp and a check,
// pot 200, the big blind to act, with a view following it.
var translationGame *Game

func translationHand(t *testing.T) (*TableHand, *TreeView) {
	t.Helper()
	if translationGame == nil {
		translationGame = tableGame(t, "configs/table_50bb.json")
	}
	g := translationGame
	h, _ := NewTableHand(g, tableDeal)
	mustPlay(t, h, ActCall, 0)
	mustPlay(t, h, ActCheck, 0)
	return h, NewTreeView(g)
}

// The flop menu here is 0.5, 1 and 2 pots (100, 200, 400 chips) and
// all-in (4,900). A bet between two sizes is read as the smaller with
// the pseudo-harmonic probability, an exact size as itself, a bet under
// the menu as its smallest, an all-in as the all-in; a big bet that is
// not all-in may be read as the all-in, and then the view leaves the tree
// when its line ends.
func TestTranslationReadsRealBets(t *testing.T) {
	rnd := rand.New(rand.NewPCG(5, 6))
	read := func(to int, draws int) map[int]int {
		got := map[int]int{}
		for i := 0; i < draws; i++ {
			h, v := translationHand(t)
			v.Follow(h, rnd.Uint32)
			mustPlay(t, h, ActBet, to)
			v.Follow(h, rnd.Uint32)
			got[v.st.call]++ // the tree's bet, in chips
		}
		return got
	}
	for _, c := range []struct {
		to   int
		want map[int]float64 // tree bet -> share
	}{
		{100, map[int]float64{100: 1}},
		{200, map[int]float64{200: 1}},
		{150, map[int]float64{100: 0.375 / 0.875, 200: 0.5 / 0.875}},
		{300, map[int]float64{200: pseudoHarmonic(1, 2, 1.5), 400: 1 - pseudoHarmonic(1, 2, 1.5)}},
		{4900, map[int]float64{4900: 1}},
		{1200, map[int]float64{400: pseudoHarmonic(2, 24.5, 6), 4900: 1 - pseudoHarmonic(2, 24.5, 6)}},
	} {
		draws := 20000 // a share's standard error is at most 0.0036
		got := read(c.to, draws)
		for amt, share := range c.want {
			if math.Abs(float64(got[amt])/float64(draws)-share) > 0.025 {
				t.Errorf("a bet of %d was read as %d in %d of %d, want a share of %.3f (all: %v)", c.to, amt, got[amt], draws, share, got)
			}
		}
		if len(got) > len(c.want) {
			t.Errorf("a bet of %d was read as %v, want only %v", c.to, got, c.want)
		}
	}
}

// The bot's answer is played at the same fraction of the real pot,
// clamped to the legal range, and its all-in is the exact all-in.
func TestTranslationPlaysTheBotsAnswer(t *testing.T) {
	zero := func() uint32 { return 0 } // every read takes the smaller size
	h, v := translationHand(t)
	mustPlay(t, h, ActBet, 150) // read as 100 into 200
	v.Follow(h, zero)
	if v.st.call != 100 || v.st.pot != 300 {
		t.Fatalf("the tree reads the bet as %d into %d", v.st.call, v.st.pot-v.st.call)
	}
	// The tree's raise: 1x the pot after calling, 400 on its 400. At the
	// table that is 1x of 350 + 150 = 500 beyond the call, to 650.
	opts := v.actions()
	raise := -1
	for i, a := range opts {
		if a.Kind == ActBet && !a.AllIn {
			raise = i
		}
	}
	if raise < 0 || opts[raise].Amount != 400 {
		t.Fatalf("tree actions %+v", opts)
	}
	if err := v.Play(h, raise); err != nil {
		t.Fatal(err)
	}
	if o := h.Log[len(h.Log)-1]; o.To != 650 || o.Cost != 650 || o.Amount != 500 {
		t.Fatalf("the bot's 1x pot raise was played as %+v", o)
	}
	if n := v.Node(); n == nil || int(n.Player) != h.ToAct() {
		t.Fatal("the view did not follow its own raise")
	}

	// A raise the tree sizes under the table's minimum is raised to it;
	// the tree's all-in is the table's all-in.
	h, v = translationHand(t)
	mustPlay(t, h, ActBet, 4000) // read as the tree's 400
	v.Follow(h, zero)
	for i, a := range v.actions() {
		if a.Kind == ActBet && a.AllIn {
			if err := v.Play(h, i); err != nil {
				t.Fatal(err)
			}
		}
	}
	if o := h.Log[len(h.Log)-1]; !o.AllIn || o.To != 4900 {
		t.Fatalf("the tree's all-in was played as %+v", o)
	}
	h, v = translationHand(t)
	mustPlay(t, h, ActBet, 1000) // read as 400: the tree's min re-raise is 400 more, the table's 1,000
	v.Follow(h, zero)
	if v.st.call != 400 {
		t.Fatalf("read as %d", v.st.call)
	}
	for i, a := range v.actions() {
		if a.Kind == ActBet && !a.AllIn {
			if err := v.Play(h, i); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if o := h.Log[len(h.Log)-1]; o.To < 2000 || o.To > 4900 {
		t.Fatalf("a re-raise of a 1,000 bet went to %d", o.To)
	}
}

// A big bet read as the tree's all-in: the bot's call ends the tree's
// line, the hand goes on, and the bot checks and calls to the end.
func TestTranslationOffTheTree(t *testing.T) {
	big := func() uint32 { return math.MaxUint32 } // every read takes the larger size
	g := tableGame(t, "configs/table_50bb.json")
	h, _ := NewTableHand(g, tableDeal)
	v := NewTreeView(g)
	mustPlay(t, h, ActCall, 0)
	mustPlay(t, h, ActCheck, 0)
	mustPlay(t, h, ActBet, 4000) // 20 pots, read as all-in (24.5 pots)
	v.Follow(h, big)
	call := -1
	for i, a := range v.actions() {
		if a.Kind == ActCall {
			call = i
		}
	}
	if err := v.Play(h, call); err != nil {
		t.Fatal(err)
	}
	if !v.OffTree() || h.Over() || h.Round() != 2 || h.ToAct() != 1 {
		t.Fatalf("off %v, over %v, round %d, seat %d to act", v.OffTree(), h.Over(), h.Round(), h.ToAct())
	}
	bp := NewBlueprint(g, UniformStrategy{})
	if err := v.Act(h, bp, big); err != nil || h.Log[len(h.Log)-1].Kind != ActCheck {
		t.Fatalf("off the tree the bot played %+v (%v)", h.Log[len(h.Log)-1], err)
	}
	mustPlay(t, h, ActBet, 900)
	if err := v.Act(h, bp, big); err != nil || !h.Showdown() {
		t.Fatalf("off the tree the bot answered an all-in with %+v (%v)", h.Log[len(h.Log)-1], err)
	}
}

// randomMove is a person at the table: a menu button, any legal amount,
// the minimum or the maximum, or one chip off a menu size.
func randomMove(h *TableHand, rnd *rand.Rand) (ActionKind, int) {
	l := h.Legal()
	switch k := rnd.IntN(10); {
	case k < 3:
		opts := h.Options()
		o := opts[rnd.IntN(len(opts))]
		return o.Kind, o.To
	case k < 7 && l.Wager:
		return ActBet, l.MinTo + rnd.IntN(l.MaxTo-l.MinTo+1)
	case k == 7 && l.Wager:
		return ActBet, l.MinTo
	case k == 8 && l.Wager:
		opts := h.Options()
		o := opts[rnd.IntN(len(opts))]
		if o.Kind == ActBet {
			return ActBet, min(max(o.To+rnd.IntN(3)-1, l.MinTo), l.MaxTo)
		}
	}
	switch {
	case l.Check:
		return ActCheck, 0
	case l.Call > 0 && rnd.IntN(4) > 0:
		return ActCall, 0
	case l.Fold:
		return ActFold, 0
	}
	return ActBet, l.MaxTo
}

// A bot that plays every tree action (uniform play) against a person who
// bets anything legal, over 100,000 hands on three trees: the bot never
// makes an illegal move, the chips always add up, and the view never
// loses step with the hand: it leaves the tree only where the tree's line
// ended.
func TestBotNeverPlaysIllegallyAgainstAnySize(t *testing.T) {
	hands := 100000
	if testing.Short() {
		hands = 10000
	}
	short := NLConfig{SB: 50, BB: 100, StackBB: 3, Opening: []float64{0.5, 1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true}
	games := []*Game{tableGame(t, "configs/table_50bb.json"), tableGame(t, "configs/tiny_20bb.json"), tableGameFor(t, short)}
	bps := make([]*Blueprint, len(games))
	for i, g := range games {
		bps[i] = NewBlueprint(g, UniformStrategy{})
	}
	rnd := rand.New(rand.NewPCG(7, 8))
	showdowns, offTree := 0, 0
	for i := 0; i < hands; i++ {
		g, bp := games[i%len(games)], bps[i%len(games)]
		var cards [TableCards]Card
		for j, c := range rnd.Perm(g.Rules.Deck)[:TableCards] {
			cards[j] = Card(c)
		}
		h, err := NewTableHand(g, cards)
		if err != nil {
			t.Fatal(err)
		}
		v := NewTreeView(g)
		botSeat := (i / len(games)) % 2
		wentOff := false
		for !h.Over() {
			me := h.ToAct()
			if me == botSeat {
				wentOff = wentOff || v.OffTree()
				if err := v.Act(h, bp, rnd.Uint32); err != nil {
					t.Fatalf("hand %d: the bot, after %v: %v", i, h.Log, err)
				}
			} else if kind, to := randomMove(h, rnd); h.Play(kind, to) != nil {
				t.Fatalf("hand %d: the person's %v to %d was refused with %+v legal", i, kind, to, h.Legal())
			}
			if h.Put(0) > g.Stack() || h.Put(1) > g.Stack() || h.Pot() != h.Put(0)+h.Put(1) {
				t.Fatalf("hand %d: chips %d %d, pot %d after %v", i, h.Put(0), h.Put(1), h.Pot(), h.Log)
			}
			if last := h.Log[len(h.Log)-1]; last.Seat != me || (!h.Over() && h.ToAct() == me && h.Round() == last.Round) {
				t.Fatalf("hand %d: seat %d acts twice in a round after %v", i, me, h.Log)
			}
		}
		if h.Net(0)+h.Net(1) != 0 || (h.Showdown() && h.Put(0) != h.Put(1)) {
			t.Fatalf("hand %d: nets %d %d, chips %d %d", i, h.Net(0), h.Net(1), h.Put(0), h.Put(1))
		}
		if h.Showdown() {
			showdowns++
		}
		v.Follow(h, rnd.Uint32)
		if v.OffTree() && !h.Over() {
			t.Fatalf("hand %d: off the tree in play", i)
		}
		if v.OffTree() && v.g.Tree.Nodes[v.node].Kind == Decision {
			t.Fatalf("hand %d: the view lost step with the hand after %v", i, h.Log)
		}
		if wentOff {
			offTree++
		}
	}
	t.Logf("%d hands, %d showdowns, %d went on past the end of the tree's line", hands, showdowns, offTree)
}
