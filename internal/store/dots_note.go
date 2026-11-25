package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A dots note (hk 6307) tells the project's design session that dots wrote a
// comment or filed an intake. It is an opinion for the session to sort, never
// an order: orders stay on the task_order path. Each note is one
// notification_outbox row to the project's design lane by the owner rule
// (effectiveProjectOwner), keyed by its own event id, so a retry cannot queue
// it twice.

const (
	// dotsNoteTitleMax and dotsNoteLineMax cap the quoted task title and the
	// comment's first line, counted in runes, so the note stays far under the
	// forwarder's 2,048-byte cap whatever the input.
	dotsNoteTitleMax = 80
	dotsNoteLineMax  = 160
)

// dotsNoteCap returns the first n runes of s, never splitting a rune.
func dotsNoteCap(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// dotsNoteFirstLine returns the first non-empty line of body, trimmed.
func dotsNoteFirstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// dotsNoteCommentText is the text of a dots comment's note. The read hint
// names the comment itself and the comment before it, so the reader's list
// starts at this comment.
func dotsNoteCommentText(taskID int64, title, state, claimedBy string, commentID int64, body string) string {
	claim := ""
	if claimedBy != "" {
		claim = ", claimed by " + claimedBy
	}
	return fmt.Sprintf(`[hk] dots note on hk %d "%s" (%s%s) comment #%d: %s | a dots opinion for the design session to sort, not an order | read: handoffkeep tasks comments %d --after-id %d`,
		taskID, dotsNoteCap(title, dotsNoteTitleMax), state, claim, commentID,
		dotsNoteCap(dotsNoteFirstLine(body), dotsNoteLineMax), taskID, commentID-1)
}

// dotsNoteIntakeText is the text of a dots intake's note.
func dotsNoteIntakeText(taskID int64, title, state string) string {
	return fmt.Sprintf(`[hk] dots filed intake hk %d "%s" (%s) | a dots proposal for the design session to sort, not an order | read: handoffkeep tasks show %d`,
		taskID, dotsNoteCap(title, dotsNoteTitleMax), state, taskID)
}

func dotsNoteCommentEventID(commentID int64) string {
	return "dots-comment-" + strconv.FormatInt(commentID, 10)
}

func dotsNoteIntakeEventID(taskID int64) string {
	return "dots-intake-" + strconv.FormatInt(taskID, 10)
}

// dotsNoteOwnerTx is the lane a project's notes go to: its design lane, or
// operator-desk when the project has none (effectiveProjectOwner). A task with
// no project, or a project the vocabulary does not hold, also goes to
// operator-desk.
func dotsNoteOwnerTx(ctx context.Context, tx transaction, project *string) (string, error) {
	if project == nil || *project == "" {
		return effectiveProjectOwner(""), nil
	}
	var designLane string
	err := tx.QueryRow(ctx, sqlFor(tx,
		`SELECT design_lane FROM task_projects WHERE name=$1`,
		`SELECT design_lane FROM task_projects WHERE name=?`), *project).Scan(&designLane)
	if errors.Is(err, ErrNoRows) {
		designLane = ""
	} else if err != nil {
		return "", err
	}
	return effectiveProjectOwner(designLane), nil
}

// queueDotsCommentNoteTx queues the note of a comment dots wrote through the
// MCP path, inside that write's transaction. A task in lane dots gets no note:
// dots_reply already queues its own notices there.
//
// Comments written through dots_reply and the decision answers never reach
// this function: they insert their comments through their own paths and queue
// their own notices, so each comment produces one notice at most.
//
// The reads are plain selects. They take no lock, so the comment writer's
// lock order (lockorder.go) is unchanged.
func queueDotsCommentNoteTx(ctx context.Context, tx transaction, c TaskComment) error {
	var task Task
	err := scanTask(tx.QueryRow(ctx, sqlFor(tx,
		`SELECT `+taskColumns+` FROM tasks WHERE id=$1`,
		`SELECT `+taskColumns+` FROM tasks WHERE id=?`), c.TaskID), &task)
	if err != nil {
		return err
	}
	if task.Lane == "dots" {
		return nil
	}
	lane, err := dotsNoteOwnerTx(ctx, tx, task.Project)
	if err != nil {
		return err
	}
	_, err = insertNotificationOutboxOnce(ctx, tx, OutboxKindDotsNote, lane,
		dotsNoteCommentEventID(c.ID),
		dotsNoteCommentText(task.ID, task.Title, task.State, task.ClaimedBy, c.ID, c.Body),
		c.CreatedAt)
	return err
}

// queueDotsIntakeNoteTx queues the note of an intake dots filed. It runs in
// the intake's own transaction and only for a task that intake created, so a
// retry that finds the task queues nothing.
func queueDotsIntakeNoteTx(ctx context.Context, tx transaction, x Task, at time.Time) error {
	lane, err := dotsNoteOwnerTx(ctx, tx, x.Project)
	if err != nil {
		return err
	}
	_, err = insertNotificationOutboxOnce(ctx, tx, OutboxKindDotsNote, lane,
		dotsNoteIntakeEventID(x.ID),
		dotsNoteIntakeText(x.ID, x.Title, x.State),
		at)
	return err
}
