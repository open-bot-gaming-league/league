package league

import (
	"fmt"
	"math"
	"strings"
)

// Prices is what compute costs, in USD.
type Prices struct {
	VCPUSecond float64 // per vCPU-second
	GiBSecond  float64 // per GiB-second of memory
	MinSeconds float64 // the least time an instance is billed for
	Source     string  // where the numbers come from
}

// CloudRunJobs is Cloud Run jobs' default (on-demand, no committed-use
// discount) price, from the "Jobs" table of
// https://cloud.google.com/run/pricing, read 2026-11-27: $0.000018 per
// vCPU-second and $0.000002 per GiB-second. Jobs are billed for an
// instance's whole lifetime at the CPU and memory it was given, with a
// minimum of one minute. The free tier is not counted.
var CloudRunJobs = Prices{
	VCPUSecond: 0.000018,
	GiBSecond:  0.000002,
	MinSeconds: 60,
	Source:     "cloud.google.com/run/pricing, Jobs, default USD, read 2026-11-27",
}

// Task is the size of the instance a table runs on. Zero fields default
// from the record: VCPU to the pairs played at once, GiB to the peak
// resident memory rounded up to a whole GiB.
type Task struct {
	VCPU float64
	GiB  float64
}

// Cost is a table's compute, two ways: what it used, and what a Cloud Run
// job of a given size would be billed for running it.
type Cost struct {
	WallS   float64 // the table's wall time
	CPUS    float64 // CPU-seconds used
	PeakGiB float64 // peak resident memory
	GiBS    float64 // memory used, estimated as peak × wall
	Used    float64 // USD for CPUS and GiBS: a floor no instance size beats

	Task    Task
	BilledS float64 // the task's billed time: the wall time, or the CPU time spread over its vCPUs if longer (a smaller task runs longer), and at least MinSeconds
	Billed  float64 // USD for the task's vCPU and memory over BilledS
	Prices  Prices
}

// CostOf prices a record's Measured figures. A record without CPU or
// memory figures (not measured on this platform) is an error.
func CostOf(r *Record, p Prices, t Task) (Cost, error) {
	m := r.Measured
	if m.CPUS == nil || m.PeakRSS == nil {
		return Cost{}, fmt.Errorf("league: the record has no CPU or memory figures to price")
	}
	c := Cost{WallS: m.WallS, CPUS: *m.CPUS, PeakGiB: float64(*m.PeakRSS) / (1 << 30), Prices: p}
	c.GiBS = c.PeakGiB * c.WallS
	c.Used = c.CPUS*p.VCPUSecond + c.GiBS*p.GiBSecond
	if t.VCPU == 0 {
		t.VCPU = float64(m.Parallel)
	}
	if t.GiB == 0 {
		t.GiB = max(1, math.Ceil(c.PeakGiB))
	}
	c.Task = t
	c.BilledS = max(c.WallS, c.CPUS/t.VCPU, p.MinSeconds)
	c.Billed = c.BilledS * (t.VCPU*p.VCPUSecond + t.GiB*p.GiBSecond)
	return c, nil
}

func (c Cost) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "wall %.1f s, CPU %.1f s, peak memory %.2f GiB (%.0f GiB-s as peak x wall)\n", c.WallS, c.CPUS, c.PeakGiB, c.GiBS)
	fmt.Fprintf(&b, "used:   $%.5f  (CPU-s x $%g + GiB-s x $%g)\n", c.Used, c.Prices.VCPUSecond, c.Prices.GiBSecond)
	fmt.Fprintf(&b, "billed: $%.5f  as a job task of %g vCPU, %g GiB for %.1f s (the longest of wall, CPU/vCPU, %g s)\n", c.Billed, c.Task.VCPU, c.Task.GiB, c.BilledS, c.Prices.MinSeconds)
	fmt.Fprintf(&b, "prices: %s\n", c.Prices.Source)
	return b.String()
}
