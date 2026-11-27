package arena

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v51"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// The hostile bots: each breaks a rule, and the sandbox must stop it
// with that rule's limit, finish the match, and say what
// happened in the Report.

// checkCaller, checkFolder and panicker are the native bots whose chips
// a wasm bot must match when it plays the same moves (or has them played
// for it).
type checkCaller struct{}

func (checkCaller) Act(s sdk.State) sdk.Action { return s.CheckOrCall() }

type checkFolder struct{}

func (checkFolder) Act(s sdk.State) sdk.Action { return s.CheckOrFold() }

// panicker is the panic guest as the sandbox plays it: check or call, but
// check or fold on every tenth decision.
type panicker struct{ n int }

func (p *panicker) Act(s sdk.State) sdk.Action {
	if p.n++; p.n%10 == 0 {
		return s.CheckOrFold()
	}
	return s.CheckOrCall()
}

type hostile struct {
	guest  string
	hands  int
	limits Limits
	data   func(t *testing.T) Config
	fault  FaultKind // "" for none
	same   sdk.Bot   // if set: the chips must equal this native bot's
	check  func(t *testing.T, r Report)
}

// play runs the guest for h.hands against Shark, and returns its report.
func (h hostile) play(t *testing.T, p *Program) Report {
	t.Helper()
	cfg := Config{Limits: h.limits}
	if h.data != nil {
		cfg = h.data(t)
		cfg.Limits = h.limits
	}
	b, err := p.Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	hands := cmp(h.hands, 200)
	start := time.Now()
	res, err := sdk.Match(b, bots.Shark{}, hands, 1)
	if err != nil {
		t.Fatalf("the match did not finish: %v", err)
	}
	r := b.Report()
	t.Logf("outcome: %s; %d decisions, %d forfeited (%d illegal, %d errors); fuel %d (max decision %d), start fuel %d; memory %d MiB; output %d B; wall %v; %v",
		faultString(r.Fault), r.Decisions, r.Forfeited(), r.Illegal, r.Errors, r.Fuel, r.MaxDecisionFuel, r.StartFuel,
		r.MemoryBytes>>20, r.OutputBytes, time.Since(start).Round(time.Millisecond), res)
	if r.Decisions == 0 || res.Hands != hands+hands%2 {
		t.Fatalf("the match played %d hands, %d decisions", res.Hands, r.Decisions)
	}
	switch {
	case h.fault == "" && r.Fault != nil:
		t.Fatalf("want no fault, got %v", r.Fault)
	case h.fault != "" && (r.Fault == nil || r.Fault.Kind != h.fault):
		t.Fatalf("want a %s fault, got %v", h.fault, faultString(r.Fault))
	}
	if r.Fault != nil && r.Fault.Decision > 0 && r.Forfeited() < r.Decisions-r.Fault.Decision+1 {
		t.Fatalf("faulted at decision %d of %d but only %d forfeited", r.Fault.Decision, r.Decisions, r.Forfeited())
	}
	if h.same != nil {
		want, err := sdk.Match(h.same, bots.Shark{}, hands, 1)
		if err != nil {
			t.Fatal(err)
		}
		if res.Net != want.Net {
			t.Fatalf("net %d chips; the native %T it should play as won %d", res.Net, h.same, want.Net)
		}
	}
	if h.check != nil {
		h.check(t, r)
	}
	return r
}

func cmp(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}

func faultString(f *Fault) string {
	if f == nil {
		return "no fault"
	}
	return f.String()
}

