// A house bot (Turtle, Ox) as a wasm bot: one module, the bot is its
// data. The bucket file is read on demand, lookup by lookup.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() {
	wasmbot.Register(func() (sdk.Bot, error) { return sdk.LoadHouse("/data/buckets.bin", "/data/blueprint.bp") })
}

func main() {}
