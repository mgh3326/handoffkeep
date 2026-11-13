package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBenchCatalogWritersIncludeOperatorDesk pins the writer set: operator,
// operator-desk and panewire-design pass the client check (and reach body
// validation), any other client is refused with operator_required before the
// body is read.
func TestBenchCatalogWritersIncludeOperatorDesk(t *testing.T) {
	h := httptest.NewServer(Server{Tokens: Tokens{"operator": "op-token", "operator-desk": "desk-token", "panewire-design": "pw-token", "peer": "peer-token"}}.Handler())
	defer h.Close()
	for _, path := range []string{"/v1/bench/catalog"} {
		for token, want := range map[string]int{"op-token": http.StatusBadRequest, "desk-token": http.StatusBadRequest, "pw-token": http.StatusBadRequest, "peer-token": http.StatusForbidden} {
			req, err := http.NewRequest(http.MethodPut, h.URL+path, strings.NewReader(`{"nope":1}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := h.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != want {
				t.Fatalf("%s with %s: status=%d, want %d", path, token, resp.StatusCode, want)
			}
		}
	}
}
