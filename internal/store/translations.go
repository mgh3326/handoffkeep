package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// TranslationBatchMaxBytes bounds a batch while allowing one maximally escaped task body.
const TranslationBatchMaxBytes = 100 * (6*MaxBytes + 1024)

// Translation is a translation of one field at a particular source revision.
// Decision request entity IDs identify a specific request revision.
type Translation struct {
	Entity       string    `json:"entity"`
	EntityID     string    `json:"entity_id"`
	Field        string    `json:"field"`
	Lang         string    `json:"lang"`
	SourceSHA256 string    `json:"source_sha256"`
	Text         string    `json:"text"`
	Translator   string    `json:"translator"`
	UpdatedAt    time.Time `json:"updated_at"`
	Stale        bool      `json:"stale"`
}

func validTranslationEntityID(entity, id string) bool {
	if entity == "decision_request" {
		task, revision, ok := ParseDecisionRequestID(id)
		return ok && decisionRequestID(task, revision) == id
	}
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}

var translationHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func checkTranslation(t Translation) error {
	fields := map[string]map[string]bool{
		"task":             {"title": true, "body": true},
		"decision_request": {"question": true, "options": true},
		"task_comment":     {"body": true},
	}
	if _, ok := fields[t.Entity]; !ok {
		return fmt.Errorf("invalid translation: unknown entity %q", t.Entity)
	}
	if !fields[t.Entity][t.Field] {
		return fmt.Errorf("invalid translation: field %q is invalid for %s", t.Field, t.Entity)
	}
	if t.Lang != "ko" {
		return fmt.Errorf("invalid translation: lang must be ko")
	}
	if !validTranslationEntityID(t.Entity, t.EntityID) {
		return fmt.Errorf("invalid translation: entity_id is invalid for %s", t.Entity)
	}
	if !translationHash.MatchString(t.SourceSHA256) {
		return fmt.Errorf("invalid translation: source_sha256 must be a lowercase SHA-256 digest")
	}
	textLimit := MaxBytes
	if t.Entity == "task" && t.Field == "body" {
		textLimit = 3 * DocumentMaxBytes
	} else if t.Entity == "task_comment" && t.Field == "body" {
		textLimit = DocumentMaxBytes
	}
	if strings.TrimSpace(t.Text) == "" || len(t.Text) > textLimit || !utf8.ValidString(t.Text) || strings.ContainsRune(t.Text, 0) {
		return fmt.Errorf("invalid translation: text is empty, invalid, or too long")
	}
	if strings.TrimSpace(t.Translator) == "" || !validText(t.Translator, 128) {
		return fmt.Errorf("invalid translation: translator is invalid")
	}
	return guard.Reject(t.Text)
}

// SourceSHA256 returns the digest of the exact original text.
func SourceSHA256(original string) string {
	sum := sha256.Sum256([]byte(original))
	return hex.EncodeToString(sum[:])
}

