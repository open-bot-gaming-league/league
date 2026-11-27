// Loops forever on its first decision.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

type bot struct{}

func (bot) Act(s sdk.State) sdk.Action {
	for {
	}
}

func main() {}
