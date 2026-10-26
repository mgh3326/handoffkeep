package wrkrecords_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

// Exact ownership split from sections 6b/9 and director-1 comments 7595/7596.
// Each row/observable is pinned below. S1 still executes every available server
// or CLI-transport observable; durable file state is never implemented here.
var c1OnlyObservables = map[string][]string{
	"transport/1":  {"pre-exec canonical binary guard", "fake executable sees zero calls"},
	"transport/2":  {"guard resolver spy on every invocation/page"},
	"transport/3":  {"no alternate HTTP/curl fallback", "no local completion/quota success"},
	"transport/4":  {"no local fallback"},
	"transport/5":  {"pending request survives changed evidence file", "accepted intent persistence and local projection"},
	"transport/6":  {"retain rejected intent", "revalidate before creating new intent/key"},
	"transport/7":  {"durable intent crash points", "GET projection repair", "single local release"},
	"transport/8":  {"regular 0600 nonsymlink intent validation", "lock/atomic-write/fsync failure before send"},
	"transport/9":  {"compare retained receipts/generation across calls", "no decisions on partial history"},
	"transport/10": {"keep append intent pending"},
	"transport/11": {"remove endpoint/token/CF override keys before exec", "supply canonical account home"},
	"transport/12": {"preserve pending intent on noncontract reply/503"},
	"r4life/1":     {"pending durable intent", "no local event/relay/housekeeping/quota release"},
}

func TestC1OnlyObservableList(t *testing.T) {
	// Literal equality prevents a broad category or a newly omitted row from
	// hiding an S1 obligation. The quoted observable names are reviewed against
	// the fixed section 6b transport table; section 9 assigns their owner C1.
	want := map[string][]string{
		"transport/1":  {"pre-exec canonical binary guard", "fake executable sees zero calls"},
		"transport/2":  {"guard resolver spy on every invocation/page"},
		"transport/3":  {"no alternate HTTP/curl fallback", "no local completion/quota success"},
		"transport/4":  {"no local fallback"},
		"transport/5":  {"pending request survives changed evidence file", "accepted intent persistence and local projection"},
		"transport/6":  {"retain rejected intent", "revalidate before creating new intent/key"},
		"transport/7":  {"durable intent crash points", "GET projection repair", "single local release"},
		"transport/8":  {"regular 0600 nonsymlink intent validation", "lock/atomic-write/fsync failure before send"},
		"transport/9":  {"compare retained receipts/generation across calls", "no decisions on partial history"},
		"transport/10": {"keep append intent pending"},
		"transport/11": {"remove endpoint/token/CF override keys before exec", "supply canonical account home"},
		"transport/12": {"preserve pending intent on noncontract reply/503"},
		"r4life/1":     {"pending durable intent", "no local event/relay/housekeeping/quota release"},
	}
	if !reflect.DeepEqual(c1OnlyObservables, want) {
		t.Fatal("C1-only ownership list changed")
	}
	rows := map[string]bool{}
	for _, v := range loadVectors(t) {
		if Text(v.Request, "type") == "cli" {
			rows[v.Source.Table+"/"+strconv.Itoa(v.Source.Row)] = true
		}
	}
	if len(rows) != len(want) {
		t.Fatal("CLI source rows changed; review ownership")
	}
	for k := range want {
		if !rows[k] {
			t.Fatalf("unknown C1-only row %s", k)
		}
	}
}

func buildWrkCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "handoffkeep")
	cmd := exec.Command("go", "build", "-p", "2", "-o", path, "./cmd/handoffkeep")
	cmd.Dir = "../.."
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build CLI: %v %s", e, b)
	}
	return path
}
func runWrkCLI(t *testing.T, binary, endpoint string, args []string, input []byte) (int, string, string) {
	t.Helper()
	home := t.TempDir()
	conf := filepath.Join(home, ".config", "handoffkeep")
	if e := os.MkdirAll(conf, 0700); e != nil {
		t.Fatal(e)
	}
	// Synthetic installed configuration exists only inside the test fixture HOME.
	token := NewUUID()
	if e := os.WriteFile(filepath.Join(conf, "config.env"), []byte("HANDOFFKEEP_URL="+endpoint+"\nHANDOFFKEEP_TOKEN="+token+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "HANDOFFKEEP_") || strings.HasPrefix(k, "HK_") || k == "HOME" || k == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	cmd := exec.Command(binary, append([]string{"wrk"}, args...)...)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal("CLI invocation failed")
		}
	}
	return code, stdout.String(), stderr.String()
}
func cliScenario(t *testing.T, job string) *scenario {
	t.Helper()
	c := fresh(t)
	delete(c.s.Jobs, "subject")
	c.must("director-1", job, "claim", Object{"job_id": job, "owner_lane": "builder-a", "parent_lane": "director-1", "task_id": 9001, "host_client_id": "desktop", "role": "builder", "tier": "T3", "review_of": nil, "predecessor_job_id": nil, "supersedes": nil})
	c.must("director-1", job, "activate", Object{"seal_seq": nil, "reason_code": "fixture"})
	c.must("desktop", job, "round_open", opening(strings.Repeat("1", 40), "", nil, nil))
	return c
}
func TestWrkCLIVectors(t *testing.T) {
	binary := buildWrkCLI(t)
	for _, v := range loadVectors(t) {
		if Text(v.Request, "type") != "cli" {
			continue
		}
		t.Run(strings.TrimSuffix(v.file, ".json")+"/"+v.Name, func(t *testing.T) {
			argv := []string{}
			for _, x := range v.Request["argv"].([]any) {
				argv = append(argv, x.(string))
			}
			input := []byte{}
			if s, ok := v.Request["stdin"].(string); ok {
				input = []byte(s)
			} else {
				var e error
				input, e = base64.StdEncoding.DecodeString(Text(Obj(v.Request, "stdin"), "$b64"))
				if e != nil {
					t.Fatal(e)
				}
			}
			job := argv[1]
			c := cliScenario(t, job)
			stub := Obj(v.Request, "stub")
			var calls, forwarded atomic.Int32
			var stateMu sync.Mutex
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stateMu.Lock()
				defer stateMu.Unlock()
				if r.URL.Path == "/redirect-target" {
					forwarded.Add(1)
					w.WriteHeader(500)
					return
				}
				attempt := calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n := Num(stub, "http_status"); n != 0 {
					w.WriteHeader(int(n))
					_ = json.NewEncoder(w).Encode(Object{"error": stub["error"]})
					return
				}
				switch Text(stub, "response") {
				case "redirect":
					http.Redirect(w, r, "/redirect-target", 307)
					return
				case "malformed_cursor":
					_, _ = io.WriteString(w, `{"contract":"hk1873.v1","snapshot_seq":4,"records":[]}`)
					return
				}
				if r.Method == "POST" {
					raw, e := io.ReadAll(r.Body)
					if e != nil {
						t.Error(e)
						return
					}
					request, e := ParseRequest(raw)
					if e != nil {
						t.Error(e)
						return
					}
					// A lost response is simulated by committing this exact request first;
					// the actual CLI invocation must then receive its original replay.
					if stub["replay"] == true {
						if _, e = c.send("desktop", job, r.Header.Get("Idempotency-Key"), request); e != nil {
							t.Error(e)
							return
						}
					}
					out, e := c.send("desktop", job, r.Header.Get("Idempotency-Key"), request)
					if e != nil {
						w.WriteHeader(Status(e))
						_ = json.NewEncoder(w).Encode(Object{"error": Code(e)})
						return
					}
					if attempt == 1 && Text(stub, "response") == "html_5xx" {
						w.Header().Set("Content-Type", "text/html")
						w.WriteHeader(502)
						_, _ = io.WriteString(w, "synthetic proxy body after commit")
						return
					}
					if attempt == 1 && Text(stub, "crash") == "any" {
						conn, _, e := w.(http.Hijacker).Hijack()
						if e != nil {
							t.Error(e)
							return
						}
						_ = conn.Close()
						return
					}
					status := 201
					if !out.Created {
						status = 200
					}
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(out)
					return
				}
				sum, e := c.s.Summary(job, c.e.Now)
				if e != nil {
					t.Error(e)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/records") {
					_ = json.NewEncoder(w).Encode(RecordsResponse{Contract: Contract, Snapshot: sum.SnapshotSeq, Next: sum.SnapshotSeq, Records: c.s.Jobs[job], Summary: sum})
					return
				}
				_ = json.NewEncoder(w).Encode(JobResponse{Contract: Contract, Claim: *c.s.Claim(job), Summary: sum})
			})
			server := httptest.NewServer(handler)
			defer server.Close()
			if Text(stub, "response") == "unavailable" {
				server.Close()
			}
			if Text(stub, "crash") == "any" {
				code, out, errout := runWrkCLI(t, binary, server.URL, argv, input)
				if code != 75 || out != "" || errout != "handoffkeep wrk: transport_indeterminate\n" {
					t.Fatal("lost committed reply did not remain indeterminate")
				}
			}
			code, out, errout := runWrkCLI(t, binary, server.URL, argv, input)
			clientOnly := v.Source.Table == "transport" && (v.Source.Row == 1 || v.Source.Row == 2 || v.Source.Row == 8 || v.Source.Row == 11)
			success := clientOnly || Text(v.Expect, "outcome") == "intent_retry" || (v.Expect["exit_code"] != nil && Num(v.Expect, "exit_code") == 0)
			if success {
				if code != 0 || errout != "" || !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 || !json.Valid([]byte(out)) {
					t.Fatalf("CLI success contract: exit=%d stderr=%q", code, errout)
				}
				if stub["replay"] == true || Text(stub, "crash") == "any" {
					var got WriteResponse
					stateMu.Lock()
					count := len(c.s.Jobs[job])
					stateMu.Unlock()
					if json.Unmarshal([]byte(out), &got) != nil || got.Created || count != 4 {
						t.Fatal("successful-key replay charged again")
					}
				}
			} else {
				if code != int(Num(v.Expect, "exit_code")) || out != "" || errout != "handoffkeep wrk: "+Text(v.Expect, "stderr_code")+"\n" {
					t.Fatalf("CLI failure contract: exit=%d stdout_len=%d stderr=%q", code, len(out), errout)
				}
			}
			if Text(stub, "response") == "html_5xx" {
				recovered, reply, diagnostic := runWrkCLI(t, binary, server.URL, argv, input)
				var replay WriteResponse
				stateMu.Lock()
				count := len(c.s.Jobs[job])
				stateMu.Unlock()
				if recovered != 0 || diagnostic != "" || json.Unmarshal([]byte(reply), &replay) != nil || replay.Created || count != 4 {
					t.Fatal("proxy-lost acceptance did not replay the original write exactly once")
				}
			}
			wantCalls := int32(1)
			if Text(stub, "crash") == "any" || Text(stub, "response") == "html_5xx" {
				wantCalls = 2
			}
			if Text(stub, "response") == "unavailable" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls || forwarded.Load() != 0 {
				t.Fatalf("network attempts=%d forwarded=%d", calls.Load(), forwarded.Load())
			}
		})
	}
}

