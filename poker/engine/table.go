package engine

import (
	"fmt"
	"math"
)

// A hand at a table: heads-up no-limit hold'em played on a game's betting
// tree with real cards and whole chips. This is the serving side of a
// blueprint: cmd/table deals hands, a person picks among the tree's
// actions for one seat and a blueprint for the other. The players are
// confined to the tree's bet sizes, so every state of play is a tree node
// and the blueprint always has an answer.
//
// Chips are the tree's own units (NLConfig.SB, BB, and Action.Amount):
// integers. The tree's payoffs are float32 big blinds; a hand keeps its
// own chip count and checks it against the tree at the end.

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
	node  int32
	put   [2]int // chips each seat has put in, blinds included
	// roundStart is put as it stood when the current betting round began,
	// for "raise to" amounts.
	roundStart [2]int
	Log        []TableAction
}

// TableAction is one action taken in a hand.
type TableAction struct {
	Seat, Round int
	TableOption
}

// TableOption is an action open to the seat to act, with what it costs.
type TableOption struct {
	Action
	Cost int // chips the seat adds
	To   int // chips the seat will have put in this betting round, blinds included
	// PotFraction, for a bet or raise: its size as a fraction of the pot
	// after calling, the unit the tree's bet menu is written in.
	PotFraction float64
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
	h := &TableHand{g: g, cards: cards, put: [2]int{int(g.Tree.Posted[0]), int(g.Tree.Posted[1])}}
	sortCards(h.cards[0:2])
	sortCards(h.cards[2:4])
	return h, nil
}

// Stack is what each seat starts every hand with, in chips.
func (g *Game) Stack() int { return g.Config.StackBB * g.Config.BB }

func (h *TableHand) cur() *Node { return &h.g.Tree.Nodes[h.node] }

// Over reports whether the hand has ended, by a fold or a showdown.
func (h *TableHand) Over() bool { return h.cur().Kind != Decision }

// Showdown reports whether the hand ended with the cards shown.
func (h *TableHand) Showdown() bool { return h.cur().Kind == Showdown }

// ToAct is the seat to act, or -1 when the hand is over.
func (h *TableHand) ToAct() int {
	if h.Over() {
		return -1
	}
	return int(h.cur().Player)
}

// Round is the betting round the hand is in (0 before the flop); for a
// finished hand, the round it ended in.
func (h *TableHand) Round() int { return int(h.cur().Round) }

// Node is the betting-tree node the hand is at.
func (h *TableHand) Node() *Node { return h.cur() }

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
func (h *TableHand) Put(seat int) int { return h.put[seat] }

// Options lists what the seat to act may do, in the tree's action order.
// Empty when the hand is over.
func (h *TableHand) Options() []TableOption {
	n := h.cur()
	if n.Kind != Decision {
		return nil
	}
	me := int(n.Player)
	call := h.put[1-me] - h.put[me]
	out := make([]TableOption, n.NumActions)
	for a := range out {
		act := h.g.Tree.Actions[int(n.FirstChild)+a]
		o := TableOption{Action: act}
		switch act.Kind {
		case ActCall:
			o.Cost = int(act.Amount)
		case ActBet:
			o.Cost = call + int(act.Amount)
			o.PotFraction = float64(act.Amount) / float64(h.put[0]+h.put[1]+call)
		}
		o.To = h.put[me] + o.Cost - h.roundStart[me]
		out[a] = o
	}
	return out
}

// Act takes the a-th of Options for the seat to act.
func (h *TableHand) Act(a int) error {
	n := h.cur()
	if n.Kind != Decision {
		return fmt.Errorf("engine: the hand is over")
	}
	if a < 0 || a >= int(n.NumActions) {
		return fmt.Errorf("engine: action %d of %d", a, n.NumActions)
	}
	me := int(n.Player)
	opt := h.Options()[a]
	if opt.Kind == ActCall && opt.Cost != h.put[1-me]-h.put[me] {
		return fmt.Errorf("engine: the tree's call is %d chips, the table's %d", opt.Cost, h.put[1-me]-h.put[me])
	}
	if h.put[me]+opt.Cost > h.g.Stack() {
		return fmt.Errorf("engine: action costs %d chips with %d behind", opt.Cost, h.g.Stack()-h.put[me])
	}
	h.put[me] += opt.Cost
	h.Log = append(h.Log, TableAction{Seat: me, Round: int(n.Round), TableOption: opt})
	h.node = h.g.Tree.Children[int(n.FirstChild)+a]
	next := h.cur()
	if next.Round != n.Round {
		h.roundStart = h.put
	}
	if next.Kind == Decision {
		return nil
	}
	// The tree's payoff and the chips on the table must be the same thing.
	lost := h.put[0]
	if next.Kind == Fold {
		lost = h.put[next.Player]
	} else if h.put[0] != h.put[1] {
		return fmt.Errorf("engine: showdown with %d and %d chips in", h.put[0], h.put[1])
	}
	if want := int(math.Round(float64(next.Stake) * float64(h.g.Config.BB))); want != lost {
		return fmt.Errorf("engine: the tree pays %d chips, the table counted %d", want, lost)
	}
	return nil
}

// Net is the chips a seat has won (negative: lost) in a finished hand.
// A folder loses what it put in; at a showdown the better hand wins what
// the other put in, and equal hands win nothing. The two seats' nets sum
// to zero.
func (h *TableHand) Net(seat int) int {
	n := h.cur()
	var seat0 int
	switch n.Kind {
	case Decision:
		return 0
	case Fold:
		seat0 = h.put[n.Player]
		if n.Player == 0 {
			seat0 = -seat0
		}
	case Showdown:
		s0, s1 := h.strength(0), h.strength(1)
		switch {
		case s0 > s1:
			seat0 = h.put[1]
		case s0 < s1:
			seat0 = -h.put[0]
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
// round: with Node, the infoset a blueprint is asked about.
func (h *TableHand) Bucket(seat int) int {
	var visible [5]Card
	n := copy(visible[:], h.cards[4:4+h.g.Rules.BoardSize(h.Round())])
	sortCards(visible[:n])
	return h.g.Abs.Bucket(h.Round(), h.Hole(seat), visible[:n])
}
