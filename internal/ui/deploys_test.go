package ui

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// deployRecordDoc is one decoded record for a block, so the fold is exercised
// without a database: the caller reads the pages, the block only assembles them.
func deployRecordDoc(key, service, result, deployedAt string) store.StoredDeployRecord {
	rec := store.DeployRecord{Schema: store.DeployRecordSchema, Service: service, Result: result}
	if deployedAt != "" {
		rec.DeployedAt = &deployedAt
	}
	return store.StoredDeployRecord{Key: key, Record: rec, Decoded: true, CreatedAt: time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)}
}

// TestDeployServiceBlockReadsOnePagePerPrefix pins what the block owes a
// caller that read one page per prefix: a key seen under two prefixes is one
// document, and the current record is chosen by deployed_at, not by key order.
func TestDeployServiceBlockReadsOnePagePerPrefix(t *testing.T) {
	svc := store.DeployService{Name: "hk", Repo: "mgh3326/handoffkeep", RecordPrefixes: []string{"deploy/hk/", "deploy/handoffkeep/"}, Active: true}
	shared := deployRecordDoc("deploy/hk/20260920T010000Z", "hk", "success", "2026-09-20T01:00:00Z")
	pages := map[string]store.DeployRecordPage{
		"deploy/hk/": {Records: []store.StoredDeployRecord{
			shared,
			deployRecordDoc("deploy/hk/20260919T010000Z", "hk", "failed", ""),
		}},
		"deploy/handoffkeep/": {Records: []store.StoredDeployRecord{
			shared, // the same document, reachable through the second prefix
			deployRecordDoc("deploy/handoffkeep/20260921T010000Z", "handoffkeep", "success", "2026-09-21T01:00:00Z"),
		}},
	}
	view := deployServiceBlock(svc, nil, pages, store.DeployLagServiceStatus{Service: "hk", LagCount: 4, Lagging: true, HasDeployed: true})
	if view.RecordCount != 3 {
		t.Fatalf("record_count = %d, want 3 documents", view.RecordCount)
	}
	if view.Current == nil || view.Current.RecordKey != "deploy/handoffkeep/20260921T010000Z" {
		t.Fatalf("current = %+v, want the newest success by deployed_at", view.Current)
	}
	if len(view.Recent) != 3 {
		t.Fatalf("recent holds %d records, want 3", len(view.Recent))
	}
	if view.LagCount != 4 || !view.Lagging {
		t.Fatalf("lag = %d lagging=%v, want the 4 the caller measured", view.LagCount, view.Lagging)
	}
}

// TestDeployServiceBlockMoreIsPartial: a prefix that holds more records than
// were read can hide its newest parseable record, so the block must report a
// partial scan instead of "no record".
func TestDeployServiceBlockMoreIsPartial(t *testing.T) {
	svc := store.DeployService{Name: "hk", Repo: "mgh3326/handoffkeep", RecordPrefixes: []string{"deploy/hk/"}, Active: true}
	pages := map[string]store.DeployRecordPage{
		"deploy/hk/": {
			More:    true,
			Records: []store.StoredDeployRecord{{Key: "deploy/hk/20260920T010000Z"}}, // undecodable body
		},
	}
	view := deployServiceBlock(svc, nil, pages, store.DeployLagServiceStatus{Service: "hk"})
	if !view.DocsCapped {
		t.Fatal("a prefix holding more records must mark the scan capped")
	}
	if view.InvalidCount != 1 || len(view.Invalid) != 1 {
		t.Fatalf("invalid = %+v, want the undecodable record listed", view.Invalid)
	}
	if view.State != DeployStatePartial {
		t.Fatalf("state = %q, want %q", view.State, DeployStatePartial)
	}
}

// TestDeployServiceBlockReadsLegacyPrefixAlias: a prefix may be legacy-named
// while its records carry the canonical service name, and the block must read
// those records rather than list them as invalid.
func TestDeployServiceBlockReadsLegacyPrefixAlias(t *testing.T) {
	svc := store.DeployService{Name: "panewire-hub", Repo: "mgh3326/panewire", RecordPrefixes: []string{"deploy/panewire/"}, Active: true}
	pages := map[string]store.DeployRecordPage{
		"deploy/panewire/": {Records: []store.StoredDeployRecord{
			deployRecordDoc("deploy/panewire/20260920T010000Z", "panewire", "success", "2026-09-20T01:00:00Z"),
		}},
	}
	view := deployServiceBlock(svc, nil, pages, store.DeployLagServiceStatus{Service: "panewire-hub", HasDeployed: true})
	if view.InvalidCount != 0 || view.Current == nil {
		t.Fatalf("legacy alias record was not read: %+v", view.Invalid)
	}
}
