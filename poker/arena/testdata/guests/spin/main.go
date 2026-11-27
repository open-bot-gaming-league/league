// Busy-waits on the clock for a second each decision.
package main

import (
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

type bot struct{}

func (bot) Act(s sdk.State) sdk.Action {
	t := time.Now()
	for time.Since(t) < time.Second {
	}
	return s.CheckOrCall()
}

func main() {}
