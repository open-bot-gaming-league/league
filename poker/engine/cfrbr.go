package engine

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
)

// Sampled-subgame CFR-BR (Johanson, Bard, Burch, Bowling 2012, "Finding
// optimal abstract strategies in extensive-form games"). CFRBR.md has the
// design and the reasons; this is the short version.
//
// The abstract player A sees buckets and learns by CFR. Its opponent R
// sees real cards. Before the first board deal (the "trunk": preflop) R
// learns by CFR too, over real hands. After it R best-responds, exactly,
// to A's current strategy. So A is trained against the opponent that
// exploitability measures, and its average strategy goes to the least
// exploitable strategy the buckets can express, instead of to the
// solution of the bucketed game.
//
// One iteration samples the first board deal and, for each seat as R,
// makes two passes of the best-response walk over the trunk and the whole
// subgame under that deal:
//
//	pass 1  R's values against A's reach. In the subgame R maximises and
//	        the action each hand takes is recorded; in the trunk R plays
//	        its current strategy and its regrets are collected. A's
//	        strategy sums are collected here, where its reach is known.
//	pass 2  A's values against R's reach (trunk strategy, then the
//	        recorded actions), collected by bucket into A's regrets.
//
// The two seats' problems are independent and share the arrays: a decision
// node belongs to A in one of them and to R in the other.

type CFRBRConfig struct {
	Seed uint64
	// Workers > 1 runs iterations concurrently, each reading the shared
	// arrays at its start and adding its changes at its end, without locks
	// (see TrainConfig.Workers). Workers = 1 is deterministic for a seed.
	Workers int
	// RegretFloor clamps A's cumulative regret from below (<= 0): -Inf is
	// plain regret matching, 0 is regret matching+.
	RegretFloor float32
	// MaxChoiceBytes, when positive, makes NewCFRBR refuse a game whose
	// table of recorded choices (per worker; see ChoiceBytes) is larger.
	// The table grows with the betting tree's river (tens of MB on the 20 bb
	// tree, over 1 GB on the 50 bb table tree, far more on a Bot Poker
	// tree), and it is cleared every
	// iteration, so an oversized one would be touched in full.
	MaxChoiceBytes int64
}

func DefaultCFRBRConfig() CFRBRConfig {
	return CFRBRConfig{Seed: 1, Workers: 1, RegretFloor: float32(math.Inf(-1)), MaxChoiceBytes: 8 << 30}
}

type CFRBR struct {
	Game   *Game
	Config CFRBRConfig
	// A's regrets and strategy sums, for both seats, laid out by Node.Slot.
	regret, sum Store
	// R's trunk regrets, for both seats: a block of classes x actions per
	// first-round decision node, starting at trunkOffset[node].
	trunkRegret []float64
	trunkOffset []int64
	// class numbers the trunk's real-card infosets: a hand's class is the
	// hand itself, or with suit-symmetric rules its orbit under suit
	// relabelling (hold'em's 169). That is lossless before any board card
	// is out, and it keeps R's reach suit-invariant, which the walk's
	// symmetry inside a subgame relies on.
	class   []int32
	classes int

	// The table of recorded choices: per round, per board, one block per
	// decision node of R's seat, holding a choice per hand in choiceWidth
	// bits (1, 2 or 4, by the node's number of actions). choiceOffset is
	// where a node's block starts within its board; roundBytes is a
	// board's size (the larger seat's); fan is how many ways the round's
	// cards can fall (1 for the sampled first deal). The walk reads and
	// writes choices a byte per hand, in scratch indexed by nodeDepth.
	choiceOffset []int64
	choiceWidth  []uint8
	nodeDepth    []uint8
	roundBytes   [MaxRounds]int64
	roundHands   [MaxRounds]int
	roundBoards  [MaxRounds]int
	fan          [MaxRounds]int

	iters   int64
	workers []*cfrbrWorker
}

