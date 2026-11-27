package sdk

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

// TestLegalAllowsWhatTheEngineAccepts: at every decision of random hands,
// Allows agrees with the engine on a spread of moves, legal and not.
func TestLegalAllowsWhatTheEngineAccepts(t *testing.T) {
	g, err := game()
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(3, 4))
	checked := 0
	for hand := range 300 {
		var cards [engine.TableCards]engine.Card
		for i, c := range r.Perm(g.Rules.Deck)[:engine.TableCards] {
			cards[i] = engine.Card(c)
		}
		h, err := engine.NewTableHand(g, cards)
		if err != nil {
			t.Fatal(err)
		}
		for !h.Over() {
			s := stateOf(g, h, h.ToAct(), hand+1, r)
			l := s.Legal
			for _, a := range []Action{
				{Kind: Fold}, {Kind: Check}, {Kind: Call}, {Kind: Kind(4)}, {Kind: Kind(-1)},
				{Kind: Raise, To: l.MinTo - 1}, {Kind: Raise, To: l.MinTo}, {Kind: Raise, To: l.MaxTo},
				{Kind: Raise, To: l.MaxTo + 1}, {Kind: Raise, To: 0},
			} {
				c := *h
				c.Log = slices.Clip(h.Log)
				if engineOK := apply(&c, a) == nil; engineOK != l.Allows(a) {
					t.Fatalf("hand %d, %v with %+v: Allows says %v, the engine %v", hand, a, l, l.Allows(a), engineOK)
				}
				checked++
			}
			if err := apply(h, Random{}.Act(s)); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("%d moves checked", checked)
}
