package engine

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// TableAbstraction is the hold'em card abstraction read from a file built
// by cmd/buckets: lossless preflop classes, then one bucket per (hole
// cards, board) on the flop, turn and river, looked up in a table.
//
// The tables are indexed by (suit-isomorphism class of the board, hole
// cards with the same suit relabelling applied): 1,755 / 16,432 / 134,459
// board classes times 1,326 hole slots. That is within 1.5x of a perfect
// hand-isomorphism index and needs nothing cleverer than a lookup table
// from board to class.
//
// File layout (little-endian):
//
//	bytes 0-7    magic "PKRABS01"
//	bytes 8-15   length of the JSON header
//	bytes 16-    JSON header (TableHeader), zero-padded to a 4096 boundary
//	then         uint16 bucket tables: flop, turn, river

const (
	tableMagic   = "PKRABS01"
	holeSlots    = 1326 // C(52, 2)
	noBucket     = 0xFFFF
	preflopCount = 169
)

var errBigEndian = errors.New("engine: bucket files are little-endian and are written and mapped as-is; this machine is not")

type TableHeader struct {
	Buckets  [MaxRounds]int `json:"buckets"` // preflop, flop, turn, river
	Bins     int            `json:"bins"`    // equity histogram bins used for flop and turn
	Seed     uint64         `json:"seed"`
	Patience int            `json:"patience,omitempty"` // see BucketConfig.Patience
	// RiverFeature is "ochs" when river hands were clustered on
	// opponent-cluster equities instead of a single equity (ochs.go).
	RiverFeature string `json:"river_feature,omitempty"`
	// FlopTurnFrom is the ID of the file the flop and turn tables were
	// copied from, if they were.
	FlopTurnFrom string `json:"flop_turn_from,omitempty"`
	Hash         string `json:"hash"` // of the table contents; part of ID
}

type TableAbstraction struct {
	Header TableHeader
	boards [MaxRounds]*boardIndex // rounds 1-3
	table  [MaxRounds][]uint16    // rounds 1-3
	mapped []byte
}

func (t *TableAbstraction) ID() string {
	b := t.Header.Buckets
	return fmt.Sprintf("table-%d-%d-%d-%d-%s", b[0], b[1], b[2], b[3], t.Header.Hash)
}

func (t *TableAbstraction) NumBuckets(round int) int { return t.Header.Buckets[round] }

func (t *TableAbstraction) Bucket(round int, hole, board []Card) int {
	if round == 0 {
		return preflopClass(13, hole[0], hole[1])
	}
	return int(t.table[round][t.boards[round].slot(hole, board)])
}

// boardIndex maps every n-card board to its suit-isomorphism class and to
// the suit relabelling that turns it into the class representative (the
// member with the smallest combIndex).
type boardIndex struct {
	n     int
	class []uint32 // by combIndex of the board
	perm  []uint8  // by combIndex: index into suitPerms
	reps  [][]Card // by class
	orbit []uint32 // by class: number of boards in it
}

// slot is the table index of (hole, board): class * 1326 + index of the
// relabelled hole cards.
func (b *boardIndex) slot(hole, board []Card) int {
	i := combIndex(board)
	p := &suitPerms[b.perm[i]]
	lo, hi := permuteCard(hole[0], p), permuteCard(hole[1], p)
	if lo > hi {
		lo, hi = hi, lo
	}
	return int(b.class[i])*holeSlots + binom[hi][2] + int(lo)
}

var (
	boardIndexOnce [6]sync.Once
	boardIndexes   [6]*boardIndex
)

// fullDeckBoards returns the shared index for n-card boards (n = 3, 4, 5)
// of the 52-card deck, building it on first use.
func fullDeckBoards(n int) *boardIndex {
	boardIndexOnce[n].Do(func() { boardIndexes[n] = newBoardIndex(n) })
	return boardIndexes[n]
}

func newBoardIndex(n int) *boardIndex {
	total := binom[maxDeck][n]
	b := &boardIndex{n: n, class: make([]uint32, total), perm: make([]uint8, total)}
	boards := make([]Card, 0, total*n)
	forEachCombo(maxDeck, n, 0, func(cs []Card) { boards = append(boards, cs...) })
	smallest := make([]uint32, total) // smallest combIndex in each board's class
	parallelFor(total, runtime.GOMAXPROCS(0), func(lo, hi, _ int) {
		for i := lo; i < hi; i++ {
			best, bestPerm := i, 0
			for p := range suitPerms {
				if img := permuteIndex(boards[i*n:(i+1)*n], &suitPerms[p]); img < best {
					best, bestPerm = img, p
				}
			}
			smallest[i], b.perm[i] = uint32(best), uint8(bestPerm)
		}
	})
	for i := 0; i < total; i++ {
		if int(smallest[i]) == i {
			b.class[i] = uint32(len(b.reps))
			b.reps = append(b.reps, boards[i*n:(i+1)*n])
			b.orbit = append(b.orbit, 0)
		} else {
			b.class[i] = b.class[smallest[i]] // a smaller index, already numbered
		}
		b.orbit[b.class[i]]++
	}
	return b
}

// parallelFor splits [0, n) into contiguous chunks, one per worker.
func parallelFor(n, workers int, fn func(lo, hi, worker int)) {
	workers = max(1, min(workers, n))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := n*w/workers, n*(w+1)/workers
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(lo, hi, w)
		}()
	}
	wg.Wait()
}

