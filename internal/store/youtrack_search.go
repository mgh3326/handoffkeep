package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// taskRefsExpr is the refs object a task search hit carries. searchTasks and
// the YouTrack task hits share it, so both search backends return one shape.
const taskRefsExpr = `jsonb_strip_nulls(jsonb_build_object('id',t.id::text,'state',t.state,'lane',t.lane,'priority',t.priority::text,'pr',t.refs->>'pr','head_sha',t.refs->>'head_sha','report_path',t.refs->>'report_path'))`

// taskRefsExprMySQL is taskRefsExpr on MySQL. It builds the same object of
// the same values; MySQL has no json_strip_nulls, so an absent ref is a JSON
// null the Go decoder turns into the field's zero value, and the two numeric
// refs are cast to text as PostgreSQL's ::text does.
const taskRefsExprMySQL = `JSON_OBJECT('id',CAST(t.id AS CHAR),'state',t.state,'lane',t.lane,'priority',CAST(t.priority AS CHAR),'pr',t.refs->>'$.pr','head_sha',t.refs->>'$.head_sha','report_path',t.refs->>'$.report_path')`

// NumericIDQuery reports whether a search query is a bare id or "#<id>". Every
// search backend answers those by exact lookup in PostgreSQL.
func NumericIDQuery(q string) bool {
	return numericIDQuery.MatchString(strings.TrimSpace(q))
}

