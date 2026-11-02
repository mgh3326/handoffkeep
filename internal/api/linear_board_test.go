package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type boardFake struct {
	issues      map[string]BoardIssue
	comments    map[string]string
	creates     int
	updates     int
	states      []string
	labels      []string
	loseCreate  bool
	loseComment bool
	failUpdate  bool
}

func newBoardFake() *boardFake {
	return &boardFake{issues: map[string]BoardIssue{}, comments: map[string]string{}}
}
func (f *boardFake) BoardTarget(context.Context) (BoardTarget, error) {
	return BoardTarget{TeamID: "team", States: map[string]string{"Backlog": "Backlog", "In Progress": "In Progress", "In Review": "In Review", "Done": "Done", "Canceled": "Canceled"}}, nil
}
func (f *boardFake) BoardLabel(_ context.Context, _, name string) (string, error) { return name, nil }
func (f *boardFake) BoardIssue(_ context.Context, id string) (BoardIssue, error) {
	v, ok := f.issues[id]
	if !ok {
		return v, errors.New("not found")
	}
	return v, nil
}
func (f *boardFake) CreateLinearIssue(_ context.Context, in LinearDecisionIssue) (LinearIssue, error) {
	if _, ok := f.issues[in.ID]; ok {
		return LinearIssue{}, errors.New("duplicate")
	}
	f.creates++
	issue := BoardIssue{ID: in.ID, Identifier: fmt.Sprintf("ROB-%d", f.creates), Description: in.Description}
	issue.Team.ID = "team"
	f.issues[in.ID] = issue
	if f.loseCreate {
		f.loseCreate = false
		return LinearIssue{}, errors.New("lost response")
	}
	return LinearIssue{Identifier: issue.Identifier}, nil
}
func (f *boardFake) UpdateBoardIssue(_ context.Context, id, state, description string, labels []string) error {
	if f.failUpdate {
		return errors.New("offline")
	}
	issue := f.issues[id]
	issue.Description = description
	f.issues[id] = issue
	f.updates++
	f.states = append(f.states, state)
	f.labels = labels
	return nil
}
func (f *boardFake) CreateLinearComment(_ context.Context, id, issue, body string) error {
	if _, ok := f.comments[id]; ok {
		return errors.New("duplicate")
	}
	f.comments[id] = issue + body
	if f.loseComment {
		f.loseComment = false
		return errors.New("lost response")
	}
	return nil
}
func (f *boardFake) BoardCommentExists(_ context.Context, id, issue, body string) (bool, error) {
	return f.comments[id] == issue+body, nil
}

type boardQueue struct {
	rows       []store.LinearOutbox
	links      map[int64]store.LinearIssue
	reconciles int
}

func (q *boardQueue) GetLinearIssue(_ context.Context, id int64) (store.LinearIssue, bool, error) {
	v, ok := q.links[id]
	return v, ok, nil
}
func (q *boardQueue) ReconcileLinearBoard(context.Context) error { q.reconciles++; return nil }
func (q *boardQueue) ProcessNextLinearBoardOutbox(ctx context.Context, process func(context.Context, store.LinearOutbox) (store.LinearOutboxResult, error)) (bool, error) {
	for i, op := range q.rows {
		if op.State != "pending" || op.NextAttemptAt.After(time.Now()) {
			continue
		}
		result, err := process(ctx, op)
		if err != nil {
			return false, err
		}
		q.rows[i].State = result.State
		q.rows[i].NextAttemptAt = result.RetryAt
		if result.RemoteID != "" {
			q.links[op.TaskID] = store.LinearIssue{IssueID: result.RemoteID, Identifier: result.Identifier}
		}
		return true, nil
	}
	return false, nil
}
func (q *boardQueue) add(b store.LinearBoard, e store.LinearBoardEvent) {
	copy := b
	q.rows = append(q.rows, store.LinearOutbox{ID: int64(len(q.rows) + 1), TaskID: b.Task.ID, Op: store.LinearOpWorkBoard, State: "pending", Payload: store.LinearOutboxPayload{Board: &copy, Event: &e}})
}
func boardFixture() store.LinearBoard {
	var b store.LinearBoard
	b.ApplyTask(store.Task{ID: 42, Title: "Fleet change", State: "claimed", Refs: store.TaskRefs{JobID: "lead-job"}, UpdatedAt: time.Now().UTC()})
	return b
}

