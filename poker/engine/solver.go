package engine

import (
	"fmt"
	"math"
	"sync"
)

// External-sampling MCCFR (Lanctot et al. 2009). One iteration samples a
// deal, picks a traverser, and walks the betting tree: every action at the
// traverser's nodes, one sampled action at the opponent's. Regrets are
// updated at the traverser's infosets and the average-strategy sums at the
// opponent's.
//
// Values are in big blinds (the tree's Stake unit).

type TrainConfig struct {
	// The two per-action arrays have separate formats because they tolerate
	// 2-byte values differently (EVAL.md, "Storage"): regrets are fine in
	// float16, strategy sums pick up a noise floor.
	RegretStorage StoreKind
	SumStorage    StoreKind
	Seed          uint64
	// Workers > 1 trains Hogwild-style: goroutines update the shared
	// arrays without locks. Racing writes can drop an update, which
	// sampling noise swamps; Go's memory model keeps a racy read of a
	// word-sized value to "some value that was written", so nothing worse
	// than a lost update can happen. Workers = 1 is deterministic for a
	// given seed.
	Workers int
	// RegretFloor clamps cumulative regret from below (<= 0). 0 gives
	// regret matching+; -Inf is plain MCCFR. Whatever is asked for, the
	// floor is never below the storage format's MinFloor.
	RegretFloor float32
	// Fixed[p], when set, is a strategy seat p plays throughout and never
	// changes. With one seat fixed the other learns a response to it; see
	// BucketedResponse.
	Fixed [2]Strategy
}

func DefaultTrainConfig() TrainConfig {
	return TrainConfig{RegretStorage: Float32, SumStorage: Float32, Seed: 1, Workers: 1,
		RegretFloor: float32(math.Inf(-1))}
}

type Solver struct {
	Game   *Game
	Config TrainConfig
	regret Store
	sum    Store
	floor  float32
	iters  int64
	rngs   []*rng
}

func NewSolver(g *Game, cfg TrainConfig) (*Solver, error) {
	if cfg.Workers < 1 {
		return nil, fmt.Errorf("engine: workers must be >= 1")
	}
	if cfg.RegretFloor > 0 {
		return nil, fmt.Errorf("engine: regret floor must be <= 0")
	}
	regret, err := NewStore(cfg.RegretStorage, g.Tree.Slots)
	if err != nil {
		return nil, err
	}
	sum, err := NewStore(cfg.SumStorage, g.Tree.Slots)
	if err != nil {
		return nil, err
	}
	s := &Solver{Game: g, Config: cfg, regret: regret, sum: sum}
	s.floor = max(cfg.RegretFloor, regret.MinFloor())
	for w := 0; w < cfg.Workers; w++ {
		s.rngs = append(s.rngs, newRNG(cfg.Seed, uint64(w)))
	}
	return s, nil
}

// Iterations is the number of traversals run so far.
func (s *Solver) Iterations() int64 { return s.iters }

// StorageBytes is the size of the regret and strategy-sum arrays together.
func (s *Solver) StorageBytes() int64 { return s.regret.Bytes() + s.sum.Bytes() }

// Run does n more iterations, split evenly over the workers.
func (s *Solver) Run(n int64) {
	workers := int64(len(s.rngs))
	var wg sync.WaitGroup
	for w := int64(0); w < workers; w++ {
		share := n / workers
		if w < n%workers {
			share++
		}
		// Alternate the traverser, picking up where the last Run stopped
		// (with one worker, chunked Run calls equal one long one).
		first := (s.iters/workers + w) & 1
		wk := &worker{s: s, nodes: s.Game.Tree.Nodes, children: s.Game.Tree.Children, rnd: s.rngs[w],
			frames: make([]frame, s.Game.Tree.Depth())}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int64(0); i < share; i++ {
				s.Game.sampleDeal(wk.rnd, &wk.deal)
				wk.traverse(0, int((first+i)&1), 0)
			}
		}()
	}
	wg.Wait()
	s.iters += n
}

