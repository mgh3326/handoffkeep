package youtrack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRetriesServerErrorsAndFiveHundreds(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"1"}`))
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
	var out struct {
		ID string `json:"id"`
	}
	if err := c.get(context.Background(), "/api/issues/1", nil, &out); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || out.ID != "1" || len(waits) != 1 {
		t.Fatalf("attempts=%d out=%+v waits=%v", attempts.Load(), out, waits)
	}
}

func TestClientHonorsRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
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
		t.Fatalf("waits=%v", waits)
	}
}

func TestClientDoesNotRetryFourHundred(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad_request"}`))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "token", Sleep: func(_ context.Context, _ time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	err = c.get(context.Background(), "/api/x", nil, nil)
	if err == nil || ErrorCode(err) != "bad_request" {
		t.Fatalf("err=%v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts=%d", attempts.Load())
	}
}

func TestClientRedactsTokenFromTransportErrors(t *testing.T) {
	token := "fixture-token"
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("request rejected for token " + token)
	})
	c, err := New(Config{BaseURL: "https://example.test", Token: token, HTTPClient: &http.Client{Transport: rt}, Sleep: func(_ context.Context, _ time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	err = c.get(context.Background(), "/api/x", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("err=%q", err)
	}
}

func TestActivitiesPageSendsCursorAndEveryCategory(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"activities":[{"id":"a1"}],"afterCursor":"c2","hasAfter":true}`))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "token", Sleep: func(_ context.Context, _ time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.ActivitiesPage(context.Background(), "c1", 25)
	if err != nil {
		t.Fatal(err)
	}
	if page.AfterCursor != "c2" || !page.HasAfter || len(page.Activities) != 1 {
		t.Fatalf("page=%+v", page)
	}
	for _, want := range []string{"after=c1", "categories=CustomFieldCategory", "categories=CommentsCategory", "categories=IssueCreatedCategory", "%24top=25"} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q is missing %q", query, want)
		}
	}
}

func TestStateFromValueAcceptsKoreanAndEnglishNames(t *testing.T) {
	for _, value := range []string{"개발 중", "개발중", "developing", "DEVELOPING"} {
		if state, ok := StateFromValue(value); !ok || state != "developing" {
			t.Fatalf("%q mapped to %q ok=%v", value, state, ok)
		}
	}
	if _, ok := StateFromValue("nonsense"); ok {
		t.Fatal("unknown state accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
