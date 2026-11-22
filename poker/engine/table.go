package engine

import "fmt"

// A hand at a table: heads-up no-limit hold'em with real cards, whole
// chips and any legal bet size. The rules (blinds, stacks, the min-raise,
// who may fold or limp) are the game's NLConfig, applied by the same code
// that builds the betting tree (tree_nl.go: step, wagerRange), but a hand
// is not a tree node: a seat may bet any whole number of chips from the
// minimum raise to all-in. cmd/table deals hands; a house bot, which only
// knows its tree's bet menu, plays through a TreeView (translate.go).
//
// Chips are the config's units (NLConfig.SB, BB): integers. Amounts a
// seat bets are "to" amounts: the chips the seat will have in this
// betting round, blinds included, as in "raise to 300".

const cardRanks, cardSuits = "23456789TJQKA", "cdhs"

// String is the usual two-character name, rank then suit: "As", "Td".
func (c Card) String() string {
	if c >= maxDeck {
		return "??"
	}
	return string([]byte{cardRanks[c>>2], cardSuits[c&3]})
}

var categoryNames = [...]string{"high card", "a pair", "two pair", "three of a kind", "a straight",
	"a flush", "a full house", "four of a kind", "a straight flush"}

// TableCards is the number of cards a hand needs: two hole cards each and
// a five-card board.
const TableCards = 9

type TableHand struct {
	g *Game
	// Seat 0's hole cards, seat 1's, then the board in the order it comes.
	cards [TableCards]Card
	st    nlState
	end   nlEnd
	// roundStart is what each seat had put in when the current betting
	// round began, for "to" amounts.
	roundStart [2]int
	Log        []TableAction
}

// TableAction is one action taken in a hand.
type TableAction struct {
	Seat, Round int
	TableOption
}

// TableOption is an action, on the menu or not, with what it costs.
type TableOption struct {
	Action     // ActBet: Amount is the chips beyond the call, any legal size
	Cost   int // chips the seat adds
	To     int // chips the seat will have put in this betting round, blinds included
	// PotFraction, for a bet or raise: Amount as a fraction of the pot
	// after calling, the unit the tree's bet menu is written in.
	PotFraction float64
}

// TableLegal is everything the seat to act may do. A bet or raise may
// take the seat to any whole number of chips in this round from MinTo to
// MaxTo; MaxTo is all-in. When the seat is short of a full minimum raise,
// all-in is its only wager: MinTo == MaxTo.
type TableLegal struct {
	Fold  bool
	Check bool
	Call  int  // chips a call adds; 0 when there is no call (Check, or the config forbids a limp)
	Wager bool // a bet or raise is possible: MinTo and MaxTo are set
	// Raise says what a wager is called: a raise when the other seat has
	// chips in this round (the blinds count), else a bet.
	Raise        bool
	MinTo, MaxTo int
}

// NewTableHand starts a hand with the blinds posted. cards are seat 0's
// hole cards, seat 1's, then the five board cards in the order they will
// come; seat 0 is the small blind and acts first before the flop.
func NewTableHand(g *Game, cards [TableCards]Card) (*TableHand, error) {
	if g.Config == nil || g.Rules.Hole != 2 || g.Rules.Rounds() != 4 {
		return nil, fmt.Errorf("engine: a table needs no-limit hold'em, not %s", g.Rules.Name)
	}
	var used uint64
	for _, c := range cards {
		if int(c) >= g.Rules.Deck || used&(1<<c) != 0 {
			return nil, fmt.Errorf("engine: cards %v are not nine different cards of the deck", cards)
		}
		used |= 1 << c
	}
	h := &TableHand{g: g, cards: cards, st: g.Config.newNLState()}
	sortCards(h.cards[0:2])
	sortCards(h.cards[2:4])
	return h, nil
}

// Stack is what each seat starts every hand with, in chips.
func (g *Game) Stack() int { return g.Config.StackBB * g.Config.BB }

