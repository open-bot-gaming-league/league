// Burns about 200 million fuel on every postflop decision, more than its budget grows.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

var sink int

type bot struct{}

func (bot) Act(s sdk.State) sdk.Action {
	if s.Round != sdk.Preflop {
		x := sink
		for i := range 30_000_000 {
			x = x*31 + i
		}
		sink = x
	}
	return s.CheckOrCall()
}

func main() {}
