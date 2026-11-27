// Command example is a bot to start from. Edit myBot.Act, then:
//
//	go run ./sdk/example                  # 2,000 hands against each starter bot
//	go run ./sdk/example -vs clam -hands 20000 -seed 7
//	go run ./sdk/example -matrix -hands 20000   # myBot and the starters, every pair: who beats whom
//	go run ./sdk/example -house turtle,ox  # after: go run ./sdk/cmd/house download all
//	go run ./sdk/example -table -hands 50  # play your table (token: obgl-token.txt or OBGL_TOKEN); watch it in the browser
//
// Run it from poker/engine, the Go module's root.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// myBot is yours to change. It plays pairs, aces and two big cards
// before the flop, and any pair after it. Run the matrix to see what it
// beats (go run ./sdk/example -matrix). Measure each change against your
// last version (the sdk README's first example), and against a house bot
// if you have its files, to learn whether it helps.
type myBot struct{}

func (myBot) Act(s sdk.State) sdk.Action {
	if s.Round == sdk.Preflop {
		hi := max(s.Hole[0].Rank(), s.Hole[1].Rank())
		lo := min(s.Hole[0].Rank(), s.Hole[1].Rank())
		switch {
		case s.Made() == sdk.Pair || hi == sdk.Ace || lo >= sdk.Ten:
			if s.Facing() && s.InRound > s.BigBlind {
				return s.CheckOrCall() // we raised and were raised back: just call
			}
			return s.RaisePot(1) // a pot-sized raise: 3 bb from the small blind
		default:
			return s.CheckOrFold()
		}
	}
	if s.Made() >= sdk.Pair {
		if s.Facing() {
			return s.CheckOrCall()
		}
		return s.RaisePot(0.5) // a half-pot bet
	}
	return s.CheckOrFold()
}

func main() {
	vs := flag.String("vs", "dove,clam,shark", "starter bots to play, by id, comma-separated")
	hands := flag.Int("hands", 2000, "hands per match (every deal is played twice, seats swapped)")
	seed := flag.Uint64("seed", 1, "seed: the same seed plays the same cards and the same random choices")
	houseIDs := flag.String("house", "", "downloaded house bots to play too, comma-separated: turtle,ox (go run ./sdk/cmd/house download all)")
	houseBuckets := flag.String("house-buckets", "", "a house bot's bucket file, already on disk (with -house-blueprint)")
	houseBlueprint := flag.String("house-blueprint", "", "a house bot's blueprint, already on disk (with -house-buckets)")
	matrix := flag.Bool("matrix", false, "play every pair of myBot and the -vs bots (and the house bot), and print who beats whom")
	table := flag.Bool("table", false, "play -hands hands at your table instead (the token: OBGL_TOKEN, or obgl-token.txt here or above; OBGL_TABLE, default the league's)")
	flag.Parse()
	log.SetFlags(0)

	if *table {
		t, err := sdk.TableFromEnv()
		if err != nil {
			log.Fatal(err)
		}
		res, err := t.Play(context.Background(), myBot{}, *hands, *seed, func(h sdk.HandResult) {
			fmt.Printf("hand %d: %+d  %s\n", h.Hand, h.Net, h.Outcome)
		})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("at %s (session %s): %v\n", t.URL, t.Session, res)
		return
	}

	type houseBot struct {
		name string
		bot  *sdk.House
	}
	var houses []houseBot
	for _, id := range strings.Split(*houseIDs, ",") {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		h, err := sdk.LoadHouseByID(id)
		if err != nil {
			log.Fatal(err)
		}
		defer h.Close()
		houses = append(houses, houseBot{sdk.Houses().Bots[id].Name, h})
	}
	switch {
	case *houseBuckets != "" && *houseBlueprint != "":
		h, err := sdk.LoadHouse(*houseBuckets, *houseBlueprint)
		if err != nil {
			log.Fatal(err)
		}
		defer h.Close()
		houses = append(houses, houseBot{"house bot", h})
	case *houseBuckets != "" || *houseBlueprint != "":
		log.Fatal("a house bot needs both -house-buckets and -house-blueprint")
	}

	if *matrix {
		cs := []sdk.Contender{{Name: "myBot", New: func() (sdk.Bot, error) { return myBot{}, nil }}}
		for _, id := range strings.Split(*vs, ",") {
			newBot, ok := bots.ByName[strings.TrimSpace(id)]
			if !ok {
				log.Fatalf("no starter bot %q (dove, clam, shark)", id)
			}
			cs = append(cs, sdk.Contender{Name: bots.Names[strings.TrimSpace(id)], New: func() (sdk.Bot, error) { return newBot(), nil }})
		}
		for _, h := range houses { // one load, a clone per pair
			cs = append(cs, sdk.Contender{Name: h.name, New: func() (sdk.Bot, error) { return h.bot.Clone(), nil }})
		}
		t, err := sdk.Matrix(cs, *hands, *seed)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(t)
		return
	}

	type opponent struct {
		name string
		bot  sdk.Bot
	}
	var opps []opponent
	for _, name := range strings.Split(*vs, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		newBot, ok := bots.ByName[name]
		if !ok {
			log.Fatalf("no starter bot %q (dove, clam, shark)", name)
		}
		opps = append(opps, opponent{bots.Names[name], newBot()})
	}
	for _, h := range houses {
		opps = append(opps, opponent{h.name, h.bot})
	}
	for _, o := range opps {
		res, err := sdk.Match(myBot{}, o.bot, *hands, *seed)
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}
		fmt.Printf("myBot vs %-9s %v\n", o.name, res)
	}
}
