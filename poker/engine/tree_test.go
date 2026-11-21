package engine

import (
	"reflect"
	"testing"
)

func mustLoad(t *testing.T, path string) NLConfig {
	t.Helper()
	cfg, err := LoadNLConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The expected numbers come from an independent Python implementation of
// the same no-limit betting-tree model (per round: decisions, terminals,
// edges), run separately against the config noted on each case. The
// engine builds the tree that tool counts, so they must agree exactly.
// The three configs under testdata/treesize_configs are copied in verbatim
// (bot_poker_200bb, bot_poker_100bb and the published Slumbot 2013
// structure) so this test has no dependency outside this repo.
func TestNLTreeMatchesTreesize(t *testing.T) {
	pot := []float64{1}
	botPoker20 := mustLoad(t, "testdata/treesize_configs/bot_poker_200bb.json")
	botPoker20.StackBB = 20
	slumbot30 := mustLoad(t, "testdata/treesize_configs/slumbot_2013.json")
	slumbot30.StackBB = 30
	slumbot30Strict := slumbot30
	slumbot30Strict.AllowSBFold, slumbot30Strict.AllowSBLimp = false, false
	cases := []struct {
		name string
		cfg  NLConfig
		want []RoundStats
	}{
		{"pot-only 20bb (the tiny config)", mustLoad(t, "configs/tiny_20bb.json"),
			[]RoundStats{{12, 22, 33}, {36, 57, 88}, {76, 113, 176}, {132, 189, 296}}},
		{"bot_poker_200bb.json at stack_bb=20", botPoker20,
			[]RoundStats{{76, 150, 225}, {860, 1609, 2432}, {5212, 9245, 14064}, {22316, 37993, 58096}}},
		{"bot_poker_100bb.json", mustLoad(t, "testdata/treesize_configs/bot_poker_100bb.json"),
			[]RoundStats{{168, 334, 501}, {4464, 8679, 13060}, {54856, 103265, 155972}, {419888, 763939, 1158548}}},
		// The engine's own copy of the same menu (treesize variant g,
		// the final Bot Poker menu, 2026-11-17): 1,355,593 betting sequences.
		{"configs/botpoker_100bb.json", mustLoad(t, "configs/botpoker_100bb.json"),
			[]RoundStats{{168, 334, 501}, {4464, 8679, 13060}, {54856, 103265, 155972}, {419888, 763939, 1158548}}},
		{"slumbot_2013.json at stack_bb=30", slumbot30,
			[]RoundStats{{196, 390, 585}, {2124, 3957, 5984}, {12228, 21561, 32824}, {49804, 84161, 128816}}},
		{"slumbot_2013.json at stack_bb=30, no SB limp, no SB fold", slumbot30Strict,
			[]RoundStats{{98, 194, 291}, {964, 1784, 2700}, {5052, 8802, 13420}, {18788, 31300, 47996}}},
		{"sb=1 bb=2 stack_bb=7 opening=[0.5,0.75,1.5] (half-chip rounding)",
			NLConfig{SB: 1, BB: 2, StackBB: 7, Opening: []float64{0.5, 0.75, 1.5}, RaiseMenu: pot,
				ThreebetMenu: pot, FourbetMenu: pot, AllowSBFold: true, AllowSBLimp: true},
			[]RoundStats{{20, 38, 57}, {84, 141, 216}, {212, 325, 504}, {404, 589, 920}}},
	}
	for _, c := range cases {
		if got := BuildNLTree(c.cfg).Stats(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %v\nwant %v", c.name, got, c.want)
		}
	}
}

// Every chip a terminal pays out must have been put in along the path to
// it, the two seats must have put in the same amount at a showdown, and
// nobody can put in more than a stack.
func TestNLTreeStakesFollowTheBetting(t *testing.T) {
	cfg := mustLoad(t, "testdata/treesize_configs/bot_poker_200bb.json")
	cfg.StackBB = 20
	tree := BuildNLTree(cfg)
	var walk func(ni int32, in [2]int)
	walk = func(ni int32, in [2]int) {
		n := &tree.Nodes[ni]
		switch n.Kind {
		case Fold:
			if want := float32(in[n.Player]) / float32(cfg.BB); n.Stake != want {
				t.Fatalf("fold node %d: stake %v, folder put in %v bb", ni, n.Stake, want)
			}
		case Showdown:
			if in[0] != in[1] || n.Stake != float32(in[0])/float32(cfg.BB) {
				t.Fatalf("showdown node %d: stake %v, players put in %v", ni, n.Stake, in)
			}
		case Decision:
			for a := 0; a < int(n.NumActions); a++ {
				act := tree.Actions[int(n.FirstChild)+a]
				next := in
				me, opp := n.Player, 1-n.Player
				switch act.Kind {
				case ActCall:
					next[me] = in[opp]
				case ActBet:
					next[me] = max(in[me], in[opp]) + int(act.Amount)
				}
				if next[me] > cfg.StackBB*cfg.BB {
					t.Fatalf("node %d action %d: %d chips in, stack is %d", ni, a, next[me], cfg.StackBB*cfg.BB)
				}
				if act.AllIn != (act.Kind != ActFold && act.Kind != ActCheck && next[me] == cfg.StackBB*cfg.BB) {
					t.Fatalf("node %d action %d: AllIn=%v with %d chips in", ni, a, act.AllIn, next[me])
				}
				walk(tree.Children[int(n.FirstChild)+a], next)
			}
		}
	}
	walk(0, [2]int{cfg.SB, cfg.BB})
}

func TestIndexAssignsDisjointDenseSlots(t *testing.T) {
	g := NewLeduc()
	var next int64
	for i := range g.Tree.Nodes {
		n := &g.Tree.Nodes[i]
		if n.Kind != Decision {
			continue
		}
		if n.Offset != next {
			t.Fatalf("node %d: offset %d, want %d", i, n.Offset, next)
		}
		next += int64(n.NumActions) * int64(g.Tree.NumBuckets[n.Round])
	}
	if next != g.Tree.Slots {
		t.Fatalf("slots: %d, tree says %d", next, g.Tree.Slots)
	}
	// 288 is the usual count of Leduc information sets with suits merged.
	if g.Tree.Infosets != 288 {
		t.Errorf("Leduc infosets: got %d, want 288", g.Tree.Infosets)
	}
}
