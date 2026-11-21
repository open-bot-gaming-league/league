package engine

import "fmt"

// Game is everything the solver and the best-response code need: the card
// rules, the betting tree, and the card abstraction that maps what a
// player holds to a bucket in each round.
type Game struct {
	Rules *Rules
	Tree  *Tree
	Abs   Abstraction
	// Config is the betting-tree config the game was built from (hold'em
	// only); it travels in blueprint headers.
	Config *NLConfig
}

// NewGame indexes tree for abs's bucket counts and checks the three parts
// agree.
func NewGame(rules *Rules, tree *Tree, abs Abstraction) (*Game, error) {
	if tree.Rounds != rules.Rounds() {
		return nil, fmt.Errorf("engine: tree has %d rounds, %s has %d", tree.Rounds, rules.Name, rules.Rounds())
	}
	nb := make([]int, tree.Rounds)
	for r := range nb {
		nb[r] = abs.NumBuckets(r)
	}
	tree.Index(nb)
	return &Game{Rules: rules, Tree: tree, Abs: abs}, nil
}

func mustGame(rules *Rules, tree *Tree, abs Abstraction) *Game {
	g, err := NewGame(rules, tree, abs)
	if err != nil {
		panic(err)
	}
	return g
}

// NewKuhn is Kuhn poker: ante 1, one bet of 1, no raise.
func NewKuhn() *Game {
	return mustGame(KuhnRules(), BuildLimitTree(LimitConfig{Ante: 1, BetSize: []int{1}, MaxBets: 1}), kuhnAbs{})
}

// NewLeduc is Leduc hold'em: ante 1, bets of 2 then 4, one raise per round.
func NewLeduc() *Game {
	return mustGame(LeducRules(), BuildLimitTree(LimitConfig{Ante: 1, BetSize: []int{2, 4}, MaxBets: 2}), leducAbs{})
}

// NewHoldem is heads-up no-limit hold'em with cfg's stacks and bet menu and
// the given card abstraction.
func NewHoldem(cfg NLConfig, abs Abstraction) (*Game, error) {
	g, err := NewGame(HoldemRules(13), BuildNLTree(cfg), abs)
	if err != nil {
		return nil, err
	}
	g.Config = &cfg
	return g, nil
}

// OpenGame builds a game from command-line names: "kuhn", "leduc", or
// "holdem" with a betting-tree config file and a card abstraction, which
// is a bucket file from cmd/buckets or the word "category" for the
// file-free CategoryAbstraction. Call the returned function when done.
func OpenGame(name, configPath, absPath string) (*Game, func() error, error) {
	noop := func() error { return nil }
	switch name {
	case "kuhn":
		return NewKuhn(), noop, nil
	case "leduc":
		return NewLeduc(), noop, nil
	case "holdem":
		if configPath == "" || absPath == "" {
			return nil, nil, fmt.Errorf("engine: holdem needs a betting config and a card abstraction")
		}
		cfg, err := LoadNLConfig(configPath)
		if err != nil {
			return nil, nil, err
		}
		if absPath == "category" {
			g, err := NewHoldem(cfg, CategoryAbstraction{Ranks: 13})
			return g, noop, err
		}
		abs, err := OpenTableAbstraction(absPath)
		if err != nil {
			return nil, nil, err
		}
		g, err := NewHoldem(cfg, abs)
		if err != nil {
			abs.Close()
			return nil, nil, err
		}
		return g, abs.Close, nil
	}
	return nil, nil, fmt.Errorf("engine: unknown game %q (kuhn, leduc, holdem)", name)
}
