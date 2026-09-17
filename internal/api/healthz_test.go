package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"testing"
)

func stubBuildInfo(t *testing.T, info *debug.BuildInfo, ok bool) {
	t.Helper()
	original := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return info, ok }
	t.Cleanup(func() { readBuildInfo = original })
}

func getHealthz(t *testing.T) (int, map[string]string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	healthz(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body is not JSON: %v (%q)", err, recorder.Body.String())
	}
	return recorder.Code, body
}

func TestHealthzReportsVCSRevision(t *testing.T) {
	stubBuildInfo(t, &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
		{Key: "vcs.time", Value: "2026-09-17T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
		{Key: "GOOS", Value: "linux"},
	}}, true)

	code, body := getHealthz(t)
	if code != http.StatusOK {
		t.Fatalf("healthz status=%d", code)
	}
	if body["status"] != "ok" {
		t.Fatalf("healthz status field=%q", body["status"])
	}
	if body["vcs_revision"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("healthz vcs_revision=%q", body["vcs_revision"])
	}
	if body["vcs_time"] != "2026-09-17T00:00:00Z" {
		t.Fatalf("healthz vcs_time=%q", body["vcs_time"])
	}
	if body["vcs_modified"] != "true" {
		t.Fatalf("healthz vcs_modified=%q", body["vcs_modified"])
	}
}

func TestHealthzOmitsVCSKeysWhenUnstamped(t *testing.T) {
	stubBuildInfo(t, &debug.BuildInfo{}, true)

	code, body := getHealthz(t)
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz code=%d status=%q", code, body["status"])
	}
	for _, key := range []string{"vcs_revision", "vcs_time", "vcs_modified"} {
		if _, present := body[key]; present {
			t.Fatalf("unstamped binary must not emit %s", key)
		}
	}
}

// /healthz stays unauthenticated and never touches the store, so a nil-service
// Server must still answer 200.
func TestHealthzRouteNeedsNoAuthOrService(t *testing.T) {
	recorder := httptest.NewRecorder()
	Server{}.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz status=%d", recorder.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["status"] != "ok" {
		t.Fatalf("healthz body=%q err=%v", recorder.Body.String(), err)
	}
}
