package wrkrecords_test

import (
	"encoding/base64"
	"encoding/json"
	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
	"testing"
)

// Build the same preactivation history for sealed and abandoned migrations.
func shadowPredecessor(t *testing.T, open bool, takeover int64) (*scenario, Object) {
	t.Helper()
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	if open {
		c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	}
	begin := c.must("director-1", "subject", "import_begin", Object{"reason_code": "archive"})
	rows := []Object{}
	if takeover >= 0 {
		b, _ := json.Marshal(Object{"kind": "job.takeover", "rounds_used": takeover})
		r := c.must("director-1", "subject", "import", Object{"source_locator": "design/inherited-takeover", "source_sha256": SHA(b), "original_actor_client_id": nil, "original_at": nil, "legacy_kind": "job.takeover", "content_b64": base64.StdEncoding.EncodeToString(b)})
		rows = append(rows, Object{"seq": r.Seq, "source_locator": r.Payload["source_locator"], "source_sha256": r.Payload["source_sha256"]})
	}
	manifest, _ := Digest(rows)
	return c, Object{"begin_seq": begin.Seq, "expected_import_count": len(rows), "manifest_sha256": manifest, "reason_code": "seal"}
}

func claimSuccessor(c *scenario, job, predecessor string) {
	c.t.Helper()
	c.must("director-1", job, "claim", Object{"job_id": job, "owner_lane": "builder-a", "parent_lane": "director-1", "task_id": 9001, "host_client_id": "desktop", "role": "builder", "tier": "T3", "review_of": nil, "predecessor_job_id": predecessor, "supersedes": nil})
}

