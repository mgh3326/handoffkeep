package youtrack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Point 1: a response over maxBodyBytes is an error. The first maxBodyBytes
// bytes are a valid empty JSON list, so a silent truncation decodes cleanly
// into zero comments and the caller cannot tell.
func TestOversizedResponseIsAnErrorNotATruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]" + strings.Repeat(" ", maxBodyBytes)))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "token", Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	comments, err := c.ListComments(context.Background(), "1-1", 0, 100)
	if err == nil {
		t.Fatalf("oversized response decoded as %d comments with no error", len(comments))
	}
}

// Point 2: a redirect to another origin is refused, and the request never
// reaches the other origin. A same-origin redirect still works.
func TestRedirectToAnotherOriginIsRefused(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/x", http.StatusFound)
	}))
	defer origin.Close()
	c, err := New(Config{BaseURL: origin.URL, Token: "token", Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	err = c.get(context.Background(), "/api/issues/1", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "other origin") {
		t.Fatalf("cross-origin redirect: err = %v, want a refusal", err)
	}
	if n := otherHits.Load(); n != 0 {
		t.Fatalf("other origin was contacted %d times", n)
	}
}

func TestSameOriginRedirectIsFollowedWithToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/old" {
			http.Redirect(w, r, "/api/new", http.StatusFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "token", Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.get(context.Background(), "/api/old", nil, nil); err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	if gotAuth != "Bearer token" {
		t.Fatalf("Authorization on the same-origin hop = %q", gotAuth)
	}
}

// Point 3: a Retry-After whose seconds overflow the duration is capped, not
// turned into a negative or tiny wait.
func TestRetryAfterCapsOverflowingSeconds(t *testing.T) {
	for _, v := range []string{"99999999999999999", "1000000000000", "99999999999999999999999999"} {
		if got := retryAfter(v); got != maxRetryWait {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, maxRetryWait)
		}
	}
}

func TestRetryAfterHTTPDateIsCapped(t *testing.T) {
	when := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	if got := retryAfter(when); got != maxRetryWait {
		t.Fatalf("retryAfter(far date) = %v, want %v", got, maxRetryWait)
	}
}

// A 503 on an idempotent read honours Retry-After as well as a 429 does.
func TestServiceUnavailableGETHonoursRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var waits []time.Duration
	c, err := New(Config{BaseURL: srv.URL, Token: "token", Sleep: func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.get(context.Background(), "/api/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != 2*time.Second {
		t.Fatalf("waits=%v, want [2s]", waits)
	}
}

// A cancelled context ends a long Retry-After wait at once.
func TestRetryAfterWaitEndsWhenContextIsCancelled(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.get(ctx, "/api/x", nil, nil); err == nil {
		t.Fatal("want an error when the context ends during the wait")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second || attempts.Load() != 1 {
		t.Fatalf("elapsed=%v attempts=%d: the wait ignored the context", elapsed, attempts.Load())
	}
}

func noSleep(context.Context, time.Duration) error { return nil }
