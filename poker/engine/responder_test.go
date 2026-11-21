package engine

import (
	"math"
	"testing"
)

// LBR is one particular responder, so what it wins can never exceed the
// exact best response, seat by seat; and against a strategy with obvious
// holes it should find a good part of it. Leduc has the exact numbers.
func TestLBRIsBelowExactBestResponse(t *testing.T) {
	g := NewLeduc()
	halfTrained := trained(t, g, DefaultTrainConfig(), 100_000).AverageStrategy()
	for name, strat := range map[string]Strategy{"uniform": UniformStrategy{}, "arbitrary": hashStrategy{}, "half-trained": halfTrained} {
		_, exact := Exploitability(g, strat, 1)
		res, _ := Respond(g, strat, ResponderConfig{Kind: LBR, Hands: 200_000, Seed: 3, Workers: 3})
		t.Logf("%-12s exact %.4f / %.4f, LBR %.4f +- %.4f / %.4f +- %.4f", name, exact[0], exact[1],
			res.Seat[0], res.SeatStdErr[0], res.Seat[1], res.SeatStdErr[1])
		for seat := 0; seat < 2; seat++ {
			if res.Seat[seat] > exact[seat]+4*res.SeatStdErr[seat] {
				t.Errorf("%s, seat %d: LBR wins %.4f +- %.4f, more than the exact best response %.4f",
					name, seat, res.Seat[seat], res.SeatStdErr[seat], exact[seat])
			}
		}
		// Against the two strategies full of holes, a working LBR finds
		// most of what is there (84% and 96% when this was written).
		if want := (exact[0] + exact[1]) / 2; name != "half-trained" && res.Value < 0.5*want {
			t.Errorf("%s: LBR finds %.4f of an exact %.4f", name, res.Value, want)
		}
	}
}

// On the river LBR's lookahead is exact for a strategy that never bets or
// raises after LBR's action, so against "always check or call" LBR must
// do as well as the exact best response. That pins the pot, asked and
// showdown arithmetic.
func TestLBRMatchesExactAgainstACallingStation(t *testing.T) {
	g := NewKuhn()
	_, exact := Exploitability(g, AlwaysCall{g.Tree}, 1)
	res, _ := Respond(g, AlwaysCall{g.Tree}, ResponderConfig{Kind: LBR, Hands: 400_000, Seed: 5, Workers: 3})
	t.Logf("kuhn calling station: exact %.4f / %.4f, LBR %.4f +- %.4f / %.4f +- %.4f", exact[0], exact[1],
		res.Seat[0], res.SeatStdErr[0], res.Seat[1], res.SeatStdErr[1])
	for seat := 0; seat < 2; seat++ {
		if math.Abs(res.Seat[seat]-exact[seat]) > 4*res.SeatStdErr[seat]+1e-9 {
			t.Errorf("seat %d: LBR %.4f +- %.4f, exact %.4f", seat, res.Seat[seat], res.SeatStdErr[seat], exact[seat])
		}
	}
}

