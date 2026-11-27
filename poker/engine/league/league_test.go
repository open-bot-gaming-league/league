package league

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

func starter(id string) Entrant {
	return Entrant{ID: id, Name: bots.Names[id], Version: "test", New: func() (sdk.Bot, error) { return bots.ByName[id](), nil }}
}

func starters() []Entrant { return []Entrant{starter("dove"), starter("clam"), starter("shark")} }

func TestDraw(t *testing.T) {
	a, b := Draw(5, 9, 7), Draw(5, 9, 7)
	if len(a) != 9 || !equal(a, b) {
		t.Fatalf("Draw(5, 9, 7) = %v then %v", a, b)
	}
	if equal(a, Draw(5, 9, 8)) {
		t.Errorf("seeds 7 and 8 draw the same seats %v", a)
	}
	counts := make([]int, 5)
	for _, i := range Draw(5, 50000, 1) {
		counts[i]++
	}
	for i, c := range counts { // 10,000 expected, SD about 89
		if c < 9500 || c > 10500 {
			t.Errorf("bot %d drawn %d times in 50,000, want about 10,000: %v", i, c, counts)
		}
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlace(t *testing.T) {
	for _, tc := range []struct {
		totals []int64
		want   []int
	}{
		{[]int64{5, 9, 1}, []int{2, 1, 3}},
		{[]int64{3, 7, 7, -2}, []int{3, 1, 1, 4}},
		{[]int64{0, 0, 0}, []int{1, 1, 1}},
		{[]int64{4, 2, 2, 2, 1}, []int{1, 2, 2, 2, 5}},
	} {
		if got := Place(tc.totals); !equal(got, tc.want) {
			t.Errorf("Place(%v) = %v, want %v", tc.totals, got, tc.want)
		}
	}
}

// A table is the same at any parallelism, and each pair is sdk.Match.
func TestPlayDeterministic(t *testing.T) {
	cfg := Config{Seats: 4, Hands: 400, Seed: 3, Runner: "test"}
	var out [][]byte
	var rec *Record
	for _, par := range []int{1, 3, 8} {
		cfg.Parallel = par
		r, err := Play(cfg, starter("shark"), starters())
		if err != nil {
			t.Fatal(err)
		}
		b, err := r.Results().JSON()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
		rec = r
	}
	for i := 1; i < len(out); i++ {
		if !bytes.Equal(out[0], out[i]) {
			t.Fatalf("parallel 1 and %d records differ:\n%s\n%s", []int{1, 3, 8}[i], out[0], out[i])
		}
	}
	if len(rec.Seats) != 5 || len(rec.Pairs) != 10 || len(rec.Places) != 5 || len(rec.Usage) != 5 {
		t.Fatalf("seats %d pairs %d places %d usage %d", len(rec.Seats), len(rec.Pairs), len(rec.Places), len(rec.Usage))
	}
	draw := Draw(3, 4, 3)
	for i, s := range rec.Seats[1:] {
		if s.ID != starters()[draw[i]].ID || s.Challenger {
			t.Errorf("seat %d is %+v, want %s", i+1, s, starters()[draw[i]].ID)
		}
	}
	if !rec.Seats[0].Challenger || rec.Seats[0].ID != "shark" {
		t.Errorf("seat 0 is %+v", rec.Seats[0])
	}
	k := 0
	for a := range rec.Seats {
		for b := a + 1; b < len(rec.Seats); b++ {
			p := rec.Pairs[k]
			k++
			want, err := sdk.Match(bots.ByName[rec.Seats[a].ID](), bots.ByName[rec.Seats[b].ID](), 400, 3)
			if err != nil {
				t.Fatal(err)
			}
			if p.A != a || p.B != b || p.Net != want.Net || p.BBPer100 != want.BBPer100 || p.SE != want.SE || p.Hands != 400 {
				t.Errorf("pair %d-%d = %+v, want %+v", a, b, p, want)
			}
		}
	}
	// Places: best first, each total the seat's sum, no metered fields.
	for i, pl := range rec.Places {
		var net int64
		var bb float64
		for _, p := range rec.Pairs {
			switch pl.Seat {
			case p.A:
				net, bb = net+int64(p.Net), bb+p.BBPer100
			case p.B:
				net, bb = net-int64(p.Net), bb-p.BBPer100
			}
		}
		if pl.TotalNet != net || math.Abs(pl.Total-bb) > 1e-9 {
			t.Errorf("place %+v: total %d / %v, want %d / %v", pl, pl.TotalNet, pl.Total, net, bb)
		}
		if i > 0 && (pl.TotalNet > rec.Places[i-1].TotalNet || pl.Place < rec.Places[i-1].Place) {
			t.Errorf("places out of order: %+v after %+v", pl, rec.Places[i-1])
		}
	}
	for _, u := range rec.Usage {
		if u.Decisions == 0 || u.Fuel != nil || u.BytesRead != nil || u.MemoryPeak != nil {
			t.Errorf("usage %+v: want decisions and no metered fields", u)
		}
	}
	m := rec.Measured
	if len(m.Pairs) != 10 || len(m.Seats) != 5 || m.Parallel != 8 || m.WallS <= 0 {
		t.Errorf("measured %+v", m)
	}
	if b, _ := rec.JSON(); !json.Valid(b) || !strings.Contains(string(b), `"fuel": null`) {
		t.Errorf("record JSON:\n%s", b)
	}
}

// metered is a bot that reports made-up usage: one fuel a decision, a byte
// read every other one, and a fixed peak.
type metered struct {
	n    int64
	peak int64
}

func (m *metered) Act(s sdk.State) sdk.Action { m.n++; return s.CheckOrCall() }
func (m *metered) Usage() Usage {
	u := Usage{Fuel: m.n, BytesRead: m.n / 2, MemoryPeak: m.peak, Forfeited: 1}
	if m.peak%2 == 0 {
		u.Fault = &Fault{Kind: "trap", Decision: 3, Hand: 2, Detail: "even"}
	}
	return u
}

type closer struct {
	sdk.Bot
	closed *atomic.Int64
}

func (c closer) Close() error { c.closed.Add(1); return nil }

func TestMeterAndClose(t *testing.T) {
	var peak atomic.Int64
	var closed atomic.Int64
	challenger := Entrant{ID: "m", Name: "Metered", Version: "1", New: func() (sdk.Bot, error) {
		return closer{&metered{peak: 100 + peak.Add(1)}, &closed}, nil
	}}
	rec, err := Play(Config{Seats: 3, Hands: 100, Seed: 1}, challenger, starters()[:1])
	if err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 3 {
		t.Errorf("closed %d bots, want 3 (one a pair)", closed.Load())
	}
	// closer hides the Meter: the challenger reports nothing.
	if u := rec.Usage[0]; u.Fuel != nil {
		t.Errorf("a closer that is not a Meter reported %+v", u)
	}

	challenger.New = func() (sdk.Bot, error) { return &metered{peak: 100 + peak.Add(1)}, nil }
	rec, err = Play(Config{Seats: 3, Hands: 100, Seed: 1}, challenger, starters()[:1])
	if err != nil {
		t.Fatal(err)
	}
	u := rec.Usage[0]
	if u.Fuel == nil || *u.Fuel != u.Decisions || u.BytesRead == nil || *u.BytesRead > u.Decisions/2 || *u.BytesRead < u.Decisions/2-3 {
		t.Fatalf("challenger usage %+v", u)
	}
	if *u.MemoryPeak != 100+peak.Load() {
		t.Errorf("memory peak %d, want the most of any pair, %d", *u.MemoryPeak, 100+peak.Load())
	}
	if rec.Usage[1].Fuel != nil || rec.Usage[1].Forfeited != nil || rec.Usage[1].Faults != nil {
		t.Errorf("Dove is not a Meter but reported %+v", rec.Usage[1])
	}
	// One forfeit a pair; a fault in each pair whose bot had an even peak.
	if *u.Forfeited != 3 || len(u.Faults) == 0 || len(u.Faults) > 2 || u.Faults[0].Kind != "trap" || u.Faults[0].Opponent == 0 {
		t.Errorf("forfeits %d, faults %+v", *u.Forfeited, u.Faults)
	}
}

type illegal struct{}

func (illegal) Act(sdk.State) sdk.Action { return sdk.Action{Kind: sdk.Raise, To: 1} }

func TestBotError(t *testing.T) {
	bad := Entrant{ID: "bad", Name: "Bad", New: func() (sdk.Bot, error) { return illegal{}, nil }}
	_, err := Play(Config{Seats: 2, Hands: 10, Parallel: 1}, bad, starters())
	if err == nil || !strings.Contains(err.Error(), "seat 0 (Bad)") {
		t.Fatalf("err = %v, want the table to fail naming seat 0 (Bad)", err)
	}
	for _, cfg := range []Config{{Seats: -1}, {Hands: 1}, {Parallel: -2}} {
		if _, err := Play(cfg, bad, starters()); err == nil {
			t.Errorf("Play(%+v) played", cfg)
		}
	}
	if _, err := Play(Config{}, bad, nil); err == nil {
		t.Error("an empty pool played")
	}
}

// The default ranked table: the challenger and ten drawn seats, eleven
// players, so the challenger plays ten opponents a million hands each.
func TestDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.Seats != 10 || c.Hands != 1_000_000 {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestCost(t *testing.T) {
	cpu, rss := 1000.0, int64(3<<30)/2 // 1.5 GiB
	r := &Record{Measured: Measured{WallS: 200, CPUS: &cpu, PeakRSS: &rss, Parallel: 8}}
	c, err := CostOf(r, CloudRunJobs, Task{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Task != (Task{VCPU: 8, GiB: 2}) {
		t.Errorf("task %+v, want 8 vCPU, 2 GiB", c.Task)
	}
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-12 }
	if !near(c.GiBS, 300) || !near(c.Used, 1000*0.000018+300*0.000002) || !near(c.Billed, 200*(8*0.000018+2*0.000002)) {
		t.Errorf("cost %+v", c)
	}
	if c, _ = CostOf(r, CloudRunJobs, Task{VCPU: 2, GiB: 2}); !near(c.BilledS, 500) {
		t.Errorf("billed %v s on 2 vCPU, want 1000 CPU-s / 2 = 500", c.BilledS)
	}
	r.Measured.WallS = 10 // billed for the one-minute minimum
	cpu = 5
	if c, _ = CostOf(r, CloudRunJobs, Task{VCPU: 4, GiB: 4}); !near(c.Billed, 60*(4*0.000018+4*0.000002)) {
		t.Errorf("billed %v under the minimum", c.Billed)
	}
	if !strings.Contains(c.String(), "cloud.google.com/run/pricing") {
		t.Errorf("String() does not cite the prices:\n%s", c)
	}
	r.Measured.CPUS = nil
	if _, err := CostOf(r, CloudRunJobs, Task{}); err == nil {
		t.Error("priced a record with no CPU figure")
	}
}

func TestPool(t *testing.T) {
	t.Setenv("OBGL_HOUSE_CACHE", t.TempDir()) // nothing downloaded
	pool, closePool, err := Pool("dove", "shark")
	if err != nil || len(pool) != 2 || pool[1].Name != "Shark" || pool[0].Version != RunnerVersion() {
		t.Fatalf("%+v, %v", pool, err)
	}
	if b, err := pool[0].New(); err != nil || b == nil {
		t.Errorf("Dove: %v, %v", b, err)
	}
	if err := closePool(); err != nil {
		t.Error(err)
	}
	for id, want := range map[string]string{"walrus": `no house bot "walrus"`, "ox": "Ox is not downloaded"} {
		if _, _, err := Pool("dove", id); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Pool(dove, %s): %v, want %q", id, err, want)
		}
	}
}
