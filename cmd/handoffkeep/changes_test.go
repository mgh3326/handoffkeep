package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/remote"
)

func TestChangesVerifyCLI(t *testing.T) {
	const response = `{"id":7,"change_id":3,"head_sha":"1111111111111111111111111111111111111111","level":"live-verified","verifier":"lead","family":"anthropic","profile":"omp-opus@low","evidence":"hk:doc reports/x","ci_result":"success","source":"direct_merge","recorded_by":"peer","recorded_at":"2026-11-05T12:00:00Z","current":true}`
	h, calls := ledgerCLIServer(t, http.StatusCreated, response)
	var out bytes.Buffer
	err := run([]string{"changes", "verify", "--url", h.URL, "--token", "tok",
		"--pr", "https://github.com/example/app/pull/1", "--head", "1111111111111111111111111111111111111111",
		"--level", "live-verified", "--verifier", "lead", "--family", "anthropic", "--evidence", "hk:doc reports/x",
		"--profile", "omp-opus@low", "--ci", "success"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if c.Method != "POST" || c.Path != "/v1/changes/verifications" {
		t.Fatalf("call=%+v", c)
	}
	var sent map[string]any
	if err = json.Unmarshal([]byte(c.Body), &sent); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"pr": "https://github.com/example/app/pull/1", "head_sha": "1111111111111111111111111111111111111111",
		"level": "live-verified", "verifier": "lead", "family": "anthropic", "evidence": "hk:doc reports/x",
		"profile": "omp-opus@low", "ci_result": "success", "source": "direct_merge"}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("body=%v\nwant %v", sent, want)
	}
	var got, exp map[string]any
	if err = json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(response), &exp)
	if !reflect.DeepEqual(got, exp) {
		t.Fatalf("output=%s", out.String())
	}

	if err = run([]string{"changes", "verify", "--url", h.URL, "--token", "tok", "--pr", "x"}, io.Discard, io.Discard); err == nil || len(*calls) != 1 {
		t.Fatalf("missing flags: err=%v calls=%d", err, len(*calls))
	}
}

