package arena

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// Play-speed benchmarks: each bot natively and as a sandboxed wasm bot
// (all limits on), against native Clam. bench.sh runs them pinned to one
// core and tabulates them. One iteration is one match; the metrics are
// the bot's own decisions: ns-mean/ns-p50/ns-p99 a decision (less the
// timer's own cost), and hands/s for the whole match.

// benchBots are the bots under test, each made once per test binary: a
// house bot's load takes seconds, and the benchmark loop runs its function
// more than once. Bots keep their state between matches, as in a long one.
var benchBots = struct {
	sync.Mutex
	m map[string]sdk.Bot
}{m: map[string]sdk.Bot{}}

func benchBot(b *testing.B, name, mode string) sdk.Bot {
	b.Helper()
	key := name + "/" + mode
	benchBots.Lock()
	defer benchBots.Unlock()
	if bot := benchBots.m[key]; bot != nil {
		return bot
	}
	var bot sdk.Bot
	var err error
	switch {
	case mode == "native" && (name == "turtle" || name == "ox"):
		bot, err = sdk.LoadHouseByID(name)
	case mode == "native":
		bot = bots.ByName[name]()
	default:
		p, cfg := benchProgram(b, name, mode)
		bot, err = p.Start(cfg)
		if err == nil && bot.(*Bot).Report().Fault != nil {
			err = fmt.Errorf("%v", bot.(*Bot).Report().Fault)
		}
	}
	if err != nil {
		b.Fatal(err)
	}
	benchBots.m[key] = bot
	return bot
}

// benchProgram is a wasm bot's module and config. mode "wasm" for the
// starter bots; "wasm-ondemand" or "wasm-inmemory" for the house bots'
// bucket file.
func benchProgram(b *testing.B, name, mode string) (*Program, Config) {
	b.Helper()
	if name != "turtle" && name != "ox" {
		return guest(b, name), Config{}
	}
	dir, err := sdk.HouseCacheDir()
	if err != nil {
		b.Fatal(err)
	}
	hb := sdk.Houses().Bots[name]
	cfg := Config{Data: Files{"buckets.bin": filepath.Join(dir, hb.Buckets), "blueprint.bp": filepath.Join(dir, hb.Blueprint)}}
	if mode == "wasm-inmemory" {
		return guest(b, "house", "-buildmode=c-shared", "-tags=tables_in_memory"), cfg
	}
	return guest(b, "house"), cfg
}

// timed is a bot with every decision's time kept.
type timed struct {
	bot   sdk.Bot
	times []time.Duration
}

func (t *timed) Act(s sdk.State) sdk.Action {
	start := time.Now()
	a := t.bot.Act(s)
	t.times = append(t.times, time.Since(start))
	return a
}

// timerCost is what timing an empty call costs, the median of many.
var timerCost = sync.OnceValue(func() time.Duration {
	d := make([]time.Duration, 100_001)
	for i := range d {
		start := time.Now()
		d[i] = time.Since(start)
	}
	slices.Sort(d)
	return d[len(d)/2]
})

var benchModes = map[string][]string{
	"dove": {"native", "wasm"}, "clam": {"native", "wasm"}, "shark": {"native", "wasm"},
	"turtle": {"native", "wasm-ondemand", "wasm-inmemory"}, "ox": {"native", "wasm-ondemand", "wasm-inmemory"},
}

// benchHands is a match's hands: about a second of Dove in wasm, less for the rest.
func benchHands(name string) int {
	if name == "dove" {
		return 2_000
	}
	return 20_000
}

func BenchmarkPlay(b *testing.B) {
	for _, name := range []string{"dove", "clam", "shark", "turtle", "ox"} {
		for _, mode := range benchModes[name] {
			b.Run(name+"/"+mode, func(b *testing.B) {
				bot := &timed{bot: benchBot(b, name, mode)}
				if _, err := sdk.Match(bot, bots.Clam{}, 200, 99); err != nil { // warm-up
					b.Fatal(err)
				}
				hands := benchHands(name)
				bot.times = bot.times[:0]
				b.ResetTimer()
				start := time.Now()
				for i := range b.N {
					if _, err := sdk.Match(bot, bots.Clam{}, hands, uint64(100+i)); err != nil {
						b.Fatal(err)
					}
				}
				elapsed := time.Since(start)
				b.StopTimer()
				if w, ok := bot.bot.(*Bot); ok && w.Report().Fault != nil {
					b.Fatalf("the bot stopped: %v", w.Report().Fault)
				}
				d := slices.Clone(bot.times)
				cost := timerCost()
				var sum time.Duration
				for i := range d {
					d[i] = max(0, d[i]-cost)
					sum += d[i]
				}
				slices.Sort(d)
				b.ReportMetric(float64(sum.Nanoseconds())/float64(len(d)), "ns-mean")
				b.ReportMetric(float64(d[len(d)/2].Nanoseconds()), "ns-p50")
				b.ReportMetric(float64(d[len(d)*99/100].Nanoseconds()), "ns-p99")
				b.ReportMetric(float64(hands*b.N)/elapsed.Seconds(), "hands/s")
				b.ReportMetric(float64(len(d))/float64(b.N), "decisions/match")
			})
		}
	}
}

// BenchmarkStart is a bot's load: for wasm, instantiating a compiled
// module and running the bot's constructor (Start then Close; the compile
// is not counted); natively, building the bot (for a house bot, opening
// its files and building its board indexes).
func BenchmarkStart(b *testing.B) {
	for _, name := range []string{"dove", "clam", "shark", "turtle", "ox"} {
		for _, mode := range benchModes[name] {
			b.Run(name+"/"+mode, func(b *testing.B) {
				var p *Program
				var cfg Config
				if mode != "native" {
					p, cfg = benchProgram(b, name, mode)
				}
				b.ResetTimer()
				for range b.N {
					switch {
					case mode != "native":
						w, err := p.Start(cfg)
						if err != nil || w.Report().Fault != nil {
							b.Fatal(err, w.Report().Fault)
						}
						w.Close()
					case name == "turtle" || name == "ox":
						h, err := sdk.LoadHouseByID(name)
						if err != nil {
							b.Fatal(err)
						}
						h.Close()
					default:
						_ = bots.ByName[name]()
					}
				}
			})
		}
	}
}
