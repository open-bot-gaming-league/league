// Makes only illegal moves: raises past all-in, folds where it could check, or a kind that does not exist.
package main

import (
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

func init() { wasmbot.Register(func() (sdk.Bot, error) { return &bot{}, nil }) }

type bot struct{ n int }

func (b *bot) Act(s sdk.State) sdk.Action {
	b.n++
	switch {
	case b.n%3 == 0:
		return sdk.Action{Kind: sdk.Raise, To: 1_000_000_000}
	case b.n%3 == 1 && s.Legal.Check:
		return sdk.Action{Kind: sdk.Fold}
	}
	return sdk.Action{Kind: 9}
}

func main() {}
