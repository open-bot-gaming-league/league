package sdk

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

type closingCaller struct {
	caller
	closed *atomic.Int32
}

func (c closingCaller) Close() error { c.closed.Add(1); return nil }

func TestMatrix(t *testing.T) {
	var closed atomic.Int32
	cs := []Contender{
		{"caller", func() (Bot, error) { return closingCaller{closed: &closed}, nil }},
		{"tight", func() (Bot, error) { return tight{}, nil }},
		{"random", func() (Bot, error) { return Random{}, nil }},
	}
	got, err := Matrix(cs, 400, 7)
	if err != nil {
		t.Fatal(err)
	}
	// Each cell is that pair's Match, the same seed for all, and the other
	// side's cell is it negated.
	for i := range cs {
		for j := i + 1; j < len(cs); j++ {
			a, _ := cs[i].New()
			b, _ := cs[j].New()
			want, _ := Match(a, b, 400, 7)
			if got.Cells[i][j] != want {
				t.Errorf("%s vs %s: %+v, Match says %+v", cs[i].Name, cs[j].Name, got.Cells[i][j], want)
			}
			back := got.Cells[j][i]
			if back.BBPer100 != -want.BBPer100 || back.Net != -want.Net || back.SE != want.SE || back.Hands != want.Hands {
				t.Errorf("%s vs %s, the other side: %+v", cs[j].Name, cs[i].Name, back)
			}
		}
	}
	if closed.Load() < 2 { // caller plays two pairs in the matrix (and two more above)
		t.Errorf("closed %d times", closed.Load())
	}
	again, _ := Matrix(cs, 400, 7)
	if again.String() != got.String() {
		t.Error("the same seed gave another table")
	}
	text := got.String()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) < 4 || !strings.Contains(lines[0], "caller") || !strings.Contains(lines[0], "random") {
		t.Fatalf("table:\n%s", text)
	}
	for i, line := range lines[1:4] {
		if !strings.HasPrefix(line, cs[i].Name) || !strings.Contains(line, "±") {
			t.Errorf("row %d: %q", i, line)
		}
	}
}

// Close is the table's rule for "too close to call": within 2 SE of even.
// The printed table marks those cells "~" and says what that means.
func TestCloseCells(t *testing.T) {
	h := HeadToHead{
		Names: []string{"a", "b", "c"},
		Cells: [][]Result{
			{{}, {BBPer100: 10, SE: 2}, {BBPer100: -3, SE: 2}},
			{{BBPer100: -10, SE: 2}, {}, {BBPer100: 4, SE: 2}},
			{{BBPer100: 3, SE: 2}, {BBPer100: -4, SE: 2}, {}},
		},
	}
	if h.Close(0, 1) || h.Close(1, 0) {
		t.Error("+10 ± 2 (5 SE) called close")
	}
	if !h.Close(0, 2) || !h.Close(2, 0) {
		t.Error("-3 ± 2 (1.5 SE) not called close")
	}
	if h.Close(1, 2) { // exactly 2 SE: not within
		t.Error("+4 ± 2 (2 SE) called close")
	}
	text := h.String()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if !strings.Contains(lines[1], "  +10.0 ± 2.0") || !strings.Contains(lines[1], "~ -3.0 ± 2.0") {
		t.Errorf("row a: %q", lines[1])
	}
	// The columns line up: column b's cells start where its name does.
	col := strings.Index(lines[0], "b") // the header is plain ASCII; the rows have "±": count in runes
	for row, want := range map[int]string{1: "  +10.0", 2: "  -", 3: "  -4.0"} {
		if got := string([]rune(lines[row])[col:]); !strings.HasPrefix(got, want) {
			t.Errorf("row %d, column b: %q, want %q first:\n%s", row, got, want, text)
		}
	}
	if !strings.HasSuffix(text, "~ = within 2 SE: too close to call at this many hands; rerun with more -hands to separate them.\n") {
		t.Errorf("no note for the ~ cells:\n%s", text)
	}
	clear := HeadToHead{Names: []string{"a", "b"}, Cells: [][]Result{{{}, {BBPer100: 10, SE: 1}}, {{BBPer100: -10, SE: 1}, {}}}}
	if strings.Contains(clear.String(), "~") {
		t.Errorf("a table with no close cells has the note:\n%s", clear.String())
	}
}

func TestMatrixReportsAFailingPair(t *testing.T) {
	cs := []Contender{
		{"caller", func() (Bot, error) { return caller{}, nil }},
		{"missing", func() (Bot, error) { return nil, errors.New("no such file") }},
	}
	if _, err := Matrix(cs, 10, 1); err == nil || !strings.Contains(err.Error(), "caller vs missing: no such file") {
		t.Fatalf("%v", err)
	}
}

type zeroEquity struct{}

func (zeroEquity) Act(s State) Action { s.Equity(0); return s.CheckOrCall() }

// Equity with no samples is a mistake, said so: in a match, the bot's error.
func TestEquityNeedsSamples(t *testing.T) {
	_, err := Match(zeroEquity{}, caller{}, 2, 1)
	if err == nil || !strings.Contains(err.Error(), "Equity needs at least 1 sample, not 0") {
		t.Fatalf("%v", err)
	}
}