// Over reports whether the hand has ended, by a fold or a showdown.
func (h *TableHand) Over() bool { return h.end != nlGoesOn }

// Showdown reports whether the hand ended with the cards shown.
func (h *TableHand) Showdown() bool { return h.end == nlShowdown }

// ToAct is the seat to act, or -1 when the hand is over.
func (h *TableHand) ToAct() int {
	if h.Over() {
		return -1
	}
	return h.st.toAct
}

// Round is the betting round the hand is in (0 before the flop); for a
// finished hand, the round it ended in.
func (h *TableHand) Round() int { return h.st.round }

func (h *TableHand) Hole(seat int) []Card { return h.cards[2*seat : 2*seat+2] }

// Board is the part of the board the players can see: the current round's
// cards, or all five at a showdown (an all-in is run out).
func (h *TableHand) Board() []Card {
	if h.Showdown() {
		return h.cards[4:]
	}
	return h.cards[4 : 4+h.g.Rules.BoardSize(h.Round())]
}

// FullBoard is all five board cards, seen or not: for a record of the
// hand, never for a player still in it.
func (h *TableHand) FullBoard() []Card { return h.cards[4:] }

// Put is the chips a seat has put in so far, blinds included.
func (h *TableHand) Put(seat int) int { return h.g.Config.contributed(&h.st, seat) }

// Pot is both seats' chips in, uncalled bets included.
func (h *TableHand) Pot() int { return h.st.pot }

// InRound is the chips a seat has put in during this betting round,
// blinds included: the base of a "raise to" amount.
func (h *TableHand) InRound(seat int) int { return h.Put(seat) - h.roundStart[seat] }

// Legal is what the seat to act may do; the zero TableLegal when the
// hand is over.
func (h *TableHand) Legal() TableLegal {
	if h.Over() {
		return TableLegal{}
	}
	c, st, me := h.g.Config, &h.st, h.st.toAct
	l := TableLegal{Fold: c.canFold(st), Raise: h.InRound(1-me) > 0}
	if c.canCall(st) {
		l.Check, l.Call = st.call == 0, st.call
	}
	if minExtra, maxExtra, ok := c.wagerRange(st); ok {
		base := h.InRound(me) + st.call
		l.Wager, l.MinTo, l.MaxTo = true, base+min(minExtra, maxExtra), base+maxExtra
	}
	return l
}

// option prices an action for the seat to act.
func (h *TableHand) option(act Action) TableOption {
	st, me := &h.st, h.st.toAct
	o := TableOption{Action: act}
	switch act.Kind {
	case ActCall:
		o.Cost = st.call
	case ActBet:
		o.Cost = st.call + int(act.Amount)
		o.PotFraction = float64(act.Amount) / float64(st.pot+st.call)
	}
	o.To = h.InRound(me) + o.Cost
	return o
}

// Options lists the bet menu for the seat to act: fold and check or call
// as the rules allow, then the menu's wagers, ascending, ending with
// all-in, each computed from this hand's own pot. While both seats have
// stayed on the menu these are exactly the tree's actions at the hand's
// node. Empty when the hand is over.
func (h *TableHand) Options() []TableOption {
	if h.Over() {
		return nil
	}
	acts := h.g.Config.menuActions(&h.st)
	out := make([]TableOption, len(acts))
	for i, act := range acts {
		out[i] = h.option(act)
	}
	return out
}

// Act takes the a-th of Options for the seat to act.
func (h *TableHand) Act(a int) error {
	if h.Over() {
		return fmt.Errorf("engine: the hand is over")
	}
	opts := h.Options()
	if a < 0 || a >= len(opts) {
		return fmt.Errorf("engine: action %d of %d", a, len(opts))
	}
	return h.play(opts[a])
}

