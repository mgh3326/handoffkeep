package ui

import (
	"net/http"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// benchCatalogResponse is the /ui/api/bench/catalog envelope. Catalog carries
// store.BenchCatalogEntry rows verbatim — the same wire shape the deployed
// GET /v1/bench/catalog (#592) emits — plus a server snapshot timestamp so the
// console can say when the data was read; the /v1 body has no clock of its own.
type benchCatalogResponse struct {
	GeneratedAt time.Time                 `json:"generated_at"`
	Catalog     []store.BenchCatalogEntry `json:"catalog"`
}

// benchCatalog serves the read-only grade table behind the console's existing
// CF Access session. It mirrors the /v1 query contract: ?pool= selects one
// subscription ladder (dropping consult_only rows, per ListBenchCatalog) and
// ?include_retired=1|true adds retired rows. There is deliberately no write
// route — catalog writes stay on the operator-token PUT /v1/bench/catalog.
// The catalog is operator reading material (profile/pool assignments and
// decision provenance), so like /ui/api/board/doc it refuses service
// principals even though the /v1 route admits any bearer token.
func (h *Handler) benchCatalog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pool := strings.TrimSpace(q.Get("pool"))
	if pool != "" && !boardNameRE.MatchString(pool) {
		http.Error(w, "invalid catalog query", http.StatusBadRequest)
		return
	}
	includeRetired := q.Get("include_retired") == "1" || q.Get("include_retired") == "true"
	catalog, err := h.store.ListBenchCatalog(r.Context(), pool, includeRetired)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	writeBoardJSON(w, benchCatalogResponse{GeneratedAt: time.Now().UTC(), Catalog: catalog})
}

// grades serves the read-only grade table page. Like /ui/fleet it is only a
// mount point: all data reaches the browser through GET /ui/api/bench/catalog
// and the page carries no CSRF token because it has no write form.
func (h *Handler) grades(w http.ResponseWriter, r *http.Request) {
	h.render(w, "grades_page", nil)
}
