package league

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/table-p8.txt from the report")

// fixture is the first full-size ranked table: the SDK's example bot
// against nine seats drawn from the house pool (the default was nine
// then; it is ten now), 1,000,000 hands a pair.
func fixture(t *testing.T) *Record {
	t.Helper()
	r, err := RecordDir("testdata").Record("table-p8")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReportOfTheFirstTable(t *testing.T) {
	rep, err := NewReport(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rep.Summary(), "beat 5 of 9, lost to 4, too close to call 0"; got != want {
		t.Errorf("summary %q, want %q", got, want)
	}
	if got := rep.Placed(); got != "10th of 10" {
		t.Errorf("placed %q", got)
	}
	ch := rep.Challenger
	if ch.Seat != 0 || ch.Label != "Example" || ch.Won != rep.Beat || ch.Lost != rep.LostTo || ch.Close != rep.TooClose {
		t.Errorf("the challenger's standing %+v disagrees with its rows (%d, %d, %d)", ch, rep.Beat, rep.LostTo, rep.TooClose)
	}
	var order []string
	for _, o := range rep.Opponents {
		order = append(order, o.Label+" "+string(o.Verdict))
	}
	want := []string{
		"Clam (seat 2) won", "Clam (seat 8) won",
		"Turtle (seat 1) won", "Turtle (seat 4) won", "Turtle (seat 7) won",
		"Ox (seat 3) lost", "Ox (seat 5) lost", "Ox (seat 9) lost",
		"Dove lost",
	}
	if strings.Join(order, ", ") != strings.Join(want, ", ") {
		t.Errorf("opponents, best first:\n got %v\nwant %v", order, want)
	}
	// The grid: each pair from both sides, and the only close cells are
	// the copies' pairs.
	for i, row := range rep.Grid {
		for j, c := range row {
			if c.Self != (i == j) {
				t.Fatalf("cell %d,%d: Self %v", i, j, c.Self)
			}
			if i == j {
				continue
			}
			if back := rep.Grid[j][i].Result; back.BBPer100 != -c.Result.BBPer100 || back.SE != c.Result.SE {
				t.Errorf("cell %d,%d is %v, %d,%d is %v", i, j, c.Result, j, i, back)
			}
			copies := strings.SplitN(rep.Labels[i], " ", 2)[0] == strings.SplitN(rep.Labels[j], " ", 2)[0]
			if (c.Verdict == TooClose) != copies {
				t.Errorf("%s vs %s: %s %v", rep.Labels[i], rep.Labels[j], c.Verdict, c.Result)
			}
		}
	}
	text := rep.Text()
	golden := filepath.Join("testdata", "table-p8.txt")
	if *update {
		if err := os.WriteFile(golden, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if text != string(b) {
		t.Errorf("text differs from %s (go test -run TestReportOfTheFirstTable -update rewrites it):\n%s", golden, text)
	}
	if strings.Contains(strings.ToLower(text), "exploit") {
		t.Error("the text says exploitability")
	}
}

// table makes a record of n seats, seat 0 the challenger, from each
// pair's bb/100 and SE (a, b in order), placed as the runner places it.
func table(n int, results map[[2]int][2]float64) *Record {
	r := &Record{Format: Format, HandsPerPair: 1000}
	for i := range n {
		r.Seats = append(r.Seats, Seat{Seat: i, Challenger: i == 0, Bot: Bot{ID: string(rune('a' + i)), Name: string(rune('A' + i))}})
	}
	for a := range n {
		for b := a + 1; b < n; b++ {
			x := results[[2]int{a, b}]
			r.Pairs = append(r.Pairs, Pair{A: a, B: b, Hands: 1000, Net: int(x[0] * 10), BBPer100: x[0], SE: x[1]})
		}
	}
	r.place()
	return r
}

func TestReportCountsAndTies(t *testing.T) {
	// A beats B, is too close with C (1.5 SE), and loses to D at exactly
	// 2 SE (not close: the sdk's rule). B and C tie for 2nd.
	r := table(4, map[[2]int][2]float64{
		{0, 1}: {10, 1}, {0, 2}: {3, 2}, {0, 3}: {-4, 2},
		{1, 2}: {0, 0}, {1, 3}: {8, 1}, {2, 3}: {1, 1},
	})
	rep, err := NewReport(r)
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Summary(); got != "beat 1 of 3, lost to 1, too close to call 1" {
		t.Errorf("summary %q", got)
	}
	if rep.Challenger.Won != 1 || rep.Challenger.Lost != 1 || rep.Challenger.Close != 1 {
		t.Errorf("the challenger's place counts %+v", rep.Challenger)
	}
	var places []string
	for _, p := range rep.Places {
		places = append(places, Ordinal(p.Place)+" "+p.Label)
		if p.Tied != (p.Label == "B" || p.Label == "C") {
			t.Errorf("%s: tied %v", p.Label, p.Tied)
		}
	}
	if got := strings.Join(places, ", "); got != "1st A, 2nd B, 2nd C, 4th D" {
		t.Errorf("places %s", got)
	}
	if rep.Grid[1][2].Verdict != TooClose {
		t.Errorf("exactly even: %s", rep.Grid[1][2].Verdict)
	}
	if !strings.HasPrefix(rep.Text(), "A placed 1st of 4, +9.0 bb/100 summed over its 3 opponents.\nIt beat 1 of 3, lost to 1, too close to call 1.\n") {
		t.Errorf("text:\n%s", rep.Text())
	}
	// A seat tied with the challenger says so.
	r = table(3, map[[2]int][2]float64{{0, 1}: {-5, 1}, {0, 2}: {-5, 1}, {1, 2}: {0, 0}})
	if rep, err = NewReport(r); err != nil || rep.Placed() != "3rd of 3" {
		t.Errorf("%v %q", err, rep.Placed())
	}
	r = table(3, map[[2]int][2]float64{{0, 1}: {5, 1}, {0, 2}: {0, 0}, {1, 2}: {-5, 1}})
	if rep, err = NewReport(r); err != nil || rep.Placed() != "tied 1st of 3" {
		t.Errorf("%v %q", err, rep.Placed())
	}
}

func TestReportRefusesARecordThatDoesNotFit(t *testing.T) {
	for name, spoil := range map[string]func(*Record){
		"no challenger":     func(r *Record) { r.Seats[0].Challenger = false },
		"two challengers":   func(r *Record) { r.Seats[1].Challenger = true },
		"a pair missing":    func(r *Record) { r.Pairs = r.Pairs[1:] },
		"a pair twice":      func(r *Record) { r.Pairs[1] = r.Pairs[0] },
		"a seat too far":    func(r *Record) { r.Pairs[0].B = 7 },
		"a place short":     func(r *Record) { r.Places = r.Places[1:] },
		"seats misnumbered": func(r *Record) { r.Seats[2].Seat = 5 },
	} {
		r := table(3, nil)
		spoil(r)
		if _, err := NewReport(r); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestOrdinalAndThousands(t *testing.T) {
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 10: "10th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd", 111: "111th", 101: "101st"} {
		if got := Ordinal(n); got != want {
			t.Errorf("Ordinal(%d) = %q", n, got)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1000000: "1,000,000", -20000: "-20,000"} {
		if got := Thousands(n); got != want {
			t.Errorf("Thousands(%d) = %q", n, got)
		}
	}
}

func TestRecordDir(t *testing.T) {
	d := RecordDir(t.TempDir())
	if err := os.WriteFile(filepath.Join(string(d), "bad.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(string(d), "..", "outside.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"missing", "", "../outside", "a/b", "a.json", strings.Repeat("x", 65)} {
		if _, err := d.Record(id); !errors.Is(err, ErrNoRecord) {
			t.Errorf("%q: %v, want ErrNoRecord", id, err)
		}
	}
	if _, err := d.Record("bad"); err == nil || errors.Is(err, ErrNoRecord) {
		t.Errorf("a broken record: %v", err)
	}
	if !ValidID("table-p8") || !ValidID(strings.Repeat("x", 64)) {
		t.Error("a good id refused")
	}
}
