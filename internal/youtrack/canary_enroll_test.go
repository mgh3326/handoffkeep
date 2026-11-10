package youtrack

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type issueReader struct {
	issues map[string]Issue
	err    error
	calls  int
}

func (r *issueReader) GetIssue(_ context.Context, id string) (Issue, error) {
	r.calls++
	if r.err != nil {
		return Issue{}, r.err
	}
	issue, ok := r.issues[id]
	if !ok {
		return Issue{}, fmt.Errorf("%w: issue %s", ErrNotFound, id)
	}
	return issue, nil
}

const canaryTenant = "canary.youtrack.example"

func issueFor(readable, summary string) Issue {
	return Issue{ID: "2-77", IDReadable: readable, Summary: summary, Project: &ProjectRef{ID: "0-2", ShortName: "HK"}}
}

func candidate(task int64) store.YTCanaryCandidate {
	return store.YTCanaryCandidate{TaskID: task, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-2"}
}

func TestCommentMappingChecksWholeTaskNumber(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		summary string
		ok      bool
	}{
		{"#37 카나리", true},
		{"#37", true},
		{"#37\tx", true},
		{"#3739 another task", false},
		{"#037 zero-padded", false},
		{"#3 shorter", false},
		{"37 no hash", false},
		{"x #37", false},
	} {
		r := &issueReader{issues: map[string]Issue{"HK-37": issueFor("HK-37", tc.summary)}}
		got, err := VerifyCanary(ctx, r, canaryTenant, candidate(37))
		if tc.ok != (err == nil) {
			t.Errorf("summary %q: err = %v, want ok=%v", tc.summary, err, tc.ok)
			continue
		}
		if !tc.ok && !errors.Is(err, ErrCanaryRefused) {
			t.Errorf("summary %q: err = %v, want ErrCanaryRefused", tc.summary, err)
		}
		if tc.ok && (got.TaskID != 37 || got.IssueID != "2-77" || got.IssueReadable != "HK-37" || got.Tenant != canaryTenant || got.ProjectID != "0-2") {
			t.Errorf("summary %q: binding %+v", tc.summary, got)
		}
	}
}

func TestCommentMappingRequiresExplicitProject(t *testing.T) {
	ctx := context.Background()
	r := &issueReader{issues: map[string]Issue{"OTHER-7": issueFor("OTHER-7", "#7 x")}}
	for name, c := range map[string]store.YTCanaryCandidate{
		"no project":                 {TaskID: 7},
		"unmapped project":           {TaskID: 7, Project: "scratch"},
		"mapping with no short name": {TaskID: 7, Project: "scratch", ProjectID: "0-2"},
	} {
		if _, err := VerifyCanary(ctx, r, canaryTenant, c); !errors.Is(err, ErrCanaryRefused) {
			t.Errorf("%s: err = %v, want ErrCanaryRefused", name, err)
		}
	}
	if r.calls != 0 {
		t.Fatalf("an unmapped task reached YouTrack (%d reads)", r.calls)
	}
}

