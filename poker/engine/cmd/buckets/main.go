// buckets builds the hold'em card abstraction file: k-means buckets on
// equity histograms for the flop, turn and river (see bucketing.go).
//
//	go run ./cmd/buckets -flop 10 -turn 10 -river 10 -out data/abs-10-10-10.bin
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

func main() {
	cfg := engine.DefaultBucketConfig()
	flag.IntVar(&cfg.Flop, "flop", cfg.Flop, "flop buckets")
	flag.IntVar(&cfg.Turn, "turn", cfg.Turn, "turn buckets")
	flag.IntVar(&cfg.River, "river", cfg.River, "river buckets")
	flag.IntVar(&cfg.Bins, "bins", cfg.Bins, "equity histogram bins (flop, turn)")
	flag.Uint64Var(&cfg.Seed, "seed", cfg.Seed, "k-means seed")
	flag.IntVar(&cfg.MaxIters, "iters", cfg.MaxIters, "k-means iterations per round, at most")
	flag.IntVar(&cfg.Patience, "patience", cfg.Patience, "iterations in a row that may fail to lower the cost before k-means stops (0 reproduces the files built before this flag)")
	flag.StringVar(&cfg.RiverFeature, "river-feature", "equity", "what river hands are clustered on: equity (one number) or ochs (equity against 8 groups of opponent hands)")
	flag.StringVar(&cfg.ReuseFlopTurn, "reuse-flop-turn", "", "copy the flop and turn tables from this bucket file instead of clustering them")
	flag.IntVar(&cfg.Workers, "workers", cfg.Workers, "goroutines")
	out := flag.String("out", "", "file to write (required)")
	flag.Parse()
	if *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	log.SetFlags(0)
	start := time.Now()
	logf := func(format string, args ...any) {
		log.Printf("[%6.1fs] %s", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
	}
	abs, err := engine.BuildTableAbstraction(cfg, logf)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := abs.WriteFile(*out); err != nil {
		log.Fatal(err)
	}
	// Check the file as it will be used: mapped back from disk.
	disk, err := engine.OpenTableAbstraction(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer disk.Close()
	if disk.ID() != abs.ID() {
		log.Fatalf("file reads back as %s, built %s", disk.ID(), abs.ID())
	}
	if err := disk.CheckSuitInvariance(1_000_000, cfg.Seed); err != nil {
		log.Fatalf("suit invariance: %v", err)
	}
	logf("wrote %s: %s, suit-invariant on 1,000,000 random deals", *out, disk.ID())
	for round, name := range []string{"preflop", "flop", "turn", "river"} {
		logf("%-7s bucket frequencies (%%): %s", name, percentages(disk.BucketFrequencies(round, 1_000_000, cfg.Seed)))
	}
}

func percentages(freq []float64) string {
	if len(freq) > 20 {
		lo, hi := freq[0], freq[0]
		for _, f := range freq {
			lo, hi = min(lo, f), max(hi, f)
		}
		return fmt.Sprintf("%d buckets, min %.3f, max %.3f", len(freq), 100*lo, 100*hi)
	}
	s := ""
	for _, f := range freq {
		s += fmt.Sprintf("%.1f ", 100*f)
	}
	return s
}
