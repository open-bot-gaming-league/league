package engine

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"unsafe"
)

// Saving and resuming a CFR-BR run.
//
// A run is hours to days and a blueprint cannot restart one: it holds the
// average strategy in bytes, not the regrets and sums behind it. A state
// file holds everything the next iteration depends on, so a run stopped
// at any point goes on from its last saved state as if it had never
// stopped. With one worker that is exact: the resumed run's blueprints
// are byte for byte those of an uninterrupted one
// (TestCFRBRResumeMatchesUninterrupted).
//
// File layout (little-endian):
//
//	bytes 0-7    magic "PKRCBR01"
//	bytes 8-15   length of the JSON header
//	bytes 16-    JSON header (cfrbrStateHeader)
//	then         float32 regrets (Slots), float32 strategy sums (Slots),
//	             float64 trunk regrets (TrunkSlots)

const cfrbrStateMagic = "PKRCBR01"

type cfrbrStateHeader struct {
	// What the arrays mean: all of this must match the trainer a state is
	// loaded into.
	Game        string `json:"game"`
	Tree        string `json:"tree"`
	Abstraction string `json:"abstraction"`
	Slots       int64  `json:"slots"`
	TrunkSlots  int64  `json:"trunk_slots"`
	Seed        uint64 `json:"seed"`
	RegretFloor string `json:"regret_floor"` // as printed; -Inf has no JSON number

	Iterations int64 `json:"iterations"`
	// Each worker's generator, in worker order. A trainer with more
	// workers than this starts the extra ones fresh; one with fewer drops
	// the rest.
	RNG [][4]uint64 `json:"rng"`
}

func (c *CFRBR) stateHeader() cfrbrStateHeader {
	h := gameHeader(c.Game)
	return cfrbrStateHeader{Game: h.Game, Tree: h.Tree, Abstraction: h.Abstraction, Slots: h.Slots,
		TrunkSlots: int64(len(c.trunkRegret)), Seed: c.Config.Seed, RegretFloor: fmt.Sprint(c.Config.RegretFloor)}
}

// rawBytes is the memory of a slice of floats, for writing and reading
// whole arrays. State files, like bucket files, are little-endian only.
func rawBytes[F float32 | float64](v []F) []byte {
	if len(v) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&v[0])), len(v)*int(unsafe.Sizeof(v[0])))
}

// arrays lists the trainer's state in file order.
func (c *CFRBR) arrays() [][]byte {
	return [][]byte{rawBytes(c.regret.(*float32Store).v), rawBytes(c.sum.(*float32Store).v), rawBytes(c.trunkRegret)}
}

// SaveState writes the trainer's state to path, by way of a temporary
// file beside it, so a run stopped in the middle of a save leaves the
// previous file whole. Call it between Run calls, not during one.
func (c *CFRBR) SaveState(path string) error {
	if !nativeLittleEndian() {
		return errBigEndian
	}
	h := c.stateHeader()
	h.Iterations = c.iters
	for _, w := range c.workers {
		h.RNG = append(h.RNG, w.rnd.s)
	}
	header, err := json.Marshal(h)
	if err != nil {
		return err
	}
	head := make([]byte, 16, 16+len(header))
	copy(head, cfrbrStateMagic)
	binary.LittleEndian.PutUint64(head[8:], uint64(len(header)))
	f, err := os.Create(path + ".tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(append(head, header...))
	for _, a := range c.arrays() {
		if err == nil {
			_, err = f.Write(a)
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path + ".tmp")
		return err
	}
	return os.Rename(path+".tmp", path)
}

// LoadState replaces the trainer's state with a saved one. The file must
// come from the same game, tree, abstraction, seed and regret floor, and
// the trainer must not have run yet.
func (c *CFRBR) LoadState(path string) error {
	if !nativeLittleEndian() {
		return errBigEndian
	}
	if c.iters != 0 {
		return fmt.Errorf("engine: a state can only be loaded into a trainer that has not run")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: "+format, append([]any{path}, args...)...)
	}
	var head [16]byte
	if _, err := io.ReadFull(f, head[:]); err != nil || string(head[:8]) != cfrbrStateMagic {
		return fail("not a CFR-BR state file")
	}
	hlen := binary.LittleEndian.Uint64(head[8:])
	if hlen == 0 || hlen > 1<<24 {
		return fail("corrupt header length")
	}
	header := make([]byte, hlen)
	if _, err := io.ReadFull(f, header); err != nil {
		return fail("header: %v", err)
	}
	var h cfrbrStateHeader
	if err := json.Unmarshal(header, &h); err != nil {
		return fail("header: %v", err)
	}
	want := c.stateHeader()
	saved := h
	saved.Iterations, saved.RNG = 0, nil
	if fmt.Sprint(saved) != fmt.Sprint(want) {
		return fail("saved for %+v, this trainer is %+v", saved, want)
	}
	// Into scratch first: a file cut short must not leave the trainer
	// half loaded.
	arrays := c.arrays()
	scratch := make([][]byte, len(arrays))
	for i, a := range arrays {
		scratch[i] = make([]byte, len(a))
		if _, err := io.ReadFull(f, scratch[i]); err != nil {
			return fail("truncated: %v", err)
		}
	}
	if n, _ := f.Read(make([]byte, 1)); n != 0 {
		return fail("longer than its header says")
	}
	for i, a := range arrays {
		copy(a, scratch[i])
	}
	c.iters = h.Iterations
	for i, w := range c.workers {
		if i < len(h.RNG) {
			w.rnd.s = h.RNG[i]
		}
	}
	return nil
}