func NewCFRBR(g *Game, cfg CFRBRConfig) (*CFRBR, error) {
	r, t := g.Rules, g.Tree
	if r.Rounds() < 2 || r.Board[0] != 0 || r.Board[1] == 0 {
		return nil, fmt.Errorf("engine: CFR-BR samples the first board deal; %s has none after its first round", r.Name)
	}
	if cfg.Workers < 1 {
		return nil, fmt.Errorf("engine: workers must be >= 1")
	}
	if cfg.RegretFloor > 0 {
		return nil, fmt.Errorf("engine: regret floor must be <= 0")
	}
	c := &CFRBR{Game: g, Config: cfg}
	var err error
	if c.regret, err = NewStore(Float32, t.Slots); err != nil {
		return nil, err
	}
	if c.sum, err = NewStore(Float32, t.Slots); err != nil {
		return nil, err
	}
	c.class, c.classes = r.trunkClasses()

	for round := 1; round < r.Rounds(); round++ {
		c.fan[round], c.roundBoards[round] = 1, 1
		if round > 1 {
			c.fan[round] = binom[r.Deck-r.BoardSize(round-1)][r.Board[round]]
			c.roundBoards[round] = c.roundBoards[round-1] * c.fan[round]
		}
		c.roundHands[round] = binom[r.Deck-r.BoardSize(round)][r.Hole]
	}
	c.trunkOffset = make([]int64, len(t.Nodes))
	c.choiceOffset = make([]int64, len(t.Nodes))
	c.choiceWidth = make([]uint8, len(t.Nodes))
	c.nodeDepth = make([]uint8, len(t.Nodes))
	var trunkSlots int64
	var seatBytes [MaxRounds][2]int64
	var number func(ni int32, depth uint8)
	number = func(ni int32, depth uint8) {
		n := &t.Nodes[ni]
		if n.Kind != Decision {
			return
		}
		c.nodeDepth[ni] = depth
		if n.Round == 0 {
			c.trunkOffset[ni] = trunkSlots
			trunkSlots += int64(c.classes) * int64(n.NumActions)
		} else {
			c.choiceWidth[ni] = choiceBits(int(n.NumActions))
			c.choiceOffset[ni] = seatBytes[n.Round][n.Player]
			seatBytes[n.Round][n.Player] += packedBytes(c.roundHands[n.Round], c.choiceWidth[ni])
		}
		for a := 0; a < int(n.NumActions); a++ {
			number(t.Children[int(n.FirstChild)+a], depth+1)
		}
	}
	number(0, 0)
	c.trunkRegret = make([]float64, trunkSlots)
	for round := range seatBytes {
		c.roundBytes[round] = max(seatBytes[round][0], seatBytes[round][1])
	}
	if need := c.ChoiceBytes(); cfg.MaxChoiceBytes > 0 && need > cfg.MaxChoiceBytes {
		last := r.Rounds() - 1
		return nil, fmt.Errorf("engine: CFR-BR on this tree records %.1f GB of choices per worker (%d river boards x %d bytes for one seat's river decision nodes); the limit is %.1f GB",
			float64(need)/(1<<30), c.roundBoards[last], c.roundBytes[last], float64(cfg.MaxChoiceBytes)/(1<<30))
	}
	c.SetWorkers(cfg.Workers)
	return c, nil
}

// SetWorkers changes how many goroutines later Run calls use (at least 1),
// so a long run can take up or give back threads on a shared machine.
func (c *CFRBR) SetWorkers(n int) {
	n = max(n, 1)
	for w := len(c.workers); w < n; w++ {
		c.workers = append(c.workers, c.newWorker(newRNG(c.Config.Seed, uint64(w))))
	}
	c.workers = c.workers[:n] // a dropped worker's tables go with it
	c.Config.Workers = n
}

