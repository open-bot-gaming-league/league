package league

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

// Record is one table's outcome. Every field but Measured is determined by
// the seed, the pool and the bots; Measured is how this run of it went.
type Record struct {
	Format       int         `json:"format"` // Format
	Runner       string      `json:"runner"` // the runner's version
	Seed         uint64      `json:"seed"`
	HandsPerPair int         `json:"hands_per_pair"`
	Draw         string      `json:"draw,omitempty"` // how the seats were drawn (Config.Draw); absent: DrawCopies
	Pool         []Bot       `json:"pool"`           // the bots seats were drawn from
	Seats        []Seat      `json:"seats"`          // seat 0 is the challenger
	Pairs        []Pair      `json:"pairs"`          // every pair of seats, A < B, in order
	Places       []Placing   `json:"places"`         // best first; equal places in seat order
	Usage        []SeatUsage `json:"usage"`          // per seat, in seat order
	Measured     Measured    `json:"measured"`
}

// Bot is a bot as the record names it.
type Bot struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Seat is who sat where.
type Seat struct {
	Seat       int  `json:"seat"`
	Challenger bool `json:"challenger,omitempty"`
	Bot
	Owner *Owner `json:"owner,omitempty"` // a submitted bot's; absent for the house's
}

// Owner is whose submitted bot sits in a seat: the account that submitted
// it, and which of its versions this is.
type Owner struct {
	Account    string `json:"account"`              // the league account's id
	Bot        string `json:"bot"`                  // the bot's name in the account
	Version    int    `json:"version"`              // its version number, from 1
	Submission string `json:"submission,omitempty"` // the submission that made the version
}

// Pair is one pair's match, as seat A saw it: sdk.Match(A, B).
type Pair struct {
	A        int     `json:"a"`
	B        int     `json:"b"`
	Hands    int     `json:"hands"`
	Net      int     `json:"net"`        // chips A won from B
	BBPer100 float64 `json:"bb_per_100"` // A's win rate against B
	SE       float64 `json:"se"`         // its standard error
}

// Placing is one seat's place. Total is the seat's bb/100 summed over its
// opponents; the place is decided on TotalNet, the chips behind it, so that
// equal is exact (every pair plays the same number of hands, so the two
// order the seats alike).
type Placing struct {
	Place    int     `json:"place"`
	Seat     int     `json:"seat"`
	Name     string  `json:"name"`
	Total    float64 `json:"total_bb_per_100"`
	TotalNet int64   `json:"total_net"`
	Won      int     `json:"won"`  // opponents it beat, past too close to call (sdk.Result.Close)
	Lost     int     `json:"lost"` // opponents that beat it, past too close to call
}

// SeatUsage is what one seat used over all its pairs. The metered fields
// are null for a bot that is not a Meter.
type SeatUsage struct {
	Seat       int    `json:"seat"`
	Decisions  int64  `json:"decisions"`
	Fuel       *int64 `json:"fuel"`
	BytesRead  *int64 `json:"bytes_read"`
	MemoryPeak *int64 `json:"memory_peak_bytes"` // the most in any one pair
	// For a Meter: the decisions its sandbox played for it, and what
	// stopped it in each pair where something did. Absent for other bots.
	Forfeited *int64      `json:"forfeited,omitempty"`
	Faults    []PairFault `json:"faults,omitempty"`
}

// PairFault is a Fault in the pair against Opponent.
type PairFault struct {
	Opponent int `json:"opponent"`
	Fault
}

// Measured is how this run of the table went: wall-clock and process
// figures, which differ from run to run.
type Measured struct {
	WallS    float64    `json:"wall_s"`         // Play, start to end
	CPUS     *float64   `json:"cpu_s"`          // the process's user+system CPU over Play; null where not measured
	PeakRSS  *int64     `json:"peak_rss_bytes"` // the process's peak resident memory so far
	Parallel int        `json:"parallel"`       // pairs at once
	Seats    []SeatTime `json:"seats"`
	Pairs    []PairTime `json:"pairs"`
}

// SeatTime is the wall time a seat spent deciding, over all its pairs.
type SeatTime struct {
	Seat     int     `json:"seat"`
	ActWallS float64 `json:"act_wall_s"`
}

// PairTime is a pair's wall time, making the bots included.
type PairTime struct {
	A     int     `json:"a"`
	B     int     `json:"b"`
	WallS float64 `json:"wall_s"`
}

