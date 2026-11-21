package engine

import (
	"fmt"
	"math"
	"sync"
)

// Player is a strategy together with the game (tree indexing and card
// abstraction) it was trained on.
type Player struct {
	Game  *Game
	Strat Strategy
}

// MatchResult is the first player's winnings per hand in a Match.
type MatchResult struct {
	// Both seatings of each deal averaged, the pair being one sample: the
	// comparison of the two players, with most of the luck of the cards
	// taken out.
	Mean, StdErr float64
	// Seat[s] is the first player's winnings when it sits in seat s, over
	// the same deals. Unpaired, so each is much noisier than Mean, and
	// each includes what the seat itself is worth.
	Seat [2]struct{ Mean, StdErr float64 }
	// Breakdown is where the winnings came from; only from
	// MatchWithBreakdown.
	Breakdown *MatchBreakdown
}

// How a hand of a match ended.
const (
	EndAFolded = iota // the first player folded
	EndBFolded
	EndACalled // showdown after the first player made the last call
	EndBCalled
	EndChecked // showdown after a check: the last round was checked through
	numMatchEndings
)

// MatchBreakdown sorts a match's hands by how and where they ended, and
// compares the two players' folding at the decisions that came up. Counts
// are in hands and decisions, not deals: each deal is two hands.
type MatchBreakdown struct {
	Hands float64
	// Ended[round][ending]: the hands whose last action came in that
	// round and ended that way, and the first player's winnings over them.
	// The Net fields of all cells sum to its total winnings.
	Ended [MaxRounds][numMatchEndings]struct{ Hands, Net, NetSquared float64 }
	// Facing[round][who]: the decisions player who (0 the first, 1 the
	// second) had in that round with folding among its options. Folded is
	// how many it folded; FoldA and FoldB are the sums over those
	// decisions of each player's own fold probability at that very
	// infoset, so FoldB/Decisions on the first player's row is how often
	// the second would have folded in the first's place.
	Facing [MaxRounds][2]struct{ Decisions, Folded, FoldA, FoldB float64 }
}

func (m *MatchBreakdown) add(o *MatchBreakdown) {
	m.Hands += o.Hands
	for r := range m.Ended {
		for e := range m.Ended[r] {
			m.Ended[r][e].Hands += o.Ended[r][e].Hands
			m.Ended[r][e].Net += o.Ended[r][e].Net
			m.Ended[r][e].NetSquared += o.Ended[r][e].NetSquared
		}
		for w := range m.Facing[r] {
			m.Facing[r][w].Decisions += o.Facing[r][w].Decisions
			m.Facing[r][w].Folded += o.Facing[r][w].Folded
			m.Facing[r][w].FoldA += o.Facing[r][w].FoldA
			m.Facing[r][w].FoldB += o.Facing[r][w].FoldB
		}
	}
}

// MatchWithBreakdown is Match with MatchResult.Breakdown filled in.
func MatchWithBreakdown(a, b Player, deals int64, seed uint64, workers int) (MatchResult, error) {
	return match(a, b, deals, seed, workers, true)
}

// Match plays a against b on random deals and returns a's winnings per
// hand with their standard error. Every deal is played twice with the
// same cards, once with a in each seat. Actions are sampled from the two
// strategies.
//
// The two players may use different card abstractions but must share the
// card rules and the betting tree.
func Match(a, b Player, deals int64, seed uint64, workers int) (MatchResult, error) {
	return match(a, b, deals, seed, workers, false)
}

