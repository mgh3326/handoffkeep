package store

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

var ErrStandingNotNewer = errors.New("standing_not_newer")
var ErrStandingConflict = errors.New("standing_conflict")
var standingTopicRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type StandingOrderInput struct {
	ScopeKind string    `json:"scope_kind"`
	Scope     string    `json:"scope"`
	TopicKey  string    `json:"topic_key"`
	Statement string    `json:"statement"`
	SourceDoc string    `json:"source_doc"`
	DecidedBy string    `json:"decided_by"`
	DecidedAt time.Time `json:"decided_at"`
}
type StandingOrder struct {
	ID int64 `json:"id"`
	StandingOrderInput
	SupersedesID *int64     `json:"supersedes_id"`
	SupersededAt *time.Time `json:"superseded_at"`
	RecordedBy   string     `json:"recorded_by"`
	CreatedAt    time.Time  `json:"created_at"`
}

const standingColumns = `id,scope_kind,scope,topic_key,statement,source_doc,decided_by,decided_at,supersedes_id,superseded_at,recorded_by,created_at`

type standingScanner interface{ Scan(...any) error }

func scanStanding(row standingScanner) (StandingOrder, error) {
	var x StandingOrder
	err := row.Scan(&x.ID, &x.ScopeKind, &x.Scope, &x.TopicKey, &x.Statement, &x.SourceDoc, &x.DecidedBy, &x.DecidedAt, &x.SupersedesID, &x.SupersededAt, &x.RecordedBy, &x.CreatedAt)
	return x, err
}
func (s *Store) PutStandingOrder(ctx context.Context, in StandingOrderInput, recordedBy string) (StandingOrder, bool, error) {
	var zero StandingOrder
	now := time.Now().UTC()
	if (in.ScopeKind != "project" && in.ScopeKind != "lane") || !validName(in.Scope) || !standingTopicRE.MatchString(in.TopicKey) || in.Statement == "" || !validText(in.Statement, 2000) || !utf8.ValidString(in.Statement) || (!strings.HasPrefix(in.SourceDoc, "decision/") && !strings.HasPrefix(in.SourceDoc, "policy/")) || strings.TrimSpace(in.DecidedBy) == "" || in.DecidedAt.IsZero() || in.DecidedAt.After(now.Add(5*time.Minute)) {
		return zero, false, errors.New("invalid standing order")
	}
	if err := guard.Reject(in.Statement); err != nil {
		return zero, false, err
	}
	_, found, err := s.GetDocument(ctx, in.SourceDoc)
	if err != nil {
		return zero, false, err
	}
	if !found {
		return zero, false, errors.New("invalid standing source document")
	}
	in.DecidedAt = in.DecidedAt.UTC().Truncate(time.Microsecond)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, false, err
	}
	defer tx.Rollback(ctx)
	old, err := scanStanding(tx.QueryRow(ctx, `SELECT `+standingColumns+` FROM standing_orders WHERE scope_kind=$1 AND scope=$2 AND topic_key=$3 AND superseded_at IS NULL FOR UPDATE`, in.ScopeKind, in.Scope, in.TopicKey))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return zero, false, err
	}
	var prior *int64
	if err == nil {
		if old.Statement == in.Statement && old.SourceDoc == in.SourceDoc && old.DecidedBy == in.DecidedBy && old.DecidedAt.Equal(in.DecidedAt) {
			return old, false, nil
		}
		if !in.DecidedAt.After(old.DecidedAt) {
			return zero, false, ErrStandingNotNewer
		}
		if _, err = tx.Exec(ctx, `UPDATE standing_orders SET superseded_at=$1 WHERE id=$2`, now, old.ID); err != nil {
			return zero, false, err
		}
		prior = &old.ID
	}
	x, err := scanStanding(tx.QueryRow(ctx, `INSERT INTO standing_orders (scope_kind,scope,topic_key,statement,source_doc,decided_by,decided_at,supersedes_id,recorded_by,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+standingColumns, in.ScopeKind, in.Scope, in.TopicKey, in.Statement, in.SourceDoc, in.DecidedBy, in.DecidedAt, prior, recordedBy, now))
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" && pgerr.ConstraintName == "standing_orders_active" {
			return zero, false, ErrStandingConflict
		}
		return zero, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, false, err
	}
	return x, true, nil
}
func (s *Store) ListStandingOrders(ctx context.Context, project, lane string) ([]StandingOrder, bool, error) {
	if (project == "" && lane == "") || (project != "" && !validName(project)) || (lane != "" && !validName(lane)) {
		return nil, false, errors.New("invalid standing scope")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+standingColumns+` FROM standing_orders WHERE superseded_at IS NULL AND ((scope_kind='project' AND scope=$1 AND $1 <> '') OR (scope_kind='lane' AND scope=$2 AND $2 <> '')) ORDER BY scope_kind,scope,topic_key,id LIMIT 201`, project, lane)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]StandingOrder, 0)
	for rows.Next() {
		x, e := scanStanding(rows)
		if e != nil {
			return nil, false, e
		}
		out = append(out, x)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > 200
	if truncated {
		out = out[:200]
	}
	return out, truncated, nil
}
