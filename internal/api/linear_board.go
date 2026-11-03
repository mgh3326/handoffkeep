package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const boardStart = "<!-- hk-work-board:start -->"
const boardEnd = "<!-- hk-work-board:end -->"

// BoardIssue includes the operator-owned text and labels that a rewrite must
// preserve. The issue's team is checked even when reusing a title's ROB id.
type BoardIssue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	ArchivedAt  string `json:"archivedAt"`
	Description string `json:"description"`
	Team        struct {
		ID string `json:"id"`
	} `json:"team"`
	Labels struct {
		Nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
	} `json:"labels"`
}

type BoardTarget struct {
	TeamID string
	States map[string]string
}

type LinearBoardAPI interface {
	BoardTarget(context.Context) (BoardTarget, error)
	BoardIssue(context.Context, string) (BoardIssue, error)
	CreateLinearIssue(context.Context, LinearDecisionIssue) (LinearIssue, error)
	BoardLabel(context.Context, string, string) (string, error)
	UpdateBoardIssue(context.Context, string, string, string, []string) error
	CreateLinearComment(context.Context, string, string, string) error
	BoardCommentExists(context.Context, string, string, string) (bool, error)
}

type LinearBoardStore interface {
	LinearBoardDeliveryStopped(context.Context, int64) (bool, error)
	HasLinearCreateReservation(context.Context, string) (bool, error)
	GetLinearBoard(context.Context, int64) (store.LinearBoard, bool, error)
	EnsureLinearBoardOwnership(context.Context, int64, string, string) error
	GetTask(context.Context, int64) (store.Task, bool, error)
	ProcessNextLinearBoardOutbox(context.Context, func(context.Context, store.LinearOutbox) (store.LinearOutboxResult, error)) (bool, error)
	GetLinearIssue(context.Context, int64) (store.LinearIssue, bool, error)
}

type LinearBoardWorker struct {
	Control     *LinearBoardControl
	Store       LinearBoardStore
	Linear      LinearBoardAPI
	TaskBaseURL string
}

// Run is event-driven: hk writes enqueue the outbox; Linear changes arrive via
// the signed webhook. ReconcileLinearBoard remains an explicit one-shot repair.
func (w LinearBoardWorker) Run(ctx context.Context) {
	if w.Control == nil {
		w.Control = &LinearBoardControl{}
	}
	drain := time.NewTicker(5 * time.Second)
	stats := time.NewTicker(time.Hour)
	defer drain.Stop()
	defer stats.Stop()
	started, calls := time.Now(), w.Control.CallCount()
	w.Drain(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-drain.C:
			w.Drain(ctx)
		case <-stats.C:
			current := w.Control.CallCount()
			elapsed := time.Since(started).Seconds()
			log.Printf("linear_board.calls elapsed_seconds=%.0f calls=%d calls_per_hour=%.2f", elapsed, current-calls, float64(current-calls)*3600/elapsed)
			started, calls = time.Now(), current
		}
	}
}

func (w LinearBoardWorker) Drain(ctx context.Context) {
	if w.Control == nil {
		if a, ok := w.Linear.(LinearAPI); ok && a.BoardControl != nil {
			w.Control = a.BoardControl
		} else {
			w.Control = &LinearBoardControl{}
		}
	}
	for ctx.Err() == nil && !w.Control.blocked() {
		found, err := w.Store.ProcessNextLinearBoardOutbox(ctx, w.send)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("linear_board.drain_failed err=%q", err)
			}
			return
		}
		if !found {
			return
		}
	}
}

var errBoardAwaitWebhook = errors.New("linear board awaiting issue webhook")

var errBoardMirrorSkipped = errors.New("linear mirror not eligible")

var boardROB = regexp.MustCompile(`\bROB-[1-9][0-9]*\b`)

func (w LinearBoardWorker) send(ctx context.Context, op store.LinearOutbox) (store.LinearOutboxResult, error) {
	result := store.LinearOutboxResult{State: "sent"}
	err := w.sync(ctx, op, &result)
	if errors.Is(err, errBoardMirrorSkipped) || errors.Is(err, ErrLinearMirrorExcluded) {
		result.State = "skipped"
	} else if errors.Is(err, store.ErrLinearIssueOwnership) {
		result.State = "failed"
		result.LastError = err.Error()
	} else if err != nil {
		var label linearBoardLabelUnavailableError
		var rate linearRateLimitError
		if errors.As(err, &label) {
			w.Control.breakLabel(label.Name, label.TeamID)
		}
		result.State = "pending"
		result.RetryAt = time.Now().UTC().Add(min(time.Hour, time.Duration(1<<min(op.Attempts, 10))*5*time.Second))
		result.LastError = err.Error()
		if errors.As(err, &rate) {
			result.RetryAt = rate.Until
		}
		if !w.Control.blocked() && !errors.Is(err, errBoardAwaitWebhook) {
			log.Printf("linear_board.sync_failed task_id=%d err=%q", op.TaskID, err)
		}
	}
	return result, nil
}