// trunkClasses numbers the real-card infosets before any board card: see
// CFRBR.class.
func (r *Rules) trunkClasses() (class []int32, classes int) {
	hands := r.Hands()
	class = make([]int32, len(hands))
	if !r.SuitSymmetric {
		for h := range class {
			class[h] = int32(h)
		}
		return class, len(hands)
	}
	perms := r.handPerms()
	for h := range class {
		first := h // the lowest-numbered hand of h's orbit
		for _, perm := range perms {
			first = min(first, int(perm[h]))
		}
		if first == h {
			class[h] = int32(classes)
			classes++
		} else {
			class[h] = class[first]
		}
	}
	return class, classes
}

// Iterations is the number of sampled first deals so far. Each is solved
// for both seats.
func (c *CFRBR) Iterations() int64 { return c.iters }

// ChoiceBytes is the size of one worker's table of recorded choices, the
// only large allocation.
func (c *CFRBR) ChoiceBytes() int64 {
	var total int64
	for round := range c.roundBoards {
		total += int64(c.roundBoards[round]) * c.roundBytes[round]
	}
	return total
}

// choiceBits is how many bits a recorded choice among n actions takes:
// 1, 2, 4 or 8, so that a byte always holds a whole number of them.
func choiceBits(n int) uint8 {
	for _, w := range []uint8{1, 2, 4} {
		if n <= 1<<w {
			return w
		}
	}
	return 8
}

// packedBytes is the size of n choices of w bits each.
func packedBytes(n int, w uint8) int64 { return int64(n*int(w)+7) / 8 }

// packChoices writes src, one choice per byte, into dst at w bits each,
// lowest bits first. dst must hold packedBytes(len(src), w).
func packChoices(dst []byte, src []uint8, w uint8) {
	per := 8 / int(w)
	for j := range dst {
		group := src[j*per : min((j+1)*per, len(src))]
		var b byte
		for k, c := range group {
			b |= c << (uint(k) * uint(w))
		}
		dst[j] = b
	}
}

// unpackChoices is packChoices backwards.
func unpackChoices(dst []uint8, src []byte, w uint8) {
	per := 8 / int(w)
	mask := byte(1)<<w - 1
	for j, b := range src {
		group := dst[j*per : min((j+1)*per, len(dst))]
		for k := range group {
			group[k] = b >> (uint(k) * uint(w)) & mask
		}
	}
}

// AverageStrategy is A's average strategy, the one the paper's theorem is
// about.
func (c *CFRBR) AverageStrategy() Strategy { return averageStrategy{c.sum} }

// CurrentStrategy is A's strategy on the next iteration: regret matching
// on its regrets now.
func (c *CFRBR) CurrentStrategy() Strategy { return currentStrategy{c.regret} }

type currentStrategy struct{ regret Store }

func (s currentStrategy) Probs(n *Node, bucket int, out []float64) {
	var regret, strat [MaxActions]float32
	na := int(n.NumActions)
	s.regret.Load(n.Slot(bucket), regret[:na])
	regretMatch(regret[:na], strat[:na])
	for a := range out {
		out[a] = float64(strat[a])
	}
}

// Blueprint is one of c's strategies in served form.
func (c *CFRBR) Blueprint(strat Strategy) *Blueprint {
	b := NewBlueprint(c.Game, strat)
	b.Header.Iterations = c.iters
	b.Header.Regrets, b.Header.Sums = c.regret.Kind(), c.sum.Kind()
	b.Header.Seed = c.Config.Seed
	return b
}

