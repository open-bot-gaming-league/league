//go:build !linux

package league

// process is this process's CPU time so far and its peak resident memory;
// measured on Linux only, where the tables run.
type process struct {
	ok      bool
	cpu     float64
	peakRSS int64
}

func readProcess() process { return process{} }
