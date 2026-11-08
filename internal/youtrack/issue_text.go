package youtrack

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// issueView is what one task's issue shows: its text and the PR it reads.
type issueView struct {
	Text string
	PR   *prView
}

// noiseLink is the marker stored for a machinery comment that is not mirrored.
// It is not a YouTrack comment id, so it never matches one in the pull.
func noiseLink(hkCommentID int64) string {
	return "noise:" + strconv.FormatInt(hkCommentID, 10)
}

// adoptedLink marks a comment the Python mirror posted before the Go sync
// took over, so the adoption does not post it again.
func adoptedLink(hkCommentID int64) string {
	return "adopted:" + strconv.FormatInt(hkCommentID, 10)
}

// commentMarkerRE reads the hk comment id out of a mirrored comment's marker.
var commentMarkerRE = regexp.MustCompile(`<!-- hk-comment:([0-9]+) -->`)

// commentMarkPage is the page size of the comment read behind commentMarks.
const commentMarkPage = 100

// commentMarks maps each hk comment id to the YouTrack comment that carries
// its marker. It reads every page, oldest first, and stops on a short page. A
// page that starts with the same comment as the one before means the server
// ignored $skip, so the read fails instead of looping.
func (s *Service) commentMarks(ctx context.Context, issueID string) (map[int64]string, error) {
	marks := map[int64]string{}
	skip := 0
	prevFirst := ""
	for {
		page, err := s.client.ListComments(ctx, issueID, skip, commentMarkPage)
		if err != nil {
			return nil, fmt.Errorf("comments: %w", err)
		}
		if skip > 0 && len(page) > 0 && page[0].ID == prevFirst {
			return nil, fmt.Errorf("comments: page at %d repeats the previous page", skip)
		}
		for _, c := range page {
			m := commentMarkerRE.FindStringSubmatch(c.Text)
			if m == nil {
				continue
			}
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				marks[id] = c.ID
			}
		}
		if len(page) < commentMarkPage {
			return marks, nil
		}
		prevFirst = page[0].ID
		skip += len(page)
	}
}

// renderTask reads what the issue quotes and renders it.
func (s *Service) renderTask(ctx context.Context, task store.Task) (issueView, error) {
	comments, err := s.store.ListTaskComments(ctx, task.ID, 0, store.TaskCommentListMax)
	if err != nil {
		return issueView{}, fmt.Errorf("comments: %w", err)
	}
	events, err := s.taskEvents(ctx, task.ID)
	if err != nil {
		return issueView{}, err
	}
	briefKey, brief, err := s.briefFor(ctx, task)
	if err != nil {
		return issueView{}, err
	}
	pr, err := s.prFor(ctx, task)
	if err != nil {
		return issueView{}, err
	}
	text := descriptionFor(issueText{Task: task, Console: s.console, BriefKey: briefKey, Brief: brief, PR: pr, Comments: comments, Events: events})
	return issueView{Text: text, PR: pr}, nil
}

// taskEvents reads every event of a task in id order, a page at a time, so
// the state history is the task's real last transitions.
func (s *Service) taskEvents(ctx context.Context, taskID int64) ([]store.TaskEvent, error) {
	var all []store.TaskEvent
	var after int64
	for {
		page, err := s.store.ListTaskEvents(ctx, taskID, after, store.TaskEventListMax)
		if err != nil {
			return nil, fmt.Errorf("events: %w", err)
		}
		all = append(all, page...)
		if len(page) < store.TaskEventListMax {
			return all, nil
		}
		after = page[len(page)-1].ID
	}
}

