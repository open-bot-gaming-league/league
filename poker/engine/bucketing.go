package engine

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"sort"
)

// Building the hold'em card abstraction (cmd/buckets).
//
// Features, the standard ones (Johanson et al. 2013, "Evaluating
// state-space abstractions in extensive-form games"):
//   - river: the hand's equity against a uniform random opponent hand;
//   - turn and flop: the distribution of that river equity over the board
//     cards still to come, as a histogram.
//
// Hands are clustered by k-means under earth mover's distance, which for
// distributions on a line is the L1 distance between cumulative
// histograms. A river hand's "distribution" is a single point, so the same
// code clusters river equities by absolute difference. Each board class
// counts once per board in it, so the clustering sees hands as often as
// the deck deals them.

type BucketConfig struct {
	Flop, Turn, River int // bucket counts
	Bins              int // equity histogram bins for flop and turn
	Seed              uint64
	MaxIters          int // k-means iterations per round
	// Patience is how many iterations in a row may fail to lower the
	// clustering cost before k-means stops. With mean centres under an L1
	// distance the cost is not guaranteed to fall every iteration, and at
	// large k the very first update can raise it slightly; 0 stops there,
	// which left the k=2000 turn clustering at its seeds. The default
	// stays 0 so that the bucket files built before this knob existed
	// rebuild byte for byte.
	Patience int
	Workers  int
	// RiverFeature is what river hands are clustered on: "equity" (the
	// default: one number, equity against a random hand) or "ochs"
	// (opponent-cluster hand strength: the equities against ochsClusters
	// groups of opponent hands, weak to strong; Johanson et al. 2013).
	RiverFeature string
	// ReuseFlopTurn, when set, is a bucket file whose flop and turn
	// tables are copied instead of being clustered again, so that two
	// files differ on the river only.
	ReuseFlopTurn string
}

func DefaultBucketConfig() BucketConfig {
	return BucketConfig{Flop: 10, Turn: 10, River: 10, Bins: 50, Seed: 1, MaxIters: 40, Workers: runtime.GOMAXPROCS(0)}
}

// equityScale is the range of the integer river equity: 2*wins + ties
// against the C(45, 2) = 990 opponent hands.
const equityScale = 2 * 990

// BuildTableAbstraction computes the features and clusters them. logf
// reports progress.
func BuildTableAbstraction(cfg BucketConfig, logf func(format string, args ...any)) (*TableAbstraction, error) {
	if cfg.Flop < 1 || cfg.Turn < 1 || cfg.River < 1 || max(cfg.Flop, cfg.Turn, cfg.River) >= noBucket {
		return nil, fmt.Errorf("engine: bucket counts must be in 1..%d", noBucket-1)
	}
	if cfg.Bins < 2 || cfg.Bins > 255 {
		return nil, fmt.Errorf("engine: bins must be in 2..255")
	}
	t := &TableAbstraction{Header: TableHeader{
		Buckets: [MaxRounds]int{preflopCount, cfg.Flop, cfg.Turn, cfg.River}, Bins: cfg.Bins, Seed: cfg.Seed, Patience: cfg.Patience}}
	t.attachBoards()
	flop, turn, river := t.boards[1], t.boards[2], t.boards[3]
	logf("board classes: flop %d, turn %d, river %d", len(flop.reps), len(turn.reps), len(river.reps))

	equity := riverEquities(river, cfg.Workers)
	logf("river equities done (%d entries)", len(equity))

	switch cfg.RiverFeature {
	case "", "equity":
		// Cluster the 1,981 possible equity values, each weighted by how
		// often it is dealt.
		weight := make([]float64, equityScale+1)
		for class := range river.reps {
			for _, v := range equity[class*holeSlots : (class+1)*holeSlots] {
				if v != noBucket {
					weight[v] += float64(river.orbit[class])
				}
			}
		}
		riverOf, _ := kmeansEMD(&scalarData{weight: weight}, cfg.River, cfg.Seed+3, cfg.MaxIters, cfg.Patience, cfg.Workers,
			func(s string) { logf("river k-means: %s", s) })
		t.table[3] = make([]uint16, len(equity))
		parallelFor(len(equity), cfg.Workers, func(lo, hi, _ int) {
			for i := lo; i < hi; i++ {
				t.table[3][i] = noBucket
				if equity[i] != noBucket {
					t.table[3][i] = riverOf[equity[i]]
				}
			}
		})
	case "ochs":
		t.Header.RiverFeature = "ochs"
		t.table[3] = ochsRiverBuckets(river, equity, cfg, logf)
	default:
		return nil, fmt.Errorf("engine: unknown river feature %q (equity, ochs)", cfg.RiverFeature)
	}

	if cfg.ReuseFlopTurn != "" {
		src, err := OpenTableAbstraction(cfg.ReuseFlopTurn)
		if err != nil {
			return nil, err
		}
		defer src.Close()
		if src.Header.Buckets[1] != cfg.Flop || src.Header.Buckets[2] != cfg.Turn {
			return nil, fmt.Errorf("engine: %s has %d flop and %d turn buckets, not %d and %d",
				cfg.ReuseFlopTurn, src.Header.Buckets[1], src.Header.Buckets[2], cfg.Flop, cfg.Turn)
		}
		for round := 1; round <= 2; round++ {
			tbl, err := src.roundTable(round)
			if err != nil {
				return nil, fmt.Errorf("engine: %s: %w", cfg.ReuseFlopTurn, err)
			}
			t.table[round] = slices.Clone(tbl)
		}
		t.Header.FlopTurnFrom = src.ID()
		logf("flop and turn tables copied from %s", src.ID())
	} else {
		for round, boards := range []*boardIndex{1: flop, 2: turn} {
			if boards == nil {
				continue
			}
			hist := equityHistograms(boards, river, equity, cfg.Bins, cfg.Workers)
			logf("round %d histograms done (%d hands)", round, len(hist.valid))
			assigned, _ := kmeansEMD(hist, t.Header.Buckets[round], cfg.Seed+uint64(round), cfg.MaxIters, cfg.Patience, cfg.Workers,
				func(s string) { logf("round %d k-means: %s", round, s) })
			tbl := make([]uint16, len(boards.reps)*holeSlots)
			for i := range tbl {
				tbl[i] = noBucket
			}
			for i, slot := range hist.valid {
				tbl[slot] = assigned[i]
			}
			t.table[round] = tbl
		}
	}
	t.Header.Hash = t.contentHash()
	return t, nil
}

