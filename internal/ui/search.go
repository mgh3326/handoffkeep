package ui

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
)

// Console-wide task search (2410 §5 AC4). Read-only: the route calls
// store.Search with scope "tasks" and nothing else. Title and snippet are
// forwarded verbatim — the store's snippet carries ts_headline's "<b>"/"</b>"
// markers around unescaped source text, so the console must treat both as
// text and rebuild only the highlight (GlobalSearch.tsx).

// searchQueryMax mirrors the store's own bound on q so an over-long query is
// answered as invalid_query, never as a store failure.
const searchQueryMax = 512

var searchIDQueryRE = regexp.MustCompile(`^#?([0-9]+)$`)

type searchHit struct {
	ID      int64  `json:"id"`
	State   string `json:"state"`
	Lane    string `json:"lane"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	// Exact marks the id-lookup hit for a "#<n>" or bare-number query; the
	// store always places it first.
	Exact bool `json:"exact,omitempty"`
}

type searchResponse struct {
	Query   string      `json:"query"`
	Scope   string      `json:"scope"`
	Results []searchHit `json:"results"`
	// HasMore is true when the store cut the page at its cap: the rows shown
	// are not every match.
	HasMore bool `json:"has_more"`
	Limit   int  `json:"limit"`
}

// search serves GET /ui/api/search?q=&scope=tasks[&limit=]. Service
// principals are refused like /ui/api/board/doc: comment-body snippets are
// operator reading material.
func (h *Handler) search(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	if identity.ServiceName != "" {
		commentError(w, http.StatusForbidden, "forbidden")
		return
	}
	query := r.URL.Query()
	scope := query.Get("scope")
	if scope == "" {
		scope = "tasks"
	}
	if scope != "tasks" {
		commentError(w, http.StatusBadRequest, "invalid_scope")
		return
	}
	q := query.Get("q")
	if strings.TrimSpace(q) == "" || len(q) > searchQueryMax || !utf8.ValidString(q) {
		commentError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	limit := 20
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 50 {
			commentError(w, http.StatusBadRequest, "invalid_limit")
			return
		}
		limit = parsed
	}
	xs, err := h.store.Search(r.Context(), q, "tasks", "", limit)
	if err != nil {
		commentError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	exactID := int64(-1)
	if m := searchIDQueryRE.FindStringSubmatch(strings.TrimSpace(q)); m != nil {
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			exactID = id
		}
	}
	response := searchResponse{Query: q, Scope: scope, Results: make([]searchHit, 0, len(xs)), Limit: limit}
	for i, x := range xs {
		id, err := strconv.ParseInt(x.Key, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		if x.Truncated {
			response.HasMore = true
		}
		response.Results = append(response.Results, searchHit{
			ID:      id,
			State:   firstRef(x.Refs["state"]),
			Lane:    x.Session,
			Kind:    x.Kind,
			Title:   strings.ToValidUTF8(x.Title, "�"),
			Snippet: strings.ToValidUTF8(x.Snippet, "�"),
			Exact:   i == 0 && id == exactID,
		})
	}
	writeBoardJSON(w, response)
}

func firstRef(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
