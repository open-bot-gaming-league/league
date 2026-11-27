// Sleeps ten seconds each decision, and checks or calls; prints how long the first sleep took by its clock.
package main

import (
	"fmt"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return &bot{}, nil }) }

type bot struct{ n int }

func (b *bot) Act(s sdk.State) sdk.Action {
	t := time.Now()
	time.Sleep(10 * time.Second)
	if b.n++; b.n == 1 {
		fmt.Printf("slept %d\n", time.Since(t).Nanoseconds())
	}
	return s.CheckOrCall()
}

func main() {}
