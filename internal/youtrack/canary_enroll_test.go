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
		_, err := VerifyBinding(ctx, tc.r, canaryTenant, "", tc.c)
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
		_, err := VerifyBinding(ctx, tc.r, canaryTenant, "", tc.c)
		if !errors.Is(err, ErrCanaryRefused) || errors.Is(err, ErrNoIssue) {
			t.Errorf("%s: err = %v, want a refusal that is not ErrNoIssue", name, err)
		}
	}
	_, err := VerifyBinding(ctx, &issueReader{err: &APIError{Status: 503}}, canaryTenant, "", candidate(7))
	if err == nil || errors.Is(err, ErrCanaryRefused) {
		t.Fatalf("failed read: %v, want an error that is not a refusal", err)
	}
	got, err := VerifyBinding(ctx, good(), canaryTenant, "", candidate(7))
	want := store.YTTaskBinding{TaskID: 7, Tenant: canaryTenant, IssueID: "2-77", IssueReadable: "HK-7", ProjectID: "0-2"}
	if err != nil || got != want {
		t.Fatalf("binding %+v %v, want %+v", got, err, want)
	}
}

type projectLister struct {
	projects []AdminProject
	err      error
}

func (l projectLister) ListAdminProjects(context.Context) ([]AdminProject, error) {
	return l.projects, l.err
}

// hk 3897: a task with no hk project binds to the import's bucket issue
// HK-<id>, in the one project whose short name is HK, when its summary names
// the task; the run refuses when there is no such project or more than one.
func TestVerifyBindingNoProjectUsesBucket(t *testing.T) {
	ctx := context.Background()
	inBucket := func(readable, summary string) Issue {
		return Issue{ID: "5-33", IDReadable: readable, Summary: summary, Project: &ProjectRef{ID: "0-1", ShortName: "HK"}}
	}
	bucket, err := ResolveBucketProject(ctx, projectLister{projects: []AdminProject{{ID: "0-2", ShortName: "PW"}, {ID: "0-1", ShortName: "HK"}, {ID: "0-3", ShortName: "HKX"}}})
	if err != nil || bucket != "0-1" {
		t.Fatalf("bucket %q %v, want 0-1", bucket, err)
	}
	noProject := store.YTCanaryCandidate{TaskID: 33}

	r := &issueReader{issues: map[string]Issue{"HK-33": inBucket("HK-33", "#33 closed, no project")}}
	got, err := VerifyBinding(ctx, r, canaryTenant, bucket, noProject)
	want := store.YTTaskBinding{TaskID: 33, Tenant: canaryTenant, IssueID: "5-33", IssueReadable: "HK-33", ProjectID: "0-1"}
	if err != nil || got != want || r.calls != 1 {
		t.Fatalf("no-project task: %+v %v (%d reads), want %+v", got, err, r.calls, want)
	}

	refused := map[string]struct {
		c     store.YTCanaryCandidate
		issue Issue
	}{
		"summary names another task": {noProject, inBucket("HK-33", "#34 other")},
		"issue moved out of HK":      {noProject, inBucket("PW-33", "#33 moved")},
		"issue in another HK project": {noProject, Issue{ID: "5-33", IDReadable: "HK-33", Summary: "#33 x",
			Project: &ProjectRef{ID: "0-9", ShortName: "HK"}}},
		"map row names another issue": {store.YTCanaryCandidate{TaskID: 33, MappedIssueID: "5-99"}, inBucket("HK-33", "#33 x")},
	}
	for name, tc := range refused {
		_, err := VerifyBinding(ctx, &issueReader{issues: map[string]Issue{"HK-33": tc.issue}}, canaryTenant, bucket, tc.c)
		if !errors.Is(err, ErrCanaryRefused) || errors.Is(err, ErrNoIssue) {
			t.Errorf("%s: err = %v, want a refusal that is not ErrNoIssue", name, err)
		}
	}
	if _, err := VerifyBinding(ctx, &issueReader{}, canaryTenant, bucket, noProject); !errors.Is(err, ErrNoIssue) {
		t.Errorf("no HK-33: err = %v, want ErrNoIssue", err)
	}
	// Without a bucket a task with no project has no issue, as before, and
	// no read is made.
	r = &issueReader{issues: map[string]Issue{"HK-33": inBucket("HK-33", "#33 x")}}
	if _, err := VerifyBinding(ctx, r, canaryTenant, "", noProject); !errors.Is(err, ErrNoIssue) || r.calls != 0 {
		t.Errorf("no bucket: err = %v after %d reads, want ErrNoIssue and no read", err, r.calls)
	}
	// A task with a project never falls back to the bucket.
	r = &issueReader{issues: map[string]Issue{"HK-33": inBucket("HK-33", "#33 x")}}
	if _, err := VerifyBinding(ctx, r, canaryTenant, bucket, store.YTCanaryCandidate{TaskID: 33, Project: "scratch"}); !errors.Is(err, ErrNoIssue) || r.calls != 0 {
		t.Errorf("unmapped project: err = %v after %d reads, want ErrNoIssue and no read", err, r.calls)
	}
	// The canary keeps refusing a task with no project.
	if _, err := VerifyCanary(ctx, &issueReader{issues: map[string]Issue{"HK-33": inBucket("HK-33", "#33 x")}}, canaryTenant, noProject); !errors.Is(err, ErrNoIssue) {
		t.Errorf("canary: err = %v, want ErrNoIssue", err)
	}
}

func TestResolveBucketProjectRefusesNoneOrSeveral(t *testing.T) {
	ctx := context.Background()
	for name, projects := range map[string][]AdminProject{
		"none":          {{ID: "0-2", ShortName: "PW"}},
		"empty list":    nil,
		"two":           {{ID: "0-1", ShortName: "HK"}, {ID: "0-7", ShortName: "HK"}},
		"two by case":   {{ID: "0-1", ShortName: "HK"}, {ID: "0-7", ShortName: "hk"}},
		"answer, no id": {{ShortName: "HK"}},
	} {
		if id, err := ResolveBucketProject(ctx, projectLister{projects: projects}); !errors.Is(err, ErrBucketProject) || id != "" {
			t.Errorf("%s: %q %v, want ErrBucketProject", name, id, err)
		}
	}
	readErr := &APIError{Status: 503}
	if _, err := ResolveBucketProject(ctx, projectLister{err: readErr}); !errors.Is(err, readErr) || errors.Is(err, ErrBucketProject) {
		t.Errorf("failed read: %v, want the read error", err)
	}
}
