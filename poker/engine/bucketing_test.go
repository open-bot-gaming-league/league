package engine

import (
	"os"
	"reflect"
	"testing"
)

func TestBoardClassCounts(t *testing.T) {
	want := map[int]int{3: 1755, 4: 16432, 5: 134459}
	for n, classes := range want {
		if n == 5 && testing.Short() {
			continue
		}
		b := fullDeckBoards(n)
		if len(b.reps) != classes {
			t.Errorf("%d-card boards: %d classes, want %d", n, len(b.reps), classes)
		}
		total := 0
		for _, o := range b.orbit {
			total += int(o)
		}
		if total != binom[52][n] {
			t.Errorf("%d-card boards: orbits cover %d boards, want %d", n, total, binom[52][n])
		}
	}
}

// The relabelling stored for a board must turn it into its class
// representative, so two isomorphic (hole, board) pairs can share a slot.
func TestBoardIndexPermMapsToRepresentative(t *testing.T) {
	b := fullDeckBoards(4)
	rnd := newRNG(9, 0)
	for trial := 0; trial < 5000; trial++ {
		board := randomCards(rnd, 4)
		i := combIndex(board)
		rep := b.reps[b.class[i]]
		if got := permuteIndex(board, &suitPerms[b.perm[i]]); got != combIndex(rep) {
			t.Fatalf("board %v: stored relabelling gives index %d, representative %v has %d", board, got, rep, combIndex(rep))
		}
	}
}

func randomCards(rnd *rng, n int) []Card {
	cs := randomDeal(rnd, n)
	sortCards(cs)
	return cs
}

// The swept river equity must equal the obvious count: compare every hand
// with every opponent hand that shares no card.
func TestRiverEquityRowMatchesBruteForce(t *testing.T) {
	rnd := newRNG(4, 0)
	boards := [][]Card{hand("2c", "2d", "2h", "2s", "3c"), hand("Tc", "Jc", "Qc", "Kc", "Ac"), hand("3c", "7d", "9h", "Js", "Kc")}
	for i := 0; i < 5; i++ {
		boards = append(boards, randomCards(rnd, 5))
	}
	row := make([]uint16, holeSlots)
	for _, board := range boards {
		sortCards(board)
		riverEquityRow(board, row, nil)
		used := cardMask(board)
		strength := func(lo, hi Card) uint32 {
			return holdemStrength(append([]Card{lo, hi}, board...))
		}
		for hi := Card(1); hi < maxDeck; hi++ {
			for lo := Card(0); lo < hi; lo++ {
				slot := binom[hi][2] + int(lo)
				if used&(1<<lo|1<<hi) != 0 {
					if row[slot] != noBucket {
						t.Fatalf("board %v: blocked hand %d %d has equity %d", board, lo, hi, row[slot])
					}
					continue
				}
				mine, want, opponents := strength(lo, hi), 0, 0
				blocked := used | 1<<lo | 1<<hi
				for b := Card(1); b < maxDeck; b++ {
					for a := Card(0); a < b; a++ {
						if blocked&(1<<a|1<<b) != 0 {
							continue
						}
						opponents++
						if theirs := strength(a, b); mine > theirs {
							want += 2
						} else if mine == theirs {
							want++
						}
					}
				}
				if opponents != 990 || int(row[slot]) != want {
					t.Fatalf("board %v hand %d %d: equity %d over %d opponents, sweep says %d", board, lo, hi, want, opponents, row[slot])
				}
			}
		}
	}
}

// Three well-separated groups of single values must come back as three
// buckets, numbered weakest first, whatever the seed.
func TestKMeansEMDSeparatesAndOrdersClusters(t *testing.T) {
	weight := make([]float64, equityScale+1)
	for v := range weight {
		if (v >= 100 && v < 200) || (v >= 900 && v < 1000) || (v >= 1700 && v < 1800) {
			weight[v] = 1 + float64(v%7)
		}
	}
	for seed := uint64(1); seed <= 5; seed++ {
		got, _ := kmeansEMD(&scalarData{weight: weight}, 3, seed, 50, 0, 3, func(string) {})
		for v, w := range weight {
			if w == 0 {
				continue
			}
			if want := uint16(v / 800); got[v] != want {
				t.Fatalf("seed %d: value %d in bucket %d, want %d", seed, v, got[v], want)
			}
		}
	}
}

// randomHistograms is a clustering input shaped like the real one:
// cumulative histograms with integer counts, several of them identical.
func randomHistograms(points, bins, mass int, seed uint64) *histData {
	rnd := newRNG(seed, 0)
	d := &histData{bins: bins, total: mass}
	for i := 0; i < points; i++ {
		hist := make([]uint16, bins)
		centre := rnd.intn(bins)
		for m := 0; m < mass; m++ {
			b := centre + rnd.intn(7) - 3 + rnd.intn(bins/4)*rnd.intn(2)
			hist[max(0, min(bins-1, b))]++
		}
		for j := 1; j < bins; j++ {
			hist[j] += hist[j-1]
		}
		copies := 1 + rnd.intn(2)
		for c := 0; c < copies; c++ {
			d.valid = append(d.valid, len(d.valid))
			d.weight = append(d.weight, uint32(1+rnd.intn(24)))
			d.cum = append(d.cum, hist...)
		}
	}
	return d
}

