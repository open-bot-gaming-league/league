package engine

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Exact best response against a fixed strategy, in the real card game.
//
// The responder sees its actual hole cards and the board; the fixed
// strategy sees only its buckets. So for a lossless abstraction (Kuhn,
// Leduc) this is the game's exploitability, and for a bucketed hold'em
// blueprint it is what a perfect opponent wins against the blueprint when
// both are confined to the blueprint's betting tree.
//
// The walk is over (betting node, public cards) with one value per
// responder hand and one reach weight per opponent hand, so a terminal
// costs O(hands) rather than O(hands^2): the standard public-tree
// evaluation (Johanson et al. 2011, "Accelerating best response
// calculation in large extensive games").

// Exploitability returns how much a best responder wins per hand against
// strat, averaged over the two seats, and each seat's best-response value.
func Exploitability(g *Game, strat Strategy, workers int) (expl float64, br [2]float64) {
	for p := 0; p < 2; p++ {
		br[p] = BestResponseValue(g, strat, p, workers)
	}
	return (br[0] + br[1]) / 2, br
}

// BestResponseValue is the expected winnings of a best-responding player
// in seat responder against strat in the other seat.
func BestResponseValue(g *Game, strat Strategy, responder, workers int) float64 {
	return newEvaluator(g, against(strat, responder), responder, true, workers).rootValue()
}

// BestResponseProfile is BestResponseValue plus where the time went.
func BestResponseProfile(g *Game, strat Strategy, responder, workers int) (float64, BRProfile) {
	e := newEvaluator(g, against(strat, responder), responder, true, workers)
	start := time.Now()
	value := e.rootValue()
	prof := BRProfile{WallSeconds: time.Since(start).Seconds()}
	for r := range prof.BettingSeconds {
		prof.BettingSeconds[r] = time.Duration(e.bettingNanos[r].Load()).Seconds()
		prof.DealSeconds[r] = time.Duration(e.dealNanos[r].Load()).Seconds()
		prof.Boards[r] = e.boards[r].Load()
	}
	return value, prof
}

// BRProfile breaks one best-response computation down by round. The
// per-round seconds are summed over worker goroutines (thread time), so
// they add up to about WallSeconds times the workers actually running.
type BRProfile struct {
	WallSeconds    float64
	BettingSeconds [MaxRounds]float64 // walking the round's betting nodes, terminals included
	DealSeconds    [MaxRounds]float64 // dealing into the round: hand lists, buckets, strengths, copying reach in and values out
	Boards         [MaxRounds]int64   // public states dealt in the round
}

// ProfileValue is seat 0's exact expected winnings when seat p plays
// strats[p].
func ProfileValue(g *Game, strats [2]Strategy, workers int) float64 {
	return newEvaluator(g, strats, 0, false, workers).rootValue()
}

type evaluator struct {
	g         *Game
	strats    [2]Strategy // by seat; nil for a responder that maximises
	responder int
	best      bool // responder maximises; otherwise it plays strats[responder]
	workers   int
	hands     [][]Card // every hole-card set, by global hand index
	lastRound int
	// probs[p] is strats[p] over every infoset, laid out like the solver's
	// arrays (Node.Slot), so the walk reads probabilities instead of
	// calling the strategy at every node of every board. 8 bytes per
	// infoset-action per fixed strategy.
	probs [2][]float64
	// learn, when set, turns the walk into one pass of a CFR-BR iteration
	// (cfrbr.go): it decides how each seat plays each node and collects
	// what the trainer needs. Nil for every plain evaluation.
	learn *learner

	// Thread time by round, for BestResponseProfile.
	bettingNanos, dealNanos, boards [MaxRounds]atomic.Int64
}

// newEvaluator sets up a walk in which each seat with a strategy plays it
// and a responder without one (best == true) maximises.
func newEvaluator(g *Game, strats [2]Strategy, responder int, best bool, workers int) *evaluator {
	r := g.Rules
	if r.Board[0] != 0 {
		panic("engine: best response assumes no board before the first round")
	}
	e := &evaluator{g: g, strats: strats, responder: responder, best: best, workers: max(workers, 1),
		hands: r.Hands(), lastRound: r.Rounds() - 1}
	t := g.Tree
	for p, strat := range strats {
		if strat == nil {
			continue
		}
		e.probs[p] = make([]float64, t.Slots)
		parallelFor(len(t.Nodes), e.workers, func(from, to, _ int) {
			for i := from; i < to; i++ {
				n := &t.Nodes[i]
				if n.Kind != Decision || int(n.Player) != p {
					continue
				}
				na := int64(n.NumActions)
				for b := 0; b < t.NumBuckets[n.Round]; b++ {
					off := n.Slot(b)
					strat.Probs(n, b, e.probs[p][off:off+na])
				}
			}
		})
	}
	return e
}

// against is the strategy pair for a best response by seat responder to
// strat.
func against(strat Strategy, responder int) (strats [2]Strategy) {
	strats[1-responder] = strat
	return strats
}

