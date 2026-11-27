package league

import "syscall"

// process is this process's CPU time so far and its peak resident memory.
type process struct {
	ok      bool
	cpu     float64 // seconds, user + system
	peakRSS int64   // bytes
}

func readProcess() process {
	var u syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &u) != nil {
		return process{}
	}
	tv := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return process{ok: true, cpu: tv(u.Utime) + tv(u.Stime), peakRSS: u.Maxrss * 1024} // Linux counts Maxrss in KiB
}
