// Command rankedtable plays one ranked table and writes its record, or
// prices a record written before.
//
//	rankedtable play -challenger shark -out table.json   # the defaults: 10 seats from the house pool, 1,000,000 hands a pair
//	rankedtable play -challenger dove -seats 3 -hands 20000 -seed 7
//	rankedtable play -challenger-wasm mybot.wasm -challenger-data mydata -wasm-pool   # every seat in the sandbox
//	rankedtable play -challenger-spec challenger.json -wasm-pool -guests guests -seed 7 -out table.json   # a submitted bot, as the queue runs it
//	rankedtable cost table.json                          # at Cloud Run job prices
//	rankedtable cost -vcpu 8 -gib 4 table.json
//
// play draws the seats from -pool (-draw; copies allowed), plays every pair
// heads-up on the same duplicate deals, and writes the record (JSON,
// package league) to -out, then prints the places and the cost to stderr.
// The house pool's trained bots must be in the house-bot cache: go run
// ./sdk/cmd/house download all (in poker/engine). The challenger is a pool
// bot by id, or a wasm bot (-challenger-wasm, with its data directory
// mounted read-only as -challenger-data) run in the arena sandbox, or a
// wasm bot described by a JSON file (-challenger-spec: its module, its
// data files and the host file each is read from, whose bot it is; see
// spec), as the ranked-table queue (mcp/ranked.py) runs a submitted bot.
// The pool runs natively, or with -wasm-pool as wasm bots from -guests
// (prebuilt <id>.wasm files, else built from the Go packages there):
// then every seat is sandboxed and metered.
//
// Run it from poker/arena, the Go module's root.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/open-bot-gaming-league/league/poker/arena"
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
	challengerWasm := fs.String("challenger-wasm", "", "the challenger: a wasm bot module (instead of -challenger)")
	challengerData := fs.String("challenger-data", "", "the wasm challenger's data directory, mounted read-only at /data")
	challengerName := fs.String("challenger-name", "Challenger", "the wasm challenger's name in the record")
	challengerSpec := fs.String("challenger-spec", "", "the challenger: a wasm bot described by a JSON file (instead of -challenger; see spec)")
	wasmPool := fs.Bool("wasm-pool", false, "run the pool as wasm bots in the sandbox, built from -guests")
	guests := fs.String("guests", "guests", "the house bots' wasm guest packages, for -wasm-pool")
	pool := fs.String("pool", strings.Join(league.HousePool, ","), "the bots seats are drawn from, by id, comma-separated")
	seats := fs.Int("seats", league.DefaultSeats, "seats drawn from the pool, besides the challenger")
	hands := fs.Int("hands", league.DefaultHands, "hands per pair")
	seed := fs.Uint64("seed", 1, "draws the seats, deals the cards and seeds the bots: the same seed plays the same table")
	draw := fs.String("draw", league.DrawCopies, "how the seats are drawn from the pool (league.Draws)")
	parallel := fs.Int("parallel", 0, "pairs played at once (0: GOMAXPROCS)")
	out := fs.String("out", "-", "where to write the record (-: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if given := len(slices.DeleteFunc([]string{*challenger, *challengerWasm, *challengerSpec}, func(s string) bool { return s == "" })); given != 1 || fs.NArg() > 0 {
		return errors.New("play: one of -challenger, -challenger-wasm and -challenger-spec is required, and play takes no arguments")
	}
	var bots []league.Entrant
	var err error
	if *wasmPool {
		if bots, err = arena.WasmPool(*guests, split(*pool)...); err != nil {
			return err
		}
	} else {
		var closePool func() error
		if bots, closePool, err = league.Pool(split(*pool)...); err != nil {
			return err
		}
		defer closePool()
	}
	if len(bots) == 0 {
		return errors.New("play: an empty pool")
	}
	var ch league.Entrant
	switch {
	case *challengerSpec != "":
		if ch, err = specChallenger(*challengerSpec); err != nil {
			return err
		}
	case *challengerWasm != "":
		var closeCh func() error
		if ch, closeCh, err = wasmChallenger(*challengerWasm, *challengerData, *challengerName); err != nil {
			return err
		}
		defer closeCh()
	case *wasmPool:
		chs, err := arena.WasmPool(*guests, *challenger)
		if err != nil {
			return err
		}
		ch = chs[0]
	default:
		chs, closeCh, err := league.Pool(*challenger)
		if err != nil {
			return err
		}
		defer closeCh()
		ch = chs[0]
	}
	cfg := league.Config{Seats: *seats, Hands: *hands, Seed: *seed, Parallel: *parallel, Runner: league.RunnerVersion(), Draw: *draw}
	rec, err := league.Play(cfg, ch, bots)
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

// wasmChallenger is a wasm bot module, and its data directory if any, as
// the challenger. Its version is the module's sha256 (the data is named,
// not hashed).
func wasmChallenger(path, dataDir, name string) (league.Entrant, func() error, error) {
	wasm, err := os.ReadFile(path)
	if err != nil {
		return league.Entrant{}, nil, err
	}
	p, err := arena.Compile(wasm)
	if err != nil {
		return league.Entrant{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	version, cfg, done := "wasm sha256:"+p.SHA256(), arena.Config{}, func() error { return nil }
	if dataDir != "" {
		root, err := os.OpenRoot(dataDir)
		if err != nil {
			return league.Entrant{}, nil, err
		}
		cfg.Data, done = root.FS(), root.Close
		version += " data " + filepath.Base(dataDir)
	}
	return arena.Entrant("challenger", name, version, p, cfg), done, nil
}

// spec is a submitted bot as -challenger-spec describes it: its module,
// its data files as a mount (arena.Files: each path of the bot's /data and
// the host file it is read from), and whose it is.
type spec struct {
	Name    string            `json:"name"`
	Version string            `json:"version"` // "": the module's sha256
	Wasm    string            `json:"wasm"`    // the module's host path
	Data    map[string]string `json:"data"`
	Owner   *league.Owner     `json:"owner"`
}

func specChallenger(path string) (league.Entrant, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return league.Entrant{}, err
	}
	var s spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return league.Entrant{}, fmt.Errorf("%s: %w", path, err)
	}
	if s.Name == "" || s.Wasm == "" {
		return league.Entrant{}, fmt.Errorf("%s: a challenger needs a name and a wasm module", path)
	}
	wasm, err := os.ReadFile(s.Wasm)
	if err != nil {
		return league.Entrant{}, err
	}
	p, err := arena.Compile(wasm)
	if err != nil {
		return league.Entrant{}, fmt.Errorf("%s: %w", s.Wasm, err)
	}
	if s.Version == "" {
		s.Version = "wasm sha256:" + p.SHA256()
	}
	for name := range s.Data {
		if !iofs.ValidPath(name) || name == "." {
			return league.Entrant{}, fmt.Errorf("%s: data path %q", path, name)
		}
	}
	var cfg arena.Config
	if len(s.Data) > 0 {
		cfg.Data = arena.Files(s.Data)
	}
	e := arena.Entrant("challenger", s.Name, s.Version, p, cfg)
	e.Owner = s.Owner
	return e, nil
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
