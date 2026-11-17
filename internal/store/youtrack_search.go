package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// taskRefsExpr is the refs object a task search hit carries. searchTasks and
// the YouTrack task hits share it, so both search backends return one shape.
const taskRefsExpr = `jsonb_strip_nulls(jsonb_build_object('id',t.id::text,'state',t.state,'lane',t.lane,'priority',t.priority::text,'pr',t.refs->>'pr','head_sha',t.refs->>'head_sha','report_path',t.refs->>'report_path'))`

// NumericIDQuery reports whether a search query is a bare id or "#<id>". Every
// search backend answers those by exact lookup in PostgreSQL.
func NumericIDQuery(q string) bool {
	return numericIDQuery.MatchString(strings.TrimSpace(q))
}

// ExactNumericHit performs an exact numeric ID lookup for tasks and documents.
// The boolean reports whether the query was a numeric ID query.
func (s *Store) ExactNumericHit(ctx context.Context, q, scope, session string) ([]SearchResult, bool, error) {
	if s != nil && s.dialect == mysqlDialect {
		return nil, false, ErrDialectUnsupported
	}
	m := numericIDQuery.FindStringSubmatch(strings.TrimSpace(q))
	if m == nil {
		return nil, false, nil
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return nil, true, nil
	}
	var out []SearchResult
	if scope == "tasks" || scope == "all" {
		row := s.pool.QueryRow(ctx, `SELECT t.id::text,t.lane,t.kind,t.title,`+taskRefsExpr+`,t.created_at FROM tasks t WHERE t.id=$2 AND ($1='' OR t.lane=$1)`, session, id)
		var x SearchResult
		var refs []byte
		switch err := row.Scan(&x.Key, &x.Session, &x.Kind, &x.Title, &refs, &x.CreatedAt); {
		case err == nil:
			if err := json.Unmarshal(refs, &x.Refs); err != nil {
				return nil, true, err
			}
			x.Scope, x.Snippet = "tasks", x.Title
			out = append(out, x)
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return nil, true, fmt.Errorf("exact numeric hit task: %w", err)
		}
	}
	if scope == "docs" || scope == "all" {
		row := s.pool.QueryRow(ctx, `SELECT key,session,kind,left(body,160),created_at FROM documents WHERE id=$2 AND ($1='' OR session=$1)`, session, id)
		var x SearchResult
		switch err := row.Scan(&x.Key, &x.Session, &x.Kind, &x.Snippet, &x.CreatedAt); {
		case err == nil:
			x.Scope, x.Title = "docs", x.Key
			out = append(out, x)
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return nil, true, fmt.Errorf("exact numeric hit doc: %w", err)
		}
	}
	if len(out) > 0 && (scope == "docs" || scope == "all") {
		if err := s.linkTaskDocs(ctx, out); err != nil {
			return nil, true, err
		}
	}
	return out, true, nil
}

// ExactDocHit looks up a document by exact key.
// The boolean reports whether an exact document match was found.
func (s *Store) ExactDocHit(ctx context.Context, key, session string) ([]SearchResult, bool, error) {
	if s != nil && s.dialect == mysqlDialect {
		return nil, false, ErrDialectUnsupported
	}
	row := s.pool.QueryRow(ctx, `SELECT key,session,kind,left(body,160),created_at FROM documents WHERE key=$2 AND ($1='' OR session=$1)`, session, key)
	var x SearchResult
	switch err := row.Scan(&x.Key, &x.Session, &x.Kind, &x.Snippet, &x.CreatedAt); {
	case err == nil:
		x.Scope, x.Title = "docs", x.Key
		out := []SearchResult{x}
		if err := s.linkTaskDocs(ctx, out); err != nil {
			return nil, true, err
		}
		return out, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("exact doc hit: %w", err)
	}
}

