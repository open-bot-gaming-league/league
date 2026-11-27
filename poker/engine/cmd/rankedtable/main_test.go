package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestPlayErrors(t *testing.T) {
	t.Setenv("OBGL_HOUSE_CACHE", t.TempDir()) // nothing downloaded
	var w bytes.Buffer
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "-challenger is required"},
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
