// Package sdk is for writing a bot that plays the Open Bot Gaming
// League's poker: heads-up no-limit hold'em, blinds 50/100, 50 big blind
// stacks reset every hand, any bet size.
//
// A bot is one method, Act(State) Action: it is shown everything its seat
// can see and returns a move. The same bot runs in two places:
//
//   - Match plays two bots against each other on this machine, as many
//     hands as you like, each deal twice with the seats swapped, and
//     reports the first bot's win rate in bb/100 with its standard error.
//   - Table plays your bot at your table on the league's server, over the
//     HTTP API the table page uses, so you can watch it there.
//
// The rules are not restated here: locally every move goes through the
// engine's TableHand, the code the league's table server runs, and at the
// table the server itself checks it. State.Legal is the engine's answer to
// "what may this seat do now".
//
// See README.md in this directory for a walk-through, and the bots
// package for three short starter bots.
package sdk

import (
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

// Bot plays one seat. Act is called whenever the bot's seat is to act and
// must return a legal move (see Legal); the State helpers (CheckOrCall,
// RaiseTo, RaisePot, AllIn, CheckOrFold) only ever build legal ones. A bot
// may keep state between calls; State.Hand says when a new hand starts.
type Bot interface {
	Act(s State) Action
}

// Seats. The small blind is the button: it acts first before the flop
// and last after it.
const (
	SmallBlind = 0
	BigBlind   = 1
)

// Round is a betting round.
type Round int

const (
	Preflop Round = iota
	Flop
	Turn
	River
)

var roundNames = [...]string{"preflop", "flop", "turn", "river"}

func (r Round) String() string { return roundNames[r] }

// State is everything a seat can see when it is to act. Chips are
// integers; BigBlind chips are one big blind.
type State struct {
	Hand  int   // which hand this is, from 1: a new number means a new hand
	Seat  int   // SmallBlind or BigBlind
	Round Round // the betting round
	Hole  [2]Card
	Board []Card // the cards dealt so far: none, 3, 4 or 5, in the order they came

	Pot        int // chips both seats have put in this hand, blinds and bets not yet called included
	Stack      int // your chips behind: not yet put in
	OppStack   int
	InRound    int // chips you have put in during this betting round, blinds included
	OppInRound int

	Legal   Legal   // every move open to you now
	History []Event // this hand's moves so far, oldest first (posting the blinds is not a move)

	BigBlind int // chips in one big blind
	// Rand is this seat's random source, seeded by the match or the
	// table run: draw from it, not from a global, and a seed replays
	// the same game.
	Rand *rand.Rand
}

// Legal is the moves open to the seat to act. Betting is open, with no
// bet menu: a bet or raise takes the seat to any whole number of chips
// in this round from MinTo to MaxTo;
// MaxTo is all-in. When all-in is short of a full minimum raise,
// MinTo == MaxTo.
type Legal struct {
	Fold  bool // only when facing a bet
	Check bool
	Call  int // chips a call adds; 0 when there is nothing to call
	MinTo int // 0 when no bet or raise is possible (you face an all-in, or a call puts you all-in)
	MaxTo int
}

// CanRaise reports whether a bet or raise is possible.
func (l Legal) CanRaise() bool { return l.MaxTo > 0 }

// Allows reports whether a is one of the moves open: the check the
// engine makes before it plays a move, for a referee that would rather
// replace a bad move than stop the game.
func (l Legal) Allows(a Action) bool {
	switch a.Kind {
	case Fold:
		return l.Fold
	case Check:
		return l.Check
	case Call:
		return l.Call > 0
	case Raise:
		return l.CanRaise() && a.To >= l.MinTo && a.To <= l.MaxTo
	}
	return false
}

// Kind is the kind of a move. Raise is a bet or a raise: the same move,
// named by whether the other seat has chips in this round.
type Kind int

const (
	Fold Kind = iota
	Check
	Call
	Raise
)

var kindNames = [...]string{"fold", "check", "call", "raise"}

func (k Kind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return fmt.Sprintf("Kind(%d)", int(k))
	}
	return kindNames[k]
}

