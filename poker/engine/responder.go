package engine

import (
	"fmt"
	"math"
	"sync"
)

// Responders: real-card players that exploit a strategy by play.
//
// A responder sits in one seat, sees its own real cards, and keeps the
// opponent's range: the exact probability of each opponent hand given the
// public history and the opponent's strategy. What it wins per hand
// against the strategy is a lower bound on the strategy's real-card
// exploitability, because it is the winnings of one particular opponent.
// It needs no best-response computation, only play, so it runs on trees
// where the exact number (bestresponse.go) is out of reach. Its actions
// are the betting tree's own, so the strategy under test is never off its
// tree.
//
// The bound is only valid if the responder's choices do not depend on
// what it cannot see. Both kinds here choose from their own cards, the
// public history, the range, and random draws of their own (thinking);
// the real opponent hand and the real undealt board are used only to play
// the hand out and score it. TestResponderChoiceIgnoresHiddenCards holds
// that.
//
// Two kinds, differing in how they value an action:
//
// LBR, local best response (Lisy and Bowling 2017, "Equilibrium
// approximation quality of current no-limit poker bots"), assumes that
// after its action the hand is checked or called down to a showdown
// unless the opponent folds at once:
//
//	check or call:  wp*pot - (1-wp)*asked
//	bet or raise s: fp*pot + (1-fp) * (wp'*(pot+s) - (1-wp')*(asked+s))
//
// with pot the chips in the middle, asked the chips to call, wp the chance
// of winning a showdown against the range, fp the chance the opponent
// folds to the bet, and wp' the chance of winning against the hands that
// do not fold. It folds if every action is negative. One departure from
// the paper: wp deals out the remaining board exhaustively only when that
// is at most Rollouts boards (the turn, in hold'em); otherwise it averages
// Rollouts random boards.
//
// Rollout values an action by playing it out: draw an opponent hand from
// the range and the rest of the board, take the action, then let both
// sides play the strategy under test to the end, the responder using its
// real hand. The average over Rollouts such games is the action's value,
// and the responder takes the best. Doing that at every decision is one
// step of policy improvement over the strategy, made with real cards and
// the exact range. It sees the later betting that LBR assumes away.

type ResponderKind string

const (
	LBR     ResponderKind = "lbr"
	Rollout ResponderKind = "rollout"
)

type ResponderConfig struct {
	Kind    ResponderKind
	Hands   int64 // deals; each is played twice, the responder in each seat
	Seed    uint64
	Workers int
	// Rollouts is, for LBR, the boards per win-probability estimate
	// (default 60) and, for Rollout, the games played out per decision,
	// every action on the same draws (default 400).
	Rollouts int
	// LBR only. The paper runs LBR in several settings and reports the
	// best, since each is a lower bound. From is the first round LBR plays
	// by its rule; in earlier rounds it just checks or calls. NoBets
	// restricts its rule to fold and call.
	From   int
	NoBets bool
	// Streets, when non-empty, lists the rounds in which the responder
	// uses its rule. In the others it plays the strategy under test
	// itself, with its real hand. That splits what it wins by street: with
	// no round allowed it is the strategy playing itself.
	Streets []int
}

type ResponderResult struct {
	Value      float64    // the responder's winnings per hand, averaged over the two seats
	StdErr     float64    // of Value, over deals
	Seat       [2]float64 // its winnings per hand in each seat
	SeatStdErr [2]float64
}

// Respond plays a responder against strat.
func Respond(g *Game, strat Strategy, cfg ResponderConfig) (ResponderResult, error) {
	switch cfg.Kind {
	case LBR:
		if cfg.Rollouts <= 0 {
			cfg.Rollouts = 60
		}
	case Rollout:
		if cfg.Rollouts <= 0 {
			cfg.Rollouts = 400
		}
	default:
		return ResponderResult{}, fmt.Errorf("engine: unknown responder %q (lbr, rollout)", cfg.Kind)
	}
	workers := max(cfg.Workers, 1)
	// Per worker: sums and sums of squares of (seat 0, seat 1, average).
	tallies := make([][6]float64, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		share := cfg.Hands / int64(workers)
		if int64(w) < cfg.Hands%int64(workers) {
			share++
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := newResponder(g, strat, cfg, uint64(w))
			var t [6]float64
			for i := int64(0); i < share; i++ {
				g.sampleDeal(p.world, &p.deal)
				v0, v1 := p.play(0), p.play(1)
				for j, v := range [3]float64{v0, v1, (v0 + v1) / 2} {
					t[j] += v
					t[3+j] += v * v
				}
			}
			tallies[w] = t
		}()
	}
	wg.Wait()
	var t [6]float64
	for _, wt := range tallies {
		for j := range t {
			t[j] += wt[j]
		}
	}
	n := float64(cfg.Hands)
	stat := func(j int) (mean, se float64) {
		mean = t[j] / n
		return mean, math.Sqrt(max(t[3+j]/n-mean*mean, 0) / n)
	}
	var res ResponderResult
	res.Seat[0], res.SeatStdErr[0] = stat(0)
	res.Seat[1], res.SeatStdErr[1] = stat(1)
	res.Value, res.StdErr = stat(2)
	return res, nil
}

