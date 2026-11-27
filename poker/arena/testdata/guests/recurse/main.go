// Recurses without end.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

type bot struct{}

func deeper(n int) int {
	var pad [64]byte
	pad[n%64] = byte(n)
	return deeper(n+1) + int(pad[3])
}

func (bot) Act(s sdk.State) sdk.Action { return sdk.Action{Kind: sdk.Kind(deeper(0))} }

func main() {}
