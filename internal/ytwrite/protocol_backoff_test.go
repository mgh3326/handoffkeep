package ytwrite

import (
	"testing"
	"time"
)

func TestReconcileBackoffAfterFiftySearches(t *testing.T) {
	p := &Protocol{Config: Config{ReconcileBase: 5 * time.Second, ReconcileMax: 5 * time.Minute}}
	for _, tc := range []struct {
		searches int
		want     time.Duration
	}{{0, 5 * time.Second}, {49, 5 * time.Minute}, {50, time.Hour}, {73, time.Hour}, {74, 24 * time.Hour}} {
		if got := p.reconcileDelay(tc.searches); got != tc.want {
			t.Errorf("after %d searches delay=%s want %s", tc.searches, got, tc.want)
		}
	}
}
