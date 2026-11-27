//go:build wasip1

package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

// In wasm, the example is myBot, ready to submit:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o mybot.wasm ./sdk/example
func init() { wasmbot.Register(func() (sdk.Bot, error) { return myBot{}, nil }) }
