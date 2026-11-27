// Package arena runs bots submitted as WebAssembly: it loads a module built
// with the SDK's wasmbot package (or any guest speaking sdk/wasmbot/abi),
// mounts the bot's data read-only, and plays it as an sdk.Bot inside fixed
// limits, so a submitted bot can neither hurt the machine nor the table it
// plays at, and is measured the same way as every other.
//
// # Sandbox
//
// The module gets WASI preview 1, implemented here (wasi.go) rather than
// by the runtime so that every call is metered and every answer decided,
// and one more import, obgl.rand, which hands it the State.Rand of the
// decision it is making (package abi):
//   - Files: the bot's data, read-only, preopened at /data. Opening for
//     write, creating, renaming or deleting is EROFS; paths leaving the
//     mount are ENOTCAPABLE. No other directory exists.
//   - Clock: virtual. It starts at a fixed date and advances one nanosecond
//     per unit of fuel the bot burns, and by the length of its sleeps,
//     which return at once. Wall time is never visible, so the same match
//     replays the same way.
//   - Randomness: State.Rand is the match's own, so a wasm bot draws what
//     its native self would. random_get (what a runtime seeds its hash
//     maps with) is one fixed stream, the same in every run.
//   - Standard output and error: accepted, kept up to Limits.OutputBytes for
//     the bot's owner, the rest counted and dropped. No stdin (it is empty),
//     no environment, one argument ("bot"), no sockets, no signals.
//
// Every WASI call costs Limits.CallFuel, and every byte it moves (reads,
// writes, directory listings, random draws) Limits.ByteFuel, so data
// access is paid for like computation.
//
// # Limits and forfeits
//
// Fuel is wasmtime's meter: about one unit per wasm instruction, plus one
// per byte a bulk memory instruction moves. The load (instantiating the
// module and running the bot's constructor) runs under Limits.StartFuel and Limits.StartTimeout. The match budget opens at
// Limits.DecisionFuel; each decision adds Limits.FuelPerDecision to it and
// may spend up to the smaller of the budget and Limits.DecisionFuel, within
// Limits.DecisionTimeout of wall time. Linear memory is capped at
// Limits.MemoryBytes for the whole match.
//
// The rule, when a bot breaks one:
//   - The decision is forfeited, and the bot plays on, when its reply is an
//     illegal move or an error (wasmbot replies with an error when Act
//     panics). The host plays State.CheckOrFold for it.
//   - The match is forfeited when the bot stops: it runs out of fuel (the
//     decision's ceiling or the match budget), passes a deadline, traps
//     (including the Go runtime's own aborts: out of memory, stack
//     overflow, deadlock), exits, sends a reply that breaks the ABI, or
//     fails to load. Its instance is not run again: the host plays
//     State.CheckOrFold for it at every remaining decision, so the match
//     still finishes and its chips are counted as usual.
//
// Report records which happened, when, and what the bot cost.
//
// # Determinism
//
// A bot's moves, its fuel, the bytes it reads and its memory are functions
// of the module, its data and the States it is shown: the same match
// gives the same figures in every run, alone or with others in parallel
// (TestConcurrentMatches). The one exception is the wall-clock deadline,
// which only a stalled machine or a bot outside its fuel's reach (slow
// host reads) meets: whether it fires depends on the machine.
package arena
