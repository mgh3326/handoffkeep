package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// metaRules executes each section 4 normative rule vector. Rule 9 needs the
// legacy routes and runs in the store-backed suite (metaStoreRules).
var metaRules = map[string]func(t *testing.T){
	"rule-01-any-post-record-uses-request-shape": func(t *testing.T) {
		s := newScenario(t)
		for _, raw := range []string{`[]`, `{"claim_seq":0,"expected_seq":0,"payload":{}}`, `"claim"`} {
			r := s.do("director-1", "POST", "/v1/wrk/jobs/job-r1/records", map[string]string{"Idempotency-Key": s.key()}, []byte(raw))
			s.refused(r, "invalid_request", raw)
		}
	},
	"rule-02-closed-request-union": func(t *testing.T) {
		s := newScenario(t)
		s.refused(s.postBody("director-1", "job-r2", s.key(), map[string]any{"kind": "promote", "claim_seq": 0, "expected_seq": 0, "payload": map[string]any{}}), "invalid_request", "unknown kind")
		s.refused(s.postBody("director-1", "job-r2", s.key(), map[string]any{"kind": "claim", "claim_seq": 0, "expected_seq": 0, "payload": claimPayload("job-r2", "b2400-r", "director-1", 2400, "desktop", "builder", "T1"), "extra": true}), "invalid_request", "unknown top-level field")
		r := s.do("director-1", "POST", "/v1/wrk/jobs/job-r2/records", map[string]string{"Idempotency-Key": s.key()}, []byte(`{"kind":"claim","kind":"claim","claim_seq":0,"expected_seq":0,"payload":{}}`))
		s.refused(r, "invalid_request", "duplicate key")
	},
	"rule-03-successful-replay-returns-original": func(t *testing.T) {
		TestWrkReplayPrecedesCAS(t)
	},
	"rule-04-digest-rfc8785-method-path-body": func(t *testing.T) {
		s := newScenario(t)
		p := claimPayload("job-r4", "b2400-r", "director-1", 2400, "desktop", "builder", "T1")
		r := s.ok(s.postBody("director-1", "job-r4", s.key(), map[string]any{"payload": p, "expected_seq": 0, "kind": "claim", "claim_seq": 0}), "claim")
		// Independent RFC 8785 text: members sorted, no whitespace, headers excluded.
		want := `{"body":{"claim_seq":0,"expected_seq":0,"kind":"claim","payload":{"host_client_id":"desktop","job_id":"job-r4","owner_lane":"b2400-r","parent_lane":"director-1","predecessor_job_id":null,"review_of":null,"role":"builder","supersedes":null,"task_id":2400,"tier":"T1"}},"method":"POST","path":"/v1/wrk/jobs/job-r4/records"}`
		sum := sha256.Sum256([]byte(want))
		if got := r["record"].(map[string]any)["request_sha256"]; got != hex.EncodeToString(sum[:]) {
			t.Fatalf("request_sha256 %v", got)
		}
	},
	"rule-05-verdict-first-line-grammar": func(t *testing.T) {
		h := strings.Repeat("a", 40)
		for text, ok := range map[string]bool{
			"VERDICT: PASS @" + h + "\nbody\n":                         true,
			"VERDICT: BLOCKER @" + strings.ToUpper(h) + "\r\nbody":     true,
			"VERDICT: PASS @" + h + " \t\n":                            true,
			"VERDICT: PASS @" + h:                                      true,
			"VERDICT: PASS @" + h + "\nbody \u00a0\u3000 x\r y\n":      true,
			"\ufeffVERDICT: PASS @" + h + "\n":                         false,
			"preamble\nVERDICT: PASS @" + h + "\n":                     false,
			"VERDICT: PASS @" + h + "a\n":                              false,
			"VERDICT: PASS @" + h + "\nVERDICT: BLOCKER @" + h + "\n":  false,
			"VERDICT: PASS @" + h + "\nVERDICT: junk\n":                false,
			"VERDICT: PASS @" + h + "\u00a0\n":                         false,
			"VERDICT: PASS @" + h + "\rbody\n":                         false,
			"VERDICT: PASS @" + h + "\r":                               false,
			"verdict: PASS @" + h + "\n":                               false,
			"VERDICT: PASS @" + h + "\n" + string([]byte{0xff}) + "\n": false,
		} {
			_, got, valid := wrk.ParseVerdict([]byte(text))
			if valid != ok || (ok && got != h) {
				t.Errorf("ParseVerdict(%q) = %v %q, want %v", text, valid, got, ok)
			}
		}
	},
	"rule-06-baseline-conservative-accounting": func(t *testing.T) {
		TestWrkPostImportNumberingAndBrokenSeal(t)
		TestWrkPlantedLegacyEarnsNoTrust(t)
	},
	"rule-07-head-repeat-enabled-required": func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal(standardPolicy(t), &doc); err != nil {
			t.Fatal(err)
		}
		doc["features"].(map[string]any)["head_repeat_enabled"] = false
		b, _ := json.Marshal(doc)
		if _, err := wrk.ParsePolicy(b); err == nil {
			t.Fatal("policy with head_repeat_enabled=false was accepted")
		}
	},
	"rule-08-review-role-no-round-budget": func(t *testing.T) {
		s := newScenario(t)
		s.activeBuilder("job-r8", "b2400-r", 2400, "T2")
		before := summaryOf(s.do("desktop", "GET", "/v1/wrk/jobs/job-r8", nil, nil))
		s.ok(s.post("desktop", "job-r8-t", "claim", reviewPayload("job-r8-t", "b2400-r", 2400, "desktop", "tester", "T2", "job-r8", 1)), "mint")
		after := summaryOf(s.do("desktop", "GET", "/v1/wrk/jobs/job-r8", nil, nil))
		if before["used"] != after["used"] || before["next_round_no"] != after["next_round_no"] || before["snapshot_seq"] != after["snapshot_seq"] {
			t.Fatalf("mint changed the target: %v -> %v", before, after)
		}
		s.refused(s.post("desktop", "job-r8-t", "round_open", openPayload(head('1'), nil, nil, nil)), "scope_denied", "reviewer opens a round")
		s.refused(s.post("director-1", "job-r8-t", "grant", grantPayload("extension", 1, head('1'), 1, nil, nil)), "scope_denied", "grant on a reviewer job")
		o := s.ok(s.post("desktop", "job-r8", "round_open", openPayload(head('1'), "job-r8-t", nil, nil)), "target open")
		if num(summaryOf(o)["used"]) != 1 {
			t.Fatalf("target open charged %v", summaryOf(o)["used"])
		}
	},
}

