package store

import "strings"

// Search on MySQL (hk 5912). PostgreSQL answers scopes "all", "docs" and
// "tasks" with full-text matching (to_tsvector / plainto_tsquery) OR ILIKE. The
// MySQL schema has no FULLTEXT index on these tables (the ngram index is B8
// work), so the MySQL statements keep the ILIKE half: a case-insensitive LIKE
// over the same columns, which PostgreSQL's ILIKE also matches. A multi-word
// query matches here only when its words appear together in order; PostgreSQL
// also matches the same words in any order.
//
// Snippets: ts_headline has no MySQL equivalent, so a snippet is 160
// characters starting 40 characters before the first case-insensitive match,
// or the first 160 characters when the match is not in that text.
//
// Matching is case-insensitive through LOWER on both sides, because the MySQL
// collation (utf8mb4_0900_bin) is case-sensitive. `key` is reserved in MySQL, so
// it is backtick-quoted, and the result key is item_key, as the ctx port does.

// docExactMySQL is the documents row for a numeric id query. Its arguments are
// the id, then the session twice; ExactNumericHit uses the same statement.
const docExactMySQL = "SELECT `key`,session,kind,LEFT(body,160),created_at FROM documents WHERE id=? AND (?='' OR session=?)"

// docsSearchMySQL is the documents part of scope "all" and "docs" on MySQL, in
// the column order of the PostgreSQL statement. q is matched as PostgreSQL
// matches it: literally inside LOCATE, and with % around it inside LIKE. fetch
// is the page size plus one, as in the PostgreSQL statement.
func docsSearchMySQL(q, session string, fetch int) (string, []any) {
	sql := "SELECT 'docs' scope, d.`key` item_key, d.session, d.kind, d.`key` title, " +
		"CASE WHEN LOCATE(LOWER(?), LOWER(d.body)) > 0 " +
		"THEN SUBSTRING(d.body, GREATEST(1, LOCATE(LOWER(?), LOWER(d.body)) - 40), 160) " +
		"ELSE LEFT(d.body, 160) END snippet, " +
		"CAST('{}' AS JSON) refs, d.created_at " +
		"FROM documents d " +
		"WHERE (? = '' OR d.session = ?) AND LOWER(CONCAT(d.`key`, ' ', d.body)) LIKE LOWER(?) " +
		"ORDER BY d.created_at DESC, d.`key`, d.session, d.`key` LIMIT ?"
	return sql, []any{q, q, session, session, "%" + q + "%", fetch}
}

// tasksExactMySQL is the tasks row for a numeric id query: the id, then the lane
// twice (the lane filter is skipped when it is empty).
const tasksExactMySQL = "SELECT CAST(t.id AS CHAR),t.lane,t.kind,t.title," + taskRefsExprMySQL + ",t.created_at FROM tasks t WHERE t.id=? AND (?='' OR t.lane=?)"

// tasksProbeCommentsMySQL reports whether task_comments exists; the PostgreSQL
// twin is to_regclass('task_comments').
const tasksProbeCommentsMySQL = "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'task_comments')"

