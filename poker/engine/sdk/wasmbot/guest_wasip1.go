//go:build wasip1

package wasmbot

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"unsafe"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot/abi"
)

// The guest side of package abi.

var (
	buffer [abi.BufferSize]byte
	bot    sdk.Bot
	rnd    = rand.New(&source)
	source hostSource
)

//go:wasmimport obgl rand
func hostRand(dst unsafe.Pointer, n uint32) uint32

// hostSource is State.Rand's source: the host's, drawn a batch at a time.
type hostSource struct {
	gen  uint32
	next int
	have int
	buf  [256]uint64
}

func (h *hostSource) Uint64() uint64 {
	if h.next == h.have {
		if hostRand(unsafe.Pointer(&h.buf[0]), uint32(len(h.buf))) != 0 {
			panic("wasmbot: State.Rand used outside Act, or the host has none")
		}
		h.next, h.have = 0, len(h.buf)
	}
	h.next++
	return h.buf[h.next-1]
}

// use starts a decision: a new generation drops the numbers drawn ahead
// from the host's previous source.
func (h *hostSource) use(gen uint32) {
	if gen != h.gen {
		h.gen, h.next, h.have = gen, 0, 0
	}
}

//go:wasmexport obgl_abi
func obglABI() int32 { return abi.Version }

//go:wasmexport obgl_buffer
func obglBuffer() uint32 { return uint32(uintptr(unsafe.Pointer(&buffer[0]))) }

//go:wasmexport obgl_start
func obglStart() (r uint64) {
	defer recoverInto(&r)
	if newBot == nil {
		return fail(errors.New("wasmbot: no bot registered (call wasmbot.Register in an init function)"))
	}
	b, err := newBot()
	if err != nil {
		return fail(err)
	}
	if b == nil {
		return fail(errors.New("wasmbot: the registered function returned no bot"))
	}
	bot = b
	return 0
}

//go:wasmexport obgl_act
func obglAct(n uint32) (r uint64) {
	defer recoverInto(&r)
	if bot == nil {
		return fail(errors.New("wasmbot: act before start"))
	}
	if n > abi.BufferSize {
		return fail(fmt.Errorf("wasmbot: a state of %d bytes", n))
	}
	s, gen, err := abi.DecodeState(buffer[:n])
	if err != nil {
		return fail(err)
	}
	source.use(gen)
	s.Rand = rnd
	return abi.PackAction(bot.Act(s))
}

func fail(err error) uint64 { return abi.PackError(copy(buffer[:], err.Error())) }

func recoverInto(r *uint64) {
	if p := recover(); p != nil {
		*r = fail(fmt.Errorf("panic: %v", p))
	}
}
