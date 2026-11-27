package league

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

// Report is one table's results as the challenger's coach reads them:
// where every seat placed, how the challenger did against each opponent,
// and every pair's result.
type Report struct {
	HandsPerPair int
	Names        []string // by seat: the bot's name
	Labels       []string // by seat: the name, with the seat when the name is not unique at the table
	Places       []Standing
	Challenger   Standing
	Opponents    []Opponent // the challenger's, best result first (seat order among equals)
	// The challenger's opponents by verdict: Beat + LostTo + TooClose is
	// len(Opponents).
	Beat, LostTo, TooClose int
	Grid                   [][]Cell // Grid[i][j]: seat i against seat j
	// Faulted are the seats whose sandbox played some of their decisions
	// for them (a check, else a fold), in seat order: a move that was
	// illegal or an error, or a limit the bot broke. Their results count
	// those decisions as played. Empty when every bot played every
	// decision itself.
	Faulted []Faulted
}

// Faulted is one seat's decisions its sandbox played, and what stopped
// it in each pair where something did.
type Faulted struct {
	Seat      int
	Label     string
	Forfeited int
	Faults    []SeatFault // in the record's order
	Pairs     int         // the pairs the seat played
}

// SeatFault is what stopped a seat's bot in its pair against Opponent.
type SeatFault struct {
	Opponent string // the opponent's label
	Fault
}

// Kinds is the seat's faults by kind, most frequent first (then by
// name): "fuel x10, memory x1".
func (f Faulted) Kinds() string {
	count := make(map[string]int)
	for _, x := range f.Faults {
		count[x.Kind]++
	}
	kinds := make([]string, 0, len(count))
	for k := range count {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(x, y int) bool {
		return count[kinds[x]] > count[kinds[y]] || count[kinds[x]] == count[kinds[y]] && kinds[x] < kinds[y]
	})
	for i, k := range kinds {
		kinds[i] = fmt.Sprintf("%s x%d", k, count[k])
	}
	return strings.Join(kinds, ", ")
}

// Standing is one seat's place.
type Standing struct {
	Place        int
	Tied         bool // another seat has the same place
	Seat         int
	Label        string
	Total        float64 // bb/100 summed over its opponents
	Won          int
	Lost         int
	Close        int // opponents too close to call
	IsChallenger bool
}

// Opponent is how the challenger did against one other seat.
type Opponent struct {
	Seat    int
	Label   string
	Result  sdk.Result // from the challenger's side
	Verdict Verdict
}

// Cell is one seat's result against another, from the row seat's side.
// The diagonal's cells are Self.
type Cell struct {
	Result  sdk.Result
	Verdict Verdict
	Self    bool
}

// Verdict is a result in a word: won, lost, or too close to call
// (sdk.Result.Close).
type Verdict string

const (
	Won      Verdict = "won"
	Lost     Verdict = "lost"
	TooClose Verdict = "too close to call"
)

// VerdictOf is r's verdict for its first bot.
func VerdictOf(r sdk.Result) Verdict {
	switch {
	case r.Close():
		return TooClose
	case r.BBPer100 > 0:
		return Won
	default:
		return Lost
	}
}

// NewReport reads a record as its report. A record whose seats, pairs or
// places do not fit together (a seat out of range, a pair missing or
// played twice, no challenger) is refused.
func NewReport(r *Record) (*Report, error) {
	n := len(r.Seats)
	challenger := -1
	for i, s := range r.Seats {
		if s.Seat != i {
			return nil, fmt.Errorf("league: record: seat %d listed at %d", s.Seat, i)
		}
		if s.Challenger {
			if challenger >= 0 {
				return nil, errors.New("league: record: two challengers")
			}
			challenger = i
		}
	}
	if challenger < 0 {
		return nil, errors.New("league: record: no challenger")
	}
	if len(r.Places) != n {
		return nil, fmt.Errorf("league: record: %d places for %d seats", len(r.Places), n)
	}
	rep := &Report{HandsPerPair: r.HandsPerPair, Names: make([]string, n), Labels: labels(r.Seats), Grid: make([][]Cell, n)}
	for i, s := range r.Seats {
		rep.Names[i] = s.Name
	}
	for i := range rep.Grid {
		rep.Grid[i] = make([]Cell, n)
		rep.Grid[i][i].Self = true
	}
	seen := make(map[[2]int]bool)
	for _, p := range r.Pairs {
		if p.A < 0 || p.A >= p.B || p.B >= n || seen[[2]int{p.A, p.B}] {
			return nil, fmt.Errorf("league: record: pair %d-%d out of range or played twice", p.A, p.B)
		}
		seen[[2]int{p.A, p.B}] = true
		res := p.Result()
		// 0 - x, not -x: an even pair is +0 from both sides, never "-0.0".
		back := sdk.Result{Hands: res.Hands, Net: -res.Net, BBPer100: 0 - res.BBPer100, SE: res.SE}
		rep.Grid[p.A][p.B] = Cell{Result: res, Verdict: VerdictOf(res)}
		rep.Grid[p.B][p.A] = Cell{Result: back, Verdict: VerdictOf(back)}
	}
	if len(seen) != n*(n-1)/2 {
		return nil, fmt.Errorf("league: record: %d pairs for %d seats, want %d", len(seen), n, n*(n-1)/2)
	}
	count := make(map[int]int)
	for _, p := range r.Places {
		count[p.Place]++
	}
	for _, p := range r.Places {
		if p.Seat < 0 || p.Seat >= n {
			return nil, fmt.Errorf("league: record: a place for seat %d", p.Seat)
		}
		s := Standing{Place: p.Place, Tied: count[p.Place] > 1, Seat: p.Seat, Label: rep.Labels[p.Seat], Total: p.Total,
			Won: p.Won, Lost: p.Lost, Close: n - 1 - p.Won - p.Lost, IsChallenger: p.Seat == challenger}
		rep.Places = append(rep.Places, s)
		if s.IsChallenger {
			rep.Challenger = s
		}
	}
	for j := range n {
		if j == challenger {
			continue
		}
		c := rep.Grid[challenger][j]
		rep.Opponents = append(rep.Opponents, Opponent{Seat: j, Label: rep.Labels[j], Result: c.Result, Verdict: c.Verdict})
		switch c.Verdict {
		case Won:
			rep.Beat++
		case Lost:
			rep.LostTo++
		default:
			rep.TooClose++
		}
	}
	sort.SliceStable(rep.Opponents, func(x, y int) bool {
		return rep.Opponents[x].Result.BBPer100 > rep.Opponents[y].Result.BBPer100
	})
	for _, u := range r.Usage {
		if u.Seat < 0 || u.Seat >= n {
			return nil, fmt.Errorf("league: record: usage for seat %d", u.Seat)
		}
		if (u.Forfeited == nil || *u.Forfeited == 0) && len(u.Faults) == 0 {
			continue
		}
		f := Faulted{Seat: u.Seat, Label: rep.Labels[u.Seat], Pairs: n - 1}
		if u.Forfeited != nil {
			f.Forfeited = int(*u.Forfeited)
		}
		for _, x := range u.Faults {
			if x.Opponent < 0 || x.Opponent >= n || x.Opponent == u.Seat {
				return nil, fmt.Errorf("league: record: seat %d faulted against seat %d", u.Seat, x.Opponent)
			}
			f.Faults = append(f.Faults, SeatFault{Opponent: rep.Labels[x.Opponent], Fault: x.Fault})
		}
		rep.Faulted = append(rep.Faulted, f)
	}
	sort.SliceStable(rep.Faulted, func(x, y int) bool { return rep.Faulted[x].Seat < rep.Faulted[y].Seat })
	return rep, nil
}

