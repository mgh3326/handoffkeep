package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// The design §6b CLI vectors run through the built handoffkeep binary
// against an in-process test server. Per director-1's S1 test-scope
// ruling, each vector's server-side observables (HTTP status, body,
// recorded rows, codes, exits) run here; its client-only observables are
// C1's and are listed, keyed by vector and observable, in
// c1OwnedObservables. TestC1OwnedObservablesPinned derives that set from
// the pack and the §6b/§9 C1 scope, so no other omission is possible.

const wrkPackRoot = "../../testdata/o3"

// c1OwnedObservables are the client-only observables S1 does not execute.
// Each is the wrk adapter's (C1, hk 1926) by design §6b and §9.
var c1OwnedObservables = map[string]string{
	"transport-intents.json/path-fake-or-overridden-binary-refused":        "pr242_guard",                      // §6b: PR242 guard refuses the override before exec (exit 2)
	"transport-intents.json/unguarded-hk-bin-lookup-rejected":              "canonical_resolver_spy",           // §6b: C1 test spy on the canonical resolver
	"transport-intents.json/env-token-with-cli-unavailable-no-fallback":    "no_fallback_when_cli_unavailable", // §6b: no HTTP/curl/env-token fallback in wrk
	"transport-intents.json/commit-then-response-lost-file-changed":        "durable_intent",                   // §6b: resend from the pending intent; changed file ignored
	"transport-intents.json/definitive-sequence-conflict-then-refresh":     "durable_intent",                   // §6b: keep the rejected intent, new key after revalidation
	"transport-intents.json/crash-before-send-after-commit-after-fsync":    "durable_intent",                   // §6b: saved-intent retry and GET projection repair
	"transport-intents.json/intent-symlink-mode-malformed-fsync-fails":     "intent_file_validation",           // §6b: 0600 intent file checks, exit 74
	"transport-intents.json/cursor-page-inconsistent-history-regressed":    "history_regressed",                // §6b: retained-receipt comparison (alt code)
	"transport-intents.json/redirect-html-namespace-unknown-code":          "durable_intent",                   // §6b: append keeps the intent pending
	"transport-intents.json/caller-env-overrides-scrubbed":                 "env_scrub",                        // §6b: C1 removes the four override keys
	"transport-intents.json/commit-then-proxy-5xx-pending-intent":          "durable_intent",                   // §6b: pending intent unchanged, exact-key retry
	"r4-review-lifecycle.json/r-done-5xx-intent-pending-exit-75":           "durable_intent",                   // §6: pending intent, no local completion
	"failure-abuse.json/server-loses-acknowledged-write-history-regressed": "history_regressed",                // §8: history_regressed is a client code
}

type cliCase struct {
	file    string
	Name    string         `json:"name"`
	Request map[string]any `json:"request"`
	Expect  map[string]any `json:"expect"`
}

func (c cliCase) id() string { return c.file + "/" + c.Name }

func loadCLICases(t *testing.T, all bool) []cliCase {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(wrkPackRoot, "vectors", "*.json"))
	sort.Strings(files)
	var out []cliCase
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var vf struct {
			Cases []cliCase `json:"cases"`
		}
		if err := json.Unmarshal(b, &vf); err != nil {
			t.Fatal(err)
		}
		for _, c := range vf.Cases {
			c.file = filepath.Base(f)
			if all || c.Request["type"] == "cli" {
				out = append(out, c)
			}
		}
	}
	return out
}

