package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// The O3 contract harness (hk 1925): every vector of the M1 pack under
// testdata/o3/vectors runs against this implementation. The pack is never
// edited; vectors that contradict the fixed design are listed in
// packDivergence (director-1 ruling on esc-1) and vectors whose observable
// belongs to the wrk client (C1) are listed in c1OwnedObservables
// (director-1 6b test-scope ruling).

const packRoot = "../../testdata/o3"

// packGeneration is the store_generation every pack record carries.
const packGeneration = "00000000-0000-4000-8000-ffff00000001"

// packNow is the fixed server clock for vectors: after every seeded
// received_at and before every seeded grant expiry.
var packNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type packCase struct {
	File   string
	Name   string `json:"name"`
	Source struct {
		Section   string `json:"section"`
		TableName string `json:"table_name"`
		Row       int    `json:"row"`
		RowText   string `json:"row_text"`
	} `json:"source"`
	Given struct {
		JobID   string          `json:"job_id"`
		Policy  json.RawMessage `json:"policy"`
		Records []wrk.Record    `json:"records"`
	} `json:"given"`
	Request map[string]any `json:"request"`
	Expect  map[string]any `json:"expect"`
}

func loadPackCases(t *testing.T) []*packCase {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(packRoot, "vectors", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("pack vectors: %v", err)
	}
	sort.Strings(files)
	var out []*packCase
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var vf struct {
			Cases []json.RawMessage `json:"cases"`
		}
		if err := json.Unmarshal(b, &vf); err != nil {
			t.Fatal(err)
		}
		for _, raw := range vf.Cases {
			c := &packCase{File: filepath.Base(f)}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(c); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			for i := range c.Given.Records {
				c.Given.Records[i].Payload = normalizeNumbers(c.Given.Records[i].Payload).(map[string]any)
			}
			out = append(out, c)
		}
	}
	return out
}

// normalizeNumbers turns json.Number into int64 so seeded payloads share
// the DecodeStrict shape the engine reads.
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeNumbers(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = normalizeNumbers(e)
		}
		return x
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			panic(err)
		}
		return n
	}
	return v
}

func (c *packCase) id() string { return c.File + "/" + c.Name }

func (c *packCase) reqType() string { s, _ := c.Request["type"].(string); return s }

// packReportBytes is the report every seeded completed/joined record
// attests (the pack's report_b64).
var packReportBytes = []byte("pack report bytes\n")

func packVerdictBytes(verdict, head string) []byte {
	return []byte(fmt.Sprintf("VERDICT: %s @%s\nreview notes\n", verdict, head))
}

