package main

import (
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/engine/league"
)

// The fixture: the first full-size ranked table (league/testdata).
const recordsDir = "../../league/testdata"

func get(t *testing.T, url string) (int, string, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), string(b)
}

// section is the page's text from the element with that id to its end tag.
func section(t *testing.T, page, id, end string) string {
	t.Helper()
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no #%s on the page", id)
	}
	j := strings.Index(page[i:], end)
	if j < 0 {
		t.Fatalf("#%s has no %s", id, end)
	}
	return page[i : i+j]
}

var cellText = regexp.MustCompile(`<td[^>]*>([^<]*)</td>`)

func TestResultsPage(t *testing.T) {
	ts, s := testTable(t)
	s.records = league.RecordDir(recordsDir)
	status, ctype, page := get(t, ts.URL+"/results/table-p8")
	if status != http.StatusOK || !strings.HasPrefix(ctype, "text/html") {
		t.Fatalf("%d %s\n%s", status, ctype, page)
	}
	page = html.UnescapeString(page) // html/template writes "+" as "&#43;"
	for _, want := range []string{
		"<h1>Example placed 10th of 10</h1>",
		`<p class="lead">Example beat 5 of 9, lost to 4, too close to call 0.</p>`,
		"1,000,000 hands",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page has no %q", want)
		}
	}
	// The challenger's row per opponent, best first: bb/100 ± SE and the verdict.
	var rows []string
	for _, tr := range strings.Split(section(t, page, "opponents", "</table>"), "<tr>")[2:] {
		var cells []string
		for _, m := range cellText.FindAllStringSubmatch(tr, -1) {
			cells = append(cells, m[1])
		}
		rows = append(rows, strings.Join(cells, " | "))
	}
	want := []string{
		"Clam (seat 2) | +17.3 ± 0.4 | won", "Clam (seat 8) | +17.3 ± 0.4 | won",
		"Turtle (seat 1) | +3.7 ± 0.8 | won", "Turtle (seat 4) | +3.7 ± 0.8 | won", "Turtle (seat 7) | +3.7 ± 0.8 | won",
		"Ox (seat 3) | -47.9 ± 0.9 | lost", "Ox (seat 5) | -47.9 ± 0.9 | lost", "Ox (seat 9) | -47.9 ± 0.9 | lost",
		"Dove | -89.0 ± 1.2 | lost",
	}
	if strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Errorf("opponent rows:\n%s\nwant\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
	// Places: ten, best first, the challenger's marked.
	places := section(t, page, "places", "</table>")
	if n := strings.Count(places, "<tr"); n != 11 {
		t.Errorf("%d rows in the places, want a header and 10", n)
	}
	if !strings.Contains(places, `<tr class="you"><td class="num">10</td><td>Example <span class="dim">(your bot)</span></td><td class="num neg">-187.0</td><td class="num">5</td><td class="num">4</td><td class="num">0</td></tr>`) {
		t.Errorf("no row for the challenger in 10th:\n%s", places)
	}
	if strings.Index(places, "Ox (seat 9)") > strings.Index(places, "Dove") {
		t.Error("the places are not best first")
	}
	// The grid: collapsed, every seat against every other.
	grid := section(t, page, "grid", "</details>")
	if strings.Contains(grid[:strings.Index(grid, ">")], "open") {
		t.Error("the grid is open by default")
	}
	if n := strings.Count(grid, "<td"); n != 100 {
		t.Errorf("%d grid cells, want 100", n)
	}
	if n := strings.Count(grid, `class="self"`); n != 10 {
		t.Errorf("%d diagonal cells, want 10", n)
	}
	for _, bad := range []string{"xploit", `href="/`, `src="/`, "-0.0 "} {
		if strings.Contains(page, bad) {
			t.Errorf("the page has %q", bad)
		}
	}
}

func TestResultsAPI(t *testing.T) {
	ts, s := testTable(t)
	s.records = league.RecordDir(recordsDir)
	var got resultsAnswer
	if status := do(t, ts, "GET", "/api/results/table-p8", nil, &got); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	text, err := os.ReadFile(filepath.Join(recordsDir, "table-p8.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := resultsAnswer{ID: "table-p8", Placed: "10th of 10", Summary: "beat 5 of 9, lost to 4, too close to call 0", Text: string(text)}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestResultsRefused(t *testing.T) {
	ts, s := testTable(t)
	for _, path := range []string{"/results/table-p8", "/api/results/table-p8"} {
		if status, _, body := get(t, ts.URL+path); status != http.StatusNotFound || !strings.Contains(body, "keeps no ranked tables") {
			t.Errorf("%s with no records: %d %s", path, status, body)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"format": 1, "seats": [{"seat": 0}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.records = league.RecordDir(dir)
	for path, want := range map[string]int{
		"/results/nope": 404, "/api/results/nope": 404,
		"/results/no%2E%2E%2Fescape": 404, "/api/results/a.b": 404,
		"/results/broken": 500, "/api/results/broken": 500,
	} {
		if status, _, body := get(t, ts.URL+path); status != want {
			t.Errorf("%s: %d, want %d: %s", path, status, want, body)
		}
	}
}

// A seat whose sandbox played for it: the page says where, and what
// stopped it; a table without that has no such section.
func TestResultsPageFaults(t *testing.T) {
	ts, s := testTable(t)
	rec, err := league.RecordDir(recordsDir).Record("table-p8")
	if err != nil {
		t.Fatal(err)
	}
	five := int64(5)
	rec.Usage[0].Forfeited = &five
	rec.Usage[0].Faults = []league.PairFault{{Opponent: 1, Fault: league.Fault{Kind: "fuel", Decision: 3, Hand: 2, Detail: "out of decision fuel"}}}
	dir := t.TempDir()
	b, err := rec.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "faulty.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	s.records = league.RecordDir(dir)
	status, _, page := get(t, ts.URL+"/results/faulty")
	if status != http.StatusOK {
		t.Fatalf("%d\n%s", status, page)
	}
	page = html.UnescapeString(page)
	for _, want := range []string{
		"<tr><td>Example</td><td class=\"num\">5</td><td>in 1 of its 9 pairs: fuel x1</td></tr>",
		"<tr><td>Turtle (seat 1)</td><td>fuel</td><td class=\"num\">3</td><td class=\"num\">2</td><td class=\"small\">out of decision fuel</td></tr>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page has no %q", want)
		}
	}
	s.records = league.RecordDir(recordsDir)
	if _, _, clean := get(t, ts.URL+"/results/table-p8"); strings.Contains(clean, `id="faults"`) {
		t.Error("a table without faults has a faults section")
	}
}
