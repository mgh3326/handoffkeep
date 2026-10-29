package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const quotaFreeKey = "devin:0123456789abcdef"
const quotaFreeHolder = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// The two contract bodies are accepted, stored, returned unchanged by latest,
// and deduplicated on free_bucket; malformed bodies and old-client bodies keep
// their existing results.
func TestQuotaFreeBucketPostgres(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL integration tests")
	}
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewServer(api.Server{
		Service:       api.Service{Store: s},
		Tokens:        api.Tokens{"writer": "writer-token"},
		QuotaAccounts: api.QuotaAccounts{"writer": {quotaFreeKey: true}},
	}.Handler())
	defer h.Close()
	c := h.Client()
	base := h.URL + "/v1/quota/accounts/" + quotaFreeKey
	put := func(rawObs string) (int, store.QuotaObservation) {
		body := `{"holder":"` + quotaFreeHolder + `","observation":` + rawObs + `}`
		res := request(t, c, "PUT", base+"/observation", "writer-token", json.RawMessage(body))
		defer res.Body.Close()
		var x store.QuotaObservation
		raw, _ := io.ReadAll(res.Body)
		_ = json.Unmarshal(raw, &x)
		return res.StatusCode, x
	}
	latest := func() store.QuotaLatest {
		res := request(t, c, "GET", base+"/latest", "writer-token", nil)
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var x store.QuotaLatest
		if err := json.Unmarshal(raw, &x); err != nil || res.StatusCode != 200 {
			t.Fatalf("latest: %d %s %v", res.StatusCode, raw, err)
		}
		if x.Observation != nil && x.Observation.FreeBucket != nil && len(x.Observation.Buckets) == 0 && !bytes.Contains(raw, []byte(`"buckets":[]`)) {
			t.Fatalf("empty buckets not returned as []: %s", raw)
		}
		return x
	}
	res := request(t, c, "POST", base+"/lease", "writer-token", map[string]any{"holder": quotaFreeHolder, "ttl_s": 120})
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("lease: %d", res.StatusCode)
	}
	defer func() {
		r := request(t, c, "DELETE", base+"/lease", "writer-token", map[string]any{"holder": quotaFreeHolder})
		r.Body.Close()
	}()

	// The contract bodies with timestamps near now: measured_at may not lead
	// the server clock, and the latest pointer only moves forward.
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	stamp := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339) }
	available := `{"id":0,"kind":"auto","measured_at":"` + stamp(0) + `","buckets":[],"free_bucket":{"state":"available"}}`
	availableLater := `{"id":0,"kind":"auto","measured_at":"` + stamp(time.Minute) + `","buckets":[],"free_bucket":{"state":"available"}}`
	exhausted := func(reset time.Duration) string {
		return `{"id":0,"kind":"auto","measured_at":"` + stamp(2*time.Minute) + `","buckets":[],"free_bucket":{"state":"exhausted","resets_at":"` + stamp(reset) + `"}}`
	}
	code, first := put(available)
	if code != 201 || first.FreeBucket == nil || first.FreeBucket.State != "available" {
		t.Fatalf("available: %d %+v", code, first)
	}
	code, again := put(availableLater)
	if code != 200 || again.ID != first.ID {
		t.Fatalf("available dedupe: %d id=%d want=%d", code, again.ID, first.ID)
	}
	code, ex := put(exhausted(time.Hour))
	if code != 201 || ex.ID == first.ID || ex.FreeBucket == nil || ex.FreeBucket.State != "exhausted" {
		t.Fatalf("exhausted: %d %+v", code, ex)
	}
	got := latest().Observation
	if got == nil || got.ID != ex.ID || got.FreeBucket == nil || got.FreeBucket.State != "exhausted" || got.FreeBucket.ResetsAt == nil || !got.FreeBucket.ResetsAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("latest exhausted: %+v", got)
	}
	// A different reset time is a different value, not a duplicate.
	code, moved := put(exhausted(2 * time.Hour))
	if code != 201 || moved.ID == ex.ID {
		t.Fatalf("moved reset: %d id=%d prev=%d", code, moved.ID, ex.ID)
	}
	// Rejections leave the latest pointer where it was.
	for _, bad := range []string{
		`{"kind":"auto","measured_at":"` + stamp(3*time.Minute) + `","buckets":[]}`,
		`{"kind":"auto","measured_at":"` + stamp(3*time.Minute) + `","buckets":[],"free_bucket":{"state":"exhausted"}}`,
		`{"kind":"auto","measured_at":"` + stamp(3*time.Minute) + `","buckets":[],"free_bucket":{"state":"available","used_pct":3}}`,
	} {
		if code, _ := put(bad); code != 400 {
			t.Fatalf("bad %s: %d", bad, code)
		}
	}
	if got := latest().Observation; got == nil || got.ID != moved.ID {
		t.Fatalf("latest moved by rejection: %+v", got)
	}
	// Old clients: a percentage-only observation carries no free_bucket.
	code, pct := put(`{"kind":"auto","measured_at":"` + stamp(4*time.Minute) + `","buckets":[{"window":"5h","used_pct":40,"scope":"account"}]}`)
	if code != 201 || pct.FreeBucket != nil || len(pct.Buckets) != 1 {
		t.Fatalf("percentage-only: %d %+v", code, pct)
	}
	if got := latest().Observation; got == nil || got.FreeBucket != nil || got.ID != pct.ID {
		t.Fatalf("latest percentage-only: %+v", got)
	}
}