func (t *TableAbstraction) attachBoards() {
	for round := 1; round < MaxRounds; round++ {
		t.boards[round] = fullDeckBoards(round + 2)
	}
}

func tableBytes(tbl []uint16) []byte {
	if len(tbl) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&tbl[0])), len(tbl)*2)
}

func (t *TableAbstraction) contentHash() string {
	h := fnv.New64a()
	for round := 1; round < MaxRounds; round++ {
		h.Write(tableBytes(t.table[round]))
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

func (t *TableAbstraction) WriteFile(path string) error {
	if !nativeLittleEndian() {
		return errBigEndian
	}
	header, err := json.Marshal(t.Header)
	if err != nil {
		return err
	}
	head := make([]byte, 16, blueprintAlign)
	copy(head, tableMagic)
	binary.LittleEndian.PutUint64(head[8:], uint64(len(header)))
	head = append(head, header...)
	if pad := len(head) % blueprintAlign; pad != 0 {
		head = append(head, make([]byte, blueprintAlign-pad)...)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = f.Write(head)
	for round := 1; round < MaxRounds && err == nil; round++ {
		_, err = f.Write(tableBytes(t.table[round]))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// OpenTableAbstraction maps a bucket file read-only. The tables stay in
// the page cache, shared by every process that opens the file.
func OpenTableAbstraction(path string) (*TableAbstraction, error) {
	if !nativeLittleEndian() {
		return nil, errBigEndian
	}
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
		return nil, fmt.Errorf("%s: too short to be a bucket file", path)
	}
	mapped, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("%s: mmap: %w", path, err)
	}
	t := &TableAbstraction{mapped: mapped}
	fail := func(format string, args ...any) (*TableAbstraction, error) {
		t.Close()
		return nil, fmt.Errorf("%s: "+format, append([]any{path}, args...)...)
	}
	if string(mapped[:8]) != tableMagic {
		return fail("not a bucket file")
	}
	hlen := int64(binary.LittleEndian.Uint64(mapped[8:]))
	if hlen <= 0 || 16+hlen > info.Size() {
		return fail("corrupt header length")
	}
	if err := json.Unmarshal(mapped[16:16+hlen], &t.Header); err != nil {
		return fail("header: %v", err)
	}
	t.attachBoards()
	at := (16 + hlen + blueprintAlign - 1) / blueprintAlign * blueprintAlign
	for round := 1; round < MaxRounds; round++ {
		entries := int64(len(t.boards[round].reps)) * holeSlots
		if at+entries*2 > info.Size() {
			return fail("truncated")
		}
		t.table[round] = unsafe.Slice((*uint16)(unsafe.Pointer(&mapped[at])), entries)
		at += entries * 2
	}
	if at != info.Size() {
		return fail("size %d, want %d", info.Size(), at)
	}
	return t, nil
}

func (t *TableAbstraction) Close() error {
	if t.mapped == nil {
		return nil
	}
	err := syscall.Munmap(t.mapped)
	t.mapped = nil
	t.table = [MaxRounds][]uint16{}
	return err
}

// CheckSuitInvariance draws random hands and boards and checks that
// relabelling the suits never changes the bucket, and that every bucket
// is in range. Best response's flop symmetry depends on the first; the
// solver's array bounds on the second.
func (t *TableAbstraction) CheckSuitInvariance(samples int, seed uint64) error {
	rnd := newRNG(seed, 0)
	for s := 0; s < samples; s++ {
		var cards [7]Card
		copy(cards[:], randomDeal(rnd, len(cards)))
		perm := &suitPerms[rnd.intn(len(suitPerms))]
		var other [7]Card
		for i, c := range cards {
			other[i] = permuteCard(c, perm)
		}
		for round := 0; round < MaxRounds; round++ {
			n := 0
			if round > 0 {
				n = round + 2
			}
			bucket := func(cs [7]Card) int {
				hole, board := cs[:2], cs[2:2+n]
				sortCards(hole)
				sortCards(board)
				return t.Bucket(round, hole, board)
			}
			a, b := bucket(cards), bucket(other)
			if a < 0 || a >= t.NumBuckets(round) {
				return fmt.Errorf("round %d: bucket %d out of range for cards %v", round, a, cards[:2+n])
			}
			if a != b {
				return fmt.Errorf("round %d: cards %v are in bucket %d, relabelled %v in bucket %d", round, cards[:2+n], a, other[:2+n], b)
			}
		}
	}
	return nil
}

// BucketFrequencies estimates how often each bucket of a round is dealt,
// from random hands and boards.
func (t *TableAbstraction) BucketFrequencies(round, samples int, seed uint64) []float64 {
	rnd := newRNG(seed, uint64(round)+1)
	freq := make([]float64, t.NumBuckets(round))
	n := 0
	if round > 0 {
		n = round + 2
	}
	for s := 0; s < samples; s++ {
		cs := randomDeal(rnd, 2+n)
		hole, board := cs[:2], cs[2:]
		sortCards(hole)
		sortCards(board)
		freq[t.Bucket(round, hole, board)] += 1 / float64(samples)
	}
	return freq
}

// randomDeal draws n distinct cards from the full deck.
func randomDeal(rnd *rng, n int) []Card {
	var used uint64
	cs := make([]Card, 0, n)
	for len(cs) < n {
		c := rnd.intn(maxDeck)
		if used&(1<<uint(c)) == 0 {
			used |= 1 << uint(c)
			cs = append(cs, Card(c))
		}
	}
	return cs
}

func nativeLittleEndian() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}