// Every mapping hk keeps must name the same issue, or the enrollment is
// refused before any comment could be sent there.
func TestCommentMappingConflictBeforeSend(t *testing.T) {
	ctx := context.Background()
	good := func() *issueReader {
		return &issueReader{issues: map[string]Issue{"HK-7": issueFor("HK-7", "#7 x")}}
	}
	refused := map[string]struct {
		c store.YTCanaryCandidate
		r *issueReader
	}{
		"old mirror names another issue":  {store.YTCanaryCandidate{TaskID: 7, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-2", MirrorIssueID: "2-55", MirrorTenant: canaryTenant}, good()},
		"old mirror is on another tenant": {store.YTCanaryCandidate{TaskID: 7, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-2", MirrorIssueID: "2-77", MirrorTenant: "other.youtrack.cloud"}, good()},
		"entity map names another issue":  {store.YTCanaryCandidate{TaskID: 7, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-2", MappedIssueID: "2-99"}, good()},
		"issue is in another project":     {store.YTCanaryCandidate{TaskID: 7, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-9"}, good()},
		"moved issue answers another id":  {candidate(7), &issueReader{issues: map[string]Issue{"HK-7": issueFor("PW-7", "#7 x")}}},
		"no such issue":                   {candidate(7), &issueReader{}},
	}
	for name, tc := range refused {
		if _, err := VerifyCanary(ctx, tc.r, canaryTenant, tc.c); !errors.Is(err, ErrCanaryRefused) {
			t.Errorf("%s: err = %v, want ErrCanaryRefused", name, err)
		}
	}
	agree := store.YTCanaryCandidate{TaskID: 7, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-2", MirrorIssueID: "2-77", MirrorTenant: canaryTenant, MappedIssueID: "2-77"}
	if got, err := VerifyCanary(ctx, good(), canaryTenant, agree); err != nil || got.IssueID != "2-77" {
		t.Fatalf("agreeing mappings: %+v %v", got, err)
	}
	// A read that failed is an error, but not a refusal: the operator retries.
	_, err := VerifyCanary(ctx, &issueReader{err: &APIError{Status: 503}}, canaryTenant, candidate(7))
	if err == nil || errors.Is(err, ErrCanaryRefused) {
		t.Fatalf("failed read: %v", err)
	}
}

// Comment mode on (hk 3810) binds with the canary's checks and sorts each
// refusal: no_issue (no project, an unmapped project, a project with no
// YouTrack id, no such issue) or a disagreement, which is everything else.
func TestVerifyBindingClassifiesRefusals(t *testing.T) {
	ctx := context.Background()
	good := func() *issueReader {
		return &issueReader{issues: map[string]Issue{"HK-7": issueFor("HK-7", "#7 x")}}
	}
	noIssue := map[string]struct {
		c       store.YTCanaryCandidate
		r       *issueReader
		offline bool
	}{
		"no project":                  {store.YTCanaryCandidate{TaskID: 7}, good(), true},
		"unmapped project":            {store.YTCanaryCandidate{TaskID: 7, Project: "scratch"}, good(), true},
		"project with no YouTrack id": {store.YTCanaryCandidate{TaskID: 7, Project: "handoffkeep", ShortName: "HK"}, good(), true},
		"no such issue":               {candidate(7), &issueReader{}, false},
	}
	for name, tc := range noIssue {
		_, err := VerifyBinding(ctx, tc.r, canaryTenant, tc.c)
		if !errors.Is(err, ErrNoIssue) || !errors.Is(err, ErrCanaryRefused) {
			t.Errorf("%s: err = %v, want ErrNoIssue", name, err)
		}
		if tc.offline && tc.r.calls != 0 {
			t.Errorf("%s: %d reads; this needs no YouTrack call", name, tc.r.calls)
		}
	}
	disagree := map[string]struct {
		c store.YTCanaryCandidate
		r *issueReader
	}{
		"old mirror names another issue": {store.YTCanaryCandidate{TaskID: 7, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-2", MirrorIssueID: "2-55", MirrorTenant: canaryTenant}, good()},
		"entity map names another issue": {store.YTCanaryCandidate{TaskID: 7, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-2", MappedIssueID: "2-99"}, good()},
		"issue is in another project":    {store.YTCanaryCandidate{TaskID: 7, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-9"}, good()},
		"moved issue answers another id": {candidate(7), &issueReader{issues: map[string]Issue{"HK-7": issueFor("PW-7", "#7 x")}}},
		"summary names another task":     {candidate(7), &issueReader{issues: map[string]Issue{"HK-7": issueFor("HK-7", "#8 x")}}},
	}
	for name, tc := range disagree {
		_, err := VerifyBinding(ctx, tc.r, canaryTenant, tc.c)
		if !errors.Is(err, ErrCanaryRefused) || errors.Is(err, ErrNoIssue) {
			t.Errorf("%s: err = %v, want a refusal that is not ErrNoIssue", name, err)
		}
	}
	_, err := VerifyBinding(ctx, &issueReader{err: &APIError{Status: 503}}, canaryTenant, candidate(7))
	if err == nil || errors.Is(err, ErrCanaryRefused) {
		t.Fatalf("failed read: %v, want an error that is not a refusal", err)
	}
	got, err := VerifyBinding(ctx, good(), canaryTenant, candidate(7))
	want := store.YTTaskBinding{TaskID: 7, Tenant: canaryTenant, IssueID: "2-77", IssueReadable: "HK-7", ProjectID: "0-2"}
	if err != nil || got != want {
		t.Fatalf("binding %+v %v, want %+v", got, err, want)
	}
}