func TestDesignUnsealedPredecessorInheritsWholeBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		open     bool
		takeover int64
		used     int64
	}{
		{"shadow_open_and_takeover_five", true, 5, 6},
		{"takeover_five_only", false, 5, 5},
		{"shadow_open_only_minimum_three", true, -1, 3},
		{"claim_only_zero_work", false, -1, 0},
		{"last_representable_floor", false, MaxInt - 1, MaxInt - 1},
	} {
		for _, phase := range []string{"unsealed", "sealed", "active", "unsealed_bridge"} {
			t.Run(tc.name+"/"+phase, func(t *testing.T) {
				c, sealRequest := shadowPredecessor(t, tc.open, tc.takeover)
				if phase == "sealed" || phase == "active" {
					seal := c.must("director-1", "subject", "import_seal", sealRequest)
					if Num(seal.Payload, "baseline_used") != tc.used {
						t.Fatal("sealed comparison lost conservative budget")
					}
					if phase == "active" {
						c.must("director-1", "subject", "activate", Object{"seal_seq": seal.Seq, "reason_code": "enforce"})
					}
				}
				c.must("director-1", "subject", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
				pred := "subject"
				if phase == "unsealed_bridge" {
					claimSuccessor(c, "bridge", pred)
					c.must("director-1", "bridge", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
					pred = "bridge"
				}
				claimSuccessor(c, "successor", pred)
				r, err := c.send("director-1", "successor", NewUUID(), c.request("successor", "activate", Object{"seal_seq": nil, "reason_code": "enforce"}))
				if err != nil || !r.Created || Num(r.Record.Payload, "baseline_used") != tc.used || r.Summary.Baseline != tc.used || r.Summary.Used != tc.used || r.Summary.Next != tc.used+1 {
					t.Fatalf("successor lost effective preactivation budget: want used=%d next=%d, got %+v payload=%+v err=%v", tc.used, tc.used+1, r.Summary, r.Record.Payload, err)
				}
				if tc.used >= 3 {
					c.want("desktop", "successor", "round_open", "round_cap", opening(h4, "", nil, nil))
				}
			})
		}
	}
}

func TestDesignUnsealedPredecessorOverflowBlocksSuccessorActivation(t *testing.T) {
	for _, tc := range []struct {
		open bool
		used int64
	}{{false, MaxInt}, {true, MaxInt}, {true, MaxInt - 1}} {
		c, sealRequest := shadowPredecessor(t, tc.open, tc.used)
		c.want("director-1", "subject", "import_seal", "invalid_request", sealRequest)
		c.must("director-1", "subject", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
		claimSuccessor(c, "successor", "subject")
		before := len(c.s.Jobs["successor"])
		r, err := c.send("director-1", "successor", NewUUID(), c.request("successor", "activate", Object{"seal_seq": nil, "reason_code": "enforce"}))
		if err == nil || Code(err) != "invalid_request" || r.Created || len(c.s.Jobs["successor"]) != before || c.s.Activation("successor") != 0 {
			t.Fatalf("overflow escaped through unsealed predecessor: open=%v takeover=%d response=%+v err=%v", tc.open, tc.used, r, err)
		}
		begin := c.must("director-1", "successor", "import_begin", Object{"reason_code": "archive"})
		manifest, _ := Digest([]Object{})
		c.want("director-1", "successor", "import_seal", "invalid_request", Object{"begin_seq": begin.Seq, "expected_import_count": 0, "manifest_sha256": manifest, "reason_code": "seal"})
	}
}

func TestDesignSuccessorSealIncludesUnsealedPredecessor(t *testing.T) {
	c, _ := shadowPredecessor(t, true, 5)
	c.must("director-1", "subject", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
	claimSuccessor(c, "successor", "subject")
	c.must("desktop", "successor", "round_open", opening(h2, "", nil, nil))
	begin := c.must("director-1", "successor", "import_begin", Object{"reason_code": "archive"})
	manifest, _ := Digest([]Object{})
	seal := c.must("director-1", "successor", "import_seal", Object{"begin_seq": begin.Seq, "expected_import_count": 0, "manifest_sha256": manifest, "reason_code": "seal"})
	if Num(seal.Payload, "baseline_used") != 7 || Num(seal.Payload, "numbering_floor") != 7 {
		t.Fatalf("successor seal dropped predecessor L+I: %+v", seal.Payload)
	}
	c.must("director-1", "successor", "activate", Object{"seal_seq": seal.Seq, "reason_code": "enforce"})
}

func TestDesignUnsealedPredecessorPreservesImportedNumbering(t *testing.T) {
	c, _ := shadowPredecessor(t, true, 5)
	for _, event := range []Object{{"kind": "job.round", "round": 100, "head": h2}, {"kind": "job.round_void", "round": 123}} {
		b, _ := json.Marshal(event)
		c.must("director-1", "subject", "import", Object{"source_locator": "design/" + Text(event, "kind"), "source_sha256": SHA(b), "original_actor_client_id": nil, "original_at": nil, "legacy_kind": Text(event, "kind"), "content_b64": base64.StdEncoding.EncodeToString(b)})
	}
	c.must("director-1", "subject", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
	claimSuccessor(c, "successor", "subject")
	c.must("director-1", "successor", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	sum, err := c.s.Summary("successor", c.e.Time())
	if err != nil || sum.Used != 7 || sum.Next != 124 || len(sum.Open) != 0 {
		t.Fatalf("unsealed imported numbers or void reduced budget: %+v %v", sum, err)
	}
	c.want("desktop", "successor", "joined", "zero_rounds", report(h4))
	g := c.must("director-1", "successor", "grant", Object{"grant_type": "extension", "round_no": 124, "head": h4, "extra": 1, "earlier_round_no": nil, "tester_job_id": nil, "expires_at": c.e.Now.Add(3600e9).Format("2006-01-02T15:04:05Z"), "reason_code": "fresh_review"})
	r := c.must("desktop", "successor", "round_open", opening(h4, "", g.Seq, nil))
	if Num(r.Payload, "round_no") != 124 {
		t.Fatal("successor recycled an imported ordinal")
	}
}