// labels names each seat by its bot's name, adding the seat where two
// seats share a name (copies of a bot).
func labels(seats []Seat) []string {
	count := make(map[string]int)
	for _, s := range seats {
		count[s.Name]++
	}
	out := make([]string, len(seats))
	for i, s := range seats {
		out[i] = s.Name
		if count[s.Name] > 1 {
			out[i] = fmt.Sprintf("%s (seat %d)", s.Name, s.Seat)
		}
	}
	return out
}

// Summary is the challenger's results in one line: "beat 5 of 9, lost
// to 4, too close to call 0".
func (rep *Report) Summary() string {
	return fmt.Sprintf("beat %d of %d, lost to %d, too close to call %d", rep.Beat, len(rep.Opponents), rep.LostTo, rep.TooClose)
}

// Placed is where the challenger placed, in words: "10th of 10", "tied
// 2nd of 10".
func (rep *Report) Placed() string {
	s := fmt.Sprintf("%s of %d", Ordinal(rep.Challenger.Place), len(rep.Places))
	if rep.Challenger.Tied {
		s = "tied " + s
	}
	return s
}

// Text is the report as plain text, short enough to read aloud: the
// challenger's place and summary, its result against each opponent, and
// the places. The full grid is left to the page.
func (rep *Report) Text() string {
	var b strings.Builder
	ch := rep.Challenger
	fmt.Fprintf(&b, "%s placed %s, %+.1f bb/100 summed over its %d opponents.\n", ch.Label, rep.Placed(), ch.Total, len(rep.Opponents))
	fmt.Fprintf(&b, "It %s.\n", rep.Summary())
	fmt.Fprintf(&b, "Every pair played %s hands heads-up on duplicate deals. Too close to call: within 2 standard errors of even.\n", Thousands(rep.HandsPerPair))
	fmt.Fprintf(&b, "\n%s against each opponent, bb/100 ± standard error:\n", ch.Label)
	w := 0
	for _, o := range rep.Opponents {
		w = max(w, len([]rune(o.Label)))
	}
	for _, o := range rep.Opponents {
		fmt.Fprintf(&b, "  %s  %+6.1f ± %.1f  %s\n", pad(o.Label, w), o.Result.BBPer100, o.Result.SE, o.Verdict)
	}
	b.WriteString("\nPlaces, by bb/100 summed over each seat's opponents:\n")
	w = 0
	for _, p := range rep.Places {
		w = max(w, len([]rune(p.Label)))
	}
	for _, p := range rep.Places {
		mark := ""
		if p.IsChallenger {
			mark = "  <- " + ch.Label
		}
		fmt.Fprintf(&b, "  %2d. %s  %+7.1f%s\n", p.Place, pad(p.Label, w), p.Total, mark)
	}
	if len(rep.Faulted) > 0 {
		b.WriteString("\nThe sandbox played for these bots (a check, else a fold) where they broke a rule or a limit:\n")
		for _, f := range rep.Faulted {
			fmt.Fprintf(&b, "  %s: %s decisions played for it", f.Label, Thousands(f.Forfeited))
			if len(f.Faults) > 0 {
				fmt.Fprintf(&b, "; stopped in %d of its %d pairs (%s)", len(f.Faults), f.Pairs, f.Kinds())
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func pad(s string, w int) string { return s + strings.Repeat(" ", w-len([]rune(s))) }

// ResultText is a result as the report shows it: "+3.7 ± 0.8".
func ResultText(r sdk.Result) string { return fmt.Sprintf("%+.1f ± %.1f", r.BBPer100, r.SE) }

// Ordinal is 1st, 2nd, 3rd, 4th ... 11th, 12th, 13th, 21st.
func Ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// Thousands writes n with commas: 1,000,000.
func Thousands(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}