func TestWorkBoardFirstClaimAndRedelivery(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			linear := newBoardFake()
			linear.loseCreate = lost
			q := &boardQueue{links: map[int64]store.LinearIssue{}}
			b := boardFixture()
			e := store.LinearBoardEvent{ID: "claim", Kind: "claimed", At: time.Now().UTC()}
			q.add(b, e)
			w := LinearBoardWorker{Store: q, Linear: linear}
			w.Drain(t.Context())
			q.add(b, e)
			w.Drain(t.Context())
			if linear.creates != 1 || len(q.links) != 1 {
				t.Fatalf("creates=%d links=%v", linear.creates, q.links)
			}
		})
	}
}

func TestWorkBoardReusesROBTitleAndPreservesOtherText(t *testing.T) {
	linear := newBoardFake()
	issue := BoardIssue{ID: "existing", Identifier: "ROB-1234", Description: "Operator notes\n\n" + boardStart + "\nold\n" + boardEnd + "\nKeep this"}
	issue.Team.ID = "team"
	linear.issues["ROB-1234"] = issue
	linear.issues["existing"] = issue
	q := &boardQueue{links: map[int64]store.LinearIssue{}}
	b := boardFixture()
	b.Task.Title = "Fix ROB-1234 now"
	q.add(b, store.LinearBoardEvent{})
	LinearBoardWorker{Store: q, Linear: linear}.Drain(t.Context())
	got := linear.issues["existing"].Description
	if linear.creates != 0 || !strings.HasPrefix(got, "Operator notes\n\n") || !strings.HasSuffix(got, "\nKeep this") || strings.Contains(got, "\nold\n") || strings.Count(got, boardStart) != 1 {
		t.Fatalf("creates=%d description=%q", linear.creates, got)
	}
	if q.links[42].IssueID != "existing" {
		t.Fatal(q.links)
	}
}

func TestWorkBoardStateTransitions(t *testing.T) {
	for _, tc := range []struct{ state, pr, event, want string }{{"backlog", "", "", "Backlog"}, {"claimed", "", "", "In Progress"}, {"in_progress", "", "", "In Progress"}, {"claimed", "https://github.com/example/repo/pull/1", "", "In Review"}, {"verifying", "", "", "In Review"}, {"claimed", "", "READY", "In Review"}, {"merged", "", "", "Done"}, {"claimed", "", "MERGED", "Done"}, {"dropped", "", "", "Canceled"}} {
		t.Run(tc.state+tc.event+tc.pr, func(t *testing.T) {
			var b store.LinearBoard
			b.ApplyTask(store.Task{ID: 42, Title: "State", State: tc.state, Refs: store.TaskRefs{PR: tc.pr}})
			if tc.event != "" {
				b.ApplyEvent(store.LinearBoardEvent{Kind: tc.event, At: time.Now().UTC()}, "", "", "")
			}
			q := &boardQueue{links: map[int64]store.LinearIssue{}}
			q.add(b, store.LinearBoardEvent{})
			linear := newBoardFake()
			LinearBoardWorker{Store: q, Linear: linear}.Drain(t.Context())
			if len(linear.states) != 1 || linear.states[0] != tc.want {
				t.Fatalf("states=%v want=%s", linear.states, tc.want)
			}
		})
	}
}

