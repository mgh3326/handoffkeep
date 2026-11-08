package store

import (
	"testing"
)

// The decision mirror is one row per decision request. A put replaces the
// row, and a list returns only the rows asked for.
func TestYouTrackDecisionMirrorPutGetList(t *testing.T) {
	st := schemaStore(t, "yt_decision_mirror")
	ctx := t.Context()
	if err := st.PutYouTrackDecisionMirror(ctx, YouTrackDecisionMirror{DecisionID: 7, IssueID: "2-7", DisplayRef: "DEC-7", Stamp: "s1"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetYouTrackDecisionMirror(ctx, 7)
	if err != nil || !ok || got.IssueID != "2-7" || got.Stamp != "s1" {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
	if err := st.PutYouTrackDecisionMirror(ctx, YouTrackDecisionMirror{DecisionID: 7, IssueID: "2-7", DisplayRef: "DEC-7", Stamp: "s2"}); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListYouTrackDecisionMirrors(ctx, []int64{7, 8})
	if err != nil || len(list) != 1 || list[7].Stamp != "s2" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if _, ok, _ := st.GetYouTrackDecisionMirror(ctx, 8); ok {
		t.Fatal("an unmapped decision was found")
	}
	if err := st.PutYouTrackDecisionMirror(ctx, YouTrackDecisionMirror{DecisionID: 9}); err == nil {
		t.Fatal("a mirror without an issue was accepted")
	}
}