func (w LinearBoardWorker) sync(ctx context.Context, op store.LinearOutbox, result *store.LinearOutboxResult) error {
	b := op.Payload.Board
	if b == nil || b.Task.ID != op.TaskID {
		return errors.New("invalid work board outbox payload")
	}
	// Re-read admission immediately before any remote work: pending payloads
	// may predate this policy or a later task edit. Missing tasks fail closed.
	task, found, err := w.Store.GetTask(ctx, op.TaskID)
	if err != nil {
		return err
	}
	if !found {
		return errBoardMirrorSkipped
	}
	stopped, err := w.Store.LinearBoardDeliveryStopped(ctx, op.TaskID)
	if err != nil {
		return err
	}
	if stopped {
		return errBoardMirrorSkipped
	}
	link, linked, err := w.Store.GetLinearIssue(ctx, op.TaskID)
	if err != nil {
		return err
	}
	if !linked {
		stableID := store.LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", op.TaskID))
		pending, err := w.Store.HasLinearCreateReservation(ctx, stableID)
		if err != nil {
			return err
		}
		if pending {
			return errBoardAwaitWebhook
		}
		if store.LinearMirrorReason(task) == "" {
			return errBoardMirrorSkipped
		}
	}

	// Old deliveries retain their own event/comment identity, but issue fields
	// always come from the latest committed projection version.
	latest, found, err := w.Store.GetLinearBoard(ctx, op.TaskID)
	if err != nil {
		return err
	}
	if !found || latest.Task.ID != op.TaskID || latest.Version < 0 || b.Version > latest.Version {
		return errors.New("invalid current board version")
	}
	current := latest
	current.ApplyTask(task)
	b = &current
	target, err := w.Linear.BoardTarget(ctx)
	if err != nil {
		return err
	}
	stateID := target.States[b.State]
	if stateID == "" {
		return fmt.Errorf("linear board state %q missing", b.State)
	}
	fleet, err := w.Linear.BoardLabel(ctx, target.TeamID, "fleet")
	if err != nil {
		return err
	}
	var issue BoardIssue
	if linked {
		issue, err = w.Linear.BoardIssue(ctx, link.IssueID)
	} else if b.Task.Refs.Linear != nil && b.Task.Refs.Linear.MirrorReason == store.LinearMirrorOpened && b.Task.Refs.Linear.IssueID != "" {
		issue, err = w.Linear.BoardIssue(ctx, b.Task.Refs.Linear.IssueID)
	} else if identifier := boardROB.FindString(b.Task.Title); identifier != "" {
		issue, err = w.Linear.BoardIssue(ctx, identifier)
	} else {
		id := store.LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", op.TaskID))
		_, err := w.Linear.CreateLinearIssue(ctx, LinearDecisionIssue{Board: true, ID: id, TaskID: op.TaskID, TeamID: target.TeamID, LabelID: fleet, Title: shortBoardText(strings.SplitN(b.Task.Title, "\n", 2)[0], linearTitleMaxRunes), Description: fmt.Sprintf("[hk-task:%d] <!-- work-board:task:%d -->", op.TaskID, op.TaskID)})
		if err != nil {
			return err
		}
		return errBoardAwaitWebhook
	}
	if err != nil {
		return err
	}
	if issue.ArchivedAt != "" {
		return errBoardMirrorSkipped
	}
	if issue.ID == "" || issue.Team.ID != target.TeamID || issue.Labels.PageInfo.HasNextPage {
		return errors.New("linear board issue team or labels incomplete")
	}
	if err := w.Store.EnsureLinearBoardOwnership(ctx, op.TaskID, issue.ID, issue.Identifier); err != nil {
		return err
	}
	result.RemoteID, result.Identifier = issue.ID, issue.Identifier
	section := renderBoard(*b)
	section = fmt.Sprintf("<!-- work-board:task:%d -->\n", op.TaskID) + section
	if w.TaskBaseURL != "" {
		section = strings.Replace(section, fmt.Sprintf("[hk-task:%d]", op.TaskID), fmt.Sprintf("[hk-task:%d](%s/ui/tasks/%d)", op.TaskID, strings.TrimRight(w.TaskBaseURL, "/"), op.TaskID), 1)
	}
	description, err := replaceBoardSection(issue.Description, section)
	if err != nil {
		return err
	}
	names := boardLabelNames(*b)
	labels := []string{fleet}
	for _, name := range names {
		id, err := w.Linear.BoardLabel(ctx, target.TeamID, name)
		if err != nil {
			return err
		}
		labels = append(labels, id)
	}
	for _, label := range issue.Labels.Nodes {
		if label.Name != "fleet" && !managedBoardLabel(label.Name) {
			labels = append(labels, label.ID)
		}
	}
	slices.Sort(labels)
	labels = slices.Compact(labels)
	if err = w.Linear.UpdateBoardIssue(ctx, issue.ID, stateID, description, labels); err != nil {
		return err
	}
	if event := op.Payload.Event; event != nil && boardCommentKind(event.Kind) {
		body := strings.ToUpper(event.Kind) + " · " + event.At.UTC().Format(time.RFC3339)
		if event.Text != "" {
			body += " | " + shortBoardText(event.Text, 1800)
		}
		id := store.LinearDecisionUUID(fmt.Sprintf("work-board:comment:%d:%s", op.TaskID, event.ID))
		if err = w.Linear.CreateLinearComment(ctx, id, issue.ID, body); err != nil {
			// A duplicate-id response alone is not proof: read back this exact
			// comment, issue and body before acknowledging the outbox row.
			exists, readErr := w.Linear.BoardCommentExists(ctx, id, issue.ID, body)
			if readErr != nil || !exists {
				return err
			}
		}
	}
	return nil
}

