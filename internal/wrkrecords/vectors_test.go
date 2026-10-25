package wrkrecords

import (
	"encoding/base64"
	"encoding/json"
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

// pack_divergence is populated only for independently reproduced design/pack
// contradictions under director-1's 2026-10-26 ruling. Each entry must remain
// a difference until M1 repairs the fixture. There is no fallback expectation.
type divergence struct {
	Category, Section, Rule string
	Result                  Object
}

var pack_divergence = map[string]divergence{
	"head-repeat-binding.json/exact-b-open-verdict-null-tester-fails-assignment": {"design_result", "4,5", "round_before_identity", Object{"http_status": 409, "error": "round_has_verdict"}},
	"failure-abuse.json/replay-old-verdict-identical-200":                        {"design_result", "4,5", "request_digest", Object{"http_status": 409, "error": "idempotency_conflict"}},
	"failure-abuse.json/replay-old-verdict-fresh-key-same-path":                  {"design_result", "4", "round_before_identity", Object{"http_status": 409, "error": "round_has_verdict"}},
	"failure-abuse.json/two-testers-one-head-hk1901":                             {"design_result", "4,5", "grant_locator", Object{"http_status": 409, "error": "grant_invalid"}},
	"failure-abuse.json/grant-consumer-race-void-reuse":                          {"design_result", "4,5", "cas", Object{"http_status": 409, "error": "sequence_conflict"}},
	"failure-abuse.json/committed-write-lost-reply-replay-repairs":               {"design_result", "4,5", "request_digest", Object{"http_status": 409, "error": "idempotency_conflict"}},
	"head-repeat-binding.json/reviewer-end-after-pass-join-unaffected":           {"design_result", "4,5", "cas", Object{"http_status": 409, "error": "sequence_conflict"}},
	"head-repeat-binding.json/voided-open-verdict-cannot-copy-old-path":          {"design_result", "4,5", "cas", Object{"http_status": 409, "error": "sequence_conflict"}},
	"r4-scoped-claim.json/scoped-mint-foreign-job-host-mismatch":                 {"design_result", "2,5", "missing_target", Object{"http_status": 404, "error": "not_found"}},
	"r4-scoped-claim.json/scoped-mint-reused-job-id-claim-conflict":              {"design_result", "4,5", "cas", Object{"http_status": 409, "error": "sequence_conflict"}},
	"transition.json/replay-verdict-after-end-200":                               {"design_result", "4,5", "request_digest", Object{"http_status": 409, "error": "idempotency_conflict"}},
	"transition.json/same-bytes-fresh-path-duplicate-content":                    {"design_result", "2,4,5", "parsed_head", Object{"http_status": 409, "error": "head_mismatch"}},
	"failure-abuse.json/planted-legacy-events-import-untrusted":                  {"fixture_invalid", "5", "seal_floor", nil},
	"migration.json/post-activate-verdict-old-round-not-open":                    {"fixture_invalid", "5", "seal_floor", nil},
	"migration.json/post-activate-void-old-round-not-open":                       {"fixture_invalid", "5", "seal_floor", nil},
	"migration.json/post-activate-extension-open-round-4":                        {"fixture_invalid", "5", "seal_floor", nil},
	"migration.json/void-postactivate-round-4-used-3":                            {"fixture_invalid", "5", "seal_floor", nil},
	"r3-regression.json/postactivate-open-historical-head-no-repeat":             {"fixture_invalid", "5", "seal_floor", nil},
	"transition.json/imported-activated-join-zero-rounds":                        {"fixture_invalid", "5", "seal_floor", nil},
	"transition.json/imported-activated-open-needs-extension":                    {"fixture_invalid", "5", "seal_floor", nil},
}

func fixtureInvalid(t *testing.T, v vector) bool {
	t.Helper()
	entry, ok := pack_divergence[v.file+"/"+v.Name]
	bad := false
	for _, r := range v.Given.Records {
		if r.Kind == "import_seal" && Num(r.Payload, "numbering_floor") < Num(r.Payload, "baseline_used") {
			bad = true
		}
	}
	if bad && (!ok || entry.Category != "fixture_invalid") {
		t.Fatal("unlisted invalid seal fixture")
	}
	if ok && entry.Category == "fixture_invalid" {
		if !bad {
			t.Fatal("stale fixture_invalid entry: M1 repaired seal; remove entry")
		}
		return true
	}
	return false
}
func TestPackDivergenceTable(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range loadVectors(t) {
		key := v.file + "/" + v.Name
		d, ok := pack_divergence[key]
		if !ok {
			continue
		}
		seen[key] = true
		t.Run(key, func(t *testing.T) {
			if d.Section == "" {
				t.Fatal("missing design citation")
			}
			if fixtureInvalid(t, v) {
				return
			}
			if d.Category != "design_result" || reflect.DeepEqual(d.Result, v.Expect) {
				t.Fatal("invalid/stale divergence entry")
			}
			if Num(d.Result, "http_status") == Num(v.Expect, "http_status") && Text(d.Result, "error") == Text(v.Expect, "error") {
				t.Fatal("pack repaired expected outcome; remove entry")
			}
			r := v.Request
			b := Obj(r, "body")
			parts := strings.Split(Text(r, "path"), "/")
			job := parts[4]
			tail := int64(0)
			for _, x := range v.Given.Records {
				if x.JobID == job && x.Seq > tail {
					tail = x.Seq
				}
			}
			switch d.Rule {
			case "cas":
				if Num(b, "expected_seq") == tail {
					t.Fatal("stale CAS divergence")
				}
			case "request_digest":
				digest, e := Digest(Object{"method": "POST", "path": r["path"], "body": b})
				if e != nil {
					t.Fatal(e)
				}
				found := false
				for _, x := range v.Given.Records {
					if x.JobID == job && x.Actor == Text(r, "actor") && x.Key == Text(r, "idempotency_key") {
						found = x.RequestSHA != digest
					}
				}
				if !found {
					t.Fatal("stale request digest divergence")
				}
			case "missing_target":
				target := Text(Obj(Obj(b, "payload"), "review_of"), "job_id")
				for _, x := range v.Given.Records {
					if x.JobID == target {
						t.Fatal("target now exists; remove entry")
					}
				}
			case "grant_locator":
				seq := Num(Obj(b, "payload"), "repeat_grant_seq")
				for _, x := range v.Given.Records {
					if x.JobID == job && x.Seq == seq && x.Kind == "grant" {
						t.Fatal("grant locator repaired; remove entry")
					}
				}
			case "round_before_identity":
				n := Num(Obj(b, "payload"), "round_no")
				found := false
				for _, x := range v.Given.Records {
					if x.JobID == job && x.Kind == "verdict" && Num(x.Payload, "round_no") == n {
						found = true
					}
				}
				if !found {
					t.Fatal("existing verdict removed; remove entry")
				}
			case "parsed_head":
				p := Obj(b, "payload")
				bytes, e := base64.StdEncoding.DecodeString(Text(p, "content_b64"))
				if e != nil {
					t.Fatal(e)
				}
				_, head, e := ParseVerdict(bytes)
				if e != nil || head == Text(p, "head") {
					t.Fatal("head assertion repaired; remove entry")
				}
			default:
				t.Fatal("unrecognized divergence proof")
			}
		})
	}
	if len(seen) != len(pack_divergence) {
		t.Fatal("divergence table contains unknown cases")
	}
}

func TestHTTPVectors(t *testing.T) {
	vs := loadVectors(t)
	catalog := contentCatalog(t, vs)
	for _, v := range vs {
		if Text(v.Request, "type") != "http" || Text(v.Request, "method") != "POST" {
			continue
		}
		t.Run(strings.TrimSuffix(v.file, ".json")+"/"+v.Name, func(t *testing.T) {
			if fixtureInvalid(t, v) {
				return
			}
			s, p := seedVector(t, v, catalog)
			raw, _ := json.Marshal(v.Request["body"])
			r, e := ParseRequest(raw)
			status := Status(e)
			var resp WriteResponse
			if e == nil {
				mode := "enforce"
				if v.Name == "p1-shadow-central-reviewer-claim-starts" {
					mode = "shadow"
				}
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
			if override, ok := pack_divergence[v.file+"/"+v.Name]; ok {
				expect = override.Result
			}
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
