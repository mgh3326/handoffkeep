package wrkrecords_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

func TestPackExecutionInventory(t *testing.T) {
	// Same per-source inventory as M1's internal/o3contract harness. Every type
	// below has an executing runner; there is no fixture-invalid exception.
	want := map[string]int{"section4-rules.json": 46, "transition.json": 27, "migration.json": 10, "legacy-route-fence.json": 12, "r3-regression.json": 13, "r4-scoped-claim.json": 39, "r4-review-lifecycle.json": 10, "head-repeat-binding.json": 7, "transport-intents.json": 14, "failure-abuse.json": 16}
	got := map[string]int{}
	for _, v := range loadVectors(t) {
		got[v.file]++
		switch Text(v.Request, "type") {
		case "http", "http_raw", "legacy": // TestStoreHTTPVectors (CI), append engine locally too
		case "cli": // TestWrkCLIVectors (built executable)
		case "meta", "policy", "none": // TestPackNonHTTPVectors below
		default:
			t.Fatalf("unhandled vector type in %s/%s", v.file, v.Name)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source count mismatch: %v", got)
	}
	for file, count := range got {
		t.Logf("%s: %d cases", file, count)
	}
}

func TestPackNonHTTPVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		kind := Text(v.Request, "type")
		if kind != "meta" && kind != "policy" && kind != "none" {
			continue
		}
		t.Run(strings.TrimSuffix(v.file, ".json")+"/"+v.Name, func(t *testing.T) {
			if kind == "policy" {
				raw, _ := json.Marshal(v.Request["document"])
				_, e := ParsePolicy(raw)
				if (e == nil) != v.Expect["policy_valid"] {
					t.Fatalf("policy acceptance: %v", e)
				}
				return
			}
			if v.Source.Table == "errmap" {
				code := Text(v.Expect, "error")
				if Status(Error(code)) != int(Num(v.Expect, "http_status")) {
					t.Fatal("runtime HTTP map")
				}
				if Validate("Error", Object{"error": code}) != nil || Validate("Error", Object{"error": code, "reason": "private"}) == nil {
					t.Fatal("error vocabulary/hygiene")
				}
				return
			}
			if v.Source.Table == "rules" {
				switch v.Source.Row {
				case 1:
					raw, _ := json.Marshal(Request{Kind: "round_open", ClaimSeq: 1, ExpectedSeq: 2, Payload: opening(h1, "", nil, nil)})
					if _, e := ParseRequest(raw); e != nil {
						t.Fatal(e)
					}
					if _, e := ParseRequest([]byte(`{"head":"abc"}`)); e == nil {
						t.Fatal("payload accepted as Request")
					}
				case 2:
					if _, e := ParseRequest([]byte(`{"kind":"unknown","claim_seq":0,"expected_seq":0,"payload":{}}`)); e == nil {
						t.Fatal("open request union")
					}
				case 3:
					TestDesignReplayBeforeCASAndTerminal(t)
				case 4:
					a, e := Digest(Object{"method": "POST", "path": "/v1/wrk/jobs/j/records", "body": Object{"z": 1, "a": 2}})
					if e != nil {
						t.Fatal(e)
					}
					if a != SHA([]byte(`{"body":{"a":2,"z":1},"method":"POST","path":"/v1/wrk/jobs/j/records"}`)) {
						t.Fatal("canonical digest input")
					}
				case 5:
					for _, raw := range []string{"VERDICT: PASS @" + h1, "VERDICT: BLOCKER @" + strings.ToUpper(h1) + "\t\r\nbody\rtext"} {
						if _, _, e := ParseVerdict([]byte(raw)); e != nil {
							t.Fatal(e)
						}
					}
					for _, raw := range []string{"preamble\nVERDICT: PASS @" + h1, "VERDICT: PASS @" + h1 + "\nVERDICT: broken", "VERDICT: PASS @" + h1 + "\r"} {
						if _, _, e := ParseVerdict([]byte(raw)); Code(e) != "invalid_verdict" {
							t.Fatal("invalid grammar accepted")
						}
					}
				case 6:
					TestDesignImportFloorAndNoPlantedTrust(t)
				case 7:
					raw, _ := json.Marshal(Object{"features": Object{"head_repeat_enabled": false}})
					if _, e := ParsePolicy(raw); e == nil {
						t.Fatal("disabled repeat policy accepted")
					}
				case 8:
					c := fresh(t)
					c.reviewer("review-role", "desktop", 1)
					c.want("desktop", "review-role", "round_open", "scope_denied", opening(h1, "", nil, nil))
				case 9:
					c := fresh(t)
					if e := c.s.CheckLegacy(c.e.Policy, "desktop", LegacyIntent{TaskID: 9001, CurrentJob: "subject", Binding: true}); Code(e) != "wrk_receipt_required" {
						t.Fatal(e)
					}
				default:
					t.Fatal("unknown normative rule")
				}
				return
			}
			switch v.Name {
			case "server-loses-acknowledged-write-history-regressed":
				// Retained-receipt comparison belongs to C1/C2 (§6). S1 executes the
				// explicit server half: recovery freeze denies writes and host reads.
				c := fresh(t)
				c.e.Recovery = true
				c.want("desktop", "subject", "round_open", "wrk_recovery_required", opening(h1, "", nil, nil))
				if e := c.e.Check("desktop", "job.read"); Code(e) != "wrk_recovery_required" {
					t.Fatal(e)
				}
				if e := c.e.Check("director-1", "job.read"); e != nil {
					t.Fatal(e)
				}
			case "clock-skew-server-seq-orders":
				c := fresh(t)
				a := c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
				c.e.Now = c.e.Now.Add(-1)
				b := c.must("desktop", "subject", "round_open", opening(h2, "", nil, nil))
				if b.Seq != a.Seq+1 {
					t.Fatal("clock determined sequence")
				}
			case "p2-gate-unverified-durability-refused":
				// §9 assigns the restore/deploy gate to D1/D2, outside S1. Prove that
				// S1 cannot activate in shadow while those desk-owned prerequisites wait.
				if kind != "none" || v.Source.Table != "r4claim" || v.Source.Row != 19 {
					t.Fatal("D1-only scope changed")
				}
				c := fresh(t)
				c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
				c.e.Mode = "shadow"
				c.want("director-1", "subject", "activate", "wrk_unavailable", Object{"reason_code": "p1_only", "seal_seq": nil})
			default:
				t.Fatal("unhandled non-HTTP vector")
			}
		})
	}
}
func TestCLIExitMap(t *testing.T) {
	raw, e := os.ReadFile("../../testdata/o3/cli.json")
	if e != nil {
		t.Fatal(e)
	}
	var pack struct {
		Rows []struct {
			Codes []string `json:"codes"`
			Exit  int      `json:"exit"`
		} `json:"exit_map"`
	}
	if json.Unmarshal(raw, &pack) != nil || len(pack.Rows) == 0 {
		t.Fatal("exit map fixture")
	}
	for _, row := range pack.Rows {
		for _, code := range row.Codes {
			if Exit(code) != row.Exit {
				t.Errorf("%s: exit=%d want=%d", code, Exit(code), row.Exit)
			}
		}
	}
}
