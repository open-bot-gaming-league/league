// cfrbr runs sampled-subgame CFR-BR (engine/cfrbr.go, CFRBR.md) and writes
// blueprint files: the abstract player's average strategy and its current
// one at every checkpoint.
//
//	go run ./cmd/cfrbr -game leduc -iters 1e4 -checkpoints 100,1000 -exploit both
//	go run ./cmd/cfrbr -game holdem -config configs/tiny_20bb.json -abs data/abs-200-200-200.bin \
//	    -iters 2e4 -checkpoints 300,1000,3000,1e4 -workers 8 -out runs/cfrbr \
//	    -exploit both -exploit-bin runs/exploit-base
//
// An iteration is one sampled flop, solved for both seats; on the 20 bb
// tree that is several thread-seconds. One tab-separated line per
// checkpoint goes to stdout; everything else to stderr. The seconds
// columns count this process only.
//
// A run cannot be continued from a blueprint. With -out, every checkpoint
// also writes state-N.cfrbr (8 bytes per infoset-action), and
//
//	go run ./cmd/cfrbr <the same flags> -resume runs/cfrbr/state-3000.cfrbr
//
// goes on from iteration 3,000 with the remaining checkpoints. A stopped
// run loses only what came after its last checkpoint, so space the
// checkpoints by how much work that may be. With one worker the resumed
// run is bit for bit the uninterrupted one.
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

