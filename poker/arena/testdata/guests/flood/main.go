// Writes to standard output without end.
package main

import (
	"bytes"
	"os"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

type bot struct{}

func (bot) Act(s sdk.State) sdk.Action {
	b := bytes.Repeat([]byte("x"), 1<<20)
	for {
		os.Stdout.Write(b)
	}
}

func main() {}