// DocumentKeyExists reports whether a document has exactly this key. The
// YouTrack search backend uses it to keep an exact key lookup on PostgreSQL.
func (s *Store) DocumentKeyExists(ctx context.Context, key string) (bool, error) {
	if s != nil && s.dialect == mysqlDialect {
		return false, ErrDialectUnsupported
	}

	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM documents WHERE key=$1)`, key).Scan(&ok); err != nil {
		return false, fmt.Errorf("document key exists: %w", err)
	}
	return ok, nil
}

// YouTrackTaskHits maps YouTrack issue ids to the hk tasks they mirror in one
// tenant, keyed by issue id. task_origins is unique on (namespace, external_id),
// and the namespace is the tenant, so a mapping left by another YouTrack tenant
// never answers. An issue with no mapping in this tenant is absent, so the
// caller drops that hit. A non-empty lane keeps only tasks in that lane (the
// session filter). Title and Snippet are left empty: YouTrack's own text fills
// them. When two tasks mirror one issue in this tenant the lower task id wins.
func (s *Store) YouTrackTaskHits(ctx context.Context, tenant string, issueIDs []string, lane string) (map[string]SearchResult, error) {
	if s != nil && s.dialect == mysqlDialect {
		return nil, ErrDialectUnsupported
	}

	out := map[string]SearchResult{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT o.external_id,t.id::text,t.lane,t.kind,`+taskRefsExpr+` refs,t.created_at
		FROM task_origins o JOIN tasks t ON t.id=o.task_id
		WHERE o.provider='youtrack' AND o.namespace=$2 AND o.external_id=ANY($1) AND ($3='' OR t.lane=$3)
		ORDER BY o.external_id,t.id`, issueIDs, tenant, lane)
	if err != nil {
		return nil, fmt.Errorf("youtrack task hits: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var issueID string
		var x SearchResult
		var refs []byte
		if err := rows.Scan(&issueID, &x.Key, &x.Session, &x.Kind, &refs, &x.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(refs, &x.Refs); err != nil {
			return nil, err
		}
		if _, dup := out[issueID]; dup {
			continue
		}
		x.Scope = "tasks"
		out[issueID] = x
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// YouTrackDocHits maps YouTrack article ids to the hk documents they mirror,
// keyed by article id. An article with no document row (a per-prefix index
// article, or a document since deleted) is absent, so the caller drops it. A
// non-empty session keeps only documents in that session. Title is the
// document key, as in the PostgreSQL search; Refs carries the linked tasks.
func (s *Store) YouTrackDocHits(ctx context.Context, articleIDs []string, session string) (map[string]SearchResult, error) {
	if s != nil && s.dialect == mysqlDialect {
		return nil, ErrDialectUnsupported
	}

	out := map[string]SearchResult{}
	if len(articleIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT a.article_id,d.key,d.session,d.kind,d.created_at
		FROM youtrack_doc_articles a JOIN documents d ON d.key=COALESCE(NULLIF(a.document_key,''),a.doc_key)
		WHERE a.article_id=ANY($1) AND ($2='' OR d.session=$2)
		UNION ALL
		SELECT b.article_id,d.key,d.session,d.kind,d.created_at
		FROM yt_article_bindings b JOIN documents d ON d.id=b.document_id
		WHERE b.article_id=ANY($1) AND ($2='' OR d.session=$2)
		ORDER BY 1,2`, articleIDs, session)
	if err != nil {
		return nil, fmt.Errorf("youtrack doc hits: %w", err)
	}
	ids := []string{}
	list := []SearchResult{}
	for rows.Next() {
		var articleID string
		var x SearchResult
		if err := rows.Scan(&articleID, &x.Key, &x.Session, &x.Kind, &x.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if _, dup := out[articleID]; dup {
			continue
		}
		x.Scope, x.Title = "docs", x.Key
		out[articleID] = x
		ids = append(ids, articleID)
		list = append(list, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.linkTaskDocs(ctx, list); err != nil {
		return nil, err
	}
	for i, id := range ids {
		out[id] = list[i]
	}
	return out, nil
}