// deriveC1Observable applies the §6b/§9 C1 scope to one vector's own data:
// the PR 242 guard inputs, the resolver spy, the canonical-CLI-unavailable
// path, the environment scrub, the durable intent file and the
// retained-receipt history check are the wrk adapter's.
func deriveC1Observable(c cliCase) string {
	env, _ := c.Request["env"].(map[string]any)
	codes := map[string]bool{}
	for _, k := range []string{"stderr_code", "stderr_code_alt", "client_code"} {
		if s, ok := c.Expect[k].(string); ok {
			codes[s] = true
		}
	}
	note := strings.ToLower(fmt.Sprint(c.Expect["note"]))
	switch {
	case env["HANDOFFKEEP_BIN"] != nil || env["PATH_with_fake"] != nil:
		return "pr242_guard"
	case c.Expect["outcome"] == "spy_rejects":
		return "canonical_resolver_spy"
	case env["_cli_unavailable"] != nil:
		return "no_fallback_when_cli_unavailable"
	case env["HANDOFFKEEP_URL"] != nil || env["HANDOFFKEEP_TOKEN"] != nil || env["HANDOFFKEEP_CF_ACCESS_CLIENT_ID"] != nil || env["HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET"] != nil:
		return "env_scrub"
	case codes["intent_invalid"] || codes["intent_io"]:
		return "intent_file_validation"
	case codes["history_regressed"]:
		return "history_regressed"
	case strings.Contains(note, "intent") || strings.Contains(note, "pending") || c.Expect["outcome"] == "intent_retry":
		return "durable_intent"
	}
	return ""
}

func TestC1OwnedObservablesPinned(t *testing.T) {
	derived := map[string]string{}
	for _, c := range loadCLICases(t, true) {
		if c.Request["type"] != "cli" && c.Expect["client_code"] == nil {
			continue
		}
		if obs := deriveC1Observable(c); obs != "" {
			derived[c.id()] = obs
		}
	}
	for id, obs := range derived {
		if c1OwnedObservables[id] != obs {
			t.Errorf("%s: design assigns %s to C1; list says %q", id, obs, c1OwnedObservables[id])
		}
	}
	for id := range c1OwnedObservables {
		if _, ok := derived[id]; !ok {
			t.Errorf("%s is listed C1-owned but the design assigns it no client-only observable", id)
		}
	}
}

var (
	wrkBinOnce sync.Once
	wrkBinPath string
	wrkBinErr  error
	wrkBinDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if wrkBinDir != "" {
		_ = os.RemoveAll(wrkBinDir)
	}
	os.Exit(code)
}

func wrkBinary(t *testing.T) string {
	t.Helper()
	wrkBinOnce.Do(func() {
		if wrkBinDir, wrkBinErr = os.MkdirTemp("", "hkwrk"); wrkBinErr != nil {
			return
		}
		wrkBinPath = filepath.Join(wrkBinDir, "handoffkeep")
		out, err := exec.Command("go", "build", "-o", wrkBinPath, ".").CombinedOutput()
		if err != nil {
			wrkBinErr = fmt.Errorf("build: %v: %s", err, out)
		}
	})
	if wrkBinErr != nil {
		t.Fatal(wrkBinErr)
	}
	return wrkBinPath
}

type cliResult struct {
	exit           int
	stdout, stderr string
}