// A head-to-head's sampled result must agree with the exact value of the
// same pairing, and a strategy against itself must come out even.
func TestMatchAgreesWithExactValue(t *testing.T) {
	g := NewLeduc()
	a := Player{g, trained(t, g, DefaultTrainConfig(), 200_000).AverageStrategy()}
	b := Player{g, hashStrategy{}}
	// a's exact winnings per hand, averaged over the seats.
	exact := (ProfileValue(g, [2]Strategy{a.Strat, b.Strat}, 1) - ProfileValue(g, [2]Strategy{b.Strat, a.Strat}, 1)) / 2
	res, err := Match(a, b, 400_000, 9, 3)
	if err != nil {
		t.Fatal(err)
	}
	mean, se := res.Mean, res.StdErr
	t.Logf("leduc: exact %.4f, match %.4f +- %.4f", exact, mean, se)
	if se <= 0 || math.Abs(mean-exact) > 4*se {
		t.Errorf("match %.4f +- %.4f, exact %.4f", mean, se, exact)
	}
	// Each seating on its own must agree with its exact value too.
	for seat, exact := range [2]float64{ProfileValue(g, [2]Strategy{a.Strat, b.Strat}, 1), -ProfileValue(g, [2]Strategy{b.Strat, a.Strat}, 1)} {
		got := res.Seat[seat]
		if got.StdErr <= 0 || math.Abs(got.Mean-exact) > 4*got.StdErr {
			t.Errorf("a in seat %d: match %.4f +- %.4f, exact %.4f", seat, got.Mean, got.StdErr, exact)
		}
	}
	if math.Abs((res.Seat[0].Mean+res.Seat[1].Mean)/2-mean) > 1e-9 {
		t.Errorf("the seats' means %.6f and %.6f do not average to %.6f", res.Seat[0].Mean, res.Seat[1].Mean, mean)
	}
	if res, _ := Match(a, a, 200_000, 9, 3); math.Abs(res.Mean) > 4*res.StdErr {
		t.Errorf("a strategy against itself: %.4f +- %.4f", res.Mean, res.StdErr)
	}
	if _, err := Match(a, Player{NewKuhn(), UniformStrategy{}}, 10, 1, 1); err == nil {
		t.Error("a match across different games should be refused")
	}
}

// The rollout responder is also one particular opponent: never above the
// exact best response. Against strategies full of holes it should find
// most of what is there.
func TestRolloutResponderIsBelowExactBestResponse(t *testing.T) {
	g := NewLeduc()
	halfTrained := trained(t, g, DefaultTrainConfig(), 100_000).AverageStrategy()
	for name, strat := range map[string]Strategy{"uniform": UniformStrategy{}, "arbitrary": hashStrategy{}, "half-trained": halfTrained} {
		_, exact := Exploitability(g, strat, 1)
		res, err := Respond(g, strat, ResponderConfig{Kind: Rollout, Hands: 12_000, Seed: 4, Workers: 3, Rollouts: 300})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%-12s exact %.4f / %.4f, rollout responder %.4f +- %.4f / %.4f +- %.4f", name, exact[0], exact[1],
			res.Seat[0], res.SeatStdErr[0], res.Seat[1], res.SeatStdErr[1])
		for seat := 0; seat < 2; seat++ {
			if res.Seat[seat] > exact[seat]+4*res.SeatStdErr[seat] {
				t.Errorf("%s, seat %d: the rollout responder wins %.4f +- %.4f, more than the exact best response %.4f",
					name, seat, res.Seat[seat], res.SeatStdErr[seat], exact[seat])
			}
		}
		if want := (exact[0] + exact[1]) / 2; name != "half-trained" && res.Value < 0.5*want {
			t.Errorf("%s: the rollout responder finds %.4f of an exact %.4f", name, res.Value, want)
		}
	}
}