func (e *evaluator) rootValue() float64 {
	r := e.g.Rules
	ar := e.newArena()
	pub := ar.pub[0]
	e.deal(pub, nil, nil)
	n := len(pub.hands)
	root := entry{node: 0, reach: ar.get(n), out: ar.get(n)}
	for i := range root.reach {
		root.reach[i] = 1
	}
	e.solve(pub, []entry{root}, ar)
	var total float64
	for _, v := range root.out {
		total += v
	}
	return total / float64(binom[r.Deck][r.Hole]*binom[r.Deck-r.Hole][r.Hole])
}

// pubState is the public card state. The hands the board does not block
// are numbered densely ("local" index), and every vector in the walk is
// indexed that way, so the inner loops are plain sweeps. In the last round
// the local order is by showdown strength, which is what the showdown
// evaluation needs.
type pubState struct {
	round  int
	board  []Card  // ascending
	hands  []int32 // local -> global hand index
	parent []int32 // local -> local index in the state this was dealt from
	local  []int32 // global hand -> local index (not kept for the last round)
	bucket []int32 // local -> bucket in this round
	// stab lists the suit relabellings (indices into suitPerms) that map
	// every set of cards dealt so far onto itself. Boards that differ by
	// one of them are solved once (chance). Empty unless the rules are
	// suit-symmetric.
	stab []uint8

	// Card removal. Terminal values need, for each hand, the opponent reach
	// over hands sharing one of its cards. For that the hands holding each
	// card are kept as a list in local order; every card is held by the
	// same number of hands, so the lists form a table of lists x listLen.
	// A one-card hand's missing second card is list number `lists`, whose
	// total is always zero.
	lists, listLen int
	self           float64    // hole cards - 1: how many times too often inclusion-exclusion removes a hand's own reach
	listHands      []uint16   // list after list: the hands holding the card
	handList       [2][]uint8 // per card slot: local -> its card's list number

	// Last round only: the strength order cut into groups of tied hands,
	// and for each list entry how many of the list's hands are weaker
	// than, and weaker than or tied with, the entry's hand.
	strength   []uint32 // local -> showdown strength, ascending
	group      []uint16 // local -> tie group
	groupStart []int32  // group -> its first hand; one extra entry = number of hands
	listBelow  []uint8  // parallel to listHands
	listUpto   []uint8

	keys   []uint64 // scratch: sort keys
	spot   []int32  // scratch: per hand and card slot, where it sits in its card's list
	weaker []uint8  // scratch: per hand and card slot, weaker hands in its card's list
}

// arena is one goroutine's scratch: a stack of float64 vectors, grown a
// chunk at a time and never shrunk, and one pubState per round (the walk
// is depth-first, so one per round is all that is ever live).
type arena struct {
	chunks   [][]float64
	cur, top int // next free slot is chunks[cur][top]
	pub      []*pubState
	// Reused per round, so that a walk over millions of boards allocates
	// nothing per board: the leaves collected in the round, the entries
	// made for it from the round before, and the deals out of it.
	leaves   [][]entry
	entries  [][]entry
	outcomes [][]outcome
	perms    [][][]int32 // backing for the outcomes' perms
	targets  [][]int32   // see chanceOutcome
	// A stack of reachSums, marked and released with the vectors. They are
	// handed out by pointer and would otherwise be heap-allocated once per
	// node visit.
	sums    []*reachSums
	sumsTop int
}

type arenaMark struct{ cur, top, sumsTop int }

func (e *evaluator) newArena() *arena {
	ar := &arena{chunks: [][]float64{make([]float64, len(e.hands)*256)}}
	for r := 0; r <= e.lastRound; r++ {
		ar.pub = append(ar.pub, &pubState{})
	}
	ar.leaves = make([][]entry, e.lastRound+1)
	ar.entries = make([][]entry, e.lastRound+1)
	ar.outcomes = make([][]outcome, e.lastRound+1)
	ar.perms = make([][][]int32, e.lastRound+1)
	ar.targets = make([][]int32, e.lastRound+1)
	return ar
}

// scratch returns a vector with arbitrary contents that stays valid until
// release is called with a mark taken before it.
func (a *arena) scratch(n int) []float64 {
	for a.top+n > len(a.chunks[a.cur]) {
		if a.cur+1 == len(a.chunks) {
			a.chunks = append(a.chunks, make([]float64, max(n, len(a.chunks[0]))))
		}
		a.cur++
		a.top = 0
	}
	s := a.chunks[a.cur][a.top : a.top+n]
	a.top += n
	return s
}

// get is scratch, zeroed.
func (a *arena) get(n int) []float64 {
	s := a.scratch(n)
	clear(s)
	return s
}

func (a *arena) mark() arenaMark     { return arenaMark{a.cur, a.top, a.sumsTop} }
func (a *arena) release(m arenaMark) { a.cur, a.top, a.sumsTop = m.cur, m.top, m.sumsTop }