// responder is one goroutine's player, with its scratch.
type responder struct {
	g     *Game
	strat Strategy
	cfg   ResponderConfig
	// world deals the cards and draws the opponent's actions; thinking is
	// the responder's own randomness. Keeping them apart is what lets a
	// test show the choices do not depend on the hidden cards.
	world, thinking *rng
	deal            Deal
	hands           [][]Card
	masks           []uint64 // per hand: its cards as a bit set

	deviate [MaxRounds]bool // rounds in which the responder uses its rule
	seat    int             // the responder's seat this hand
	rng     []float64       // opponent range, by hand; unnormalised
	bucket  []int32         // opponent hand -> its bucket on the visible board
	probs   [MaxActions]float64
	visible [5]Card

	// LBR
	win, cnt []float64 // per opponent hand: showdowns won (ties half), and showdowns, over the rollout boards
	cont     []float64 // scratch: the range that does not fold
	full     [5]Card

	// Rollout
	cum      []float64 // running sum of the range, for drawing a hand
	imagined Deal      // one drawn opponent hand and board
	value    [MaxActions]float64
}

func newResponder(g *Game, strat Strategy, cfg ResponderConfig, stream uint64) *responder {
	hands := g.Rules.Hands()
	n := len(hands)
	p := &responder{g: g, strat: strat, cfg: cfg, hands: hands,
		world: newRNG(cfg.Seed, 2*stream), thinking: newRNG(cfg.Seed, 2*stream+1),
		masks: make([]uint64, n), rng: make([]float64, n), bucket: make([]int32, n),
		win: make([]float64, n), cnt: make([]float64, n), cont: make([]float64, n), cum: make([]float64, n)}
	for h, hand := range hands {
		p.masks[h] = cardMask(hand)
	}
	for round := range p.deviate {
		p.deviate[round] = len(cfg.Streets) == 0
	}
	for _, round := range cfg.Streets {
		if round >= 0 && round < MaxRounds {
			p.deviate[round] = true
		}
	}
	return p
}

// play plays the current deal with the responder in the given seat and
// returns the responder's winnings.
func (p *responder) play(seat int) float64 {
	t := p.g.Tree
	p.sit(seat)
	in := [2]float64{float64(t.Posted[0]), float64(t.Posted[1])}
	round := -1
	ni := int32(0)
	for {
		n := &t.Nodes[ni]
		switch n.Kind {
		case Fold:
			if int(n.Player) == seat {
				return -float64(n.Stake)
			}
			return float64(n.Stake)
		case Showdown:
			v := float64(p.deal.Sign) * float64(n.Stake)
			if seat == 1 {
				v = -v
			}
			return v
		}
		if int(n.Round) != round {
			round = int(n.Round)
			p.newRound(round)
		}
		na := int(n.NumActions)
		var a int
		if int(n.Player) == seat {
			a = p.choose(n, in)
		} else {
			// The opponent acts on its real hand; the range follows by
			// Bayes' rule.
			probs := p.probs[:na]
			p.strat.Probs(n, int(p.deal.Bucket[1-seat][round]), probs)
			a = sampleProbs(probs, p.world.float64())
			for h, w := range p.rng {
				if w != 0 {
					p.strat.Probs(n, int(p.bucket[h]), probs)
					p.rng[h] = w * probs[a]
				}
			}
		}
		act := t.Actions[int(n.FirstChild)+a]
		me, opp := int(n.Player), 1-int(n.Player)
		switch act.Kind {
		case ActCall:
			in[me] = in[opp]
		case ActBet:
			in[me] = max(in[me], in[opp]) + float64(act.Amount)
		}
		ni = t.Children[int(n.FirstChild)+a]
	}
}

