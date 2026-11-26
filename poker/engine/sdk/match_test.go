package sdk

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

// caller checks or calls, always: no randomness.
type caller struct{}

func (caller) Act(s State) Action { return s.CheckOrCall() }

// tight raises the pot with a pair and folds to any bet otherwise: no
// randomness.
type tight struct{}

func (tight) Act(s State) Action {
	if s.Made() >= Pair {
		return s.RaisePot(1)
	}
	return s.CheckOrFold()
}

func TestMatchIsDeterministic(t *testing.T) {
	a, err := Match(Random{}, caller{}, 400, 5)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Match(Random{}, caller{}, 400, 5)
	c, _ := Match(Random{}, caller{}, 400, 6)
	if a != b {
		t.Fatalf("seed 5 twice: %v, then %v", a, b)
	}
	if a == c {
		t.Fatalf("seeds 5 and 6 played the same match: %v", a)
	}
	if a.Hands != 400 || a.SE <= 0 {
		t.Fatalf("%+v", a)
	}
}

// Duplicate deals: two bots without randomness get the same cards from
// both seats, so a bot against itself breaks exactly even, and swapping
// the two bots negates the result.
func TestDuplicateDeals(t *testing.T) {
	self, err := Match(tight{}, tight{}, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if self.Net != 0 || self.BBPer100 != 0 {
		t.Fatalf("a bot against itself: %v (%d chips)", self, self.Net)
	}
	ab, _ := Match(tight{}, caller{}, 1000, 1)
	ba, _ := Match(caller{}, tight{}, 1000, 1)
	if ab.Net == 0 || ab.Net != -ba.Net || ab.SE != ba.SE {
		t.Fatalf("tight vs caller %+v, caller vs tight %+v", ab, ba)
	}
	if odd, _ := Match(tight{}, caller{}, 7, 1); odd.Hands != 8 {
		t.Fatalf("7 hands played as %d", odd.Hands)
	}
}

// watcher checks every State it is shown, then plays Random.
type watcher struct {
	t     *testing.T
	seen  int
	kinds map[Kind]int
}

func (w *watcher) Act(s State) Action {
	w.t.Helper()
	w.seen++
	const start = 5000
	if s.Stack+s.OppStack+s.Pot != 2*start || s.Stack < 0 || s.OppStack < 0 || s.BigBlind != 100 {
		w.t.Fatalf("chips do not add up: %+v", s)
	}
	if call := s.OppInRound - s.InRound; call != s.Legal.Call || (call > 0) != s.Legal.Fold || (call == 0) != s.Legal.Check {
		w.t.Fatalf("legal %+v with %d in against %d", s.Legal, s.InRound, s.OppInRound)
	}
	if s.Legal.CanRaise() && (s.Legal.MinTo <= s.OppInRound || s.Legal.MaxTo != s.InRound+s.Stack) {
		w.t.Fatalf("raise range %+v with %d in, %d behind", s.Legal, s.InRound, s.Stack)
	}
	if want := []int{0, 3, 4, 5}[s.Round]; len(s.Board) != want {
		w.t.Fatalf("%s with %d board cards", s.Round, len(s.Board))
	}
	if len(s.History) == 0 && (s.Seat != SmallBlind || s.Round != Preflop) {
		w.t.Fatalf("first to act: seat %d in the %s", s.Seat, s.Round)
	}
	if n := len(s.History); n > 0 && s.History[n-1].Seat == s.Seat && s.History[n-1].Round == s.Round {
		w.t.Fatalf("acting twice in a row: %+v", s.History)
	}
	a := Random{}.Act(s)
	w.kinds[a.Kind]++
	return a
}

// Every State a bot sees is consistent, and the helpers' moves are all
// legal (the engine refuses anything else, which would fail the match).
func TestStatesAndHelpers(t *testing.T) {
	w := &watcher{t: t, kinds: map[Kind]int{}}
	if _, err := Match(w, Random{}, 2000, 3); err != nil {
		t.Fatal(err)
	}
	for k := Fold; k <= Raise; k++ {
		if w.kinds[k] == 0 {
			t.Errorf("no %s seen in %d decisions", k, w.seen)
		}
	}
}

type badBot struct{ a Action }

func (b badBot) Act(State) Action { return b.a }

type panicBot struct{}

func (panicBot) Act(s State) Action { return Action{To: s.Board[4].Rank()} }

func TestBadBotsAreErrors(t *testing.T) {
	for _, tc := range []struct {
		bot  Bot
		want string
	}{
		{badBot{Action{Kind: Raise, To: 1}}, "raise to 1"},
		{badBot{Action{Kind: Check}}, "no check here"},
		{badBot{Action{Kind: 9}}, "no move kind 9"},
		{panicBot{}, "panicked"},
	} {
		_, err := Match(tc.bot, caller{}, 10, 1)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "hand 1") {
			t.Errorf("%T %v: %v, want an error with %q and the hand", tc.bot, tc.bot, err, tc.want)
		}
	}
}

