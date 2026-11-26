// Command matrix plays every pair of the given bots against each other and
// prints who beats whom: a head-to-head table, bb/100 ± SE for the row bot
// against the column bot (duplicate deals, the same seed for every pair).
//
//	go run ./sdk/cmd/matrix                              # the three starter bots
//	go run ./sdk/cmd/matrix -bots dove,shark -hands 1000000
//	go run ./sdk/cmd/matrix -house turtle=abs-200-200-200.bin,turtle-k200-1500.bp
//
// Your own bot: go run ./sdk/example -matrix puts the example bot in the
// table with the starters; for a bot of your own, add one line to roster
// below (or call sdk.Matrix from your program).
//
// Run it from poker/engine, the Go module's root.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// roster is every bot -bots can name, by id: the starter bots. Add yours
// here in one line, e.g. "mine": func() sdk.Bot { return mybot.Bot{} }.
var roster = func() map[string]func() sdk.Bot {
	r := map[string]func() sdk.Bot{}
	for id, newBot := range bots.ByName {
		r[id] = newBot
	}
	return r
}()

// houses is the -house flag: name=buckets,blueprint, as often as given.
type houses []string

func (h *houses) String() string     { return strings.Join(*h, " ") }
func (h *houses) Set(v string) error { *h = append(*h, v); return nil }

func main() {
	list := flag.String("bots", "dove,clam,shark", "bots to play, by id, comma-separated")
	hands := flag.Int("hands", 20000, "hands per pair; 1,000,000 separates close bots (SE about 0.5 to 1 bb/100; it takes a while)")
	seed := flag.Uint64("seed", 1, "seed: the same seed plays the same cards, so everyone gets the same table")
	var house houses
	flag.Var(&house, "house", "a house bot whose files are on disk, as name=buckets,blueprint (e.g. turtle=abs-200-200-200.bin,turtle-k200-1500.bp); repeat for more")
	flag.Parse()
	log.SetFlags(0)
	cs, err := contenders(*list, house)
	if err != nil {
		log.Fatal(err)
	}
	t, err := sdk.Matrix(cs, *hands, *seed)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("bb/100 for the row bot against the column bot, %d hands a pair, seed %d\n\n", *hands, *seed)
	fmt.Fprint(os.Stdout, t)
}

// contenders is the bots named in list, then the house bots.
func contenders(list string, house []string) ([]sdk.Contender, error) {
	var cs []sdk.Contender
	for _, id := range strings.Split(list, ",") {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		newBot, ok := roster[id]
		if !ok {
			return nil, fmt.Errorf("no bot %q (have: dove, clam, shark, and any you add to roster)", id)
		}
		name := bots.Names[id]
		if name == "" {
			name = id
		}
		cs = append(cs, sdk.Contender{Name: name, New: func() (sdk.Bot, error) { return newBot(), nil }})
	}
	for _, spec := range house {
		name, files, ok := strings.Cut(spec, "=")
		buckets, blueprint, ok2 := strings.Cut(files, ",")
		if !ok || !ok2 || name == "" || buckets == "" || blueprint == "" {
			return nil, fmt.Errorf("-house %q: want name=buckets,blueprint", spec)
		}
		if _, err := os.Stat(blueprint); err != nil {
			return nil, fmt.Errorf("-house %s: %w", name, err)
		}
		cs = append(cs, sdk.Contender{Name: name, New: func() (sdk.Bot, error) { return sdk.LoadHouse(buckets, blueprint) }})
	}
	if len(cs) < 2 {
		return nil, fmt.Errorf("a matrix needs at least two bots, not %d", len(cs))
	}
	return cs, nil
}
