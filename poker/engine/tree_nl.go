package engine

import (
	_ "embed"
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
	data, err := os.ReadFile(path)
	if err != nil {
		return NLConfig{}, err
	}
	return parseNLConfig(data, path)
}

func parseNLConfig(data []byte, name string) (NLConfig, error) {
	var cfg NLConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", name, err)
	}
	if cfg.SB <= 0 || cfg.BB < cfg.SB || cfg.StackBB <= 0 {
		return cfg, fmt.Errorf("%s: need 0 < sb <= bb and stack_bb > 0", name)
	}
	return cfg, nil
}

// tableConfigJSON is the betting config the league's table serves
// (deploy/cloudrun/Dockerfile copies this same file into the image).
//
//go:embed configs/table_50bb.json
var tableConfigJSON []byte

// TableConfig is the league table's betting config, configs/table_50bb.json:
// blinds 50/100, 50 bb stacks, and the bet menu the house bots train on.
// Code that plays the league's game (the SDK) takes the rules from here
// rather than restating them.
func TableConfig() NLConfig {
	cfg, err := parseNLConfig(tableConfigJSON, "configs/table_50bb.json")
	if err != nil {
		panic(err) // embedded at build time; TestTableConfig reads it
	}
	return cfg
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

// wagerRange is the extra amounts (chips beyond the call) the next wager
// may be: from the minimum raise up to all-in. The minimum is a big blind
// for a round's first wager (the blinds do not count as one) and the
// last wager's size after that. ok is false when the seat cannot wager:
// it is facing an all-in, or calling would put it all-in. A wager below
// minExtra is legal only as an all-in (minExtra may exceed maxExtra).
func (c *NLConfig) wagerRange(st *nlState) (minExtra, maxExtra int, ok bool) {
	maxExtra = st.behind[st.toAct] - st.call
	if st.allIn || maxExtra <= 0 {
		return 0, 0, false
	}
	minExtra = c.BB
	if st.level > 0 {
		minExtra = st.lastInc
	}
	return minExtra, maxExtra, true
}

// wagerSizes lists the menu's extra amounts (beyond the call) for the
// next wager, ascending and deduplicated: each menu fraction of the pot
// after calling, clipped up to the minimum raise and down to all-in, plus
// all-in itself.
func (c *NLConfig) wagerSizes(st *nlState) []int {
	minExtra, maxExtra, ok := c.wagerRange(st)
	if !ok {
		return nil
	}
	seen := map[int]bool{maxExtra: true}
	out := []int{maxExtra}
	for _, frac := range c.menu(st.level + 1) {
		want := math.Min(math.Max(frac*float64(st.pot+st.call), float64(minExtra)), float64(maxExtra))
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

// nlState is the public betting state of a hand in play: a decision node
// of the tree, or a hand at a table (TableHand), which bets any size.
type nlState struct {
	round   int
	toAct   int
	pot     int
	call    int    // chips toAct must add to match
	level   int    // wagers made so far this round
	lastInc int    // size of the last wager's raise, the next minimum
	checked bool   // the other player already acted this round, so a check closes it
	allIn   bool   // the bet being faced put its maker all-in
	opening bool   // the small blind's first decision of the hand
	behind  [2]int // chips each player still has
}

// newNLState is the state at the first decision of a hand: blinds posted,
// the small blind (player 0) to act.
func (c *NLConfig) newNLState() nlState {
	stack := c.StackBB * c.BB
	st := nlState{toAct: 0, pot: c.SB + c.BB, call: c.BB - c.SB,
		behind: [2]int{stack - c.SB, stack - c.BB}}
	if st.call > 0 {
		st.opening = true
	}
	return st
}

// canFold and canCall say which of fold and check-or-call the rules allow
// the seat to act (the config may forbid the small blind's opening fold
// or limp). A check is a call of nothing.
func (c *NLConfig) canFold(st *nlState) bool {
	return st.call > 0 && !(st.opening && !c.AllowSBFold)
}

func (c *NLConfig) canCall(st *nlState) bool {
	return st.call == 0 || !st.opening || c.AllowSBLimp
}

// menuActions is the tree's actions at a decision: fold and check or
// call as the rules allow, then the menu's wagers, ascending.
func (c *NLConfig) menuActions(st *nlState) []Action {
	me := st.toAct
	var acts []Action
	if c.canFold(st) {
		acts = append(acts, Action{Kind: ActFold})
	}
	if c.canCall(st) {
		if st.call > 0 {
			acts = append(acts, Action{Kind: ActCall, Amount: int32(st.call), AllIn: st.call == st.behind[me]})
		} else {
			acts = append(acts, Action{Kind: ActCheck})
		}
	}
	for _, amt := range c.wagerSizes(st) {
		acts = append(acts, Action{Kind: ActBet, Amount: int32(amt), AllIn: st.call+amt == st.behind[me]})
	}
	return acts
}

// nlEnd is how an action leaves a hand.
type nlEnd uint8

const (
	nlGoesOn   nlEnd = iota // a decision follows, in this round or the next
	nlFolded                // the player who acted folded
	nlShowdown              // both are in for the same chips, the cards decide
)

// step applies act, which the rules must allow, for the player to act:
// the betting state after it and whether the hand goes on. An ActBet may
// be any size; legality (wagerRange) is the caller's. A fold leaves the
// state as it was.
func (c *NLConfig) step(st nlState, act Action) (nlState, nlEnd) {
	me, opp := st.toAct, 1-st.toAct
	switch act.Kind {
	case ActFold:
		return st, nlFolded
	case ActCheck:
		// Nothing to call means no wager yet this round, so the second
		// check (or the big blind checking its option after a limp)
		// closes it.
		if st.checked {
			return c.closeRound(st)
		}
		st.toAct, st.checked = opp, true
		return st, nlGoesOn
	case ActCall:
		allIn := st.allIn
		st.behind[me] -= st.call
		st.pot += st.call
		st.call, st.allIn = 0, false
		switch {
		case st.opening:
			// A limp does not close the round: the big blind still has
			// its option.
			st.toAct, st.opening, st.checked = opp, false, true
			return st, nlGoesOn
		case allIn:
			return st, nlShowdown
		}
		return c.closeRound(st)
	}
	amt := int(act.Amount)
	st.behind[me] -= st.call + amt
	st.pot += st.call + amt
	st.call, st.lastInc, st.level = amt, amt, st.level+1
	st.allIn = st.behind[me] == 0
	st.toAct, st.opening, st.checked = opp, false, false
	return st, nlGoesOn
}

// closeRound ends a betting round with both players still in and chips
// behind: the next round starts with the big blind, or the hand is shown
// down after the river.
func (c *NLConfig) closeRound(st nlState) (nlState, nlEnd) {
	if st.round == 3 {
		return st, nlShowdown
	}
	return nlState{round: st.round + 1, toAct: 1, pot: st.pot, behind: st.behind}, nlGoesOn
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
	b.decision(cfg.newNLState())
	return b.tree
}

func (b *nlBuilder) stake(chips int) float32 {
	return float32(float64(chips) / float64(b.cfg.BB))
}

func (c *NLConfig) contributed(st *nlState, player int) int {
	return c.StackBB*c.BB - st.behind[player]
}

func (b *nlBuilder) decision(st nlState) int32 {
	me := st.toAct
	acts := b.cfg.menuActions(&st)
	node, first := b.tree.addDecision(me, st.round, acts)
	for i, act := range acts {
		next, end := b.cfg.step(st, act)
		var child int32
		switch end {
		case nlFolded:
			child = b.tree.add(Node{Kind: Fold, Player: uint8(me), Round: uint8(st.round),
				Stake: b.stake(b.cfg.contributed(&next, me))})
		case nlShowdown:
			child = b.tree.add(Node{Kind: Showdown, Round: uint8(st.round),
				Stake: b.stake(b.cfg.contributed(&next, 0))})
		default:
			child = b.decision(next)
		}
		b.tree.Children[int(first)+i] = child
	}
	return node
}
