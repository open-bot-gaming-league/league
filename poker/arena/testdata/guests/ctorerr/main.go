// Its constructor fails.
package main

import (
	"errors"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() {
	wasmbot.Register(func() (sdk.Bot, error) { return nil, errors.New("no table file") })
}

func main() {}