func TestChangesShowCLI(t *testing.T) {
	h, calls := ledgerCLIServer(t, http.StatusOK, `{"changes":[]}`)
	var out bytes.Buffer
	if err := run([]string{"changes", "show", "--url", h.URL, "--token", "tok", "--task", "42"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Method != "GET" || c.Path != "/v1/changes" || c.Query != "task=42" {
		t.Fatalf("call=%+v", c)
	}
	if out.String() != "{\"changes\":[]}\n" {
		t.Fatalf("output=%q", out.String())
	}
	if err := run([]string{"changes", "show", "--url", h.URL, "--token", "tok", "--task", "1", "--pr", "https://github.com/a/b/pull/1"}, io.Discard, io.Discard); err == nil {
		t.Fatal("both selectors accepted")
	}
}

const changeStateFixture = `{"change":{"id":3,"repo":"example/app","pr_number":9,"pr":"https://github.com/example/app/pull/9","state":"open","head_sha":"1111111111111111111111111111111111111111","opened_at":"2026-11-06T01:00:00Z","updated_at":"2026-11-06T02:00:00Z"},` +
	`"gate":{"state":"blocked","head_sha":"1111111111111111111111111111111111111111","current":true,"reasons":["threads_open"],"decided_at":"2026-11-06T02:00:00Z"},` +
	`"checks":[{"head_sha":"1111111111111111111111111111111111111111","name":"build","state":"completed","conclusion":"success","observed_at":"2026-11-06T02:00:00Z","source":"poll"}],` +
	`"findings":[{"reviewed_head":"1111111111111111111111111111111111111111","round":1,"pass":1,"lens":"correctness","ordinal":0,"review_id":5,"id":"PRRT_1","location":"a.go:3","severity":"high","blocking":true,"summary":"open one","status":"open","author_replied":false,"observed_at":"2026-11-06T02:00:00Z","source":"review"},` +
	`{"reviewed_head":"1111111111111111111111111111111111111111","round":1,"pass":1,"lens":"correctness","ordinal":1,"review_id":5,"id":"PRRT_2","severity":"low","blocking":false,"summary":"done one","status":"resolved","author_replied":true,"observed_at":"2026-11-06T02:00:00Z","source":"review"}],` +
	`"freshness":{"state":null,"checks":{"observed_at":"2026-11-06T02:00:00Z","source":"poll"},"threads":null,"findings":null,"gate":null}}`

func TestChangesShowOnePRCLI(t *testing.T) {
	h, calls := ledgerCLIServer(t, http.StatusOK, changeStateFixture)
	var out bytes.Buffer
	if err := run([]string{"changes", "show", "--url", h.URL, "--token", "tok", "--repo", "example/app", "--pr", "9", "--all-heads", "--include", "checks,gate"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if c.Method != "GET" || c.Path != "/v1/changes/one" || c.Query != "all_heads=1&include=checks%2Cgate&pr=9&repo=example%2Fapp" {
		t.Fatalf("call=%+v", c)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if got["gate"].(map[string]any)["state"] != "blocked" || len(got["checks"].([]any)) != 1 {
		t.Fatalf("output=%v", got)
	}
}

func TestChangesChecksCLI(t *testing.T) {
	h, calls := ledgerCLIServer(t, http.StatusOK, changeStateFixture)
	var out bytes.Buffer
	if err := run([]string{"changes", "checks", "--url", h.URL, "--token", "tok", "--repo", "example/app", "--pr", "9"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Path != "/v1/changes/one" || c.Query != "include=checks&pr=9&repo=example%2Fapp" {
		t.Fatalf("call=%+v", c)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["checks"].([]any)[0].(map[string]any)["name"] != "build" {
		t.Fatalf("output=%s err=%v", out.String(), err)
	}
}

func TestChangesFindingsCLI(t *testing.T) {
	h, calls := ledgerCLIServer(t, http.StatusOK, changeStateFixture)
	titles := func(args ...string) []string {
		t.Helper()
		var out bytes.Buffer
		if err := run(append([]string{"changes", "findings", "--url", h.URL, "--token", "tok", "--repo", "example/app", "--pr", "9"}, args...), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Findings []struct{ Summary string } `json:"findings"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("output=%s err=%v", out.String(), err)
		}
		names := []string{}
		for _, f := range got.Findings {
			names = append(names, f.Summary)
		}
		return names
	}
	if all := titles(); !reflect.DeepEqual(all, []string{"open one", "done one"}) {
		t.Fatalf("all findings: %v", all)
	}
	if open := titles("--open"); !reflect.DeepEqual(open, []string{"open one"}) {
		t.Fatalf("--open: %v", open)
	}
	if c := (*calls)[1]; c.Query != "include=findings&pr=9&repo=example%2Fapp" {
		t.Fatalf("call=%+v", c)
	}
}

func TestChangesStateCLIErrors(t *testing.T) {
	h, calls := ledgerCLIServer(t, http.StatusNotFound, `{"error":"not_found"}`)
	base := []string{"--url", h.URL, "--token", "tok"}
	// A PR hk has never seen is the server's not_found, not a usage error.
	if err := run(append([]string{"changes", "findings", "--repo", "example/app", "--pr", "9"}, base...), io.Discard, io.Discard); err == nil || remote.ErrorCode(err) != "not_found" {
		t.Fatalf("err=%v", err)
	}
	for _, args := range [][]string{
		{"changes", "show", "--repo", "example/app"},
		{"changes", "show", "--repo", "example/app", "--pr", "https://github.com/example/app/pull/9"},
		{"changes", "show", "--repo", "example/app", "--pr", "9", "--task", "4"},
		{"changes", "show", "--pr", "https://github.com/example/app/pull/9", "--all-heads"},
		{"changes", "checks", "--repo", "example/app"},
		{"changes", "findings", "--pr", "9"},
	} {
		before := len(*calls)
		if err := run(append(args, base...), io.Discard, io.Discard); err == nil || len(*calls) != before {
			t.Errorf("%v: err=%v, made %d calls", args, err, len(*calls)-before)
		}
	}
}

// TestChangesShowPrintsReviewHistoryAndDelivery: the review-history fields of
// a review and the delivery_id of a check pass through `changes show` as the
// server sent them, and a review without them prints without the keys.
func TestChangesShowPrintsReviewHistoryAndDelivery(t *testing.T) {
	const digest = "abababababababababababababababab"
	list := `{"changes":[{"id":3,"repo":"example/app","pr_number":9,"pr":"https://github.com/example/app/pull/9","state":"open","opened_at":"2026-11-06T01:00:00Z","created_at":"2026-11-06T01:00:00Z","updated_at":"2026-11-06T01:00:00Z","priority":0,"open_threads":0,"open_findings":0,"tasks":[],"verification":null,"verifications":[],` +
		`"reviews":[{"id":1,"reviewed_head":"1111111111111111111111111111111111111111","round":1,"pass":1,"lens":"correctness","recorded_at":"2026-11-06T01:00:00Z","reviewer":"sonnet@high","same_family":false,"delta":true,"quiet":false,"outside_diff_blocking":0,"patch_digest":"` + digest + `"},` +
		`{"id":2,"reviewed_head":"1111111111111111111111111111111111111111","round":1,"pass":2,"lens":"correctness","recorded_at":"2026-11-06T01:00:00Z"}]}]}`
	h, _ := ledgerCLIServer(t, http.StatusOK, list)
	var out bytes.Buffer
	if err := run([]string{"changes", "show", "--url", h.URL, "--token", "tok", "--pr", "https://github.com/example/app/pull/9"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Changes []struct {
			Reviews []map[string]any `json:"reviews"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Changes) != 1 || len(got.Changes[0].Reviews) != 2 {
		t.Fatalf("output=%s err=%v", out.String(), err)
	}
	full, bare := got.Changes[0].Reviews[0], got.Changes[0].Reviews[1]
	want := map[string]any{"reviewer": "sonnet@high", "same_family": false, "delta": true, "quiet": false, "outside_diff_blocking": float64(0), "patch_digest": digest}
	for k, v := range want {
		if full[k] != v {
			t.Errorf("review[%q]=%v, want %v", k, full[k], v)
		}
	}
	for k := range want {
		if _, has := bare[k]; has {
			t.Errorf("a review without %q prints it: %v", k, bare)
		}
	}

	state := strings.Replace(changeStateFixture, `"name":"build","state":"completed"`, `"name":"build","delivery_id":"delivery-1","state":"completed"`, 1)
	h, _ = ledgerCLIServer(t, http.StatusOK, state)
	out.Reset()
	if err := run([]string{"changes", "checks", "--url", h.URL, "--token", "tok", "--repo", "example/app", "--pr", "9"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var one struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &one); err != nil || len(one.Checks) != 1 || one.Checks[0]["delivery_id"] != "delivery-1" {
		t.Fatalf("output=%s err=%v", out.String(), err)
	}

	// The one-PR read prints its reviews section with the review-history fields.
	withReviews := strings.Replace(state, `"freshness":`, `"reviews":[{"id":1,"reviewed_head":"1111111111111111111111111111111111111111","round":1,"pass":1,"lens":"correctness","recorded_at":"2026-11-06T01:00:00Z","reviewer":"sonnet@high","same_family":false,"patch_digest":"`+digest+`"}],"freshness":`, 1)
	h, calls := ledgerCLIServer(t, http.StatusOK, withReviews)
	out.Reset()
	if err := run([]string{"changes", "show", "--url", h.URL, "--token", "tok", "--repo", "example/app", "--pr", "9", "--include", "reviews"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Path != "/v1/changes/one" || c.Query != "include=reviews&pr=9&repo=example%2Fapp" {
		t.Fatalf("call=%+v", c)
	}
	var rv struct {
		Reviews []map[string]any `json:"reviews"`
	}
	if err := json.Unmarshal(out.Bytes(), &rv); err != nil || len(rv.Reviews) != 1 || rv.Reviews[0]["reviewer"] != "sonnet@high" || rv.Reviews[0]["same_family"] != false || rv.Reviews[0]["patch_digest"] != digest {
		t.Fatalf("output=%s err=%v", out.String(), err)
	}
}
