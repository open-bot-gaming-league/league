package arena

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v51"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot/abi"
)

// Config is how a bot is started.
type Config struct {
	// Data is the bot's data, mounted read-only at /data; nil mounts
	// nothing. Its files must implement io.ReaderAt (those of os.DirFS,
	// os.Root.FS and Files do).
	Data fs.FS
	// Limits; zero fields take DefaultLimits.
	Limits Limits
}

// FaultKind is what stopped a bot.
type FaultKind string

const (
	FaultDecisionFuel FaultKind = "decision fuel" // a decision used more than Limits.DecisionFuel
	FaultMatchFuel    FaultKind = "match fuel"    // the match budget ran out
	FaultStartFuel    FaultKind = "start fuel"    // the load used more than Limits.StartFuel
	FaultDeadline     FaultKind = "deadline"      // a decision or the load passed its wall-clock deadline
	FaultMemory       FaultKind = "memory"        // the bot aborted at the memory cap
	FaultExit         FaultKind = "exit"          // the bot exited (a Go runtime abort exits with 2)
	FaultTrap         FaultKind = "trap"          // a wasm trap: unreachable, out-of-bounds access, stack overflow, ...
	FaultABI          FaultKind = "abi"           // the module broke the ABI: wrong version, bad buffer, bad reply
	FaultStart        FaultKind = "start"         // the bot's constructor returned an error
	FaultHost         FaultKind = "host"          // the host failed; a bug here, not the bot's
)

// Fault is what stopped a bot, and when.
type Fault struct {
	Kind     FaultKind
	Decision int // the decision it stopped in, from 1; 0 for the load
	Hand     int // the State.Hand of that decision
	Detail   string
}

func (f *Fault) String() string {
	at := "loading"
	if f.Decision > 0 {
		at = fmt.Sprintf("decision %d (hand %d)", f.Decision, f.Hand)
	}
	return fmt.Sprintf("%s at %s: %s", f.Kind, at, f.Detail)
}

// Report is how a bot's match went for the sandbox.
type Report struct {
	Decisions int // decisions asked of it
	Played    int // decisions where its own move was played
	Illegal   int // its move was illegal: checked or folded for it
	Errors    int // it replied with an error (wasmbot: Act panicked): checked or folded for it
	// FirstError is the first error or illegal move, for the bot's owner.
	FirstError string
	// Fault is what stopped the bot, if anything did: every decision
	// from Fault.Decision on was checked or folded for it.
	Fault *Fault

	StartFuel       uint64 // fuel the load used
	Fuel            uint64 // fuel the decisions used
	MaxDecisionFuel uint64
	StartTime       time.Duration
	DecisionTime    time.Duration // wall time inside the bot's decisions
	ReadCalls       int64         // reads from /data
	ReadBytes       int64
	MemoryBytes     uint64 // linear memory at the end: its peak, as it never shrinks
	OutputBytes     int64  // written to standard output and error
	Output          []byte // the first Limits.OutputBytes of it
}

// Forfeited is the decisions checked or folded for the bot.
func (r Report) Forfeited() int { return r.Decisions - r.Played }

// Bot is a started bot module: an sdk.Bot. It is not safe for concurrent
// use (sdk.Match asks one decision at a time); start one Bot per match.
type Bot struct {
	lim   Limits
	store *wasmtime.Store
	box   *sandbox
	act   *wasmtime.Func
	buf   uint32
	state []byte
	bank  uint64 // the match budget
	// The State.Rand of the last decision, and its generation (abi.EncodeState).
	rand    *rand.Rand
	randGen uint32
	rep     Report
	dead    bool
}

// Start loads the bot: it instantiates the module and runs the bot's
// constructor under the Limits' start fuel and deadline. Whatever the bot
// does is in its Report, not an error: a bot that fails to load has a
// Fault and checks or folds every decision. Close it when done.
func (p *Program) Start(cfg Config) (*Bot, error) {
	rt, err := shared()
	if err != nil {
		return nil, err
	}
	lim := cfg.Limits.filled()
	box := newSandbox(cfg.Data, lim)
	store := wasmtime.NewStoreWithData(rt.engine, box)
	box.store = store
	store.Limiter(int64(lim.MemoryBytes), 1<<20, 1, 1, 1)
	// The match budget opens with one decision's ceiling, so that a bot may
	// spend it on its first decision, and grows FuelPerDecision a decision.
	b := &Bot{lim: lim, store: store, box: box, state: make([]byte, abi.BufferSize), bank: lim.DecisionFuel}
	t := time.Now()
	b.start(rt, p)
	b.rep.StartTime = time.Since(t)
	b.rep.StartFuel = box.spent
	return b, nil
}

