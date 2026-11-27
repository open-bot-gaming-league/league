// Exits with code 3 on its first decision.
package main

import (
	"os"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

type bot struct{}

func (bot) Act(s sdk.State) sdk.Action {
	os.Exit(3)
	return s.CheckOrCall()
}

func main() {}
