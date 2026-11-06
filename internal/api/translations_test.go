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