func TestHostileBots(t *testing.T) {
	cases := map[string]hostile{
		"loop":     {guest: "loop", fault: FaultDecisionFuel, same: checkFolder{}, check: faultAt(1)},
		"membomb":  {guest: "membomb", fault: FaultMemory, same: checkFolder{}, check: detailHas("fatal error: out of memory")},
		"bigalloc": {guest: "bigalloc", fault: FaultTrap, same: checkFolder{}, check: detailHas("fatal error: out of memory")},
		"recurse":  {guest: "recurse", fault: FaultTrap, same: checkFolder{}, check: detailHas("call stack exhausted")},
		"panic": {guest: "panic", same: &panicker{}, check: func(t *testing.T, r Report) {
			if r.Errors != r.Decisions/10 || !strings.Contains(r.FirstError, "panic: decision 10") {
				t.Fatalf("%d errors in %d decisions, first %q", r.Errors, r.Decisions, r.FirstError)
			}
		}},
		"deadlock": {guest: "deadlock", fault: FaultTrap, same: checkFolder{}, check: detailHas("fatal error: all goroutines are asleep - deadlock!")},
		"exit": {guest: "exit", fault: FaultExit, same: checkFolder{}, check: func(t *testing.T, r Report) {
			if r.Fault.Detail != "exit code 3" {
				t.Fatalf("detail %q", r.Fault.Detail)
			}
		}},
		"flood": {guest: "flood", fault: FaultDecisionFuel, same: checkFolder{}, check: func(t *testing.T, r Report) {
			if len(r.Output) != DefaultLimits().OutputBytes || r.OutputBytes < 500<<20 {
				t.Fatalf("kept %d bytes of %d", len(r.Output), r.OutputBytes)
			}
		}},
		"chatty": {guest: "chatty", same: checkCaller{}, check: func(t *testing.T, r Report) {
			if len(r.Output) != DefaultLimits().OutputBytes || r.OutputBytes != int64(r.Decisions)*(10<<10+1) {
				t.Fatalf("kept %d bytes of %d", len(r.Output), r.OutputBytes)
			}
		}},
		"sleep": {guest: "sleep", same: checkCaller{}, check: func(t *testing.T, r Report) {
			if per := r.DecisionTime / time.Duration(r.Decisions); per > 50*time.Millisecond {
				t.Fatalf("%v of wall time a decision", per)
			}
			var slept int64
			if _, err := fmt.Sscanf(string(r.Output), "slept %d", &slept); err != nil || slept < 10e9 || slept > 11e9 {
				t.Fatalf("the first sleep took %d ns by the bot's clock (%v; output %q)", slept, err, r.Output)
			}
		}},
		// With fuel to burn for a thousand seconds, the wall clock stops it.
		"deadline": {guest: "loop", limits: Limits{DecisionFuel: 1e12, DecisionTimeout: 100 * time.Millisecond}, fault: FaultDeadline,
			same: checkFolder{}, check: func(t *testing.T, r Report) {
				faultAt(1)(t, r)
				if r.DecisionTime < 100*time.Millisecond || r.DecisionTime > 300*time.Millisecond {
					t.Fatalf("stopped after %v", r.DecisionTime)
				}
			}},
		"spin":    {guest: "spin", fault: FaultDecisionFuel, same: checkFolder{}, check: faultAt(1)},
		"memcopy": {guest: "memcopy", fault: FaultDecisionFuel, same: checkFolder{}, check: faultAt(1)},
		"illegal": {guest: "illegal", same: checkFolder{}, check: func(t *testing.T, r Report) {
			if r.Illegal != r.Decisions || r.Played != 0 {
				t.Fatalf("%d illegal and %d played of %d", r.Illegal, r.Played, r.Decisions)
			}
		}},
		"fuelburn": {guest: "fuelburn", hands: 400, fault: FaultMatchFuel, check: func(t *testing.T, r Report) {
			if r.Fault.Decision < 3 {
				t.Fatalf("stopped at decision %d", r.Fault.Decision)
			}
		}},
		"ctorerr": {guest: "ctorerr", fault: FaultStart, same: checkFolder{}, check: func(t *testing.T, r Report) {
			if r.Fault.Decision != 0 || r.Fault.Detail != "no table file" {
				t.Fatalf("fault %v", r.Fault)
			}
		}},
		"ctorloop": {guest: "ctorloop", limits: Limits{StartFuel: 2e9}, fault: FaultStartFuel, same: checkFolder{}, check: faultAt(0)},
		"escape":   {guest: "escape", data: escapeMount, same: checkCaller{}, check: checkEscapes},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) { h.play(t, guest(t, h.guest)) })
	}
}

func faultAt(decision int) func(*testing.T, Report) {
	return func(t *testing.T, r Report) {
		if r.Fault.Decision != decision {
			t.Fatalf("fault at decision %d, want %d", r.Fault.Decision, decision)
		}
	}
}

// detailHas checks a fault at decision 1 whose detail says s.
func detailHas(s string) func(*testing.T, Report) {
	return func(t *testing.T, r Report) {
		faultAt(1)(t, r)
		if !strings.Contains(r.Fault.Detail, s) {
			t.Fatalf("the fault's detail %q has no %q", r.Fault.Detail, s)
		}
	}
}