// briefFor finds the brief document of the task's job. The brief is keyed by
// the day it was written, so the task's start, creation and update days are
// tried in that order, as the Python mirror did.
func (s *Service) briefFor(ctx context.Context, task store.Task) (string, string, error) {
	job := task.Refs.JobID
	if job == "" {
		return "", "", nil
	}
	days := []time.Time{task.CreatedAt, task.UpdatedAt}
	if task.StartedAt != nil {
		days = append([]time.Time{*task.StartedAt}, days...)
	}
	seen := map[string]bool{}
	for _, at := range days {
		if at.IsZero() {
			continue
		}
		day := at.UTC().Format("2006-01-02")
		if seen[day] {
			continue
		}
		seen[day] = true
		key := "brief/" + day + "/" + job
		doc, ok, err := s.store.GetDocument(ctx, key)
		if err != nil {
			return "", "", fmt.Errorf("brief %s: %w", key, err)
		}
		if ok {
			return key, doc.Body, nil
		}
	}
	return "", "", nil
}

// prWorkStates are the task states that show no PR, as the Python mirror did.
var prWorkStates = map[string]bool{
	store.StateBacklog: true, store.StateIcebox: true, store.StateTriage: true,
	store.StateUnstarted: true, store.StateDropped: true, store.StateDuplicate: true,
}

// prFor reads the PR of the task's change: the open one if there is one,
// otherwise the newest. Its checks, findings and gate come from the change
// state read.
func (s *Service) prFor(ctx context.Context, task store.Task) (*prView, error) {
	if prWorkStates[task.State] {
		return nil, nil
	}
	changes, err := s.store.ListChangesByTask(ctx, task.ID, 50)
	if err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	if len(changes) == 0 {
		return nil, nil
	}
	best := changes[0]
	for _, c := range changes[1:] {
		if morePrimary(c, best) {
			best = c
		}
	}
	view, ok, err := s.store.GetChangeState(ctx, store.ChangeRef{Repo: best.Repo, PR: best.PRNumber},
		store.ChangeReadOptions{Include: store.ChangeInclude{Checks: true, Findings: true, Gate: true}})
	if err != nil {
		return nil, fmt.Errorf("change %s#%d: %w", best.Repo, best.PRNumber, err)
	}
	if !ok {
		return prViewOf(best, nil), nil
	}
	return prViewOf(best, &view), nil
}

// morePrimary reports whether change a is the one the issue reads in
// preference to b: an open change first, then the one opened latest.
func morePrimary(a, b store.Change) bool {
	openA, openB := a.State == "open", b.State == "open"
	if openA != openB {
		return openA
	}
	return a.OpenedAt.After(b.OpenedAt)
}

// prViewOf reduces a change and its state read to the fields the issue shows.
// Checks, findings and gate count only for the head the change is on now.
func prViewOf(c store.Change, view *store.ChangeView) *prView {
	pr := &prView{Repo: c.Repo, Number: c.PRNumber, URL: c.PR, State: c.State, MergedAt: c.MergedAt}
	head := ""
	if c.HeadSHA != nil {
		head = *c.HeadSHA
	}
	if view != nil {
		if view.Change.Title != nil {
			pr.Title = *view.Change.Title
		}
		if view.Change.HeadSHA != nil {
			head = *view.Change.HeadSHA
		}
		if view.Gate != nil {
			pr.Gate = view.Gate.State
			pr.Reasons = view.Gate.Reasons
		}
		if view.Checks != nil {
			failed := map[string]bool{}
			for _, k := range *view.Checks {
				if k.HeadSHA != head {
					continue
				}
				conclusion := ""
				if k.Conclusion != nil {
					conclusion = *k.Conclusion
				}
				switch {
				case conclusion == "failure" || conclusion == "timed_out" || conclusion == "cancelled":
					failed[k.Name] = true
				case k.State == "failure" || k.State == "error":
					failed[k.Name] = true
				}
			}
			for name := range failed {
				pr.Fails = append(pr.Fails, name)
			}
			sort.Strings(pr.Fails)
		}
		if view.Findings != nil {
			for _, f := range *view.Findings {
				if f.ReviewedHead != head || !openFinding(f.Status) {
					continue
				}
				pr.Findings++
				if f.Severity != nil && (*f.Severity == "critical" || *f.Severity == "high") {
					pr.High++
				}
			}
		}
	}
	pr.Head = head
	if len(head) > 7 {
		pr.Head = head[:7]
	}
	return pr
}