func match(a, b Player, deals int64, seed uint64, workers int, breakdown bool) (res MatchResult, err error) {
	ta, tb := a.Game.Tree, b.Game.Tree
	if a.Game.Rules.Name != b.Game.Rules.Name || len(ta.Nodes) != len(tb.Nodes) || len(ta.Children) != len(tb.Children) {
		return res, fmt.Errorf("engine: the two players are not on the same game and betting tree")
	}
	for i := range ta.Nodes {
		x, y := &ta.Nodes[i], &tb.Nodes[i]
		if x.Kind != y.Kind || x.Player != y.Player || x.NumActions != y.NumActions || x.FirstChild != y.FirstChild || x.Stake != y.Stake {
			return res, fmt.Errorf("engine: the two players' betting trees differ at node %d", i)
		}
	}
	workers = max(workers, 1)
	// Per worker, sums then sums of squares of: the pair's average, a's
	// winnings in seat 0, a's winnings in seat 1.
	tallies := make([][6]float64, workers)
	breakdowns := make([]*MatchBreakdown, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		share := deals / int64(workers)
		if int64(w) < deals%int64(workers) {
			share++
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rnd := newRNG(seed, uint64(w))
			players := [2]Player{a, b}
			var seen [2]Deal // the same cards as each player's abstraction sees them
			var probs, others [MaxActions]float64
			var bd *MatchBreakdown
			if breakdown {
				bd = new(MatchBreakdown)
				breakdowns[w] = bd
			}
			// ended books a finished hand: v is seat 0's winnings, lastWho
			// and lastKind the last action taken.
			ended := func(first int, n *Node, v float64, lastWho int, lastKind ActionKind) {
				ending := EndChecked
				switch {
				case n.Kind == Fold:
					ending = EndAFolded + (first ^ int(n.Player)) // the folding seat's player
				case lastKind == ActCall:
					ending = EndACalled + lastWho
				}
				if first == 1 {
					v = -v // a's winnings
				}
				cell := &bd.Ended[n.Round][ending]
				cell.Hands++
				cell.Net += v
				cell.NetSquared += v * v
				bd.Hands++
			}
			// play returns seat 0's winnings with players[first] in seat 0.
			play := func(first int) (v float64) {
				ni := int32(0)
				lastWho, lastKind := 0, ActCheck
				for {
					n := &ta.Nodes[ni]
					if n.Kind != Decision {
						switch {
						case n.Kind == Showdown:
							v = float64(seen[0].Sign) * float64(n.Stake)
						case n.Player == 0:
							v = -float64(n.Stake)
						default:
							v = float64(n.Stake)
						}
						if bd != nil {
							ended(first, n, v, lastWho, lastKind)
						}
						return v
					}
					who := first ^ int(n.Player) // which player sits in the acting seat
					pl := &players[who]
					own := &pl.Game.Tree.Nodes[ni]
					pr := probs[:n.NumActions]
					pl.Strat.Probs(own, int(seen[who].Bucket[n.Player][n.Round]), pr)
					act := sampleProbs(pr, rnd.float64())
					lastWho, lastKind = who, ta.Actions[int(n.FirstChild)+act].Kind
					// The fold, when there is one, is the first action.
					if bd != nil && ta.Actions[n.FirstChild].Kind == ActFold {
						other := &players[1-who]
						or := others[:n.NumActions]
						other.Strat.Probs(&other.Game.Tree.Nodes[ni], int(seen[1-who].Bucket[n.Player][n.Round]), or)
						cell := &bd.Facing[n.Round][who]
						cell.Decisions++
						if act == 0 {
							cell.Folded++
						}
						var folds [2]float64
						folds[who], folds[1-who] = pr[0], or[0]
						cell.FoldA += folds[0]
						cell.FoldB += folds[1]
					}
					ni = ta.Children[int(n.FirstChild)+act]
				}
			}
			var tally [6]float64
			for i := int64(0); i < share; i++ {
				a.Game.Rules.dealCards(rnd, &seen[0])
				seen[1].cards = seen[0].cards
				a.Game.fillDeal(&seen[0])
				b.Game.fillDeal(&seen[1])
				first, second := play(0), -play(1) // a's winnings in seat 0, then in seat 1
				for k, v := range [3]float64{(first + second) / 2, first, second} {
					tally[k] += v
					tally[3+k] += v * v
				}
			}
			tallies[w] = tally
		}()
	}
	wg.Wait()
	var total [6]float64
	for _, tally := range tallies {
		for k, v := range tally {
			total[k] += v
		}
	}
	n := float64(deals)
	stat := func(k int) (mean, stderr float64) {
		mean = total[k] / n
		return mean, math.Sqrt(max(total[3+k]/n-mean*mean, 0) / n)
	}
	res.Mean, res.StdErr = stat(0)
	res.Seat[0].Mean, res.Seat[0].StdErr = stat(1)
	res.Seat[1].Mean, res.Seat[1].StdErr = stat(2)
	if breakdown {
		res.Breakdown = new(MatchBreakdown)
		for _, bd := range breakdowns {
			res.Breakdown.add(bd)
		}
	}
	return res, nil
}