// Run does n more iterations and returns what R won per hand in each seat,
// averaged over them: R's trunk strategy and subgame best response against
// A's strategy of the moment, on the sampled deals. Averaged over the
// seats it is a noisy reading of the current strategy's exploitability,
// from below in so far as R's trunk play is still learning.
//
// mismatch is the largest |R's value + A's value| over the n iterations.
// The two come from different passes and must cancel, so anything above
// rounding (1e-12 or so) says the passes did not describe the same game
// and the run should not be trusted.
func (c *CFRBR) Run(n int64) (realValue [2]float64, mismatch float64) {
	var next atomic.Int64
	var wg sync.WaitGroup
	totals := make([][3]float64, len(c.workers)) // R's winnings by seat, then the worker's largest mismatch
	for wi, w := range c.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= n {
				real, abstract := w.iterate()
				for seat := range real {
					totals[wi][seat] += real[seat]
					totals[wi][2] = max(totals[wi][2], math.Abs(real[seat]+abstract[seat]))
				}
			}
		}()
	}
	wg.Wait()
	c.iters += n
	for _, t := range totals {
		realValue[0] += t[0] / float64(n)
		realValue[1] += t[1] / float64(n)
		mismatch = max(mismatch, t[2])
	}
	return realValue, mismatch
}

// cfrbrWorker is one goroutine's state: an evaluator in learning mode, its
// scratch, and the learner.
type cfrbrWorker struct {
	c     *CFRBR
	e     *evaluator
	ar    *arena
	l     *learner
	rnd   *rng
	scale float64 // one deal of hole cards, as a probability
}

// learner is what the best-response walk consults when it is a CFR-BR
// pass (evaluator.learn).
type learner struct {
	c    *CFRBR
	real int // R's seat in the passes under way

	// Snapshots taken at the start of the iteration: A's current strategy
	// over every slot, and R's current trunk strategy laid out like
	// trunkRegret.
	strat []float64
	trunk []float64
	// What the iteration adds to the shared arrays at its end.
	dRegret, dSum []float64
	dTrunk        []float64

	sampled [1]outcome // the first deal
	// Where the walk is: per round, the weight of the public state (the
	// chance of its cards given the players' hands, times the boards it
	// stands for, times the sampling correction) and its ordinal among the
	// round's boards.
	weight [MaxRounds]float64
	board  [MaxRounds]int
	// The packed table (see CFRBR.choiceOffset), and per tree depth the
	// vector the walk works on: a node's choices unpacked. One per depth
	// is enough, because while a node's vector is in use the walk visits
	// only nodes below it.
	choice  [MaxRounds][]byte
	scratch [][]uint8
	// orbit[round], while orbitOf[round] is set, maps each hand of the
	// round's public state to the first hand of its orbit under the
	// state's suit relabellings (see record). local is scratch for
	// building it: global hand to place in the public state.
	orbit   [MaxRounds][]int32
	orbitOf [MaxRounds]bool
	local   []int32
}

func (c *CFRBR) newWorker(rnd *rng) *cfrbrWorker {
	g := c.Game
	r := g.Rules
	l := &learner{c: c,
		strat: make([]float64, g.Tree.Slots), dRegret: make([]float64, g.Tree.Slots), dSum: make([]float64, g.Tree.Slots),
		trunk: make([]float64, len(c.trunkRegret)), dTrunk: make([]float64, len(c.trunkRegret))}
	l.weight[0] = 1
	for round := 1; round < r.Rounds(); round++ {
		l.choice[round] = make([]byte, int64(c.roundBoards[round])*c.roundBytes[round])
	}
	l.scratch = make([][]uint8, g.Tree.Depth())
	for d := range l.scratch {
		l.scratch[d] = make([]uint8, len(r.Hands()))
	}
	l.local = make([]int32, len(r.Hands()))
	for round := range l.orbit {
		l.orbit[round] = make([]int32, len(r.Hands()))
	}
	e := &evaluator{g: g, workers: 1, hands: r.Hands(), lastRound: r.Rounds() - 1, learn: l}
	w := &cfrbrWorker{c: c, e: e, ar: e.newArena(), l: l, rnd: rnd,
		scale: 1 / float64(binom[r.Deck][r.Hole]*binom[r.Deck-r.Hole][r.Hole])}
	e.deal(w.ar.pub[0], nil, nil)
	return w
}

