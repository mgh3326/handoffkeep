package youtrack

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// AdoptState is the part of the Python mirror's yt_state.json the adoption
// reads: which YouTrack issue each task and decision maps to, and which hk
// comments the mirror already posted or chose not to post.
type AdoptState struct {
	Issues    map[string]string           `json:"issues"`
	Comments  map[string][]int64          `json:"comments"`
	Decisions map[string]AdoptDecisionRow `json:"decisions"`
}

// AdoptDecisionRow is one decision entry of the Python state.
type AdoptDecisionRow struct {
	Issue string `json:"issue"`
	Stamp string `json:"stamp"`
}

// AdoptAction is one mapping the adoption wrote, or would write in dry-run.
type AdoptAction struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	IssueID    string `json:"issue_id"`
	ReadableID string `json:"readable_id,omitempty"`
	// Action is mapped, missing (the issue is gone; the sync creates one),
	// or project_mismatch (the issue is in another project; left alone).
	Action string `json:"action"`
	// Comments is the number of hk comments marked as already posted.
	Comments int `json:"comments,omitempty"`
}

// AdoptReport is the outcome of one adoption.
type AdoptReport struct {
	DryRun  bool          `json:"dry_run"`
	Actions []AdoptAction `json:"actions"`
	Errors  []string      `json:"errors,omitempty"`
}

// Adopt maps the issues the Python mirror created onto the Go sync's mirror
// rows, so the first Go run updates those issues instead of creating new
// ones, and marks the comments the mirror already posted so they are not
// posted again. Run it once, before the first sync.
//
// A task whose issue is in a different YouTrack project from the one its hk
// project maps to is left unmapped and reported: the sync would create a new
// issue there, and that is a decision for the operator.
func (s *Service) Adopt(ctx context.Context, st AdoptState, dryRun bool) (AdoptReport, error) {
	report := AdoptReport{DryRun: dryRun, Actions: []AdoptAction{}}
	taskKeys := sortedKeys(st.Issues)
	for _, key := range taskKeys {
		action, err := s.adoptTask(ctx, key, st, dryRun)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("task %s: %v", key, err))
			continue
		}
		report.Actions = append(report.Actions, action)
	}
	decisionKeys := make([]string, 0, len(st.Decisions))
	for key := range st.Decisions {
		decisionKeys = append(decisionKeys, key)
	}
	sort.Strings(decisionKeys)
	for _, key := range decisionKeys {
		action, err := s.adoptDecision(ctx, key, st.Decisions[key], dryRun)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("decision %s: %v", key, err))
			continue
		}
		report.Actions = append(report.Actions, action)
	}
	return report, nil
}

func (s *Service) adoptTask(ctx context.Context, key string, st AdoptState, dryRun bool) (AdoptAction, error) {
	taskID, err := strconv.ParseInt(key, 10, 64)
	if err != nil || taskID < 1 {
		return AdoptAction{}, fmt.Errorf("task id %q is not a number", key)
	}
	issueID := st.Issues[key]
	action := AdoptAction{Kind: "task", ID: key, IssueID: issueID}
	if issueID == "" {
		return action, fmt.Errorf("no issue id")
	}
	task, found, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return action, err
	}
	if !found {
		action.Action = "missing"
		return action, nil
	}
	hkProject := ""
	if task.Project != nil {
		hkProject = *task.Project
	}
	_, projectID, err := s.projectForTask(ctx, hkProject, false)
	if err != nil {
		return action, err
	}
	issue, err := s.client.GetIssue(ctx, issueID)
	if errors.Is(err, ErrNotFound) {
		action.Action = "missing"
		return action, nil
	}
	if err != nil {
		return action, err
	}
	action.ReadableID = issue.IDReadable
	if issue.Project == nil || issue.Project.ID != projectID {
		action.Action = "project_mismatch"
		return action, nil
	}
	action.Action = "mapped"
	if !dryRun {
		if _, err := s.store.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{TaskID: taskID, IssueID: issue.ID, DisplayRef: issue.IDReadable, URL: s.issueURL(issue.IDReadable), Tenant: s.tenant, Role: "mirror"}); err != nil {
			return action, err
		}
	}
	for _, hkCommentID := range st.Comments[key] {
		if hkCommentID < 1 {
			continue
		}
		action.Comments++
		if dryRun {
			continue
		}
		if err := s.store.LinkYouTrackComment(ctx, taskID, hkCommentID, adoptedLink(hkCommentID), s.now()); err != nil {
			return action, err
		}
	}
	return action, nil
}

func (s *Service) adoptDecision(ctx context.Context, key string, row AdoptDecisionRow, dryRun bool) (AdoptAction, error) {
	decisionID, err := strconv.ParseInt(key, 10, 64)
	if err != nil || decisionID < 1 {
		return AdoptAction{}, fmt.Errorf("decision id %q is not a number", key)
	}
	action := AdoptAction{Kind: "decision", ID: key, IssueID: row.Issue}
	issue, err := s.client.GetIssue(ctx, row.Issue)
	if errors.Is(err, ErrNotFound) {
		action.Action = "missing"
		return action, nil
	}
	if err != nil {
		return action, err
	}
	action.ReadableID = issue.IDReadable
	// A decision mirror row is trusted by the next sync, which writes the DEC
	// body and fields into the issue. An issue outside the DEC project must not
	// be stored as one.
	decProjectID, err := s.client.ProjectID(ctx, DecisionProject)
	if err != nil {
		return action, err
	}
	if issue.Project == nil || issue.Project.ID != decProjectID {
		action.Action = "project_mismatch"
		return action, nil
	}
	action.Action = "mapped"
	if dryRun {
		return action, nil
	}
	// An empty stamp makes the next sync rewrite the issue once, so the body
	// and fields follow the current request.
	if err := s.store.PutYouTrackDecisionMirror(ctx, store.YouTrackDecisionMirror{DecisionID: decisionID, IssueID: issue.ID, DisplayRef: issue.IDReadable}); err != nil {
		return action, err
	}
	return action, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, errA := strconv.ParseInt(keys[i], 10, 64)
		b, errB := strconv.ParseInt(keys[j], 10, 64)
		if errA == nil && errB == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}
