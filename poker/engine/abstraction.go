package engine

import "fmt"

// Abstraction maps a player's cards to a bucket per round. The solver's
// information set is (betting node, bucket of the acting player in the
// node's round); nothing about earlier rounds is remembered unless the
// bucket itself encodes it.
//
// Bucket must depend only on the hole cards and the visible board as sets
// (both arrive sorted ascending). For suit-symmetric rules it must also be
// invariant under relabelling suits; best response relies on that.
type Abstraction interface {
	NumBuckets(round int) int
	Bucket(round int, hole, board []Card) int
	// ID names the mapping, so a blueprint is only ever read with the
	// abstraction it was trained on.
	ID() string
}

// kuhnAbs is lossless: the bucket is the card.
type kuhnAbs struct{}

func (kuhnAbs) ID() string                       { return "kuhn-lossless" }
func (kuhnAbs) NumBuckets(int) int               { return 3 }
func (kuhnAbs) Bucket(_ int, hole, _ []Card) int { return int(hole[0]) }

// leducAbs is lossless up to suits: own rank before the public card, (own
// rank, public rank) after it.
type leducAbs struct{}

func (leducAbs) ID() string { return "leduc-lossless" }

func (leducAbs) NumBuckets(round int) int {
	if round == 0 {
		return 3
	}
	return 9
}

func (leducAbs) Bucket(round int, hole, board []Card) int {
	rank := int(hole[0] >> 1)
	if round == 0 {
		return rank
	}
	return rank*3 + int(board[0]>>1)
}

// preflopClass is the lossless preflop abstraction for a deck of the given
// rank count: pairs, suited and offsuit combinations of two ranks, laid out
// on a ranks x ranks grid (169 classes for the full deck). a < b.
func preflopClass(ranks int, a, b Card) int {
	lo, hi := int(a>>2), int(b>>2) // a < b so rank(a) <= rank(b)
	if lo == hi || a&3 == b&3 {
		return hi*ranks + lo // pairs on the diagonal, suited below it
	}
	return lo*ranks + hi // offsuit above it
}

// CategoryAbstraction is a deliberately crude hold'em abstraction that
// needs no precomputed files: lossless preflop classes, then the made-hand
// category (high card ... straight flush) of hole plus visible board. It
// exists for tests and smoke runs; real runs use a TableAbstraction built
// by cmd/buckets.
type CategoryAbstraction struct{ Ranks int }

func (c CategoryAbstraction) ID() string { return fmt.Sprintf("category-%d", c.Ranks) }

func (c CategoryAbstraction) NumBuckets(round int) int {
	if round == 0 {
		return c.Ranks * c.Ranks
	}
	return catStraightFlush + 1
}

func (c CategoryAbstraction) Bucket(round int, hole, board []Card) int {
	if round == 0 {
		return preflopClass(c.Ranks, hole[0], hole[1])
	}
	var cs [7]Card
	n := copy(cs[:], hole)
	n += copy(cs[n:], board)
	return int(holdemStrength(cs[:n]) >> catShift)
}
