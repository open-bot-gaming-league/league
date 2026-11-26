// Package bots has three starter bots, each a short file to read and
// copy: Dove bets what its hand is worth and never bluffs, Clam waits for
// a big hand, Shark plays a tight chart and keeps the initiative. None is
// strong; each has a leak a bot that watches it can find.
package bots

import "github.com/open-bot-gaming-league/league/poker/engine/sdk"

// ByName is the starter bots by id, for flags.
var ByName = map[string]func() sdk.Bot{
	"dove":  func() sdk.Bot { return Dove{} },
	"clam":  func() sdk.Bot { return Clam{} },
	"shark": func() sdk.Bot { return Shark{} },
}

// Names is each starter bot's name, by id: the one place a bot's name is
// written, for anything that shows it.
var Names = map[string]string{
	"dove":  "Dove",
	"clam":  "Clam",
	"shark": "Shark",
}
