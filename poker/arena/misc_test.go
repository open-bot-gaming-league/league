package arena

import (
	"os"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// TestConcurrentMatches: one Program serves bots in several matches at
// once, and each match, chips and every metered figure, is the same as
// when it plays alone: fuel, bytes read and memory are deterministic.
func TestConcurrentMatches(t *testing.T) {
	dove, clam := guest(t, "dove"), guest(t, "clam")
	type outcome struct {
		net                           int
		fuel, maxFuel, startFuel, mem uint64
	}
	play := func() (outcome, error) {
		a, err := dove.Start(Config{})
		if err != nil {
			return outcome{}, err
		}
		defer a.Close()
		b, err := clam.Start(Config{})
		if err != nil {
			return outcome{}, err
		}
		defer b.Close()
		res, err := sdk.Match(a, b, 100, 7)
		ra, rb := a.Report(), b.Report()
		return outcome{res.Net, ra.Fuel + rb.Fuel, ra.MaxDecisionFuel, ra.StartFuel + rb.StartFuel, ra.MemoryBytes + rb.MemoryBytes}, err
	}
	alone, err := play()
	if err != nil {
		t.Fatal(err)
	}
	if native, err := sdk.Match(bots.Dove{}, bots.Clam{}, 100, 7); err != nil || native.Net != alone.net {
		t.Fatalf("net %d, native %d (%v)", alone.net, native.Net, err)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			got, err := play()
			if err != nil || got != alone {
				t.Errorf("%+v (%v), alone %+v", got, err, alone)
			}
		})
	}
	wg.Wait()
}

func TestFilesMount(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x"
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := Files{"a.bin": path, "b.bin": path}
	list, err := f.ReadDir(".")
	if err != nil || len(list) != 2 || list[0].Name() != "a.bin" || list[1].Name() != "b.bin" {
		t.Fatalf("ReadDir: %v %v", list, err)
	}
	for _, bad := range []string{"x", "../x", "a.bin/..", "/a.bin"} {
		if _, err := f.Open(bad); err == nil {
			t.Errorf("opened %q", bad)
		}
	}
	info, err := f.Stat("a.bin")
	if err != nil || info.Size() != 3 || info.Name() != "a.bin" {
		t.Fatalf("Stat: %v %v", info, err)
	}
	// Folders, made up from the names: a submitted bot's data.
	nested := Files{"a.bin": path, "t/c.bin": path, "t/u/d.bin": path, "t/u/e.bin": path}
	if err := fstest.TestFS(nested, "a.bin", "t/c.bin", "t/u/d.bin", "t/u/e.bin"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"t/", "t/x", "u", "t/u/d.bin/x"} {
		if _, err := nested.Open(bad); err == nil {
			t.Errorf("opened %q", bad)
		}
	}
	if info, err := nested.Stat("t/u"); err != nil || !info.IsDir() || info.Name() != "u" {
		t.Errorf("Stat(t/u): %v %v", info, err)
	}
}

func TestLimitsDefaults(t *testing.T) {
	l := Limits{DecisionFuel: 5}.filled()
	d := DefaultLimits()
	if l.DecisionFuel != 5 || l.MemoryBytes != d.MemoryBytes || l.OutputBytes != d.OutputBytes || l.DecisionTimeout != d.DecisionTimeout {
		t.Fatalf("filled: %+v", l)
	}
}
