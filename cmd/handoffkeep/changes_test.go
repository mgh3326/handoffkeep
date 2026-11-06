package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
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