// escapeMount is a data dir, opened as an os.Root, with a file the bot may
// read and a symlink out of the mount that it may not follow.
func escapeMount(t *testing.T) Config {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("ok\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return Config{Data: root.FS()}
}

func checkEscapes(t *testing.T, r Report) {
	out := string(r.Output)
	t.Logf("the escape bot's output:\n%s", out)
	if strings.Contains(out, "ESCAPED") || strings.Contains(out, "BROKEN") {
		t.Fatal("an attempt was not refused, or a read of its own data failed")
	}
	for _, when := range []string{"load", "act"} {
		if n := strings.Count(out, "denied "+when+" "); n != 24 {
			t.Errorf("%d attempts denied at %s, want 24", n, when)
		}
		if !strings.Contains(out, "allowed "+when+" read /data/ok.txt") {
			t.Errorf("no read of its own data at %s", when)
		}
		if !strings.Contains(out, "clock "+when+" 2026-01-01T00:00:00.") {
			t.Errorf("the clock at %s is not the virtual one", when)
		}
		if !strings.Contains(out, fmt.Sprintf("args %s [\"bot\"]", when)) {
			t.Errorf("args at %s", when)
		}
	}
}

// TestEscapeIsDeterministic: the same bot run twice says the same thing,
// clock readings included.
func TestEscapeIsDeterministic(t *testing.T) {
	p := guest(t, "escape")
	var outs [2]string
	for i := range outs {
		cfg := escapeMount(t)
		b, err := p.Start(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sdk.Match(b, bots.Shark{}, 20, 1); err != nil {
			t.Fatal(err)
		}
		outs[i] = string(b.Report().Output)
		b.Close()
	}
	if outs[0] != outs[1] {
		t.Fatalf("two runs differ:\n%s\n---\n%s", outs[0], outs[1])
	}
}

// TestExeBuildIsRejected: a bot built as a command, not a reactor, is
// refused with the build flag it needs.
func TestExeBuildIsRejected(t *testing.T) {
	out := filepath.Join(t.TempDir(), "dove.wasm")
	cmd := exec.Command("go", "build", "-o", out, "./guests/dove")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	wasm, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(wasm); err == nil || !strings.Contains(err.Error(), "-buildmode=c-shared") {
		t.Fatalf("Compile: %v", err)
	}
}

// watModule is a hand-written guest: the ABI's exports around act's body.
func watModule(t *testing.T, version, buffer int, act string, extra string) []byte {
	t.Helper()
	wasm, err := wasmtime.Wat2Wasm(fmt.Sprintf(`(module
		%s
		(memory (export "memory") 2)
				(func (export "_initialize"))
		(func (export "obgl_abi") (result i32) (i32.const %d))
		(func (export "obgl_buffer") (result i32) (i32.const %d))
		(func (export "obgl_start") (result i64) (i64.const 0))
		(func (export "obgl_act") (param i32) (result i64) %s))`, extra, version, buffer, act))
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

// checkReply checks or calls: Check (1) if the State's Legal.Check, the
// word at byte 68, is set, else Call (2).
const checkReply = "(if (result i64) (i32.load (i32.const 68)) (then (i64.const 0x100000000)) (else (i64.const 0x200000000)))"

func TestHandWrittenGuests(t *testing.T) {
	cases := map[string]struct {
		wasm   func(t *testing.T) []byte
		limits Limits
		fault  FaultKind
		check  func(t *testing.T, r Report)
	}{
		"plays":       {wasm: func(t *testing.T) []byte { return watModule(t, 1, 0, checkReply, "") }},
		"unreachable": {wasm: func(t *testing.T) []byte { return watModule(t, 1, 0, "unreachable", "") }, fault: FaultTrap},
		"out of bounds": {wasm: func(t *testing.T) []byte {
			return watModule(t, 1, 0, "(drop (i32.load (i32.const 0x7ffffff0))) "+checkReply, "")
		}, fault: FaultTrap},
		// wasmtime charges bulk memory operations a fuel a byte, so fuel
		// stops a loop of large copies too.
		"bulk copy": {wasm: func(t *testing.T) []byte {
			return watModule(t, 1, 0, `(drop (memory.grow (i32.const 16384))) (loop $l (memory.copy (i32.const 0) (i32.const 1) (i32.const 0x3ff00000)) (br $l)) `+checkReply, "")
		}, fault: FaultDecisionFuel},
		"version 2":  {wasm: func(t *testing.T) []byte { return watModule(t, 2, 0, checkReply, "") }, fault: FaultABI},
		"buffer out": {wasm: func(t *testing.T) []byte { return watModule(t, 1, 0x10001, checkReply, "") }, fault: FaultABI},
		"huge error": {wasm: func(t *testing.T) []byte { return watModule(t, 1, 0, "(i64.const 0x8000000040000000)", "") }, fault: FaultABI},
		"error reply": {wasm: func(t *testing.T) []byte {
			return watModule(t, 1, 0, `(i64.store (i32.const 0) (i64.const 0x6f6c6c6568)) (i64.const 0x8000000000000005)`, "") // "hello"
		},
			check: func(t *testing.T, r Report) {
				if r.Errors != r.Decisions || !strings.Contains(r.FirstError, "hello") {
					t.Fatalf("%d errors of %d, first %q", r.Errors, r.Decisions, r.FirstError)
				}
			}},
		// Every pointer a WASI call is given is out of bounds or nonsense:
		// each call fails with an errno, and the bot plays on.
		"bad pointers": {wasm: func(t *testing.T) []byte {
			return watModule(t, 1, 0, `
				(drop (call $fd_write (i32.const 1) (i32.const 0xfffffff0) (i32.const 4) (i32.const 0xfffffff0)))
				(drop (call $fd_write (i32.const 1) (i32.const 0x1fff8) (i32.const 1000) (i32.const 0)))
				(drop (call $fd_read (i32.const 3) (i32.const 0x1fffc) (i32.const 1) (i32.const 0)))
				(drop (call $path_open (i32.const 3) (i32.const 0) (i32.const 0x1fffe) (i32.const 0x7fffffff) (i32.const 0) (i64.const 2) (i64.const 2) (i32.const 0) (i32.const 0xffffffff)))
				(drop (call $random_get (i32.const 0x1ffff) (i32.const 2)))
				(drop (call $clock_time_get (i32.const 1) (i64.const 0) (i32.const 0x1fffc)))
				(drop (call $poll_oneoff (i32.const 0x1ffd0) (i32.const 0) (i32.const 1) (i32.const 0)))
				(drop (call $fd_readdir (i32.const 3) (i32.const 0x1fff0) (i32.const 0x7fffffff) (i64.const 0) (i32.const 0)))
				(drop (call $rand (i32.const 0x1fff8) (i32.const 2)))
				(drop (call $rand (i32.const 0) (i32.const 0x7fffffff)))
				`+checkReply, `
				(import "obgl" "rand" (func $rand (param i32 i32) (result i32)))
				(import "wasi_snapshot_preview1" "fd_write" (func $fd_write (param i32 i32 i32 i32) (result i32)))
				(import "wasi_snapshot_preview1" "fd_read" (func $fd_read (param i32 i32 i32 i32) (result i32)))
				(import "wasi_snapshot_preview1" "path_open" (func $path_open (param i32 i32 i32 i32 i32 i64 i64 i32 i32) (result i32)))
				(import "wasi_snapshot_preview1" "random_get" (func $random_get (param i32 i32) (result i32)))
				(import "wasi_snapshot_preview1" "clock_time_get" (func $clock_time_get (param i32 i64 i32) (result i32)))
				(import "wasi_snapshot_preview1" "poll_oneoff" (func $poll_oneoff (param i32 i32 i32 i32) (result i32)))
				(import "wasi_snapshot_preview1" "fd_readdir" (func $fd_readdir (param i32 i32 i32 i64 i32) (result i32)))`)
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := Compile(c.wasm(t))
			if err != nil {
				t.Fatal(err)
			}
			h := hostile{limits: c.limits, fault: c.fault, check: c.check, data: func(t *testing.T) Config { return Config{Data: Files{}} }}
			if c.fault != "" {
				h.same = checkFolder{}
			} else if c.check == nil {
				h.same = checkCaller{}
			}
			h.play(t, p)
		})
	}
}

func TestBadModulesAreRejected(t *testing.T) {
	for name, wat := range map[string]string{
		"imports env":                      `(module (import "env" "evil" (func)) (memory (export "memory") 1) (func (export "_initialize")))`,
		"imports an unknown WASI function": `(module (import "wasi_snapshot_preview1" "sock_open" (func)) (memory (export "memory") 1))`,
		"no exports":                       `(module (memory (export "memory") 1) (func (export "_initialize")))`,
		"act mistyped": `(module (memory (export "memory") 1) (func (export "_initialize"))
			(func (export "obgl_abi") (result i32) (i32.const 1)) (func (export "obgl_buffer") (result i32) (i32.const 0))
			(func (export "obgl_start") (result i64) (i64.const 0)) (func (export "obgl_act") (param i64) (result i64) (i64.const 0)))`,
		"rand mistyped": `(module (import "obgl" "rand" (func (param i64) (result i32))) (memory (export "memory") 1))`,
	} {
		wasm, err := wasmtime.Wat2Wasm(wat)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Compile(wasm); err == nil {
			t.Errorf("%s: compiled", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
	if _, err := Compile([]byte("not wasm")); err == nil {
		t.Error("not wasm: compiled")
	}
}