// sit starts a hand in the given seat: the opponent may hold any hand
// that shares no card with the responder's.
func (p *responder) sit(seat int) {
	p.seat = seat
	mine := cardMask(p.g.Rules.hole(&p.deal, seat))
	for h := range p.rng {
		p.rng[h] = 1
		if p.masks[h]&mine != 0 {
			p.rng[h] = 0
		}
	}
}

// choose is the responder's decision at node n, with in the chips each
// seat has put in so far. It may read the responder's own hole cards, the
// visible board, the range and p.thinking, and nothing else of the deal.
func (p *responder) choose(n *Node, in [2]float64) int {
	if !p.deviate[n.Round] {
		// Not a round it may deviate in: play the strategy under test.
		probs := p.probs[:n.NumActions]
		p.strat.Probs(n, int(p.deal.Bucket[p.seat][n.Round]), probs)
		return sampleProbs(probs, p.thinking.float64())
	}
	if p.cfg.Kind == Rollout {
		return p.chooseByRollout(n)
	}
	return p.chooseLBR(n, in)
}

// newRound takes the newly visible board cards out of the range and
// looks up every opponent hand's bucket on the board as it now stands.
func (p *responder) newRound(round int) {
	r := p.g.Rules
	visible := p.visible[:r.BoardSize(round)]
	copy(visible, r.board(&p.deal))
	board := cardMask(visible)
	sortCards(visible)
	for h, w := range p.rng {
		if w == 0 {
			continue
		}
		if p.masks[h]&board != 0 {
			p.rng[h] = 0
			continue
		}
		p.bucket[h] = int32(p.g.Abs.Bucket(round, p.hands[h], visible))
	}
}

// chooseLBR is the local best response rule.
func (p *responder) chooseLBR(n *Node, in [2]float64) int {
	t := p.g.Tree
	me, opp := p.seat, 1-p.seat
	pot, asked := in[0]+in[1], in[opp]-in[me]
	if int(n.Round) < p.cfg.From {
		for a := 0; a < int(n.NumActions); a++ {
			if k := t.Actions[int(n.FirstChild)+a].Kind; k == ActCheck || k == ActCall {
				return a
			}
		}
	}
	p.rollout(int(n.Round))

	best, bestValue := -1, 0.0 // folding is worth 0
	fold := -1
	for a := 0; a < int(n.NumActions); a++ {
		act := t.Actions[int(n.FirstChild)+a]
		var value float64
		switch act.Kind {
		case ActFold:
			fold = a
			continue
		case ActCheck, ActCall:
			wp := p.winProbability(p.rng)
			value = wp*pot - (1-wp)*asked
		case ActBet:
			if p.cfg.NoBets {
				continue
			}
			size := float64(act.Amount)
			child := &t.Nodes[t.Children[int(n.FirstChild)+a]]
			foldAt := -1
			if child.Kind == Decision {
				for c := 0; c < int(child.NumActions); c++ {
					if t.Actions[int(child.FirstChild)+c].Kind == ActFold {
						foldAt = c
					}
				}
			}
			var all, stay float64
			probs := p.probs[:child.NumActions]
			for h, w := range p.rng {
				p.cont[h] = 0
				if w == 0 {
					continue
				}
				keep := w
				if foldAt >= 0 {
					p.strat.Probs(child, int(p.bucket[h]), probs)
					keep = w * (1 - probs[foldAt])
				}
				p.cont[h] = keep
				all += w
				stay += keep
			}
			if all == 0 {
				continue
			}
			fp := 1 - stay/all
			wp := p.winProbability(p.cont)
			value = fp*pot + (1-fp)*(wp*(pot+size)-(1-wp)*(asked+size))
		}
		if best < 0 || value > bestValue {
			best, bestValue = a, value
		}
	}
	if fold >= 0 && (best < 0 || bestValue < 0) {
		return fold
	}
	return best
}

