package sdk

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

// testHouse writes a blueprint for the table's tree on the file-free
// category abstraction and loads it as a house bot.
func testHouse(t *testing.T, strat engine.Strategy) *House {
	t.Helper()
	g, _, err := engine.OpenHoldem(engine.TableConfig(), "category")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.bp")
	if err := engine.NewBlueprint(g, strat).WriteFile(path); err != nil {
		t.Fatal(err)
	}
	h, err := LoadHouse("category", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// A house bot plays every kind of spot (Random bets any size, so the
// bot's view reads sizes off its menu), against itself (one House in
// both seats), and the same seed plays the same match.
func TestHouse(t *testing.T) {
	h := testHouse(t, engine.UniformStrategy{})
	a, err := Match(h, Random{}, 2000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := Match(h, Random{}, 2000, 1); a != b {
		t.Fatalf("seed 1 twice: %v, then %v", a, b)
	}
	if _, err := Match(h, h, 500, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := Match(Random{}, h, 500, 3); err != nil {
		t.Fatal(err)
	}
}

func TestLoadHouseRefusesAnotherGame(t *testing.T) {
	cfg := engine.TableConfig()
	cfg.Opening = []float64{1}
	g, err := engine.NewHoldem(cfg, engine.CategoryAbstraction{Ranks: 13})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "other.bp")
	if err := engine.NewBlueprint(g, engine.UniformStrategy{}).WriteFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHouse("category", path); err == nil {
		t.Fatal("a blueprint for another bet menu loaded")
	}
}

// The served house bots, when their files are on this machine:
// OBGL_HOUSE_BUCKETS and OBGL_HOUSE_BLUEPRINT name them.
func TestServedHouse(t *testing.T) {
	buckets, blueprint := os.Getenv("OBGL_HOUSE_BUCKETS"), os.Getenv("OBGL_HOUSE_BLUEPRINT")
	if buckets == "" || blueprint == "" {
		t.Skip("OBGL_HOUSE_BUCKETS and OBGL_HOUSE_BLUEPRINT are not set")
	}
	h, err := LoadHouse(buckets, blueprint)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	res, err := Match(h, Random{}, 2000, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("house vs Random: %v", res)
	if res.BBPer100 < 0 {
		t.Errorf("a trained bot loses to random play: %v", res)
	}
}

// A house bot here makes the move the table server's bot would make in
// the same spot with the same random draws: the server's way, a
// TreeView on the real hand (cmd/table botActs), is run alongside.
func TestHouseMatchesTheTable(t *testing.T) {
	house := testHouse(t, engine.UniformStrategy{})
	g, err := game()
	if err != nil {
		t.Fatal(err)
	}
	deck := rand.New(rand.NewPCG(9, 0))
	opp := rand.New(rand.NewPCG(9, 1))
	decisions := 0
	for n := 1; n <= 300; n++ {
		var cards [engine.TableCards]engine.Card
		for i, c := range deck.Perm(52)[:engine.TableCards] {
			cards[i] = engine.Card(c)
		}
		h, _ := engine.NewTableHand(g, cards)
		seat := n % 2 // the house bot's
		direct := engine.NewTreeView(house.g)
		for !h.Over() {
			if h.ToAct() != seat {
				if err := apply(h, Random{}.Act(stateOf(g, h, h.ToAct(), n, opp))); err != nil {
					t.Fatal(err)
				}
				continue
			}
			seed := uint64(n)<<32 | uint64(len(h.Log))
			got := house.Act(stateOf(g, h, seat, n, rand.New(rand.NewPCG(seed, 0))))
			twin := *h
			twin.Log = slices.Clone(h.Log)
			if err := direct.Act(&twin, house.bp, rand.New(rand.NewPCG(seed, 0)).Uint32); err != nil {
				t.Fatal(err)
			}
			want := twin.Log[len(twin.Log)-1]
			if got != (Action{Kind: kindOf(want.Kind), To: want.To}) {
				t.Fatalf("hand %d, move %d: the house bot plays %v, the table's would play %v to %d", n, len(h.Log), got, want.Kind, want.To)
			}
			if err := apply(h, got); err != nil {
				t.Fatal(err)
			}
			decisions++
		}
	}
	if decisions < 500 {
		t.Fatalf("only %d decisions compared", decisions)
	}
}