// worker is one training goroutine. It owns all the scratch a traversal
// needs, one frame per tree depth, so the hot loop allocates nothing: a
// stack array passed to a Store method would escape to the heap on every
// node visit, and the garbage both slows training and makes the process
// several times larger than its arrays.
type worker struct {
	s        *Solver
	nodes    []Node
	children []int32
	rnd      *rng
	deal     Deal
	frames   []frame
}

// frame is the scratch for one decision node on the current path.
type frame struct {
	regret, strat, delta [MaxActions]float32
	fixed                [MaxActions]float64
}

func (w *worker) traverse(ni int32, trav, depth int) float32 {
	n := &w.nodes[ni]
	switch n.Kind {
	case Fold:
		if int(n.Player) == trav {
			return -n.Stake
		}
		return n.Stake
	case Showdown:
		v := float32(w.deal.Sign) * n.Stake
		if trav == 1 {
			v = -v
		}
		return v
	}
	na := int(n.NumActions)
	player := int(n.Player)
	off := n.Slot(int(w.deal.Bucket[player][n.Round]))
	kids := w.children[n.FirstChild : int(n.FirstChild)+na]
	f := &w.frames[depth]
	if fixed := w.s.Config.Fixed[player]; fixed != nil {
		// A seat that does not learn: follow one action of its strategy.
		probs := f.fixed[:na]
		fixed.Probs(n, int(w.deal.Bucket[player][n.Round]), probs)
		return w.traverse(kids[sampleProbs(probs, w.rnd.float64())], trav, depth+1)
	}
	regret, strat, delta := f.regret[:na], f.strat[:na], f.delta[:na]
	w.s.regret.Load(off, regret)
	regretMatch(regret, strat)

	if player != trav {
		// The opponent's node: count this visit in its average strategy,
		// then follow one sampled action.
		unit := w.s.sum.VisitUnit()
		for a := range delta {
			delta[a] = strat[a] * unit
		}
		w.s.sum.Add(off, delta, 0, w.rnd)
		return w.traverse(kids[sampleProbs(strat, w.rnd.float32())], trav, depth+1)
	}

	var ev float32
	for a := range delta {
		delta[a] = w.traverse(kids[a], trav, depth+1)
		ev += strat[a] * delta[a]
	}
	for a := range delta {
		delta[a] -= ev // utilities become regret increments
	}
	w.s.regret.Add(off, delta, w.s.floor, w.rnd)
	return ev
}

// regretMatch fills strat with the regret-matching strategy: proportional
// to positive regret, uniform when there is none.
func regretMatch[F float32 | float64](regret, strat []F) {
	var total F
	for _, r := range regret {
		if r > 0 {
			total += r
		}
	}
	if total <= 0 {
		u := 1 / F(len(strat))
		for a := range strat {
			strat[a] = u
		}
		return
	}
	for a, r := range regret {
		if r > 0 {
			strat[a] = r / total
		} else {
			strat[a] = 0
		}
	}
}

// sampleProbs picks an index from a probability vector with u in [0, 1).
func sampleProbs[F float32 | float64](probs []F, u F) int {
	var cum F
	for a, p := range probs {
		cum += p
		if u < cum {
			return a
		}
	}
	// Rounding left cum a hair under 1: fall back to the last action with
	// any probability.
	for a := len(probs) - 1; a > 0; a-- {
		if probs[a] > 0 {
			return a
		}
	}
	return 0
}

// AverageStrategy is the solver's average strategy, the one that converges.
func (s *Solver) AverageStrategy() Strategy { return averageStrategy{s.sum} }

type averageStrategy struct{ sum Store }

func (a averageStrategy) Probs(n *Node, bucket int, out []float64) {
	var buf [MaxActions]float32
	vals := buf[:n.NumActions]
	a.sum.Load(n.Slot(bucket), vals)
	normalize(vals, out)
}

// normalize turns non-negative weights into probabilities, uniform when
// they are all zero (an infoset the opponent never reached).
func normalize(vals []float32, out []float64) {
	var total float64
	for _, v := range vals {
		total += float64(v)
	}
	for a, v := range vals {
		if total > 0 {
			out[a] = float64(v) / total
		} else {
			out[a] = 1 / float64(len(vals))
		}
	}
}