// deal fills pub with parent's board plus cards (parent nil: the empty
// board, every hand, local index = global index).
func (e *evaluator) deal(pub, parent *pubState, cards []Card) {
	r := e.g.Rules
	pub.round, pub.board, pub.stab = 0, pub.board[:0], pub.stab[:0]
	if parent != nil {
		pub.round = parent.round + 1
		pub.board = append(pub.board, parent.board...)
		// A relabelling survives if it also maps the new cards onto
		// themselves.
		self := combIndex(cards)
		for _, p := range parent.stab {
			if permuteIndex(cards, &suitPerms[p]) == self {
				pub.stab = append(pub.stab, p)
			}
		}
	} else if r.SuitSymmetric {
		for p := range suitPerms {
			pub.stab = append(pub.stab, uint8(p))
		}
	}
	pub.board = append(pub.board, cards...)
	sortCards(pub.board)
	blocked := cardMask(cards)
	last := pub.round == e.lastRound

	// Survivors of the new cards, as (strength, parent-local index) keys;
	// strength is 0 before the last round, which keeps the parent's order.
	pub.keys = pub.keys[:0]
	count := len(e.hands)
	if parent != nil {
		count = len(parent.hands)
	}
	for j := 0; j < count; j++ {
		h := j
		if parent != nil {
			h = int(parent.hands[j])
		}
		hand := e.hands[h]
		if cardMask(hand)&blocked != 0 {
			continue
		}
		var strength uint64
		if last {
			strength = uint64(r.Strength(hand, pub.board))
		}
		pub.keys = append(pub.keys, strength<<32|uint64(j))
	}
	if last {
		slices.Sort(pub.keys)
	}

	n := len(pub.keys)
	pub.hands, pub.parent, pub.bucket = pub.hands[:0], pub.parent[:0], pub.bucket[:0]
	pub.strength = pub.strength[:0]
	if !last {
		if pub.local == nil {
			pub.local = make([]int32, len(e.hands))
		}
		for h := range pub.local {
			pub.local[h] = -1
		}
	}
	for i, key := range pub.keys {
		j := int32(uint32(key))
		h := j
		if parent != nil {
			h = parent.hands[j]
		}
		hand := e.hands[h]
		pub.hands = append(pub.hands, h)
		pub.parent = append(pub.parent, j)
		pub.bucket = append(pub.bucket, int32(e.g.Abs.Bucket(pub.round, hand, pub.board)))
		pub.strength = append(pub.strength, uint32(key>>32))
		if !last {
			pub.local[h] = int32(i)
		}
	}

	// Number the cards still in play; each gets a list of listLen hands.
	hole := r.Hole
	pub.self = float64(hole - 1)
	var listOf [maxDeck]uint8
	pub.lists = 0
	dead := cardMask(pub.board)
	for c := 0; c < r.Deck; c++ {
		if dead&(1<<uint(c)) == 0 {
			listOf[c] = uint8(pub.lists)
			pub.lists++
		}
	}
	pub.listLen = 0
	if pub.lists > 0 {
		pub.listLen = n * hole / pub.lists
	}
	if pub.listLen*pub.lists != n*hole || pub.listLen > 255 {
		panic("engine: best response expects every card to be held by the same number of hands, at most 255")
	}
	pub.listHands = slices.Grow(pub.listHands[:0], n*hole)[:n*hole]
	for s := range pub.handList {
		pub.handList[s] = slices.Grow(pub.handList[s][:0], n)[:n]
		if s >= hole {
			for i := range pub.handList[s] {
				pub.handList[s][i] = uint8(pub.lists)
			}
		}
	}
	var filled [maxDeck]int32 // hands placed in each list so far
	pub.spot = slices.Grow(pub.spot[:0], n*hole)[:n*hole]
	place := func(i int) {
		for s, c := range e.hands[pub.hands[i]] {
			l := listOf[c]
			at := int32(l)*int32(pub.listLen) + filled[l]
			pub.listHands[at] = uint16(i)
			pub.handList[s][i] = l
			pub.spot[i*hole+s] = at
			filled[l]++
		}
	}
	if !last {
		for i := 0; i < n; i++ {
			place(i)
		}
		return
	}
	// Last round: hands arrive in strength order, so placing them a tie
	// group at a time tells each list entry how many of its list are
	// weaker (the fill before the group) and weaker or tied (after it).
	pub.listBelow = slices.Grow(pub.listBelow[:0], n*hole)[:n*hole]
	pub.listUpto = slices.Grow(pub.listUpto[:0], n*hole)[:n*hole]
	pub.group = slices.Grow(pub.group[:0], n)[:n]
	pub.weaker = slices.Grow(pub.weaker[:0], n*hole)[:n*hole]
	pub.groupStart = pub.groupStart[:0]
	for i := 0; i < n; {
		j := i
		for ; j < n && pub.strength[j] == pub.strength[i]; j++ {
			pub.group[j] = uint16(len(pub.groupStart))
		}
		pub.groupStart = append(pub.groupStart, int32(i))
		// Nothing of this group is placed yet, so the fill of a list now
		// is its number of weaker hands.
		for k := i; k < j; k++ {
			for s, c := range e.hands[pub.hands[k]] {
				pub.weaker[k*hole+s] = uint8(filled[listOf[c]])
			}
		}
		for k := i; k < j; k++ {
			place(k)
		}
		for k := i; k < j; k++ {
			for s, c := range e.hands[pub.hands[k]] {
				at := pub.spot[k*hole+s]
				pub.listBelow[at] = pub.weaker[k*hole+s]
				pub.listUpto[at] = uint8(filled[listOf[c]])
			}
		}
		i = j
	}
	pub.groupStart = append(pub.groupStart, int32(n))
}