// Action is a move. For a Raise, To is the chips the seat will have put
// in during this betting round, blinds included, as in "raise to 300";
// it is ignored for the other kinds.
type Action struct {
	Kind Kind
	To   int
}

func (a Action) String() string {
	if a.Kind == Raise {
		return fmt.Sprintf("raise to %d", a.To)
	}
	return a.Kind.String()
}

// Event is one move of the hand: who made it, in which round, and what
// it cost.
type Event struct {
	Seat  int
	Round Round
	Kind  Kind
	To    int // the seat's chips in this round after the move, blinds included
	Cost  int // chips the move added
}

// CheckOrCall checks if it can, else calls. (Where the rules allowed
// neither it would fold; the league's table always allows one.)
func (s State) CheckOrCall() Action {
	switch {
	case s.Legal.Check:
		return Action{Kind: Check}
	case s.Legal.Call > 0:
		return Action{Kind: Call}
	}
	return Action{Kind: Fold}
}

// CheckOrFold checks if it can, else folds.
func (s State) CheckOrFold() Action {
	if s.Legal.Check || !s.Legal.Fold {
		return s.CheckOrCall()
	}
	return Action{Kind: Fold}
}

// RaiseTo bets or raises to `to` chips in this round, moved into the
// legal range; where no raise is possible, it checks or calls.
func (s State) RaiseTo(to int) Action {
	if !s.Legal.CanRaise() {
		return s.CheckOrCall()
	}
	return Action{Kind: Raise, To: min(max(to, s.Legal.MinTo), s.Legal.MaxTo)}
}

// RaisePot bets or raises by f times the pot after calling, the unit the
// table's bet menu and sizing buttons use: RaisePot(0.5) is a half-pot
// bet, RaisePot(1) a pot-sized raise. The amount is moved into the legal
// range, as RaiseTo.
func (s State) RaisePot(f float64) Action {
	extra := int(math.Round(f * float64(s.Pot+s.Legal.Call)))
	return s.RaiseTo(s.InRound + s.Legal.Call + extra)
}

// AllIn puts every chip in: a raise to Legal.MaxTo, or a call when no
// raise is possible.
func (s State) AllIn() Action { return s.RaiseTo(s.Legal.MaxTo) }

// Facing reports whether the other seat's last move this round was a
// bet or raise you have not answered yet.
func (s State) Facing() bool {
	for i := len(s.History) - 1; i >= 0; i-- {
		e := s.History[i]
		if e.Round != s.Round {
			break
		}
		if e.Seat != s.Seat {
			return e.Kind == Raise
		}
	}
	return false
}

// Random plays a random legal move, any size: for trying a bot against
// every kind of spot, not for winning.
type Random struct{}

func (Random) Act(s State) Action {
	switch n := s.Rand.IntN(10); {
	case n < 2:
		return s.CheckOrFold()
	case n < 6:
		return s.CheckOrCall()
	case n < 7:
		return s.AllIn()
	}
	return s.RaiseTo(s.Legal.MinTo + s.Rand.IntN(s.Legal.MaxTo-s.Legal.MinTo+1))
}

// kindOf is an engine action kind as a Kind.
func kindOf(k engine.ActionKind) Kind {
	switch k {
	case engine.ActFold:
		return Fold
	case engine.ActCheck:
		return Check
	case engine.ActCall:
		return Call
	}
	return Raise
}

// engineKind is a Kind as the engine's action kind.
func engineKind(k Kind) (engine.ActionKind, error) {
	switch k {
	case Fold:
		return engine.ActFold, nil
	case Check:
		return engine.ActCheck, nil
	case Call:
		return engine.ActCall, nil
	case Raise:
		return engine.ActBet, nil
	}
	return 0, fmt.Errorf("sdk: no move kind %d", int(k))
}
