package engine

import (
	"fmt"
	"math"
)

// Flat per-action storage. Regrets and strategy sums each live in one
// contiguous array indexed by Node.Slot: no maps, no per-infoset
// allocations, so bytes per (infoset, action) is exactly the element size.
// The liar's-dice experiment (liarsdice/EVAL.md, 2026-11-17) found 2-byte
// values converge fine but that map-of-struct storage ate the saving; this
// layout is the fix.
//
// The solver works in float32 and goes through Load/Save a whole infoset
// at a time, so the storage type is one interface call per node visit.

type StoreKind string

const (
	Float32  StoreKind = "float32"
	Float16  StoreKind = "float16"  // IEEE half: 10-bit mantissa, max 65504
	BFloat16 StoreKind = "bfloat16" // top half of a float32: 7-bit mantissa, float32's range
)

type Store interface {
	Kind() StoreKind
	// Load decodes len(dst) values starting at slot off.
	Load(off int64, dst []float32)
	// Add adds delta to one infoset's values, starting at slot off, and
	// clamps the results from below at floor.
	//
	// A result the format cannot hold exactly is rounded to one of its two
	// neighbours at random, with probability proportional to closeness, so
	// the stored value is right in expectation. Round-to-nearest is not
	// good enough for any of the formats: an increment under half the
	// spacing of the stored value would be dropped every time, and sums
	// would stall (float32 stops counting visits at 2^24; EVAL.md
	// "Storage" has the run where that happened).
	//
	// If a result exceeds what the format can hold (float16 only), the
	// whole infoset is halved until it fits. That keeps every ratio inside
	// the infoset, so the strategy read back is unchanged, but from then
	// on later updates at this infoset count double relative to earlier
	// ones. For regrets that is a mild discount. For strategy sums it
	// would turn the long-run average into a recent-window average, which
	// does not converge, so sums are scaled to stay clear of it (VisitUnit).
	Add(off int64, delta []float32, floor float32, rnd *rng)
	// VisitUnit is what a strategy sum should grow by per unit of
	// probability per visit: the smallest value that keeps the format's
	// full relative precision, so that overflow is as many visits away as
	// possible. 1 for formats with float32's range.
	VisitUnit() float32
	// MinFloor is the lowest regret floor the format supports.
	MinFloor() float32
	// Bytes is the size of the backing array.
	Bytes() int64
}

func NewStore(kind StoreKind, slots int64) (Store, error) {
	switch kind {
	case Float32:
		return &float32Store{v: make([]float32, slots)}, nil
	case Float16:
		return &float16Store{v: make([]uint16, slots)}, nil
	case BFloat16:
		return &bfloat16Store{v: make([]uint16, slots)}, nil
	}
	return nil, fmt.Errorf("engine: unknown storage %q (float32, float16, bfloat16)", kind)
}

// updated returns old + delta for one infoset in float64, clamped below at
// floor and halved as a whole until nothing exceeds limit.
func updated(vals *[MaxActions]float64, n int, floor, limit float64) {
	var top float64
	for a := 0; a < n; a++ {
		if vals[a] < floor {
			vals[a] = floor
		}
		top = max(top, vals[a])
	}
	for top > limit {
		top *= 0.5
		for a := 0; a < n; a++ {
			vals[a] *= 0.5
		}
	}
}

type float32Store struct{ v []float32 }

func (s *float32Store) Kind() StoreKind               { return Float32 }
func (s *float32Store) Load(off int64, dst []float32) { copy(dst, s.v[off:]) }
func (s *float32Store) VisitUnit() float32            { return 1 }
func (s *float32Store) MinFloor() float32             { return float32(math.Inf(-1)) }
func (s *float32Store) Bytes() int64                  { return int64(len(s.v)) * 4 }

func (s *float32Store) Add(off int64, delta []float32, floor float32, rnd *rng) {
	raw := s.v[off : off+int64(len(delta))]
	var vals [MaxActions]float64
	for a, d := range delta {
		vals[a] = float64(raw[a]) + float64(d)
	}
	updated(&vals, len(delta), float64(floor), math.MaxFloat32)
	for a := range raw {
		raw[a] = float32Round(vals[a], rnd)
	}
}

// float32Round rounds v to a float32 neighbour at random, right in
// expectation.
func float32Round(v float64, rnd *rng) float32 {
	f := float32(v)
	exact := float64(f)
	if exact == v {
		return f
	}
	lo, hi := exact, exact
	if exact > v {
		lo = float64(math.Nextafter32(f, float32(math.Inf(-1))))
	} else {
		hi = float64(math.Nextafter32(f, float32(math.Inf(1))))
	}
	if rnd.float64()*(hi-lo) < v-lo {
		return float32(hi)
	}
	return float32(lo)
}

// ---- float16 ----

type float16Store struct{ v []uint16 }

// float16Max leaves headroom under the largest finite half (65504) so one
// stochastic round-up cannot reach infinity.
const float16Max = 60000

var float16Table [1 << 16]float32

func init() {
	for i := range float16Table {
		float16Table[i] = float16Decode(uint16(i))
	}
}