// play implements the table in the comment at the top of the file.
func (l *learner) play(e *evaluator, ni int32, pub *pubState, responderActs bool) nodePlay {
	c := l.c
	n := &c.Game.Tree.Nodes[ni]
	na := int64(n.NumActions)
	switch {
	case int(n.Player) != l.real:
		// A: its snapshot strategy by bucket. As the responder (pass 2) it
		// collects regret; as the opponent (pass 1), strategy sums.
		to := n.Offset + int64(c.Game.Tree.NumBuckets[n.Round])*na
		pl := nodePlay{probs: l.strat[n.Offset:to], index: pub.bucket, acc: l.dSum[n.Offset:to], weight: l.weight[pub.round]}
		if responderActs {
			pl.acc = l.dRegret[n.Offset:to]
		}
		return pl
	case pub.round == 0:
		// R in the trunk: its snapshot strategy by class, collecting
		// regret when the values are its own (pass 1).
		from := c.trunkOffset[ni]
		to := from + int64(c.classes)*na
		pl := nodePlay{probs: l.trunk[from:to], index: c.class, weight: 1}
		if responderActs {
			pl.acc = l.dTrunk[from:to]
		}
		return pl
	}
	// R in the subgame: maximise and record (pass 1), then replay (pass 2).
	choice := l.scratch[c.nodeDepth[ni]][:len(pub.hands)]
	if responderActs {
		clear(choice)
	} else {
		unpackChoices(choice, l.packed(ni, pub), c.choiceWidth[ni])
	}
	return nodePlay{maximise: responderActs, index: pub.bucket, choice: choice}
}

// packed is where a node's choices at a public state live in the table.
func (l *learner) packed(ni int32, pub *pubState) []byte {
	c := l.c
	at := int64(l.board[pub.round])*c.roundBytes[pub.round] + c.choiceOffset[ni]
	return l.choice[pub.round][at : at+packedBytes(len(pub.hands), c.choiceWidth[ni])]
}

// record stores the choices the walk made at a node where R maximised.
//
// First it makes them agree across suit relabellings that leave the board
// as it is. Two hands that such a relabelling swaps have the same values,
// so the same best actions, but rounding can break a tie between two
// actions one way for one hand and the other way for the other. The walk
// solves one board for each class of later boards on the strength of R's
// reach being the same for both hands; were it not, the second pass would
// replay a player whose flop action depends on which turn card is coming,
// and the two passes would stop describing the same game
// (TestCFRBRPassesCancel, the wide menu). So every hand takes the choice
// of the first hand of its orbit.
func (l *learner) record(e *evaluator, ni int32, pub *pubState, choice []uint8) {
	if len(pub.stab) > 1 {
		for i, first := range l.orbits(e, pub) {
			choice[i] = choice[first] // first <= i, and a first hand is its own
		}
	}
	packChoices(l.packed(ni, pub), choice, l.c.choiceWidth[ni])
}

// orbits returns, for each hand of pub, the lowest-placed hand that pub's
// suit relabellings turn it into. Built once per public state.
func (l *learner) orbits(e *evaluator, pub *pubState) []int32 {
	orbit := l.orbit[pub.round][:len(pub.hands)]
	if l.orbitOf[pub.round] {
		return orbit
	}
	// The relabellings fix the board, so they send pub's hands to pub's
	// hands and only those entries of local are read.
	for i, h := range pub.hands {
		l.local[h] = int32(i)
	}
	perms := e.g.Rules.handPerms()
	for i, h := range pub.hands {
		first := int32(i)
		for _, p := range pub.stab {
			first = min(first, l.local[perms[p][h]])
		}
		orbit[i] = first
	}
	l.orbitOf[pub.round] = true
	return orbit
}

// enter is called before the walk deals the ordinal-th outcome of a
// round's cards; factor is the outcome's weight relative to its parent.
func (l *learner) enter(round, ordinal int, factor float64) {
	l.weight[round] = l.weight[round-1] * factor
	l.board[round] = l.board[round-1]*l.c.fan[round] + ordinal
	l.orbitOf[round] = false
}

