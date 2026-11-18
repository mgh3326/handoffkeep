package store

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

// TestDriftMergedWithoutDeployReadsAPageInOneStatement: a page's PRs are read
// together, so the class costs the same statements at 40 PRs as at 1. The
// page reads its candidates, then its deploy records: two statements. Before
// hk 5058 it ran one deploy-record scan per PR (114 per request in production).
func TestDriftMergedWithoutDeployReadsAPageInOneStatement(t *testing.T) {
	s, pool := searchTestStore(t)
	for i := range 40 {
		x := flowTask(t, s, pool, "implement", StateMerged, false)
		setRefsPR(t, s, x.ID, testPR(7000+i))
	}
	deployed := flowTask(t, s, pool, "implement", StateMerged, false)
	setRefsPR(t, s, deployed.ID, testPR(7100))
	putDeploy(t, s, "deploy/svc/20261105T070000Z", "svc", "success", []string{testPR(7100)})

	ctx, count := telemetry.WithQueryCount(t.Context())
	tx, err := s.beginReportRead(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	before := count.Count()
	c, err := driftMergedWithoutDeploy(ctx, tx, time.Now().UTC().Add(72*time.Hour), DriftDefaultMergedDays)
	if err != nil {
		t.Fatal(err)
	}
	if n := count.Count() - before; n > 2 {
		t.Fatalf("%d statements for a page of 40 PRs, want at most 2 (candidates, then deploy records)", n)
	}
	if ids := driftIDs(c); len(ids) != 40 || ids[deployed.ID] {
		t.Fatalf("merged_without_deploy lists %d tasks (deployed listed: %v), want the 40 undeployed", len(ids), ids[deployed.ID])
	}
}

// TestDeployRecordsForPRsBindEveryPlaceholder: each batched deploy-record
// statement binds exactly the arguments it is given, for both dialects, so a
// page of any size runs with matching placeholders.
func TestDeployRecordsForPRsBindEveryPlaceholder(t *testing.T) {
	prs := []string{testPR(1), testPR(2), testPR(3)}
	pgSQL, pgArgs := deployRecordsForPRsPG("deploy/", "deploy0", prs)
	indexes := map[int]bool{}
	for _, m := range regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(pgSQL, -1) {
		n, _ := strconv.Atoi(m[1])
		indexes[n] = true
	}
	if len(indexes) != len(pgArgs) {
		t.Fatalf("pg placeholders $1..$%d used %d distinct, args %d", len(pgArgs), len(indexes), len(pgArgs))
	}
	for i := 1; i <= len(pgArgs); i++ {
		if !indexes[i] {
			t.Fatalf("pg placeholder $%d unused in %q", i, pgSQL)
		}
	}
	mySQL, myArgs := deployRecordsForPRsMySQL("deploy/", "deploy0", prs)
	if got := strings.Count(mySQL, "?"); got != len(myArgs) {
		t.Fatalf("mysql has %d placeholders for %d args", got, len(myArgs))
	}
}
