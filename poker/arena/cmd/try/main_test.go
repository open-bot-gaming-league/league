package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/arena"
)

func TestTry(t *testing.T) {
	dir := t.TempDir()
	for _, g := range []string{"../../guests/shark", "../../testdata/guests/panic", "../../testdata/guests/loop"} {
		wasm, err := arena.Build(g)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(g)+".wasm"), wasm, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := run([]string{filepath.Join(dir, "shark.wasm"), "-vs", "dove,clam", "-hands", "200"}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "vs Dove") || !strings.Contains(s, "vs Clam") || strings.Contains(s, "STOPPED") || !strings.Contains(s, "fuel ") {
		t.Errorf("shark:\n%s", s)
	}
	out.Reset()
	if err := run([]string{"-vs", "clam", "-hands", "200", filepath.Join(dir, "panic.wasm")}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "decisions played for it") || !strings.Contains(s, "panic: decision 10") {
		t.Errorf("panic:\n%s", s)
	}
	out.Reset()
	if err := run([]string{filepath.Join(dir, "loop.wasm"), "-vs", "clam", "-hands", "20"}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "STOPPED: decision fuel at decision 1") {
		t.Errorf("loop:\n%s", s)
	}
	if err := run([]string{"-vs", "clam"}, &out); err == nil {
		t.Error("ran without a module")
	}
}
