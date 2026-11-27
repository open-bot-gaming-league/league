package abi

import (
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

func TestStateRoundTrip(t *testing.T) {
	s := sdk.State{
		Hand: 7, Seat: sdk.BigBlind, Round: sdk.Turn, Hole: [2]sdk.Card{51, 0},
		Board: []sdk.Card{1, 2, 3, 4}, Pot: 900, Stack: 4600, OppStack: 4500, InRound: 0, OppInRound: 100,
		Legal:    sdk.Legal{Fold: true, Call: 100, MinTo: 200, MaxTo: 4600},
		History:  []sdk.Event{{Seat: 0, Round: sdk.Preflop, Kind: sdk.Raise, To: 300, Cost: 250}, {Seat: 1, Round: sdk.Turn, Kind: sdk.Call, To: 300, Cost: 200}},
		BigBlind: 100, Rand: rand.New(rand.NewPCG(1, 2)),
	}
	b := make([]byte, BufferSize)
	n, err := EncodeState(b, s, 7)
	if err != nil {
		t.Fatal(err)
	}
	got, gen, err := DecodeState(b[:n])
	if err != nil || gen != 7 {
		t.Fatal(gen, err)
	}
	s.Rand = nil
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, s)
	}
	if _, _, err := DecodeState(b[:n-4]); err == nil {
		t.Fatal("a truncated state decoded")
	}
	if _, err := EncodeState(make([]byte, n-1), s, 7); err == nil {
		t.Fatal("a state encoded into a buffer too small for it")
	}
}

func TestActionRoundTrip(t *testing.T) {
	for _, a := range []sdk.Action{{Kind: sdk.Fold}, {Kind: sdk.Raise, To: 5000}, {Kind: sdk.Kind(-3), To: -1}} {
		r := PackAction(a)
		if r&ErrBit != 0 && a.Kind >= 0 {
			t.Fatalf("%v packs with the error bit", a)
		}
		if got := UnpackAction(r); got != a {
			t.Fatalf("%v round-trips as %v", a, got)
		}
	}
	if r := PackError(12); r&ErrBit == 0 || ErrorLen(r) != 12 {
		t.Fatalf("PackError(12) = %x", r)
	}
}
