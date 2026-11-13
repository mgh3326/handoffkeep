package store

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// dotsVector is one row of testdata/dots_vectors.json: a fake comment and what
// the reconciling poller's own code produces for it. Both paths must agree byte
// for byte, or the second writer would rewrite the first one's document.
type dotsVector struct {
	ID         string  `json:"id"`
	CreatedAt  string  `json:"createdAt"`
	Body       string  `json:"body"`
	Identifier string  `json:"identifier"`
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	Kind       *string `json:"kind"`
	Summary    string  `json:"summary"`
	Follow     string  `json:"follow"`
	Key        string  `json:"key"`
	DocBody    string  `json:"docBody"`
	Text       string  `json:"text"`
}

func TestDotsFormatsMatchPoller(t *testing.T) {
	raw, err := os.ReadFile("testdata/dots_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []dotsVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range vectors {
		t.Run(v.Identifier, func(t *testing.T) {
			kind, dots := DotsKind(v.Body)
			if dots != (v.Kind != nil) || (dots && kind != *v.Kind) {
				t.Fatalf("kind=%q dots=%t want %v", kind, dots, v.Kind)
			}
			if got := dotsSummaryLine(v.Body); got != v.Summary {
				t.Fatalf("summary=%q want %q", got, v.Summary)
			}
			issue := DotsIssue{Identifier: v.Identifier, Title: v.Title, URL: v.URL}
			if DotsKindRecorded(kind) != (v.Key != "") {
				t.Fatalf("recorded=%t key=%q", DotsKindRecorded(kind), v.Key)
			}
			if v.Key == "" {
				return
			}
			c := DotsComment{ID: v.ID, CreatedAt: v.CreatedAt, Body: v.Body}
			key := dotsResultKey(issue, c)
			if key != v.Key {
				t.Fatalf("key=%q want %q", key, v.Key)
			}
			if got := dotsResultBody(kind, issue, c); got != v.DocBody {
				t.Fatalf("body=%q want %q", got, v.DocBody)
			}
			if got := dotsResultText(kind, key, issue, c); got != v.Text {
				t.Fatalf("text=%q want %q", got, v.Text)
			}
		})
	}
}

// The poller would fail on these; the outbox row must stay drainable instead.
func TestDotsLaneTextIsDrainable(t *testing.T) {
	text := dotsLaneText("a\tb\x7fc\u0085d" + strings.Repeat("가", RelayLaneEventMaxBytes))
	if len(text) > RelayLaneEventMaxBytes {
		t.Fatalf("len=%d", len(text))
	}
	for _, r := range text {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("control character %q in %q", r, text[:16])
		}
	}
	if !strings.HasPrefix(text, "a b c d") {
		t.Fatalf("prefix %q", text[:16])
	}
}

func dotsStore(t *testing.T) *Store { return schemaStore(t, "hkdots") }

const dotsTestIssueUUID = "0a1b2c3d-0000-4000-8000-0000000000aa"
