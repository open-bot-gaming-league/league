package engine

import (
	"math"
	"testing"
)

func TestFloat16RepresentableValuesRoundTrip(t *testing.T) {
	rnd := newRNG(1, 0)
	for i := 0; i < 1<<16; i++ {
		h := uint16(i)
		if h&0x7C00 == 0x7C00 || h == 0x8000 { // inf, nan, and -0 (which encodes as -0 but compares equal to 0)
			continue
		}
		f := float16Decode(h)
		if got := float16Encode(float64(f), rnd.uint64()); got != h {
			t.Fatalf("%#04x decodes to %g, which re-encodes to %#04x", h, f, got)
		}
		if h&0x7F80 == 0 || h&0x7F80 == 0x7F80 { // bfloat16 zero/subnormal and inf/nan patterns
			continue
		}
		if got := bfloat16Encode(float64(bfloat16Decode(h)), rnd.uint64()); got != h {
			t.Fatalf("bfloat16 %#04x does not round-trip (got %#04x)", h, got)
		}
	}
	if float16Decode(0x3C00) != 1 || float16Decode(0xC000) != -2 || float16Decode(0x7BFF) != 65504 || float16Decode(0x0001) != 1.0/(1<<24) {
		t.Fatal("float16Decode disagrees with IEEE half precision on spot values")
	}
}

// A value between two representable neighbours must land on one of them,
// and on average on itself, in every format, including values a hair off a
// representable one (the case a float32 intermediate would lose).
func TestStochasticRoundingIsUnbiased(t *testing.T) {
	values := []float64{0.1, -0.1, 1.0001, 3.14159, -777.777, 30000.01, 59999.9, 1e-5, -3e-7, 2e-8,
		3000 + 6e-5, 3000 - 6e-5, 1<<25 + 0.25, -(1<<25 + 0.25)}
	rnd := newRNG(7, 0)
	codecs := []struct {
		name  string
		round func(float64) float64
	}{
		{"float16", func(v float64) float64 { return float64(float16Decode(float16Encode(v, rnd.uint64()))) }},
		{"bfloat16", func(v float64) float64 { return float64(bfloat16Decode(bfloat16Encode(v, rnd.uint64()))) }},
		{"float32", func(v float64) float64 { return float64(float32Round(v, rnd)) }},
	}
	const n = 400_000
	for _, c := range codecs {
		for _, v := range values {
			// Skip what float16 cannot hold, and for bfloat16 the case whose
			// round-up chance (1 in a million) no sample of this size sees;
			// TestSmallIncrementsAccumulateInEveryStore covers it.
			if math.Abs(v) > 65504 && c.name != "float32" {
				continue
			}
			seen := map[float64]int{}
			var sum float64
			for i := 0; i < n; i++ {
				f := c.round(v)
				seen[f]++
				sum += f
			}
			if len(seen) > 2 {
				t.Errorf("%s(%g): %d distinct results, want at most 2", c.name, v, len(seen))
			}
			lo, hi := math.Inf(1), math.Inf(-1)
			for f := range seen {
				lo, hi = min(lo, f), max(hi, f)
			}
			if !(lo <= v && v <= hi) {
				t.Errorf("%s(%g): results %g..%g do not bracket it", c.name, v, lo, hi)
			}
			// A draw is hi with probability p = (v-lo)/(hi-lo), so the mean
			// of n draws has standard error (hi-lo)*sqrt(p(1-p)/n); allow 5
			// of those.
			if hi > lo {
				p := (v - lo) / (hi - lo)
				tol := 5 * (hi - lo) * math.Sqrt(p*(1-p)/n)
				if mean := sum / n; math.Abs(mean-v) > tol {
					t.Errorf("%s(%g): mean %.10g is off by more than %g", c.name, v, mean, tol)
				}
			}
		}
	}
}

