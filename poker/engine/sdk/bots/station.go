package bots

import "github.com/open-bot-gaming-league/league/poker/engine/sdk"

// Station is a calling station: it calls almost every bet, and raises
// only now and then with a strong hand. It folds just one thing: a river
// bet of the pot or more when it has nothing at all.
type Station struct{}

func (Station) Act(s sdk.State) sdk.Action {
	strong := s.Made() >= sdk.TwoPair || (s.Round == sdk.Preflop && s.Made() == sdk.Pair)
	if strong && s.Legal.CanRaise() && s.Rand.IntN(10) == 0 {
		return s.RaisePot(0.5)
	}
	// Pot includes the bet: a bet of the pot before it is half of Pot.
	if s.Round == sdk.River && s.Made() == sdk.HighCard && 2*s.Legal.Call >= s.Pot {
		return s.CheckOrFold()
	}
	return s.CheckOrCall()
}
