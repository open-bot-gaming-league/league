package engine

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
)

// A blueprint is the strategy as it is served: one byte per (infoset,
// action), probability = byte/255, each infoset's bytes summing to 255. It
// is laid out exactly like the solver's arrays (Node.Slot), so serving a
// decision is one index computation and a read from an mmap'd file.
//
// File layout (little-endian):
//
//	bytes 0-7    magic "PKRBP001"
//	bytes 8-15   length of the JSON header
//	bytes 16-    JSON header (BlueprintHeader), zero-padded to a 4096 boundary
//	then         Slots bytes of strategy

const (
	blueprintMagic = "PKRBP001"
	blueprintAlign = 4096
)

// BlueprintHeader identifies what the bytes mean. Tree and Abstraction
// must match the game the file is opened against; the rest is provenance.
type BlueprintHeader struct {
	Game        string    `json:"game"`
	Tree        string    `json:"tree"`        // Tree.Hash, hex
	Abstraction string    `json:"abstraction"` // Abstraction.ID
	NumBuckets  []int     `json:"num_buckets"`
	Slots       int64     `json:"slots"`
	Config      *NLConfig `json:"config,omitempty"` // the betting tree's config, for hold'em
	Iterations  int64     `json:"iterations"`
	Regrets     StoreKind `json:"regret_storage,omitempty"`
	Sums        StoreKind `json:"sum_storage,omitempty"`
	Seed        uint64    `json:"seed,omitempty"`
	// Derived says how the bytes were changed after training, if they
	// were (cmd/scaleraise); empty for a blueprint as trained.
	Derived string `json:"derived,omitempty"`
}

type Blueprint struct {
	Header BlueprintHeader
	probs  []uint8
	mapped []byte // the whole mmap, when the blueprint came from OpenBlueprint
}

func gameHeader(g *Game) BlueprintHeader {
	return BlueprintHeader{Game: g.Rules.Name, Tree: fmt.Sprintf("%016x", g.Tree.Hash()),
		Abstraction: g.Abs.ID(), NumBuckets: g.Tree.NumBuckets, Slots: g.Tree.Slots, Config: g.Config}
}

// NewBlueprint quantizes strat over every infoset of g.
func NewBlueprint(g *Game, strat Strategy) *Blueprint {
	b := &Blueprint{Header: gameHeader(g), probs: make([]uint8, g.Tree.Slots)}
	var buf [MaxActions]float64
	for i := range g.Tree.Nodes {
		n := &g.Tree.Nodes[i]
		if n.Kind != Decision {
			continue
		}
		na := int64(n.NumActions)
		for bucket := 0; bucket < g.Tree.NumBuckets[n.Round]; bucket++ {
			strat.Probs(n, bucket, buf[:na])
			off := n.Slot(bucket)
			quantize(buf[:na], b.probs[off:off+na])
		}
	}
	return b
}

// Blueprint is the solver's average strategy in served form.
func (s *Solver) Blueprint() *Blueprint {
	b := NewBlueprint(s.Game, s.AverageStrategy())
	b.Header.Iterations = s.iters
	b.Header.Regrets, b.Header.Sums = s.Config.RegretStorage, s.Config.SumStorage
	b.Header.Seed = s.Config.Seed
	return b
}

// quantize writes probabilities as bytes summing to exactly 255: round
// down, then hand the leftover units to the largest remainders (lowest
// action first on ties). An action with probability 0 always gets 0.
func quantize(probs []float64, out []uint8) {
	var rem [MaxActions]float64
	left := 255
	for a, p := range probs {
		x := p * 255
		q := int(x)
		out[a] = uint8(q)
		rem[a] = x - float64(q)
		left -= q
	}
	for ; left > 0; left-- {
		best := 0
		for a := 1; a < len(probs); a++ {
			if rem[a] > rem[best] {
				best = a
			}
		}
		out[best]++
		rem[best] = -1
	}
}

// Probs implements Strategy.
func (b *Blueprint) Probs(n *Node, bucket int, out []float64) {
	off := n.Slot(bucket)
	for a := range out {
		out[a] = float64(b.probs[off+int64(a)]) / 255
	}
}

// Action picks the action to play at (n, bucket) from 32 uniform random
// bits. This is the serving decision: a pure function of the stored bytes
// and r.
func (b *Blueprint) Action(n *Node, bucket int, r uint32) int {
	off := n.Slot(bucket)
	target := uint32(uint64(r) * 255 >> 32) // uniform in [0, 255)
	var cum uint32
	last := int(n.NumActions) - 1
	for a := 0; a < last; a++ {
		cum += uint32(b.probs[off+int64(a)])
		if target < cum {
			return a
		}
	}
	return last
}

// Equal reports whether two blueprints hold the same strategy bytes.
func (b *Blueprint) Equal(other *Blueprint) bool { return bytes.Equal(b.probs, other.probs) }

func (b *Blueprint) WriteFile(path string) error {
	header, err := json.Marshal(b.Header)
	if err != nil {
		return err
	}
	head := make([]byte, 16, blueprintAlign)
	copy(head, blueprintMagic)
	binary.LittleEndian.PutUint64(head[8:], uint64(len(header)))
	head = append(head, header...)
	if pad := len(head) % blueprintAlign; pad != 0 {
		head = append(head, make([]byte, blueprintAlign-pad)...)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err = f.Write(head); err == nil {
		_, err = f.Write(b.probs)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// OpenBlueprint maps a blueprint file read-only and checks it was written
// for g's tree and abstraction. Nothing is read into the heap: the
// strategy bytes are the page cache's.
func OpenBlueprint(path string, g *Game) (*Blueprint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 16 {
		return nil, fmt.Errorf("%s: too short to be a blueprint", path)
	}
	mapped, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("%s: mmap: %w", path, err)
	}
	b := &Blueprint{mapped: mapped}
	fail := func(format string, args ...any) (*Blueprint, error) {
		b.Close()
		return nil, fmt.Errorf("%s: "+format, append([]any{path}, args...)...)
	}
	if string(mapped[:8]) != blueprintMagic {
		return fail("not a blueprint file")
	}
	hlen := int64(binary.LittleEndian.Uint64(mapped[8:]))
	if hlen <= 0 || 16+hlen > info.Size() {
		return fail("corrupt header length")
	}
	if err := json.Unmarshal(mapped[16:16+hlen], &b.Header); err != nil {
		return fail("header: %v", err)
	}
	want := gameHeader(g)
	if b.Header.Tree != want.Tree || b.Header.Slots != want.Slots {
		return fail("written for a different betting tree (%s, %d slots; this game is %s, %d slots)",
			b.Header.Tree, b.Header.Slots, want.Tree, want.Slots)
	}
	if b.Header.Abstraction != want.Abstraction {
		return fail("written for card abstraction %q, this game uses %q", b.Header.Abstraction, want.Abstraction)
	}
	start := (16 + hlen + blueprintAlign - 1) / blueprintAlign * blueprintAlign
	if start+want.Slots != info.Size() {
		return fail("size %d, want %d", info.Size(), start+want.Slots)
	}
	b.probs = mapped[start:]
	return b, nil
}

// Close unmaps a blueprint opened with OpenBlueprint.
func (b *Blueprint) Close() error {
	if b.mapped == nil {
		return nil
	}
	err := syscall.Munmap(b.mapped)
	b.mapped, b.probs = nil, nil
	return err
}
