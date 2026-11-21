package engine

// Fixed-limit betting tree, for Kuhn and Leduc: both players ante, player
// 0 acts first in every round, each round allows MaxBets wagers of that
// round's fixed size.

type LimitConfig struct {
	Ante    int
	BetSize []int // per round
	MaxBets int   // wagers allowed per round (1 = bet only, 2 = bet and one raise)
}

type limitState struct {
	round   int
	toAct   int
	bets    int // wagers so far this round
	facing  bool
	checked bool
	in      [2]int // chips each player has put in
}

func BuildLimitTree(cfg LimitConfig) *Tree {
	t := &Tree{Rounds: len(cfg.BetSize), Posted: [2]int32{int32(cfg.Ante), int32(cfg.Ante)}}
	buildLimit(cfg, t, limitState{in: [2]int{cfg.Ante, cfg.Ante}})
	return t
}

func buildLimit(cfg LimitConfig, t *Tree, st limitState) int32 {
	me, opp := st.toAct, 1-st.toAct
	size := cfg.BetSize[st.round]
	var acts []Action
	if st.facing {
		acts = append(acts, Action{Kind: ActFold}, Action{Kind: ActCall, Amount: int32(st.in[opp] - st.in[me])})
	} else {
		acts = append(acts, Action{Kind: ActCheck})
	}
	if st.bets < cfg.MaxBets {
		acts = append(acts, Action{Kind: ActBet, Amount: int32(size)})
	}
	node, first := t.addDecision(me, st.round, acts)

	closeRound := func(st limitState) int32 {
		if st.round == t.Rounds-1 {
			return t.add(Node{Kind: Showdown, Round: uint8(st.round), Stake: float32(st.in[0])})
		}
		return buildLimit(cfg, t, limitState{round: st.round + 1, in: st.in})
	}
	for i, act := range acts {
		var child int32
		next := st
		next.toAct = opp
		switch act.Kind {
		case ActFold:
			child = t.add(Node{Kind: Fold, Player: uint8(me), Round: uint8(st.round), Stake: float32(st.in[me])})
		case ActCheck:
			if st.checked {
				child = closeRound(st)
			} else {
				next.checked = true
				child = buildLimit(cfg, t, next)
			}
		case ActCall:
			next.in[me] = st.in[opp]
			child = closeRound(next)
		case ActBet:
			next.in[me] = st.in[opp] + size
			next.bets, next.facing, next.checked = st.bets+1, true, false
			child = buildLimit(cfg, t, next)
		}
		t.Children[int(first)+i] = child
	}
	return node
}