// runCLI runs the built binary with only HOME, PATH and TMPDIR in its
// environment; HOME holds the installed config.env pointing at url.
func runCLI(t *testing.T, url, token string, argv []string, stdin []byte, extraEnv ...string) cliResult {
	t.Helper()
	home := t.TempDir()
	if url != "" {
		dir := filepath.Join(home, ".config", "handoffkeep")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.env"), []byte("HANDOFFKEEP_URL="+url+"\nHANDOFFKEEP_TOKEN="+token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(wrkBinary(t), append([]string{"wrk"}, argv...)...)
	cmd.Env = append([]string{"HOME=" + home, "PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}, extraEnv...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := cliResult{stdout: out.String(), stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.exit = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return res
}

func cliToken(t *testing.T) string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "tk" + hex.EncodeToString(b)
}

// realServer is an in-process /v1/wrk server over a memory journal with
// job-b0debcafebabe00 claimed, activated and holding one open round
// (tail 3, claim_seq 1), which is the history the transport vectors'
// append arguments name.
type realServer struct {
	srv    *httptest.Server
	mem    *wrk.Memory
	tokens map[string]string
	hits   int
	mu     sync.Mutex
}

func newRealServer(t *testing.T) *realServer {
	t.Helper()
	policyRaw := packPolicy(t)
	policy, err := wrk.ParsePolicy(policyRaw)
	if err != nil {
		t.Fatal(err)
	}
	rs := &realServer{mem: wrk.NewMemory(), tokens: map[string]string{}}
	rs.mem.AddTask(2499)
	tokens := api.Tokens{}
	for _, id := range wrk.ClientIDs {
		tok := cliToken(t)
		tokens[id], rs.tokens[id] = tok, tok
	}
	eng := &wrk.Engine{Policy: policy, Generation: "00000000-0000-4000-8000-ffff00000001", Enforce: true, Now: func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }}
	h := api.Server{Tokens: tokens, Wrk: &api.Wrk{Mode: api.WrkEnforce, Policy: policy, Engine: eng, Backend: rs.mem}}.Handler()
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.hits++
		rs.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(rs.srv.Close)
	job := "job-b0debcafebabe00"
	for i, step := range []struct {
		actor, kind string
		claim, exp  int64
		payload     map[string]any
	}{
		{"director-1", "claim", 0, 0, map[string]any{"job_id": job, "owner_lane": "b2499-x", "parent_lane": "director-1", "task_id": 2499, "host_client_id": "desktop", "role": "builder", "tier": "T1", "review_of": nil, "predecessor_job_id": nil, "supersedes": nil}},
		{"director-1", "activate", 1, 1, map[string]any{"reason_code": "activation", "seal_seq": nil}},
		{"desktop", "round_open", 1, 2, map[string]any{"head": strings.Repeat("1", 40), "tester_job_id": nil, "extension_grant_seq": nil, "repeat_grant_seq": nil}},
	} {
		body, _ := json.Marshal(map[string]any{"kind": step.kind, "claim_seq": step.claim, "expected_seq": step.exp, "payload": step.payload})
		req, _ := http.NewRequest("POST", rs.srv.URL+"/v1/wrk/jobs/"+job+"/records", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rs.tokens[step.actor])
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("00000000-0000-4000-8000-%012d", 900+i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 201 {
			t.Fatalf("setup %s: %v %v", step.kind, err, resp)
		}
		resp.Body.Close()
	}
	return rs
}

func packPolicy(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(wrkPackRoot, "vectors", "transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vf struct {
		Cases []struct {
			Given struct {
				Policy json.RawMessage `json:"policy"`
			} `json:"given"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &vf); err != nil {
		t.Fatal(err)
	}
	return vf.Cases[0].Given.Policy
}

// stubServer answers every request with one canned reply.
func stubServer(t *testing.T, stub map[string]any) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch {
		case stub["response"] == "redirect":
			http.Redirect(w, r, "https://elsewhere.invalid/v1/wrk/jobs/x", http.StatusFound)
		case stub["response"] == "html_5xx":
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(502)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		case stub["response"] == "malformed_cursor":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(malformedRecordsPage())
		case stub["http_status"] != nil:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(stub["http_status"].(float64)))
			_ = json.NewEncoder(w).Encode(map[string]any{"error": stub["error"]})
		default:
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// malformedRecordsPage is a schema-valid RecordsResponse whose cursor
// arithmetic is inconsistent (next_after_seq past the returned records).
func malformedRecordsPage() []byte {
	gen := "00000000-0000-4000-8000-ffff00000001"
	z := strings.Repeat("0", 64)
	sum := map[string]any{"job_id": "job-b0debcafebabe00", "claim_seq": 1, "snapshot_seq": 5, "state": "active", "baseline_used": 0, "used": 1, "cap": 3, "next_round_no": 2, "open_rounds": []any{1}, "last_verdict_seq": nil, "active_grants": []any{}, "warnings": []any{}, "store_generation": gen, "tail_hash": z}
	b, _ := json.Marshal(map[string]any{"contract": "hk1873.v1", "snapshot_seq": 5, "next_after_seq": 4, "has_more": true, "records": []any{}, "summary": sum})
	return b
}

func vectorStdin(t *testing.T, c cliCase) []byte {
	t.Helper()
	switch s := c.Request["stdin"].(type) {
	case map[string]any:
		b, err := base64.StdEncoding.DecodeString(s["$b64"].(string))
		if err != nil {
			t.Fatal(err)
		}
		return b
	case string:
		return []byte(s)
	}
	return nil
}

func argvOf(c cliCase) []string {
	var out []string
	for _, a := range c.Request["argv"].([]any) {
		out = append(out, a.(string))
	}
	return out
}

func checkFailure(t *testing.T, r cliResult, code string, exit int) {
	t.Helper()
	if r.exit != exit || r.stderr != "handoffkeep wrk: "+code+"\n" || r.stdout != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want exit %d code %s", r.exit, r.stdout, r.stderr, exit, code)
	}
}

// cliServerHalf runs one vector's S1 observables.
var cliServerHalf = map[string]func(t *testing.T, c cliCase){
	"path-fake-or-overridden-binary-refused": func(t *testing.T, c cliCase) {
		// The S1 CLI selects no binary and reads no HANDOFFKEEP_BIN: with an
		// override set and a fake handoffkeep first on PATH, the canonical
		// CLI still runs against its installed configuration.
		rs := newRealServer(t)
		fake := t.TempDir()
		marker := filepath.Join(fake, "ran")
		if err := os.WriteFile(filepath.Join(fake, "handoffkeep"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := runCLI(t, rs.srv.URL, rs.tokens["desktop"], argvOf(c), nil, "HANDOFFKEEP_BIN=/evil/handoffkeep", "PATH="+fake+":/usr/bin:/bin")
		if r.exit != 0 || !strings.HasSuffix(r.stdout, "}\n") {
			t.Fatalf("canonical CLI: %d %q %q", r.exit, r.stdout, r.stderr)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the fake PATH entry ran")
		}
	},
	"unguarded-hk-bin-lookup-rejected": func(t *testing.T, c cliCase) {
		rs := newRealServer(t)
		r := runCLI(t, rs.srv.URL, rs.tokens["desktop"], argvOf(c), nil)
		var page map[string]any
		if r.exit != 0 || json.Unmarshal([]byte(r.stdout), &page) != nil || page["has_more"] != false || page["snapshot_seq"] != float64(3) {
			t.Fatalf("records: %d %q %q", r.exit, r.stdout, r.stderr)
		}
	},
	"env-token-with-cli-unavailable-no-fallback": func(t *testing.T, c cliCase) {
		srv, _ := stubServer(t, nil)
		url := srv.URL
		srv.Close()
		checkFailure(t, runCLI(t, url, "unused", argvOf(c), nil), "transport_unavailable", 75)
	},
	"server-host-mismatch-exit-77":     stubbedFailure,
	"server-round-cap-exit-78":         stubbedFailure,
	"server-sequence-conflict-exit-73": stubbedFailure,
	"commit-then-response-lost-file-changed": func(t *testing.T, c cliCase) {
		replayHalf(t, c)
	},
	"definitive-sequence-conflict-then-refresh": stubbedFailure,
	"crash-before-send-after-commit-after-fsync": func(t *testing.T, c cliCase) {
		replayHalf(t, c)
	},
	"intent-symlink-mode-malformed-fsync-fails": func(t *testing.T, c cliCase) {
		// S1 keeps no intent file: the append is one request and nothing is
		// written under HOME or the working directory.
		rs := newRealServer(t)
		r := runCLI(t, rs.srv.URL, rs.tokens["desktop"], argvOf(c), vectorStdin(t, c))
		if r.exit != 0 {
			t.Fatalf("append: %d %q", r.exit, r.stderr)
		}
		if matches, _ := filepath.Glob("o3-intents*"); len(matches) > 0 {
			t.Fatalf("CLI wrote an intents file: %v", matches)
		}
	},
	"cursor-page-inconsistent-history-regressed": stubbedProtocol,
	"redirect-html-namespace-unknown-code": func(t *testing.T, c cliCase) {
		srv, hits := stubServer(t, c.Request["stub"].(map[string]any))
		r := runCLI(t, srv.URL, "tok", argvOf(c), nil)
		checkFailure(t, r, "protocol_invalid", 75)
		if *hits != 1 || strings.Contains(r.stderr, "elsewhere") {
			t.Fatalf("redirect followed or echoed: hits=%d stderr=%q", *hits, r.stderr)
		}
	},
	"caller-env-overrides-scrubbed": func(t *testing.T, c cliCase) {
		// With the four override keys absent (C1 scrubs them), the CLI uses
		// the installed configuration.
		rs := newRealServer(t)
		r := runCLI(t, rs.srv.URL, rs.tokens["desktop"], argvOf(c), nil)
		if r.exit != 0 {
			t.Fatalf("installed config: %d %q", r.exit, r.stderr)
		}
	},
	"commit-then-proxy-5xx-pending-intent": stubbedProtocol,
	"r-done-5xx-intent-pending-exit-75": func(t *testing.T, c cliCase) {
		srv, _ := stubServer(t, nil)
		url := srv.URL
		srv.Close()
		checkFailure(t, runCLI(t, url, "unused", argvOf(c), vectorStdin(t, c)), "transport_unavailable", 75)
	},
}

func stubbedFailure(t *testing.T, c cliCase) {
	srv, _ := stubServer(t, c.Request["stub"].(map[string]any))
	code := c.Expect["stderr_code"].(string)
	checkFailure(t, runCLI(t, srv.URL, "tok", argvOf(c), vectorStdin(t, c)), code, int(c.Expect["exit_code"].(float64)))
}

func stubbedProtocol(t *testing.T, c cliCase) {
	srv, _ := stubServer(t, c.Request["stub"].(map[string]any))
	checkFailure(t, runCLI(t, srv.URL, "tok", argvOf(c), vectorStdin(t, c)), "protocol_invalid", 75)
}

// replayHalf: the server half of the lost-reply rows. The identical
// key/body is sent twice; the second answers 200 created=false with the
// same record and no second charge.
func replayHalf(t *testing.T, c cliCase) {
	rs := newRealServer(t)
	argv, stdin := argvOf(c), vectorStdin(t, c)
	first := runCLI(t, rs.srv.URL, rs.tokens["desktop"], argv, stdin)
	second := runCLI(t, rs.srv.URL, rs.tokens["desktop"], argv, stdin)
	var a, b map[string]any
	if first.exit != 0 || second.exit != 0 || json.Unmarshal([]byte(first.stdout), &a) != nil || json.Unmarshal([]byte(second.stdout), &b) != nil {
		t.Fatalf("append twice: %+v %+v", first, second)
	}
	if a["created"] != true || b["created"] != false || a["record"].(map[string]any)["record_hash"] != b["record"].(map[string]any)["record_hash"] {
		t.Fatalf("replay: %v / %v", a, b)
	}
	if n := len(rs.mem.Records("job-b0debcafebabe00")); n != 4 {
		t.Fatalf("journal has %d records, want 4", n)
	}
}

func TestO3PackCLIVectors(t *testing.T) {
	cases := loadCLICases(t, false)
	if len(cases) != 15 {
		t.Fatalf("%d cli vectors, the M1 pack has 15", len(cases))
	}
	for _, c := range cases {
		c := c
		t.Run(c.id(), func(t *testing.T) {
			half := cliServerHalf[c.Name]
			if half == nil {
				t.Fatalf("no S1 server half for %s", c.id())
			}
			half(t, c)
		})
	}
}
