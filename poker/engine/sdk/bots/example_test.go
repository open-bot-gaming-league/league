package bots_test

import (
	"fmt"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// respectful is Dove with one rule added: Dove never bluffs, so facing a
// bet of half the pot or more, fold unless the hand is very strong.
type respectful struct {
	bots.Dove
	strong float64 // the equity it takes to go on
}

func (b respectful) Act(s sdk.State) sdk.Action {
	if s.Legal.Call > 0 && 3*s.Legal.Call >= s.Pot && s.Equity(300) < b.strong {
		return s.CheckOrFold() // a big bet from a bot that never bluffs: let it go
	}
	return b.Dove.Act(s) // everything else: Dove as it was
}

// Fork a starter bot, add one rule, and measure it against the bot it came
// from: the same seed deals the same cards, each deal twice with the seats
// swapped, so what is left is mostly the rule. This is the README's first
// worked example.
func Example_forkAStarterBot() {
	res, err := sdk.Match(respectful{strong: 0.8}, bots.Dove{}, 20000, 1)
	if err != nil {
		panic(err)
	}
	fmt.Println(res)
	// Output: +59.6 ± 6.0 bb/100 over 20000 hands
}

// The rule refined after measuring: fold a little less. (It won less: the
// two are within noise of each other, so the simpler first rule stays.)
func Example_refineTheRule() {
	res, err := sdk.Match(respectful{strong: 0.7}, bots.Dove{}, 20000, 1)
	if err != nil {
		panic(err)
	}
	fmt.Println(res)
	// Output: +47.1 ± 6.9 bb/100 over 20000 hands
}
