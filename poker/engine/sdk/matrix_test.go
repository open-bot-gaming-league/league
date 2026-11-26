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
	if len(lines) != 4 || !strings.Contains(lines[0], "caller") || !strings.Contains(lines[0], "random") {
		t.Fatalf("table:\n%s", text)
	}
	for i, line := range lines[1:] {
		fields := strings.Fields(line)
		if fields[0] != cs[i].Name || fields[1+3*i] != "-" {
			t.Errorf("row %d: %q", i, line)
		}
		if !strings.Contains(line, "±") {
			t.Errorf("row %d has no results: %q", i, line)
		}
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