func newRecord(cfg Config, challenger Entrant, pool, seats []Entrant) *Record {
	r := &Record{Format: Format, Runner: cfg.Runner, Seed: cfg.Seed, HandsPerPair: cfg.Hands}
	if cfg.Draw != DrawCopies {
		r.Draw = cfg.Draw
	}
	for _, e := range pool {
		r.Pool = append(r.Pool, botOf(e))
	}
	r.Seats = make([]Seat, len(seats))
	r.Usage = make([]SeatUsage, len(seats))
	r.Measured.Seats = make([]SeatTime, len(seats))
	for i, e := range seats {
		r.Seats[i] = Seat{Seat: i, Challenger: i == 0, Bot: botOf(e), Owner: e.Owner}
		r.Usage[i].Seat = i
		r.Measured.Seats[i].Seat = i
	}
	return r
}

func botOf(e Entrant) Bot { return Bot{ID: e.ID, Name: e.Name, Version: e.Version} }

// Result is the pair's match as sdk.Match reported it.
func (p Pair) Result() sdk.Result {
	return sdk.Result{Hands: p.Hands, Net: p.Net, BBPer100: p.BBPer100, SE: p.SE}
}

// addPair adds a pair's match and what each side used.
func (r *Record) addPair(a, b int, p played) {
	r.Pairs = append(r.Pairs, Pair{A: a, B: b, Hands: p.result.Hands, Net: p.result.Net, BBPer100: p.result.BBPer100, SE: p.result.SE})
	r.Measured.Pairs = append(r.Measured.Pairs, PairTime{A: a, B: b, WallS: seconds(p.wall)})
	for k, seat := range [2]int{a, b} {
		s := p.side[k]
		u := &r.Usage[seat]
		u.Decisions += s.decisions
		r.Measured.Seats[seat].ActWallS = roundS(r.Measured.Seats[seat].ActWallS + s.act.Seconds())
		if s.usage != nil {
			u.Fuel = add(u.Fuel, s.usage.Fuel)
			u.BytesRead = add(u.BytesRead, s.usage.BytesRead)
			if u.MemoryPeak == nil || s.usage.MemoryPeak > *u.MemoryPeak {
				m := s.usage.MemoryPeak
				u.MemoryPeak = &m
			}
			u.Forfeited = add(u.Forfeited, s.usage.Forfeited)
			if f := s.usage.Fault; f != nil {
				u.Faults = append(u.Faults, PairFault{Opponent: [2]int{b, a}[k], Fault: *f})
			}
		}
	}
}

func add(sum *int64, x int64) *int64 {
	if sum != nil {
		x += *sum
	}
	return &x
}

// place fills Places from Pairs.
func (r *Record) place() {
	n := len(r.Seats)
	totals := make([]int64, n)
	bb := make([]float64, n)
	won, lost := make([]int, n), make([]int, n)
	for _, p := range r.Pairs {
		totals[p.A] += int64(p.Net)
		totals[p.B] -= int64(p.Net)
		bb[p.A] += p.BBPer100
		bb[p.B] -= p.BBPer100
		if !p.Result().Close() {
			w, l := p.A, p.B
			if p.BBPer100 < 0 {
				w, l = l, w
			}
			won[w]++
			lost[l]++
		}
	}
	places := Place(totals)
	r.Places = make([]Placing, n)
	for i := range n {
		r.Places[i] = Placing{Place: places[i], Seat: i, Name: r.Seats[i].Name, Total: bb[i], TotalNet: totals[i], Won: won[i], Lost: lost[i]}
	}
	sortPlacings(r.Places)
}

func sortPlacings(ps []Placing) {
	sort.Slice(ps, func(x, y int) bool {
		return ps[x].Place < ps[y].Place || ps[x].Place == ps[y].Place && ps[x].Seat < ps[y].Seat
	})
}

// JSON is the record as written: indented, with a final newline.
func (r *Record) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ReadRecord parses a record written by JSON.
func ReadRecord(b []byte) (*Record, error) {
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Format != Format {
		return nil, fmt.Errorf("league: record format %d, want %d", r.Format, Format)
	}
	return &r, nil
}

// Results is the record without Measured: the part another run of the
// same table reproduces byte for byte.
func (r *Record) Results() *Record {
	c := *r
	c.Measured = Measured{}
	return &c
}
