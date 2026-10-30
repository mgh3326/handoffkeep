package ui

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Documents page (hk 2245). The console had exact-key document views only;
// this is the browse surface behind them. Listing responses never carry whole
// bodies — the store reads a bounded head so the first heading can stand in as
// a title, and the reader fetches the selected body through the existing
// /ui/api/board/doc. Both routes are operator reading material, so service
// principals are refused like /ui/api/board/doc.

const (
	docsDefaultLimit = 100
	docsMaxLimit     = 200
)

var atxHeadingRE = regexp.MustCompile(`^#{1,6}[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$`)

type docsItem struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	Session   string `json:"session"`
	Size      int64  `json:"size"`
	UpdatedAt string `json:"updated_at"`
}

type docsResponse struct {
	Prefix     string     `json:"prefix"`
	Limit      int        `json:"limit"`
	Documents  []docsItem `json:"documents"`
	HasMore    bool       `json:"has_more"`
	NextBefore string     `json:"next_before,omitempty"`
}

type docsPrefixesResponse struct {
	Prefixes []docsPrefix `json:"prefixes"`
}

type docsPrefix struct {
	Prefix string `json:"prefix"`
	Count  int64  `json:"count"`
}

// docsList serves GET /ui/api/docs?prefix=&before=&limit=. Documents are
// ordered newest key first; next_before is the cursor for the following page
// and is present only when has_more is true.
func (h *Handler) docsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	before := q.Get("before")
	if !validDocsText(prefix) {
		writeJSONError(w, http.StatusBadRequest, "invalid_prefix")
		return
	}
	if before != "" && !validDocsText(before) {
		writeJSONError(w, http.StatusBadRequest, "invalid_cursor")
		return
	}
	limit := docsDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > docsMaxLimit {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit")
			return
		}
		limit = parsed
	}
	// One row past the page distinguishes "exactly full" from "truncated".
	summaries, err := h.store.ListDocumentSummariesByPrefix(r.Context(), prefix, before, limit+1)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	response := docsResponse{Prefix: prefix, Limit: limit, Documents: make([]docsItem, 0, limit)}
	if len(summaries) > limit {
		response.HasMore = true
		summaries = summaries[:limit]
	}
	for _, summary := range summaries {
		response.Documents = append(response.Documents, docsItem{
			Key:       strings.ToValidUTF8(summary.Key, "\uFFFD"),
			Title:     documentTitle(summary.Head, summary.Size > int64(len(summary.Head))),
			Kind:      strings.ToValidUTF8(summary.Kind, "\uFFFD"),
			Session:   strings.ToValidUTF8(summary.Session, "\uFFFD"),
			Size:      summary.Size,
			UpdatedAt: summary.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	if response.HasMore && len(summaries) > 0 {
		// The cursor must be the stored key, not the display key: the store
		// compares it against the raw column value.
		response.NextBefore = summaries[len(summaries)-1].Key
	}
	writeBoardJSON(w, response)
}

// docsPrefixes serves GET /ui/api/docs/prefixes — top-level key prefixes with
// whole-key-space counts for the browse tree.
func (h *Handler) docsPrefixes(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.ListDocumentPrefixCounts(r.Context())
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	response := docsPrefixesResponse{Prefixes: make([]docsPrefix, 0, len(rows))}
	for _, row := range rows {
		response.Prefixes = append(response.Prefixes, docsPrefix{
			Prefix: strings.ToValidUTF8(row.Prefix, "\uFFFD"),
			Count:  row.Count,
		})
	}
	writeBoardJSON(w, response)
}

// docsPage serves the browse page. Like /ui/grades it is only a mount point:
// all data reaches the browser through the two API routes above.
func (h *Handler) docsPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, "docs_page", nil)
}

func validDocsText(value string) bool {
	return len(value) <= 512 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// documentTitle returns the first ATX heading in a body head, skipping a YAML
// front-matter block. truncated means the head is only the body's leading
// characters, so the final line may be cut: a heading without its terminating
// newline is then not a title. The key still names such a document.
func documentTitle(head string, truncated bool) string {
	head = strings.TrimPrefix(strings.ToValidUTF8(head, "\uFFFD"), "\ufeff")
	lines := strings.Split(head, "\n")
	if truncated && !strings.HasSuffix(head, "\n") && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	start := 0
	if len(lines) > 0 && strings.TrimSpace(strings.TrimRight(lines[0], "\r")) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(strings.TrimRight(lines[i], "\r")) == "---" {
				start = i + 1
				break
			}
		}
	}
	for _, line := range lines[start:] {
		match := atxHeadingRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if match == nil {
			continue
		}
		title := strings.TrimSpace(match[1])
		if title == "" {
			continue
		}
		return truncateTitle(title)
	}
	return ""
}

func truncateTitle(title string) string {
	const maxRunes = 200
	runes := []rune(title)
	if len(runes) <= maxRunes {
		return title
	}
	return string(runes[:maxRunes]) + "…"
}