// The reason for stochastic rounding: increments far below the spacing of
// the stored value must still add up. Under round-to-nearest every one of
// these sums would stay where it started. Each case is a strategy sum deep
// into a long run: float16 and bfloat16 counting visits in their own
// units, float32 past 2^24 visits.
func TestSmallIncrementsAccumulateInEveryStore(t *testing.T) {
	cases := []struct {
		kind            StoreKind
		start, step     float32
		steps           int
		want, tolerance float64
	}{
		{Float16, 30000, 0.01, 100_000, 31000, 400},
		{Float16, 3000, 1.0 / (1 << 14), 16 << 20, 4024, 150}, // one visit at a time, 50M visits in
		{BFloat16, 30000, 0.01, 100_000, 31000, 400},
		{BFloat16, 5e7, 1, 16 << 20, 5e7 + 16<<20, 4e6},
		{Float32, 1 << 25, 0.25, 4_000_000, 1<<25 + 1_000_000, 10_000},
	}
	for _, c := range cases {
		st, _ := NewStore(c.kind, 1)
		rnd := newRNG(3, 0)
		floor := float32(math.Inf(-1))
		st.Add(0, []float32{c.start}, floor, rnd)
		for i := 0; i < c.steps; i++ {
			st.Add(0, []float32{c.step}, floor, rnd)
		}
		v := make([]float32, 1)
		st.Load(0, v)
		if math.Abs(float64(v[0])-c.want) > c.tolerance {
			t.Errorf("%s: %g + %d * %g = %g, want about %g", c.kind, c.start, c.steps, c.step, v[0], c.want)
		}
	}
}

func TestAddFloorsAndRescales(t *testing.T) {
	st, _ := NewStore(Float16, 3)
	rnd := newRNG(1, 0)
	st.Add(0, []float32{120000, 30000, -500}, -100, rnd)
	got := make([]float32, 3)
	st.Load(0, got)
	// Floored at -100 first, then halved once to fit float16Max.
	if got[0] != 60000 || got[1] != 15000 || got[2] != -50 {
		t.Errorf("got %v, want [60000 15000 -50]", got)
	}
	// float16 cannot go below -float16Max whatever floor is asked for.
	st.Add(0, []float32{0, 0, -1e9}, float32(math.Inf(-1)), rnd)
	st.Load(0, got)
	if got[2] != -float16Max {
		t.Errorf("unbounded floor: got %v, want %v", got[2], -float16Max)
	}
}

// Storage formats on Leduc at an iteration count where 2-byte effects
// show (EVAL.md "Storage" has the full table and the reasoning):
//   - float16 regrets must be as good as float32 regrets (the liar's-dice
//     bar: within 1.5x at equal iterations);
//   - 2-byte strategy sums carry rounding noise that float32 sums do not,
//     so they get a looser, absolute bound that still catches a broken
//     average (a float16 sum that halves on overflow sits near 0.13 here).
func TestLeducStorageFormats(t *testing.T) {
	const iters = 3_000_000
	run := func(regret, sum StoreKind) float64 {
		g := NewLeduc()
		cfg := DefaultTrainConfig()
		cfg.RegretStorage, cfg.SumStorage = regret, sum
		s := trained(t, g, cfg, iters)
		expl, _ := Exploitability(g, s.AverageStrategy(), 1)
		t.Logf("regrets %-8s sums %-8s exploitability %.5f after %d iterations", regret, sum, expl, iters)
		return expl
	}
	base := run(Float32, Float32)
	if expl := run(Float16, Float32); expl > 1.5*base {
		t.Errorf("float16 regrets: exploitability %.5f is more than 1.5x float32's %.5f", expl, base)
	}
	if expl := run(Float16, Float16); expl > 0.05 {
		t.Errorf("float16 regrets and sums: exploitability %.5f, want < 0.05", expl)
	}
	if expl := run(BFloat16, BFloat16); expl > 0.08 {
		t.Errorf("bfloat16 regrets and sums: exploitability %.5f, want < 0.08", expl)
	}
}
