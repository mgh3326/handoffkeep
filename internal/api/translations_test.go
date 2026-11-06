package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestTranslationBatchCap(t *testing.T) {
	h := Server{Tokens: Tokens{"tester": "tok"}}.Handler()
	for _, n := range []int{0, 101} {
		body, _ := json.Marshal(map[string]any{"translations": make([]store.Translation, n)})
		req := httptest.NewRequest(http.MethodPut, "/v1/translations", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("n=%d status=%d body=%s", n, rec.Code, rec.Body.String())
		}
	}
}

func TestTranslationsMissingAPI(t *testing.T) {
	h, st, pool := ledgerAPIServer(t)
	ctx := t.Context()
	ids := make([]int64, 3)
	for i := range ids {
		if err := pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at) VALUES('translation-test',$1,'implement','backlog','test',now(),now(),now()) RETURNING id`, "title "+strconv.Itoa(i)).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < 3; i++ {
		original := "title " + strconv.Itoa(i)
		if i == 2 {
			original = "old title"
		}
		x := store.Translation{Entity: "task", EntityID: strconv.FormatInt(ids[i], 10), Field: "title", Lang: "ko", SourceSHA256: store.SourceSHA256(original), Text: "번역", Translator: "test"}
		if err := st.PutTranslation(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	var out struct {
		Translations []store.MissingTranslation `json:"translations"`
	}
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/translations/missing?entity=task&state=open&limit=10", nil, &out); status != http.StatusOK {
		t.Fatal(status)
	}
	if len(out.Translations) != 2 || out.Translations[0].EntityID != strconv.FormatInt(ids[0], 10) || out.Translations[0].Stale || out.Translations[1].EntityID != strconv.FormatInt(ids[2], 10) || !out.Translations[1].Stale {
		t.Fatalf("unexpected: %+v", out.Translations)
	}
}

func TestTranslationBatchAPI(t *testing.T) {
	h, st, _ := ledgerAPIServer(t)
	rows := make([]store.Translation, 100)
	for i := range rows {
		rows[i] = store.Translation{Entity: "task", EntityID: strconv.Itoa(i + 1), Field: "title", Lang: "ko", SourceSHA256: store.SourceSHA256("source"), Text: "번역", Translator: "test"}
	}
	var result map[string]any
	if status := decisionAPICall(t, h, http.MethodPut, "/v1/translations", map[string]any{"translations": rows}, &result); status != http.StatusOK || result["count"] != float64(100) {
		t.Fatalf("status=%d result=%v", status, result)
	}
	rows[0].Text = "새 번역"
	rows[1].Lang = "en"
	if status := decisionAPICall(t, h, http.MethodPut, "/v1/translations", map[string]any{"translations": rows}, &result); status != http.StatusBadRequest {
		t.Fatalf("invalid batch status=%d", status)
	}
	stored, err := st.ListTranslations(t.Context(), "task", []store.TranslationSource{{EntityID: "1", Field: "title", Original: "source"}})
	if err != nil || len(stored) != 1 || stored[0].Text != "번역" {
		t.Fatalf("atomic batch: %+v err=%v", stored, err)
	}
}
