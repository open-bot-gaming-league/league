// Package bots has three starter bots, each a short file to read and
// copy: Station calls, Maniac raises, Rock waits for a big hand. None is
// good; each loses to a bot that sees what it does.
package bots

import "github.com/open-bot-gaming-league/league/poker/engine/sdk"

// ByName is the starter bots by their names in lower case, for flags.
var ByName = map[string]func() sdk.Bot{
	"station": func() sdk.Bot { return Station{} },
	"maniac":  func() sdk.Bot { return Maniac{} },
	"rock":    func() sdk.Bot { return Rock{} },
}
