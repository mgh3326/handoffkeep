package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/capabilities"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// resetCapabilitiesCache forgets the process-wide answers. httptest reuses
// ports, so a test server must not inherit another test's capabilities.
func resetCapabilitiesCache() {
	capsCache.Lock()
	capsCache.byOrigin = map[string]serverCaps{}
	capsCache.flight = map[string]*capabilitiesFlight{}
	capsCache.Unlock()
}

// answerCurrentCapabilities writes the full feature list when the request is
// GET /v1/capabilities, the way a current server does, and reports whether it
// did.
func answerCurrentCapabilities(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/capabilities" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"protocol_version": capabilities.ProtocolVersion,
		"features":         capabilities.Features,
	})
	return true
}

// capsServer answers the capabilities route with the given body (or 404 when
// legacy is set) and counts claim posts, so a test can see whether a shape
// reached the server.
func capsServer(t *testing.T, features []string, legacy bool, claims *atomic.Int32) Client {
	t.Helper()
	resetCapabilitiesCache()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			if legacy {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_version": capabilities.ProtocolVersion, "features": features})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/claim") {
			if claims != nil {
				claims.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(store.Task{ID: 7, State: "developing", ClaimedBy: "worker", Refs: store.TaskRefs{JobID: "job-1"}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return Client{URL: server.URL, Token: "t"}
}

// A server that does not list claim_job_id is refused before any claim is
// sent, with the feature named.
func TestClaimRefusesFeatureTheServerDoesNotList(t *testing.T) {
	var claims atomic.Int32
	features := []string{capabilities.FeatureClaimNoJob}
	c := capsServer(t, features, false, &claims)
	_, err := c.ClaimTask(context.Background(), 7, "worker", "job-1", "")
	if err == nil || !strings.HasPrefix(err.Error(), "server_lacks_feature:claim_job_id") {
		t.Fatalf("err=%v, want server_lacks_feature:claim_job_id", err)
	}
	if n := claims.Load(); n != 0 {
		t.Fatalf("claim posts=%d, want 0: the shape must not be sent", n)
	}
}

// A current server lists the feature, and the claim goes out.
func TestClaimSendsWhenServerListsFeature(t *testing.T) {
	var claims atomic.Int32
	c := capsServer(t, capabilities.Features, false, &claims)
	if _, err := c.ClaimTask(context.Background(), 7, "worker", "job-1", ""); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if n := claims.Load(); n != 1 {
		t.Fatalf("claim posts=%d, want 1", n)
	}
}

// A 404 capabilities route is a legacy server: the claim goes out as it always
// did, and the server's own answer is judged as before.
func TestLegacyServerSendsAsBefore(t *testing.T) {
	var claims atomic.Int32
	c := capsServer(t, nil, true, &claims)
	if _, err := c.ClaimTask(context.Background(), 7, "worker", "job-1", ""); err != nil {
		t.Fatalf("claim against legacy server: %v", err)
	}
	if n := claims.Load(); n != 1 {
		t.Fatalf("claim posts=%d, want 1", n)
	}
}

// The list is read once per process, not once per request.
func TestCapabilitiesReadOncePerOrigin(t *testing.T) {
	resetCapabilitiesCache()
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			reads.Add(1)
		}
		answerCurrentCapabilities(w, r)
		if r.URL.Path == "/v1/tasks/7/claim" {
			_ = json.NewEncoder(w).Encode(store.Task{ID: 7, State: "developing", ClaimedBy: "worker", Refs: store.TaskRefs{JobID: "job-1"}})
		}
	}))
	t.Cleanup(server.Close)
	c := Client{URL: server.URL, Token: "t"}
	for i := 0; i < 3; i++ {
		if _, err := c.ClaimTask(context.Background(), 7, "worker", "job-1", ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("capabilities reads=%d, want 1", n)
	}
}

func TestConcurrentFirstCallsShareCapabilitiesRead(t *testing.T) {
	resetCapabilitiesCache()
	const callers = 12
	var reads atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			reads.Add(1)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			answerCurrentCapabilities(w, r)
			return
		}
		if r.URL.Path == "/v1/tasks/7/claim" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(store.Task{ID: 7, State: "developing", ClaimedBy: "worker", Refs: store.TaskRefs{JobID: "job-1"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	c := Client{URL: server.URL, Token: "t"}
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, err := c.ClaimTask(context.Background(), 7, "worker", "job-1", "")
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	<-entered
	close(release)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent claim: %v", err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("concurrent capabilities reads=%d, want 1", n)
	}
}

// A refused capabilities read is an error, not a guess, and is not cached.
func TestCapabilitiesTransportFailureIsNotCached(t *testing.T) {
	resetCapabilitiesCache()
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	c := Client{URL: url, Token: "t"}
	_, err := c.ClaimTask(context.Background(), 7, "worker", "job-1", "")
	if err == nil || !strings.HasPrefix(err.Error(), "capabilities_unavailable:") {
		t.Fatalf("err=%v, want capabilities_unavailable", err)
	}
	capsCache.Lock()
	_, cached := capsCache.byOrigin[url]
	capsCache.Unlock()
	if cached {
		t.Fatal("a failed read was cached")
	}
}

// Every gated call refuses a shape the server does not list, names it, and
// sends nothing; the same call sends once the feature is listed.
func TestGatedCallsRefuseUnlistedShapesWithoutSending(t *testing.T) {
	cases := []struct {
		name    string
		feature string
		call    func(c Client) error
	}{
		{"transition no_job", capabilities.FeatureTransitionNoJob, func(c Client) error {
			_, err := c.TransitionTask(context.Background(), 7, "developing", "", nil, "no job")
			return err
		}},
		{"disposition project", capabilities.FeatureDispositionProject, func(c Client) error {
			_, _, err := c.CreateDisposition(context.Background(), store.DispositionInput{Project: "p"})
			return err
		}},
		{"resolve no_job", capabilities.FeatureResolveNoJob, func(c Client) error {
			_, err := c.ResolveDecision(context.Background(), "decision", 1, "by", "answer", "", false, "no job")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetCapabilitiesCache()
			// The server lists everything except the feature under test.
			var listed []string
			for _, f := range capabilities.Features {
				if f != tc.feature {
					listed = append(listed, f)
				}
			}
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/capabilities" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"protocol_version": capabilities.ProtocolVersion, "features": listed})
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(store.Task{ID: 7, State: "developing", ClaimedBy: "worker", Refs: store.TaskRefs{JobID: "job-1"}})
			}))
			defer server.Close()
			err := tc.call(Client{URL: server.URL, Token: "t"})
			if err == nil || !strings.HasPrefix(err.Error(), "server_lacks_feature:"+tc.feature) {
				t.Fatalf("err=%v, want server_lacks_feature:%s", err, tc.feature)
			}
			if n := posts.Load(); n != 0 {
				t.Fatalf("posts=%d, want 0", n)
			}
		})
	}
}

// A transient 5xx on the capabilities read is not "legacy": the gated call is
// refused before any shape is sent, the failure is not cached, and the next
// call asks again.
func TestCapabilitiesServerErrorRefusesAndRetries(t *testing.T) {
	resetCapabilitiesCache()
	var reads, claims atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			if reads.Add(1) == 1 {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			answerCurrentCapabilities(w, r)
			return
		}
		claims.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.Task{ID: 7, State: "developing", ClaimedBy: "worker", Refs: store.TaskRefs{JobID: "job-1"}})
	}))
	defer server.Close()
	c := Client{URL: server.URL, Token: "t"}
	_, err := c.ClaimTask(context.Background(), 7, "worker", "job-1", "")
	if err == nil || !strings.HasPrefix(err.Error(), "capabilities_unavailable:") {
		t.Fatalf("err=%v, want capabilities_unavailable", err)
	}
	if n := claims.Load(); n != 0 {
		t.Fatalf("claim posts=%d after a 5xx read, want 0", n)
	}
	if _, err := c.ClaimTask(context.Background(), 7, "worker", "job-1", ""); err != nil {
		t.Fatalf("retry after 5xx: %v", err)
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("capabilities reads=%d, want 2 (the 5xx was not cached)", n)
	}
}
