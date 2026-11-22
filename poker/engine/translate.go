package engine

import (
	"fmt"
	"math"
)

// Action translation: a house bot knows only its tree's bet menu, and a
// person at the table bets any size. A TreeView follows a real hand
// (TableHand) on the tree. Each action of the other seat is read as one
// of the tree's actions at the view's node: fold, check and call as
// themselves, all-in as the tree's all-in, and any other bet or raise as
// one of the two menu sizes around it, picked at random by the
// pseudo-harmonic mapping (pseudoHarmonic). The bot's own choice is
// played back at the table at the same fraction of the real pot, clamped
// to the legal range; its all-in is the exact all-in.
//
// The two seats always make the same kinds of moves in the same order,
// so the view's node has the hand's seat to act and round, with one
// exception: a bet read as an all-in that was not one. When the tree's
// line then ends (a call of its all-in is its showdown) while the hand
// goes on, the view is off the tree and the bot checks or calls to the
// end, as the tree's line has it committed.

// TreeView is one hand of a bot's view of the table: where the hand is on
// the bot's tree.
type TreeView struct {
	g    *Game
	node int32
	st   nlState // the tree's betting state at node, for its pot
	seen int     // the hand's actions followed so far
	off  bool    // the tree's line has ended and the hand has not
}

// NewTreeView starts a view at the root, for a hand just dealt.
func NewTreeView(g *Game) *TreeView { return &TreeView{g: g, st: g.Config.newNLState()} }

// Node is the tree decision the view is at; nil once it is off the tree.
func (v *TreeView) Node() *Node {
	if v.off {
		return nil
	}
	return &v.g.Tree.Nodes[v.node]
}

// OffTree reports whether the tree's line has ended while the hand goes on.
func (v *TreeView) OffTree() bool { return v.off }

func (v *TreeView) actions() []Action {
	n := v.Node()
	return v.g.Tree.Actions[n.FirstChild : n.FirstChild+int32(n.NumActions)]
}

// advance moves the view along its tree's action a.
func (v *TreeView) advance(a int) {
	n := v.Node()
	act := v.actions()[a]
	v.st, _ = v.g.Config.step(v.st, act)
	v.node = v.g.Tree.Children[int(n.FirstChild)+a]
	if v.g.Tree.Nodes[v.node].Kind != Decision {
		v.off = true // nothing follows on the tree; if the hand is over, nothing asks
	}
}

// sync checks the view against the hand: a decision on the tree must be
// the same seat's in the same round. A view that is not goes off the tree
// rather than answer for the wrong spot (translation keeps them in step,
// so this is a guard; the tests count it).
func (v *TreeView) sync(h *TableHand) {
	if n := v.Node(); n != nil && !h.Over() && (int(n.Player) != h.ToAct() || int(n.Round) != h.Round()) {
		v.off = true
	}
}

// Follow reads the hand's actions since the last call onto the tree. r
// gives the uniform random bits a translation draws.
func (v *TreeView) Follow(h *TableHand, r func() uint32) {
	for ; v.seen < len(h.Log); v.seen++ {
		if v.off {
			continue
		}
		a, ok := v.translate(h.Log[v.seen].TableOption, r)
		if !ok {
			v.off = true
			continue
		}
		v.advance(a)
	}
	v.sync(h)
}

// translate reads a real action as one of the tree's at the view's node.
// ok is false when the tree has no action of its kind (see sync).
func (v *TreeView) translate(o TableOption, r func() uint32) (a int, ok bool) {
	acts := v.actions()
	if o.Kind != ActBet {
		for i, act := range acts {
			if act.Kind == o.Kind {
				return i, true
			}
		}
		return 0, false
	}
	// The tree's wagers, ascending (menuActions' order), as pot fractions
	// on the tree's own pot: the unit the real bet is measured in.
	var bets []int
	var fracs []float64
	for i, act := range acts {
		if act.Kind == ActBet {
			bets = append(bets, i)
			fracs = append(fracs, float64(act.Amount)/float64(v.st.pot+v.st.call))
		}
	}
	switch {
	case len(bets) == 0:
		return 0, false
	case o.AllIn: // all-in is all-in, whatever it comes to in chips
		return bets[len(bets)-1], true
	}
	// A menu size is itself (the same arithmetic on the same chips gives
	// the same fraction); under the menu is its smallest size, over it its
	// largest; between two sizes, a draw.
	x := o.PotFraction
	if x <= fracs[0] {
		return bets[0], true
	}
	for i := 1; i < len(fracs); i++ {
		switch {
		case x == fracs[i]:
			return bets[i], true
		case x < fracs[i]:
			if float64(r())/(1<<32) < pseudoHarmonic(fracs[i-1], fracs[i], x) {
				return bets[i-1], true
			}
			return bets[i], true
		}
	}
	return bets[len(bets)-1], true
}