// riverEquities returns, for every (river board class, hole slot), the
// hand's integer equity 2*wins + ties against all opponent hands that
// share no card with it or the board; noBucket where the hole cards
// overlap the board.
func riverEquities(river *boardIndex, workers int) []uint16 {
	out := make([]uint16, len(river.reps)*holeSlots)
	parallelFor(len(river.reps), workers, func(from, to, _ int) {
		var scratch []riverHand
		for class := from; class < to; class++ {
			scratch = riverEquityRow(river.reps[class], out[class*holeSlots:(class+1)*holeSlots], scratch)
		}
	})
	return out
}

type riverHand struct {
	strength uint32
	slot     uint16
	c0, c1   Card
}

// riverHands lists the hands a river board does not block, weakest
// first. scratch is reused between calls.
func riverHands(board []Card, scratch []riverHand) []riverHand {
	used := cardMask(board)
	var cs [7]Card
	copy(cs[2:], board)
	hands := scratch[:0]
	for hi := Card(1); hi < maxDeck; hi++ {
		for lo := Card(0); lo < hi; lo++ {
			if used&(1<<lo|1<<hi) != 0 {
				continue
			}
			cs[0], cs[1] = lo, hi
			hands = append(hands, riverHand{holdemStrength(cs[:]), uint16(binom[hi][2] + int(lo)), lo, hi})
		}
	}
	slices.SortFunc(hands, func(a, b riverHand) int {
		if a.strength != b.strength {
			if a.strength < b.strength {
				return -1
			}
			return 1
		}
		return int(a.slot) - int(b.slot)
	})
	return hands
}

// riverEquityRow fills one board's row of riverEquities. scratch is
// reused between calls.
func riverEquityRow(board []Card, row []uint16, scratch []riverHand) []riverHand {
	for i := range row {
		row[i] = noBucket
	}
	hands := riverHands(board, scratch)
	// Sweep ascending: "below" counts strictly weaker hands, and the
	// per-card counts take out the ones sharing a card.
	var belowCard [maxDeck]int
	below := 0
	for i := 0; i < len(hands); {
		j := i
		var tieCard [maxDeck]int
		for ; j < len(hands) && hands[j].strength == hands[i].strength; j++ {
			tieCard[hands[j].c0]++
			tieCard[hands[j].c1]++
		}
		group := j - i
		for _, h := range hands[i:j] {
			wins := below - belowCard[h.c0] - belowCard[h.c1]
			// Tied hands sharing a card with h: tieCard counts h itself
			// once for each of its cards.
			ties := group - 1 - (tieCard[h.c0] - 1) - (tieCard[h.c1] - 1)
			row[h.slot] = uint16(2*wins + ties)
		}
		for _, h := range hands[i:j] {
			belowCard[h.c0]++
			belowCard[h.c1]++
		}
		below += group
		i = j
	}
	return hands
}

