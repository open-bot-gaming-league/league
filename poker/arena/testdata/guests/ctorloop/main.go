// Its constructor never returns.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

var sink int

func init() {
	wasmbot.Register(func() (sdk.Bot, error) {
		for {
			sink++
		}
	})
}

func main() {}
