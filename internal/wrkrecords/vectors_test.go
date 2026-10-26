package wrkrecords_test

import (
	"encoding/base64"
	"encoding/json"
	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type vector struct {
	Name   string `json:"name"`
	Source struct {
		Section string `json:"section"`
		Table   string `json:"table_name"`
		Row     int    `json:"row"`
	} `json:"source"`
	Given struct {
		Job     string          `json:"job_id"`
		Policy  json.RawMessage `json:"policy"`
		Records []Record        `json:"records"`
	} `json:"given"`
	Request Object `json:"request"`
	Expect  Object `json:"expect"`
	file    string
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	files, e := filepath.Glob("../../testdata/o3/vectors/*.json")
	if e != nil {
		t.Fatal(e)
	}
	out := []vector{}
	for _, f := range files {
		b, e := os.ReadFile(f)
		if e != nil {
			t.Fatal(e)
		}
		var pack struct {
			Cases []vector `json:"cases"`
		}
		if e = json.Unmarshal(b, &pack); e != nil {
			t.Fatal(e)
		}
		for _, v := range pack.Cases {
			v.file = filepath.Base(f)
			out = append(out, v)
		}
	}
	return out
}
func contentCatalog(t *testing.T, vs []vector) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, v := range vs {
		// M1's committed generator (scripts/o3gen/genlib.py:verdict_bytes)
		// supplies this exact default body for seeded historical verdicts.
		// Verify the digest before using bytes; never fabricate matching state.
		for _, r := range v.Given.Records {
			if r.Kind == "verdict" {
				for _, body := range []string{"review notes\n", "prior review\n"} {
					// prior review is the exact historical body in gen.py's
					// voided_open_prior_verdict_given.
					b := []byte("VERDICT: " + Text(r.Payload, "verdict") + " @" + Text(r.Payload, "head") + "\n" + body)
					if SHA(b) == Text(r.Payload, "content_sha256") {
						out[SHA(b)] = b
					}
				}
			}
		}
		p := Obj(Obj(v.Request, "body"), "payload")
		for _, key := range []string{"content_b64", "report_b64"} {
			if s := Text(p, key); s != "" {
				b, e := base64.StdEncoding.DecodeString(s)
				if e != nil {
					t.Fatal(e)
				}
				out[SHA(b)] = b
			}
		}
	}
	return out
}
func seedVector(t *testing.T, v vector, catalog map[string][]byte) (Snapshot, *Policy) {
	t.Helper()
	p, e := ParsePolicy(v.Given.Policy)
	if e != nil {
		t.Fatalf("policy: %v", e)
	}
	s := Snapshot{Jobs: map[string][]Record{}, Contents: map[string]map[int64][]byte{}}
	for _, r := range v.Given.Records {
		s.Jobs[r.JobID] = append(s.Jobs[r.JobID], r)
		if s.Contents[r.JobID] == nil {
			s.Contents[r.JobID] = map[int64][]byte{}
		}
		if r.Kind == "verdict" {
			b, ok := catalog[Text(r.Payload, "content_sha256")]
			if !ok {
				t.Fatal("missing exact verdict fixture bytes")
			}
			s.Contents[r.JobID][r.Seq] = b
		}
		if r.Kind == "completed" || r.Kind == "joined" {
			b, ok := catalog[Text(r.Payload, "report_sha256")]
			if !ok || int64(len(b)) != Num(r.Payload, "report_size_bytes") {
				t.Fatal("missing exact report fixture bytes")
			}
			s.Contents[r.JobID][r.Seq] = b
		}
		if r.Kind == "import" {
			b, e := os.ReadFile(filepath.Join("../..", Text(r.Payload, "source_locator")))
			if e != nil {
				t.Fatal(e)
			}
			if SHA(b) != Text(r.Payload, "source_sha256") {
				t.Fatal("import fixture digest")
			}
			s.Contents[r.JobID][r.Seq] = b
		}
	}
	return s, p
}
func normalized(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
func subset(t *testing.T, expected, actual any, where string) {
	t.Helper()
	expected, actual = normalized(expected), normalized(actual)
	if m, ok := expected.(map[string]any); ok {
		a, ok := actual.(map[string]any)
		if !ok {
			t.Fatalf("%s: expected object got %v", where, actual)
		}
		for k, v := range m {
			subset(t, v, a[k], where+"."+k)
		}
		return
	}
	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("%s: got %v want %v", where, actual, expected)
	}
}

// All M1 hk 2049 repairs are merged. Any exception requires a fresh ruling.
var pack_divergence = map[string]struct{}{}

func TestPackDivergenceTable(t *testing.T) {
	if len(pack_divergence) != 0 {
		t.Fatal("revised M1 must run without divergences")
	}
}

func TestEngineAppendVectors(t *testing.T) {
	vs := loadVectors(t)
	catalog := contentCatalog(t, vs)
	for _, v := range vs {
		if (Text(v.Request, "type") != "http" && Text(v.Request, "type") != "http_raw") || Text(v.Request, "method") != "POST" {
			continue
		}
		t.Run(strings.TrimSuffix(v.file, ".json")+"/"+v.Name, func(t *testing.T) {
			s, p := seedVector(t, v, catalog)
			raw := vectorBody(t, v)
			r, e := ParseRequest(raw)
			status := Status(e)
			var resp WriteResponse
			if e == nil {
				mode := "enforce"
				engine := Engine{Policy: p, Mode: mode, Now: time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC), Generation: "00000000-0000-4000-8000-ffff00000001"}
				parts := strings.Split(Text(v.Request, "path"), "/")
				resp, _, e = engine.Append(s, Text(v.Request, "actor"), parts[4], Text(v.Request, "idempotency_key"), r)
				if e == nil {
					status = 201
					if !resp.Created {
						status = 200
					}
				} else {
					status = Status(e)
				}
			}
			expect := v.Expect
			if int64(status) != Num(expect, "http_status") {
				t.Fatalf("status %d code %v want %v", status, e, expect)
			}
			if e != nil {
				if Code(e) != Text(expect, "error") {
					t.Errorf("code %s want %s", Code(e), Text(expect, "error"))
				}
				return
			}
			if x, ok := expect["created"]; ok && x != resp.Created {
				t.Errorf("created %v want %v", resp.Created, x)
			}
			if x, ok := expect["record"]; ok {
				subset(t, x, resp.Record, "record")
			}
			if x, ok := expect["record_ref"]; ok {
				subset(t, x, resp.Record, "record_ref")
			}
			if x, ok := expect["summary"]; ok {
				subset(t, x, resp.Summary, "summary")
			}
		})
	}
}