// histData is one round's clustering input: for each valid (board class,
// hole) the cumulative equity histogram over the cards to come.
type histData struct {
	bins   int
	total  int      // histogram mass: the number of ways the board completes
	valid  []int    // table slot of each point
	weight []uint32 // board-class orbit of each point
	cum    []uint16 // cumulative counts, bins per point
}

func (d *histData) Len() int             { return len(d.valid) }
func (d *histData) Dim() int             { return d.bins }
func (d *histData) Weight(i int) float64 { return float64(d.weight[i]) }

// A cumulative histogram's mean is (number of bins - sum of the CDF) up to
// scale, so a smaller sum is a stronger hand.
func (d *histData) Strength(centre []float32) float64 {
	var s float64
	for _, x := range centre {
		s -= float64(x)
	}
	return s
}
func (d *histData) Point(i int, out []float32) {
	for j, c := range d.cum[i*d.bins : (i+1)*d.bins] {
		out[j] = float32(c) / float32(d.total)
	}
}

// equityHistograms builds histData for flop (3-card) or turn (4-card)
// boards by running out every completion and looking up its river equity.
func equityHistograms(boards, river *boardIndex, equity []uint16, bins, workers int) *histData {
	d := &histData{bins: bins}
	missing := 5 - boards.n
	d.total = binom[maxDeck-boards.n-2][missing]
	for class, board := range boards.reps {
		used := cardMask(board)
		for hi := Card(1); hi < maxDeck; hi++ {
			for lo := Card(0); lo < hi; lo++ {
				if used&(1<<lo|1<<hi) == 0 {
					d.valid = append(d.valid, class*holeSlots+binom[hi][2]+int(lo))
					d.weight = append(d.weight, boards.orbit[class])
				}
			}
		}
	}
	d.cum = make([]uint16, len(d.valid)*bins)
	parallelFor(len(d.valid), workers, func(from, to, _ int) {
		hole := make([]Card, 2)
		full := make([]Card, 5)
		for i := from; i < to; i++ {
			slot := d.valid[i]
			board := boards.reps[slot/holeSlots]
			// Recover the hole cards from the slot.
			h := slot % holeSlots
			top := Card(1)
			for binom[top+1][2] <= h {
				top++
			}
			hole[0], hole[1] = Card(h-binom[top][2]), top
			used := cardMask(board) | cardMask(hole)
			hist := d.cum[i*bins : (i+1)*bins]
			forEachCombo(maxDeck, missing, used, func(extra []Card) {
				n := copy(full, board)
				copy(full[n:], extra)
				sortCards(full)
				v := int(equity[river.slot(hole, full)])
				hist[v*bins/(equityScale+1)]++
			})
			for j := 1; j < bins; j++ {
				hist[j] += hist[j-1]
			}
		}
	})
	return d
}

// scalarData is the river's clustering input: point i is the equity value
// i, weighted by how often it occurs.
type scalarData struct{ weight []float64 }

func (d *scalarData) Len() int                          { return len(d.weight) }
func (d *scalarData) Dim() int                          { return 1 }
func (d *scalarData) Weight(i int) float64              { return d.weight[i] }
func (d *scalarData) Point(i int, out []float32)        { out[0] = float32(i) / equityScale }
func (d *scalarData) Strength(centre []float32) float64 { return float64(centre[0]) }

// clusterData is a weighted point set. Points are cumulative
// distributions (or single values), so L1 distance between them is earth
// mover's distance.
type clusterData interface {
	Len() int
	Dim() int
	Weight(i int) float64
	Point(i int, out []float32)
	// Strength orders cluster centres weakest first, for numbering the
	// buckets.
	Strength(centre []float32) float64
}

// l1Within returns the L1 distance between a and b, or some value >= limit
// as soon as the running sum reaches limit (the caller only wants to know
// whether the distance beats limit).
func l1Within(a, b []float32, limit float32) float32 {
	var d float32
	for j, x := range a {
		y := x - b[j]
		if y < 0 {
			y = -y
		}
		d += y
		if j&7 == 7 && d >= limit {
			return d
		}
	}
	return d
}