// entry is a betting node entered at some public state: the opponent's
// reach weight per hand on the way in, and the responder's value per hand
// on the way out. out[i] is the sum over opponent hands j of reach[j]
// times the responder's value given (hand i, hand j, public cards), under
// best play (or strat) below. Vectors are in the public state's local
// hand order.
type entry struct {
	node  int32
	reach []float64
	out   []float64
}

// solve fills in out for every entry at pub. All the entries share pub, so
// each further board is dealt once for all of them, however many betting
// lines lead there. Three passes: walk this round's betting to find where
// each line leaves the round and with what reach; deal the next cards and
// solve those leaves; walk the betting again to back the values up.
func (e *evaluator) solve(pub *pubState, entries []entry, ar *arena) {
	mark := ar.mark()
	start := time.Now()
	var below time.Duration // time spent in later rounds
	leaves := ar.leaves[pub.round][:0]
	if pub.round < e.lastRound {
		for _, en := range entries {
			e.walk(en.node, pub, en.reach, nil, &leaves, nil, ar)
		}
		ar.leaves[pub.round] = leaves // keep the grown backing array
		if len(leaves) > 0 {
			chanceStart := time.Now()
			e.chance(pub, leaves, ar)
			below = time.Since(chanceStart)
		}
	}
	for _, en := range entries {
		e.walk(en.node, pub, en.reach, en.out, &leaves, e.newReachSums(pub, ar), ar)
	}
	e.bettingNanos[pub.round].Add(int64(time.Since(start) - below))
	e.boards[pub.round].Add(1)
	ar.release(mark)
}

// walk visits node ni and everything below it in the current round. With
// out == nil it only collects, appending to *leaves every node that needs
// more board cards (with a copy of its reach, and space for its values).
// With out != nil it computes values into out (every element is written,
// whatever out held), taking the leaves' values from the front of *leaves,
// which it consumes in the same order; sums is then the running-sum
// scratch for this reach vector, shared by every terminal that is
// evaluated with it.
func (e *evaluator) walk(ni int32, pub *pubState, reach, out []float64, leaves *[]entry, sums *reachSums, ar *arena) {
	t := e.g.Tree
	n := &t.Nodes[ni]
	collect := out == nil
	size := len(reach)
	switch {
	case n.Kind == Fold:
		if !collect {
			stake := float64(n.Stake)
			if int(n.Player) == e.responder {
				stake = -stake
			}
			sums.foldValues(pub, reach, out, stake)
		}
		return
	case n.Kind == Showdown && pub.round == e.lastRound:
		if !collect {
			sums.showdownValues(pub, reach, out, float64(n.Stake))
		}
		return
	case n.Kind == Showdown || int(n.Round) > pub.round:
		if collect {
			leaf := entry{node: ni, reach: ar.scratch(size), out: ar.get(size)}
			copy(leaf.reach, reach)
			*leaves = append(*leaves, leaf)
		} else {
			copy(out, (*leaves)[0].out)
			*leaves = (*leaves)[1:]
		}
		return
	}

	na := int(n.NumActions)
	kids := t.Children[n.FirstChild : int(n.FirstChild)+na]
	mark := ar.mark()
	if collect {
		// Leaves live on the arena past this call; only solve releases.
		defer func(leavesBefore int) {
			if len(*leaves) == leavesBefore {
				ar.release(mark)
			}
		}(len(*leaves))
	} else {
		defer func() { ar.release(mark) }()
	}

	responderActs := int(n.Player) == e.responder
	pl := e.play(ni, pub, responderActs)
	probs, bucket, choice := pl.probs, pl.index[:size], pl.choice

	if responderActs {
		// The opponent's reach is the same after every action here, so the
		// children share sums.
		if !collect && pub.round == e.lastRound {
			for _, kid := range kids {
				if t.Nodes[kid].Kind == Showdown {
					sums.showdown = true
				}
			}
		}
		for a, kid := range kids {
			if collect {
				e.walk(kid, pub, reach, nil, leaves, nil, ar)
				continue
			}
			if a == 0 && pl.maximise {
				// A recorded choice starts out as action 0 (see nodePlay).
				e.walk(kid, pub, reach, out, leaves, sums, ar)
				continue
			}
			m := ar.mark()
			va := ar.scratch(size)
			e.walk(kid, pub, reach, va, leaves, sums, ar)
			switch {
			case pl.maximise && choice != nil:
				for i, v := range va {
					if v > out[i] {
						out[i] = v
						choice[i] = uint8(a)
					}
				}
			case pl.maximise:
				for i, v := range va {
					if v > out[i] {
						out[i] = v
					}
				}
			case a == 0:
				for i, v := range va {
					out[i] = probs[int(bucket[i])*na] * v
				}
			default:
				for i, v := range va {
					out[i] += probs[int(bucket[i])*na+a] * v
				}
			}
			if pl.acc != nil {
				for i, v := range va {
					pl.acc[int(bucket[i])*na+a] += pl.weight * v
				}
			}
			ar.release(m)
		}
		if pl.maximise && choice != nil && !collect {
			e.learn.record(e, ni, pub, choice)
		}
		if pl.acc != nil && !collect {
			// With the per-action values added above, this leaves each
			// (index, action) the sum over its hands of the action's value
			// less the node's: the regret increment.
			for i, v := range out {
				row := pl.acc[int(bucket[i])*na:][:na]
				for a := range row {
					row[a] -= pl.weight * v
				}
			}
		}
		return
	}

	wrote := false // out holds the sum over the actions so far
	for a, kid := range kids {
		m := ar.mark()
		ra := ar.scratch(size)
		live := false
		if probs == nil {
			// Replaying recorded choices: each hand takes its one action.
			for i, r := range reach {
				if choice[i] != uint8(a) {
					r = 0
				}
				ra[i] = r
				if r != 0 {
					live = true
				}
			}
		} else {
			for i, r := range reach {
				p := r * probs[int(bucket[i])*na+a]
				ra[i] = p
				if p != 0 {
					live = true
				}
			}
		}
		if pl.acc != nil && !collect {
			for i, p := range ra {
				pl.acc[int(bucket[i])*na+a] += pl.weight * p
			}
		}
		// An action the opponent never takes contributes nothing, in
		// either pass.
		if live && collect {
			before := len(*leaves)
			e.walk(kid, pub, ra, nil, leaves, nil, ar)
			if len(*leaves) != before {
				continue // ra sits under the new leaves on the arena
			}
		} else if live {
			kidSums := e.newReachSums(pub, ar)
			if !wrote {
				// The first action's values go straight into out.
				e.walk(kid, pub, ra, out, leaves, kidSums, ar)
				wrote = true
			} else {
				va := ar.scratch(size)
				e.walk(kid, pub, ra, va, leaves, kidSums, ar)
				for i, v := range va {
					out[i] += v
				}
			}
		}
		ar.release(m)
	}
	if !collect && !wrote {
		clear(out)
	}
}

