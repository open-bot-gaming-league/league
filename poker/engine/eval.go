package engine

import "math/bits"

// Hold'em showdown strength for 5 to 7 cards. Higher is better; equal
// values tie. The top bits hold the category, the low 26 bits break ties
// inside it.

const (
	catHighCard = iota
	catPair
	catTwoPair
	catTrips
	catStraight
	catFlush
	catFullHouse
	catQuads
	catStraightFlush
)

const catShift = 26

// holdemStrength evaluates the best five-card hand among cards (5-7 cards,
// rank = card>>2 in 0..12 with 12 the ace, suit = card&3).
func holdemStrength(cards []Card) uint32 {
	var suitMask [numSuits]uint32
	var count [13]uint8
	var all uint32
	for _, c := range cards {
		r := uint(c >> 2)
		suitMask[c&3] |= 1 << r
		count[r]++
		all |= 1 << r
	}
	// With at most 7 cards a flush rules out quads and full houses, so the
	// flush check can come first.
	for s := 0; s < numSuits; s++ {
		if bits.OnesCount32(suitMask[s]) >= 5 {
			if hi := straightHigh(suitMask[s]); hi >= 0 {
				return catStraightFlush<<catShift | uint32(hi)
			}
			return catFlush<<catShift | topBits(suitMask[s], 5)
		}
	}
	quad, trip1, trip2, pair1, pair2 := -1, -1, -1, -1, -1
	for r := 12; r >= 0; r-- {
		switch count[r] {
		case 4:
			quad = r
		case 3:
			if trip1 < 0 {
				trip1 = r
			} else if trip2 < 0 {
				trip2 = r
			}
		case 2:
			if pair1 < 0 {
				pair1 = r
			} else if pair2 < 0 {
				pair2 = r
			}
		}
	}
	switch {
	case quad >= 0:
		kicker := bits.Len32(all&^(1<<uint(quad))) - 1
		return catQuads<<catShift | uint32(quad)<<4 | uint32(kicker)
	case trip1 >= 0 && (trip2 >= 0 || pair1 >= 0):
		p := trip2
		if pair1 > p {
			p = pair1
		}
		return catFullHouse<<catShift | uint32(trip1)<<4 | uint32(p)
	}
	if hi := straightHigh(all); hi >= 0 {
		return catStraight<<catShift | uint32(hi)
	}
	switch {
	case trip1 >= 0:
		return catTrips<<catShift | uint32(trip1)<<13 | topBits(all&^(1<<uint(trip1)), 2)
	case pair2 >= 0:
		rest := all &^ (1<<uint(pair1) | 1<<uint(pair2))
		return catTwoPair<<catShift | uint32(pair1)<<17 | uint32(pair2)<<13 | topBits(rest, 1)
	case pair1 >= 0:
		return catPair<<catShift | uint32(pair1)<<13 | topBits(all&^(1<<uint(pair1)), 3)
	}
	return catHighCard<<catShift | topBits(all, 5)
}

// straightHigh returns the top rank of the best straight in a 13-bit rank
// mask (3 for the wheel A-2-3-4-5), or -1 if there is none.
func straightHigh(mask uint32) int {
	m := mask<<1 | mask>>12&1 // bit 0 = ace playing low
	run := m & (m >> 1) & (m >> 2) & (m >> 3) & (m >> 4)
	if run == 0 {
		return -1
	}
	return bits.Len32(run) - 1 + 3
}

// topBits keeps the n highest set bits of mask.
func topBits(mask uint32, n int) uint32 {
	for bits.OnesCount32(mask) > n {
		mask &= mask - 1
	}
	return mask
}
