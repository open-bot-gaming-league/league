//go:build !unix

package engine

import (
	"io"
	"os"
)

// mapFile reads the whole file where there is no mmap (Windows, wasm):
// the same bytes, all of them in memory.
func mapFile(f *os.File, size int64) ([]byte, error) {
	b := make([]byte, size)
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, err
	}
	return b, nil
}

func unmapFile([]byte) error { return nil }