func (b *Bot) start(rt *runtime, p *Program) {
	defer func() {
		if p := recover(); p != nil {
			b.die(0, 0, FaultHost, hostPanic{p}.Error())
		}
	}()
	box := b.box
	box.given = b.lim.StartFuel
	if err := b.store.SetFuel(box.given); err != nil {
		b.die(0, 0, FaultHost, err.Error())
		return
	}
	b.store.SetEpochDeadline(ticks(b.lim.StartTimeout))
	defer func() { box.spent = box.fuelSpent() }()
	inst, err := rt.linker.Instantiate(b.store, p.module)
	if err != nil {
		b.fail(0, 0, err)
		return
	}
	if box.mem = inst.GetExport(b.store, "memory").Memory(); box.mem == nil {
		b.die(0, 0, FaultABI, "no memory export")
		return
	}
	call := func(name string, args ...any) (any, bool) {
		r, err := inst.GetFunc(b.store, name).Call(b.store, args...)
		if err != nil {
			b.fail(0, 0, err)
			return nil, false
		}
		return r, true
	}
	if _, ok := call("_initialize"); !ok {
		return
	}
	v, ok := call(abi.ExportABI)
	if !ok {
		return
	}
	if v.(int32) != abi.Version {
		b.die(0, 0, FaultABI, fmt.Sprintf("ABI version %d; this host speaks %d", v.(int32), abi.Version))
		return
	}
	if v, ok = call(abi.ExportBuffer); !ok {
		return
	}
	b.buf = uint32(v.(int32))
	if uint64(b.buf)+abi.BufferSize > uint64(box.mem.DataSize(b.store)) {
		b.die(0, 0, FaultABI, fmt.Sprintf("the buffer at %#x is not inside memory", b.buf))
		return
	}
	if v, ok = call(abi.ExportStart); !ok {
		return
	}
	if r := uint64(v.(int64)); r&abi.ErrBit != 0 {
		msg, ok := b.message(r, 0)
		if !ok {
			return
		}
		b.die(0, 0, FaultStart, msg)
		return
	}
	b.act = inst.GetFunc(b.store, abi.ExportAct)
}

// Act asks the bot for its move. A bot that has stopped, or that makes an
// illegal move or replies with an error, gets s.CheckOrFold played for it.
func (b *Bot) Act(s sdk.State) sdk.Action {
	b.rep.Decisions++
	if b.dead {
		return s.CheckOrFold()
	}
	a, err := b.ask(s)
	switch {
	case b.dead:
	case err != nil:
		b.rep.Errors++
		b.firstError(fmt.Sprintf("decision %d (hand %d): %v", b.rep.Decisions, s.Hand, err))
	case !s.Legal.Allows(a):
		b.rep.Illegal++
		b.firstError(fmt.Sprintf("decision %d (hand %d): %v is not legal (%+v)", b.rep.Decisions, s.Hand, a, s.Legal))
	default:
		b.rep.Played++
		return a
	}
	return s.CheckOrFold()
}

// ask runs one decision in the guest. An error is the bot's own error
// reply; a fault kills the bot (b.dead) instead.
func (b *Bot) ask(s sdk.State) (a sdk.Action, err error) {
	if s.Rand != b.rand {
		b.rand, b.randGen = s.Rand, b.randGen+1
	}
	n, err := abi.EncodeState(b.state, s, b.randGen)
	if err != nil {
		b.die(b.rep.Decisions, s.Hand, FaultHost, err.Error())
		return a, nil
	}
	box := b.box
	copy(box.mem.UnsafeData(b.store)[b.buf:], b.state[:n]) // the buffer was checked at start; memory only grows
	b.bank += b.lim.FuelPerDecision
	box.given = min(b.bank, b.lim.DecisionFuel)
	if err := b.store.SetFuel(box.given); err != nil {
		b.die(b.rep.Decisions, s.Hand, FaultHost, err.Error())
		return a, nil
	}
	b.store.SetEpochDeadline(ticks(b.lim.DecisionTimeout))
	box.rand = s.Rand
	t := time.Now()
	r, callErr := b.call(int32(n))
	box.rand = nil
	b.rep.DecisionTime += time.Since(t)
	used := box.fuelSpent() - box.spent
	box.spent += used
	b.bank -= min(used, b.bank)
	b.rep.Fuel += used
	b.rep.MaxDecisionFuel = max(b.rep.MaxDecisionFuel, used)
	if callErr != nil {
		b.fail(b.rep.Decisions, s.Hand, callErr)
		return a, nil
	}
	if r&abi.ErrBit != 0 {
		msg, ok := b.message(r, s.Hand)
		if !ok {
			return a, nil
		}
		return a, errors.New(msg)
	}
	return abi.UnpackAction(r), nil
}