// pseudoHarmonic is the probability that a bet of x is read as the
// smaller of the two menu sizes around it, A <= x <= B, all three as
// fractions of the pot: the pseudo-harmonic mapping of Ganzfried and
// Sandholm, "Action Translation in Extensive-Form Games with Large Action
// Spaces: Axioms, Paradoxes, and the Pseudo-Harmonic Mapping" (IJCAI
// 2013), eq. for f_{A,B}:
//
//	f(x) = (B - x)(1 + A) / ((B - A)(1 + x))
//
// It is 1 at A and 0 at B, falls monotonically between, and is the
// mapping that makes an opponent indifferent between the sizes in the
// paper's simplified game, which is what limits how much an off-menu
// size can gain.
func pseudoHarmonic(A, B, x float64) float64 {
	if B <= A {
		return 1
	}
	return (B - x) * (1 + A) / ((B - A) * (1 + x))
}

// Play plays the tree's action a at the view's node as a real move for
// the seat to act, which must be the view's (call Follow first). A bet is
// the same fraction of the hand's pot after calling as the tree's bet is
// of the tree's, clamped to the legal range; the tree's all-in is the
// hand's all-in.
func (v *TreeView) Play(h *TableHand, a int) error {
	n := v.Node()
	switch {
	case n == nil:
		return fmt.Errorf("engine: the view is off the tree")
	case v.seen != len(h.Log):
		return fmt.Errorf("engine: the view has followed %d of the hand's %d actions", v.seen, len(h.Log))
	case a < 0 || a >= int(n.NumActions):
		return fmt.Errorf("engine: action %d of %d", a, n.NumActions)
	}
	act := v.actions()[a]
	kind, to := act.Kind, 0
	if kind == ActBet {
		l := h.Legal()
		if !l.Wager {
			// The tree bets where the hand cannot (it faces an all-in the
			// tree does not see): the aggressive line calls.
			return v.passive(h)
		}
		to = l.MaxTo
		if !act.AllIn {
			frac := float64(act.Amount) / float64(v.st.pot+v.st.call)
			extra := int(math.Round(frac * float64(h.st.pot+h.st.call)))
			to = min(max(h.InRound(h.st.toAct)+h.st.call+extra, l.MinTo), l.MaxTo)
		}
	}
	if err := h.Play(kind, to); err != nil {
		return err
	}
	v.seen++
	v.advance(a)
	v.sync(h)
	return nil
}

// passive checks, or calls, or (where the rules allow neither) folds:
// the bot's move once its view is off the tree.
func (v *TreeView) passive(h *TableHand) error {
	l := h.Legal()
	kind := ActFold
	switch {
	case l.Check:
		kind = ActCheck
	case l.Call > 0:
		kind = ActCall
	}
	if err := h.Play(kind, 0); err != nil {
		return err
	}
	v.Follow(h, nil) // a check or call is never a random read
	return nil
}

// Act makes the bot's move for the seat to act: follow the hand, ask the
// blueprint at the view's node and the seat's bucket, play its answer.
// r gives uniform random bits, for the translation and the blueprint.
func (v *TreeView) Act(h *TableHand, bp *Blueprint, r func() uint32) error {
	if h.Over() {
		return fmt.Errorf("engine: the hand is over")
	}
	v.Follow(h, r)
	n := v.Node()
	if n == nil {
		return v.passive(h)
	}
	return v.Play(h, bp.Action(n, h.Bucket(h.ToAct()), r()))
}
