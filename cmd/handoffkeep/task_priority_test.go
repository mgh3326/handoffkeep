package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveSetPriority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantID  int64
		wantP   int
		wantErr bool
	}{
		{"valid", []string{"12", "3"}, 12, 3, false},
		{"zero priority", []string{"12", "0"}, 12, 0, false},
		{"negative priority", []string{"12", "-1"}, 0, 0, true},
		{"bad id", []string{"0", "2"}, 0, 0, true},
		{"not a number id", []string{"abc", "2"}, 0, 0, true},
		{"too few args", []string{"12"}, 0, 0, true},
		{"too many args", []string{"12", "2", "extra"}, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, prio, err := resolveSetPriority(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%t", err, tc.wantErr)
			}
			if !tc.wantErr && (id != tc.wantID || prio != tc.wantP) {
				t.Fatalf("got id=%d prio=%d, want id=%d prio=%d", id, prio, tc.wantID, tc.wantP)
			}
		})
	}
}

func TestTaskSetPriorityCLI(t *testing.T) {
	var seenPath string
	var seenBody map[string]any
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.Header().Set("Content-Type", "application/json")
		prio := 0
		if p, ok := seenBody["priority"].(float64); ok {
			prio = int(p)
		}
		resp, _ := json.Marshal(map[string]any{"id": 42, "priority": prio})
		_, _ = w.Write(resp)
	}))
	defer h.Close()

	var out bytes.Buffer
	err := run([]string{"tasks", "set-priority", "42", "7", "--note", "raise", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err != nil {
		t.Fatalf("set-priority run: %v", err)
	}
	if seenPath != "/v1/tasks/42/priority" || seenBody["priority"] != float64(7) || seenBody["note"] != "raise" {
		t.Fatalf("seenPath=%q seenBody=%v", seenPath, seenBody)
	}
	if !strings.Contains(out.String(), `"priority":7`) {
		t.Fatalf("output %s lacks priority 7", out.String())
	}
}
