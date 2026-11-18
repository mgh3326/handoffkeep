package store

import (
	"sort"
	"strings"
	"sync"
	"testing"
)

// The collation migration rebuilds every trigger that is live when it runs,
// so a trigger created before the connection collation was pinned cannot
// keep its stale creation collation (hk 5169). The quota_history_immutable_*
// triggers are not live: 20261117-mysql-bulk-guards dropped them.
func TestMySQLCollationRecreationCoversTriggers(t *testing.T) {
	const collationID = "20261118-mysql-collation"
	// The collation migration names other migrations in after: an unknown ID
	// fails every MySQL open at startup, so the full registry must order.
	ordered, err := orderMySQLMigrations(mysqlMigrations)
	if err != nil {
		t.Fatalf("full MySQL registry does not order: %v", err)
	}
	liveMap, _, err := mysqlLiveTriggers(mysqlMigrations)
	if err != nil {
		t.Fatalf("derive live triggers: %v", err)
	}
	var live []string
	for name := range liveMap {
		live = append(live, name)
	}
	var myDrops, myCreates []string
	for _, m := range ordered {
		if m.id != collationID {
			continue
		}
		for _, step := range m.steps {
			if name, ok := mysqlCollationDropName(step); ok {
				myDrops = append(myDrops, name)
				continue
			}
			rest, ok := strings.CutPrefix(step, "CREATE TRIGGER ")
			if !ok {
				continue
			}
			if strings.HasPrefix(rest, "IF NOT EXISTS ") {
				t.Errorf("collation migration must rebuild, not keep: %.60s...", step)
				continue
			}
			name, _, _ := strings.Cut(rest, " ")
			myCreates = append(myCreates, name)
			if wantBody, exists := liveMap[name]; exists && step != wantBody {
				t.Errorf("collation migration recreated trigger %s has body:\n%s\nwant:\n%s", name, step, wantBody)
			}
		}
	}
	sort.Strings(live)
	sort.Strings(myDrops)
	sort.Strings(myCreates)
	if strings.Join(myDrops, ",") != strings.Join(live, ",") {
		t.Errorf("collation migration drops %v, want live triggers %v", myDrops, live)
	}
	if strings.Join(myCreates, ",") != strings.Join(live, ",") {
		t.Errorf("collation migration creates %v, want live triggers %v", myCreates, live)
	}
}

// The collation migration rebuilds both JSON CHECK constraints from the
// shared predicate consts (hk 5169).
func TestMySQLCollationRecreationCoversChecks(t *testing.T) {
	const collationID = "20261118-mysql-collation"
	ordered, err := orderMySQLMigrations(mysqlMigrations)
	if err != nil {
		t.Fatalf("order MySQL migrations: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range ordered {
		if m.id != collationID {
			continue
		}
		for _, step := range m.steps {
			kind, table, object, stmt, ok := parseMySQLGuard(step)
			if !ok {
				continue
			}
			if table == "tasks" && object == "tasks_closure_check" {
				seen[kind+":tasks_closure_check"] = true
				if kind == "constraint" && !strings.Contains(stmt, mysqlTasksClosureCheckBody) {
					t.Errorf("collation migration re-adds tasks_closure_check from another text")
				}
			}
			if table == "jobs" && object == "jobs_skills_check" {
				seen[kind+":jobs_skills_check"] = true
				if kind == "constraint" && !strings.Contains(stmt, mysqlJobsSkillsCheckBody) {
					t.Errorf("collation migration re-adds jobs_skills_check from another text")
				}
			}
		}
	}
	for _, want := range []string{"has_constraint:tasks_closure_check", "constraint:tasks_closure_check", "has_constraint:jobs_skills_check", "constraint:jobs_skills_check"} {
		if !seen[want] {
			t.Errorf("collation migration is missing %s", want)
		}
	}
}

// orderMySQLMigrations must return a caller-owned ordered plan and must not mutate
// the input slice or the global mysqlMigrations registry (hk 5192).
func TestOrderMySQLMigrationsLeavesSharedRegistryUntouched(t *testing.T) {
	clone := make([]mysqlMigration, len(mysqlMigrations))
	copy(clone, mysqlMigrations)

	collationIdx := -1
	for i, m := range clone {
		if m.id == mysqlCollationMigrationID {
			collationIdx = i
			break
		}
	}
	if collationIdx == -1 {
		t.Fatalf("missing %s in mysqlMigrations", mysqlCollationMigrationID)
	}
	beforeSteps := clone[collationIdx].steps
	beforeAfter := clone[collationIdx].after

	ordered, err := orderMySQLMigrations(clone)
	if err != nil {
		t.Fatalf("orderMySQLMigrations: %v", err)
	}

	// 1. clone must not have had its element mutated in place.
	if len(clone[collationIdx].steps) != len(beforeSteps) || (len(beforeSteps) > 0 && &clone[collationIdx].steps[0] != &beforeSteps[0]) {
		t.Fatalf("orderMySQLMigrations mutated input slice in place: steps count %d != %d", len(clone[collationIdx].steps), len(beforeSteps))
	}
	if len(clone[collationIdx].after) != len(beforeAfter) || (len(beforeAfter) > 0 && &clone[collationIdx].after[0] != &beforeAfter[0]) {
		t.Fatalf("orderMySQLMigrations mutated input slice in place: after count %d != %d", len(clone[collationIdx].after), len(beforeAfter))
	}

	// 2. global mysqlMigrations must not have had its element mutated in place either.
	for _, m := range mysqlMigrations {
		if m.id == mysqlCollationMigrationID {
			if len(m.steps) != len(beforeSteps) {
				t.Fatalf("orderMySQLMigrations wrote back to global mysqlMigrations: steps=%d, want %d", len(m.steps), len(beforeSteps))
			}
			if len(m.after) != len(beforeAfter) {
				t.Fatalf("orderMySQLMigrations wrote back to global mysqlMigrations: after=%d, want %d", len(m.after), len(beforeAfter))
			}
			break
		}
	}

	// 3. returned ordered plan must have the resolved, expanded steps.
	var found bool
	for _, m := range ordered {
		if m.id == mysqlCollationMigrationID {
			found = true
			if len(m.steps) <= len(beforeSteps) {
				t.Fatalf("ordered plan steps not expanded: got %d <= %d", len(m.steps), len(beforeSteps))
			}
		}
	}
	if !found {
		t.Fatal("ordered plan missing collation migration")
	}
}

func TestOrderMySQLMigrationsConcurrent(t *testing.T) {
	const routines = 16
	var wg sync.WaitGroup
	errCh := make(chan error, routines)
	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := orderMySQLMigrations(mysqlMigrations)
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent orderMySQLMigrations: %v", err)
	}
}
