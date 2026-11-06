package store

import (
	"errors"
	"fmt"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"strings"
	"testing"
	"time"
)

func TestPutStandingOrderSupersede(t *testing.T) {
	s, err := Open(t.Context(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	name := fmt.Sprintf("standing-%d", time.Now().UnixNano())
	doc := "decision/" + name
	if _, _, err = s.PutDocument(t.Context(), Document{Key: doc, Kind: "note", Body: "fake", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	in := StandingOrderInput{ScopeKind: "lane", Scope: name, TopicKey: "deploy", Statement: "A", SourceDoc: doc, DecidedBy: "operator", DecidedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	a, created, err := s.PutStandingOrder(t.Context(), in, "test")
	if err != nil || !created {
		t.Fatalf("A: %v %v", created, err)
	}
	in.Statement = "B"
	in.DecidedAt = in.DecidedAt.Add(time.Minute)
	b, created, err := s.PutStandingOrder(t.Context(), in, "test")
	if err != nil || !created || b.SupersedesID == nil || *b.SupersedesID != a.ID {
		t.Fatalf("B: %+v %v %v", b, created, err)
	}
	var superseded *time.Time
	if err = s.pool.QueryRow(t.Context(), `SELECT superseded_at FROM standing_orders WHERE id=$1`, a.ID).Scan(&superseded); err != nil || superseded == nil {
		t.Fatalf("superseded: %v %v", superseded, err)
	}
	replay, created, err := s.PutStandingOrder(t.Context(), in, "test")
	if err != nil || created || replay.ID != b.ID {
		t.Fatalf("replay: %+v %v %v", replay, created, err)
	}
	bad := in
	bad.Statement = "C"
	for _, at := range []time.Time{in.DecidedAt, in.DecidedAt.Add(-time.Minute)} {
		bad.DecidedAt = at
		if _, _, err = s.PutStandingOrder(t.Context(), bad, "test"); !errors.Is(err, ErrStandingNotNewer) {
			t.Fatalf("not newer: %v", err)
		}
	}
	for _, v := range []StandingOrderInput{func() StandingOrderInput { x := in; x.SourceDoc = "decision/missing-" + name; return x }(), func() StandingOrderInput { x := in; x.SourceDoc = "other/" + name; return x }(), func() StandingOrderInput { x := in; x.TopicKey = "Bad Key"; return x }(), func() StandingOrderInput { x := in; x.Statement = strings.Repeat("x", 2001); return x }(), func() StandingOrderInput { x := in; x.Statement = "gh" + "p_" + strings.Repeat("a", 36); return x }()} {
		if _, _, err = s.PutStandingOrder(t.Context(), v, "test"); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	other := in
	other.ScopeKind = "project"
	other.Scope = name
	if _, ok, e := s.PutStandingOrder(t.Context(), other, "test"); e != nil || !ok {
		t.Fatal(e)
	}
	other = in
	other.TopicKey = "other"
	if _, ok, e := s.PutStandingOrder(t.Context(), other, "test"); e != nil || !ok {
		t.Fatal(e)
	}
	orders, truncated, err := s.ListStandingOrders(t.Context(), name, name)
	if err != nil || truncated || len(orders) != 3 {
		t.Fatalf("list: %+v %v %v", orders, truncated, err)
	}
	var count int
	if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM standing_orders WHERE scope=$1`, name).Scan(&count); err != nil || count != 4 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestStandingOrdersMigrationAfterDecisionDeny(t *testing.T) {
	s := schemaStore(t, "standing_migration")
	var version int
	var tableExists, denyColumnExists, sessionColumnExists, deliveriesExist, verificationsExist, dotsExist, checksExist bool
	err := s.pool.QueryRow(t.Context(), `SELECT (SELECT max(version) FROM schema_version), to_regclass('standing_orders') IS NOT NULL, EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='decision_answer_attempts' AND column_name='denies_attempt_id'), EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='decision_answer_attempts' AND column_name='operator_quote'), to_regclass('decision_deliveries') IS NOT NULL, to_regclass('verifications') IS NOT NULL, to_regclass('dots_results') IS NOT NULL, to_regclass('change_checks') IS NOT NULL`).Scan(&version, &tableExists, &denyColumnExists, &sessionColumnExists, &deliveriesExist, &verificationsExist, &dotsExist, &checksExist)
	if err != nil || version != SchemaVersion || !tableExists || !denyColumnExists || !sessionColumnExists || !deliveriesExist || !verificationsExist || !dotsExist || !checksExist {
		t.Fatalf("version=%d table=%v deny_column=%v session_column=%v deliveries=%v verifications=%v dots=%v checks=%v err=%v", version, tableExists, denyColumnExists, sessionColumnExists, deliveriesExist, verificationsExist, dotsExist, checksExist, err)
	}
}
