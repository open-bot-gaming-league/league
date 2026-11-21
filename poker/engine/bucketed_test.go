package engine

import (
	"math"
	"testing"
)

// The sampled value must agree with the exact one to within its own
// stated error.
func TestSampledValueMatchesExactValue(t *testing.T) {
	g := NewLeduc()
	strats := [2]Strategy{hashStrategy{}, UniformStrategy{}}
	exact := ProfileValue(g, strats, 1)
	mean, se := SampledValue(g, strats, 400_000, 5, 3)
	t.Logf("leduc: exact %.5f, sampled %.5f +- %.5f", exact, mean, se)
	if se <= 0 || se > 0.01 || math.Abs(mean-exact) > 4*se {
		t.Errorf("sampled value %.5f +- %.5f, exact %.5f", mean, se, exact)
	}
}

// On Leduc the buckets lose nothing, so a responder trained inside the
// abstraction is up against the same problem as the exact best response
// and must close on its value from below: never above it, and within a
// few hundredths after 2M iterations. The opponents are a half-trained
// strategy and uniform play.
func TestBucketedResponseApproachesExactBestResponse(t *testing.T) {
	g := NewLeduc()
	halfTrained := trained(t, g, DefaultTrainConfig(), 100_000).AverageStrategy()
	for name, strat := range map[string]Strategy{"half-trained": halfTrained, "uniform": UniformStrategy{}} {
		for seat := 0; seat < 2; seat++ {
			exact := BestResponseValue(g, strat, seat, 1)
			resp, err := BucketedResponse(g, strat, seat, 2_000_000, DefaultTrainConfig())
			if err != nil {
				t.Fatal(err)
			}
			strats := [2]Strategy{strat, strat}
			strats[seat] = resp
			got := ProfileValue(g, strats, 1)
			if seat == 1 {
				got = -got
			}
			t.Logf("%s, seat %d: exact best response %.5f, trained bucketed response %.5f", name, seat, exact, got)
			if got > exact+1e-9 || got < exact-0.03 {
				t.Errorf("%s, seat %d: trained response wins %.5f, exact best response %.5f", name, seat, got, exact)
			}
		}
	}
}