// iterate runs one iteration and returns, per seat of R, what R won and
// what A won, per hand. The two are computed by different passes and must
// cancel.
func (w *cfrbrWorker) iterate() (realValue, abstractValue [2]float64) {
	l := w.l
	w.snapshot()
	r := w.c.Game.Rules
	l.sampled[0] = outcome{count: r.Board[1]}
	copy(l.sampled[0].cards[:], sortedDraw(w.rnd, r.Deck, r.Board[1]))
	for real := 0; real < 2; real++ {
		l.real = real
		for round := range l.choice {
			clear(l.choice[round])
		}
		w.e.responder, w.e.best = real, true
		realValue[real] = w.pass()
		w.e.responder, w.e.best = 1-real, false
		abstractValue[real] = w.pass()
	}
	w.apply()
	return realValue, abstractValue
}

// sortedDraw draws n distinct cards of a deck uniformly, ascending.
func sortedDraw(rnd *rng, deck, n int) []Card {
	var used uint64
	cs := make([]Card, 0, n)
	for len(cs) < n {
		c := rnd.intn(deck)
		if used&(1<<uint(c)) == 0 {
			used |= 1 << uint(c)
			cs = append(cs, Card(c))
		}
	}
	sortCards(cs)
	return cs
}

// pass walks the trunk and the sampled subgame for the evaluator's current
// responder and returns the responder's value per hand dealt.
func (w *cfrbrWorker) pass() float64 {
	mark := w.ar.mark()
	pub := w.ar.pub[0]
	size := len(pub.hands)
	root := entry{node: 0, reach: w.ar.scratch(size), out: w.ar.scratch(size)}
	for i := range root.reach {
		root.reach[i] = w.scale
	}
	w.e.solve(pub, []entry{root}, w.ar)
	var total float64
	for _, v := range root.out {
		total += v
	}
	w.ar.release(mark)
	return total
}

// snapshot reads the strategies this iteration is played against out of
// the shared regrets.
func (w *cfrbrWorker) snapshot() {
	c, l := w.c, w.l
	t := c.Game.Tree
	var regret, strat [MaxActions]float32
	for i := range t.Nodes {
		n := &t.Nodes[i]
		if n.Kind != Decision {
			continue
		}
		na := int(n.NumActions)
		for b := 0; b < t.NumBuckets[n.Round]; b++ {
			off := n.Slot(b)
			c.regret.Load(off, regret[:na])
			regretMatch(regret[:na], strat[:na])
			for a, p := range strat[:na] {
				l.strat[off+int64(a)] = float64(p)
			}
		}
		if n.Round == 0 {
			for k := 0; k < c.classes; k++ {
				from := c.trunkOffset[i] + int64(k*na)
				regretMatch(c.trunkRegret[from:from+int64(na)], l.trunk[from:from+int64(na)])
			}
		}
	}
}

// apply adds the iteration's deltas to the shared arrays and zeroes them.
func (w *cfrbrWorker) apply() {
	c, l := w.c, w.l
	t := c.Game.Tree
	var delta [MaxActions]float32
	add := func(to Store, from []float64, off int64, floor float32) {
		changed := false
		for a, d := range from {
			delta[a] = float32(d)
			changed = changed || d != 0
			from[a] = 0
		}
		if changed {
			to.Add(off, delta[:len(from)], floor, w.rnd)
		}
	}
	for i := range t.Nodes {
		n := &t.Nodes[i]
		if n.Kind != Decision {
			continue
		}
		na := int64(n.NumActions)
		for b := 0; b < t.NumBuckets[n.Round]; b++ {
			off := n.Slot(b)
			add(c.regret, l.dRegret[off:off+na], off, c.Config.RegretFloor)
			add(c.sum, l.dSum[off:off+na], off, 0)
		}
	}
	for i, d := range l.dTrunk {
		c.trunkRegret[i] += d
		l.dTrunk[i] = 0
	}
}
