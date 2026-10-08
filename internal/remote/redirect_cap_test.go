package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// The wrapper's own ten-hop cap is reported as ErrTooManyRedirects by
// identity on every request form that follows redirects.
func TestRedirectCapIsSentinel(t *testing.T) {
	for name := range allRequestForms(Client{}) {
		if name == "presign" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Location", r.URL.RequestURI())
				w.WriteHeader(http.StatusFound)
			})
			c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			e := allRequestForms(c)[name](ctx)
			if !errors.Is(e, ErrTooManyRedirects) || e != ErrTooManyRedirects {
				t.Fatalf("err=%v want ErrTooManyRedirects itself", e)
			}
			if hits.Load() != 10 {
				t.Fatalf("hops=%d want 10", hits.Load())
			}
		})
	}
}

// A caller policy that wraps the sentinel with the refused URL is still
// reported as the bare sentinel: errors.Is recognizes the cap, and nothing
// the policy attached around it surfaces.
func TestRedirectCapWrappedByPolicyLeaksNothing(t *testing.T) {
	const marker = "cap-location-canary.fixture"
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/"+marker+"?secret="+cfFixtureSecret)
		w.WriteHeader(http.StatusFound)
	})
	policy := func(r *http.Request, _ []*http.Request) error {
		return fmt.Errorf("%w at %s", ErrTooManyRedirects, r.URL.String())
	}
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret, HTTP: &http.Client{CheckRedirect: policy}}
	_, e := c.ListTaskProjects(context.Background())
	if e != ErrTooManyRedirects {
		t.Fatalf("err=%T want ErrTooManyRedirects itself", e)
	}
	if leaks := errorLeaks(e, marker, cfFixtureSecret); len(leaks) != 0 {
		t.Fatalf("cap error leaks %d Location values", len(leaks))
	}
}