// metaStoreRules are rule vectors executed by the store-backed suite.
var metaStoreRules = map[string]bool{"rule-09-protected-transitions-need-receipt": true}

// metaFailure executes the S1 half of the section 8 failure rows whose
// vectors are type meta. The client half of row 7 (history_regressed) is
// C1's and is listed in cmd/handoffkeep c1OwnedObservables.
var metaFailure = map[string]func(t *testing.T){
	"server-loses-acknowledged-write-history-regressed": func(t *testing.T) {
		s := newScenario(t)
		s.activeBuilder("job-f7", "b2400-f", 2400, "T1")
		s.server.Wrk.Recovery = true
		s.handler = s.server.Handler()
		s.refused(s.post("desktop", "job-f7", "round_open", openPayload(head('1'), nil, nil, nil)), "wrk_recovery_required", "write during recovery")
		s.refused(s.do("desktop", "GET", "/v1/wrk/jobs/job-f7", nil, nil), "wrk_recovery_required", "host read during recovery")
		if r := s.do("director-1", "GET", "/v1/wrk/jobs/job-f7/records", nil, nil); r.status != 200 {
			t.Fatalf("central read during recovery: %d %s", r.status, r.raw)
		}
		if r := s.do("director-1", "POST", "/v1/wrk/jobs/job-f7/records", map[string]string{"Idempotency-Key": s.key()}, []byte(`{}`)); r.status != 503 {
			t.Fatalf("central write during recovery: %d", r.status)
		}
	},
	"clock-skew-server-seq-orders": func(t *testing.T) {
		s := newScenario(t)
		s.activeBuilder("job-f8", "b2400-f", 2400, "T1")
		for _, h := range []byte{'1', '2', '3'} {
			s.ok(s.post("desktop", "job-f8", "round_open", openPayload(head(h), nil, nil, nil)), "open")
		}
		g := grantPayload("extension", 4, head('4'), 1, nil, nil)
		g["expires_at"] = wrk.FormatTime(packNow.Add(time.Hour))
		gr := s.ok(s.post("director-1", "job-f8", "grant", g), "grant")
		if gr["record"].(map[string]any)["received_at"] != wrk.FormatTime(packNow) {
			t.Fatalf("received_at is not server time: %v", gr["record"])
		}
		// Server UTC at consumption decides expiry; no request carries time.
		s.server.Wrk.Engine.Now = func() time.Time { return packNow.Add(2 * time.Hour) }
		s.refused(s.post("desktop", "job-f8", "round_open", openPayload(head('4'), nil, num(gr["record"].(map[string]any)["seq"]), nil)), "grant_expired", "expired by server time")
	},
}

func TestO3PackMetaVectors(t *testing.T) {
	for _, c := range loadPackCases(t) {
		if c.reqType() != "meta" {
			continue
		}
		c := c
		t.Run(c.id(), func(t *testing.T) {
			switch {
			case strings.HasPrefix(c.Name, "errmap-"):
				code, _ := c.Expect["error"].(string)
				want, _ := c.Expect["http_status"].(json.Number).Int64()
				if st, ok := wrk.Status(wrk.Code(code)); !ok || int64(st) != want {
					t.Fatalf("Status(%s) = %d, vector says %d", code, st, want)
				}
				if _, ok := wrk.Exit(wrk.Code(code)); !ok {
					t.Fatalf("%s has no CLI exit", code)
				}
			case metaRules[c.Name] != nil:
				metaRules[c.Name](t)
			case metaStoreRules[c.Name]:
				// Executed by TestO3PackStore (store-backed); counted there.
			case metaFailure[c.Name] != nil:
				metaFailure[c.Name](t)
			default:
				t.Fatalf("no executable check for meta vector %s", c.Name)
			}
		})
	}
	// Every §4 server code is a member of the vector error map, and back.
	seen := map[wrk.Code]bool{}
	for _, c := range loadPackCases(t) {
		if strings.HasPrefix(c.Name, "errmap-") {
			code, _ := c.Expect["error"].(string)
			seen[wrk.Code(code)] = true
		}
	}
	for _, code := range wrk.Codes() {
		if !seen[code] {
			t.Errorf("server code %s has no errmap vector", code)
		}
	}
	if len(seen) != len(wrk.Codes()) {
		t.Errorf("errmap vectors %d, server codes %d", len(seen), len(wrk.Codes()))
	}
}
