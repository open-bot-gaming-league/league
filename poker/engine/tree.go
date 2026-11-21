package engine

import (
	"fmt"
	"hash/fnv"
)

// The betting tree: every node of the public action sequence, in one flat
// slice. Chance is not in the tree; a node's Round says how much of the
// board the players have seen when they act there.

type NodeKind uint8

const (
	Decision NodeKind = iota
	Fold              // Player folded; the other player wins Stake
	Showdown          // both players are in for Stake each; the board is run out if needed
)

// MaxActions bounds the actions at one decision node (fold, call, the bet
// menu, all-in). The solver keeps per-node scratch on the stack at this
// size.
const MaxActions = 16

type Node struct {
	Kind       NodeKind
	Player     uint8   // Decision: who acts. Fold: who folded.
	Round      uint8   // betting round of the node
	NumActions uint8   // Decision only
	FirstChild int32   // Decision only: Tree.Children[FirstChild+a] is the child for action a
	Offset     int64   // Decision only: slot of (bucket 0, action 0); see Slot
	Stake      float32 // terminals: what the winner takes from the loser, in big blinds
}

// Slot is the dense index of (node, bucket, action 0) in every per-action
// array (regrets, strategy sums, blueprint bytes). No maps anywhere: the
// tree assigns each decision node a contiguous block of
// NumActions * buckets(round) slots.
func (n *Node) Slot(bucket int) int64 {
	return n.Offset + int64(bucket)*int64(n.NumActions)
}

type ActionKind uint8

const (
	ActFold ActionKind = iota
	ActCheck
	ActCall
	ActBet // a bet or raise; Amount is the chips added on top of any call
)

// Action labels an edge so a served blueprint can be turned back into
// poker moves.
type Action struct {
	Kind   ActionKind
	Amount int32 // ActBet: chips put in beyond the call. ActCall: chips called.
	AllIn  bool
}

type Tree struct {
	Nodes    []Node
	Children []int32
	Actions  []Action // parallel to Children
	Rounds   int
	// Posted is what each seat has put in before the first decision
	// (blinds or antes), in the chips Action.Amount counts.
	Posted [2]int32

	// Set by Index.
	NumBuckets []int
	Slots      int64 // total (infoset, action) pairs
	Infosets   int64
}

// RoundStats counts one round the way poker/treesize does, so the two can
// be compared number for number.
type RoundStats struct {
	Decisions int // decision nodes
	Terminals int // folds, round-closing calls, round-closing checks
	Edges     int // sum of actions over decision nodes
}

func (t *Tree) add(n Node) int32 {
	t.Nodes = append(t.Nodes, n)
	return int32(len(t.Nodes) - 1)
}

// addDecision appends a decision node with its action labels and reserves
// its child slots; the caller fills Children[first+a].
func (t *Tree) addDecision(player, round int, acts []Action) (node, first int32) {
	if len(acts) > MaxActions {
		panic(fmt.Sprintf("engine: %d actions at one node, MaxActions is %d", len(acts), MaxActions))
	}
	first = int32(len(t.Children))
	for range acts {
		t.Children = append(t.Children, -1)
	}
	t.Actions = append(t.Actions, acts...)
	node = t.add(Node{Kind: Decision, Player: uint8(player), Round: uint8(round),
		NumActions: uint8(len(acts)), FirstChild: first})
	return node, first
}

// Index assigns every decision node its block of slots for the given
// bucket counts (one per round).
func (t *Tree) Index(numBuckets []int) {
	if len(numBuckets) != t.Rounds {
		panic("engine: bucket counts do not match the tree's rounds")
	}
	t.NumBuckets = append([]int(nil), numBuckets...)
	t.Slots, t.Infosets = 0, 0
	for i := range t.Nodes {
		n := &t.Nodes[i]
		if n.Kind != Decision {
			continue
		}
		n.Offset = t.Slots
		nb := int64(numBuckets[n.Round])
		t.Slots += nb * int64(n.NumActions)
		t.Infosets += nb
	}
}

// Depth is the largest number of decisions on any path from the root.
func (t *Tree) Depth() int {
	var walk func(ni int32) int
	walk = func(ni int32) int {
		n := &t.Nodes[ni]
		if n.Kind != Decision {
			return 0
		}
		deepest := 0
		for a := 0; a < int(n.NumActions); a++ {
			deepest = max(deepest, walk(t.Children[int(n.FirstChild)+a]))
		}
		return deepest + 1
	}
	return walk(0)
}

// Stats returns per-round counts in poker/treesize's convention.
func (t *Tree) Stats() []RoundStats {
	st := make([]RoundStats, t.Rounds)
	for i := range t.Nodes {
		n := &t.Nodes[i]
		switch n.Kind {
		case Fold:
			// Counted below, from the parent's edge.
		case Decision:
			st[n.Round].Decisions++
			st[n.Round].Edges += int(n.NumActions)
			for a := 0; a < int(n.NumActions); a++ {
				child := &t.Nodes[t.Children[int(n.FirstChild)+a]]
				// An edge ends the round when it folds, reaches a showdown,
				// or lands in a later round.
				if child.Kind != Decision || child.Round != n.Round {
					st[n.Round].Terminals++
				}
			}
		}
	}
	return st
}

// Hash fingerprints the tree's shape, labels, payoffs and slot layout, so
// a blueprint file can refuse to be read against a different tree.
func (t *Tree) Hash() uint64 {
	h := fnv.New64a()
	var b [32]byte
	put := func(v uint64, at int) {
		for i := 0; i < 8; i++ {
			b[at+i] = byte(v >> (8 * i))
		}
	}
	for i := range t.Nodes {
		n := &t.Nodes[i]
		put(uint64(n.Kind)|uint64(n.Player)<<8|uint64(n.Round)<<16|uint64(n.NumActions)<<24, 0)
		put(uint64(n.FirstChild), 8)
		put(uint64(n.Offset), 16)
		put(uint64(int64(n.Stake*1e6)), 24)
		h.Write(b[:])
	}
	for i, c := range t.Children {
		a := t.Actions[i]
		allIn := uint64(0)
		if a.AllIn {
			allIn = 1
		}
		put(uint64(c), 0)
		put(uint64(a.Kind)|allIn<<8|uint64(a.Amount)<<16, 8)
		h.Write(b[:16])
	}
	return h.Sum64()
}
