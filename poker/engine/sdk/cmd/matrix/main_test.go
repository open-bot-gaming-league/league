package main

import (
	"strings"
	"testing"
)

func TestContenders(t *testing.T) {
	cs, err := contenders(" dove, shark ,", nil)
	if err != nil || len(cs) != 2 || cs[0].Name != "Dove" || cs[1].Name != "Shark" {
		t.Fatalf("%+v, %v", cs, err)
	}
	if b, err := cs[1].New(); err != nil || b == nil {
		t.Fatalf("Shark: %v, %v", b, err)
	}
	for _, tc := range []struct {
		list  string
		house []string
		want  string
	}{
		{"dove,station", nil, `no bot "station"`},
		{"dove", nil, "at least two bots"},
		{"dove,clam", []string{"turtle"}, "want name=buckets,blueprint"},
		{"dove,clam", []string{"turtle=a.bin,missing.bp"}, "-house turtle"},
	} {
		if _, err := contenders(tc.list, tc.house); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q %v: %v, want %q", tc.list, tc.house, err, tc.want)
		}
	}
}
