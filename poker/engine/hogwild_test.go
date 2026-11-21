//go:build !race

package engine

import "testing"

// Multi-worker training shares the arrays without locks on purpose (see
// TrainConfig.Workers), which the race detector rightly reports; hence the
// build tag. What must hold is that it still converges.
func TestLeducConvergesWithFourWorkers(t *testing.T) {
	g := NewLeduc()
	cfg := DefaultTrainConfig()
	cfg.Workers = 4
	s := trained(t, g, cfg, 4_000_000)
	expl, _ := Exploitability(g, s.AverageStrategy(), 1)
	t.Logf("leduc, 4 workers: exploitability %.5f", expl)
	if expl > 0.02 {
		t.Errorf("exploitability %.5f, want < 0.02", expl)
	}
}