func TestWrkCLICommandsAndArguments(t *testing.T) {
	binary := buildWrkCLI(t)
	c := fresh(t)
	c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	record := c.must("desktop", "subject", "verdict", evidence(1, h1, "/cli/content", "immutable bytes", nil))
	var calls atomic.Int32
	var stateMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateMu.Lock()
		defer stateMu.Unlock()
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("installed authentication missing")
		}
		if r.Method == "POST" {
			raw, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
				return
			}
			request, e := ParseRequest(raw)
			if e != nil {
				t.Error(e)
				return
			}
			out, e := c.send("desktop", "subject", r.Header.Get("Idempotency-Key"), request)
			if e != nil {
				t.Error(e)
				return
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		sum, e := c.s.Summary("subject", c.e.Now)
		if e != nil {
			t.Error(e)
			return
		}
		switch {
		case r.URL.Path == "/v1/wrk/capabilities":
			_ = json.NewEncoder(w).Encode(Capabilities{Contract: Contract, Generation: c.e.Generation, ClientID: "desktop", PolicySHA: c.e.Policy.Digest(), Writes: true})
		case r.URL.Path == "/v1/wrk/jobs/subject":
			_ = json.NewEncoder(w).Encode(JobResponse{Contract: Contract, Claim: *c.s.Claim("subject"), Summary: sum})
		case strings.HasSuffix(r.URL.Path, "/content"):
			b := c.s.Contents["subject"][record.Seq]
			_ = json.NewEncoder(w).Encode(ContentResponse{JobID: "subject", Seq: record.Seq, Base64: base64.StdEncoding.EncodeToString(b), SHA: SHA(b), Size: len(b)})
		case strings.HasSuffix(r.URL.Path, "/records"):
			after, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
			limit := int64(100)
			if q := r.URL.Query().Get("limit"); q != "" {
				limit, _ = strconv.ParseInt(q, 10, 64)
			}
			rs := []Record{}
			next := after
			for _, rec := range c.s.Jobs["subject"] {
				if rec.Seq > after && int64(len(rs)) < limit {
					rs = append(rs, rec)
					next = rec.Seq
				}
			}
			_ = json.NewEncoder(w).Encode(RecordsResponse{Contract: Contract, Snapshot: sum.SnapshotSeq, Next: next, More: next < sum.SnapshotSeq, Records: rs, Summary: sum})
		default:
			t.Error("unexpected CLI request path")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	for _, args := range [][]string{{"capabilities"}, {"job", "subject"}, {"records", "subject"}, {"records", "subject", "--after-seq", "2", "--snapshot-seq", "4", "--limit", "1"}, {"content", "subject", "4"}} {
		before := calls.Load()
		code, out, stderr := runWrkCLI(t, binary, server.URL, args, nil)
		if code != 0 || stderr != "" || !json.Valid([]byte(out)) || strings.Count(out, "\n") != 1 || calls.Load() != before+1 {
			t.Fatalf("%s success binding failed: exit=%d", args[0], code)
		}
	}
	key := NewUUID()
	args := []string{"append", "subject", "--kind", "round_open", "--idempotency-key", key, "--expected-seq", "4", "--claim-seq", "1"}
	input, _ := json.Marshal(opening(h2, "", nil, nil))
	code, out, stderr := runWrkCLI(t, binary, server.URL, args, input)
	if code != 0 || stderr != "" {
		t.Fatalf("append binding: %d", code)
	}
	var written WriteResponse
	if json.Unmarshal([]byte(out), &written) != nil || !written.Created || written.Record.Key != key || written.Record.Seq != 5 {
		t.Fatal("append request changed key/CAS/body")
	}
	invalid := []struct {
		args  []string
		input string
		code  string
	}{
		{[]string{"job", "subject", "extra"}, "", "cli_usage"},
		{[]string{"job", "subject", "--url", "https://example.invalid"}, "", "cli_usage"},
		{[]string{"job", "subject", "--token", "synthetic"}, "", "cli_usage"},
		{[]string{"job", "subject", "--token-path", "/synthetic"}, "", "cli_usage"},
		{[]string{"job", "subject", "--transport", "alternate"}, "", "cli_usage"},
		{[]string{"records", "subject", "--limit", "1", "--limit", "2"}, "", "cli_usage"},
		{[]string{"records", "subject", "--after-seq", "-1"}, "", "cli_usage"},
		{[]string{"content", "subject", "0"}, "", "cli_usage"},
		{[]string{"capabilities"}, "\n", "invalid_request"},
		{args, `{"head":"` + h2 + `","head":"` + h3 + `"}`, "invalid_request"},
		{args, `{"head":"` + h2 + `","tester_job_id":null,"extension_grant_seq":null,"repeat_grant_seq":null,"extra":true}`, "invalid_request"},
	}
	for _, bad := range invalid {
		before := calls.Load()
		code, out, stderr := runWrkCLI(t, binary, server.URL, bad.args, []byte(bad.input))
		if code != Exit(bad.code) || out != "" || stderr != "handoffkeep wrk: "+bad.code+"\n" || calls.Load() != before {
			t.Fatalf("local argument/payload refusal: %s exit=%d", bad.code, code)
		}
	}
}
