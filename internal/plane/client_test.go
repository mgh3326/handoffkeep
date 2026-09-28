package plane

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestHasMarkerBoundary pins the adoption contract: hk:task/1 must match its
// own description but never the description of hk:task/10 or any other id
// whose decimal encoding has 1 as a prefix.
func TestHasMarkerBoundary(t *testing.T) {
	cases := []struct {
		name   string
		html   string
		marker string
		want   bool
	}{
		{"own marker", "<p>Mirrored read-only from hk; hk is the source of truth. Reference: hk:task/1</p>", "hk:task/1", true},
		{"digit prefix longer id", "<p>Mirrored read-only from hk; hk is the source of truth. Reference: hk:task/10</p>", "hk:task/1", false},
		{"digit prefix middle", "x hk:task/12 y", "hk:task/1", false},
		{"marker at end", "ref hk:task/7", "hk:task/7", true},
		{"marker before punctuation", "(hk:task/3)", "hk:task/3", true},
		{"longer id then own", "hk:task/10 and hk:task/1", "hk:task/1", true},
		{"unrelated", "<p>nothing here</p>", "hk:task/1", false},
		{"empty html", "", "hk:task/1", false},
	}
	for _, tc := range cases {
		if got := hasMarker(tc.html, tc.marker); got != tc.want {
			t.Errorf("%s: hasMarker(%q, %q)=%t want %t", tc.name, tc.html, tc.marker, got, tc.want)
		}
	}
}

// TestNewClientSchemePolicy proves the client refuses to send the API key
// over plaintext http to any non-loopback host while keeping loopback http
// for tests and local dev servers.
func TestNewClientSchemePolicy(t *testing.T) {
	cases := []struct {
		name    string
		apiURL  string
		wantErr bool
	}{
		{"https remote", "https://api.plane.so", false},
		{"http remote refused", "http://plane.example.com", true},
		{"empty scheme refused", "plane.example.com", true},
		{"loopback ip ok", "http://127.0.0.1:9999", false},
		{"loopback name ok", "http://localhost:9999", false},
		{"loopback ipv6 ok", "http://[::1]:9999", false},
		{"non-loopback ip refused", "http://10.0.0.5", true},
		{"default url", "", false},
	}
	for _, tc := range cases {
		_, err := NewClient(Config{APIURL: tc.apiURL, APIKey: "k", Workspace: "w"})
		if tc.wantErr != (err != nil) {
			t.Errorf("%s: err=%v wantErr=%t", tc.name, err, tc.wantErr)
		}
	}
}

// TestClientDoesNotFollowRedirects proves a redirecting Plane endpoint cannot
// make the client forward the X-API-Key credential to another host.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if r.Header.Get("X-API-Key") != "" {
			t.Error("redirect target saw the API key")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	client, err := NewClient(Config{
		APIURL:    redirector.URL,
		APIKey:    "fixture-key",
		Workspace: "ws-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListProjects(t.Context()); err == nil {
		t.Fatal("redirect should surface as an error, not a followed request")
	}
	if hits := targetHits.Load(); hits != 0 {
		t.Fatalf("redirect target received %d requests", hits)
	}
}
