// train runs external-sampling MCCFR and writes blueprint files.
//
//	go run ./cmd/train -game leduc -iters 1e7 -checkpoints 0,1e5,1e6 -exploit served
//	go run ./cmd/train -game holdem -config configs/tiny_20bb.json -abs data/abs-10-10-10.bin \
//	    -iters 1e9 -checkpoints 0,1e5,1e6,1e7,1e8 -exploit served -out runs/tiny
//
// One tab-separated line per checkpoint goes to stdout; everything else to
// stderr.
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

func main() {
	game := flag.String("game", "holdem", "kuhn, leduc or holdem")
	config := flag.String("config", "", "holdem: betting-tree config (poker/treesize format)")
	abs := flag.String("abs", "", "holdem: bucket file from cmd/buckets, or \"category\"")
	itersFlag := flag.String("iters", "1e6", "iterations (traversals) to run in total")
	checkpointsFlag := flag.String("checkpoints", "", "comma-separated iteration counts to report at, besides the end (0 = the untrained strategy)")
	storage := flag.String("storage", "float32", "regret storage: float32, float16 or bfloat16")
	sumStorage := flag.String("sum-storage", "", "strategy-sum storage (default: same as -storage)")
	floor := flag.Float64("floor", math.Inf(-1), "regret floor (<= 0); 0 is regret matching+")
	seed := flag.Uint64("seed", 1, "random seed")
	workers := flag.Int("workers", 1, "training goroutines (1 is deterministic)")
	exploit := flag.String("exploit", "", "at each checkpoint, best-respond to: served (the blueprint bytes), average (the float average strategy) or both")
	exploitWorkers := flag.Int("exploit-workers", runtime.GOMAXPROCS(0), "goroutines for best response")
	out := flag.String("out", "", "directory for blueprint files (one per checkpoint); none written if empty")
	flag.Parse()
	log.SetFlags(0)

	total, err := parseCount(*itersFlag)
	if err != nil {
		log.Fatalf("-iters: %v", err)
	}
	stops := []int64{total}
	for _, s := range strings.Split(*checkpointsFlag, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		n, err := parseCount(s)
		if err != nil {
			log.Fatalf("-checkpoints: %v", err)
		}
		if n < total {
			stops = append(stops, n)
		}
	}
	sort.Slice(stops, func(a, b int) bool { return stops[a] < stops[b] })
	doServed := *exploit == "served" || *exploit == "both"
	doAverage := *exploit == "average" || *exploit == "both"
	if *exploit != "" && !doServed && !doAverage {
		log.Fatalf("-exploit: want served, average or both")
	}

	g, closeGame, err := engine.OpenGame(*game, *config, *abs)
	if err != nil {
		log.Fatal(err)
	}
	defer closeGame()
	cfg := engine.DefaultTrainConfig()
	cfg.RegretStorage, cfg.SumStorage = engine.StoreKind(*storage), engine.StoreKind(*storage)
	if *sumStorage != "" {
		cfg.SumStorage = engine.StoreKind(*sumStorage)
	}
	cfg.Seed, cfg.Workers, cfg.RegretFloor = *seed, *workers, float32(*floor)
	solver, err := engine.NewSolver(g, cfg)
	if err != nil {
		log.Fatal(err)
	}
	if *out != "" {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	t := g.Tree
	log.Printf("%s: %d nodes, %d infosets, %d infoset-actions, buckets %v, abstraction %s",
		g.Rules.Name, len(t.Nodes), t.Infosets, t.Slots, t.NumBuckets, g.Abs.ID())
	log.Printf("storage: regrets %s + sums %s = %d bytes (%.2f per infoset-action, %.2f per infoset)",
		cfg.RegretStorage, cfg.SumStorage, solver.StorageBytes(),
		float64(solver.StorageBytes())/float64(t.Slots), float64(solver.StorageBytes())/float64(t.Infosets))

	fmt.Println("iterations\ttrain_seconds\titers_per_second\texploit_served\tbr_seat0\tbr_seat1\texploit_average")
	report := func(trainSeconds float64) {
		row := []string{strconv.FormatInt(solver.Iterations(), 10), fmt.Sprintf("%.1f", trainSeconds), "", "", "", "", ""}
		if trainSeconds > 0 {
			row[2] = fmt.Sprintf("%.0f", float64(solver.Iterations())/trainSeconds)
		}
		if *out != "" || doServed {
			bp := solver.Blueprint()
			if *out != "" {
				path := filepath.Join(*out, fmt.Sprintf("blueprint-%d.bp", solver.Iterations()))
				if err := writeAndVerify(bp, path, g); err != nil {
					log.Fatal(err)
				}
			}
			if doServed {
				start := time.Now()
				expl, br := engine.Exploitability(g, bp, *exploitWorkers)
				row[3], row[4], row[5] = fmt.Sprintf("%.6f", expl), fmt.Sprintf("%.6f", br[0]), fmt.Sprintf("%.6f", br[1])
				log.Printf("  best response to the served blueprint took %.1fs", time.Since(start).Seconds())
			}
		}
		if doAverage {
			expl, _ := engine.Exploitability(g, solver.AverageStrategy(), *exploitWorkers)
			row[6] = fmt.Sprintf("%.6f", expl)
		}
		fmt.Println(strings.Join(row, "\t"))
	}

	var trainSeconds float64
	for _, stop := range stops {
		start := time.Now()
		solver.Run(stop - solver.Iterations())
		trainSeconds += time.Since(start).Seconds()
		report(trainSeconds)
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	log.Printf("Go heap at exit: %d bytes in use, %d bytes obtained from the OS, %d GC cycles",
		mem.HeapInuse, mem.Sys, mem.NumGC)
}

// writeAndVerify writes the blueprint, maps the file back, and checks the
// mapped copy holds the same bytes: every decision served from the file is
// then the decision the in-memory strategy would make.
func writeAndVerify(bp *engine.Blueprint, path string, g *engine.Game) error {
	if err := bp.WriteFile(path); err != nil {
		return err
	}
	disk, err := engine.OpenBlueprint(path, g)
	if err != nil {
		return err
	}
	defer disk.Close()
	if !disk.Equal(bp) {
		return fmt.Errorf("%s: mapped file differs from the strategy in memory", path)
	}
	log.Printf("  wrote %s (%d strategy bytes; mmap read-back identical)", path, g.Tree.Slots)
	return nil
}

// parseCount reads an iteration count written as 2500000 or 2.5e6.
func parseCount(s string) (int64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f != math.Trunc(f) || f > 1e18 {
		return 0, fmt.Errorf("%q is not an iteration count", s)
	}
	return int64(f), nil
}
