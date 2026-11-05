package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJobsSkillsReportCmd(t *testing.T) {
	var requested bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
		if r.URL.Path != "/v1/job-skills" || r.URL.Query().Get("limit") != "5" {
			t.Errorf("request %s", r.URL.String())
		}
		since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		if err != nil || time.Since(since) < 7*24*time.Hour-time.Minute || time.Since(since) > 7*24*time.Hour+time.Minute {
			t.Errorf("since %v %v", since, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jobs_total":3}`))
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := jobsCmd([]string{"skills-report", "--url", srv.URL, "--token", "test", "--since", "7d", "--limit", "5"}, &out); err != nil {
		t.Fatal(err)
	}
	if !requested || !bytes.Contains(out.Bytes(), []byte(`"jobs_total":3`)) {
		t.Fatal(out.String())
	}
	if err := jobsCmd(nil, &out); err == nil || err.Error() != "usage: jobs skills-report" {
		t.Fatal(err)
	}
}
