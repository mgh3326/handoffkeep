package store

import (
	"testing"
	"time"
)

// Dialect-neutral ledger case (hk 5088): the desk ledger aggregate reads every
// section in one read-only snapshot. It runs on PostgreSQL and on MySQL, so a
// MySQL section that fails to read (or a snapshot that cannot open) fails here.
func init() {
	registerTaskAreaCase("ledger/aggregate", ledgerAreaCase)
}

func ledgerAreaCase(t *testing.T, s *Store) {
	ctx := t.Context()
	flowCaseTask(t, s, "implement")
	now := time.Now().UTC()
	led, err := s.Ledger(ctx, "ledger-case", now)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if led.Consumer != "ledger-case" || led.At.IsZero() {
		t.Fatalf("ledger header = consumer %q at %v", led.Consumer, led.At)
	}
}