// nodePlay is how the acting seat plays one decision node at one public
// state.
type nodePlay struct {
	// maximise: the responder takes the best action for each hand. With
	// choice set (by a learner), the action taken is written there, one
	// byte per hand, and handed to the learner's record when the node is
	// done; it arrives zeroed, and action 0 is left unwritten.
	maximise bool
	// Otherwise the seat plays a strategy: a hand whose index is x takes
	// action a with probability probs[x*na+a]. The index is the hand's
	// bucket except where a learner says otherwise. With probs nil (the
	// opponent only) each hand takes the one action recorded in choice.
	probs  []float64
	index  []int32
	choice []uint8
	// acc, when set, collects per (index, action), laid out like probs and
	// scaled by weight: at a responder's node the regret increments, at
	// the opponent's node the reach with which each action is taken.
	acc    []float64
	weight float64
}

func (e *evaluator) play(ni int32, pub *pubState, responderActs bool) nodePlay {
	if e.learn != nil {
		return e.learn.play(e, ni, pub, responderActs)
	}
	if responderActs && e.best {
		return nodePlay{maximise: true, index: pub.bucket}
	}
	n := &e.g.Tree.Nodes[ni]
	slots := int64(e.g.Tree.NumBuckets[n.Round]) * int64(n.NumActions)
	return nodePlay{probs: e.probs[n.Player][n.Offset : n.Offset+slots], index: pub.bucket}
}

// reachSums is what the terminals need from one opponent reach vector at
// a public state, computed on first use and shared by every terminal
// evaluated with that vector (a fold and the showdown after a call see the
// same reach).
//
// Terminals are where best response spends its time, so the layout here
// is for speed: everything is sequential or stays within a few kilobytes,
// and no sum is carried through memory from one hand to the next.
type reachSums struct {
	total      float64
	cardTotal  [maxDeck + 1]float64 // per list: reach of the hands holding that card; the last stays zero
	correction []float64            // see showdownValues
	prefix     []float64            // scratch
	base       []float64            // see showdownValues
	haveTotals bool
	haveShow   bool
	// showdown says a showdown will be evaluated with this reach vector,
	// so whoever comes first should do the showdown's sums, which yield
	// the totals a fold needs as a by-product.
	showdown bool
}

// newReachSums returns empty sums for one reach vector at pub, valid
// until the arena is released past this point.
func (e *evaluator) newReachSums(pub *pubState, ar *arena) *reachSums {
	if ar.sumsTop == len(ar.sums) {
		ar.sums = append(ar.sums, new(reachSums))
	}
	s := ar.sums[ar.sumsTop]
	ar.sumsTop++
	n := len(pub.hands)
	s.correction, s.prefix, s.base = ar.scratch(n), ar.scratch(n+1), ar.scratch(n)
	s.haveTotals, s.haveShow, s.showdown = false, false, false
	return s
}

