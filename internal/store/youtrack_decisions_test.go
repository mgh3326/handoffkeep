package store

import (
	"testing"
)

// The decision mirror is one row per decision request. The backfill reads
// the rows it asks for and nothing else.
func TestYouTrackDecisionMirrorListReturnsAskedRows(t *testing.T) {
	st := schemaStore(t, "yt_decision_mirror")
	ctx := t.Context()
	if _, err := st.pool.Exec(ctx, `INSERT INTO youtrack_decision_mirrors(decision_id,issue_id,display_ref,stamp) VALUES(7,'2-7','DEC-7','s2'),(9,'2-9','DEC-9','s9')`); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListYouTrackDecisionMirrors(ctx, []int64{7, 8})
	if err != nil || len(list) != 1 || list[7].IssueID != "2-7" || list[7].Stamp != "s2" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	none, err := st.ListYouTrackDecisionMirrors(ctx, nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("empty ids: list=%+v err=%v", none, err)
	}
}
