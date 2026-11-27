// Package abi is the wire format between the league's host and a bot
// compiled to WebAssembly: what the wasmbot package speaks on the guest
// side and the arena package on the host side. A bot written in Go never
// needs it (wasmbot does it all); it is the spec for a guest in another
// language.
//
// A guest is a WASI preview 1 reactor module (it exports _initialize, and
// its memory as "memory") with these exports:
//
//	obgl_abi() -> i32          the ABI version: Version
//	obgl_buffer() -> i32       address of the guest's exchange buffer, BufferSize bytes
//	obgl_start() -> i64        build the bot
//	obgl_act(n i32) -> i64     the State is the first n bytes of the buffer; return the Action
//
// obgl_start and obgl_act return a Reply: 0 / an Action packed by
// PackAction, or, with ErrBit set, an error whose message is in the
// buffer, its length the low 32 bits.
//
// A State is a run of little-endian int32 words (EncodeState). Its Rand is
// the host's: during obgl_act the guest may call the one import besides
// WASI,
//
//	obgl.rand(dst i32, n i32) -> i32   write the next n uint64 of State.Rand's source at dst; 0 if done
//
// so that a wasm bot draws exactly the numbers its native self would. A
// guest may draw ahead and keep what it has not used for later decisions,
// as long as it drops them when the State's RandGen changes: the host
// counts up RandGen whenever the State.Rand it is handed is a different
// one.
package abi

import (
	"encoding/binary"
	"fmt"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

// Version is the ABI version a guest returns from obgl_abi.
const Version = 1

// The import the host provides besides WASI.
const (
	ImportModule = "obgl"
	ImportRand   = "rand"
	// MaxRand is the most words one obgl.rand call may ask for.
	MaxRand = 4096
)

// The export names.
const (
	ExportABI    = "obgl_abi"
	ExportBuffer = "obgl_buffer"
	ExportStart  = "obgl_start"
	ExportAct    = "obgl_act"
)

// BufferSize is the guest's exchange buffer. A State takes 4*(24 + 5 events)
// bytes; 64 KiB holds about 3,000 events, far more than a hand has.
const BufferSize = 64 << 10

const head = 24 // words before the history

// EncodeState writes s, and the generation of its Rand, into b and
// returns the bytes used.
func EncodeState(b []byte, s sdk.State, randGen uint32) (int, error) {
	n := 4 * (head + 5*len(s.History))
	if n > len(b) {
		return 0, fmt.Errorf("abi: a state of %d bytes does not fit %d", n, len(b))
	}
	if len(s.Board) > 5 {
		return 0, fmt.Errorf("abi: a board of %d cards", len(s.Board))
	}
	var board [5]int32
	for i, c := range s.Board {
		board[i] = int32(c)
	}
	w := make([]int32, 0, n/4)
	w = append(w, int32(s.Hand), int32(s.Seat), int32(s.Round), int32(s.Hole[0]), int32(s.Hole[1]), int32(len(s.Board)))
	w = append(w, board[:]...)
	w = append(w, int32(s.Pot), int32(s.Stack), int32(s.OppStack), int32(s.InRound), int32(s.OppInRound),
		bool32(s.Legal.Fold), bool32(s.Legal.Check), int32(s.Legal.Call), int32(s.Legal.MinTo), int32(s.Legal.MaxTo),
		int32(s.BigBlind), int32(len(s.History)), int32(randGen))
	for _, e := range s.History {
		w = append(w, int32(e.Seat), int32(e.Round), int32(e.Kind), int32(e.To), int32(e.Cost))
	}
	for i, x := range w {
		binary.LittleEndian.PutUint32(b[4*i:], uint32(x))
	}
	return n, nil
}

// DecodeState is EncodeState's inverse; the State's Rand is left nil.
func DecodeState(b []byte) (s sdk.State, randGen uint32, err error) {
	if len(b) < 4*head {
		return s, 0, fmt.Errorf("abi: a short state (%d bytes)", len(b))
	}
	at := func(i int) int { return int(int32(binary.LittleEndian.Uint32(b[4*i:]))) }
	s.Hand, s.Seat, s.Round = at(0), at(1), sdk.Round(at(2))
	s.Hole = [2]sdk.Card{sdk.Card(at(3)), sdk.Card(at(4))}
	nb := at(5)
	if nb < 0 || nb > 5 {
		return s, 0, fmt.Errorf("abi: a board of %d cards", nb)
	}
	s.Board = make([]sdk.Card, nb)
	for i := range nb {
		s.Board[i] = sdk.Card(at(6 + i))
	}
	s.Pot, s.Stack, s.OppStack, s.InRound, s.OppInRound = at(11), at(12), at(13), at(14), at(15)
	s.Legal = sdk.Legal{Fold: at(16) != 0, Check: at(17) != 0, Call: at(18), MinTo: at(19), MaxTo: at(20)}
	s.BigBlind = at(21)
	nh, randGen := at(22), uint32(at(23))
	if nh < 0 || 4*(head+5*nh) > len(b) {
		return s, 0, fmt.Errorf("abi: a history of %d events in %d bytes", nh, len(b))
	}
	s.History = make([]sdk.Event, nh)
	for i := range nh {
		o := head + 5*i
		s.History[i] = sdk.Event{Seat: at(o), Round: sdk.Round(at(o + 1)), Kind: sdk.Kind(at(o + 2)), To: at(o + 3), Cost: at(o + 4)}
	}
	return s, randGen, nil
}

// ErrBit marks a Reply as an error.
const ErrBit = uint64(1) << 63

// PackAction is an Action as a Reply: its Kind in the high 32 bits, its To
// in the low 32.
func PackAction(a sdk.Action) uint64 {
	return uint64(uint32(int32(a.Kind)))<<32 | uint64(uint32(int32(a.To)))
}

// UnpackAction is PackAction's inverse, for a Reply without ErrBit.
func UnpackAction(r uint64) sdk.Action {
	return sdk.Action{Kind: sdk.Kind(int32(r >> 32)), To: int(int32(uint32(r)))}
}

// PackError is an error Reply for a message of n bytes in the buffer.
func PackError(n int) uint64 { return ErrBit | uint64(uint32(n)) }

// ErrorLen is the message length of an error Reply.
func ErrorLen(r uint64) int { return int(uint32(r)) }

func bool32(b bool) int32 {
	if b {
		return 1
	}
	return 0
}