func boardCommentKind(kind string) bool {
	switch strings.ToUpper(kind) {
	case "READY", "ESC", "BLOCKED", "FINDINGS", "MERGED":
		return true
	}
	return false
}

func managedBoardLabel(name string) bool {
	for _, prefix := range []string{"host:", "role:", "model:", "repo:"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func boardLabelNames(b store.LinearBoard) []string {
	names := []string{}
	for _, s := range b.Sessions {
		if !s.Active && s.JobID != b.PrimaryJob() {
			continue
		}
		for _, label := range []struct{ prefix, value string }{{"host:", s.Host}, {"role:", s.Role}, {"model:", s.Family}, {"repo:", s.Repo}} {
			if label.value != "" {
				names = append(names, label.prefix+label.value)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func replaceBoardSection(description, section string) (string, error) {
	start := strings.Index(description, boardStart)
	if start < 0 {
		if strings.Contains(description, boardEnd) {
			return "", errors.New("work board section has no start marker")
		}
		return description + "\n\n" + boardStart + "\n" + section + "\n" + boardEnd, nil
	}
	end := strings.Index(description[start:], boardEnd)
	if end < 0 {
		return "", errors.New("work board section has no end marker")
	}
	tail := description[start+end+len(boardEnd):]
	// Refuse ambiguous markers instead of deleting operator text.
	if strings.Contains(tail, boardStart) || strings.Contains(tail, boardEnd) {
		return "", errors.New("multiple work board sections")
	}
	return description[:start] + boardStart + "\n" + section + "\n" + boardEnd + tail, nil
}

func renderBoard(b store.LinearBoard) string {
	var out strings.Builder
	fmt.Fprintf(&out, "## Fleet work\n\n[hk-task:%d] · `handoffkeep tasks show %d`\n\n", b.Task.ID, b.Task.ID)
	fmt.Fprintf(&out, "State: **%s**\n\n", b.State)
	for _, s := range b.Sessions {
		if s.Role == "review" {
			continue
		}
		if s.JobID != b.PrimaryJob() && !s.Active {
			continue
		}
		fmt.Fprintf(&out, "- %s: `%s` · host `%s` · pane `%s` · model `%s` (%s / %s)\n", s.Role, s.JobID, s.Host, s.Pane, s.Family, s.Model, s.Profile)
	}
	if b.Task.Refs.PR != "" {
		fmt.Fprintf(&out, "\nPR: %s · head `%s`\n", b.Task.Refs.PR, b.Task.Refs.HeadSHA)
	}
	out.WriteString("\n### Review passes\n\n")
	for _, s := range b.Sessions {
		if s.Role != "review" {
			continue
		}
		verdict := s.Verdict
		if verdict == "" {
			if s.Active {
				verdict = "running"
			} else {
				verdict = "ended"
			}
		}
		fmt.Fprintf(&out, "- `%s`: host `%s` · pane `%s` · model `%s` (%s / %s) · %s\n", s.JobID, s.Host, s.Pane, s.Family, s.Model, s.Profile, shortBoardText(verdict, 1000))
	}
	fmt.Fprintf(&out, "\nLast event: **%s** · %s\n\n%s", b.Last.Kind, b.Last.At.UTC().Format(time.RFC3339), shortBoardText(b.Last.Text, 2000))
	return strings.NewReplacer(boardStart, "&lt;!-- hk-work-board:start --&gt;", boardEnd, "&lt;!-- hk-work-board:end --&gt;").Replace(out.String())
}

func shortBoardText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}
