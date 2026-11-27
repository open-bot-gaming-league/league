package arena

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// TestHouseBotsPlayAsNative: the five house bots as wasm win exactly the
// chips their native selves win, in every pairing. Turtle and Ox are one
// module with different data; they need the downloaded house files
// (go run ./sdk/cmd/house download all, in poker/engine), and the test
// skips without them. PARITY_HANDS sets the hands a pairing (default 10,000).
func TestHouseBotsPlayAsNative(t *testing.T) {
	if testing.Short() {
		t.Skip("plays 10 matches")
	}
	dir, err := sdk.HouseCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	m := sdk.Houses()
	native := map[string]func() (sdk.Bot, func()){}
	wasm := map[string]func() (*Program, Config){}
	for _, name := range []string{"dove", "clam", "shark"} {
		native[name] = func() (sdk.Bot, func()) { return bots.ByName[name](), func() {} }
		wasm[name] = func() (*Program, Config) { return guest(t, name), Config{} }
	}
	for _, id := range []string{"turtle", "ox"} {
		hb := m.Bots[id]
		if _, err := os.Stat(filepath.Join(dir, hb.Buckets)); err != nil {
			t.Skipf("%s is not downloaded (%v)", hb.Name, err)
		}
		native[id] = func() (sdk.Bot, func()) {
			h, err := sdk.LoadHouseByID(id)
			if err != nil {
				t.Fatal(err)
			}
			return h, func() { h.Close() }
		}
		data := Files{"buckets.bin": filepath.Join(dir, hb.Buckets), "blueprint.bp": filepath.Join(dir, hb.Blueprint)}
		wasm[id] = func() (*Program, Config) { return guest(t, "house"), Config{Data: data} }
	}
	hands := 10_000
	if s := os.Getenv("PARITY_HANDS"); s != "" {
		fmt.Sscan(s, &hands)
	}
	names := []string{"dove", "clam", "shark", "turtle", "ox"}
	for _, n := range names {
		wasm[n]() // build the modules before the clock starts
	}
	var wg sync.WaitGroup
	for i, a := range names {
		for _, b := range names[i+1:] {
			wg.Go(func() {
				t.Run(a+"-"+b, func(t *testing.T) { playPair(t, a, b, hands, native, wasm) })
			})
		}
	}
	wg.Wait()
}

func playPair(t *testing.T, a, b string, hands int, native map[string]func() (sdk.Bot, func()), wasm map[string]func() (*Program, Config)) {
	na, ca := native[a]()
	defer ca()
	nb, cb := native[b]()
	defer cb()
	start := time.Now()
	want, err := sdk.Match(na, nb, hands, 1)
	if err != nil {
		t.Fatal(err)
	}
	nativeTime := time.Since(start)
	var w [2]*Bot
	for i, name := range []string{a, b} {
		p, cfg := wasm[name]()
		if w[i], err = p.Start(cfg); err != nil {
			t.Fatal(err)
		}
		defer w[i].Close()
	}
	start = time.Now()
	got, err := sdk.Match(w[0], w[1], hands, 1)
	if err != nil {
		t.Fatal(err)
	}
	wasmTime := time.Since(start)
	for i, name := range []string{a, b} {
		r := w[i].Report()
		t.Logf("%s: %s; fuel/decision mean %d max %d; start fuel %d (%v); memory %d MiB; reads %d (%d B); %.1f us/decision",
			name, faultString(r.Fault), r.Fuel/uint64(max(1, r.Decisions)), r.MaxDecisionFuel, r.StartFuel, r.StartTime.Round(time.Millisecond),
			r.MemoryBytes>>20, r.ReadCalls, r.ReadBytes, float64(r.DecisionTime.Microseconds())/float64(max(1, r.Decisions)))
		if r.Fault != nil || r.Forfeited() > 0 {
			t.Errorf("%s: %v, %d forfeited", name, r.Fault, r.Forfeited())
		}
	}
	t.Logf("%s vs %s: native %v in %v; wasm %v in %v", a, b, want, nativeTime.Round(time.Millisecond), got, wasmTime.Round(time.Millisecond))
	if got.Net != want.Net {
		t.Fatalf("wasm net %d chips, native %d", got.Net, want.Net)
	}
}