// PutTranslation stores the digest supplied by the translator, not a digest of
// the current original, so a concurrent edit cannot make an old translation fresh.
func (s *Store) PutTranslation(ctx context.Context, t Translation) error {
	if err := checkTranslation(t); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO translations(entity,entity_id,field,lang,source_sha256,text,translator,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(entity,entity_id,field,lang) DO UPDATE SET source_sha256=EXCLUDED.source_sha256,text=EXCLUDED.text,translator=EXCLUDED.translator,updated_at=EXCLUDED.updated_at`, t.Entity, t.EntityID, t.Field, t.Lang, t.SourceSHA256, t.Text, t.Translator, time.Now().UTC())
	return err
}

// TranslationSource pairs the identity of a field with the original being read.
type TranslationSource struct {
	EntityID        string
	Field, Original string
}

// ListTranslations loads one page's translations and compares each with the
// original supplied by its reader. Missing rows are omitted.
func (s *Store) ListTranslations(ctx context.Context, entity string, sources []TranslationSource) ([]Translation, error) {
	ids := make([]string, 0, len(sources))
	originals := make(map[string]map[string]string, len(sources))
	for _, source := range sources {
		if originals[source.EntityID] == nil {
			originals[source.EntityID] = map[string]string{}
			ids = append(ids, source.EntityID)
		}
		originals[source.EntityID][source.Field] = source.Original
	}
	if len(ids) == 0 {
		return []Translation{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT entity,entity_id,field,lang,source_sha256,text,translator,updated_at FROM translations WHERE entity=$1 AND entity_id=ANY($2) AND lang='ko' ORDER BY entity_id,field`, entity, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Translation{}
	for rows.Next() {
		var t Translation
		if err := rows.Scan(&t.Entity, &t.EntityID, &t.Field, &t.Lang, &t.SourceSHA256, &t.Text, &t.Translator, &t.UpdatedAt); err != nil {
			return nil, err
		}
		original, ok := originals[t.EntityID][t.Field]
		if !ok {
			continue
		}
		t.Stale = t.SourceSHA256 != SourceSHA256(original)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if entity != "decision_request" {
		return out, nil
	}
	var taskIDs []int64
	for _, id := range ids {
		if task, _, ok := ParseDecisionRequestID(id); ok {
			taskIDs = append(taskIDs, task)
		}
	}
	if len(taskIDs) == 0 {
		return out, nil
	}
	taskRows, err := s.pool.Query(ctx, `SELECT refs FROM tasks WHERE id=ANY($1)`, taskIDs)
	if err != nil {
		return nil, err
	}
	defer taskRows.Close()
	for taskRows.Next() {
		var raw []byte
		var refs TaskRefs
		if err := taskRows.Scan(&raw); err != nil {
			return nil, err
		}
		if _, err := scanJSONB(raw, &refs); err != nil {
			return nil, err
		}
		request := refs.DecisionRequest
		if request == nil || request.Status != DecisionRequestOpen {
			continue
		}
		rendered, ok := RenderDecisionTemplate(*request, refs.DecisionOptions)
		if !ok {
			continue
		}
		fields, requested := originals[request.ID]
		if !requested {
			continue
		}
		encoded, err := json.Marshal(rendered.Options)
		if err != nil {
			return nil, err
		}
		for field, value := range map[string]string{"question": rendered.Question, "options": string(encoded)} {
			original, wanted := fields[field]
			if !wanted {
				continue
			}
			// Never attach a template to a different revision or source value.
			if field == "question" && original != request.Question {
				continue
			}
			if field == "options" {
				source, err := json.Marshal(refs.DecisionOptions)
				if err != nil {
					return nil, err
				}
				if original != string(source) {
					continue
				}
			}
			filtered := out[:0]
			for _, tr := range out {
				if tr.EntityID != request.ID || tr.Field != field {
					filtered = append(filtered, tr)
				}
			}
			out = filtered
			out = append(out, Translation{Entity: entity, EntityID: request.ID, Field: field, Lang: "ko", Text: value, SourceSHA256: SourceSHA256(original), Translator: "hk-template"})
		}
	}
	return out, taskRows.Err()
}

// PutTranslations validates the whole batch before writing it atomically.
func (s *Store) PutTranslations(ctx context.Context, translations []Translation) error {
	if len(translations) == 0 || len(translations) > 100 {
		return fmt.Errorf("invalid translation batch size")
	}
	for _, t := range translations {
		if err := checkTranslation(t); err != nil {
			return err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	for _, t := range translations {
		_, err = tx.Exec(ctx, `INSERT INTO translations(entity,entity_id,field,lang,source_sha256,text,translator,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(entity,entity_id,field,lang) DO UPDATE SET source_sha256=EXCLUDED.source_sha256,text=EXCLUDED.text,translator=EXCLUDED.translator,updated_at=EXCLUDED.updated_at`, t.Entity, t.EntityID, t.Field, t.Lang, t.SourceSHA256, t.Text, t.Translator, now)
		if err != nil {
			return err
		}
	}
	// translations has no feed trigger, but a task's YouTrack description
	// carries its Korean title (D5), so a title written after the task must
	// reach the feed (hk 3997): one task 'U' row per task, in this
	// transaction. The commit-time allocator gives it its sync_id.
	if _, err = tx.Exec(ctx, `INSERT INTO sync_log(model,model_id,action,at)
		SELECT 'task', id, 'U', clock_timestamp() FROM (SELECT DISTINCT unnest($1::text[]) AS id) titled ORDER BY id`,
		translatedTaskTitles(translations)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// translatedTaskTitles lists the task ids whose title translations holds.
func translatedTaskTitles(translations []Translation) []string {
	ids := []string{}
	for _, t := range translations {
		if t.Entity == "task" && t.Field == "title" {
			ids = append(ids, t.EntityID)
		}
	}
	return ids
}

type MissingTranslation struct {
	Entity       string `json:"entity"`
	EntityID     string `json:"entity_id"`
	Field        string `json:"field"`
	Lang         string `json:"lang"`
	Original     string `json:"original"`
	SourceSHA256 string `json:"source_sha256"`
	Stale        bool   `json:"stale"`
}

// MissingTaskTranslations returns missing and stale fields in task ID order.
func (s *Store) MissingTaskTranslations(ctx context.Context, limit int) ([]MissingTranslation, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid translation limit")
	}
	rows, err := s.pool.Query(ctx, `SELECT t.id, t.title, coalesce(d.body,''), coalesce(tt.source_sha256,''), coalesce(tb.source_sha256,'') FROM tasks t LEFT JOIN documents d ON d.key=split_part(t.body_doc,'#',1) LEFT JOIN translations tt ON tt.entity='task' AND tt.entity_id=t.id::text AND tt.field='title' AND tt.lang='ko' LEFT JOIN translations tb ON tb.entity='task' AND tb.entity_id=t.id::text AND tb.field='body' AND tb.lang='ko' WHERE t.state NOT IN (`+settledStatesSQL+`) ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MissingTranslation{}
	totalBytes := 0
	for rows.Next() {
		var id int64
		var title, body, titleHash, bodyHash string
		if err := rows.Scan(&id, &title, &body, &titleHash, &bodyHash); err != nil {
			return nil, err
		}
		for _, f := range []struct{ field, original, hash string }{{"title", title, titleHash}, {"body", body, bodyHash}} {
			if f.original == "" {
				continue
			}
			hash := SourceSHA256(f.original)
			if f.hash != hash {
				// Bound response size while keeping the first missing field reachable.
				if len(out) > 0 && totalBytes+len(f.original) > 2*DocumentMaxBytes {
					return out, nil
				}
				totalBytes += len(f.original)
				out = append(out, MissingTranslation{Entity: "task", EntityID: strconv.FormatInt(id, 10), Field: f.field, Lang: "ko", Original: f.original, SourceSHA256: hash, Stale: f.hash != ""})
			}
			if len(out) == limit {
				return out, nil
			}
		}
	}
	return out, rows.Err()
}

// MissingDecisionTranslations enumerates each structured decision revision.
func (s *Store) MissingDecisionTranslations(ctx context.Context, limit int) ([]MissingTranslation, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid translation limit")
	}
	rows, err := s.pool.Query(ctx, `SELECT d.source_ref,d.question,d.options,coalesce(q.source_sha256,''),coalesce(o.source_sha256,''),t.refs FROM decision_requests d LEFT JOIN tasks t ON t.id=d.task_id LEFT JOIN translations q ON q.entity='decision_request' AND q.entity_id=d.source_ref AND q.field='question' AND q.lang='ko' LEFT JOIN translations o ON o.entity='decision_request' AND o.entity_id=d.source_ref AND o.field='options' AND o.lang='ko' WHERE d.status='open' AND d.source IN ('task_request','linear') ORDER BY d.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MissingTranslation{}
	for rows.Next() {
		var id, question, qhash, ohash string
		var raw, refsRaw []byte
		if err := rows.Scan(&id, &question, &raw, &qhash, &ohash, &refsRaw); err != nil {
			return nil, err
		}
		var refs TaskRefs
		if _, err := scanJSONB(refsRaw, &refs); err != nil {
			return nil, err
		}
		if refs.DecisionRequest != nil && refs.DecisionRequest.ID == id && refs.DecisionRequest.Status == DecisionRequestOpen {
			if _, ok := RenderDecisionTemplate(*refs.DecisionRequest, refs.DecisionOptions); ok {
				continue
			}
		}
		options := ""
		if len(raw) > 0 && string(raw) != "null" {
			var parsed DecisionOptions
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(parsed)
			if err != nil {
				return nil, err
			}
			options = string(encoded)
		}
		for _, f := range []struct{ field, original, hash string }{{"question", question, qhash}, {"options", options, ohash}} {
			if f.original == "" {
				continue
			}
			hash := SourceSHA256(f.original)
			if f.hash != hash {
				out = append(out, MissingTranslation{Entity: "decision_request", EntityID: id, Field: f.field, Lang: "ko", Original: f.original, SourceSHA256: hash, Stale: f.hash != ""})
			}
			if len(out) == limit {
				return out, nil
			}
		}
	}
	return out, rows.Err()
}

// MissingCommentTranslations enumerates comments belonging to unsettled tasks.
func (s *Store) MissingCommentTranslations(ctx context.Context, limit int) ([]MissingTranslation, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid translation limit")
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id,c.body,coalesce(tr.source_sha256,'') FROM task_comments c JOIN tasks t ON t.id=c.task_id LEFT JOIN translations tr ON tr.entity='task_comment' AND tr.entity_id=c.id::text AND tr.field='body' AND tr.lang='ko' WHERE t.state NOT IN (`+settledStatesSQL+`) ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MissingTranslation{}
	for rows.Next() {
		var id int64
		var body, stored string
		if err := rows.Scan(&id, &body, &stored); err != nil {
			return nil, err
		}
		hash := SourceSHA256(body)
		if stored != hash {
			out = append(out, MissingTranslation{Entity: "task_comment", EntityID: strconv.FormatInt(id, 10), Field: "body", Lang: "ko", Original: body, SourceSHA256: hash, Stale: stored != ""})
		}
		if len(out) == limit {
			return out, nil
		}
	}
	return out, rows.Err()
}
