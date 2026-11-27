// Prints 10 KiB each decision, and checks or calls.
package main

import (
	"fmt"
	"strings"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return bot{}, nil }) }

var line = strings.Repeat("y", 10<<10)

type bot struct{}

func (bot) Act(s sdk.State) sdk.Action {
	fmt.Println(line)
	return s.CheckOrCall()
}

func main() {}
