package bots

import (
	"strings"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

// Shark is tight and aggressive. Before the flop it plays the hands in its
// chart (below: edit it hand by hand) and raises them; after the flop, if
// it made the last raise before the flop, it bets the flop whether it hit
// or not (a continuation bet), and after that keeps betting only with a
// real made hand. Called and holding nothing, it slows down.
type Shark struct{}

// The chart. A hand is its two ranks, high first, and "s" (same suit) or
// "o" (offsuit); a pair is just its ranks: "AKs", "T9o", "77".
var (
	// Small blind, first in: raise these (to 2.5 bb), fold the rest.
	sharkOpen = hands(
		"AA KK QQ JJ TT 99 88 77 66 55 44 33 22",
		"AKs AQs AJs ATs A9s A8s A7s A6s A5s A4s A3s A2s AKo AQo AJo ATo A9o A8o A7o A5o",
		"KQs KJs KTs K9s K8s K7s KQo KJo KTo K9o",
		"QJs QTs Q9s Q8s QJo QTo Q9o JTs J9s J8s JTo J9o T9s T8s T9o 98s 97s 87s 76s 65s",
	)
	// Big blind, facing a raise: re-raise (pot) with these ...
	sharkThreeBet = hands("AA KK QQ JJ TT", "AKs AQs AJs KQs AKo AQo")
	// ... call with these, fold the rest.
	sharkDefend = hands(
		"99 88 77 66 55 44 33 22",
		"ATs A9s A8s A7s A6s A5s A4s A3s A2s AJo ATo A9o",
		"KJs KTs K9s KQo KJo KTo QJs QTs Q9s QJo JTs J9s T9s T8s 98s 87s 76s 65s",
	)
	// Facing a re-raise: go all in with these, fold the rest.
	sharkAllIn = hands("AA KK QQ JJ", "AKs AKo")
)

func (Shark) Act(s sdk.State) sdk.Action {
	if s.Round == sdk.Preflop {
		return sharkPreflop(s)
	}
	strong := realHand(s)
	if !s.Legal.Check { // facing a bet
		switch {
		case s.Made() >= sdk.Trips && usesHoleCard(s):
			return s.RaisePot(1)
		case strong:
			return s.CheckOrCall()
		}
		return s.CheckOrFold()
	}
	// Checked to, or first to act.
	if s.Round == sdk.Flop && raisedLastPreflop(s) && !betThisRound(s) {
		return s.RaisePot(0.5) // the continuation bet: hit or not
	}
	if strong {
		return s.RaisePot(0.66)
	}
	return s.CheckOrFold() // called and nothing: slow down
}

func sharkPreflop(s sdk.State) sdk.Action {
	h := handKey(s.Hole)
	raises := 0
	for _, e := range s.History {
		if e.Kind == sdk.Raise {
			raises++
		}
	}
	switch {
	case raises >= 2 || (raises == 1 && s.Seat == sdk.SmallBlind): // facing a re-raise
		if sharkAllIn[h] {
			return s.AllIn()
		}
		return s.CheckOrFold()
	case s.Seat == sdk.SmallBlind: // first in
		if sharkOpen[h] {
			return s.RaiseTo(5 * s.BigBlind / 2)
		}
		return s.CheckOrFold()
	case raises == 1: // big blind, facing the small blind's raise
		if sharkThreeBet[h] {
			return s.RaisePot(1)
		}
		if sharkDefend[h] {
			return s.CheckOrCall()
		}
		return s.CheckOrFold()
	}
	// Big blind, the small blind only called: raise the strong ones, check the rest.
	if sharkThreeBet[h] {
		return s.RaisePot(1)
	}
	return s.CheckOrCall()
}

// realHand is two pair or better, or a pair at least as high as the
// board's highest card (top pair or an overpair), built with a hole card.
func realHand(s sdk.State) bool {
	if !usesHoleCard(s) {
		return false
	}
	if s.Made() >= sdk.TwoPair {
		return true
	}
	top := 0
	for _, c := range s.Board {
		top = max(top, c.Rank())
	}
	a, b := s.Hole[0].Rank(), s.Hole[1].Rank()
	if a == b {
		return a >= top // a pocket pair: an overpair
	}
	return a == top || b == top // top pair
}

// usesHoleCard reports whether a hole card plays: a pocket pair, a hole
// card paired with the board, or a straight or flush.
func usesHoleCard(s sdk.State) bool {
	a, b := s.Hole[0].Rank(), s.Hole[1].Rank()
	if a == b || s.Made() >= sdk.Straight {
		return true
	}
	for _, c := range s.Board {
		if c.Rank() == a || c.Rank() == b {
			return true
		}
	}
	return false
}

// raisedLastPreflop reports whether the last raise before the flop was
// this seat's.
func raisedLastPreflop(s sdk.State) bool {
	last := -1
	for _, e := range s.History {
		if e.Round == sdk.Preflop && e.Kind == sdk.Raise {
			last = e.Seat
		}
	}
	return last == s.Seat
}

// betThisRound reports whether anyone has bet in this round yet.
func betThisRound(s sdk.State) bool {
	for _, e := range s.History {
		if e.Round == s.Round && e.Kind == sdk.Raise {
			return true
		}
	}
	return false
}

// handKey names two hole cards as the chart does: "AKs", "T9o", "77".
func handKey(hole [2]sdk.Card) string {
	const ranks = "23456789TJQKA"
	hi, lo := hole[0], hole[1]
	if lo.Rank() > hi.Rank() {
		hi, lo = lo, hi
	}
	key := string([]byte{ranks[hi.Rank()], ranks[lo.Rank()]})
	switch {
	case hi.Rank() == lo.Rank():
		return key
	case hi.Suit() == lo.Suit():
		return key + "s"
	}
	return key + "o"
}

// hands is a set of chart hands, from space-separated lists.
func hands(lists ...string) map[string]bool {
	set := map[string]bool{}
	for _, list := range lists {
		for _, h := range strings.Fields(list) {
			set[h] = true
		}
	}
	return set
}