// rollout fills win and cnt for LBR's hand against every opponent hand
// still in the range: over the ways the board can finish, how many
// showdowns there are (the opponent's hand must miss the new cards) and
// how many LBR wins. Every way, if there are at most p.cfg.Rollouts of them;
// otherwise p.cfg.Rollouts ways drawn at random.
func (p *responder) rollout(round int) {
	r := p.g.Rules
	mine := r.hole(&p.deal, p.seat)
	seen := r.BoardSize(round)
	total := r.BoardSize(r.Rounds() - 1)
	missing := total - seen
	full := p.full[:total]
	copy(full, r.board(&p.deal)[:seen])
	for h, w := range p.rng {
		if w != 0 {
			p.win[h], p.cnt[h] = 0, 0
		}
	}
	used := cardMask(mine) | cardMask(full[:seen])
	one := func(extra uint64) {
		strength := r.Strength(mine, full)
		for h, w := range p.rng {
			if w == 0 || p.masks[h]&extra != 0 {
				continue
			}
			p.cnt[h]++
			switch theirs := r.Strength(p.hands[h], full); {
			case strength > theirs:
				p.win[h]++
			case strength == theirs:
				p.win[h] += 0.5
			}
		}
	}
	if missing == 0 {
		one(0)
		return
	}
	free := r.Deck - len(mine) - seen
	if binom[free][min(missing, 7)] <= p.cfg.Rollouts && missing <= 7 {
		forEachCombo(r.Deck, missing, used, func(cs []Card) {
			copy(full[seen:], cs)
			one(cardMask(cs))
		})
		return
	}
	for k := 0; k < p.cfg.Rollouts; k++ {
		var extra uint64
		for i := seen; i < total; {
			c := p.thinking.intn(r.Deck)
			if (used|extra)&(1<<uint(c)) == 0 {
				extra |= 1 << uint(c)
				full[i] = Card(c)
				i++
			}
		}
		one(extra)
	}
}

// winProbability is LBR's chance of winning a showdown against the given
// range (ties count half), from the last rollout.
func (p *responder) winProbability(weights []float64) float64 {
	var win, cnt float64
	for h, w := range weights {
		if w != 0 {
			win += w * p.win[h]
			cnt += w * p.cnt[h]
		}
	}
	if cnt == 0 {
		return 0.5
	}
	return win / cnt
}

// chooseByRollout values each action by playing it out against drawn
// opponent hands and boards, both sides then following the strategy, and
// returns the best. Every action faces the same draws.
func (p *responder) chooseByRollout(n *Node) int {
	g, r := p.g, p.g.Rules
	t := g.Tree
	me := p.seat
	na := int(n.NumActions)
	kids := t.Children[n.FirstChild : int(n.FirstChild)+na]

	var total float64
	for h, w := range p.rng {
		total += w
		p.cum[h] = total
	}
	if total == 0 {
		// The opponent's strategy gives this line probability zero for
		// every hand: nothing to learn from; take the first action.
		return 0
	}
	value := p.value[:na]
	clear(value)
	d := &p.imagined
	mine := r.hole(&p.deal, me)
	seen := r.BoardSize(int(n.Round))
	need := r.BoardSize(r.Rounds() - 1)
	copy(r.hole(d, me), mine)
	copy(r.board(d)[:seen], r.board(&p.deal)[:seen])
	used := cardMask(mine) | cardMask(r.board(d)[:seen])
	for k := 0; k < p.cfg.Rollouts; k++ {
		// An opponent hand from the range, then the rest of the board
		// from the cards neither player nor the board holds.
		x := p.thinking.float64() * total
		lo, hi := 0, len(p.cum)-1
		for lo < hi {
			if mid := (lo + hi) / 2; p.cum[mid] > x {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		copy(r.hole(d, 1-me), p.hands[lo])
		taken := used | p.masks[lo]
		for i := seen; i < need; {
			c := p.thinking.intn(r.Deck)
			if taken&(1<<uint(c)) == 0 {
				taken |= 1 << uint(c)
				r.board(d)[i] = Card(c)
				i++
			}
		}
		g.fillDeal(d)
		for a, kid := range kids {
			value[a] += p.playOut(kid, d)
		}
	}
	best := 0
	for a := 1; a < na; a++ {
		if value[a] > value[best] {
			best = a
		}
	}
	return best
}

// playOut finishes an imagined deal from node ni with both seats sampling
// from the strategy, and returns the responder's winnings.
func (p *responder) playOut(ni int32, d *Deal) float64 {
	t := p.g.Tree
	for {
		n := &t.Nodes[ni]
		switch n.Kind {
		case Fold:
			if int(n.Player) == p.seat {
				return -float64(n.Stake)
			}
			return float64(n.Stake)
		case Showdown:
			v := float64(d.Sign) * float64(n.Stake)
			if p.seat == 1 {
				v = -v
			}
			return v
		}
		probs := p.probs[:n.NumActions]
		p.strat.Probs(n, int(d.Bucket[n.Player][n.Round]), probs)
		ni = t.Children[int(n.FirstChild)+sampleProbs(probs, p.thinking.float64())]
	}
}
