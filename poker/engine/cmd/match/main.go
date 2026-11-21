// match plays blueprints against each other on the same betting tree,
// every deal twice with the seats swapped, and reports the first one's
// winnings in big blinds per 100 hands with a standard error: over both
// seatings (the paired comparison), and in each seat on its own.
//
// With -breakdown each pair's line is followed by where the winnings
// came from: see printBreakdown.
//
// Blueprints are given in pairs. The two sides may use different bucket
// files (-abs-a, -abs-b).
//
//	go run ./cmd/match -config configs/botpoker_100bb.json -abs-a data/abs-10-10-10.bin \
//	    -abs-b data/abs-200-200-200.bin -deals 1e7 -workers 16 \
//	    runs/botpoker-k10/blueprint-300000000.bp runs/botpoker-k200/blueprint-300000000.bp
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"strconv"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

func main() {
	game := flag.String("game", "holdem", "kuhn, leduc or holdem")
	config := flag.String("config", "", "holdem: betting-tree config (see configs/ for examples)")
	absA := flag.String("abs-a", "", "holdem: bucket file for the first blueprint of each pair")
	absB := flag.String("abs-b", "", "holdem: bucket file for the second (default: the first's)")
	dealsFlag := flag.String("deals", "1e6", "deals per pair; each is played in both seatings")
	seed := flag.Uint64("seed", 1, "random seed")
	workers := flag.Int("workers", 1, "goroutines")
	breakdown := flag.Bool("breakdown", false, "after each pair's line, print where the first blueprint's winnings came from (how and in which round hands ended) and both blueprints' fold frequencies at the decisions that came up")
	flag.Parse()
	log.SetFlags(0)
	if flag.NArg() == 0 || flag.NArg()%2 != 0 {
		log.Fatal("usage: match [flags] a.bp b.bp [a2.bp b2.bp ...]")
	}
	deals, err := strconv.ParseFloat(*dealsFlag, 64)
	if err != nil || deals < 1 {
		log.Fatalf("-deals: %q is not a count", *dealsFlag)
	}
	if *absB == "" {
		*absB = *absA
	}
	ga, closeA, err := engine.OpenGame(*game, *config, *absA)
	if err != nil {
		log.Fatal(err)
	}
	defer closeA()
	gb, closeB, err := engine.OpenGame(*game, *config, *absB)
	if err != nil {
		log.Fatal(err)
	}
	defer closeB()

	fmt.Println("a_iterations\tb_iterations\ta_wins_bb_per_100\tstd_err\ta_in_seat0\tstd_err\ta_in_seat1\tstd_err\tseconds")
	for i := 0; i < flag.NArg(); i += 2 {
		a, err := engine.OpenBlueprint(flag.Arg(i), ga)
		if err != nil {
			log.Fatal(err)
		}
		b, err := engine.OpenBlueprint(flag.Arg(i+1), gb)
		if err != nil {
			log.Fatal(err)
		}
		start := time.Now()
		play := engine.Match
		if *breakdown {
			play = engine.MatchWithBreakdown
		}
		res, err := play(engine.Player{Game: ga, Strat: a}, engine.Player{Game: gb, Strat: b},
			int64(math.Round(deals)), *seed, *workers)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%d\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.1f\n", a.Header.Iterations, b.Header.Iterations,
			100*res.Mean, 100*res.StdErr, 100*res.Seat[0].Mean, 100*res.Seat[0].StdErr, 100*res.Seat[1].Mean, 100*res.Seat[1].StdErr,
			time.Since(start).Seconds())
		if bd := res.Breakdown; bd != nil {
			printBreakdown(bd)
		}
		a.Close()
		b.Close()
	}
}

var endings = [...]string{"a_folded", "b_folded", "showdown_a_called", "showdown_b_called", "showdown_checked"}

// printBreakdown writes two tab-separated tables: hands by the round and
// manner of their ending, with a's winnings in each cell as bb/100 over
// all hands (the cells sum to a's total; the standard error treats hands
// as independent); and, per round and acting player, the decisions that
// had folding among the options.
func printBreakdown(bd *engine.MatchBreakdown) {
	fmt.Println("ended\tround\tpct_of_hands\ta_wins_bb_per_100\tstd_err")
	for e, name := range endings {
		for r := range bd.Ended {
			c := bd.Ended[r][e]
			if c.Hands == 0 {
				continue
			}
			mean := c.Net / bd.Hands
			se := math.Sqrt(max(c.NetSquared/bd.Hands-mean*mean, 0) / bd.Hands)
			fmt.Printf("%s\t%d\t%.2f\t%.2f\t%.2f\n", name, r, 100*c.Hands/bd.Hands, 100*mean, 100*se)
		}
	}
	fmt.Println("could_fold\tround\tdecisions_per_100_hands\tfolded_pct\ta_folds_here_pct\tb_folds_here_pct")
	for who, name := range [2]string{"a", "b"} {
		for r := range bd.Facing {
			c := bd.Facing[r][who]
			if c.Decisions == 0 {
				continue
			}
			fmt.Printf("%s\t%d\t%.2f\t%.2f\t%.2f\t%.2f\n", name, r, 100*c.Decisions/bd.Hands, 100*c.Folded/c.Decisions, 100*c.FoldA/c.Decisions, 100*c.FoldB/c.Decisions)
		}
	}
}
