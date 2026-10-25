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
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect-target" {
					forwarded.Add(1)
					w.WriteHeader(500)
					return
				}
				calls.Add(1)
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
				case "html_5xx":
					w.Header().Set("Content-Type", "text/html")
					w.WriteHeader(502)
					_, _ = io.WriteString(w, "synthetic proxy body")
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
			code, out, errout := runWrkCLI(t, binary, server.URL, argv, input)
			clientOnly := v.Source.Table == "transport" && (v.Source.Row == 1 || v.Source.Row == 2 || v.Source.Row == 8 || v.Source.Row == 11)
			success := clientOnly || Text(v.Expect, "outcome") == "intent_retry" || (v.Expect["exit_code"] != nil && Num(v.Expect, "exit_code") == 0)
			if success {
				if code != 0 || errout != "" || !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 || !json.Valid([]byte(out)) {
					t.Fatalf("CLI success contract: exit=%d stderr=%q", code, errout)
				}
				if stub["replay"] == true {
					var got WriteResponse
					if json.Unmarshal([]byte(out), &got) != nil || got.Created || len(c.s.Jobs[job]) != 4 {
						t.Fatal("successful-key replay charged again")
					}
				}
			} else {
				if code != int(Num(v.Expect, "exit_code")) || out != "" || errout != "handoffkeep wrk: "+Text(v.Expect, "stderr_code")+"\n" {
					t.Fatalf("CLI failure contract: exit=%d stdout_len=%d stderr=%q", code, len(out), errout)
				}
			}
			wantCalls := int32(1)
			if Text(stub, "response") == "unavailable" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls || forwarded.Load() != 0 {
				t.Fatalf("network attempts=%d forwarded=%d", calls.Load(), forwarded.Load())
			}
		})
	}
}
