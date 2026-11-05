package store

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// Task origins say where a task came from by the provider's durable id. The
// display number a person reads (MGH-151, later ROB-1514) is metadata and may
// change; (provider, namespace, external_id) never does. Origins between hk
// tasks are task_relations, not origins. Schema: migration_origins.go.

// Origin providers.
const (
	OriginLinear = "linear"
	OriginGitHub = "github"
	OriginSentry = "sentry"
)

const (
	originTitleRunes      = 200
	originDisplayRefBytes = 64
	originURLBytes        = 512
	originExternalIDBytes = 256
	// OriginListMax is the most rows FindTaskOrigins returns.
	OriginListMax = 1000
)

var (
	originNamespaceRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`)
	originUUIDRE      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// TaskOriginInput is what a caller supplies for one origin. It is also the
// body of POST /v1/tasks/{id}/origins.
type TaskOriginInput struct {
	Provider   string `json:"provider"`
	Namespace  string `json:"namespace"`
	ExternalID string `json:"external_id"`
	DisplayRef string `json:"display_ref,omitempty"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
}

// TaskOrigin is one stored origin. An unknown display_ref, url or title is
// NULL in the table and absent here.
type TaskOrigin struct {
	ID            int64     `json:"id"`
	TaskID        int64     `json:"task_id"`
	Provider      string    `json:"provider"`
	Namespace     string    `json:"namespace"`
	ExternalID    string    `json:"external_id"`
	DisplayRef    string    `json:"display_ref,omitempty"`
	URL           string    `json:"url,omitempty"`
	TitleSnapshot string    `json:"title_snapshot,omitempty"`
	SnapshotAt    time.Time `json:"snapshot_at"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

func validOriginProvider(p string) bool {
	return p == OriginLinear || p == OriginGitHub || p == OriginSentry
}

// checkOriginExternalID judges an external id for its provider: a Linear id is
// a canonical lowercase UUID, any other is 1-256 bytes of non-space,
// non-control text.
func checkOriginExternalID(provider, id string) error {
	if provider == OriginLinear {
		if !originUUIDRE.MatchString(id) {
			return errors.New("invalid origin: a linear external_id is a lowercase UUID")
		}
		return nil
	}
	if id == "" || len(id) > originExternalIDBytes || !utf8.ValidString(id) {
		return errors.New("invalid origin: external_id is 1-256 bytes of text")
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("invalid origin: external_id has whitespace or a control character")
		}
	}
	return nil
}

// checkTaskOriginInput refuses a malformed origin and cuts the title to 200
// runes in place. It reads no database.
func checkTaskOriginInput(in *TaskOriginInput) error {
	if !validOriginProvider(in.Provider) {
		return errors.New("invalid origin: provider is linear, github or sentry")
	}
	if !originNamespaceRE.MatchString(in.Namespace) {
		return errors.New("invalid origin: namespace is 1-128 characters of A-Za-z0-9._/-")
	}
	if err := checkOriginExternalID(in.Provider, in.ExternalID); err != nil {
		return err
	}
	if !validText(in.DisplayRef, originDisplayRefBytes) {
		return errors.New("invalid origin: display_ref is at most 64 bytes")
	}
	if in.URL != "" {
		u, err := url.Parse(in.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || !validText(in.URL, originURLBytes) {
			return errors.New("invalid origin: url is an https URL of at most 512 bytes")
		}
	}
	if !utf8.ValidString(in.Title) || strings.ContainsRune(in.Title, 0) {
		return errors.New("invalid origin: title is not text")
	}
	if r := []rune(in.Title); len(r) > originTitleRunes {
		in.Title = string(r[:originTitleRunes])
	}
	if err := guard.Reject(in.Title + "\n" + in.URL); err != nil {
		return err
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const taskOriginColumns = `id,task_id,provider,namespace,external_id,COALESCE(display_ref,''),COALESCE(url,''),COALESCE(title_snapshot,''),snapshot_at,created_by,created_at`

func scanTaskOrigin(row pgx.Row, x *TaskOrigin) error {
	return row.Scan(&x.ID, &x.TaskID, &x.Provider, &x.Namespace, &x.ExternalID, &x.DisplayRef, &x.URL, &x.TitleSnapshot, &x.SnapshotAt, &x.CreatedBy, &x.CreatedAt)
}

// AddTaskOrigin records that a task came from an external object. The key is
// (provider, namespace, external_id, task): adding it again never makes a
// second row. It refreshes display_ref, url and the title snapshot when the
// new value is given and differs, and answers created=false. A value left out
// keeps the stored one. A missing task is ErrTaskNotFound.
//
// The insert takes KEY SHARE on one task row through the foreign key and
// locks no other task (lockorder.go, level 4).
func (s *Store) AddTaskOrigin(ctx context.Context, taskID int64, by string, in TaskOriginInput) (TaskOrigin, bool, error) {
	if taskID < 1 || by == "" || !validText(by, 128) {
		return TaskOrigin{}, false, errors.New("invalid origin")
	}
	if err := checkTaskOriginInput(&in); err != nil {
		return TaskOrigin{}, false, err
	}
	now := time.Now().UTC()
	var x TaskOrigin
	var created bool
	err := s.pool.QueryRow(ctx, `INSERT INTO task_origins(task_id,provider,namespace,external_id,display_ref,url,title_snapshot,snapshot_at,created_by,created_at)
SELECT t.id,$2,$3,$4,$5,$6,$7,$8,$9,$8 FROM tasks t WHERE t.id=$1
ON CONFLICT(provider,namespace,external_id,task_id) DO UPDATE SET
  display_ref=COALESCE(EXCLUDED.display_ref,task_origins.display_ref),
  url=COALESCE(EXCLUDED.url,task_origins.url),
  title_snapshot=COALESCE(EXCLUDED.title_snapshot,task_origins.title_snapshot),
  snapshot_at=EXCLUDED.snapshot_at
WHERE (COALESCE(EXCLUDED.display_ref,task_origins.display_ref),COALESCE(EXCLUDED.url,task_origins.url),COALESCE(EXCLUDED.title_snapshot,task_origins.title_snapshot))
  IS DISTINCT FROM (task_origins.display_ref,task_origins.url,task_origins.title_snapshot)
RETURNING `+taskOriginColumns+`, (xmax = 0)`,
		taskID, in.Provider, in.Namespace, in.ExternalID, nullIfEmpty(in.DisplayRef), nullIfEmpty(in.URL), nullIfEmpty(in.Title), now, by).
		Scan(&x.ID, &x.TaskID, &x.Provider, &x.Namespace, &x.ExternalID, &x.DisplayRef, &x.URL, &x.TitleSnapshot, &x.SnapshotAt, &x.CreatedBy, &x.CreatedAt, &created)
	if err == nil {
		return x, created, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TaskOrigin{}, false, err
	}
	// No row came back: the origin exists and nothing differed (the conflict
	// update was skipped), or the task does not exist.
	err = scanTaskOrigin(s.pool.QueryRow(ctx, `SELECT `+taskOriginColumns+` FROM task_origins WHERE provider=$1 AND namespace=$2 AND external_id=$3 AND task_id=$4`, in.Provider, in.Namespace, in.ExternalID, taskID), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskOrigin{}, false, ErrTaskNotFound
	}
	if err != nil {
		return TaskOrigin{}, false, err
	}
	return x, false, nil
}

func scanTaskOrigins(rows pgx.Rows) ([]TaskOrigin, error) {
	defer rows.Close()
	out := []TaskOrigin{}
	for rows.Next() {
		var x TaskOrigin
		if err := scanTaskOrigin(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListTaskOrigins returns a task's origins in id order. A missing task is
// ErrTaskNotFound, never an empty list.
func (s *Store) ListTaskOrigins(ctx context.Context, taskID int64) ([]TaskOrigin, error) {
	if taskID < 1 {
		return nil, errors.New("invalid origin query")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, taskID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrTaskNotFound
	}
	rows, err := tx.Query(ctx, `SELECT `+taskOriginColumns+` FROM task_origins WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	return scanTaskOrigins(rows)
}

// FindTaskOrigins looks origins up by the provider's durable id, across
// namespaces and tasks, in (task_id, id) order. The display number plays no
// part: a moved issue is found by its UUID.
func (s *Store) FindTaskOrigins(ctx context.Context, provider, externalID string, limit int) ([]TaskOrigin, error) {
	if !validOriginProvider(provider) {
		return nil, errors.New("invalid origin query: provider is linear, github or sentry")
	}
	if err := checkOriginExternalID(provider, externalID); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 100
	}
	if limit > OriginListMax {
		limit = OriginListMax
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskOriginColumns+` FROM task_origins WHERE provider=$1 AND external_id=$2 ORDER BY task_id,id LIMIT $3`, provider, externalID, limit)
	if err != nil {
		return nil, err
	}
	return scanTaskOrigins(rows)
}
