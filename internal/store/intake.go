package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// Triage intake and ticket sets (docs/linear-internalize.md, "Triage intake"
// and "Ticket sets"). Integrations and repo sessions put work into triage
// idempotently on (source, source_ref); the desk accepts or declines it
// through the triage gate in transitionTask.

var (
	// ErrTicketSetConflict refuses a ticket set whose (source, source_ref) is
	// already recorded with a different payload (Q12). Nothing changes.
	ErrTicketSetConflict = errors.New("ticket_set_conflict")
	// ErrTicketSetInvalid refuses a ticket set before anything is written: a
	// missing section heading, an unknown blocked_by reference, a bad ref.
	ErrTicketSetInvalid = errors.New("ticket_set_invalid")
)

// SimilarTaskThreshold is the pg_trgm similarity of two titles at or above
// which the duplicate check calls them similar.
const SimilarTaskThreshold = 0.5

// SimilarTaskLimit caps the similar tasks the duplicate check and a triage
// row list.
const SimilarTaskLimit = 5

// IntakeLane is the lane of an intake task or ticket set that names none.
const IntakeLane = "triage"

// TicketSetMaxChildren caps the children of one ticket set.
const TicketSetMaxChildren = 50

// TicketSections are the headings every ticket body carries. Acceptance is
// the closed contract mergewake reads from a linked task's body.
var TicketSections = []string{"Current", "Desired", "Key interfaces", "Out of scope", "Acceptance"}

// SimilarTask is an open task whose title is similar to a new one.
type SimilarTask struct {
	ID         int64   `json:"id"`
	Title      string  `json:"title"`
	State      string  `json:"state"`
	Lane       string  `json:"lane"`
	Similarity float64 `json:"similarity"`
}

// SimilarTasksError refuses a create that found similar open tasks; the
// caller answers with a link or new.
type SimilarTasksError struct {
	Tasks []SimilarTask
}

func (e *SimilarTasksError) Error() string {
	parts := make([]string, 0, len(e.Tasks))
	for _, x := range e.Tasks {
		parts = append(parts, fmt.Sprintf("#%d %q (%s)", x.ID, x.Title, x.State))
	}
	return "similar_tasks: " + strings.Join(parts, "; ")
}

type rowsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// similarOpenTasks lists started, unstarted and backlog tasks whose title is
// similar to any of titles, most similar first.
func similarOpenTasks(ctx context.Context, q rowsQuerier, titles []string) ([]SimilarTask, error) {
	rows, err := q.Query(ctx, `SELECT t.id, t.title, t.state, t.lane, max(similarity(t.title, n.title))::float8 AS score
		FROM tasks t CROSS JOIN unnest($1::text[]) AS n(title)
		WHERE t.state IN ('backlog','unstarted',`+startedStatesSQL+`) AND similarity(t.title, n.title) >= $2
		GROUP BY t.id
		ORDER BY score DESC, t.id
		LIMIT $3`, titles, SimilarTaskThreshold, SimilarTaskLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SimilarTask{}
	for rows.Next() {
		var x SimilarTask
		if err := rows.Scan(&x.ID, &x.Title, &x.State, &x.Lane, &x.Similarity); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// IntakeInput is one item put into triage by an integration.
type IntakeInput struct {
	Source    string  `json:"source"`
	SourceRef string  `json:"source_ref"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	Lane      string  `json:"lane,omitempty"`
	Project   *string `json:"project,omitempty"`
	Kind      string  `json:"kind,omitempty"`
	Priority  int     `json:"priority,omitempty"`
	CreatedBy string  `json:"-"`
}

// triageItem is what one triage task is created from: an intake item or one
// ticket of a set.
type triageItem struct {
	Title, Body, Kind string
	Project           *string
	Priority          int
}

func validSourceRef(x string) bool {
	if x == "" || len(x) > 512 {
		return false
	}
	for _, r := range x {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func checkTriageItem(x triageItem) error {
	if x.Title == "" || !validText(x.Title, MaxBytes) || strings.ContainsAny(x.Title, "\r\n") {
		return errors.New("invalid intake: title must be one non-empty line")
	}
	if !validText(x.Body, DocumentMaxBytes) {
		return errors.New("invalid intake: body is too long")
	}
	if x.Kind != "" && !taskKinds[x.Kind] {
		return fmt.Errorf("invalid intake: kind must be empty or one of %s", strings.Join(TaskKinds(), ", "))
	}
	if x.Priority < 0 {
		return errors.New("invalid intake: priority must not be negative")
	}
	if x.Project != nil && *x.Project == "" {
		return errors.New("invalid intake: project must be omitted or named")
	}
	if err := guard.Reject(x.Title); err != nil {
		return err
	}
	return guard.Reject(x.Body)
}

func checkIntakeHeader(source, sourceRef, lane, by string) error {
	if !validName(source) {
		return errors.New("invalid intake: source must match [A-Za-z0-9._-]{1,128}")
	}
	if !validSourceRef(sourceRef) {
		return errors.New("invalid intake: source_ref must be 1-512 bytes without control characters")
	}
	if lane != "" && !validName(lane) {
		return errors.New("invalid intake: lane must match [A-Za-z0-9._-]{1,128}")
	}
	if by == "" || !validName(by) {
		return errors.New("invalid intake: created_by")
	}
	return guard.Reject(sourceRef)
}

// taskBySourceTx returns the task recorded for (source, source_ref).
func taskBySourceTx(ctx context.Context, q rowQuerier, source, sourceRef string) (Task, bool, error) {
	var x Task
	err := scanTask(q.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE source=$1 AND source_ref=$2`, source, sourceRef), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, false, nil
	}
	return x, err == nil, err
}

// insertTriageTaskTx creates one task in triage and, when it has a body, the
// hk document task-body/<id> that holds it, linked by body_doc. The key is
// named after a task created in this transaction, so no other writer holds it.
func insertTriageTaskTx(ctx context.Context, tx pgx.Tx, item triageItem, source, sourceRef, lane string, parent *int64, by string, now time.Time) (Task, error) {
	if item.Project != nil {
		if err := requireTaskProject(ctx, tx, item.Project); err != nil {
			return Task{}, err
		}
	}
	if lane == "" {
		lane = IntakeLane
	}
	var x Task
	if err := scanTask(tx.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,priority,created_by,created_at,updated_at,last_activity_at,started_triage_at,project,parent_id,source,source_ref) VALUES($1,$2,$3,$4,$5,$6,$7,$7,$7,$7,$8,$9,$10,$11) RETURNING `+taskColumns,
		lane, item.Title, item.Kind, StateTriage, item.Priority, by, now, item.Project, parent, source, sourceRef), &x); err != nil {
		return Task{}, err
	}
	if item.Body == "" {
		return x, nil
	}
	key := fmt.Sprintf("task-body/%d", x.ID)
	sum := sha256.Sum256([]byte(item.Body))
	if _, err := tx.Exec(ctx, `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at) VALUES($1,'brief','','',$2,$3,$4,$5,$5)`, key, item.Body, hex.EncodeToString(sum[:]), by, now); err != nil {
		return Task{}, err
	}
	err := scanTask(tx.QueryRow(ctx, `UPDATE tasks SET body_doc=$2 WHERE id=$1 RETURNING `+taskColumns, x.ID, key), &x)
	return x, err
}

// IntakeTask puts one item into triage. A repeated (source, source_ref)
// returns the task already recorded for it with created=false and changes
// nothing. Kind may be left unset; the triage gate requires it on the way out.
func (s *Store) IntakeTask(ctx context.Context, in IntakeInput) (Task, bool, error) {
	if err := checkIntakeHeader(in.Source, in.SourceRef, in.Lane, in.CreatedBy); err != nil {
		return Task{}, false, err
	}
	item := triageItem{Title: in.Title, Body: in.Body, Kind: in.Kind, Project: in.Project, Priority: in.Priority}
	if err := checkTriageItem(item); err != nil {
		return Task{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx)
	// The same key is looked up and inserted under the task-mutation lock, so
	// two deliveries of one item cannot both insert.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, false, err
	}
	existing, found, err := taskBySourceTx(ctx, tx, in.Source, in.SourceRef)
	if err != nil {
		return Task{}, false, err
	}
	if found {
		return existing, false, tx.Commit(ctx)
	}
	x, err := insertTriageTaskTx(ctx, tx, item, in.Source, in.SourceRef, in.Lane, nil, in.CreatedBy, time.Now().UTC())
	if err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

// TicketBlocker is one blocked_by entry: a ref in the same set (a JSON
// string) or an existing task id (a JSON number).
type TicketBlocker struct {
	Ref    string
	TaskID int64
}

func (b *TicketBlocker) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &b.Ref)
	}
	var id int64
	if err := json.Unmarshal(data, &id); err != nil || id < 1 {
		return errors.New("blocked_by entries are a ref string or a positive task id")
	}
	b.TaskID = id
	return nil
}

func (b TicketBlocker) MarshalJSON() ([]byte, error) {
	if b.Ref != "" {
		return json.Marshal(b.Ref)
	}
	return json.Marshal(b.TaskID)
}

// Ticket is the parent or one child of a ticket set. Project, kind and
// priority are optional; a child that leaves them unset inherits the
// parent's when the set is accepted.
type Ticket struct {
	Ref       string          `json:"ref,omitempty"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	BlockedBy []TicketBlocker `json:"blocked_by,omitempty"`
	Project   *string         `json:"project,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Priority  int             `json:"priority,omitempty"`
}

func (t Ticket) item() triageItem {
	return triageItem{Title: t.Title, Body: t.Body, Kind: t.Kind, Project: t.Project, Priority: t.Priority}
}

// TicketSetInput is the document a repo session emits.
type TicketSetInput struct {
	Source    string   `json:"source"`
	SourceRef string   `json:"source_ref"`
	Lane      string   `json:"lane,omitempty"`
	Parent    Ticket   `json:"parent"`
	Children  []Ticket `json:"children"`
	CreatedBy string   `json:"-"`
}

// TicketSet is a recorded set: the parent and each child's task id.
type TicketSet struct {
	ID        int64            `json:"id"`
	Source    string           `json:"source"`
	SourceRef string           `json:"source_ref"`
	ParentID  int64            `json:"parent_id"`
	Children  []TicketSetChild `json:"children"`
	CreatedAt time.Time        `json:"created_at"`
}

// TicketSetChild maps one child ref to its task.
type TicketSetChild struct {
	Ref string `json:"ref"`
	ID  int64  `json:"id"`
}

// missingTicketSections lists the TicketSections headings body lacks. A
// heading is an ATX line outside a code fence whose text starts with the
// section name, case-insensitively, followed by nothing or a non-letter
// ("Acceptance (closed contract)"): the rule mergewake uses to find the
// Acceptance section.
func missingTicketSections(body string) []string {
	found := map[string]bool{}
	fenced := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || !strings.HasPrefix(trimmed, "#") {
			continue
		}
		text := strings.TrimLeft(trimmed, "#")
		if len(trimmed)-len(text) > 6 || (text != "" && text[0] != ' ' && text[0] != '\t') {
			continue
		}
		text = strings.ToLower(strings.TrimSpace(text))
		for _, name := range TicketSections {
			rest, ok := strings.CutPrefix(text, strings.ToLower(name))
			if !ok {
				continue
			}
			if r := []rune(rest); len(r) == 0 || !unicode.IsLetter(r[0]) {
				found[name] = true
			}
		}
	}
	var missing []string
	for _, name := range TicketSections {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func ticketSetInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrTicketSetInvalid}, args...)...)
}

// checkTicketSet validates a set without the database: shapes, section
// headings, refs, and that blocked_by among refs is acyclic.
func checkTicketSet(in TicketSetInput) error {
	if err := checkIntakeHeader(in.Source, in.SourceRef, in.Lane, in.CreatedBy); err != nil {
		return err
	}
	if in.Parent.Ref != "" || len(in.Parent.BlockedBy) > 0 {
		return ticketSetInvalid("the parent takes no ref or blocked_by")
	}
	if len(in.Children) == 0 || len(in.Children) > TicketSetMaxChildren {
		return ticketSetInvalid("a set has 1-%d children", TicketSetMaxChildren)
	}
	refs := map[string]int{}
	for i, c := range in.Children {
		if !validName(c.Ref) {
			return ticketSetInvalid("child %d needs a ref matching [A-Za-z0-9._-]{1,128}", i+1)
		}
		if !validSourceRef(childSourceRef(in.SourceRef, c.Ref)) {
			return ticketSetInvalid("source_ref/ref of child %s is longer than 512 bytes", c.Ref)
		}
		if _, dup := refs[c.Ref]; dup {
			return ticketSetInvalid("ref %s is used twice", c.Ref)
		}
		refs[c.Ref] = i
	}
	// Refs are checked above, so the reasons below name only checked refs.
	tickets := append([]Ticket{in.Parent}, in.Children...)
	for i, t := range tickets {
		name := "parent"
		if i > 0 {
			name = "child " + t.Ref
		}
		if err := checkTriageItem(t.item()); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if missing := missingTicketSections(t.Body); len(missing) > 0 {
			return ticketSetInvalid("%s body lacks the heading %s", name, strings.Join(missing, ", "))
		}
	}
	for _, c := range in.Children {
		for _, b := range c.BlockedBy {
			if b.TaskID != 0 {
				continue
			}
			if _, ok := refs[b.Ref]; !ok {
				return ticketSetInvalid("child %s is blocked by a ref that is not in the set", c.Ref)
			}
			if b.Ref == c.Ref {
				return fmt.Errorf("%w: child %s blocks itself", ErrRelationCycle, c.Ref)
			}
		}
	}
	// Depth-first search over blocked_by among refs; a ref met again while
	// it is on the path closes a cycle.
	const (
		unseen = iota
		onPath
		done
	)
	mark := make([]int, len(in.Children))
	var visit func(i int, path []string) error
	visit = func(i int, path []string) error {
		mark[i] = onPath
		path = append(path, in.Children[i].Ref)
		for _, b := range in.Children[i].BlockedBy {
			if b.TaskID != 0 {
				continue
			}
			j := refs[b.Ref]
			switch mark[j] {
			case onPath:
				return fmt.Errorf("%w: blocked_by %s -> %s closes a cycle", ErrRelationCycle, strings.Join(path, " -> "), b.Ref)
			case unseen:
				if err := visit(j, path); err != nil {
					return err
				}
			}
		}
		mark[i] = done
		return nil
	}
	for i := range in.Children {
		if mark[i] == unseen {
			if err := visit(i, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// ticketSetHash is the sha256 of the set as hk decoded it, so formatting and
// key order do not count as a change.
func ticketSetHash(in TicketSetInput) (string, error) {
	in.CreatedBy = ""
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func childSourceRef(setRef, ref string) string { return setRef + "/" + ref }

// EmitTicketSet records a ticket set in one transaction: the parent and the
// children in triage, each child's parent link, each body as a document, and
// the blocks edges. A repeated payload returns the recorded ids with
// created=false; a changed payload for the same (source, source_ref) is
// refused with ErrTicketSetConflict. Any refusal writes nothing.
func (s *Store) EmitTicketSet(ctx context.Context, in TicketSetInput) (TicketSet, bool, error) {
	if err := checkTicketSet(in); err != nil {
		return TicketSet{}, false, err
	}
	hash, err := ticketSetHash(in)
	if err != nil {
		return TicketSet{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TicketSet{}, false, err
	}
	defer tx.Rollback(ctx)
	// Several task rows and the blocks edges of existing tasks: the
	// task-mutation lock comes first (lockorder.go).
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return TicketSet{}, false, err
	}
	out := TicketSet{Source: in.Source, SourceRef: in.SourceRef}
	var recorded string
	err = tx.QueryRow(ctx, `SELECT id,payload_sha256,parent_task_id,created_at FROM ticket_sets WHERE source=$1 AND source_ref=$2`, in.Source, in.SourceRef).Scan(&out.ID, &recorded, &out.ParentID, &out.CreatedAt)
	switch {
	case err == nil:
		if recorded != hash {
			return TicketSet{}, false, fmt.Errorf("%w: this source_ref was emitted with a different payload", ErrTicketSetConflict)
		}
		for _, c := range in.Children {
			var id int64
			if err = tx.QueryRow(ctx, `SELECT id FROM tasks WHERE source=$1 AND source_ref=$2`, in.Source, childSourceRef(in.SourceRef, c.Ref)).Scan(&id); err != nil {
				return TicketSet{}, false, err
			}
			out.Children = append(out.Children, TicketSetChild{Ref: c.Ref, ID: id})
		}
		return out, false, tx.Commit(ctx)
	case !errors.Is(err, pgx.ErrNoRows):
		return TicketSet{}, false, err
	}
	keys := []string{in.SourceRef}
	for _, c := range in.Children {
		keys = append(keys, childSourceRef(in.SourceRef, c.Ref))
	}
	var taken bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE source=$1 AND source_ref=ANY($2))`, in.Source, keys).Scan(&taken); err != nil {
		return TicketSet{}, false, err
	}
	if taken {
		return TicketSet{}, false, fmt.Errorf("%w: a task already holds a source_ref of this set", ErrTicketSetConflict)
	}
	for _, c := range in.Children {
		for _, b := range c.BlockedBy {
			if b.TaskID == 0 {
				continue
			}
			var exists bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, b.TaskID).Scan(&exists); err != nil {
				return TicketSet{}, false, err
			}
			if !exists {
				return TicketSet{}, false, ticketSetInvalid("child %s is blocked by task %d, which does not exist", c.Ref, b.TaskID)
			}
		}
	}
	now := time.Now().UTC()
	parent, err := insertTriageTaskTx(ctx, tx, in.Parent.item(), in.Source, in.SourceRef, in.Lane, nil, in.CreatedBy, now)
	if err != nil {
		return TicketSet{}, false, err
	}
	out.ParentID, out.CreatedAt = parent.ID, now
	ids := map[string]int64{}
	for _, c := range in.Children {
		child, err := insertTriageTaskTx(ctx, tx, c.item(), in.Source, childSourceRef(in.SourceRef, c.Ref), in.Lane, &parent.ID, in.CreatedBy, now)
		if err != nil {
			return TicketSet{}, false, fmt.Errorf("child %s: %w", c.Ref, err)
		}
		ids[c.Ref] = child.ID
		out.Children = append(out.Children, TicketSetChild{Ref: c.Ref, ID: child.ID})
	}
	for _, c := range in.Children {
		for _, b := range c.BlockedBy {
			blocker := b.TaskID
			if blocker == 0 {
				blocker = ids[b.Ref]
			}
			if _, err = addRelationTx(ctx, tx, blocker, RelationBlocks, ids[c.Ref], in.CreatedBy, now); err != nil {
				return TicketSet{}, false, err
			}
		}
	}
	if err = tx.QueryRow(ctx, `INSERT INTO ticket_sets(source,source_ref,payload_sha256,parent_task_id,created_at) VALUES($1,$2,$3,$4,$5) RETURNING id`, in.Source, in.SourceRef, hash, parent.ID, now).Scan(&out.ID); err != nil {
		return TicketSet{}, false, err
	}
	return out, true, tx.Commit(ctx)
}

