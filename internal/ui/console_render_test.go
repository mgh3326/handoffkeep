package ui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Render the actual embedded route documents. An optional output directory
// lets the browser capture the exact same fictional fixtures without a DB.
func TestConsoleRouteDocuments(t *testing.T) {
	h := newAssetTestHandler(t, "capture")
	task := store.Task{ID: 101, Lane: "example-project", Title: "Ship a clear operator console", State: "developing", AwaitingDecision: true}
	decision := decisionData{CSRF: "fixture-token", CanWrite: true, Tasks: []taskDecisionView{{Type: "task", ID: 101, Task: task, Question: "Keep the compact list as the default?", Options: []string{"Yes, keep the list", "Use the board"}, Index: 0, CanWrite: true}}}
	pages := []struct {
		name, template string
		data           any
	}{
		{"board", "board_page", boardPageData{CSRF: "fixture-token"}},
		{"inbox", "dashboard_page", nil}, {"fleet", "fleet_page", nil},
		{"docs", "docs_page", nil}, {"deploys", "deploys_page", nil}, {"grades", "grades_page", nil},
		{"decisions", "page", pageData{Title: "Decisions", Page: "decisions", Body: decision}},
		{"timeline", "page", pageData{Title: "Timeline", Page: "timeline", Body: timelineData{Events: []store.RelayEvent{{ID: 1, OwnerLane: "example-project", Kind: "ready", Text: "READY · console layout ready for review", ReceivedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}}}}},
		{"compose", "page", pageData{Title: "Compose", Page: "compose", Body: composeData{CSRF: "fixture-token", Lanes: []string{"example-project"}}}},
		{"document", "page", pageData{Title: "Document", Page: "document", Body: struct{ Key, Body string }{"guide/console", "Console guide\nUse the palette to find tasks and documents."}}},
	}
	for _, p := range pages {
		t.Run(p.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.render(rec, p.template, p.data)
			body := rec.Body.String()
			if rec.Code != 200 || !strings.Contains(body, "/console/tokens.css") || !strings.Contains(body, "/console/AppShell.css") || !strings.Contains(body, "type=\"module\"") {
				t.Fatalf("missing shell assets: %s", body)
			}
			if strings.Contains(body, "<style>") || strings.Contains(body, "<h1>Fleet console</h1>") {
				t.Fatal("legacy page layout survived")
			}
			if dir := os.Getenv("HK_CONSOLE_CAPTURE_DIR"); dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, p.name+".html"), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
