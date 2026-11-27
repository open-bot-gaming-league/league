// Command matrix plays every pair of the given bots against each other and
// prints who beats whom: a head-to-head table, bb/100 ± SE for the row bot
// against the column bot (duplicate deals, the same seed for every pair).
//
//	go run ./sdk/cmd/matrix                              # the three starter bots
//	go run ./sdk/cmd/matrix -bots dove,shark -hands 1000000
//	go run ./sdk/cmd/matrix -house turtle -house ox  # after: go run ./sdk/cmd/house download all
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
	flag.Var(&house, "house", "a house bot: turtle or ox once downloaded (go run ./sdk/cmd/house download all), or name=buckets,blueprint for files of your own; repeat for more")
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

// openHouse loads a -house: a downloaded house bot by id ("turtle"), or
// name=buckets,blueprint for files of your own.
func openHouse(spec string) (*sdk.House, string, error) {
	name, files, explicit := strings.Cut(spec, "=")
	if !explicit {
		h, err := sdk.LoadHouseByID(spec)
		if err != nil {
			return nil, "", err
		}
		return h, sdk.Houses().Bots[spec].Name, nil
	}
	buckets, blueprint, ok := strings.Cut(files, ",")
	if !ok || name == "" || buckets == "" || blueprint == "" {
		return nil, "", fmt.Errorf("-house %q: want a house bot's id (turtle, ox) or name=buckets,blueprint", spec)
	}
	h, err := sdk.LoadHouse(buckets, blueprint)
	if err != nil {
		return nil, "", fmt.Errorf("-house %s: %w", name, err)
	}
	return h, name, nil
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
		h, name, err := openHouse(spec)
		if err != nil {
			return nil, err
		}
		// One load per run, shared: each pair plays a Clone (its own hands,
		// the same files). On Windows a load reads the files whole.
		cs = append(cs, sdk.Contender{Name: name, New: func() (sdk.Bot, error) { return h.Clone(), nil }})
	}
	if len(cs) < 2 {
		return nil, fmt.Errorf("a matrix needs at least two bots, not %d", len(cs))
	}
	return cs, nil
}
