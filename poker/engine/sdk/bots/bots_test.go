package bots

import (
	"math/rand/v2"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

// Each starter bot makes only legal moves (a match stops at the first
// illegal one) in every kind of spot Random's bets of any size lead to,
// from both seats, and the same seed replays the same match.
func TestStarterBotsAreLegalAndDeterministic(t *testing.T) {
	for name, newBot := range ByName {
		t.Run(name, func(t *testing.T) {
			a, err := sdk.Match(newBot(), sdk.Random{}, 4000, 1)
			if err != nil {
				t.Fatal(err)
			}
			b, err := sdk.Match(newBot(), sdk.Random{}, 4000, 1)
			if err != nil {
				t.Fatal(err)
			}
			if a != b {
				t.Fatalf("seed 1 twice: %v, then %v", a, b)
			}
			t.Logf("%s vs Random: %v", name, a)
		})
	}
}

// spot is a state for one decision; r seeds its Rand.
func spot(round sdk.Round, hole, board string, l sdk.Legal, pot int, r uint64) sdk.State {
	parse := func(names string) []sdk.Card {
		var out []sdk.Card
		for i := 0; i+1 < len(names); i += 2 {
			c, err := sdk.ParseCard(names[i : i+2])
			if err != nil {
				panic(err)
			}
			out = append(out, c)
		}
		return out
	}
	s := sdk.State{Hand: 1, Seat: sdk.BigBlind, Round: round, Board: parse(board), Pot: pot, Legal: l,
		BigBlind: 100, Rand: rand.New(rand.NewPCG(r, 0))}
	copy(s.Hole[:], parse(hole))
	return s
}

// Clam in four spots: facing a preflop raise, a pot-sized river bet, and
// first to act on the flop.
func TestClamPlaysItsStyle(t *testing.T) {
	riverBet := sdk.Legal{Fold: true, Call: 1200, MinTo: 2400, MaxTo: 4400} // 1200 into 1200
	preflopRaise := sdk.Legal{Fold: true, Call: 200, MinTo: 500, MaxTo: 5000}
	flopFirst := sdk.Legal{Check: true, MinTo: 100, MaxTo: 4800}
	count := func(bot sdk.Bot, round sdk.Round, hole, board string, l sdk.Legal, pot int, kind sdk.Kind) int {
		n := 0
		for r := range uint64(1000) {
			if bot.Act(spot(round, hole, board, l, pot, r)).Kind == kind {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		what        string
		bot         sdk.Bot
		round       sdk.Round
		hole, board string
		l           sdk.Legal
		pot         int
		kind        sdk.Kind
		least, most int // of 1000
	}{
		{"Clam folds junk to a raise", Clam{}, sdk.Preflop, "7c2d", "", preflopRaise, 400, sdk.Fold, 1000, 1000},
		{"Clam raises aces", Clam{}, sdk.Preflop, "AcAd", "", preflopRaise, 400, sdk.Raise, 1000, 1000},
		{"Clam folds one pair to a bet", Clam{}, sdk.River, "KcQd", "Ks9h4s2dJc", riverBet, 2400, sdk.Fold, 1000, 1000},
		{"Clam bets two pair", Clam{}, sdk.Flop, "Kc9d", "Ks9h4s", flopFirst, 400, sdk.Raise, 1000, 1000},
		{"Clam checks one pair", Clam{}, sdk.Flop, "KcQd", "Ks9h4s", flopFirst, 400, sdk.Check, 1000, 1000},
	} {
		if n := count(tc.bot, tc.round, tc.hole, tc.board, tc.l, tc.pot, tc.kind); n < tc.least || n > tc.most {
			t.Errorf("%s: %s %d times in 1000, want %d to %d", tc.what, tc.kind, n, tc.least, tc.most)
		}
	}
}

// Dove bets what its hand is worth and never bluffs.
func TestDovePlaysItsStyle(t *testing.T) {
	checked := sdk.Legal{Check: true, MinTo: 100, MaxTo: 4800}
	riverBet := sdk.Legal{Fold: true, Call: 1200, MinTo: 2400, MaxTo: 4400} // 1200 into 1200
	smallBet := sdk.Legal{Fold: true, Call: 200, MinTo: 400, MaxTo: 4600}   // 200 into 1000
	n := func(round sdk.Round, hole, board string, l sdk.Legal, pot int, kind sdk.Kind) int {
		c := 0
		for r := range uint64(200) {
			if (Dove{}).Act(spot(round, hole, board, l, pot, r)).Kind == kind {
				c++
			}
		}
		return c
	}
	for _, tc := range []struct {
		what        string
		round       sdk.Round
		hole, board string
		l           sdk.Legal
		pot         int
		kind        sdk.Kind
	}{
		{"it bets the nuts", sdk.River, "AhKh", "QhJhTh2c3d", checked, 1200, sdk.Raise},
		{"it raises aces", sdk.Preflop, "AcAd", "", sdk.Legal{Fold: true, Call: 200, MinTo: 500, MaxTo: 5000}, 400, sdk.Raise},
		{"it never bluffs: nothing, checked to, it checks", sdk.River, "7c2d", "KsQh9s4dJc", checked, 1200, sdk.Check},
		{"it folds nothing to a pot-sized bet", sdk.River, "7c2d", "KsQh9s4dJc", riverBet, 2400, sdk.Fold},
		{"it calls a fair price with a bottom pair", sdk.Turn, "5c3d", "Ks9h3s2d", smallBet, 1000, sdk.Call},
	} {
		if got := n(tc.round, tc.hole, tc.board, tc.l, tc.pot, tc.kind); got < 190 {
			t.Errorf("%s: %s %d times in 200", tc.what, tc.kind, got)
		}
	}
}

// Shark plays its chart before the flop and keeps the initiative after it.
func TestSharkPlaysItsStyle(t *testing.T) {
	act := func(s sdk.State) sdk.Action { return Shark{}.Act(s) }
	sbFirst := spot(sdk.Preflop, "AhKh", "", sdk.Legal{Fold: true, Call: 50, MinTo: 200, MaxTo: 5000}, 150, 1)
	sbFirst.Seat, sbFirst.InRound, sbFirst.OppInRound = sdk.SmallBlind, 50, 100
	if a := act(sbFirst); a.Kind != sdk.Raise || a.To != 250 {
		t.Errorf("AKs first in: %v, want a raise to 250", a)
	}
	sbFirst.Hole = [2]sdk.Card(cards(t, "7c2d"))
	if a := act(sbFirst); a.Kind != sdk.Fold {
		t.Errorf("72o first in: %v, want a fold", a)
	}
	opened := []sdk.Event{{Seat: sdk.SmallBlind, Round: sdk.Preflop, Kind: sdk.Raise, To: 250, Cost: 200}}
	bb := spot(sdk.Preflop, "9c9d", "", sdk.Legal{Fold: true, Call: 150, MinTo: 400, MaxTo: 5000}, 350, 1)
	bb.History, bb.InRound, bb.OppInRound = opened, 100, 250
	for hole, want := range map[string]sdk.Kind{"9c9d": sdk.Call, "AcAd": sdk.Raise, "7c2d": sdk.Fold} {
		bb.Hole = [2]sdk.Card(cards(t, hole))
		if a := act(bb); a.Kind != want {
			t.Errorf("big blind with %s facing an open: %v, want %s", hole, a, want)
		}
	}
	// It raised before the flop and was called: it bets the flop with nothing ...
	called := append(opened, sdk.Event{Seat: sdk.BigBlind, Round: sdk.Preflop, Kind: sdk.Call, To: 250, Cost: 150},
		sdk.Event{Seat: sdk.BigBlind, Round: sdk.Flop, Kind: sdk.Check})
	flop := spot(sdk.Flop, "7c2d", "KsQh9s", sdk.Legal{Check: true, MinTo: 100, MaxTo: 4750}, 500, 1)
	flop.Seat, flop.History = sdk.SmallBlind, called
	if a := act(flop); a.Kind != sdk.Raise {
		t.Errorf("the continuation bet: %v", a)
	}
	// ... and on the turn, called, it checks nothing and bets a real hand.
	turn := spot(sdk.Turn, "7c2d", "KsQh9s4d", sdk.Legal{Check: true, MinTo: 100, MaxTo: 4500}, 1000, 1)
	turn.Seat = sdk.SmallBlind
	turn.History = append(append([]sdk.Event{}, called...), sdk.Event{Seat: sdk.SmallBlind, Round: sdk.Flop, Kind: sdk.Raise, To: 250, Cost: 250},
		sdk.Event{Seat: sdk.BigBlind, Round: sdk.Flop, Kind: sdk.Call, To: 250, Cost: 250}, sdk.Event{Seat: sdk.BigBlind, Round: sdk.Turn, Kind: sdk.Check})
	if a := act(turn); a.Kind != sdk.Check {
		t.Errorf("called, nothing on the turn: %v, want a check", a)
	}
	turn.Hole = [2]sdk.Card(cards(t, "KcJd"))
	if a := act(turn); a.Kind != sdk.Raise {
		t.Errorf("top pair on the turn: %v, want a bet", a)
	}
	for hole, want := range map[string]string{"AhKh": "AKs", "KhAd": "AKo", "7c7d": "77", "2cTc": "T2s"} {
		if got := handKey([2]sdk.Card(cards(t, hole))); got != want {
			t.Errorf("handKey(%s) = %s, want %s", hole, got, want)
		}
	}
}

func cards(t *testing.T, names string) []sdk.Card {
	t.Helper()
	var out []sdk.Card
	for i := 0; i+1 < len(names); i += 2 {
		c, err := sdk.ParseCard(names[i : i+2])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}
