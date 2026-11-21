// inside measures a blueprint's exploitability inside its own card
// abstraction: what responders who see only buckets win against it. It is
// a lower bound (the responders are trained, not solved for), and it is
// the number the solver itself is minimising, which makes it the check on
// the solver when the real-card exploitability from cmd/exploit stops
// falling.
//
//	go run ./cmd/inside -game holdem -config configs/tiny_20bb.json -abs data/abs-10-10-10.bin \
//	    -iters 1e8 -deals 2e7 runs/tiny/blueprint-*.bp
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"strconv"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

func main() {
	game := flag.String("game", "holdem", "kuhn, leduc or holdem")
	config := flag.String("config", "", "holdem: betting-tree config (see configs/ for examples)")
	abs := flag.String("abs", "", "holdem: bucket file from cmd/buckets, or \"category\"")
	itersFlag := flag.String("iters", "1e8", "training iterations for each seat's responder")
	dealsFlag := flag.String("deals", "2e7", "random deals to measure each responder on")
	seed := flag.Uint64("seed", 1, "random seed")
	workers := flag.Int("workers", 1, "goroutines")
	flag.Parse()
	log.SetFlags(0)
	if flag.NArg() == 0 {
		log.Fatal("usage: inside [flags] blueprint.bp ...")
	}
	iters, err := strconv.ParseFloat(*itersFlag, 64)
	if err != nil || iters < 1 {
		log.Fatalf("-iters: %q is not an iteration count", *itersFlag)
	}
	deals, err := strconv.ParseFloat(*dealsFlag, 64)
	if err != nil || deals < 1 {
		log.Fatalf("-deals: %q is not a count", *dealsFlag)
	}

	g, closeGame, err := engine.OpenGame(*game, *config, *abs)
	if err != nil {
		log.Fatal(err)
	}
	defer closeGame()
	cfg := engine.DefaultTrainConfig()
	cfg.Seed, cfg.Workers = *seed, *workers

	fmt.Println("iterations\tinside_exploit_lower_bound\tstd_err\tresponder_seat0\tresponder_seat1")
	for _, path := range flag.Args() {
		bp, err := engine.OpenBlueprint(path, g)
		if err != nil {
			log.Fatal(err)
		}
		bound, se, value, err := engine.BucketedExploitability(g, bp, int64(math.Round(iters)), int64(math.Round(deals)), cfg)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%d\t%.6f\t%.6f\t%.6f\t%.6f\n", bp.Header.Iterations, bound, se, value[0], value[1])
		bp.Close()
	}
}
