package arena

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// guests builds a guest as a wasm bot, once per test run: the house
// pool's from guests/<name>, the hostile ones from testdata/guests/<name>.
var guests = struct {
	sync.Mutex
	prog map[string]*Program
}{prog: map[string]*Program{}}

func guest(t testing.TB, name string, flags ...string) *Program {
	t.Helper()
	key := name + " " + strings.Join(flags, " ")
	guests.Lock()
	defer guests.Unlock()
	if p := guests.prog[key]; p != nil {
		return p
	}
	p, err := Compile(buildGuest(t, name, flags...))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	guests.prog[key] = p
	return p
}

// guestDir is where a guest's package is.
func guestDir(name string) string {
	if _, err := os.Stat(filepath.Join("guests", name)); err == nil {
		return filepath.Join("guests", name)
	}
	return filepath.Join("testdata", "guests", name)
}

func buildGuest(t testing.TB, name string, flags ...string) []byte {
	t.Helper()
	wasm, err := Build(guestDir(name), flags...)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}