// Play makes a move for the seat to act: ActFold, ActCheck, ActCall, or
// ActBet to take the seat to `to` chips in this round (to is ignored for
// the others). It refuses anything Legal does not allow, and changes
// nothing then.
func (h *TableHand) Play(kind ActionKind, to int) error {
	if h.Over() {
		return fmt.Errorf("engine: the hand is over")
	}
	l, st := h.Legal(), &h.st
	act := Action{Kind: kind}
	switch kind {
	case ActFold:
		if !l.Fold {
			return fmt.Errorf("engine: no fold here: there is nothing to call, or the rules forbid it")
		}
	case ActCheck:
		if !l.Check {
			return fmt.Errorf("engine: no check here: %d to call", st.call)
		}
	case ActCall:
		if l.Call == 0 {
			return fmt.Errorf("engine: no call here: nothing to call, or the rules forbid a limp")
		}
		act.Amount, act.AllIn = int32(st.call), st.call == st.behind[st.toAct]
	case ActBet:
		switch {
		case !l.Wager:
			return fmt.Errorf("engine: no bet or raise here")
		case to < l.MinTo || to > l.MaxTo:
			return fmt.Errorf("engine: a wager here goes to %d-%d chips (all-in %d), not %d", l.MinTo, l.MaxTo, l.MaxTo, to)
		}
		act.Amount, act.AllIn = int32(to-h.InRound(st.toAct)-st.call), to == l.MaxTo
	default:
		return fmt.Errorf("engine: no action kind %d", kind)
	}
	return h.play(h.option(act))
}

// play applies a priced, legal option.
func (h *TableHand) play(o TableOption) error {
	me, round := h.st.toAct, h.st.round
	next, end := h.g.Config.step(h.st, o.Action)
	if next.behind[me] < 0 {
		return fmt.Errorf("engine: %v costs %d chips with %d behind", o.Action, o.Cost, h.st.behind[me])
	}
	h.Log = append(h.Log, TableAction{Seat: me, Round: round, TableOption: o})
	// A call that puts a seat all-in ends the betting: run the board out.
	if end == nlGoesOn && (next.behind[0] == 0 || next.behind[1] == 0) && next.call == 0 {
		end = nlShowdown
	}
	h.st, h.end = next, end
	if next.round != round {
		h.roundStart = [2]int{h.Put(0), h.Put(1)}
	}
	if end == nlShowdown && h.Put(0) != h.Put(1) {
		return fmt.Errorf("engine: showdown with %d and %d chips in", h.Put(0), h.Put(1))
	}
	return nil
}

// Net is the chips a seat has won (negative: lost) in a finished hand.
// A folder loses what it put in; at a showdown the better hand wins what
// the other put in, and equal hands win nothing. The two seats' nets sum
// to zero.
func (h *TableHand) Net(seat int) int {
	var seat0 int
	switch h.end {
	case nlGoesOn:
		return 0
	case nlFolded:
		folder := h.st.toAct
		seat0 = h.Put(folder)
		if folder == 0 {
			seat0 = -seat0
		}
	case nlShowdown:
		s0, s1 := h.strength(0), h.strength(1)
		switch {
		case s0 > s1:
			seat0 = h.Put(1)
		case s0 < s1:
			seat0 = -h.Put(0)
		}
	}
	if seat == 0 {
		return seat0
	}
	return -seat0
}

func (h *TableHand) strength(seat int) uint32 { return h.g.Rules.Strength(h.Hole(seat), h.cards[4:]) }

// HandName is what a seat holds with the full board: "two pair", "a
// flush".
func (h *TableHand) HandName(seat int) string { return categoryNames[h.strength(seat)>>catShift] }

// Bucket is the card-abstraction bucket of a seat's cards in the current
// round: with a TreeView's node, the infoset a blueprint is asked about.
func (h *TableHand) Bucket(seat int) int {
	var visible [5]Card
	n := copy(visible[:], h.cards[4:4+h.g.Rules.BoardSize(h.Round())])
	sortCards(visible[:n])
	return h.g.Abs.Bucket(h.Round(), h.Hole(seat), visible[:n])
}
