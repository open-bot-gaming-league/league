package engine

// River buckets from opponent-cluster hand strength (OCHS; Johanson et
// al. 2013, "Evaluating state-space abstractions in extensive-form
// games").
//
// A single equity number says how a river hand does against a random
// hand. Two hands with the same number can be very different against the
// hands an opponent actually has left: one beats every weak hand and
// loses to every strong one, the other wins and loses across the board.
// OCHS keeps that: the opponent's hole cards are put into ochsClusters
// groups by how strong they are before the flop, and a river hand is
// described by its equity against each group. Hands are then clustered
// by k-means on those vectors, with the engine's L1 distance (the paper
// uses L2).

const ochsClusters = 8

// ochsRiverBuckets returns the river bucket table for cfg.River buckets.
func ochsRiverBuckets(river *boardIndex, equity []uint16, cfg BucketConfig, logf func(string, ...any)) []uint16 {
	group := opponentGroups(river, equity, cfg, logf)

	// vec[slot*8+g] = equity against group g, in 255ths.
	vec := make([]uint8, len(equity)*ochsClusters)
	parallelFor(len(river.reps), cfg.Workers, func(from, to, _ int) {
		var scratch []riverHand
		for class := from; class < to; class++ {
			scratch = riverOCHSRow(river.reps[class], &group, vec[class*holeSlots*ochsClusters:(class+1)*holeSlots*ochsClusters], scratch)
		}
	})
	logf("river opponent-cluster equities done")

	all := &ochsData{vec: vec}
	for class := range river.reps {
		for h := 0; h < holeSlots; h++ {
			if slot := class*holeSlots + h; equity[slot] != noBucket {
				all.valid = append(all.valid, int32(slot))
				all.weight = append(all.weight, river.orbit[class])
			}
		}
	}
	// Fit the centres on a sample, then give every hand its nearest
	// centre: 145 million hands are far more than the centres need.
	const sampleSize = 4_000_000
	sample := &ochsData{vec: vec}
	rnd := newRNG(cfg.Seed+3, 1)
	for i := 0; i < sampleSize; i++ {
		j := int(rnd.uint64() % uint64(len(all.valid)))
		sample.valid = append(sample.valid, all.valid[j])
		sample.weight = append(sample.weight, all.weight[j])
	}
	_, centres := kmeansEMD(sample, cfg.River, cfg.Seed+3, cfg.MaxIters, cfg.Patience, cfg.Workers,
		func(s string) { logf("river k-means (on %d sampled hands): %s", sampleSize, s) })
	assigned := assignNearest(all, centres, cfg.Workers)
	table := make([]uint16, len(equity))
	for i := range table {
		table[i] = noBucket
	}
	for i, slot := range all.valid {
		table[slot] = assigned[i]
	}
	return table
}

// opponentGroups puts each of the 1,326 hole-card slots into one of
// ochsClusters groups, weakest first, by the preflop equity of its class
// against a random hand (the mean of its river equity over all boards).
func opponentGroups(river *boardIndex, equity []uint16, cfg BucketConfig, logf func(string, ...any)) [holeSlots]uint8 {
	var sum, count [preflopCount]float64
	var classOf [holeSlots]int
	for hi := Card(1); hi < maxDeck; hi++ {
		for lo := Card(0); lo < hi; lo++ {
			classOf[binom[hi][2]+int(lo)] = preflopClass(13, lo, hi)
		}
	}
	for class := range river.reps {
		w := float64(river.orbit[class])
		for h, v := range equity[class*holeSlots : (class+1)*holeSlots] {
			if v != noBucket {
				sum[classOf[h]] += w * float64(v)
				count[classOf[h]] += w
			}
		}
	}
	// Cluster the 169 class equities with the same k-means, as weighted
	// points on the equity scale.
	weight := make([]float64, equityScale+1)
	var value [preflopCount]int
	for c := range sum {
		value[c] = int(sum[c]/count[c] + 0.5)
		weight[value[c]] += count[c]
	}
	of, _ := kmeansEMD(&scalarData{weight: weight}, ochsClusters, cfg.Seed+4, 100, 3, 1, func(string) {})
	var group [holeSlots]uint8
	var sizes [ochsClusters]int
	for h := range group {
		group[h] = uint8(of[value[classOf[h]]])
		sizes[group[h]]++
	}
	logf("opponent hands in %d groups, weakest first: %v combinations", ochsClusters, sizes)
	return group
}

// riverOCHSRow fills one river board's vectors: for every hand the board
// does not block, its equity against the opponent hands of each group
// that share no card with it (ties count half), in 255ths. Where a group
// has no such hand, the hand's equity against all opponent hands stands
// in. out holds 8 bytes per hole slot.
func riverOCHSRow(board []Card, group *[holeSlots]uint8, out []uint8, scratch []riverHand) []riverHand {
	hands := riverHands(board, scratch)
	type counts struct {
		n    [ochsClusters]int32
		card [ochsClusters][maxDeck]int16
	}
	var all, below counts
	add := func(c *counts, h riverHand) {
		g := group[h.slot]
		c.n[g]++
		c.card[g][h.c0]++
		c.card[g][h.c1]++
	}
	// seen returns how many of c's hands in group g share no card with h;
	// self says c includes h itself, in h's own group.
	seen := func(c *counts, g int, h riverHand, self bool) int32 {
		v := c.n[g] - int32(c.card[g][h.c0]) - int32(c.card[g][h.c1])
		if self && int(group[h.slot]) == g {
			v++ // h was counted once and taken out twice
		}
		return v
	}
	for _, h := range hands {
		add(&all, h)
	}
	for i := 0; i < len(hands); {
		j := i
		var tied counts
		for ; j < len(hands) && hands[j].strength == hands[i].strength; j++ {
			add(&tied, hands[j])
		}
		for _, h := range hands[i:j] {
			var win2, opp [ochsClusters]int32 // twice the wins plus ties; opponents
			var win2All, oppAll int32
			for g := 0; g < ochsClusters; g++ {
				win2[g] = 2*seen(&below, g, h, false) + seen(&tied, g, h, true)
				opp[g] = seen(&all, g, h, true)
				win2All += win2[g]
				oppAll += opp[g]
			}
			row := out[int(h.slot)*ochsClusters : (int(h.slot)+1)*ochsClusters]
			for g := range row {
				w, o := win2[g], opp[g]
				if o == 0 {
					w, o = win2All, oppAll
				}
				row[g] = uint8((255*int(w) + int(o)) / (2 * int(o))) // round(255 * w/(2o))
			}
		}
		for _, h := range hands[i:j] {
			add(&below, h)
		}
		i = j
	}
	return hands
}

// ochsData is the river's clustering input under OCHS: point i is the
// 8-vector of table slot valid[i].
type ochsData struct {
	vec    []uint8
	valid  []int32
	weight []uint32
}

func (d *ochsData) Len() int             { return len(d.valid) }
func (d *ochsData) Dim() int             { return ochsClusters }
func (d *ochsData) Weight(i int) float64 { return float64(d.weight[i]) }
func (d *ochsData) Point(i int, out []float32) {
	at := int(d.valid[i]) * ochsClusters
	for g, v := range d.vec[at : at+ochsClusters] {
		out[g] = float32(v) / 255
	}
}

// A stronger hand has more equity against every group.
func (d *ochsData) Strength(centre []float32) float64 {
	var s float64
	for _, x := range centre {
		s += float64(x)
	}
	return s
}