// kmeansEMD clusters data into k buckets and returns each point's bucket
// and the bucket centres (k rows of data.Dim() values). Buckets are
// numbered weakest first (data.Strength of the centre).
func kmeansEMD(data clusterData, k int, seed uint64, maxIters, patience, workers int, logf func(string)) ([]uint16, []float32) {
	n, dim := data.Len(), data.Dim()
	rnd := newRNG(seed, 0)

	// k-means++ seeding on a sample: each next centre is drawn with
	// probability proportional to weight * distance^2 to the nearest
	// centre so far.
	sampleSize := min(n, 20000+100*k)
	sample := make([]float32, sampleSize*dim)
	sampleWeight := make([]float64, sampleSize)
	for s := 0; s < sampleSize; {
		i := s
		if sampleSize < n {
			i = int(rnd.uint64() % uint64(n))
		}
		if w := data.Weight(i); w > 0 || sampleSize == n {
			data.Point(i, sample[s*dim:(s+1)*dim])
			sampleWeight[s] = w
			s++
		}
	}
	centroids := make([]float32, 0, k*dim)
	dist2 := make([]float64, sampleSize)
	for s := range dist2 {
		dist2[s] = math.Inf(1)
	}
	for c := 0; c < k; c++ {
		var total float64
		for s := range dist2 {
			if c == 0 {
				total += sampleWeight[s]
			} else {
				total += sampleWeight[s] * dist2[s]
			}
		}
		pick := sampleSize - 1
		if total > 0 {
			target := rnd.float64() * total
			for s := range dist2 {
				w := sampleWeight[s]
				if c > 0 {
					w *= dist2[s]
				}
				if target -= w; target < 0 {
					pick = s
					break
				}
			}
		}
		centroids = append(centroids, sample[pick*dim:(pick+1)*dim]...)
		for s := range dist2 {
			d := float64(l1Within(sample[s*dim:(s+1)*dim], centroids[c*dim:(c+1)*dim], float32(math.Inf(1))))
			dist2[s] = math.Min(dist2[s], d*d)
		}
	}

	// Lloyd iterations: assign by EMD, move each centre to its members'
	// weighted mean.
	//
	// Most points do not change cluster from one iteration to the next, and
	// the triangle inequality can prove it without looking at the other
	// centres (Hamerly 2010, "Making k-means even faster"): a point stays
	// if its distance to its own centre is below both a lower bound on its
	// distance to every other centre, carried over and loosened by how far
	// the centres moved, and half the distance from its centre to the
	// nearest other centre. Only points that fail the test are compared
	// with all k centres. The test is strict and leaves a margin for
	// rounding, so the assignments are exactly what the full comparison
	// gives; TestKMeansPruningChangesNothing holds that.
	type partial struct {
		sum    []float64
		weight []float64
		cost   float64
	}
	own := make([]int32, n)     // each point's cluster; -1 before its first assignment
	other := make([]float32, n) // lower bound on its distance to any other centre
	for i := range own {
		own[i] = -1
	}
	half := make([]float32, k)   // half the distance to the nearest other centre
	loosen := make([]float32, k) // how far any centre but this one moved in the last update
	search := func(p []float32, centres []float32) (best int32, bestDist, second float32) {
		bestDist, second = float32(math.Inf(1)), float32(math.Inf(1))
		for c := 0; c < k; c++ {
			// Lowest index wins ties, so identical points always land
			// together.
			if d := l1Within(p, centres[c*dim:(c+1)*dim], second); d < bestDist {
				best, bestDist, second = int32(c), d, bestDist
			} else if d < second {
				second = d
			}
		}
		return best, bestDist, second
	}
	// settled reports whether point i provably stays with its centre, at
	// distance d from it.
	settled := func(i int, d float32) bool {
		const margin = 1 - 1e-4 // rounding in the bounds is orders of magnitude smaller
		return kmeansPrune && d < margin*max(half[own[i]], other[i]-loosen[own[i]])
	}
	halfDistances := func() {
		parallelFor(k, workers, func(lo, hi, _ int) {
			for c := lo; c < hi; c++ {
				nearestOther := float32(math.Inf(1))
				for o := 0; o < k; o++ {
					if o != c {
						nearestOther = min(nearestOther, l1Within(centroids[c*dim:(c+1)*dim], centroids[o*dim:(o+1)*dim], nearestOther))
					}
				}
				half[c] = nearestOther / 2
			}
		})
	}
	prevCost := math.Inf(1)
	stalled := 0 // iterations in a row that did not lower the cost
	for iter := 1; iter <= maxIters; iter++ {
		halfDistances()
		parts := make([]partial, workers)
		parallelFor(n, workers, func(lo, hi, w int) {
			part := partial{sum: make([]float64, k*dim), weight: make([]float64, k)}
			p := make([]float32, dim)
			for i := lo; i < hi; i++ {
				wt := data.Weight(i)
				if wt == 0 {
					continue
				}
				data.Point(i, p)
				c, d := own[i], float32(0)
				if c >= 0 {
					d = l1Within(p, centroids[int(c)*dim:(int(c)+1)*dim], float32(math.Inf(1)))
				}
				if c < 0 || !settled(i, d) {
					c, d, other[i] = search(p, centroids)
					own[i] = c
				} else {
					other[i] -= loosen[c]
				}
				part.cost += wt * float64(d)
				part.weight[c] += wt
				for j, x := range p {
					part.sum[int(c)*dim+j] += wt * float64(x)
				}
			}
			parts[w] = part
		})
		sum, weight := make([]float64, k*dim), make([]float64, k)
		var cost, totalWeight float64
		for _, part := range parts {
			if part.sum == nil {
				continue
			}
			cost += part.cost
			for c := range weight {
				weight[c] += part.weight[c]
				totalWeight += part.weight[c]
			}
			for j := range sum {
				sum[j] += part.sum[j]
			}
		}
		empty := 0
		moved := make([]float32, k)
		old := make([]float32, dim)
		for c := 0; c < k; c++ {
			centre := centroids[c*dim : (c+1)*dim]
			copy(old, centre)
			if weight[c] == 0 {
				// An empty cluster restarts on a random sample point.
				s := rnd.intn(sampleSize)
				copy(centre, sample[s*dim:(s+1)*dim])
				empty++
			} else {
				for j := range centre {
					centre[j] = float32(sum[c*dim+j] / weight[c])
				}
			}
			moved[c] = l1Within(old, centre, float32(math.Inf(1)))
		}
		// loosen[c] = the largest move among the centres other than c.
		first, second := 0, -1
		for c := 1; c < k; c++ {
			switch {
			case moved[c] > moved[first]:
				first, second = c, first
			case second < 0 || moved[c] > moved[second]:
				second = c
			}
		}
		for c := range loosen {
			loosen[c] = moved[first]
			if c == first {
				loosen[c] = 0
				if second >= 0 {
					loosen[c] = moved[second]
				}
			}
		}
		logf(fmt.Sprintf("iteration %d: mean EMD %.6f, %d empty", iter, cost/totalWeight, empty))
		if empty == 0 && prevCost-cost < 1e-5*cost {
			if stalled++; stalled > patience {
				break
			}
		} else {
			stalled = 0
		}
		prevCost = cost
	}

	// Number buckets weakest first.
	strength := make([]float64, k)
	for c := range strength {
		strength[c] = data.Strength(centroids[c*dim : (c+1)*dim])
	}
	rank := make([]int, k)
	for c := range rank {
		rank[c] = c
	}
	sort.SliceStable(rank, func(a, b int) bool { return strength[rank[a]] < strength[rank[b]] })
	ordered := make([]float32, 0, k*dim)
	number := make([]uint16, k) // cluster -> bucket number
	for bucket, c := range rank {
		ordered = append(ordered, centroids[c*dim:(c+1)*dim]...)
		number[c] = uint16(bucket)
	}

	// Final assignment to the centres as they now stand, with ties going
	// to the lowest bucket number.
	halfDistances()
	assigned := make([]uint16, n)
	parallelFor(n, workers, func(lo, hi, _ int) {
		p := make([]float32, dim)
		for i := lo; i < hi; i++ {
			data.Point(i, p)
			if c := own[i]; c >= 0 && settled(i, l1Within(p, centroids[int(c)*dim:(int(c)+1)*dim], float32(math.Inf(1)))) {
				assigned[i] = number[c]
				continue
			}
			bucket, _, _ := search(p, ordered)
			assigned[i] = uint16(bucket)
		}
	})
	return assigned, ordered
}

// assignNearest gives each point of data the nearest of the given centres
// (L1 distance; the lowest-numbered on ties).
func assignNearest(data clusterData, centres []float32, workers int) []uint16 {
	n, dim := data.Len(), data.Dim()
	assigned := make([]uint16, n)
	parallelFor(n, workers, func(lo, hi, _ int) {
		p := make([]float32, dim)
		for i := lo; i < hi; i++ {
			data.Point(i, p)
			best, bestDist := 0, float32(math.Inf(1))
			for c := 0; c*dim < len(centres); c++ {
				if d := l1Within(p, centres[c*dim:(c+1)*dim], bestDist); d < bestDist {
					best, bestDist = c, d
				}
			}
			assigned[i] = uint16(best)
		}
	})
	return assigned
}

// kmeansPrune turns the triangle-inequality shortcut in kmeansEMD on. It
// is a variable only so a test can turn it off and check the shortcut
// changes nothing.
var kmeansPrune = true