// The triangle-inequality shortcut must be invisible: same buckets for
// every point as comparing every point with every centre, at every k,
// including runs that go to the iteration limit.
func TestKMeansPruningChangesNothing(t *testing.T) {
	defer func() { kmeansPrune = true }()
	data := randomHistograms(6000, 20, 46, 8)
	for _, k := range []int{1, 2, 7, 40, 300} {
		for seed := uint64(1); seed <= 3; seed++ {
			var logs [2][]string
			var got [2][]uint16
			for mode, prune := range []bool{false, true} {
				kmeansPrune = prune
				got[mode], _ = kmeansEMD(data, k, seed, 25, int(seed)-1, 3, func(s string) { logs[mode] = append(logs[mode], s) })
			}
			if !reflect.DeepEqual(got[0], got[1]) {
				t.Fatalf("k=%d seed=%d: pruning changed the buckets", k, seed)
			}
			if !reflect.DeepEqual(logs[0], logs[1]) {
				t.Fatalf("k=%d seed=%d: pruning changed the iteration log:\n%v\n%v", k, seed, logs[0], logs[1])
			}
		}
	}
}

// The swept opponent-cluster equities must equal the obvious count: for
// each hand and each group of opponent hands, compare it with every
// opponent hand in the group that shares no card. And assignNearest must
// agree with the clustering's own assignment.
func TestRiverOCHSRowMatchesBruteForce(t *testing.T) {
	rnd := newRNG(14, 0)
	var group [holeSlots]uint8
	for h := range group {
		group[h] = uint8(rnd.intn(ochsClusters))
	}
	group[0], group[1] = 7, 7
	boards := [][]Card{hand("2c", "2d", "2h", "2s", "3c"), hand("Tc", "Jc", "Qc", "Kc", "Ac")}
	for i := 0; i < 3; i++ {
		boards = append(boards, randomCards(rnd, 5))
	}
	out := make([]uint8, holeSlots*ochsClusters)
	for _, board := range boards {
		sortCards(board)
		riverOCHSRow(board, &group, out, nil)
		used := cardMask(board)
		strength := func(lo, hi Card) uint32 { return holdemStrength(append([]Card{lo, hi}, board...)) }
		for hi := Card(1); hi < maxDeck; hi++ {
			for lo := Card(0); lo < hi; lo++ {
				if used&(1<<lo|1<<hi) != 0 {
					continue
				}
				slot := binom[hi][2] + int(lo)
				mine := strength(lo, hi)
				var win2, opp [ochsClusters]int
				win2All, oppAll := 0, 0
				blocked := used | 1<<lo | 1<<hi
				for b := Card(1); b < maxDeck; b++ {
					for a := Card(0); a < b; a++ {
						if blocked&(1<<a|1<<b) != 0 {
							continue
						}
						g := group[binom[b][2]+int(a)]
						opp[g]++
						oppAll++
						if theirs := strength(a, b); mine > theirs {
							win2[g] += 2
							win2All += 2
						} else if mine == theirs {
							win2[g]++
							win2All++
						}
					}
				}
				for g := 0; g < ochsClusters; g++ {
					w, o := win2[g], opp[g]
					if o == 0 {
						w, o = win2All, oppAll
					}
					if want := uint8((255*w + o) / (2 * o)); out[slot*ochsClusters+g] != want {
						t.Fatalf("board %v hand %d %d group %d: %d of 255, brute force says %d (%d half-wins over %d opponents)",
							board, lo, hi, g, out[slot*ochsClusters+g], want, w, o)
					}
				}
			}
		}
	}
}

func TestAssignNearestAgreesWithKMeans(t *testing.T) {
	data := randomHistograms(3000, 20, 46, 5)
	for _, k := range []int{1, 9, 120} {
		assigned, centres := kmeansEMD(data, k, 2, 25, 0, 3, func(string) {})
		if again := assignNearest(data, centres, 3); !reflect.DeepEqual(assigned, again) {
			t.Fatalf("k=%d: assignNearest on the returned centres differs from the clustering's assignment", k)
		}
	}
}

// builtTable opens the bucket file the README's commands build, or skips.
func builtTable(t *testing.T) *TableAbstraction {
	t.Helper()
	const path = "data/abs-10-10-10.bin"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s not built (go run ./cmd/buckets -out %s)", path, path)
	}
	abs, err := OpenTableAbstraction(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { abs.Close() })
	return abs
}

func TestBuiltTableIsSuitInvariant(t *testing.T) {
	abs := builtTable(t)
	if err := abs.CheckSuitInvariance(200_000, 1); err != nil {
		t.Fatal(err)
	}
}