// ExactNumericHit performs an exact numeric ID lookup for tasks and documents.
// The boolean reports whether the query was a numeric ID query.
func (s *Store) ExactNumericHit(ctx context.Context, q, scope, session string) ([]SearchResult, bool, error) {
	m := numericIDQuery.FindStringSubmatch(strings.TrimSpace(q))
	if m == nil {
		return nil, false, nil
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return nil, true, nil
	}
	var out []SearchResult
	args := []any{session, id}
	if s.dialect == mysqlDialect {
		// The MySQL twins number their placeholders in statement order: the
		// id, then the session twice.
		args = []any{id, session, session}
	}
	if scope == "tasks" || scope == "all" {
		row := s.db().QueryRow(ctx, sqlFor(s, `SELECT t.id::text,t.lane,t.kind,t.title,`+taskRefsExpr+`,t.created_at FROM tasks t WHERE t.id=$2 AND ($1='' OR t.lane=$1)`,
			`SELECT CAST(t.id AS CHAR),t.lane,t.kind,t.title,`+taskRefsExprMySQL+`,t.created_at FROM tasks t WHERE t.id=? AND (?='' OR t.lane=?)`), args...)
		var x SearchResult
		var refs []byte
		switch err := row.Scan(&x.Key, &x.Session, &x.Kind, &x.Title, &refs, &x.CreatedAt); {
		case err == nil:
			if err := json.Unmarshal(refs, &x.Refs); err != nil {
				return nil, true, err
			}
			x.Scope, x.Snippet = "tasks", x.Title
			out = append(out, x)
		case errors.Is(err, ErrNoRows):
		default:
			return nil, true, fmt.Errorf("exact numeric hit task: %w", err)
		}
	}
	if scope == "docs" || scope == "all" {
		row := s.db().QueryRow(ctx, sqlFor(s, `SELECT key,session,kind,left(body,160),created_at FROM documents WHERE id=$2 AND ($1='' OR session=$1)`,
			"SELECT `key`,session,kind,LEFT(body,160),created_at FROM documents WHERE id=? AND (?='' OR session=?)"), args...)
		var x SearchResult
		switch err := row.Scan(&x.Key, &x.Session, &x.Kind, &x.Snippet, &x.CreatedAt); {
		case err == nil:
			x.Scope, x.Title = "docs", x.Key
			out = append(out, x)
		case errors.Is(err, ErrNoRows):
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
	args := []any{session, key}
	if s.dialect == mysqlDialect {
		args = []any{key, session, session}
	}
	row := s.db().QueryRow(ctx, sqlFor(s, `SELECT key,session,kind,left(body,160),created_at FROM documents WHERE key=$2 AND ($1='' OR session=$1)`,
		"SELECT `key`,session,kind,LEFT(body,160),created_at FROM documents WHERE `key`=? AND (?='' OR session=?)"), args...)
	var x SearchResult
	switch err := row.Scan(&x.Key, &x.Session, &x.Kind, &x.Snippet, &x.CreatedAt); {
	case err == nil:
		x.Scope, x.Title = "docs", x.Key
		out := []SearchResult{x}
		if err := s.linkTaskDocs(ctx, out); err != nil {
			return nil, true, err
		}
		return out, true, nil
	case errors.Is(err, ErrNoRows):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("exact doc hit: %w", err)
	}
}

// DocumentKeyExists reports whether a document has exactly this key. The
// YouTrack search backend uses it to keep an exact key lookup before its
// ranked search, on both dialects.
func (s *Store) DocumentKeyExists(ctx context.Context, key string) (bool, error) {
	var ok bool
	if err := s.db().QueryRow(ctx, sqlFor(s, `SELECT EXISTS (SELECT 1 FROM documents WHERE key=$1)`,
		"SELECT EXISTS (SELECT 1 FROM documents WHERE `key`=?)"), key).Scan(&ok); err != nil {
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
	out := map[string]SearchResult{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	args := []any{issueIDs, tenant, lane}
	if s.dialect == mysqlDialect {
		// MySQL has no = ANY: the ids are an IN list, in statement order
		// after the namespace, and the lane's predicate repeats it.
		args = make([]any, 0, len(issueIDs)+3)
		args = append(args, tenant)
		for _, id := range issueIDs {
			args = append(args, id)
		}
		args = append(args, lane, lane)
	}
	rows, err := s.db().Query(ctx, sqlFor(s, `SELECT o.external_id,t.id::text,t.lane,t.kind,`+taskRefsExpr+` refs,t.created_at
		FROM task_origins o JOIN tasks t ON t.id=o.task_id
		WHERE o.provider='youtrack' AND o.namespace=$2 AND o.external_id=ANY($1) AND ($3='' OR t.lane=$3)
		ORDER BY o.external_id,t.id`, `SELECT o.external_id,CAST(t.id AS CHAR),t.lane,t.kind,`+taskRefsExprMySQL+` refs,t.created_at
		FROM task_origins o JOIN tasks t ON t.id=o.task_id
		WHERE o.provider='youtrack' AND o.namespace=? AND o.external_id IN (`+placeholders(len(issueIDs))+`) AND (?='' OR t.lane=?)
		ORDER BY o.external_id,t.id`), args...)
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
	out := map[string]SearchResult{}
	if len(articleIDs) == 0 {
		return out, nil
	}
	var q string
	var args []any
	if s.dialect == mysqlDialect {
		// MySQL has no youtrack_doc_articles (left out, design §2; it was
		// empty in production at the cutover), so only the bindings answer
		// (hk 5613). The search itself runs in YouTrack, not in the store.
		args = make([]any, 0, len(articleIDs)+2)
		for _, id := range articleIDs {
			args = append(args, id)
		}
		args = append(args, session, session)
		q = `SELECT b.article_id,d.key,d.session,d.kind,d.created_at
			FROM yt_article_bindings b JOIN documents d ON d.id=b.document_id
			WHERE b.article_id IN (` + inPlaceholders(s, len(articleIDs)) + `) AND (?='' OR d.session=?)
			ORDER BY 1,2`
	} else {
		q = `SELECT a.article_id,d.key,d.session,d.kind,d.created_at
		FROM youtrack_doc_articles a JOIN documents d ON d.key=COALESCE(NULLIF(a.document_key,''),a.doc_key)
		WHERE a.article_id=ANY($1) AND ($2='' OR d.session=$2)
		UNION ALL
		SELECT b.article_id,d.key,d.session,d.kind,d.created_at
		FROM yt_article_bindings b JOIN documents d ON d.id=b.document_id
		WHERE b.article_id=ANY($1) AND ($2='' OR d.session=$2)
		ORDER BY 1,2`
		args = []any{articleIDs, session}
	}
	rows, err := s.db().Query(ctx, q, args...)
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
