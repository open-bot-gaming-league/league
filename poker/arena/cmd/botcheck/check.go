package main

import (
	"errors"
	"strings"

	"github.com/open-bot-gaming-league/league/poker/arena"
)

// check is why wasm cannot be run as a bot, or nil: the arena's own check
// (arena.Compile: valid wasm, WASI preview 1 imports only, the guest ABI's
// exports), so a module that passes here is one the arena will start. The
// reason is one line, without the package prefix.
func check(wasm []byte) error {
	_, err := arena.Compile(wasm)
	if err == nil {
		return nil
	}
	line, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	return errors.New(strings.TrimPrefix(line, "arena: "))
}