// legacyBySHA maps archived legacy event digests to their exact bytes.
func legacyBySHA(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	files, _ := filepath.Glob(filepath.Join(packRoot, "legacy", "*", "events", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		out[hex.EncodeToString(sum[:])] = b
	}
	return out
}

// readLegacy returns one archived legacy event fixture's exact bytes.
func readLegacy(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(packRoot, "legacy", dir, "events", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seedContent reconstructs the stored bytes of a seeded content-bearing
// record; the pack gives only digests, and every one is reproducible.
func seedContent(t *testing.T, legacy map[string][]byte, r wrk.Record) []byte {
	t.Helper()
	p := r.Payload
	var b []byte
	var want string
	switch r.Kind {
	case "verdict":
		b = packVerdictBytes(p["verdict"].(string), p["head"].(string))
		want = p["content_sha256"].(string)
	case "completed", "joined":
		b, want = packReportBytes, p["report_sha256"].(string)
	case "import":
		want = p["source_sha256"].(string)
		b = legacy[want]
	default:
		return nil
	}
	if wrk.ContentSHA256(b) != want {
		t.Fatalf("seeded %s %s#%d: cannot reproduce content %s", r.Kind, r.JobID, r.Seq, want)
	}
	return b
}

func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "tk" + hex.EncodeToString(b)
}

// packTaskIDs lists every task id the case's history or request names, so
// the harness can create those tasks (tasks are external to the journal).
func packTaskIDs(c *packCase) []int64 {
	seen := map[int64]bool{}
	for _, r := range c.Given.Records {
		if n, ok := r.Payload["task_id"].(int64); ok {
			seen[n] = true
		}
	}
	if body, ok := c.Request["body"].(map[string]any); ok {
		if p, ok := body["payload"].(map[string]any); ok {
			if n, ok := p["task_id"].(json.Number); ok {
				v, _ := n.Int64()
				seen[v] = true
			}
		}
	}
	var out []int64
	for n := range seen {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// packEnv is one isolated server for one vector.
type packEnv struct {
	t       *testing.T
	backend wrk.Backend
	mem     *wrk.Memory
	server  Server
	handler http.Handler
	tokens  map[string]string
	policy  *wrk.Policy
	records func(job string) []wrk.Record
}

func newPackEnv(t *testing.T, c *packCase, legacy map[string][]byte, recovery bool) *packEnv {
	t.Helper()
	policy, err := wrk.ParsePolicy(c.Given.Policy)
	if err != nil {
		t.Fatalf("%s: given policy invalid: %v", c.id(), err)
	}
	mem := wrk.NewMemory()
	for _, id := range packTaskIDs(c) {
		mem.AddTask(id)
	}
	for _, r := range c.Given.Records {
		mem.Seed(r, seedContent(t, legacy, r))
	}
	// Seeded builder claims hold their task bindings.
	for _, r := range c.Given.Records {
		if r.Kind == "claim" && (r.Payload["role"] == "builder" || r.Payload["role"] == "worker") {
			_ = mem.Update(t.Context(), false, nil, func(tx wrk.Tx) error {
				return tx.BindTask(r.Payload["task_id"].(int64), r.JobID, r.JobID, 0)
			})
		}
	}
	tokens := Tokens{}
	byClient := map[string]string{}
	for _, id := range append(append([]string{}, wrk.ClientIDs...), "unknown-host") {
		tok := randomToken(t)
		tokens[id] = tok
		byClient[id] = tok
	}
	eng := &wrk.Engine{Policy: policy, Generation: packGeneration, Enforce: true, Now: func() time.Time { return packNow }}
	s := Server{Tokens: tokens, Wrk: &Wrk{Mode: WrkEnforce, Recovery: recovery, Policy: policy, Engine: eng, Backend: mem}}
	return &packEnv{t: t, backend: mem, mem: mem, server: s, handler: s.Handler(), tokens: byClient, policy: policy, records: mem.Records}
}

type packResponse struct {
	status int
	body   map[string]any
	raw    []byte
}

func (e *packEnv) do(actor, method, path string, header map[string]string, body []byte) packResponse {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if actor != "" {
		req.Header.Set("Authorization", "Bearer "+e.tokens[actor])
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	out := packResponse{status: rec.Code, raw: rec.Body.Bytes()}
	_ = json.Unmarshal(out.raw, &out.body)
	return out
}

// packRequestBody builds the exact request bytes of an http/http_raw case.
// The guard vector's content is generated at runtime (never committed).
func packRequestBody(t *testing.T, c *packCase) []byte {
	t.Helper()
	body, ok := c.Request["body"].(map[string]any)
	if !ok {
		return nil
	}
	if p, ok := body["payload"].(map[string]any); ok {
		if _, ok := p["content_b64_construct"]; ok {
			delete(p, "content_b64_construct")
			head := p["head"].(string)
			content := append([]byte("VERDICT: PASS @"+head+"\n"), syntheticSecret(t)...)
			p["content_b64"] = base64.StdEncoding.EncodeToString(content)
			p["content_sha256"] = wrk.ContentSHA256(content)
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// syntheticSecret builds a token-shaped line internal/guard recognises from
// random bytes at test runtime; no token-shaped literal is committed.
func syntheticSecret(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return []byte("gh" + "p_" + strings.ToUpper(hex.EncodeToString(b))[:36] + "\n")
}

// subsetMismatch reports the first expected key whose value differs.
func subsetMismatch(want map[string]any, got map[string]any) string {
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			return fmt.Sprintf("missing %s", k)
		}
		if wm, ok := wv.(map[string]any); ok {
			gm, ok := gv.(map[string]any)
			if !ok {
				return fmt.Sprintf("%s not an object", k)
			}
			if m := subsetMismatch(wm, gm); m != "" {
				return k + "." + m
			}
			continue
		}
		if !jsonEqual(wv, gv) {
			return fmt.Sprintf("%s=%v want %v", k, gv, wv)
		}
	}
	return ""
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(normalizeAny(a))
	jb, _ := json.Marshal(normalizeAny(b))
	return bytes.Equal(ja, jb)
}

func normalizeAny(v any) any {
	b, _ := json.Marshal(v)
	var out any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

var _ = reflect.DeepEqual
