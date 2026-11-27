// Package league runs a ranked table: a challenger and seats drawn from a
// pool of bots, every pair heads-up on duplicate deals, placed by how much
// each won from everyone else.
//
// Play is the whole table. Its Record is what the table produced, as JSON:
// who sat where, every pair's result with its standard error, the places,
// and what each seat used. Everything in the Record except its Measured
// part (wall-clock times and the process's CPU and memory) is a function of
// the seed, the bots and the pool: playing the same table again gives the
// same bytes, at any parallelism, which is how anyone checks a table.
//
// The bots are sdk.Bots. A bot run in a sandbox reports what it used by
// implementing Meter (fuel, bytes of its data read, peak memory); the
// record carries those figures and leaves them null for a bot that does not.
package league

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

// Format is the Record format this package writes.
const Format = 1

// Defaults for a ranked table: the challenger and nine seats from the
// pool, a million hands a pair (a standard error of about 0.2 to 1.2
// bb/100 a pair between the house bots).
const (
	DefaultSeats = 9
	DefaultHands = 1_000_000
)

// drawStream is the PCG stream the seats are drawn from, apart from the
// streams sdk.Match uses with the same seed (0 for the cards, 1 and 2 for
// the bots).
const drawStream = 0x7461626c65 // "table"

// Entrant is a bot that can take a seat.
type Entrant struct {
	ID      string // the bot's id in the pool, e.g. "ox"; the challenger's own id
	Name    string // its name to show
	Version string // exactly what plays: a content hash or a build revision
	// New makes the bot for one pair. It is called once per pair the seat
	// plays, possibly from several goroutines at once, so a bot that keeps
	// state is never shared between pairs. A bot that is an io.Closer is
	// closed when its pair is done.
	New func() (sdk.Bot, error)
}

// Meter is a bot that measures what it uses, as a sandboxed bot does.
// Usage is the total since the bot was made by Entrant.New; the table
// reads it when the bot's pair is done, before closing the bot.
type Meter interface {
	Usage() Usage
}

// Usage is what one bot used in one pair, and how its sandbox ruled.
type Usage struct {
	Fuel       int64 // the runtime's fuel units burned
	BytesRead  int64 // bytes of the bot's data read
	MemoryPeak int64 // the bot's peak memory, bytes
	// Forfeited is the decisions its sandbox played for it (check, else
	// fold): moves that were illegal or errors, and every decision after a
	// Fault.
	Forfeited int64
	Fault     *Fault // what stopped the bot in this pair; nil if nothing did
}

// Fault is what stopped a sandboxed bot: from Decision on (counted from 1
// in the pair; 0 is its load) the sandbox played for it.
type Fault struct {
	Kind     string `json:"kind"`
	Decision int    `json:"decision"`
	Hand     int    `json:"hand"`
	Detail   string `json:"detail"`
}

// Config is a table's settings. The zero value of each field is the usual
// one: DefaultSeats, DefaultHands, seed 0, and as many pairs at once as
// GOMAXPROCS.
type Config struct {
	Seats    int    // seats drawn from the pool, besides the challenger
	Hands    int    // hands per pair, rounded up to even (every deal twice)
	Seed     uint64 // draws the seats, deals the cards, seeds the bots
	Parallel int    // pairs played at once
	Runner   string // the runner's version, for the record
}

func (c Config) withDefaults() Config {
	if c.Seats == 0 {
		c.Seats = DefaultSeats
	}
	if c.Hands == 0 {
		c.Hands = DefaultHands
	}
	if c.Parallel == 0 {
		c.Parallel = runtime.GOMAXPROCS(0)
	}
	return c
}

// Draw picks n seats from the pool, with replacement (a bot can sit more
// than once), as indexes into pool. The same seed and pool give the same
// seats.
func Draw(poolSize, n int, seed uint64) []int {
	r := rand.New(rand.NewPCG(seed, drawStream))
	seats := make([]int, n)
	for i := range seats {
		seats[i] = r.IntN(poolSize)
	}
	return seats
}

