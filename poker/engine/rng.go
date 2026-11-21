package engine

import "math/bits"

// rng is a small, fast, seedable generator (xoshiro256**), one per worker.
// math/rand/v2 would do; this keeps the hot loop free of interface calls
// and the stream stable across Go releases, which the fixed-seed tests
// rely on.
type rng struct{ s [4]uint64 }

func newRNG(seed, stream uint64) *rng {
	r := &rng{}
	// SplitMix64 expands the two words into the state.
	x := seed ^ stream*0x9E3779B97F4A7C15
	for i := range r.s {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ z>>30) * 0xBF58476D1CE4E5B9
		z = (z ^ z>>27) * 0x94D049BB133111EB
		r.s[i] = z ^ z>>31
	}
	return r
}

func (r *rng) uint64() uint64 {
	s := &r.s
	out := bits.RotateLeft64(s[1]*5, 7) * 9
	t := s[1] << 17
	s[2] ^= s[0]
	s[3] ^= s[1]
	s[1] ^= s[2]
	s[0] ^= s[3]
	s[2] ^= t
	s[3] = bits.RotateLeft64(s[3], 45)
	return out
}

func (r *rng) uint32() uint32 { return uint32(r.uint64() >> 32) }

// intn returns a uniform integer in [0, n) for small n (multiply-shift; the
// bias is below 2^-32*n, far under sampling noise).
func (r *rng) intn(n int) int {
	return int(uint64(r.uint32()) * uint64(n) >> 32)
}

// float32 returns a uniform value in [0, 1).
func (r *rng) float32() float32 {
	return float32(r.uint32()>>8) * (1.0 / (1 << 24))
}

// float64 returns a uniform value in [0, 1).
func (r *rng) float64() float64 {
	return float64(r.uint64()>>11) * (1.0 / (1 << 53))
}
