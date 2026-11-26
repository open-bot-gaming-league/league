package bots

import "github.com/open-bot-gaming-league/league/poker/engine/sdk"

// Clam plays premium hands and nothing else. Before the flop it raises
// with a big pair or two big cards and folds the rest (checking when it
// can); after the flop it bets two pair or better and gives up on
// anything less as soon as it meets a bet.
type Clam struct{}

func (Clam) Act(s sdk.State) sdk.Action {
	if s.Round == sdk.Preflop {
		if premium(s.Hole) {
			return s.RaisePot(1)
		}
		return s.CheckOrFold()
	}
	if s.Made() >= sdk.TwoPair {
		return s.RaisePot(0.75)
	}
	return s.CheckOrFold()
}

// premium is a pair of nines or better, or two cards queen or higher:
// ace-king, ace-queen, king-queen.
func premium(hole [2]sdk.Card) bool {
	a, b := hole[0].Rank(), hole[1].Rank()
	if a == b {
		return a >= sdk.Nine
	}
	return min(a, b) >= sdk.Queen
}
