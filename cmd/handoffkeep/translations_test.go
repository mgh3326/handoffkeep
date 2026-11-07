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
	"strings"
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
	prepared, err := decisionTranslations(input, ko)
	if err != nil {
		t.Fatal(err)
	}
	if err := putDecisionTranslations(context.Background(), client, store.DecisionRequest{ID: "dr-7-2", Question: "Ship?"}, prepared); err != nil {
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

func TestCreateOutputsSurviveTranslationFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		primary  string
		expected map[string]any
	}{
		{"comment", []string{"tasks", "comment", "42", "--body", "hello", "--body-ko", "안녕"}, `{"id":7,"task_id":42,"body":"hello","author":"node","created_at":"2026-09-21T00:00:00Z"}`, map[string]any{"id": float64(7)}},
		{"task", []string{"tasks", "add", "--lane", "d", "--title", "hello", "--title-ko", "안녕", "--project", "experiment"}, `{"id":9,"title":"hello","project":"experiment"}`, map[string]any{"id": float64(9)}},
		{"decision", []string{"tasks", "decision-request", "618", "--question", "q", "--question-ko", "질문", "--option", "A|x", "--default-action", "보류"}, decisionRequestOKBody, map[string]any{"request_id": "dr-618-1", "recorded": true}},
		{"ask", []string{"decisions", "ask", "618", "--question", "q", "--question-ko", "질문", "--option", "A|x", "--default-action", "보류", "--linear"}, decisionAskOKBody, map[string]any{"request_id": "dr-618-1", "issue_created": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts, puts := 0, 0
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/translations" {
					puts++
					http.Error(w, `{"error":"internal_error"}`, http.StatusInternalServerError)
					return
				}
				posts++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, tc.primary)
			}))
			defer h.Close()
			var out bytes.Buffer
			args := append(append([]string{}, tc.args...), "--url", h.URL, "--token", "tok")
			if err := run(args, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			for key, want := range tc.expected {
				if got[key] != want {
					t.Fatalf("%s: got=%v want=%v", key, got[key], want)
				}
			}
			if got["translation_error"] != "translation_write_failed" || posts != 1 || puts != 1 {
				t.Fatalf("got=%v posts=%d puts=%d", got, posts, puts)
			}
			if tc.name == "decision" && got["notify"] == "" {
				t.Fatalf("notify missing: %v", got)
			}
			if tc.name == "ask" && got["issue"] == nil {
				t.Fatalf("issue missing: %v", got)
			}
		})
	}
}

func TestDecisionTranslationInvalidOptionsDoNotRecord(t *testing.T) {
	for _, command := range [][]string{{"tasks", "decision-request", "618"}, {"decisions", "ask", "618"}, {"decisions", "ask", "618", "--linear"}} {
		for _, tc := range []struct {
			name    string
			options []string
			reason  string
		}{
			{"malformed", []string{"--option-ko", "invalid"}, "option"},
			{"count", []string{"--option-ko", "A|하나"}, "one translation per option"},
			{"order", []string{"--option-ko", "B|둘", "--option-ko", "A|하나"}, "keys must match"},
		} {
			t.Run(strings.Join(command, "_")+"_"+tc.name, func(t *testing.T) {
				requests := 0
				h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer h.Close()
				args := append([]string{}, command...)
				args = append(args, "--question", "q", "--option", "A|one", "--option", "B|two", "--default-action", "wait")
				args = append(args, tc.options...)
				args = append(args, "--url", h.URL, "--token", "tok")
				err := run(args, io.Discard, io.Discard)
				if err == nil || !strings.Contains(err.Error(), "NOT recorded") || !strings.Contains(err.Error(), tc.reason) || requests != 0 {
					t.Fatalf("err=%v requests=%d", err, requests)
				}
			})
		}
	}
}
