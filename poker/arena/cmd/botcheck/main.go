// Command botcheck says whether a WebAssembly module can be submitted as a
// bot: the check the league's server runs on every submission before it
// keeps it.
//
//	botcheck bot.wasm     (or - for standard input)
//
// Exit 0: it can. Exit 1: it cannot, and why, in one line on standard
// error. Exit 2: botcheck itself failed (no file, bad arguments).
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: botcheck bot.wasm | botcheck -")
		os.Exit(2)
	}
	var wasm []byte
	var err error
	if os.Args[1] == "-" {
		wasm, err = io.ReadAll(os.Stdin)
	} else {
		wasm, err = os.ReadFile(os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := check(wasm); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
