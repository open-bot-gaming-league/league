// Command try plays a wasm bot the way the league runs it, in the arena
// sandbox with the ranked table's limits, against the starter bots (and
// the house bots, if downloaded), and prints its row of the matrix and
// what the sandbox saw: faults, forfeits, fuel, memory, reads, output.
//
//	go run ./cmd/try mybot.wasm
//	go run ./cmd/try mybot.wasm -vs dove,clam,shark,turtle,ox -hands 20000 -data ./mydata
//
// Build the bot first (see package wasmbot):
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o mybot.wasm .
//
// Run it from poker/arena, the Go module's root.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/open-bot-gaming-league/league/poker/arena"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

func main() {
	log.SetFlags(0)
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// outputShown is how much of the bot's output is printed.
const outputShown = 2 << 10

func run(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("try", flag.ContinueOnError)
	vs := fs.String("vs", "dove,clam,shark", "opponents, by id, comma-separated: dove, clam, shark, and turtle, ox once downloaded")
	hands := fs.Int("hands", 20_000, "hands per opponent (every deal is played twice, seats swapped)")
	seed := fs.Uint64("seed", 1, "seed: the same seed plays the same cards and the same random choices")
	data := fs.String("data", "", "the bot's data directory, mounted read-only at /data")
	// The file may come before the flags or after them.
	var path string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if path == "" && fs.NArg() == 1 {
		path = fs.Arg(0)
	} else if path == "" || fs.NArg() > 0 {
		return errors.New("usage: try bot.wasm [-vs ids] [-hands n] [-seed n] [-data dir]")
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	p, err := arena.Compile(wasm)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	cfg := arena.Config{}
	if *data != "" {
		root, err := os.OpenRoot(*data)
		if err != nil {
			return err
		}
		defer root.Close()
		cfg.Data = root.FS()
	}
	lim := arena.DefaultLimits()
	fmt.Fprintf(w, "%s (sha256 %.12s), %d hands an opponent, seed %d\n", path, p.SHA256(), *hands, *seed)
	fmt.Fprintf(w, "limits: %d MiB memory; %.3g fuel a decision at most, the budget growing %.3g a decision; %v a decision; load %.3g fuel, %v\n\n",
		lim.MemoryBytes>>20, float64(lim.DecisionFuel), float64(lim.FuelPerDecision), lim.DecisionTimeout, float64(lim.StartFuel), lim.StartTimeout)
	var shown bool
	for _, id := range split(*vs) {
		opp, name, done, err := opponent(id)
		if err != nil {
			return err
		}
		b, err := p.Start(cfg)
		if err != nil {
			done()
			return err
		}
		start := time.Now()
		res, err := sdk.Match(b, opp, *hands, *seed)
		wall := time.Since(start)
		r := b.Report()
		b.Close()
		done()
		if err != nil {
			return fmt.Errorf("vs %s: %w", name, err)
		}
		fmt.Fprintf(w, "vs %-7s %+7.1f ± %.1f bb/100  (%v)\n", name, res.BBPer100, res.SE, wall.Round(time.Millisecond))
		report(w, r)
		if !shown && len(r.Output) > 0 {
			defer func(out []byte, total int64) {
				fmt.Fprintf(w, "\nthe bot's output, vs %s (first %d of %d bytes):\n%s\n", name, min(len(out), outputShown), total, out[:min(len(out), outputShown)])
			}(r.Output, r.OutputBytes)
			shown = true
		}
	}
	return nil
}

func report(w io.Writer, r arena.Report) {
	if r.Fault != nil {
		fmt.Fprintf(w, "    STOPPED: %v\n", r.Fault)
	}
	if r.Forfeited() > 0 {
		fmt.Fprintf(w, "    %d of %d decisions played for it (%d illegal, %d errors); first: %s\n", r.Forfeited(), r.Decisions, r.Illegal, r.Errors, r.FirstError)
	}
	perDecision := uint64(0)
	if r.Decisions > 0 {
		perDecision = r.Fuel / uint64(r.Decisions)
	}
	fmt.Fprintf(w, "    fuel %.3g a decision (most %.3g), load %.3g in %v; memory %d MiB; read %d B in %d calls; output %d B\n",
		float64(perDecision), float64(r.MaxDecisionFuel), float64(r.StartFuel), r.StartTime.Round(time.Millisecond),
		r.MemoryBytes>>20, r.ReadBytes, r.ReadCalls, r.OutputBytes)
}

// opponent is a native starter or house bot.
func opponent(id string) (sdk.Bot, string, func(), error) {
	if newBot, ok := bots.ByName[id]; ok {
		return newBot(), bots.Names[id], func() {}, nil
	}
	h, err := sdk.LoadHouseByID(id)
	if err != nil {
		return nil, "", nil, err
	}
	return h, sdk.Houses().Bots[id].Name, func() { h.Close() }, nil
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
