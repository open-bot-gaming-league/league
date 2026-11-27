package arena

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v51"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot/abi"
)

// tick is how often the wall-clock deadlines are checked: wasmtime's epoch
// advances once a tick, and a deadline is a count of ticks.
const tick = 10 * time.Millisecond

// runtime is the one wasmtime engine and WASI linker every bot shares.
type runtime struct {
	engine *wasmtime.Engine
	linker *wasmtime.Linker
}

var shared = sync.OnceValues(func() (*runtime, error) {
	cfg := wasmtime.NewConfig()
	cfg.SetConsumeFuel(true)
	cfg.SetEpochInterruption(true)
	cfg.SetWasmRelaxedSIMDDeterministic(true) // the same answers on every machine
	e := wasmtime.NewEngineWithConfig(cfg)
	l := wasmtime.NewLinker(e)
	if err := defineImports(l); err != nil {
		return nil, err
	}
	go func() {
		for range time.Tick(tick) {
			e.IncrementEpoch()
		}
	}()
	return &runtime{engine: e, linker: l}, nil
})

// ticks is a deadline of d as epoch ticks, rounded up, plus one for the
// tick in progress.
func ticks(d time.Duration) uint64 { return uint64((d+tick-1)/tick) + 1 }

// Program is a compiled bot module, checked against the ABI. One Program
// starts any number of Bots, concurrently.
type Program struct {
	module *wasmtime.Module
	sha256 string
}

// SHA256 is the module's sha256, in hex: what plays, for a record.
func (p *Program) SHA256() string { return p.sha256 }

// Compile checks and compiles a bot module. It is rejected if it is not
// valid wasm, imports anything but the WASI preview 1 functions, or does
// not export what the ABI asks (package abi).
func Compile(wasm []byte) (*Program, error) {
	rt, err := shared()
	if err != nil {
		return nil, err
	}
	m, err := wasmtime.NewModule(rt.engine, wasm)
	if err != nil {
		return nil, fmt.Errorf("arena: not a valid module: %w", err)
	}
	for _, imp := range m.Imports() {
		name := ""
		if imp.Name() != nil {
			name = *imp.Name()
		}
		_, wasi := wasiFuncs[name]
		switch {
		case imp.Module() == wasiModule && wasi:
		case imp.Module() == abi.ImportModule && name == abi.ImportRand:
			if got := externSig(imp.Type()); got != "(i32, i32) -> (i32)" {
				return nil, fmt.Errorf("arena: the module imports %s.%s as %s", imp.Module(), name, got)
			}
		default:
			return nil, fmt.Errorf("arena: the module imports %s.%s; a bot may import only WASI preview 1 (%s) and %s.%s",
				imp.Module(), name, wasiModule, abi.ImportModule, abi.ImportRand)
		}
	}
	want := map[string]string{
		"memory":         "memory",
		"_initialize":    "() -> ()",
		abi.ExportABI:    "() -> (i32)",
		abi.ExportBuffer: "() -> (i32)",
		abi.ExportStart:  "() -> (i64)",
		abi.ExportAct:    "(i32) -> (i64)",
	}
	for _, exp := range m.Exports() {
		if w, ok := want[exp.Name()]; ok {
			if got := externSig(exp.Type()); got != w {
				return nil, fmt.Errorf("arena: the module exports %s as %s, want %s", exp.Name(), got, w)
			}
			delete(want, exp.Name())
		}
	}
	if len(want) > 0 {
		missing := slices.Sorted(func(yield func(string) bool) {
			for k := range want {
				if !yield(k) {
					return
				}
			}
		})
		hint := ""
		if slices.Contains(missing, "_initialize") {
			hint = " (build a Go bot with -buildmode=c-shared: see package wasmbot)"
		}
		return nil, fmt.Errorf("arena: the module does not export %s%s", strings.Join(missing, ", "), hint)
	}
	sum := sha256.Sum256(wasm)
	return &Program{module: m, sha256: hex.EncodeToString(sum[:])}, nil
}

// externSig is an export's type as "memory" or "(i32, i64) -> (i64)".
func externSig(t *wasmtime.ExternType) string {
	if t.MemoryType() != nil {
		return "memory"
	}
	f := t.FuncType()
	if f == nil {
		return "not a function"
	}
	list := func(vs []*wasmtime.ValType) string {
		s := make([]string, len(vs))
		for i, v := range vs {
			switch k := v.Kind(); k {
			case wasmtime.KindI32, wasmtime.KindI64, wasmtime.KindF32, wasmtime.KindF64:
				s[i] = k.String()
			default:
				s[i] = "other" // String panics on kinds it does not know (v128)
			}
		}
		return "(" + strings.Join(s, ", ") + ")"
	}
	return list(f.Params()) + " -> " + list(f.Results())
}
