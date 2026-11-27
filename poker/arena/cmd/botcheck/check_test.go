package main

import (
	"strings"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/arena"
)

// A bot built the way the SDK says (wasmbot.Register, -buildmode=c-shared)
// passes; a module that is valid wasm but not a bot, and bytes that are not
// wasm, are refused with a one-line reason.
func TestCheck(t *testing.T) {
	wasm, err := arena.Build("../../../engine/sdk/example")
	if err != nil {
		t.Fatal(err)
	}
	if err := check(wasm); err != nil {
		t.Errorf("the SDK example: %v", err)
	}
	for _, c := range []struct {
		wasm   []byte
		reason string
	}{
		{[]byte("\x00asm\x01\x00\x00\x00"), "the module does not export "}, // the empty module: valid, no ABI
		{nil, "not a valid WebAssembly module: "},
		{[]byte("MZ"), "not a valid WebAssembly module: "},
		{[]byte("\x00asm\x01\x00\x00\x00\x01\xff"), "not a valid WebAssembly module: "},
	} {
		err := check(c.wasm)
		if err == nil || !strings.HasPrefix(err.Error(), c.reason) || strings.Contains(err.Error(), "\n") {
			t.Errorf("%q: %v, want %q...", c.wasm, err, c.reason)
		}
	}
}
