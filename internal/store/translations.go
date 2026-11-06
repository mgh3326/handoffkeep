package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

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

var decisionTranslationID = regexp.MustCompile(`^dr-[1-9][0-9]*-[1-9][0-9]*$`)

func validTranslationEntityID(entity, id string) bool {
	if entity == "decision_request" {
		return decisionTranslationID.MatchString(id)
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
	if strings.TrimSpace(t.Text) == "" || len(t.Text) > MaxBytes || !utf8.ValidString(t.Text) || strings.ContainsRune(t.Text, 0) {
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
	return out, rows.Err()
}