func float16Decode(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1F
	man := uint32(h & 0x3FF)
	switch {
	case exp == 0: // zero or subnormal: man * 2^-24
		f := float32(man) * (1.0 / (1 << 24))
		return math.Float32frombits(math.Float32bits(f) | sign)
	case exp == 0x1F:
		return math.Float32frombits(sign | 0x7F800000 | man<<13)
	}
	return math.Float32frombits(sign | (exp+112)<<23 | man<<13)
}

// float16Encode rounds v to a half: toward zero, then one step away from
// zero with probability equal to the fraction that was cut off. r is 64
// uniform random bits. It works from the float64 directly: going through
// float32 first would round-to-nearest away any increment below 2^-24 of
// the value, and a strategy sum would stall there.
func float16Encode(v float64, r uint64) uint16 {
	b := math.Float64bits(v)
	sign := uint16(b>>48) & 0x8000
	e := int(b>>52&0x7FF) - 1023
	sig := b&(1<<52-1) | 1<<52 // 53-bit significand, value = sig * 2^(e-52)
	if e > 15 {
		return sign | 0x7BFF // out of range: clamp to the largest finite half
	}
	// A normal half keeps 10 bits below the leading one; a subnormal half
	// counts units of 2^-24.
	shift := uint(42)
	var mag uint64
	if e >= -14 {
		mag = uint64(e+15)<<10 | sig>>42&0x3FF
	} else {
		shift = uint(28 - e)
		if shift >= 64 {
			return sign // below 2^-36 (this also catches zero)
		}
		mag = sig >> shift
	}
	if rem := sig & (1<<shift - 1); rem != 0 && r>>(64-shift) < rem {
		mag++ // carries into the exponent correctly: halfs are ordered by bit pattern
		if mag > 0x7BFF {
			mag = 0x7BFF
		}
	}
	return sign | uint16(mag)
}

func (s *float16Store) Kind() StoreKind { return Float16 }
func (s *float16Store) Load(off int64, dst []float32) {
	src := s.v[off : off+int64(len(dst))]
	for i, h := range src {
		dst[i] = float16Table[h]
	}
}

func (s *float16Store) Add(off int64, delta []float32, floor float32, rnd *rng) {
	raw := s.v[off : off+int64(len(delta))]
	var vals [MaxActions]float64
	for a, d := range delta {
		vals[a] = float64(float16Table[raw[a]]) + float64(d)
	}
	updated(&vals, len(delta), float64(max(floor, -float16Max)), float16Max)
	for a := range raw {
		raw[a] = float16Encode(vals[a], rnd.uint64())
	}
}

// A half's normal range runs from 2^-14 to 65504, about 2^30. Counting
// visits in units of 2^-14 puts the overflow point at 60000 * 2^14, about
// a billion visits of one infoset, instead of 60000.
func (s *float16Store) VisitUnit() float32 { return 1.0 / (1 << 14) }
func (s *float16Store) MinFloor() float32  { return -float16Max }
func (s *float16Store) Bytes() int64       { return int64(len(s.v)) * 2 }

// ---- bfloat16 ----

type bfloat16Store struct{ v []uint16 }

func bfloat16Decode(h uint16) float32 { return math.Float32frombits(uint32(h) << 16) }

// bfloat16Encode rounds v to the nearest-toward-zero value with a 7-bit
// mantissa, then steps one away from zero with probability equal to the
// fraction cut off. r is 64 uniform random bits. Like float16Encode it
// works from the float64 directly.
func bfloat16Encode(v float64, r uint64) uint16 {
	b := math.Float64bits(v)
	sign := uint16(b>>48) & 0x8000
	e := int(b>>52&0x7FF) - 1023
	if e < -126 {
		return sign // below float32's normal range (this also catches zero)
	}
	if e > 127 {
		return sign | 0x7F7F // clamp to the largest finite value
	}
	man := b & (1<<52 - 1)
	mag := uint64(e+127)<<7 | man>>45
	if rem := man & (1<<45 - 1); rem != 0 && r>>(64-45) < rem && mag < 0x7F7F {
		mag++
	}
	return sign | uint16(mag)
}

func (s *bfloat16Store) Kind() StoreKind { return BFloat16 }
func (s *bfloat16Store) Load(off int64, dst []float32) {
	src := s.v[off : off+int64(len(dst))]
	for i, h := range src {
		dst[i] = bfloat16Decode(h)
	}
}

func (s *bfloat16Store) Add(off int64, delta []float32, floor float32, rnd *rng) {
	raw := s.v[off : off+int64(len(delta))]
	var vals [MaxActions]float64
	for a, d := range delta {
		vals[a] = float64(bfloat16Decode(raw[a])) + float64(d)
	}
	updated(&vals, len(delta), float64(floor), math.MaxFloat32)
	for a := range raw {
		raw[a] = bfloat16Encode(vals[a], rnd.uint64())
	}
}

func (s *bfloat16Store) VisitUnit() float32 { return 1 }
func (s *bfloat16Store) MinFloor() float32  { return float32(math.Inf(-1)) }
func (s *bfloat16Store) Bytes() int64       { return int64(len(s.v)) * 2 }
