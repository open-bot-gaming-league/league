// Package wasmbot turns an sdk.Bot into a WebAssembly module the league
// can run: the form a bot is submitted in for ranked play. Register the
// bot in an init function of its main package:
//
//	package main
//
//	import (
//		"github.com/open-bot-gaming-league/league/poker/engine/sdk"
//		"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
//	)
//
//	func init() { wasmbot.Register(func() (sdk.Bot, error) { return MyBot{}, nil }) }
//
//	func main() {}
//
// and build it as a WASI reactor:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o bot.wasm .
//
// The same package still builds and runs natively (Register only records
// the constructor there), so a bot's tests and its local Matches need no
// change.
//
// What the bot sees when it runs in the league:
//   - Its data, if it was submitted with any, as read-only files under
//     /data: os.Open("/data/table.bin") and the usual reads work. Read
//     what a decision needs when it needs it (File.ReadAt) rather than
//     copying a large file into memory: every read costs fuel, but memory
//     is capped.
//   - State.Rand: the match's own, drawn through the host, so a wasm bot
//     plays the same hands the same way as the native one. Draw from it
//     only inside Act.
//   - Nothing else: no network, no writes, no environment. The clock is
//     not the wall clock: it advances with the work the bot does (a
//     nanosecond per unit of fuel: about one per wasm instruction) and
//     with its sleeps, which cost no real time. Standard output and error are kept, up to a cap, for the
//     bot's owner to read.
//
// The constructor runs once, when the bot is loaded (load files there:
// its time does not count against any decision). Act then runs on every
// decision, under a fuel and time limit. A panic in Act forfeits that
// decision (the league checks or folds for the bot) and the bot plays on;
// a crash that cannot be recovered (out of memory, a fatal runtime error,
// running out of fuel or time) ends the bot's match: it checks or folds
// from then on. The arena package has the limits and the exact rules.
package wasmbot

import "github.com/open-bot-gaming-league/league/poker/engine/sdk"

var newBot func() (sdk.Bot, error)

// Register names the function that builds the bot. Call it once, from an
// init function of the main package.
func Register(f func() (sdk.Bot, error)) {
	if newBot != nil {
		panic("wasmbot: Register called twice")
	}
	newBot = f
}