// Play runs one ranked table: the challenger in seat 0 and cfg.Seats seats
// drawn from pool (Draw) after it; every pair of seats plays sdk.Match with
// cfg.Hands hands and cfg.Seed (every pair the same deals, so every seat
// faces the same cards), up to cfg.Parallel pairs at once; and the seats
// are placed by their total (Place).
//
// A bot's error (an illegal move, a panic) ends the table with an error and
// no record: a bot runner that can fail (a sandbox out of fuel) must turn
// the failure into play, such as folding, so the table finishes.
func Play(cfg Config, challenger Entrant, pool []Entrant) (*Record, error) {
	cfg = cfg.withDefaults()
	switch {
	case len(pool) == 0:
		return nil, errors.New("league: an empty pool")
	case cfg.Seats < 1:
		return nil, fmt.Errorf("league: %d seats; a table needs at least one beside the challenger", cfg.Seats)
	case cfg.Hands < 2:
		return nil, fmt.Errorf("league: %d hands a pair; at least 2", cfg.Hands)
	case cfg.Parallel < 1:
		return nil, fmt.Errorf("league: parallel %d", cfg.Parallel)
	}
	seats := []Entrant{challenger}
	for _, i := range Draw(len(pool), cfg.Seats, cfg.Seed) {
		seats = append(seats, pool[i])
	}
	rec := newRecord(cfg, challenger, pool, seats)

	var pairs [][2]int
	for i := range seats {
		for j := i + 1; j < len(seats); j++ {
			pairs = append(pairs, [2]int{i, j})
		}
	}
	results := make([]played, len(pairs))
	jobs := make(chan int)
	var (
		mu     sync.Mutex
		failed []error
		wg     sync.WaitGroup
	)
	start := time.Now()
	before := readProcess()
	for range min(cfg.Parallel, len(pairs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range jobs {
				p := pairs[k]
				res, err := playPair(seats[p[0]], seats[p[1]], cfg.Hands, cfg.Seed)
				if err != nil {
					mu.Lock()
					failed = append(failed, fmt.Errorf("league: seat %d (%s) vs seat %d (%s): %w",
						p[0], seats[p[0]].Name, p[1], seats[p[1]].Name, err))
					mu.Unlock()
				}
				results[k] = res
			}
		}()
	}
	for k := range pairs {
		mu.Lock()
		stop := len(failed) > 0
		mu.Unlock()
		if stop {
			break // the table has failed: start no more pairs
		}
		jobs <- k
	}
	close(jobs)
	wg.Wait()
	if len(failed) > 0 {
		return nil, errors.Join(failed...)
	}
	after := readProcess()
	for k, p := range pairs {
		rec.addPair(p[0], p[1], results[k])
	}
	rec.place()
	rec.Measured.WallS = seconds(time.Since(start))
	rec.Measured.Parallel = cfg.Parallel
	if before.ok && after.ok {
		cpu := roundS(after.cpu - before.cpu)
		peak := after.peakRSS
		rec.Measured.CPUS, rec.Measured.PeakRSS = &cpu, &peak
	}
	return rec, nil
}

// played is one pair's outcome: the match, and each side's use.
type played struct {
	result sdk.Result
	wall   time.Duration
	side   [2]sideUse
}

// sideUse is what one side of a pair used.
type sideUse struct {
	decisions int64
	act       time.Duration
	usage     *Usage // nil: the bot is not a Meter
}

func playPair(a, b Entrant, hands int, seed uint64) (played, error) {
	var p played
	start := time.Now()
	ba, err := newCounted(a)
	if err != nil {
		return p, err
	}
	defer ba.close()
	bb, err := newCounted(b)
	if err != nil {
		return p, err
	}
	defer bb.close()
	p.result, err = sdk.Match(ba, bb, hands, seed)
	if err != nil {
		return p, err
	}
	p.side = [2]sideUse{ba.use(), bb.use()}
	p.wall = time.Since(start)
	return p, nil
}

// counted is a bot with its decisions counted and the time it took over
// them clocked.
type counted struct {
	bot       sdk.Bot
	decisions int64
	act       time.Duration
}

func newCounted(e Entrant) (*counted, error) {
	b, err := e.New()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", e.Name, err)
	}
	return &counted{bot: b}, nil
}

func (c *counted) Act(s sdk.State) sdk.Action {
	t := time.Now()
	a := c.bot.Act(s)
	c.act += time.Since(t)
	c.decisions++
	return a
}

func (c *counted) use() sideUse {
	u := sideUse{decisions: c.decisions, act: c.act}
	if m, ok := c.bot.(Meter); ok {
		x := m.Usage()
		u.usage = &x
	}
	return u
}

func (c *counted) close() {
	if cl, ok := c.bot.(io.Closer); ok {
		cl.Close()
	}
}

// Place gives each total its place, highest first. Equal totals share a
// place, and the next place skips as many as shared it (1, 2, 2, 4).
// It returns the places in seat order.
func Place(totals []int64) []int {
	order := make([]int, len(totals))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool { return totals[order[x]] > totals[order[y]] })
	places := make([]int, len(totals))
	for k, i := range order {
		if k > 0 && totals[i] == totals[order[k-1]] {
			places[i] = places[order[k-1]]
		} else {
			places[i] = k + 1
		}
	}
	return places
}

func seconds(d time.Duration) float64 { return roundS(d.Seconds()) }

// roundS rounds seconds to the millisecond, for a record people read.
func roundS(s float64) float64 { return float64(int64(s*1000+0.5)) / 1000 }
