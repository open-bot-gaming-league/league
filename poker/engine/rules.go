package engine

import "sync"

// MaxRounds bounds the number of betting rounds (hold'em has four).
const MaxRounds = 4

// Rules describes a two-player poker variant as cards only: a deck dealt
// uniformly at random, private hole cards, public board cards arriving
// before each betting round, and a showdown order. Kuhn, Leduc and hold'em
// are three values of this one type, so the solver and the best-response
// code have a single path for all of them.
type Rules struct {
	Name  string
	Deck  int   // cards are 0..Deck-1
	Hole  int   // hole cards per player (1 or 2)
	Board []int // new public cards dealt before each betting round; len = rounds

	// Strength orders showdowns with the full board: higher wins, equal ties.
	Strength func(hole, board []Card) uint32

	// SuitSymmetric says the cards are rank*4+suit and suits are
	// interchangeable, which lets best response solve one board per
	// suit-isomorphism class (see evaluator.outcomes).
	SuitSymmetric bool

	handsOnce sync.Once
	hands     [][]Card // every hole-card set, in combIndex order
	permsOnce sync.Once
	perms     [][]int32 // see handPerms
}

func (r *Rules) Rounds() int { return len(r.Board) }

// BoardSize is the number of public cards visible in the given round.
func (r *Rules) BoardSize(round int) int {
	n := 0
	for i := 0; i <= round; i++ {
		n += r.Board[i]
	}
	return n
}

// Hands lists every hole-card set; a hand's position is its combIndex.
func (r *Rules) Hands() [][]Card {
	r.handsOnce.Do(func() {
		forEachCombo(r.Deck, r.Hole, 0, func(cs []Card) {
			r.hands = append(r.hands, append([]Card(nil), cs...))
		})
	})
	return r.hands
}

// KuhnRules: three cards, one each, no board, high card wins.
func KuhnRules() *Rules {
	return &Rules{
		Name: "kuhn", Deck: 3, Hole: 1, Board: []int{0},
		Strength: func(hole, _ []Card) uint32 { return uint32(hole[0]) },
	}
}

// LeducRules: six cards (three ranks, two suits; rank = card/2), one hole
// card each, one public card before the second round. A pair with the
// board beats any unpaired hand; otherwise the higher rank wins.
func LeducRules() *Rules {
	return &Rules{
		Name: "leduc", Deck: 6, Hole: 1, Board: []int{0, 1},
		Strength: func(hole, board []Card) uint32 {
			rank := uint32(hole[0] >> 1)
			if hole[0]>>1 == board[0]>>1 {
				return 100 + rank
			}
			return rank
		},
	}
}

// HoldemRules: two hole cards, flop/turn/river, best five of seven.
// ranks < 13 gives a short deck using the lowest ranks only; it exists so
// tests can cross-check the suit-symmetry path against full enumeration on
// a deck small enough to enumerate. Real play is ranks = 13.
func HoldemRules(ranks int) *Rules {
	return &Rules{
		Name: "holdem", Deck: ranks * numSuits, Hole: 2, Board: []int{0, 3, 1, 1},
		Strength: func(hole, board []Card) uint32 {
			var cs [7]Card
			n := copy(cs[:], hole)
			n += copy(cs[n:], board)
			return holdemStrength(cs[:n])
		},
		SuitSymmetric: true,
	}
}

// Deal is one sampled chance outcome as the solver sees it: each player's
// bucket in every round, and who wins a showdown.
type Deal struct {
	Bucket [2][MaxRounds]int32
	Sign   int8 // +1 player 0 wins a showdown, -1 player 1 wins, 0 tie

	// The cards themselves (see dealCards) and scratch for the visible
	// board. They live here, in the worker's Deal, because card slices
	// handed to Abstraction.Bucket and Rules.Strength would otherwise be
	// heap-allocated on every deal.
	cards   [2*2 + 5]Card
	visible [5]Card
}

// sampleDeal draws hole cards and a full board uniformly and fills in d.
func (g *Game) sampleDeal(rnd *rng, d *Deal) {
	g.Rules.dealCards(rnd, d)
	g.fillDeal(d)
}

// dealCards draws both players' hole cards and a full board uniformly
// into d.cards: seat 0's hole cards (sorted), seat 1's (sorted), then the
// board in the order it comes.
func (r *Rules) dealCards(rnd *rng, d *Deal) {
	var used uint64
	need := 2*r.Hole + r.BoardSize(r.Rounds()-1)
	for i := 0; i < need; {
		c := rnd.intn(r.Deck)
		if used&(1<<uint(c)) != 0 {
			continue
		}
		used |= 1 << uint(c)
		d.cards[i] = Card(c)
		i++
	}
	sortCards(d.cards[:r.Hole])
	sortCards(d.cards[r.Hole : 2*r.Hole])
}

// hole returns a seat's hole cards and board the full board of a dealt d.
func (r *Rules) hole(d *Deal, seat int) []Card { return d.cards[seat*r.Hole : (seat+1)*r.Hole] }
func (r *Rules) board(d *Deal) []Card {
	return d.cards[2*r.Hole : 2*r.Hole+r.BoardSize(r.Rounds()-1)]
}

// fillDeal sets the buckets and the showdown sign for the cards in d,
// under g's abstraction.
func (g *Game) fillDeal(d *Deal) {
	r := g.Rules
	h0, h1, board := r.hole(d, 0), r.hole(d, 1), r.board(d)
	seen := 0
	for round := 0; round < r.Rounds(); round++ {
		// Buckets depend on the visible board as a set, so each round's
		// prefix of the board is sorted before the lookup.
		seen += r.Board[round]
		visible := d.visible[:seen]
		copy(visible, board)
		sortCards(visible)
		d.Bucket[0][round] = int32(g.Abs.Bucket(round, h0, visible))
		d.Bucket[1][round] = int32(g.Abs.Bucket(round, h1, visible))
	}
	s0, s1 := r.Strength(h0, board), r.Strength(h1, board)
	switch {
	case s0 > s1:
		d.Sign = 1
	case s0 < s1:
		d.Sign = -1
	default:
		d.Sign = 0
	}
}
