package league

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

// A submitted challenger's record names its owner; a house-only table's
// record is the same bytes as before owners existed.
func TestOwner(t *testing.T) {
	b, err := os.ReadFile("testdata/table-p8.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := ReadRecord(b)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := r.JSON(); err != nil || !bytes.Equal(again, b) {
		t.Fatalf("a record from before owners does not round-trip byte for byte (%v)", err)
	}

	cfg := Config{Seats: 2, Hands: 20, Seed: 3}
	plain, err := Play(cfg, starter("dove"), starters())
	if err != nil {
		t.Fatal(err)
	}
	js, _ := plain.JSON()
	if bytes.Contains(js, []byte(`"owner"`)) || bytes.Contains(js, []byte(`"draw"`)) {
		t.Errorf("a house challenger's record names an owner or a draw:\n%s", js)
	}
	ch := starter("dove")
	ch.Owner = &Owner{Account: "github:1001", Bot: "mybot", Version: 2, Submission: "ab12"}
	owned, err := Play(cfg, ch, starters())
	if err != nil {
		t.Fatal(err)
	}
	if got := owned.Seats[0].Owner; got == nil || *got != *ch.Owner {
		t.Fatalf("seat 0's owner %+v, want %+v", got, ch.Owner)
	}
	for _, s := range owned.Seats[1:] {
		if s.Owner != nil {
			t.Errorf("pool seat %d has an owner", s.Seat)
		}
	}
	js, _ = owned.JSON()
	back, err := ReadRecord(js)
	if err != nil || *back.Seats[0].Owner != *ch.Owner {
		t.Fatalf("read back: %v, %+v", err, back)
	}
	owned.Seats[0].Owner = nil
	if !bytes.Equal(mustJSON(t, owned.Results()), mustJSON(t, plain.Results())) {
		t.Error("the owner changed more of the record than seat 0's owner")
	}
}

func mustJSON(t *testing.T, r *Record) []byte {
	t.Helper()
	b, err := r.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDrawPolicy(t *testing.T) {
	cfg := Config{Seats: 4, Hands: 10, Seed: 5}
	a, err := Play(cfg, starter("dove"), starters())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Draw = DrawCopies
	b, err := Play(cfg, starter("dove"), starters())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustJSON(t, a.Results()), mustJSON(t, b.Results())) {
		t.Error("the default draw is not DrawCopies")
	}
	cfg.Draw = "no-copies"
	if _, err := Play(cfg, starter("dove"), starters()); err == nil || !strings.Contains(err.Error(), `no draw "no-copies"`) {
		t.Errorf("an unknown draw: %v", err)
	}
}

// A seat whose sandbox played for it is in the report, page and text.
func TestReportFaults(t *testing.T) {
	var peak int64
	ch := Entrant{ID: "m", Name: "Metered", Version: "1", New: func() (sdk.Bot, error) {
		peak++
		return &metered{peak: peak}, nil
	}}
	rec, err := Play(Config{Seats: 3, Hands: 20, Seed: 1, Parallel: 1}, ch, starters()[:1])
	if err != nil {
		t.Fatal(err)
	}
	rep, err := NewReport(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Faulted) != 1 {
		t.Fatalf("faulted %+v, want the challenger alone", rep.Faulted)
	}
	f := rep.Faulted[0]
	// Parallel 1: the pairs in order, so the challenger's peaks are 1, 2, 3
	// and only the pair against seat 2 faults.
	if f.Seat != 0 || f.Forfeited != 3 || f.Pairs != 3 || len(f.Faults) != 1 || f.Faults[0].Opponent != "Dove (seat 2)" || f.Kinds() != "trap x1" {
		t.Errorf("faulted %+v", f)
	}
	if !strings.Contains(rep.Text(), "\n  Metered: 3 decisions played for it; stopped in 1 of its 3 pairs (trap x1)\n") {
		t.Errorf("the text does not say what the sandbox did:\n%s", rep.Text())
	}
	clean, err := NewReport(fixture(t))
	if err != nil || clean.Faulted != nil || strings.Contains(clean.Text(), "sandbox") {
		t.Errorf("a table without faults reports some: %v %+v", err, clean.Faulted)
	}
	rec.Usage[0].Faults[0].Opponent = 0
	if _, err := NewReport(rec); err == nil {
		t.Error("a fault against the seat itself was read")
	}
}
