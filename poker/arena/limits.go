package arena

import "time"

// Limits are what a bot may use. The zero value of a field means its
// DefaultLimits value.
type Limits struct {
	// MemoryBytes caps the bot's linear memory.
	MemoryBytes uint64
	// StartFuel and StartTimeout bound the load: instantiating the module
	// and running the bot's constructor, data reads included.
	StartFuel    uint64
	StartTimeout time.Duration
	// The match budget starts at DecisionFuel and FuelPerDecision is added
	// to it at each decision; a decision may spend up to
	// min(budget, DecisionFuel). Over n decisions a bot may so spend
	// DecisionFuel + n*FuelPerDecision.
	FuelPerDecision uint64
	DecisionFuel    uint64
	// DecisionTimeout is the wall-clock deadline of one decision: a
	// backstop for work that fuel does not see, such as bulk memory copies.
	DecisionTimeout time.Duration
	// CallFuel is charged for each WASI call, and ByteFuel for each byte
	// one moves (reads, writes, directory listings, random draws).
	CallFuel uint64
	ByteFuel uint64
	// OutputBytes is how much of the bot's standard output and error is
	// kept for its Report.
	OutputBytes int
}

// DefaultLimits are the league's Lightweight class: 2 GiB of memory, and
// fuel enough for ten times the work of the SDK's Dove bot (about 2 million
// fuel a decision) on average, with a ceiling of 500 times it for any one
// decision.
//
// Fuel is wasmtime's: about one unit per wasm instruction, and one per
// byte that a bulk memory instruction (memory.copy, memory.fill) moves.
// Go's runtime clears and copies memory with those, so a Go bot can burn
// 1 to 30 fuel a nanosecond.
func DefaultLimits() Limits {
	return Limits{
		MemoryBytes:     2 << 30,
		StartFuel:       200_000_000_000, // a house bot's load takes 73e9
		StartTimeout:    60 * time.Second,
		FuelPerDecision: 20_000_000,
		DecisionFuel:    1_000_000_000,
		DecisionTimeout: 5 * time.Second,
		CallFuel:        1_000,
		ByteFuel:        1,
		OutputBytes:     64 << 10,
	}
}

// filled is l with its zero fields set to the defaults.
func (l Limits) filled() Limits {
	d := DefaultLimits()
	set := func(v *uint64, def uint64) {
		if *v == 0 {
			*v = def
		}
	}
	set(&l.MemoryBytes, d.MemoryBytes)
	set(&l.StartFuel, d.StartFuel)
	set(&l.FuelPerDecision, d.FuelPerDecision)
	set(&l.DecisionFuel, d.DecisionFuel)
	set(&l.CallFuel, d.CallFuel)
	set(&l.ByteFuel, d.ByteFuel)
	if l.StartTimeout == 0 {
		l.StartTimeout = d.StartTimeout
	}
	if l.DecisionTimeout == 0 {
		l.DecisionTimeout = d.DecisionTimeout
	}
	if l.OutputBytes == 0 {
		l.OutputBytes = d.OutputBytes
	}
	return l
}
