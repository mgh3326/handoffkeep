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

// YouTrackTaskHits maps issue ids to tasks through their verified bindings.
// Older tasks that still have only a task_origins mirror remain searchable;
// a binding takes precedence when both forms exist. A non-empty lane keeps
// only tasks in that lane. YouTrack's own text fills Title and Snippet.
func (s *Store) YouTrackTaskHits(ctx context.Context, tenant string, issueIDs []string, lane string) (map[string]SearchResult, error) {
	out := map[string]SearchResult{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	args := []any{issueIDs, tenant, lane, issueIDs, tenant, lane}
	if s.dialect == mysqlDialect {
		// MySQL has no = ANY; each source has its own IN list and filters.
		args = make([]any, 0, 2*len(issueIDs)+7)
		for i := 0; i < 2; i++ {
			args = append(args, tenant)
			for _, id := range issueIDs {
				args = append(args, id)
			}
			args = append(args, lane, lane)
		}
		// The second select's NOT EXISTS counts only this tenant's bindings; it
		// is the last placeholder in the statement.
		args = append(args, tenant)
	}
	rows, err := s.db().Query(ctx, sqlFor(s, `SELECT issue_id,task_id,lane,kind,refs,created_at FROM (
		SELECT b.issue_id,t.id::text AS task_id,t.id AS task_order,t.lane,t.kind,`+taskRefsExpr+` AS refs,t.created_at,0 AS source_order
		FROM yt_task_bindings b JOIN tasks t ON t.id=b.task_id
		WHERE b.tenant=$2 AND b.issue_id=ANY($1) AND ($3='' OR t.lane=$3)
		UNION ALL
		SELECT o.external_id,t.id::text,t.id,t.lane,t.kind,`+taskRefsExpr+`,t.created_at,1
		FROM task_origins o JOIN tasks t ON t.id=o.task_id
		WHERE o.provider='youtrack' AND o.namespace=$5 AND o.external_id=ANY($4) AND ($6='' OR t.lane=$6)
		AND NOT EXISTS (SELECT 1 FROM yt_task_bindings b WHERE b.task_id=o.task_id AND b.tenant=$2)
		) hits ORDER BY issue_id,source_order,task_order`, `SELECT issue_id,task_id,lane,kind,refs,created_at FROM (
		SELECT b.issue_id,CAST(t.id AS CHAR) AS task_id,t.id AS task_order,t.lane,t.kind,`+taskRefsExprMySQL+` AS refs,t.created_at,0 AS source_order
		FROM yt_task_bindings b JOIN tasks t ON t.id=b.task_id
		WHERE b.tenant=? AND b.issue_id IN (`+placeholders(len(issueIDs))+`) AND (?='' OR t.lane=?)
		UNION ALL
		SELECT o.external_id,CAST(t.id AS CHAR),t.id,t.lane,t.kind,`+taskRefsExprMySQL+`,t.created_at,1
		FROM task_origins o JOIN tasks t ON t.id=o.task_id
		WHERE o.provider='youtrack' AND o.namespace=? AND o.external_id IN (`+placeholders(len(issueIDs))+`) AND (?='' OR t.lane=?)
		AND NOT EXISTS (SELECT 1 FROM yt_task_bindings b WHERE b.task_id=o.task_id AND b.tenant=?)
		) hits ORDER BY issue_id,source_order,task_order`), args...)
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
		q = `SELECT b.article_id,d.` + "`key`" + `,d.session,d.kind,d.created_at
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
