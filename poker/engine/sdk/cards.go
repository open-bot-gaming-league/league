package sdk

import "github.com/open-bot-gaming-league/league/poker/engine"

// Card is a playing card. c.Rank() is 0 (Two) to 12 (Ace), c.Suit() 0 to
// 3 (clubs, diamonds, hearts, spades), c.String() is "As", "Td", "2c".
type Card = engine.Card

// Ranks, as Card.Rank returns them.
const (
	Two = iota
	Three
	Four
	Five
	Six
	Seven
	Eight
	Nine
	Ten
	Jack
	Queen
	King
	Ace
)

// ParseCard reads a card's name: "As", "Td", "2c".
func ParseCard(name string) (Card, error) { return engine.ParseCard(name) }

// Category is the kind of a made hand, weakest first.
type Category int

const (
	HighCard Category = iota
	Pair
	TwoPair
	Trips
	Straight
	Flush
	FullHouse
	Quads
	StraightFlush
)

// String is the category in words: "two pair", "a flush".
func (c Category) String() string { return engine.CategoryName(int(c)) }

var holdem = engine.HoldemRules(13)

// Strength is the showdown value of the best five cards from hole and a
// board of 3 to 5 cards: of two hands on the same board, the higher
// value wins and equal values split the pot.
func Strength(hole, board []Card) uint32 { return holdem.Strength(hole, board) }

// Made is the category of the best hand the seat holds now, the board
// included (a paired board is a Pair for everyone). Before the flop it
// is Pair or HighCard, from the hole cards alone.
func (s State) Made() Category {
	if len(s.Board) == 0 {
		if s.Hole[0].Rank() == s.Hole[1].Rank() {
			return Pair
		}
		return HighCard
	}
	return Category(engine.HandCategory(Strength(s.Hole[:], s.Board)))
}
