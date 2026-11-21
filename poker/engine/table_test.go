package engine

import "testing"

func tableGame(t *testing.T, config string) *Game {
	t.Helper()
	cfg, err := LoadNLConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewHoldem(cfg, CategoryAbstraction{Ranks: 13})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var tableDeal = [TableCards]Card{card("As"), card("Kd"), card("7c"), card("7h"), card("2c"), card("9d"), card("Kh"), card("3s"), card("7d")}

// Every line of play the tree allows, on both table configs: the hand's
// chip count must agree with the tree's payoff at every fold and showdown
// (Act refuses otherwise), nobody puts in more than a stack, the options
// are the tree's actions, and what one seat wins the other loses.
func TestTableChipsMatchTreeOnEveryLine(t *testing.T) {
	for _, config := range []string{"configs/tiny_20bb.json", "configs/table_50bb.json"} {
		g := tableGame(t, config)
		lines, folds, showdowns := 0, 0, 0
		var walk func(h TableHand)
		walk = func(h TableHand) {
			if h.Over() {
				lines++
				if h.Net(0)+h.Net(1) != 0 {
					t.Fatalf("%s: nets %d and %d after %v", config, h.Net(0), h.Net(1), h.Log)
				}
				if h.Showdown() {
					showdowns++
					if len(h.Board()) != 5 || h.put[0] != h.put[1] {
						t.Fatalf("%s: showdown with board %v, chips %v", config, h.Board(), h.put)
					}
					// Seat 1 holds three sevens against a pair of kings.
					if h.Net(1) != h.put[0] {
						t.Fatalf("%s: seat 1 won %d with %d in each", config, h.Net(1), h.put[0])
					}
				} else {
					folds++
					folder := int(h.Node().Player)
					if h.Net(folder) != -h.put[folder] || h.put[folder] > h.put[1-folder] {
						t.Fatalf("%s: seat %d folded with %v in and lost %d", config, folder, h.put, -h.Net(folder))
					}
				}
				return
			}
			opts := h.Options()
			if len(opts) != int(h.Node().NumActions) || len(h.Board()) != g.Rules.BoardSize(h.Round()) {
				t.Fatalf("%s: %d options at a node with %d actions, board %v in round %d", config, len(opts), h.Node().NumActions, h.Board(), h.Round())
			}
			me := h.ToAct()
			for a, o := range opts {
				if o.Cost < 0 || h.put[me]+o.Cost > g.Stack() || (o.AllIn != (h.put[me]+o.Cost == g.Stack())) {
					t.Fatalf("%s: option %+v with %d of %d in", config, o, h.put[me], g.Stack())
				}
				next := h
				next.Log = append([]TableAction(nil), h.Log...)
				if err := next.Act(a); err != nil {
					t.Fatalf("%s: after %v, option %d: %v", config, h.Log, a, err)
				}
				walk(next)
			}
		}
		h, err := NewTableHand(g, tableDeal)
		if err != nil {
			t.Fatal(err)
		}
		if h.Put(0) != g.Config.SB || h.Put(1) != g.Config.BB || h.ToAct() != 0 {
			t.Fatalf("%s: starts with %v in, seat %d to act", config, h.put, h.ToAct())
		}
		walk(*h)
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
	if h.Net(0) != -g.Config.SB || h.Net(1) != g.Config.SB {
		t.Errorf("the small blind folded: nets %d and %d", h.Net(0), h.Net(1))
	}
}
