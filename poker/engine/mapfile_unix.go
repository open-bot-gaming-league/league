//go:build unix

package engine

import (
	"os"
	"syscall"
)

// mapFile maps a whole file read-only. Blueprints and bucket files are
// hundreds of megabytes; mapped, they cost only the pages a bot touches.
func mapFile(f *os.File, size int64) ([]byte, error) {
	return syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
}

func unmapFile(b []byte) error { return syscall.Munmap(b) }