// call is the act export, with a panic in the host's own code (a host
// function's bug) turned into an error.
func (b *Bot) call(n int32) (r uint64, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = hostPanic{p}
		}
	}()
	v, err := b.act.Call(b.store, n)
	if err != nil {
		return 0, err
	}
	return uint64(v.(int64)), nil
}

type hostPanic struct{ p any }

func (h hostPanic) Error() string { return fmt.Sprintf("host panic: %v", h.p) }

// message is an error reply's message, or false (and the bot dead) if
// the reply breaks the ABI.
func (b *Bot) message(r uint64, hand int) (string, bool) {
	n := abi.ErrorLen(r)
	if n > abi.BufferSize {
		b.die(b.rep.Decisions, hand, FaultABI, fmt.Sprintf("an error reply of %d bytes", n))
		return "", false
	}
	return string(b.box.mem.UnsafeData(b.store)[b.buf : b.buf+uint32(n)]), true
}

// fail kills the bot for a failed call, saying why.
func (b *Bot) fail(decision, hand int, err error) {
	box := b.box
	kind, detail := FaultTrap, trapLine(err)
	var trap *wasmtime.Trap
	var hp hostPanic
	switch {
	case errors.As(err, &hp):
		kind = FaultHost
	case box.outOfFuel || errors.As(err, &trap) && trap.Code() != nil && *trap.Code() == wasmtime.OutOfFuel:
		switch {
		case decision == 0:
			kind = FaultStartFuel
		case box.given == b.lim.DecisionFuel:
			kind = FaultDecisionFuel
		default:
			kind = FaultMatchFuel
		}
		detail = fmt.Sprintf("%d fuel given", box.given)
	case errors.As(err, &trap) && trap.Code() != nil && *trap.Code() == wasmtime.Interrupt:
		kind, detail = FaultDeadline, "past the deadline"
	case box.exited != nil:
		kind, detail = FaultExit, fmt.Sprintf("exit code %d", *box.exited)
	}
	if kind == FaultTrap || kind == FaultExit {
		// A runtime that cannot grow its memory aborts (Go traps, after
		// printing "fatal error: out of memory"): stopping at the cap is the
		// cap's doing.
		if mem := b.memoryBytes(); mem+capSlack >= b.lim.MemoryBytes {
			kind = FaultMemory
			detail += fmt.Sprintf(", at %d MiB of memory", mem>>20)
		}
		if box.lastWords != "" {
			detail += "; the bot said: " + box.lastWords
		}
	}
	b.die(decision, hand, kind, detail)
}

// trapLine is the line of a wasmtime error that says what trapped.
func trapLine(err error) string {
	lines := strings.Split(err.Error(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.Contains(l, "wasm trap:") {
			return l[strings.Index(l, "wasm trap:"):]
		}
	}
	return strings.TrimSpace(lines[0])
}

// capSlack is how close to the memory cap an exit counts as the cap's:
// more than the Go runtime asks for in one growth.
const capSlack = 64 << 20

func (b *Bot) die(decision, hand int, kind FaultKind, detail string) {
	b.dead = true
	b.rep.Fault = &Fault{Kind: kind, Decision: decision, Hand: hand, Detail: detail}
}

func (b *Bot) firstError(msg string) {
	if b.rep.FirstError == "" {
		b.rep.FirstError = msg
	}
}

func (b *Bot) memoryBytes() uint64 {
	if b.box.mem == nil || b.store == nil {
		return 0
	}
	return uint64(b.box.mem.DataSize(b.store))
}

// Report is how the bot's match has gone so far.
func (b *Bot) Report() Report {
	r := b.rep
	box := b.box
	if b.store != nil {
		r.MemoryBytes = b.memoryBytes()
	}
	r.ReadCalls, r.ReadBytes = box.readCalls, box.readB
	r.OutputBytes, r.Output = box.outBytes, append([]byte(nil), box.out...)
	return r
}

// Close releases the instance and its files. The Report stays readable;
// a closed bot checks or folds.
func (b *Bot) Close() error {
	if b.store == nil {
		return nil
	}
	b.rep.MemoryBytes = b.memoryBytes()
	b.box.close()
	b.store.Close()
	b.store, b.dead = nil, true
	return nil
}
