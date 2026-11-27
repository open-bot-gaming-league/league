// Keeps every 64 MiB it allocates, until it cannot.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

var keep [][]byte

type bot struct{}

func (bot) Act(s sdk.State) sdk.Action {
	for {
		b := make([]byte, 64<<20)
		b[len(b)-1] = 1
		keep = append(keep, b)
	}
}

func main() {}
