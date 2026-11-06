package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestTranslationsCLI(t *testing.T) {
	var received []store.Translation
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/translations":
			var x struct {
				Translations []store.Translation `json:"translations"`
			}
			if err := json.NewDecoder(r.Body).Decode(&x); err != nil {
				t.Error(err)
			}
			received = x.Translations
			_, _ = io.WriteString(w, `{"count":1}`)
		case r.Method == http.MethodGet && r.URL.Query().Get("entity") == "task" && r.URL.Query().Get("state") == "open":
			_, _ = io.WriteString(w, `{"translations":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer h.Close()
	rows := []store.Translation{{Entity: "task", EntityID: "1", Field: "title", Lang: "ko", SourceSHA256: store.SourceSHA256("hello"), Text: "안녕", Translator: "test"}}
	data, _ := json.Marshal(rows)
	path := filepath.Join(t.TempDir(), "translations.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"translations", "put", "--file", path, "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].Text != "안녕" {
		t.Fatalf("received=%+v", received)
	}
	out.Reset()
	if err := run([]string{"translations", "missing", "--entity", "task", "--state", "open", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"translations\":[]}\n" {
		t.Fatalf("output=%s", out.String())
	}
}

func TestDecisionTranslationPayload(t *testing.T) {
	var rows []store.Translation
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Translations []store.Translation `json:"translations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		rows = in.Translations
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"count":2}`)
	}))
	defer h.Close()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	ko := addDecisionTranslationFlags(fs)
	if err := fs.Parse([]string{"--question-ko", "배포할까요?", "--option-ko", "A|대기", "--option-ko", "B|배포"}); err != nil {
		t.Fatal(err)
	}
	input := store.DecisionRequestInput{Question: "Ship?", Options: store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "Wait"}, {Key: "B", Label: "Ship", Recommended: true}}, AllowFree: true}}
	client := &remote.Client{URL: h.URL, Token: "tok"}
	if err := putDecisionTranslations(context.Background(), client, store.DecisionRequest{ID: "dr-7-2", Question: "Ship?"}, input, ko); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(input.Options)
	if len(rows) != 2 || rows[0].EntityID != "dr-7-2" || rows[0].SourceSHA256 != store.SourceSHA256("Ship?") || rows[1].SourceSHA256 != store.SourceSHA256(string(original)) {
		t.Fatalf("rows=%+v", rows)
	}
	var translated store.DecisionOptions
	if err := json.Unmarshal([]byte(rows[1].Text), &translated); err != nil || translated.Options[1].Label != "배포" || !translated.Options[1].Recommended {
		t.Fatalf("options=%+v err=%v", translated, err)
	}
}
