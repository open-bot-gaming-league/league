package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/arena"
	"github.com/open-bot-gaming-league/league/poker/engine/league"
)

func TestPlayAndCost(t *testing.T) {
	out := filepath.Join(t.TempDir(), "table.json")
	var stdout, stderr bytes.Buffer
	err := play([]string{"-challenger", "shark", "-pool", "dove, clam", "-seats", "3", "-hands", "200", "-seed", "5", "-out", out}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), " 1. seat ") || !strings.Contains(stderr.String(), "billed: $") {
		t.Errorf("stdout %q, stderr:\n%s", stdout.String(), stderr.String())
	}
	var c bytes.Buffer
	if err := cost([]string{"-vcpu", "2", "-gib", "1", out}, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.String(), "2 vCPU, 1 GiB") || !strings.Contains(c.String(), "run/pricing") {
		t.Errorf("cost:\n%s", c.String())
	}
	if err := cost([]string{"-vcpu-price", "1", out}, &c); err != nil || !strings.Contains(c.String(), "given on the command line") {
		t.Errorf("cost with a price given: %v\n%s", err, c.String())
	}

	// To stdout: the same table's results, byte for byte.
	stdout.Reset()
	if err := play([]string{"-challenger", "shark", "-pool", "dove,clam", "-seats", "3", "-hands", "200", "-seed", "5", "-parallel", "1"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	again, err := league.ReadRecord(stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	first, err := readFile(out)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := first.Results().JSON()
	b, _ := again.Results().JSON()
	if !bytes.Equal(a, b) {
		t.Errorf("the same table twice:\n%s\n%s", a, b)
	}
}

// TestWasmTable: every seat as a wasm bot plays the native table's
// results, places and chips, and reports what each seat used.
func TestWasmTable(t *testing.T) {
	dir := t.TempDir()
	wasm, err := arena.Build("../../guests/shark")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shark.wasm"), wasm, 0o644); err != nil {
		t.Fatal(err)
	}
	common := []string{"-pool", "dove,clam", "-seats", "3", "-hands", "400", "-seed", "5"}
	var native, sandboxed, stderr bytes.Buffer
	if err := play(append([]string{"-challenger", "shark"}, common...), &native, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := play(append([]string{"-challenger-wasm", filepath.Join(dir, "shark.wasm"), "-challenger-data", dir,
		"-wasm-pool", "-guests", "../../guests"}, common...), &sandboxed, &stderr); err != nil {
		t.Fatal(err)
	}
	n, err := league.ReadRecord(native.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	w, err := league.ReadRecord(sandboxed.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for i := range n.Places {
		n.Places[i].Name, w.Places[i].Name = "", ""
	}
	a, _ := json.Marshal([]any{n.Pairs, n.Places})
	b, _ := json.Marshal([]any{w.Pairs, w.Places})
	if !bytes.Equal(a, b) {
		t.Fatalf("native and wasm tables differ:\n%s\n%s", a, b)
	}
	for _, u := range w.Usage {
		if u.Fuel == nil || *u.Fuel == 0 || u.MemoryPeak == nil || u.Forfeited == nil || *u.Forfeited != 0 || u.Faults != nil {
			t.Errorf("seat %d usage %+v", u.Seat, u)
		}
	}
	if !strings.Contains(w.Seats[0].Version, "wasm sha256:") || !strings.HasSuffix(w.Seats[0].Version, "data "+filepath.Base(dir)) {
		t.Errorf("challenger version %q", w.Seats[0].Version)
	}

	// The challenger as the queue gives it (a spec: its files by host path,
	// its owner), against a pool of prebuilt guests: the same table.
	guests := t.TempDir()
	for _, id := range []string{"dove", "clam"} {
		g, err := arena.Build("../../guests/" + id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(guests, id+".wasm"), g, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	owner := league.Owner{Account: "github:1001", Bot: "shark-copy", Version: 3, Submission: "ab"}
	specJSON, _ := json.Marshal(spec{Name: "Shark copy", Wasm: filepath.Join(dir, "shark.wasm"),
		Data: map[string]string{"tables/shark.wasm": filepath.Join(dir, "shark.wasm")}, Owner: &owner})
	specPath := filepath.Join(dir, "challenger.json")
	if err := os.WriteFile(specPath, specJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	var queued bytes.Buffer
	if err := play(append([]string{"-challenger-spec", specPath, "-wasm-pool", "-guests", guests}, common...), &queued, &stderr); err != nil {
		t.Fatal(err)
	}
	q, err := league.ReadRecord(queued.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for i := range q.Places {
		q.Places[i].Name = ""
	}
	if c, _ := json.Marshal([]any{q.Pairs, q.Places}); !bytes.Equal(a, c) {
		t.Fatalf("the spec's table differs from the native one:\n%s\n%s", a, c)
	}
	s := q.Seats[0]
	if s.Name != "Shark copy" || s.Owner == nil || *s.Owner != owner || s.Version != w.Seats[0].Version[:len("wasm sha256:")+64] {
		t.Errorf("challenger seat %+v", s)
	}
	if q.Pool[0].Version != w.Pool[0].Version {
		t.Errorf("a prebuilt Dove is %q, the one built from source %q", q.Pool[0].Version, w.Pool[0].Version)
	}
	os.WriteFile(specPath, []byte(`{"name": "x", "wasm": "x.wasm", "colour": "red"}`), 0o644)
	if err := play([]string{"-challenger-spec", specPath, "-pool", "dove"}, &queued, &stderr); err == nil || !strings.Contains(err.Error(), "colour") {
		t.Errorf("a spec with a field it does not know: %v", err)
	}
}

func TestPlayErrors(t *testing.T) {
	t.Setenv("OBGL_HOUSE_CACHE", t.TempDir()) // nothing downloaded
	var w bytes.Buffer
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "one of -challenger, -challenger-wasm and -challenger-spec is required"},
		{[]string{"-challenger", "dove", "-challenger-wasm", "x.wasm"}, "one of -challenger, -challenger-wasm and -challenger-spec is required"},
		{[]string{"-challenger", "dove", "-pool", "dove", "-draw", "nope"}, `no draw "nope"`},
		{[]string{"-challenger", "walrus", "-pool", "dove"}, `no house bot "walrus"`},
		{[]string{"-challenger", "dove", "-pool", "clam,turtle"}, "Turtle is not downloaded"},
		{[]string{"-challenger", "dove", "-pool", ""}, "empty pool"},
	} {
		if err := play(tc.args, &w, &w); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("play %q: %v, want %q", tc.args, err, tc.want)
		}
	}
	if err := cost([]string{filepath.Join(t.TempDir(), "none.json")}, &w); err == nil {
		t.Error("priced a missing file")
	}
}

func readFile(path string) (*league.Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return league.ReadRecord(b)
}
