package engine

// Card encoding and the small combinatorics every other file leans on.
//
// A card is rank*4 + suit. For hold'em rank 0 is the deuce and rank 12 the
// ace; Kuhn and Leduc reuse the same byte with their own (smaller) decks,
// see rules.go.

type Card uint8

const (
	maxDeck  = 52
	numSuits = 4
)

// binom[n][k] = C(n, k) for the sizes used here (k <= 7).
var binom [maxDeck + 1][8]int

func init() {
	for n := 0; n <= maxDeck; n++ {
		binom[n][0] = 1
		for k := 1; k < 8 && k <= n; k++ {
			binom[n][k] = binom[n-1][k-1] + binom[n-1][k]
		}
	}
}

// combIndex is the colexicographic rank of an ascending-sorted card set:
// a dense index in [0, C(deck, len(cards))) that does not depend on the
// deck size.
func combIndex(sorted []Card) int {
	idx := 0
	for i, c := range sorted {
		idx += binom[c][i+1]
	}
	return idx
}

// sortCards sorts a short card slice ascending in place (insertion sort:
// the slices here are 1-5 cards).
func sortCards(cs []Card) {
	for i := 1; i < len(cs); i++ {
		c := cs[i]
		j := i
		for j > 0 && cs[j-1] > c {
			cs[j] = cs[j-1]
			j--
		}
		cs[j] = c
	}
}

// forEachCombo calls fn with every ascending k-subset of the cards in
// [0, deck) whose bit is clear in used. Subsets come in colex order (top
// card ascending outermost), which with used == 0 is combIndex order.
// fn must not keep the slice.
func forEachCombo(deck, k int, used uint64, fn func(cards []Card)) {
	buf := make([]Card, k)
	if k == 0 {
		fn(buf)
		return
	}
	var rec func(pos, maxCard int)
	rec = func(pos, maxCard int) {
		for c := pos; c < maxCard; c++ {
			if used&(1<<uint(c)) != 0 {
				continue
			}
			buf[pos] = Card(c)
			if pos == 0 {
				fn(buf)
			} else {
				rec(pos-1, c)
			}
		}
	}
	rec(k-1, deck)
}

func cardMask(cs []Card) uint64 {
	var m uint64
	for _, c := range cs {
		m |= 1 << uint(c)
	}
	return m
}
