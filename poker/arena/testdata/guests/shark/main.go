// The SDK's Shark, as a wasm bot.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bots.Shark{}, nil }) }

func main() {}