// tasksSearchMySQL is scope "tasks" on MySQL: the same four candidate arms as
// searchTasks (title, translated body, comment bodies), one row per task with
// the lowest match rank and then the newest created_at, as PostgreSQL's
// DISTINCT ON keeps. The window functions pick that row before the LIMIT, so a
// comment-heavy task cannot fill the page. fetch is the page size plus one.
func tasksSearchMySQL(q, lane, like string, fetch int, comments bool) (string, []any) {
	pat := "%" + like + "%"
	// titleMatch: the task title, or its Korean translation, contains the term.
	const titleMatch = "(LOWER(t.title) LIKE LOWER(?) OR EXISTS (SELECT 1 FROM translations tr WHERE tr.entity='task' AND tr.entity_id=CAST(t.id AS CHAR) AND tr.field='title' AND tr.lang='ko' AND tr.source_sha256=SHA2(t.title,256) AND LOWER(tr.text) LIKE LOWER(?)))"
	const titleWindow = "CASE WHEN LOCATE(LOWER(?), LOWER(t.title)) > 0 " +
		"THEN SUBSTRING(t.title, GREATEST(1, LOCATE(LOWER(?), LOWER(t.title)) - 40), 160) ELSE LEFT(t.title, 160) END"

	titleSnippet := "COALESCE((SELECT SUBSTRING(tr.text, GREATEST(1, LOCATE(LOWER(?), LOWER(tr.text)) - 40), 160) " +
		"FROM translations tr WHERE tr.entity='task' AND tr.entity_id=CAST(t.id AS CHAR) AND tr.field='title' AND tr.lang='ko' " +
		"AND tr.source_sha256=SHA2(t.title,256) AND LOWER(tr.text) LIKE LOWER(?)), " + titleWindow + ")"

	parts := []string{
		"SELECT 'tasks' scope, CAST(t.id AS CHAR) item_key, t.lane session, t.kind, t.title, " + titleSnippet + " snippet, " +
			taskRefsExprMySQL + " refs, t.created_at, 0 match_rank FROM tasks t WHERE (? = '' OR t.lane = ?) AND " + titleMatch,
	}
	args := []any{
		// titleSnippet: translated (LOCATE, LIKE), then the title window (LOCATE twice).
		q, pat, q, q,
		// the lane filter, then titleMatch.
		lane, lane, pat, pat,
	}

	// A translated body is a task hit that ranks below a title hit.
	parts = append(parts,
		"SELECT 'tasks' scope, CAST(t.id AS CHAR) item_key, t.lane session, t.kind, t.title, "+
			"SUBSTRING(tr.text, GREATEST(1, LOCATE(LOWER(?), LOWER(tr.text)) - 40), 160) snippet, "+
			taskRefsExprMySQL+" refs, t.created_at, 1 match_rank "+
			"FROM tasks t JOIN documents d ON d.`key` = SUBSTRING_INDEX(t.body_doc, '#', 1) "+
			"JOIN translations tr ON tr.entity='task' AND tr.entity_id=CAST(t.id AS CHAR) AND tr.field='body' AND tr.lang='ko' "+
			"AND tr.source_sha256=SHA2(d.body,256) "+
			"WHERE (? = '' OR t.lane = ?) AND LOWER(tr.text) LIKE LOWER(?) AND NOT "+titleMatch)
	args = append(args, q, lane, lane, pat, pat, pat)

	if comments {
		// The comment arm keeps the newest matching comment of each task. The
		// window runs after the WHERE, as PostgreSQL's DISTINCT ON does.
		commentSnippet := "COALESCE((SELECT SUBSTRING(tr.text, GREATEST(1, LOCATE(LOWER(?), LOWER(tr.text)) - 40), 160) " +
			"FROM translations tr WHERE tr.entity='task_comment' AND tr.entity_id=CAST(c.id AS CHAR) AND tr.field='body' AND tr.lang='ko' " +
			"AND tr.source_sha256=SHA2(c.body,256) AND LOWER(tr.text) LIKE LOWER(?)), " +
			"CASE WHEN LOCATE(LOWER(?), LOWER(c.body)) > 0 " +
			"THEN SUBSTRING(c.body, GREATEST(1, LOCATE(LOWER(?), LOWER(c.body)) - 40), 160) ELSE LEFT(c.body, 160) END)"
		parts = append(parts,
			"SELECT 'tasks' scope, item_key, session, kind, title, snippet, refs, created_at, 2 match_rank FROM ("+
				"SELECT CAST(t.id AS CHAR) item_key, t.lane session, t.kind, t.title, "+commentSnippet+" snippet, "+
				taskRefsExprMySQL+" refs, t.created_at, "+
				"ROW_NUMBER() OVER (PARTITION BY t.id ORDER BY c.created_at DESC, c.id DESC) rn "+
				"FROM task_comments c JOIN tasks t ON t.id = c.task_id "+
				"WHERE (? = '' OR t.lane = ?) AND (LOWER(c.body) LIKE LOWER(?) OR EXISTS (SELECT 1 FROM translations tr "+
				"WHERE tr.entity='task_comment' AND tr.entity_id=CAST(c.id AS CHAR) AND tr.field='body' AND tr.lang='ko' "+
				"AND tr.source_sha256=SHA2(c.body,256) AND LOWER(tr.text) LIKE LOWER(?))) AND NOT "+titleMatch+
				") cm WHERE cm.rn = 1")
		args = append(args, q, pat, q, q, lane, lane, pat, pat, pat, pat)
	}

	sql := "SELECT scope, item_key, session, kind, title, snippet, refs, created_at FROM (" +
		"SELECT u.*, ROW_NUMBER() OVER (PARTITION BY u.item_key ORDER BY u.match_rank, u.created_at DESC) rn " +
		"FROM (" + strings.Join(parts, " UNION ALL ") + ") u) r " +
		"WHERE r.rn = 1 ORDER BY r.match_rank, r.created_at DESC, r.item_key DESC LIMIT ?"
	return sql, append(args, fetch)
}
