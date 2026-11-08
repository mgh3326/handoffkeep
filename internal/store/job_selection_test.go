package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestJobSelectionMigration(t *testing.T) {
	s := schemaStore(t, "job_selection_migration")
	var nullable, dataType string
	if err := s.pool.QueryRow(t.Context(), `SELECT is_nullable, data_type FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='jobs' AND column_name='selection'`).Scan(&nullable, &dataType); err != nil || nullable != "YES" || dataType != "jsonb" {
		t.Fatalf("selection column %q %q: %v", nullable, dataType, err)
	}
}

func TestJobSelectionRoundTrip(t *testing.T) {
	s := schemaStore(t, "job_selection_roundtrip")
	j := jobContextClaim(t, s, "job-selection", nil)
	if j.Selection != nil {
		t.Fatalf("claim carries a selection: %s", j.Selection)
	}
	if got := j.Explain(); got.Why != NoSelectionText {
		t.Fatalf("%+v", got)
	}
	j = jobContextUpdate(t, s, j.JobID, JobUpdate{State: strptr("spawned"), Selection: json.RawMessage(panewireSample)})
	var got map[string]any
	read, found, err := s.GetJob(t.Context(), j.JobID)
	if err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	if err := json.Unmarshal(read.Selection, &got); err != nil {
		t.Fatal(err)
	}
	// Unknown keys survive.
	if got["future_key"] == nil || got["chosen"].(map[string]any)["model_id"] != "gpt-6.1-terra" {
		t.Fatalf("selection lost keys: %s", read.Selection)
	}
	if !strings.Contains(read.Explain().Why, "작업 등급 B") {
		t.Fatalf("%+v", read.Explain())
	}
	// A later update that omits selection leaves it alone.
	j = jobContextUpdate(t, s, j.JobID, JobUpdate{Label: strptr("x")})
	if len(j.Selection) == 0 {
		t.Fatal("selection cleared by an unrelated update")
	}
}

func TestJobSelectionSizeCap(t *testing.T) {
	s := schemaStore(t, "job_selection_cap")
	j := jobContextClaim(t, s, "job-selection-cap", nil)
	over := json.RawMessage(`{"pad":"` + strings.Repeat("x", JobSelectionMaxBytes) + `"}`)
	if _, err := s.UpdateJob(t.Context(), j.JobID, JobUpdate{Selection: over}); err == nil {
		t.Fatal("oversize selection accepted")
	}
	for _, raw := range []string{`null`, `[1]`, `"x"`} {
		if _, err := s.UpdateJob(t.Context(), j.JobID, JobUpdate{Selection: json.RawMessage(raw)}); err == nil {
			t.Fatalf("%s accepted", raw)
		}
	}
	read, _, _ := s.GetJob(t.Context(), j.JobID)
	if read.Selection != nil {
		t.Fatalf("rejected write stored %s", read.Selection)
	}
}

func TestJobSelectionTerminalRecordIsFrozen(t *testing.T) {
	s := schemaStore(t, "job_selection_terminal")
	j := jobContextClaim(t, s, "job-selection-terminal", nil)
	jobContextUpdate(t, s, j.JobID, JobUpdate{State: strptr("lost")})
	_, err := s.UpdateJob(t.Context(), j.JobID, JobUpdate{Selection: json.RawMessage(`{}`)})
	if !errors.Is(err, ErrJobStateConflict) {
		t.Fatalf("terminal selection write: %v", err)
	}
}