// A responder's winnings are a lower bound only if its choices do not
// depend on what it cannot see. Deal a hand, take its first decision,
// then change every hidden card (the opponent's hand and the whole
// undealt board) and take the decision again with its own randomness
// rewound: the choice must be the same, for both kinds, in both games.
func TestResponderChoiceIgnoresHiddenCards(t *testing.T) {
	holdem, err := NewHoldem(NLConfig{SB: 1, BB: 2, StackBB: 6, Opening: []float64{0.5, 1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true},
		CategoryAbstraction{Ranks: 13})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []*Game{NewLeduc(), holdem} {
		r := g.Rules
		root := &g.Tree.Nodes[0]
		seat := int(root.Player)
		in := [2]float64{float64(g.Tree.Posted[0]), float64(g.Tree.Posted[1])}
		for _, kind := range []ResponderKind{LBR, Rollout} {
			cfg := ResponderConfig{Kind: kind, Seed: 6, Rollouts: 40}
			p := newResponder(g, hashStrategy{}, cfg, 0)
			shuffle := newRNG(77, 0)
			differed := 0
			for trial := 0; trial < 200; trial++ {
				g.sampleDeal(p.world, &p.deal)
				decide := func() int {
					p.thinking = newRNG(uint64(trial), 1)
					p.sit(seat)
					p.newRound(0)
					return p.choose(root, in)
				}
				first := decide()
				// Redeal everything the responder cannot see.
				mine := cardMask(r.hole(&p.deal, seat))
				before := p.deal.cards
				used := mine
				hidden := append(r.hole(&p.deal, 1-seat), r.board(&p.deal)...)
				for i := range hidden {
					for {
						c := shuffle.intn(r.Deck)
						if used&(1<<uint(c)) == 0 {
							used |= 1 << uint(c)
							hidden[i] = Card(c)
							break
						}
					}
				}
				sortCards(r.hole(&p.deal, 1-seat))
				g.fillDeal(&p.deal)
				if p.deal.cards != before {
					differed++
				}
				if second := decide(); second != first {
					t.Fatalf("%s, %s, trial %d: chose action %d, then %d after only the hidden cards changed",
						r.Name, kind, trial, first, second)
				}
			}
			if differed < 150 {
				t.Fatalf("%s: the redeal changed the cards in only %d of 200 trials", r.Name, differed)
			}
		}
	}
}

// Against a player who always folds, the best response is known exactly
// on any blind structure: win the small blind's post without acting when
// it folds first, and raise the big blind off its post otherwise. Both
// responders must find exactly that, hand after hand.
func TestRespondersTakeTheBlindsFromAlwaysFold(t *testing.T) {
	g, err := NewHoldem(NLConfig{SB: 1, BB: 2, StackBB: 6, Opening: []float64{0.5, 1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true},
		CategoryAbstraction{Ranks: 13})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []ResponderKind{LBR, Rollout} {
		res, err := Respond(g, AlwaysFold{g.Tree}, ResponderConfig{Kind: kind, Hands: 2000, Seed: 1, Workers: 2, Rollouts: 50})
		if err != nil {
			t.Fatal(err)
		}
		if res.Seat != [2]float64{1, 0.5} || res.StdErr != 0 {
			t.Errorf("%s against always-fold: %v +- %v by seat, want exactly [1 0.5]", kind, res.Seat, res.StdErr)
		}
	}
}

// With no round to deviate in, a responder is the strategy playing
// itself, which over the two seats comes out even; and being allowed to
// deviate in one round of Leduc must not beat being allowed in both.
func TestResponderRestrictedByStreet(t *testing.T) {
	g := NewLeduc()
	run := func(streets []int) ResponderResult {
		res, err := Respond(g, hashStrategy{}, ResponderConfig{Kind: Rollout, Hands: 40_000, Seed: 8, Workers: 3, Rollouts: 200, Streets: streets})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	none, first, second, both := run([]int{-1}), run([]int{0}), run([]int{1}), run(nil)
	t.Logf("leduc, arbitrary strategy: none %.3f +- %.3f, round 0 only %.3f, round 1 only %.3f, both %.3f",
		none.Value, none.StdErr, first.Value, second.Value, both.Value)
	if math.Abs(none.Value) > 4*none.StdErr {
		t.Errorf("no deviation allowed: %.4f +- %.4f, want 0", none.Value, none.StdErr)
	}
	for name, one := range map[string]ResponderResult{"round 0": first, "round 1": second} {
		if one.Value <= none.Value || one.Value > both.Value+4*both.StdErr {
			t.Errorf("%s only: %.4f, want between none (%.4f) and both (%.4f)", name, one.Value, none.Value, both.Value)
		}
	}
}

// The breakdown of a match adds up to the match, and its fold counts are
// what the strategies say. Checked on Leduc against sampling error, and
// exactly on a hold'em pairing whose every hand is known: a player who
// folds whenever it can against one who always calls.
func TestMatchBreakdown(t *testing.T) {
	g := NewLeduc()
	a := Player{g, trained(t, g, DefaultTrainConfig(), 200_000).AverageStrategy()}
	b := Player{g, hashStrategy{}}
	const deals = 300_000
	res, err := MatchWithBreakdown(a, b, deals, 9, 3)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := Match(a, b, deals, 9, 3)
	if plain.Mean != res.Mean || plain.Breakdown != nil {
		t.Errorf("asking for the breakdown changed the match: %.6f against %.6f", res.Mean, plain.Mean)
	}
	bd := res.Breakdown
	var hands, net float64
	for r := range bd.Ended {
		for _, cell := range bd.Ended[r] {
			hands += cell.Hands
			net += cell.Net
		}
	}
	if hands != 2*deals || bd.Hands != 2*deals || math.Abs(net/hands-res.Mean) > 1e-9 {
		t.Errorf("the cells hold %.0f hands and %.6f per hand; the match was %d hands at %.6f", hands, net/hands, 2*deals, res.Mean)
	}
	for r := range bd.Facing {
		for who, cell := range bd.Facing[r] {
			if cell.Decisions == 0 {
				continue
			}
			own := [2]float64{cell.FoldA, cell.FoldB}[who] / cell.Decisions
			se := math.Sqrt(own*(1-own)/cell.Decisions) + 1e-9
			if got := cell.Folded / cell.Decisions; math.Abs(got-own) > 5*se {
				t.Errorf("round %d, player %d: folded %.4f of the time, its strategy says %.4f", r, who, got, own)
			}
		}
	}
	if same, _ := MatchWithBreakdown(a, a, 50_000, 9, 3); same.Breakdown.Facing[0][0].FoldA != same.Breakdown.Facing[0][0].FoldB {
		t.Error("a player against itself: the two fold probabilities at its decisions differ")
	}

	// Folder against caller on the 20 bb tree. Folder in the small blind:
	// it folds at once. In the big blind: the caller limps, the folder
	// cannot fold and checks, and the hand is checked down.
	h := tableGame(t, "configs/tiny_20bb.json")
	folder, caller := Player{h, AlwaysFold{h.Tree}}, Player{h, AlwaysCall{h.Tree}}
	const n = 20_000
	res, err = MatchWithBreakdown(folder, caller, n, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	bd = res.Breakdown
	if c := bd.Ended[0][EndAFolded]; c.Hands != n || c.Net != -0.5*n {
		t.Errorf("preflop folds by the folder: %+v", c)
	}
	if c := bd.Ended[3][EndChecked]; c.Hands != n || bd.Hands != 2*n {
		t.Errorf("hands checked down: %+v of %.0f", c, bd.Hands)
	}
	// Each faced the small blind's first decision n times; the folder
	// folded every one, and would have folded the caller's too.
	if c := bd.Facing[0][0]; c.Decisions != n || c.Folded != n || c.FoldA != n || c.FoldB != 0 {
		t.Errorf("the folder's preflop decisions: %+v", c)
	}
	if c := bd.Facing[0][1]; c.Decisions != n || c.Folded != 0 || c.FoldA != n || c.FoldB != 0 {
		t.Errorf("the caller's preflop decisions: %+v", c)
	}
	// And a called bet: the caller against a player who always bets pot
	// ends every hand in a showdown after the caller's call.
	bettor := Player{h, betFirst{h.Tree}}
	res, _ = MatchWithBreakdown(caller, bettor, n, 1, 2)
	var called float64
	for r := range res.Breakdown.Ended {
		called += res.Breakdown.Ended[r][EndACalled].Hands
	}
	if called != 2*n {
		t.Errorf("the caller made the last call in %.0f of %d hands", called, 2*n)
	}
}

// betFirst makes the first bet or raise on offer, and calls when there is
// none.
type betFirst struct{ Tree *Tree }

func (s betFirst) Probs(n *Node, _ int, out []float64) {
	clear(out)
	for a := range out {
		if s.Tree.Actions[int(n.FirstChild)+a].Kind == ActBet {
			out[a] = 1
			return
		}
	}
	AlwaysCall(s).Probs(n, 0, out)
}
