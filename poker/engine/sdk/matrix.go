package sdk

import (
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"strings"
	"sync"
)

// Contender is a bot in a matrix: its name, and how to make one. New is
// called once per pair, so a bot that keeps state (a House) is never
// shared between matches running at once; one that is an io.Closer is
// closed after its match.
type Contender struct {
	Name string
	New  func() (Bot, error)
}

// HeadToHead is every pair of contenders' head to head: Cells[i][j] is how
// contender i did against contender j (Cells[i][i] is empty).
type HeadToHead struct {
	Names []string
	Cells [][]Result
}

// Matrix plays every pair of contenders against each other with Match
// (duplicate deals, hands each, the same seed for every pair, several
// pairs at once) and returns the table of results. Each pair is played once:
// Cells[j][i] is Cells[i][j] seen from the other side.
func Matrix(cs []Contender, hands int, seed uint64) (HeadToHead, error) {
	n := len(cs)
	t := HeadToHead{Names: make([]string, n), Cells: make([][]Result, n)}
	for i, c := range cs {
		t.Names[i] = c.Name
		t.Cells[i] = make([]Result, n)
	}
	type pair struct{ i, j int }
	pairs := make(chan pair)
	errs := make([]error, 0)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), max(1, n*(n-1)/2)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range pairs {
				res, err := playPair(cs[p.i], cs[p.j], hands, seed)
				mu.Lock()
				if err != nil {
					errs = append(errs, fmt.Errorf("%s vs %s: %w", cs[p.i].Name, cs[p.j].Name, err))
				} else {
					t.Cells[p.i][p.j] = res
					t.Cells[p.j][p.i] = Result{Hands: res.Hands, Net: -res.Net, BBPer100: -res.BBPer100, SE: res.SE}
				}
				mu.Unlock()
			}
		}()
	}
	for i := range n {
		for j := i + 1; j < n; j++ {
			pairs <- pair{i, j}
		}
	}
	close(pairs)
	wg.Wait()
	return t, errors.Join(errs...)
}

func playPair(a, b Contender, hands int, seed uint64) (Result, error) {
	ba, err := a.New()
	if err != nil {
		return Result{}, err
	}
	defer closeBot(ba)
	bb, err := b.New()
	if err != nil {
		return Result{}, err
	}
	defer closeBot(bb)
	return Match(ba, bb, hands, seed)
}

func closeBot(b Bot) {
	if c, ok := b.(io.Closer); ok {
		c.Close()
	}
}

// Close reports whether contender i's result against j is within two
// standard errors of even: too close to call at this many hands. (A
// contender against itself counts as close.)
func (t HeadToHead) Close(i, j int) bool {
	r := t.Cells[i][j]
	return i == j || math.Abs(r.BBPer100) < 2*r.SE
}

// String is the table to print: one row per contender, its bb/100 ± SE
// against each column, a close result (Close) marked "~", and, when any
// is, a line saying what the mark means.
func (t HeadToHead) String() string {
	anyClose := false
	cell := func(i, j int) string {
		if i == j {
			return "  -"
		}
		r := t.Cells[i][j]
		s := fmt.Sprintf("%+.1f ± %.1f", r.BBPer100, r.SE)
		if t.Close(i, j) {
			anyClose = true
			return "~ " + s
		}
		return "  " + s
	}
	width := make([]int, len(t.Names)+1) // the names' column, then one per contender
	for j, name := range t.Names {
		width[0] = max(width[0], len(name))
		width[j+1] = len(name)
		for i := range t.Names {
			width[j+1] = max(width[j+1], len([]rune(cell(i, j))))
		}
	}
	var b strings.Builder
	pad := func(s string, w int) string { return s + strings.Repeat(" ", w-len([]rune(s))) }
	line := func(first string, rest func(j int) string) {
		var l strings.Builder
		l.WriteString(pad(first, width[0]))
		for j := range t.Names {
			l.WriteString("  " + pad(rest(j), width[j+1]))
		}
		b.WriteString(strings.TrimRight(l.String(), " ") + "\n")
	}
	line("", func(j int) string { return t.Names[j] })
	for i, name := range t.Names {
		line(name, func(j int) string { return cell(i, j) })
	}
	if anyClose {
		b.WriteString("\n~ = within 2 SE: too close to call at this many hands; rerun with more -hands to separate them.\n")
	}
	return b.String()
}
