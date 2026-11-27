// Asks for 3 GiB at once.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

var keep []byte

type bot struct{}

func (bot) Act(s sdk.State) sdk.Action {
	keep = make([]byte, 3<<30)
	return s.CheckOrCall()
}

func main() {}
