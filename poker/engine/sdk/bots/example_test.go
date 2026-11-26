package bots_test

import (
	"fmt"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// riverBettor is Station with one rule added: on the river, when the other
// seat checks to it, it bets three quarters of the pot.
type riverBettor struct{ bots.Station }

func (b riverBettor) Act(s sdk.State) sdk.Action {
	if s.Round == sdk.River && s.Legal.Check && len(s.History) > 0 {
		last := s.History[len(s.History)-1]
		if last.Seat != s.Seat && last.Round == sdk.River && last.Kind == sdk.Check {
			return s.RaisePot(0.75)
		}
	}
	return b.Station.Act(s) // everything else: Station as it was
}

// Fork a starter bot, add one rule, and measure it against the bot it came
// from: the same seed deals the same cards, each deal twice with the seats
// swapped, so what is left is mostly the rule. This is the README's first
// worked example.
func Example_forkAStarterBot() {
	res, err := sdk.Match(riverBettor{}, bots.Station{}, 20000, 1)
	if err != nil {
		panic(err)
	}
	fmt.Println(res)
	// Output: -2.7 ± 1.0 bb/100 over 20000 hands
}

// valueBettor is the rule refined after measuring: bet the river when
// checked to only with a pair or better (Station calls; bluffing it loses).
type valueBettor struct{ bots.Station }

func (b valueBettor) Act(s sdk.State) sdk.Action {
	if s.Made() >= sdk.Pair {
		return riverBettor{b.Station}.Act(s)
	}
	return b.Station.Act(s)
}

func Example_refineTheRule() {
	res, err := sdk.Match(valueBettor{}, bots.Station{}, 20000, 1)
	if err != nil {
		panic(err)
	}
	fmt.Println(res)
	// Output: +7.5 ± 0.9 bb/100 over 20000 hands
}
