package engine

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuantizeSumsTo255AndKeepsZeros(t *testing.T) {
	rnd := newRNG(5, 0)
	for trial := 0; trial < 20000; trial++ {
		n := 1 + rnd.intn(MaxActions)
		probs := make([]float64, n)
		var total float64
		for a := range probs {
			if rnd.intn(3) > 0 {
				probs[a] = float64(rnd.float32())
				total += probs[a]
			}
		}
		if total == 0 {
			probs[0], total = 1, 1
		}
		for a := range probs {
			probs[a] /= total
		}
		out := make([]uint8, n)
		quantize(probs, out)
		sum := 0
		for a, q := range out {
			sum += int(q)
			if probs[a] == 0 && q != 0 {
				t.Fatalf("zero-probability action got %d/255: %v -> %v", q, probs, out)
			}
			if math.Abs(float64(q)/255-probs[a]) > 1.0/255 {
				t.Fatalf("action %d moved by more than 1/255: %v -> %v", a, probs, out)
			}
		}
		if sum != 255 {
			t.Fatalf("bytes sum to %d: %v -> %v", sum, probs, out)
		}
	}
}

// Check (3) of the step-2 brief: a blueprint written to disk and read back
// through mmap makes bit-identical decisions. Run on Leduc and on a
// short-stack hold'em tree (the same Game path the real blueprint uses).
func TestBlueprintMmapRoundTripIsBitIdentical(t *testing.T) {
	holdem, err := NewHoldem(NLConfig{SB: 1, BB: 2, StackBB: 4, Opening: []float64{1}, RaiseMenu: []float64{1},
		ThreebetMenu: []float64{1}, FourbetMenu: []float64{1}, AllowSBFold: true, AllowSBLimp: true},
		CategoryAbstraction{Ranks: 13})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []*Game{NewLeduc(), holdem} {
		s := trained(t, g, DefaultTrainConfig(), 200_000)
		mem := s.Blueprint()
		path := filepath.Join(t.TempDir(), g.Rules.Name+".bp")
		if err := mem.WriteFile(path); err != nil {
			t.Fatal(err)
		}
		disk, err := OpenBlueprint(path, g)
		if err != nil {
			t.Fatal(err)
		}
		if disk.Header.Iterations != 200_000 || disk.Header.Game != g.Rules.Name {
			t.Errorf("%s: header did not survive: %+v", g.Rules.Name, disk.Header)
		}
		if !disk.Equal(mem) {
			t.Errorf("%s: strategy bytes differ after the round trip", g.Rules.Name)
		}
		rnd := newRNG(11, 0)
		decisions := 0
		var pm, pd [MaxActions]float64
		for i := range g.Tree.Nodes {
			n := &g.Tree.Nodes[i]
			if n.Kind != Decision {
				continue
			}
			for bucket := 0; bucket < g.Tree.NumBuckets[n.Round]; bucket++ {
				mem.Probs(n, bucket, pm[:n.NumActions])
				disk.Probs(n, bucket, pd[:n.NumActions])
				if pm != pd {
					t.Fatalf("%s node %d bucket %d: probabilities differ", g.Rules.Name, i, bucket)
				}
				for k := 0; k < 8; k++ {
					r := rnd.uint32()
					if a, b := mem.Action(n, bucket, r), disk.Action(n, bucket, r); a != b {
						t.Fatalf("%s node %d bucket %d r=%d: memory plays %d, mmap plays %d", g.Rules.Name, i, bucket, r, a, b)
					}
					decisions++
				}
			}
		}
		t.Logf("%s: %d strategy bytes and %d sampled decisions identical", g.Rules.Name, g.Tree.Slots, decisions)
		if err := disk.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Action must follow the stored bytes: it splits the 2^32 draws into 255
// equal cells and gives action a exactly byte[a] of them, so a zero byte is
// never played. One draw from the middle of each cell checks that.
func TestBlueprintActionFollowsStoredProbabilities(t *testing.T) {
	g := NewKuhn()
	b := NewBlueprint(g, hashStrategy{})
	for i := range g.Tree.Nodes {
		n := &g.Tree.Nodes[i]
		if n.Kind != Decision {
			continue
		}
		for bucket := 0; bucket < 3; bucket++ {
			var count [MaxActions]int
			for cell := uint64(0); cell < 255; cell++ {
				count[b.Action(n, bucket, uint32((2*cell+1)<<31/255))]++
			}
			for a := 0; a < int(n.NumActions); a++ {
				if want := int(b.probs[n.Slot(bucket)+int64(a)]); count[a] != want {
					t.Errorf("node %d bucket %d action %d: chosen in %d of 255 cells, stored byte is %d", i, bucket, a, count[a], want)
				}
			}
		}
	}
}

func TestOpenBlueprintRejectsAnotherGame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kuhn.bp")
	if err := NewBlueprint(NewKuhn(), UniformStrategy{}).WriteFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBlueprint(path, NewLeduc()); err == nil || !strings.Contains(err.Error(), "different betting tree") {
		t.Errorf("opening a Kuhn blueprint as Leduc: got %v, want a tree mismatch error", err)
	}
}

// Scaling preflop raises: by 1 it gives the blueprint back byte for byte;
// by 2 every preflop infoset's raises are twice as likely relative to the
// rest (to within a byte's rounding), and nothing after the flop moves.
func TestPreflopRaiseScaled(t *testing.T) {
	g := tableGame(t, "configs/tiny_20bb.json")
	s, err := NewSolver(g, DefaultTrainConfig())
	if err != nil {
		t.Fatal(err)
	}
	s.Run(300_000)
	bp := s.Blueprint()
	if same := NewBlueprint(g, PreflopRaiseScaled{bp, g.Tree, 1}); !same.Equal(bp) {
		t.Error("scaling by 1 changed the blueprint's bytes")
	}
	doubled := NewBlueprint(g, PreflopRaiseScaled{bp, g.Tree, 2})
	var was, now [MaxActions]float64
	checked, moved := 0, 0
	for i := range g.Tree.Nodes {
		n := &g.Tree.Nodes[i]
		if n.Kind != Decision {
			continue
		}
		na := int(n.NumActions)
		for bucket := 0; bucket < g.Tree.NumBuckets[n.Round]; bucket++ {
			bp.Probs(n, bucket, was[:na])
			doubled.Probs(n, bucket, now[:na])
			var raise float64 // the blueprint's probability of a bet or raise here
			for a := 0; a < na; a++ {
				if g.Tree.Actions[int(n.FirstChild)+a].Kind == ActBet {
					raise += was[a]
				}
			}
			for a := 0; a < na; a++ {
				want := was[a]
				if n.Round == 0 {
					want = was[a] / (1 + raise)
					if g.Tree.Actions[int(n.FirstChild)+a].Kind == ActBet {
						want *= 2
					}
				}
				if math.Abs(now[a]-want) > 1.0/255 || (n.Round > 0 && now[a] != was[a]) {
					t.Fatalf("round %d node %d bucket %d action %d: %.4f, want %.4f (was %.4f)", n.Round, i, bucket, a, now[a], want, was[a])
				}
				if now[a] != was[a] {
					moved++
				}
				checked++
			}
		}
	}
	if moved == 0 {
		t.Error("scaling by 2 moved nothing")
	}
	t.Logf("%d probabilities checked, %d moved (all preflop)", checked, moved)
}
