package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// No-limit betting tree. This is poker/treesize's tree model (same rules,
// same config file, same rounding) built as explicit nodes instead of
// counted; TestNLTreeMatchesTreesize holds the two together. See
// poker/treesize/README.md "Rules implemented" for the full statement.

// NLConfig is the treesize config file. The buckets_* fields in those
// files are sizing inputs for treesize only; the engine takes bucket
// counts from the card abstraction, so they are accepted and ignored.
type NLConfig struct {
	SB           int       `json:"sb"`
	BB           int       `json:"bb"`
	StackBB      int       `json:"stack_bb"`
	Opening      []float64 `json:"opening"`       // pot fractions for the 1st wager of a round
	RaiseMenu    []float64 `json:"raise_menu"`    // 2nd wager
	ThreebetMenu []float64 `json:"threebet_menu"` // 3rd wager
	FourbetMenu  []float64 `json:"fourbet_menu"`  // 4th wager and later
	AllowSBFold  bool      `json:"allow_sb_fold"`
	AllowSBLimp  bool      `json:"allow_sb_limp"`

	BucketsPreflop int `json:"buckets_preflop,omitempty"`
	BucketsFlop    int `json:"buckets_flop,omitempty"`
	BucketsTurn    int `json:"buckets_turn,omitempty"`
	BucketsRiver   int `json:"buckets_river,omitempty"`
}

func LoadNLConfig(path string) (NLConfig, error) {
	var cfg NLConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.SB <= 0 || cfg.BB < cfg.SB || cfg.StackBB <= 0 {
		return cfg, fmt.Errorf("%s: need 0 < sb <= bb and stack_bb > 0", path)
	}
	return cfg, nil
}

func (c *NLConfig) menu(wager int) []float64 {
	switch wager {
	case 1:
		return c.Opening
	case 2:
		return c.RaiseMenu
	case 3:
		return c.ThreebetMenu
	}
	return c.FourbetMenu
}

// wagerSizes lists the legal extra amounts (beyond the call) for the next
// wager, ascending and deduplicated: each menu fraction of the pot after
// calling, clipped up to the minimum raise and down to all-in, plus all-in
// itself.
func (c *NLConfig) wagerSizes(pot, call, level, lastInc, behind int) []int {
	maxExtra := behind - call
	if maxExtra <= 0 {
		return nil
	}
	minExtra := c.BB
	if level > 0 {
		minExtra = lastInc
	}
	seen := map[int]bool{maxExtra: true}
	out := []int{maxExtra}
	for _, frac := range c.menu(level + 1) {
		want := math.Min(math.Max(frac*float64(pot+call), float64(minExtra)), float64(maxExtra))
		amt := int(math.RoundToEven(want)) // Python's round(), which treesize uses
		amt = max(1, min(amt, maxExtra))
		if !seen[amt] {
			seen[amt] = true
			out = append(out, amt)
		}
	}
	// Ascending; the lists are a handful long.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// nlState is the public betting state at a decision node.
type nlState struct {
	round   int
	toAct   int
	pot     int
	call    int    // chips toAct must add to match
	level   int    // wagers made so far this round
	lastInc int    // size of the last wager's raise, the next minimum
	checked bool   // the other player already checked this round
	allIn   bool   // the bet being faced put its maker all-in
	opening bool   // the small blind's first decision of the hand
	behind  [2]int // chips each player still has
}

type nlBuilder struct {
	cfg  NLConfig
	tree *Tree
}

// BuildNLTree builds the full heads-up no-limit tree for cfg. Player 0 is
// the small blind (acts first preflop, last afterwards). Stakes are in big
// blinds.
func BuildNLTree(cfg NLConfig) *Tree {
	b := &nlBuilder{cfg: cfg, tree: &Tree{Rounds: 4, Posted: [2]int32{int32(cfg.SB), int32(cfg.BB)}}}
	stack := cfg.StackBB * cfg.BB
	st := nlState{toAct: 0, pot: cfg.SB + cfg.BB, call: cfg.BB - cfg.SB,
		behind: [2]int{stack - cfg.SB, stack - cfg.BB}}
	if st.call > 0 {
		st.opening = true
	}
	b.decision(st)
	return b.tree
}

func (b *nlBuilder) stake(chips int) float32 {
	return float32(float64(chips) / float64(b.cfg.BB))
}

func (b *nlBuilder) contributed(st *nlState, player int) int {
	return b.cfg.StackBB*b.cfg.BB - st.behind[player]
}

func (b *nlBuilder) decision(st nlState) int32 {
	me, opp := st.toAct, 1-st.toAct
	facing := st.call > 0
	canFold := facing && !(st.opening && !b.cfg.AllowSBFold)
	canCall := !facing || !st.opening || b.cfg.AllowSBLimp
	var sizes []int
	if !st.allIn {
		sizes = b.cfg.wagerSizes(st.pot, st.call, st.level, st.lastInc, st.behind[me])
	}

	var acts []Action
	if canFold {
		acts = append(acts, Action{Kind: ActFold})
	}
	if canCall {
		if facing {
			acts = append(acts, Action{Kind: ActCall, Amount: int32(st.call), AllIn: st.call == st.behind[me]})
		} else {
			acts = append(acts, Action{Kind: ActCheck})
		}
	}
	for _, amt := range sizes {
		acts = append(acts, Action{Kind: ActBet, Amount: int32(amt), AllIn: st.call+amt == st.behind[me]})
	}
	node, first := b.tree.addDecision(me, st.round, acts)

	for i, act := range acts {
		var child int32
		switch act.Kind {
		case ActFold:
			child = b.tree.add(Node{Kind: Fold, Player: uint8(me), Round: uint8(st.round),
				Stake: b.stake(b.contributed(&st, me))})
		case ActCheck:
			// Nothing to call means no wager yet this round, so the second
			// check (or the big blind checking its option after a limp)
			// closes it.
			if st.checked {
				child = b.closeRound(st)
			} else {
				next := st
				next.toAct, next.checked = opp, true
				child = b.decision(next)
			}
		case ActCall:
			next := st
			next.behind[me] -= st.call
			next.pot += st.call
			next.call, next.allIn = 0, false
			switch {
			case st.opening:
				// A limp does not close the round: the big blind still has
				// its option.
				next.toAct, next.opening, next.checked = opp, false, true
				child = b.decision(next)
			case st.allIn:
				child = b.tree.add(Node{Kind: Showdown, Round: uint8(st.round),
					Stake: b.stake(b.contributed(&next, me))})
			default:
				child = b.closeRound(next)
			}
		case ActBet:
			next := st
			amt := int(act.Amount)
			next.behind[me] -= st.call + amt
			next.pot += st.call + amt
			next.call, next.lastInc, next.level = amt, amt, st.level+1
			next.allIn = next.behind[me] == 0
			next.toAct, next.opening, next.checked = opp, false, false
			child = b.decision(next)
		}
		b.tree.Children[int(first)+i] = child
	}
	return node
}

// closeRound ends a betting round with both players still in and chips
// behind: the next round starts with the big blind, or the hand is shown
// down after the river.
func (b *nlBuilder) closeRound(st nlState) int32 {
	if st.round == b.tree.Rounds-1 {
		return b.tree.add(Node{Kind: Showdown, Round: uint8(st.round),
			Stake: b.stake(b.contributed(&st, 0))})
	}
	next := nlState{round: st.round + 1, toAct: 1, pot: st.pot, behind: st.behind}
	return b.decision(next)
}