// The sums below are written four at a time. A running sum is one
// floating-point add after another, each waiting for the last, and the
// compiler may not reorder them; four independent sums in one loop run
// about four times as fast. That is most of what these functions do.

// totals fills in total and cardTotal.
func (s *reachSums) totals(pub *pubState, reach []float64) {
	if s.haveTotals {
		return
	}
	if s.showdown {
		s.showdownSums(pub, reach)
		return
	}
	s.haveTotals = true
	var t0, t1, t2, t3 float64
	i := 0
	for ; i+4 <= len(reach); i += 4 {
		t0 += reach[i]
		t1 += reach[i+1]
		t2 += reach[i+2]
		t3 += reach[i+3]
	}
	for ; i < len(reach); i++ {
		t0 += reach[i]
	}
	s.total = (t0 + t1) + (t2 + t3)

	m := pub.listLen
	l := 0
	for ; l+4 <= pub.lists; l += 4 {
		rows := pub.listHands[l*m : (l+4)*m]
		a, b, c, d := rows[:m], rows[m:2*m], rows[2*m:3*m], rows[3*m:4*m]
		var s0, s1, s2, s3 float64
		for k := range a {
			s0 += reach[a[k]]
			s1 += reach[b[k]]
			s2 += reach[c[k]]
			s3 += reach[d[k]]
		}
		s.cardTotal[l], s.cardTotal[l+1], s.cardTotal[l+2], s.cardTotal[l+3] = s0, s1, s2, s3
	}
	for ; l < pub.lists; l++ {
		var sum float64
		for _, h := range pub.listHands[l*m : (l+1)*m] {
			sum += reach[h]
		}
		s.cardTotal[l] = sum
	}
	s.cardTotal[pub.lists] = 0
}

// foldValues: out[i] = stake * (opponent reach over hands that share no
// card with hand i). Inclusion-exclusion: everything, less the holders of
// each of i's cards; with two hole cards that removes hand i itself twice,
// so it goes back once.
func (s *reachSums) foldValues(pub *pubState, reach, out []float64, stake float64) {
	s.totals(pub, reach)
	if s.total == 0 {
		clear(out)
		return
	}
	n := len(reach)
	l0, l1 := pub.handList[0][:n], pub.handList[1][:n]
	total, self := s.total, pub.self
	for i, r := range reach {
		out[i] = stake * (total + self*r - s.cardTotal[l0[i]] - s.cardTotal[l1[i]])
	}
}

// showdownValues: out[i] = stake * (compatible reach hand i beats -
// compatible reach that beats hand i). Last round only.
//
// Writing W for reach over weaker hands, T for weaker-or-tied and A for
// all, each restricted to hands sharing no card with i, the value is
// W + T - A. By inclusion-exclusion over i's cards each of the three is
// its count over all hands, less the same count within each card's list
// (hand i's own reach cancels between T and A). The counts over all hands
// depend only on i's tie group; the counts within a list come from the
// list's own running sum, at the positions deal recorded.
func (s *reachSums) showdownValues(pub *pubState, reach, out []float64, stake float64) {
	s.showdownSums(pub, reach)
	if s.total == 0 {
		clear(out)
		return
	}
	n := len(reach)
	group, correction, base := pub.group[:n], s.correction[:n], s.base
	for i := range out {
		out[i] = stake * (base[group[i]] - correction[i])
	}
}

// showdownSums fills in correction and base, and with them total and
// cardTotal.
func (s *reachSums) showdownSums(pub *pubState, reach []float64) {
	if s.haveShow {
		return
	}
	s.haveShow, s.haveTotals = true, true
	s.corrections(pub, reach)
	s.groupBases(pub, reach)
}

// corrections sets correction[i] to the sum, over hand i's cards, of
// (weaker + weakerOrTied - all) within the card's list, and cardTotal.
func (s *reachSums) corrections(pub *pubState, reach []float64) {
	correction := s.correction[:len(reach)]
	clear(correction)
	m := pub.listLen
	s.cardTotal[pub.lists] = 0
	// run[k] = reach of a list's first k hands, for four lists at a time.
	var run0, run1, run2, run3 [256]float64
	scatter := func(l int, run *[256]float64) {
		hands := pub.listHands[l*m : (l+1)*m]
		below, upto := pub.listBelow[l*m:(l+1)*m], pub.listUpto[l*m:(l+1)*m]
		all := run[m]
		s.cardTotal[l] = all
		for k, h := range hands {
			correction[h] += run[below[k]] + run[upto[k]] - all
		}
	}
	l := 0
	for ; l+4 <= pub.lists; l += 4 {
		rows := pub.listHands[l*m : (l+4)*m]
		a, b, c, d := rows[:m], rows[m:2*m], rows[2*m:3*m], rows[3*m:4*m]
		var s0, s1, s2, s3 float64
		for k := range a {
			s0 += reach[a[k]]
			s1 += reach[b[k]]
			s2 += reach[c[k]]
			s3 += reach[d[k]]
			run0[k+1], run1[k+1], run2[k+1], run3[k+1] = s0, s1, s2, s3
		}
		scatter(l, &run0)
		scatter(l+1, &run1)
		scatter(l+2, &run2)
		scatter(l+3, &run3)
	}
	for ; l < pub.lists; l++ {
		var sum float64
		for k, h := range pub.listHands[l*m : (l+1)*m] {
			sum += reach[h]
			run0[k+1] = sum
		}
		scatter(l, &run0)
	}
}

