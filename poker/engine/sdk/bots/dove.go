package bots

import "github.com/open-bot-gaming-league/league/poker/engine/sdk"

// Dove never bluffs: it bets and raises in proportion to how strong its
// hand is, calls a bet when the price is fair for its hand, and folds a
// weak hand to a bet it cannot afford. Strength is equity: the share of
// pots its hand wins against one random hand, the rest of the board dealt
// out (State.Equity). It does not guess what the other seat holds.
type Dove struct{}

const (
	doveSamples = 300  // random deals per decision for the equity estimate
	doveBet     = 0.65 // bet or raise with at least this much equity
	doveMargin  = 0.05 // call when equity beats the pot odds by this much
)

func (Dove) Act(s sdk.State) sdk.Action {
	e := s.Equity(doveSamples)
	// The bigger the edge, the bigger the bet: 0.65 equity bets a third of
	// the pot, 0.9 and up the whole pot.
	if e >= doveBet && s.Legal.CanRaise() {
		size := 1.0 / 3
		if e >= 0.9 {
			size = 1
		} else if e >= 0.78 {
			size = 2.0 / 3
		}
		return s.RaisePot(size)
	}
	if s.Legal.Call == 0 {
		return s.CheckOrCall() // nothing to call: check
	}
	// Pot odds: the share of the pot after calling that the call is.
	odds := float64(s.Legal.Call) / float64(s.Pot+s.Legal.Call)
	if e >= odds+doveMargin {
		return s.CheckOrCall()
	}
	return s.CheckOrFold()
}
