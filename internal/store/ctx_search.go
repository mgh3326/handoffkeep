package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SearchCtx implements scope "ctx": dialect-neutral LIKE search over the small
// checkpoints and memory tables, bounded by searchTimeout.
func (s *Store) SearchCtx(ctx context.Context, q, session string, limit int) ([]SearchResult, error) {
	deadline := searchTimeout()
	sctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	out, err := s.searchCtx(sctx, q, session, limit)
	if err != nil && ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: no result within %s", ErrSearchTimeout, deadline)
	}
	return out, err
}

func (s *Store) searchCtx(ctx context.Context, q, session string, limit int) ([]SearchResult, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	pattern := "%" + like + "%"

	sql := `SELECT 'ctx' scope, c.id::text key, c.session, c.kind, c.title, left(c.body, 160) snippet, c.refs, c.created_at
		FROM checkpoints c
		WHERE ($1 = '' OR c.session = $1)
		  AND (c.title LIKE $2 ESCAPE '\' OR c.body LIKE $2 ESCAPE '\')
		UNION ALL
		SELECT 'ctx' scope, m.agent || '/' || m.name key, m.agent session, m.memory_type kind, m.name title, left(m.content, 160) snippet, '{}'::jsonb refs, m.updated_at created_at
		FROM memory m
		WHERE ($1 = '' OR m.agent = $1)
		  AND (m.name LIKE $2 ESCAPE '\' OR m.description LIKE $2 ESCAPE '\' OR m.content LIKE $2 ESCAPE '\')
		ORDER BY created_at DESC, key
		LIMIT $3`

	args := []any{session, pattern, limit + 1}
	if s.dialect == mysqlDialect {
		sql, args = ctxSearchMySQL, []any{session, session, pattern, pattern, session, session, pattern, pattern, pattern, limit + 1}
	}
	rows, err := s.db().Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search ctx: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var x SearchResult
		var refs []byte
		if err := rows.Scan(&x.Scope, &x.Key, &x.Session, &x.Kind, &x.Title, &x.Snippet, &refs, &x.CreatedAt); err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			if err := json.Unmarshal(refs, &x.Refs); err != nil {
				return nil, err
			}
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > limit {
		out = out[:limit]
		for i := range out {
			out[i].Truncated = true
		}
	}
	return out, nil
}

// ctxSearchMySQL is the scope "ctx" search on MySQL. Its collation is
// utf8mb4_0900_bin, so LIKE is case-sensitive as on PostgreSQL. The result key
// is item_key because `key` is reserved in MySQL, and the memory refs are an
// empty JSON object, as in the PostgreSQL statement.
const ctxSearchMySQL = `SELECT 'ctx' scope, CAST(c.id AS CHAR) item_key, c.session, c.kind, c.title, LEFT(c.body, 160) snippet, c.refs, c.created_at
		FROM checkpoints c
		WHERE (? = '' OR c.session = ?)
		  AND (c.title LIKE ? ESCAPE '\\' OR c.body LIKE ? ESCAPE '\\')
		UNION ALL
		SELECT 'ctx' scope, CONCAT(m.agent, '/', m.name) item_key, m.agent session, m.memory_type kind, m.name title, LEFT(m.content, 160) snippet, CAST('{}' AS JSON) refs, m.updated_at created_at
		FROM memory m
		WHERE (? = '' OR m.agent = ?)
		  AND (m.name LIKE ? ESCAPE '\\' OR m.description LIKE ? ESCAPE '\\' OR m.content LIKE ? ESCAPE '\\')
		ORDER BY created_at DESC, item_key
		LIMIT ?`
