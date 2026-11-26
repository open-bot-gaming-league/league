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

// What each bot is for, in three spots: facing a pot-sized river bet,
// facing a preflop raise, and first to act on the flop.
func TestStarterBotsPlayTheirStyle(t *testing.T) {
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
		{"Station calls a river bet with a pair", Station{}, sdk.River, "7c7d", "Ks9h4s2dJc", riverBet, 2400, sdk.Call, 1000, 1000},
		{"Station folds a big river bet with nothing", Station{}, sdk.River, "7c6d", "Ks9h4s2dJc", riverBet, 2400, sdk.Fold, 1000, 1000},
		{"Station calls a preflop raise with junk", Station{}, sdk.Preflop, "7c2d", "", preflopRaise, 400, sdk.Call, 1000, 1000},
		{"Station seldom raises a big pair", Station{}, sdk.Preflop, "KcKd", "", preflopRaise, 400, sdk.Raise, 50, 150},
		{"Maniac raises junk", Maniac{}, sdk.Preflop, "7c2d", "", preflopRaise, 400, sdk.Raise, 750, 850},
		{"Maniac never folds", Maniac{}, sdk.River, "7c6d", "Ks9h4s2dJc", riverBet, 2400, sdk.Fold, 0, 0},
		{"Maniac bets the flop", Maniac{}, sdk.Flop, "7c6d", "Ks9h4s", flopFirst, 400, sdk.Raise, 750, 850},
		{"Rock folds junk to a raise", Rock{}, sdk.Preflop, "7c2d", "", preflopRaise, 400, sdk.Fold, 1000, 1000},
		{"Rock raises aces", Rock{}, sdk.Preflop, "AcAd", "", preflopRaise, 400, sdk.Raise, 1000, 1000},
		{"Rock folds one pair to a bet", Rock{}, sdk.River, "KcQd", "Ks9h4s2dJc", riverBet, 2400, sdk.Fold, 1000, 1000},
		{"Rock bets two pair", Rock{}, sdk.Flop, "Kc9d", "Ks9h4s", flopFirst, 400, sdk.Raise, 1000, 1000},
		{"Rock checks one pair", Rock{}, sdk.Flop, "KcQd", "Ks9h4s", flopFirst, 400, sdk.Check, 1000, 1000},
	} {
		if n := count(tc.bot, tc.round, tc.hole, tc.board, tc.l, tc.pot, tc.kind); n < tc.least || n > tc.most {
			t.Errorf("%s: %s %d times in 1000, want %d to %d", tc.what, tc.kind, n, tc.least, tc.most)
		}
	}
}