// TriageRow is one item of the triage queue: a task, or a ticket set shown
// by its parent. Similar lists open tasks whose title is similar to the
// row's, or to any title in the set.
type TriageRow struct {
	Task             Task          `json:"task"`
	TicketSet        bool          `json:"ticket_set"`
	ChildCount       int           `json:"child_count"`
	ChildrenInTriage int           `json:"children_in_triage"`
	AgeSeconds       int64         `json:"age_seconds"`
	Similar          []SimilarTask `json:"similar"`
}

// ListTriage returns the triage queue, oldest first. A ticket set is one row:
// its children are not listed on their own while the set's parent is still
// in triage.
func (s *Store) ListTriage(ctx context.Context, limit int) ([]TriageRow, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid triage limit")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixedTaskColumns("t")+`,
			EXISTS(SELECT 1 FROM ticket_sets ts WHERE ts.parent_task_id=t.id),
			(SELECT count(*) FROM tasks c WHERE c.parent_id=t.id),
			(SELECT count(*) FROM tasks c WHERE c.parent_id=t.id AND c.state='triage')
		FROM tasks t
		WHERE t.state='triage'
		  AND NOT EXISTS (SELECT 1 FROM ticket_sets ps JOIN tasks p ON p.id=ps.parent_task_id WHERE ps.parent_task_id=t.parent_id AND p.state='triage')
		ORDER BY COALESCE(t.started_triage_at, t.created_at), t.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	out := []TriageRow{}
	for rows.Next() {
		var r TriageRow
		var children, inTriage int64
		if err = scanTask(rows, &r.Task, &r.TicketSet, &children, &inTriage); err != nil {
			rows.Close()
			return nil, err
		}
		r.ChildCount, r.ChildrenInTriage = int(children), int(inTriage)
		out = append(out, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for i := range out {
		r := &out[i]
		since := r.Task.CreatedAt
		if r.Task.StartedTriageAt != nil {
			since = *r.Task.StartedTriageAt
		}
		r.AgeSeconds = int64(now.Sub(since) / time.Second)
		titles := []string{r.Task.Title}
		if r.TicketSet {
			kids, err := s.ListTaskChildren(ctx, r.Task.ID)
			if err != nil {
				return nil, err
			}
			for _, k := range kids {
				titles = append(titles, k.Title)
			}
		}
		if r.Similar, err = similarOpenTasks(ctx, s.pool, titles); err != nil {
			return nil, err
		}
	}
	return out, nil
}
