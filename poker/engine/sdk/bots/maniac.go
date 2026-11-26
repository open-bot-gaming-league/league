package bots

import "github.com/open-bot-gaming-league/league/poker/engine/sdk"

// Maniac bets and raises with anything: whenever it can, three times in
// ten it goes all-in and half the time it raises the pot. Otherwise it
// checks or calls. It never folds.
type Maniac struct{}

func (Maniac) Act(s sdk.State) sdk.Action {
	if s.Legal.CanRaise() {
		switch n := s.Rand.IntN(10); {
		case n < 3:
			return s.AllIn()
		case n < 8:
			return s.RaisePot(1)
		}
	}
	return s.CheckOrCall()
}
