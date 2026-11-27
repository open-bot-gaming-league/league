// Panics on every tenth decision, else checks or calls.
package main

import (
	"fmt"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return &bot{}, nil }) }

type bot struct{ n int }

func (b *bot) Act(s sdk.State) sdk.Action {
	b.n++
	if b.n%10 == 0 {
		panic(fmt.Sprintf("decision %d", b.n))
	}
	return s.CheckOrCall()
}

func main() {}
