// Copies a 512 MiB buffer onto itself without end: work that costs little fuel.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

type bot struct{}

func (bot) Act(s sdk.State) sdk.Action {
	big := make([]byte, 512<<20)
	for {
		copy(big[1:], big)
	}
}

func main() {}
