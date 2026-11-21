// respond plays a real-card responder against blueprint files: a sampled
// lower bound on real-card exploitability that needs only play, so it
// works on trees where cmd/exploit's exact answer is out of reach. Two
// kinds (see responder.go): lbr, the local best response of Lisy and
// Bowling, and rollout, which values actions by playing them out.
//
//	go run ./cmd/respond -kind rollout -game holdem -config configs/botpoker_100bb.json \
//	    -abs data/abs-10-10-10.bin -hands 1e5 -workers 16 runs/botpoker-k10/blueprint-*.bp
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

func main() {
	game := flag.String("game", "holdem", "kuhn, leduc or holdem")
	config := flag.String("config", "", "holdem: betting-tree config (poker/treesize format)")
	abs := flag.String("abs", "", "holdem: bucket file from cmd/buckets, or \"category\"")
	kind := flag.String("kind", "lbr", "lbr or rollout")
	handsFlag := flag.String("hands", "1e5", "deals per blueprint; each is played with the responder in both seats")
	rollouts := flag.Int("rollouts", 0, "lbr: boards per win-probability estimate (default 60); rollout: games played out per decision (default 400)")
	from := flag.Int("from", 0, "lbr: first round it plays by its rule (0 = preflop); before it, it checks or calls")
	noBets := flag.Bool("no-bets", false, "lbr: only fold or call")
	streets := flag.String("streets", "all", "rounds in which the responder may deviate, e.g. 2,3 (0 = preflop); elsewhere it plays the blueprint; \"none\" is the blueprint against itself")
	control := flag.String("control", "", "instead of blueprint files, play against a fixed strategy: always-fold or always-call")
	seed := flag.Uint64("seed", 1, "random seed")
	workers := flag.Int("workers", 1, "goroutines")
	flag.Parse()
	log.SetFlags(0)
	if (flag.NArg() == 0) == (*control == "") {
		log.Fatal("usage: respond [flags] blueprint.bp ...   or   respond [flags] -control always-fold|always-call")
	}
	hands, err := strconv.ParseFloat(*handsFlag, 64)
	if err != nil || hands < 1 {
		log.Fatalf("-hands: %q is not a count", *handsFlag)
	}
	g, closeGame, err := engine.OpenGame(*game, *config, *abs)
	if err != nil {
		log.Fatal(err)
	}
	defer closeGame()

	cfg := engine.ResponderConfig{Kind: engine.ResponderKind(*kind), Hands: int64(math.Round(hands)), Seed: *seed,
		Workers: *workers, Rollouts: *rollouts, From: *from, NoBets: *noBets}
	switch *streets {
	case "all":
	case "none":
		cfg.Streets = []int{-1} // non-empty, and no round matches
	default:
		for _, s := range strings.Split(*streets, ",") {
			round, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || round < 0 || round >= engine.MaxRounds {
				log.Fatalf("-streets: %q is not a list of rounds", *streets)
			}
			cfg.Streets = append(cfg.Streets, round)
		}
	}
	run := func(label string, strat engine.Strategy) {
		start := time.Now()
		res, err := engine.Respond(g, strat, cfg)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s\t%.6f\t%.6f\t%.6f\t%.6f\t%.6f\t%.6f\t%.1f\n", label, res.Value, res.StdErr,
			res.Seat[0], res.SeatStdErr[0], res.Seat[1], res.SeatStdErr[1], time.Since(start).Seconds())
	}
	fmt.Println("iterations\twins\tstd_err\twins_seat0\tstd_err_seat0\twins_seat1\tstd_err_seat1\tseconds")
	switch *control {
	case "":
		for _, path := range flag.Args() {
			bp, err := engine.OpenBlueprint(path, g)
			if err != nil {
				log.Fatal(err)
			}
			run(fmt.Sprint(bp.Header.Iterations), bp)
			bp.Close()
		}
	case "always-fold":
		run(*control, engine.AlwaysFold{Tree: g.Tree})
	case "always-call":
		run(*control, engine.AlwaysCall{Tree: g.Tree})
	default:
		log.Fatalf("-control: want always-fold or always-call, got %q", *control)
	}
}
