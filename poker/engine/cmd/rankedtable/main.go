// Command rankedtable plays one ranked table and writes its record, or
// prices a record written before.
//
//	rankedtable play -challenger shark -out table.json   # the defaults: 9 seats from the house pool, 1,000,000 hands a pair
//	rankedtable play -challenger dove -seats 3 -hands 20000 -seed 7
//	rankedtable cost table.json                          # at Cloud Run job prices
//	rankedtable cost -vcpu 8 -gib 4 table.json
//
// play draws the seats from -pool (copies allowed), plays every pair
// heads-up on the same duplicate deals, and writes the record (JSON,
// package league) to -out, then prints the places and the cost to stderr.
// The house pool's trained bots must be in the house-bot cache: go run
// ./sdk/cmd/house download all. The challenger is a pool bot by id; a
// submitted bot joins through league.Play.
//
// Run it from poker/engine, the Go module's root.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/open-bot-gaming-league/league/poker/engine/league"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: rankedtable play [flags] | rankedtable cost [flags] record.json")
	}
	var err error
	switch os.Args[1] {
	case "play":
		err = play(os.Args[2:], os.Stdout, os.Stderr)
	case "cost":
		err = cost(os.Args[2:], os.Stdout)
	default:
		err = fmt.Errorf("no command %q: play or cost", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func play(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("play", flag.ContinueOnError)
	challenger := fs.String("challenger", "", "the challenger: a pool bot's id ("+strings.Join(league.HousePool, ", ")+")")
	pool := fs.String("pool", strings.Join(league.HousePool, ","), "the bots seats are drawn from, by id, comma-separated")
	seats := fs.Int("seats", league.DefaultSeats, "seats drawn from the pool, besides the challenger")
	hands := fs.Int("hands", league.DefaultHands, "hands per pair")
	seed := fs.Uint64("seed", 1, "draws the seats, deals the cards and seeds the bots: the same seed plays the same table")
	parallel := fs.Int("parallel", 0, "pairs played at once (0: GOMAXPROCS)")
	out := fs.String("out", "-", "where to write the record (-: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *challenger == "" || fs.NArg() > 0 {
		return errors.New("play: -challenger is required, and takes no arguments")
	}
	bots, closePool, err := league.Pool(split(*pool)...)
	if err != nil {
		return err
	}
	defer closePool()
	ch, closeCh, err := league.Pool(*challenger)
	if err != nil {
		return err
	}
	defer closeCh()
	cfg := league.Config{Seats: *seats, Hands: *hands, Seed: *seed, Parallel: *parallel, Runner: league.RunnerVersion()}
	rec, err := league.Play(cfg, ch[0], bots)
	if err != nil {
		return err
	}
	b, err := rec.JSON()
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err = stdout.Write(b)
	} else {
		err = os.WriteFile(*out, b, 0o644)
	}
	if err != nil {
		return err
	}
	for _, p := range rec.Places {
		fmt.Fprintf(stderr, "%2d. seat %d %-8s %+8.1f bb/100 total, beat %d, lost to %d\n", p.Place, p.Seat, p.Name, p.Total, p.Won, p.Lost)
	}
	c, err := league.CostOf(rec, league.CloudRunJobs, league.Task{})
	if err != nil {
		return err
	}
	fmt.Fprint(stderr, c)
	return nil
}

func cost(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cost", flag.ContinueOnError)
	vcpu := fs.Float64("vcpu", 0, "the job task's vCPUs (0: the pairs the table played at once)")
	gib := fs.Float64("gib", 0, "the job task's memory, GiB (0: the peak resident memory, rounded up to a whole GiB)")
	p := league.CloudRunJobs
	fs.Float64Var(&p.VCPUSecond, "vcpu-price", p.VCPUSecond, "USD per vCPU-second")
	fs.Float64Var(&p.GiBSecond, "gib-price", p.GiBSecond, "USD per GiB-second")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("cost: one record file")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	rec, err := league.ReadRecord(b)
	if err != nil {
		return fmt.Errorf("%s: %w", fs.Arg(0), err)
	}
	if p != league.CloudRunJobs {
		p.Source = "given on the command line"
	}
	c, err := league.CostOf(rec, p, league.Task{VCPU: *vcpu, GiB: *gib})
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(stdout, c)
	return err
}

func split(list string) []string {
	var ids []string
	for _, id := range strings.Split(list, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
