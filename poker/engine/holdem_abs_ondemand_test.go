package engine

import (
	"math/rand/v2"
	"path/filepath"
	"testing"
)

// TestTableReadOnDemand: a bucket file opened to read on demand (as in wasm)
// gives the same buckets as the mapped file, and writes the same file back.
func TestTableReadOnDemand(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 400 MB bucket file")
	}
	r := rand.New(rand.NewPCG(5, 6))
	src := &TableAbstraction{Header: TableHeader{Buckets: [MaxRounds]int{169, 50, 50, 50}}}
	src.attachBoards()
	for round := 1; round < MaxRounds; round++ {
		src.table[round] = make([]uint16, len(src.boards[round].reps)*holeSlots)
		for i := range src.table[round] {
			src.table[round][i] = uint16(r.IntN(50))
		}
	}
	src.Header.Hash = src.contentHash()
	dir := t.TempDir()
	path := filepath.Join(dir, "abs.bin")
	if err := src.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	was := readTablesOnDemand
	defer func() { readTablesOnDemand = was }()
	readTablesOnDemand = false
	mapped, err := OpenTableAbstraction(path)
	if err != nil {
		t.Fatal(err)
	}
	defer mapped.Close()
	readTablesOnDemand = true
	onDemand, err := OpenTableAbstraction(path)
	if err != nil {
		t.Fatal(err)
	}
	defer onDemand.Close()
	if onDemand.file == nil || onDemand.table[1] != nil {
		t.Fatal("the table was not opened to read on demand")
	}
	for range 100_000 {
		round := 1 + r.IntN(MaxRounds-1)
		cards := randomDeal(newRNG(r.Uint64(), 0), 2+round+2)
		hole, board := cards[:2], cards[2:]
		if a, b := mapped.Bucket(round, hole, board), onDemand.Bucket(round, hole, board); a != b {
			t.Fatalf("round %d %v %v: mapped %d, on demand %d", round, hole, board, a, b)
		}
	}
	again := filepath.Join(dir, "again.bin")
	if err := onDemand.WriteFile(again); err != nil {
		t.Fatal(err)
	}
	readTablesOnDemand = false
	back, err := OpenTableAbstraction(again)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if back.ID() != mapped.ID() || back.contentHash() != src.Header.Hash {
		t.Fatalf("written back as %s (hash %s), want %s", back.ID(), back.contentHash(), mapped.ID())
	}
}