// groupBases sets base[g] to (weaker + weakerOrTied - all) over all hands
// for tie group g: the running sum of reach at the group's two ends, less
// the total. It also sets total.
func (s *reachSums) groupBases(pub *pubState, reach []float64) {
	n := len(reach)
	// ahead[i] = reach of the hands before i. Built as four running sums,
	// one per quarter of the order, then shifted into one.
	ahead := s.prefix[:n+1]
	quarter := n / 4
	var s0, s1, s2, s3 float64
	a, b, c, d := reach[:quarter], reach[quarter:2*quarter], reach[2*quarter:3*quarter], reach[3*quarter:]
	for k := range a {
		ahead[k], ahead[quarter+k], ahead[2*quarter+k], ahead[3*quarter+k] = s0, s1, s2, s3
		s0 += a[k]
		s1 += b[k]
		s2 += c[k]
		s3 += d[k]
	}
	for k := quarter; k < len(d); k++ {
		ahead[3*quarter+k] = s3
		s3 += d[k]
	}
	shift := s0
	for i := quarter; i < 2*quarter; i++ {
		ahead[i] += shift
	}
	shift += s1
	for i := 2 * quarter; i < 3*quarter; i++ {
		ahead[i] += shift
	}
	shift += s2
	for i := 3 * quarter; i < n; i++ {
		ahead[i] += shift
	}
	all := shift + s3
	ahead[n] = all
	s.total = all
	starts := pub.groupStart
	base := s.base[:len(starts)-1]
	for g := range base {
		base[g] = ahead[starts[g]] + ahead[starts[g+1]] - all
	}
}

// outcome is one way the next round's board cards can fall. perms, when
// set, lists hand relabellings (global hand index to global hand index):
// the outcome stands for len(perms) suit-isomorphic boards, and hand h on
// this board is hand perms[i][h] on the i-th of them.
type outcome struct {
	cards [3]Card // the first count of them
	count int
	perms [][]int32
}

// outcomes lists the ways the next round's cards can fall after pub, one
// per class under pub's surviving suit relabellings (the member with the
// smallest combIndex). Folding a class into one board is sound because
// reach is unchanged by those relabellings: it depends on a hand only
// through suit-invariant buckets on boards the relabelling fixes. The
// list is the arena's, valid until the next call for the same round.
func (e *evaluator) outcomes(pub *pubState, ar *arena) []outcome {
	r := e.g.Rules
	out, perms := ar.outcomes[pub.round][:0], ar.perms[pub.round][:0]
	var images [24]int // at most one per suit relabelling
	forEachCombo(r.Deck, r.Board[pub.round+1], cardMask(pub.board), func(cs []Card) {
		var o outcome
		o.count = copy(o.cards[:], cs)
		if len(pub.stab) > 1 {
			self, seen, first := combIndex(cs), 0, len(perms)
			for _, p := range pub.stab {
				img := permuteIndex(cs, &suitPerms[p])
				if img < self {
					perms = perms[:first]
					return // not the representative of its class
				}
				if !slices.Contains(images[:seen], img) {
					images[seen] = img
					seen++
					perms = append(perms, r.handPerms()[p])
				}
			}
			// Three-index slice: a later append must not grow into the
			// next outcome's perms.
			o.perms = perms[first:len(perms):len(perms)]
		}
		out = append(out, o)
	})
	ar.outcomes[pub.round], ar.perms[pub.round] = out, perms
	return out
}

// chance deals the next round's board cards every possible way and sets
// each leaf's out to the average of its values over them.
func (e *evaluator) chance(pub *pubState, leaves []entry, ar *arena) {
	r := e.g.Rules
	// Given both players' hole cards, every board is equally likely.
	norm := 1 / float64(binom[r.Deck-2*r.Hole-len(pub.board)][r.Board[pub.round+1]])
	var outcomes []outcome
	if e.learn != nil && pub.round == 0 {
		// A learner samples the first deal: one board, drawn uniformly
		// from all of them, stands for the lot.
		outcomes = e.learn.sampled[:]
		norm *= float64(binom[r.Deck][r.Board[1]])
	} else {
		outcomes = e.outcomes(pub, ar)
	}

	if pub.round > 0 || e.workers == 1 {
		sums := make([][]float64, len(leaves))
		for li, leaf := range leaves {
			sums[li] = leaf.out
		}
		for oi, o := range outcomes {
			if e.learn != nil {
				e.learn.enter(pub.round+1, oi, norm*float64(max(1, len(o.perms))))
			}
			e.chanceOutcome(pub, leaves, o, sums, norm, ar)
		}
		return
	}
	// The first deal fans out over the workers; everything below it is
	// serial inside one worker. Worker w takes outcomes w, w+workers, ...
	// and the partial sums are combined in worker order, so the result
	// does not depend on scheduling.
	partial := make([][][]float64, e.workers)
	var wg sync.WaitGroup
	for w := range partial {
		wg.Add(1)
		go func() {
			defer wg.Done()
			war := e.newArena()
			sums := make([][]float64, len(leaves))
			for li := range sums {
				sums[li] = make([]float64, len(pub.hands))
			}
			for i := w; i < len(outcomes); i += e.workers {
				e.chanceOutcome(pub, leaves, outcomes[i], sums, norm, war)
			}
			partial[w] = sums
		}()
	}
	wg.Wait()
	for _, sums := range partial {
		for li, leaf := range leaves {
			for i, v := range sums[li] {
				leaf.out[i] += v
			}
		}
	}
}