func TestFacing(t *testing.T) {
	s := State{Seat: BigBlind, Round: Flop, History: []Event{
		{Seat: SmallBlind, Round: Preflop, Kind: Call},
		{Seat: BigBlind, Round: Preflop, Kind: Check},
		{Seat: BigBlind, Round: Flop, Kind: Check},
		{Seat: SmallBlind, Round: Flop, Kind: Raise},
	}}
	if !s.Facing() {
		t.Error("a bet after our check: not facing it")
	}
	s.History = s.History[:3]
	if s.Facing() {
		t.Error("our own check: facing")
	}
	s.History = append(s.History[:2], Event{Seat: SmallBlind, Round: Preflop, Kind: Raise})
	if s.Facing() {
		t.Error("last round's raise counted in this round")
	}
}

func TestMade(t *testing.T) {
	cards := func(names ...string) []Card {
		out := make([]Card, len(names))
		for i, n := range names {
			c, err := ParseCard(n)
			if err != nil {
				t.Fatal(err)
			}
			out[i] = c
		}
		return out
	}
	for _, tc := range []struct {
		hole, board []Card
		want        Category
	}{
		{cards("As", "Ad"), nil, Pair},
		{cards("As", "Kd"), nil, HighCard},
		{cards("As", "Kd"), cards("Ac", "Kh", "2s"), TwoPair},
		{cards("9h", "Th"), cards("Jh", "Qh", "Kh", "2d"), StraightFlush},
	} {
		s := State{Hole: [2]Card(tc.hole), Board: tc.board}
		if got := s.Made(); got != tc.want {
			t.Errorf("%v on %v: %v, want %v", tc.hole, tc.board, got, tc.want)
		}
	}
	if StraightFlush.String() != "a straight flush" || HighCard.String() != "high card" {
		t.Error("category names out of step with the engine's")
	}
}

// oddSizer bets or raises to one chip over the minimum whenever it can
// (sizes no menu has), and remembers each "to" it asked for.
type oddSizer struct{ asked []int }

func (o *oddSizer) Act(s State) Action {
	if s.Legal.CanRaise() && s.Legal.MinTo < s.Legal.MaxTo {
		o.asked = append(o.asked, s.Legal.MinTo+1)
		return Action{Kind: Raise, To: s.Legal.MinTo + 1}
	}
	return s.CheckOrCall()
}

// recorder calls, and notes every raise the other seat made, as its
// States show them.
type recorder struct{ raises map[[3]int]int }

func (r *recorder) Act(s State) Action {
	for i, e := range s.History {
		if e.Seat != s.Seat && e.Kind == Raise {
			r.raises[[3]int{s.Hand, i, e.Seat}] = e.To
		}
	}
	return s.CheckOrCall()
}

// Betting is open: any whole "raise to" in [MinTo, MaxTo] is a legal
// move, played as asked, not rounded to a menu size. MinTo+1 goes
// through the engine end to end and the other seat sees exactly it;
// a house bot, which keeps its menu inside, plays against it too.
func TestOddSizesAreAccepted(t *testing.T) {
	odd, rec := &oddSizer{}, &recorder{raises: map[[3]int]int{}}
	if _, err := Match(odd, rec, 400, 1); err != nil {
		t.Fatal(err)
	}
	seen := map[int]int{}
	for _, to := range rec.raises {
		seen[to]++
	}
	asked := map[int]int{}
	for _, to := range odd.asked {
		asked[to]++
	}
	if len(odd.asked) < 200 {
		t.Fatalf("only %d odd raises asked for", len(odd.asked))
	}
	// Every odd size the other seat saw was one asked for (raises the
	// hand ended on, by a fold, are never seen: hence <=).
	for to, n := range seen {
		if n > asked[to] {
			t.Fatalf("the other seat saw a raise to %d %d times; asked for %d times", to, n, asked[to])
		}
	}
	if len(seen) == 0 || seen[201] == 0 { // preflop min-raise from the small blind: 200, so 201
		t.Fatalf("no raise to 201 seen: %v", seen)
	}
	if _, err := Match(&oddSizer{}, testHouse(t, engine.UniformStrategy{}), 400, 2); err != nil {
		t.Fatal(err)
	}
}

// Equity: against one random hand, aces win about 85% before the flop,
// seven-deuce about 35%, the nuts on the river always; a seed repeats it.
func TestEquity(t *testing.T) {
	at := func(hole, board string, seed uint64) float64 {
		s := State{Rand: rand.New(rand.NewPCG(seed, 0))}
		for i := 0; i+1 < len(hole); i += 2 {
			s.Hole[i/2], _ = ParseCard(hole[i : i+2])
		}
		for i := 0; i+1 < len(board); i += 2 {
			c, _ := ParseCard(board[i : i+2])
			s.Board = append(s.Board, c)
		}
		return s.Equity(4000)
	}
	for _, tc := range []struct {
		hole, board string
		lo, hi      float64
	}{
		{"AcAd", "", 0.82, 0.88},
		{"7c2d", "", 0.31, 0.38},
		{"AhKh", "QhJhTh2c3d", 1, 1},
		{"7c2d", "AsAhAdKsKh", 0.4, 0.5}, // the board plays: a split (half) unless they hold the last ace or king
	} {
		if e := at(tc.hole, tc.board, 1); e < tc.lo || e > tc.hi {
			t.Errorf("%s on %q: equity %.3f, want %.2f-%.2f", tc.hole, tc.board, e, tc.lo, tc.hi)
		}
	}
	if at("9c8c", "Ks7c2d", 5) != at("9c8c", "Ks7c2d", 5) {
		t.Error("the same seed gave two answers")
	}
}
