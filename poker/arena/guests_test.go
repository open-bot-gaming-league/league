package arena

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// guests builds testdata/guests/<name> as a wasm bot, once per test run.
var guests = struct {
	sync.Mutex
	dir  string
	prog map[string]*Program
}{prog: map[string]*Program{}}

func guest(t testing.TB, name string, flags ...string) *Program {
	t.Helper()
	key := name + fmtFlags(flags)
	guests.Lock()
	defer guests.Unlock()
	if p := guests.prog[key]; p != nil {
		return p
	}
	wasm := buildGuest(t, name, flags...)
	p, err := Compile(wasm)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	guests.prog[key] = p
	return p
}

func fmtFlags(flags []string) string {
	s := ""
	for _, f := range flags {
		s += " " + f
	}
	return s
}

// buildGuest is the module's bytes; the default flags are a reactor build.
func buildGuest(t testing.TB, name string, flags ...string) []byte {
	t.Helper()
	if guests.dir == "" {
		dir, err := os.MkdirTemp("", "arena-guests-")
		if err != nil {
			t.Fatal(err)
		}
		guests.dir = dir
	}
	if flags == nil {
		flags = []string{"-buildmode=c-shared"}
	}
	out := filepath.Join(guests.dir, name+fmtFlags(flags)+".wasm")
	args := append([]string{"build", "-o", out}, flags...)
	cmd := exec.Command("go", append(args, "./testdata/guests/"+name)...)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", name, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMain(m *testing.M) {
	code := m.Run()
	if guests.dir != "" {
		os.RemoveAll(guests.dir)
	}
	os.Exit(code)
}