func main() {
	game := flag.String("game", "holdem", "leduc or holdem")
	config := flag.String("config", "", "holdem: betting-tree config (poker/treesize format)")
	abs := flag.String("abs", "", "holdem: bucket file from cmd/buckets, or \"category\"")
	itersFlag := flag.String("iters", "1e3", "iterations (sampled first deals) to run in total")
	checkpointsFlag := flag.String("checkpoints", "", "comma-separated iteration counts to report at, besides the end")
	floor := flag.Float64("floor", math.Inf(-1), "the abstract player's regret floor (<= 0); 0 is regret matching+")
	seed := flag.Uint64("seed", 1, "random seed")
	workers := flag.Int("workers", 1, "training goroutines (1 is deterministic)")
	workersFile := flag.String("workers-file", "", "a file holding a thread count: when it is there, training and best response use that many from the next progress step on, instead of -workers and -exploit-workers")
	maxChoiceGB := flag.Float64("max-choice-gb", 8, "refuse a tree whose table of recorded choices is larger than this per worker (0: no limit)")
	progress := flag.Int64("progress", 0, "log a line to stderr every this many iterations (0: only at checkpoints)")
	exploit := flag.String("exploit", "", "at each checkpoint, exact exploitability of the served blueprint of: average, current or both")
	exploitBin := flag.String("exploit-bin", "", "measure with this cmd/exploit binary on the written files (needs -out) instead of this build's best response")
	exploitWorkers := flag.Int("exploit-workers", runtime.GOMAXPROCS(0), "goroutines for best response")
	out := flag.String("out", "", "directory for blueprint files and, beside each checkpoint's, the state file a run can be resumed from; nothing written if empty")
	resume := flag.String("resume", "", "a state file (state-N.cfrbr from -out) to go on from: same game, abstraction, seed and floor; the run continues at iteration N")
	flag.Parse()
	log.SetFlags(log.Ltime)

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
	measure := map[string]bool{"average": *exploit == "average" || *exploit == "both", "current": *exploit == "current" || *exploit == "both"}
	if *exploit != "" && !measure["average"] && !measure["current"] {
		log.Fatalf("-exploit: want average, current or both")
	}
	if *exploitBin != "" && *out == "" {
		log.Fatalf("-exploit-bin measures the written files: give -out")
	}

	g, closeGame, err := engine.OpenGame(*game, *config, *abs)
	if err != nil {
		log.Fatal(err)
	}
	defer closeGame()
	cfg := engine.DefaultCFRBRConfig()
	cfg.Seed, cfg.Workers, cfg.RegretFloor = *seed, *workers, float32(*floor)
	cfg.MaxChoiceBytes = int64(*maxChoiceGB * (1 << 30))
	trainer, err := engine.NewCFRBR(g, cfg)
	if err != nil {
		log.Fatal(err)
	}
	if *out != "" {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	if *resume != "" {
		if err := trainer.LoadState(*resume); err != nil {
			log.Fatal(err)
		}
		if trainer.Iterations() >= total {
			log.Fatalf("-resume: %s is at %d iterations, -iters asks for %d", *resume, trainer.Iterations(), total)
		}
		log.Printf("resumed from %s at %d iterations", *resume, trainer.Iterations())
	}
	resumedAt := trainer.Iterations()
	t := g.Tree
	log.Printf("%s: %d nodes, %d infosets, %d infoset-actions, buckets %v, abstraction %s",
		g.Rules.Name, len(t.Nodes), t.Infosets, t.Slots, t.NumBuckets, g.Abs.ID())
	log.Printf("sampled-subgame CFR-BR: %d workers, seed %d, regret floor %g, %.0f MB of recorded choices per worker",
		cfg.Workers, cfg.Seed, cfg.RegretFloor, float64(trainer.ChoiceBytes())/1e6)

	// retune applies -workers-file, if the file is there.
	retune := func() {
		if *workersFile == "" {
			return
		}
		text, err := os.ReadFile(*workersFile)
		if err != nil {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(text)))
		if err != nil || n < 1 {
			log.Printf("%s: %q is not a thread count; ignored", *workersFile, strings.TrimSpace(string(text)))
			return
		}
		if n != trainer.Config.Workers || n != *exploitWorkers {
			log.Printf("%s: %d threads from here on", *workersFile, n)
			trainer.SetWorkers(n)
			*exploitWorkers = n
		}
	}

	// exploitability measures one strategy's served blueprint, written to
	// path when there is an output directory.
	exploitability := func(bp *engine.Blueprint, path string) string {
		retune()
		start := time.Now()
		var expl float64
		if *exploitBin != "" {
			expl, err = runExploit(*exploitBin, path, *game, *config, *abs, *exploitWorkers)
			if err != nil {
				log.Fatal(err)
			}
		} else {
			expl, _ = engine.Exploitability(g, bp, *exploitWorkers)
		}
		log.Printf("  exploitability %.6f (%.1fs)", expl, time.Since(start).Seconds())
		return fmt.Sprintf("%.6f", expl)
	}

	fmt.Println("iterations\ttrain_seconds\tthread_seconds_per_iteration\treal_player_won\texploit_average\texploit_current\tworst_pass_mismatch")
	var trainSeconds, threadSeconds float64
	// The largest disagreement between an iteration's two passes so far
	// (see CFRBR.Run): the run's health check, reported at every row.
	var worstMismatch float64
	for _, stop := range stops {
		if stop <= resumedAt {
			continue // a checkpoint the resumed run had already passed
		}
		// What R won per hand since the last checkpoint, both seats averaged.
		var won float64
		from := trainer.Iterations()
		for trainer.Iterations() < stop {
			n := stop - trainer.Iterations()
			if *progress > 0 {
				n = min(n, *progress)
			}
			retune()
			start := time.Now()
			real, mismatch := trainer.Run(n)
			worstMismatch = max(worstMismatch, mismatch)
			seconds := time.Since(start).Seconds()
			trainSeconds += seconds
			threadSeconds += seconds * float64(trainer.Config.Workers)
			won += (real[0] + real[1]) / 2 * float64(n)
			if *progress > 0 {
				log.Printf("%d iterations, %.2f thread-seconds each, the real-card player won %.4f over the last %d (passes agree to %.0e)",
					trainer.Iterations(), seconds*float64(trainer.Config.Workers)/float64(n), (real[0]+real[1])/2, n, mismatch)
			}
		}
		done := trainer.Iterations()
		row := []string{strconv.FormatInt(done, 10), fmt.Sprintf("%.1f", trainSeconds), "", "", "", "", fmt.Sprintf("%.1e", worstMismatch)}
		if worstMismatch > 1e-9 {
			log.Printf("WARNING: an iteration's two passes disagreed by %.3e; they must cancel to rounding", worstMismatch)
		}
		if done > resumedAt {
			row[2] = fmt.Sprintf("%.2f", threadSeconds/float64(done-resumedAt))
		}
		if *out != "" {
			// First, before anything slow: from here the run can be
			// stopped and lose nothing up to this checkpoint.
			path := filepath.Join(*out, fmt.Sprintf("state-%d.cfrbr", done))
			if err := trainer.SaveState(path); err != nil {
				log.Fatal(err)
			}
			log.Printf("  wrote %s (resume with -resume)", path)
		}
		if done > from {
			row[3] = fmt.Sprintf("%.4f", won/float64(done-from))
		}
		for col, which := range []string{"average", "current"} {
			strat := trainer.AverageStrategy()
			if which == "current" {
				strat = trainer.CurrentStrategy()
			}
			if *out == "" && !measure[which] {
				continue
			}
			bp := trainer.Blueprint(strat)
			path := ""
			if *out != "" {
				path = filepath.Join(*out, fmt.Sprintf("blueprint-%s-%d.bp", which, done))
				if err := bp.WriteFile(path); err != nil {
					log.Fatal(err)
				}
				log.Printf("  wrote %s", path)
			}
			if measure[which] {
				row[4+col] = exploitability(bp, path)
			}
		}
		fmt.Println(strings.Join(row, "\t"))
	}
}

var exploitLine = regexp.MustCompile(`(?m)^exploitability\s+\S+ per hand \(([0-9.eE+-]+)\)`)

// runExploit measures a blueprint file with a cmd/exploit binary, keeps
// the binary's report next to the file, and returns the exploitability.
func runExploit(bin, path, game, config, abs string, workers int) (float64, error) {
	cmd := exec.Command(bin, "-game", game, "-config", config, "-abs", abs, "-blueprint", path, "-workers", strconv.Itoa(workers))
	cmd.Stderr = os.Stderr
	report, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("%s on %s: %w", bin, path, err)
	}
	if err := os.WriteFile(path+".exploit.txt", report, 0o644); err != nil {
		return 0, err
	}
	m := exploitLine.FindSubmatch(report)
	if m == nil {
		return 0, fmt.Errorf("%s on %s: no exploitability line in its output", bin, path)
	}
	return strconv.ParseFloat(string(m[1]), 64)
}

// parseCount reads an iteration count written as 2500 or 2.5e3.
func parseCount(s string) (int64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f != math.Trunc(f) || f > 1e18 {
		return 0, fmt.Errorf("%q is not an iteration count", s)
	}
	return int64(f), nil
}