func TestWorkBoardPlacementReviewAndComments(t *testing.T) {
	b := boardFixture()
	taskID := int64(42)
	lead, review := "host-a", "host-b"
	at := time.Now().UTC()
	b.ApplyPlacement(store.PlacementDecision{JobID: "lead-job", TaskID: &taskID, ChosenHost: &lead, Family: "openai", Model: "model-a", Role: "lead", Repo: "handoffkeep", DecidedAt: at})
	b.ApplyPlacement(store.PlacementDecision{JobID: "review-job", TaskID: &taskID, ChosenHost: &review, Family: "anthropic", Model: "model-b", Role: "review", DecidedAt: at})
	q := &boardQueue{links: map[int64]store.LinearIssue{}}
	q.add(b, store.LinearBoardEvent{})
	linear := newBoardFake()
	w := LinearBoardWorker{Store: q, Linear: linear}
	w.Drain(t.Context())
	labels := strings.Join(linear.labels, ",")
	for _, want := range []string{"fleet", "host:host-a", "host:host-b", "role:lead", "role:review", "model:openai", "model:anthropic", "repo:handoffkeep"} {
		if !strings.Contains(labels, want) {
			t.Fatalf("missing %s: %s", want, labels)
		}
	}
	linear.loseComment = true
	for i, kind := range []string{"READY", "ESC", "BLOCKED", "FINDINGS", "MERGED"} {
		e := store.LinearBoardEvent{ID: fmt.Sprint(i), Kind: kind, Text: "question / reason / count=2", At: at}
		q.add(b, e)
		q.add(b, e)
	}
	w.Drain(t.Context())
	if len(linear.comments) != 5 {
		t.Fatalf("comments=%v", linear.comments)
	}
	e := store.LinearBoardEvent{ID: "review-end", Kind: "FINDINGS", Text: "2 findings", At: at.Add(time.Second)}
	b.ApplyEvent(e, "review-job", "", "")
	q.add(b, e)
	w.Drain(t.Context())
	if strings.Contains(strings.Join(linear.labels, ","), "role:review") {
		t.Fatalf("stale review label: %v", linear.labels)
	}
	if !strings.Contains(linear.issues[q.links[42].IssueID].Description, "2 findings") {
		t.Fatal("review verdict absent")
	}
}

func TestWorkBoardRetryKeepsIssueAndHourlyReconciler(t *testing.T) {
	q := &boardQueue{links: map[int64]store.LinearIssue{}}
	q.add(boardFixture(), store.LinearBoardEvent{})
	linear := newBoardFake()
	linear.failUpdate = true
	w := LinearBoardWorker{Store: q, Linear: linear, ReconcileInterval: time.Second}
	w.Drain(t.Context())
	if linear.creates != 1 || q.rows[0].State != "pending" || len(q.links) != 1 {
		t.Fatal(q.rows, q.links)
	}
	linear.failUpdate = false
	q.rows[0].NextAttemptAt = time.Time{}
	w.Drain(t.Context())
	if linear.creates != 1 || q.rows[0].State != "sent" {
		t.Fatal(q.rows)
	}
	if w.reconcileInterval() < time.Hour {
		t.Fatal("reconciler runs too often")
	}
	if q.reconciles != 0 {
		t.Fatal("event drain invoked source reconciliation")
	}
}

func TestWorkBoardCompletedLeadLabelsAndEscapedMarkers(t *testing.T) {
	b := boardFixture()
	b.Task.Refs.JobID = ""
	host := "host-a"
	at := time.Now().UTC()
	b.ApplyPlacement(store.PlacementDecision{JobID: "placed-before-claim", ChosenHost: &host, Role: "lead", Family: "openai", DecidedAt: at})
	b.ApplyEvent(store.LinearBoardEvent{Kind: "READY", Text: boardEnd, At: at.Add(time.Second)}, "placed-before-claim", "", "")
	if got := strings.Join(boardLabelNames(b), ","); !strings.Contains(got, "host:host-a") {
		t.Fatal(got)
	}
	rendered := renderBoard(b)
	if !strings.Contains(rendered, "placed-before-claim") || strings.Contains(rendered, boardEnd) {
		t.Fatal(rendered)
	}
	for _, broken := range []string{boardStart + "unterminated", boardEnd, boardStart + boardEnd + boardStart + boardEnd} {
		if _, err := replaceBoardSection(broken, rendered); err == nil {
			t.Fatal("accepted broken section", broken)
		}
	}
}

func TestWorkBoardLongTextStaysBounded(t *testing.T) {
	b := boardFixture()
	b.Task.Title = strings.Repeat("긴", 300)
	q := &boardQueue{links: map[int64]store.LinearIssue{}}
	q.add(b, store.LinearBoardEvent{ID: "long-esc", Kind: "ESC", Text: strings.Repeat("긴", 5000), At: time.Now().UTC()})
	linear := newBoardFake()
	LinearBoardWorker{Store: q, Linear: linear}.Drain(t.Context())
	for _, comment := range linear.comments {
		if len([]rune(comment)) > 1900 {
			t.Fatal("comment exceeds short-event limit")
		}
	}
	if linear.creates != 1 || q.rows[0].State != "sent" {
		t.Fatal(q.rows)
	}
}