// chanceOutcome solves every leaf on one outcome of the deal and adds the
// values, scaled by norm, into sums (one vector per leaf, in pub's hand
// order).
func (e *evaluator) chanceOutcome(pub *pubState, leaves []entry, o outcome, sums [][]float64, norm float64, ar *arena) {
	mark := ar.mark()
	start := time.Now()
	child := ar.pub[pub.round+1]
	e.deal(child, pub, o.cards[:o.count])
	size := len(child.hands)
	gather := func(leaf entry, reach []float64) {
		for i, j := range child.parent {
			reach[i] = leaf.reach[j]
		}
	}
	// Where each hand's value goes in pub's order: its own place there,
	// or, for an outcome standing for a class of boards, its place on each
	// of them (hand h here is hand perm[h] on that board).
	targets := ar.targets[child.round][:0]
	if o.perms == nil {
		targets = append(targets, child.parent...)
	} else {
		for _, perm := range o.perms {
			for _, h := range child.hands {
				targets = append(targets, pub.local[perm[h]])
			}
		}
	}
	ar.targets[child.round] = targets
	scatter := func(out, sum []float64) {
		for at := 0; at < len(targets); at += size {
			target := targets[at : at+size]
			for i, v := range out {
				sum[target[i]] += norm * v
			}
		}
	}
	var solved time.Duration
	if child.round == e.lastRound {
		// Nothing is dealt after the last round, so the leaves need not be
		// solved together: take them one at a time through the same two
		// vectors, which then stay in the nearest cache.
		reach, out := ar.scratch(size), ar.scratch(size)
		var none []entry
		for li, leaf := range leaves {
			gather(leaf, reach)
			walkStart := time.Now()
			m := ar.mark()
			e.walk(leaf.node, child, reach, out, &none, e.newReachSums(child, ar), ar)
			ar.release(m)
			solved += time.Since(walkStart)
			scatter(out, sums[li])
		}
		e.bettingNanos[child.round].Add(int64(solved))
		e.boards[child.round].Add(1)
	} else {
		entries := ar.entries[child.round][:0]
		for _, leaf := range leaves {
			reach := ar.scratch(size)
			gather(leaf, reach)
			entries = append(entries, entry{node: leaf.node, reach: reach, out: ar.scratch(size)})
		}
		ar.entries[child.round] = entries
		solveStart := time.Now()
		e.solve(child, entries, ar)
		solved = time.Since(solveStart)
		for li, en := range entries {
			scatter(en.out, sums[li])
		}
	}
	e.dealNanos[child.round].Add(int64(time.Since(start) - solved))
	ar.release(mark)
}

var suitPerms = func() [][numSuits]Card {
	var out [][numSuits]Card
	for a := 0; a < 4; a++ {
		for b := 0; b < 4; b++ {
			for c := 0; c < 4; c++ {
				for d := 0; d < 4; d++ {
					if a != b && a != c && a != d && b != c && b != d && c != d {
						out = append(out, [numSuits]Card{Card(a), Card(b), Card(c), Card(d)})
					}
				}
			}
		}
	}
	return out
}()

func permuteCard(c Card, perm *[numSuits]Card) Card { return c&^3 | perm[c&3] }

// permuteIndex returns combIndex of cards with their suits relabelled.
func permuteIndex(cards []Card, perm *[numSuits]Card) int {
	var buf [5]Card
	p := buf[:len(cards)]
	for i, c := range cards {
		p[i] = permuteCard(c, perm)
	}
	sortCards(p)
	return combIndex(p)
}

// handPerms returns, for each suit relabelling, where it sends every hand
// (global hand index to global hand index).
func (r *Rules) handPerms() [][]int32 {
	r.permsOnce.Do(func() {
		hands := r.Hands()
		r.perms = make([][]int32, len(suitPerms))
		for p := range suitPerms {
			r.perms[p] = make([]int32, len(hands))
			for h, hand := range hands {
				r.perms[p][h] = int32(permuteIndex(hand, &suitPerms[p]))
			}
		}
	})
	return r.perms
}
