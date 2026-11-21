// scaleraise writes a copy of a blueprint with its preflop bets and
// raises made m times as likely (engine.PreflopRaiseScaled): each preflop
// infoset's raise probabilities, the all-in included, are multiplied by m
// and the infoset renormalised; flop, turn and river are copied as they
// are. The result is an ordinary blueprint file, so cmd/match, cmd/exploit
// and cmd/table take it; its header says how it was derived.
//
//	go run ./cmd/scaleraise -config configs/table_50bb.json -abs data/abs-200-200-200.bin \
//	    -in runs/plain.bp -m 1.5 -out runs/plain-raise1.5.bp
//
// A hand-made change to a trained strategy: nothing says the result is
// less exploitable than the original, and the expectation is the reverse.
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

func main() {
	config := flag.String("config", "", "betting-tree config (poker/treesize format)")
	abs := flag.String("abs", "", "the blueprint's bucket file")
	in := flag.String("in", "", "blueprint to start from")
	out := flag.String("out", "", "blueprint file to write")
	m := flag.Float64("m", 1, "factor for every preflop bet and raise probability (>= 0; 1 copies the blueprint)")
	flag.Parse()
	log.SetFlags(0)
	if *in == "" || *out == "" || *m < 0 {
		log.Fatal("usage: scaleraise -config c.json -abs buckets.bin -in a.bp -m 1.5 -out b.bp")
	}
	g, closeGame, err := engine.OpenGame("holdem", *config, *abs)
	if err != nil {
		log.Fatal(err)
	}
	defer closeGame()
	src, err := engine.OpenBlueprint(*in, g)
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()
	bp := engine.NewBlueprint(g, engine.PreflopRaiseScaled{Of: src, Tree: g.Tree, M: *m})
	derived := fmt.Sprintf("preflop bet and raise probabilities x %g, renormalised (cmd/scaleraise)", *m)
	if src.Header.Derived != "" {
		derived = src.Header.Derived + "; then " + derived
	}
	bp.Header = src.Header
	bp.Header.Derived = derived
	if err := bp.WriteFile(*out); err != nil {
		log.Fatal(err)
	}
	same := "differ from"
	if bp.Equal(src) {
		same = "are the same as"
	}
	log.Printf("wrote %s: %s; its strategy bytes %s the source's", *out, derived, same)
}
