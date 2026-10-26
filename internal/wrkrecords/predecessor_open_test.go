package wrkrecords_test

import (
	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
	"testing"
	"time"
)

// b953: a successor never inherits a predecessor's preactivation open as a
// head reservation. Design r4 expires every preactivation open reservation at
// activation and keeps negative head identity only on verdicts, so a
// predecessor's round_open reaches the successor's duplicate_head check only
// as a live post-activate pending open.

func sealAndActivate(c *scenario, job string) {
	c.t.Helper()
	begin := c.must("director-1", job, "import_begin", Object{"reason_code": "archive"})
	manifest, _ := Digest([]Object{})
	seal := c.must("director-1", job, "import_seal", Object{"begin_seq": begin.Seq, "expected_import_count": 0, "manifest_sha256": manifest, "reason_code": "seal"})
	c.must("director-1", job, "activate", Object{"seal_seq": seal.Seq, "reason_code": "enforce"})
}

func extensionFor(c *scenario, job, head string) Record {
	c.t.Helper()
	sum, err := c.s.Summary(job, c.e.Time())
	if err != nil {
		c.t.Fatal(err)
	}
	g := c.must("director-1", job, "grant", Object{"grant_type": "extension", "round_no": sum.Next, "head": head, "extra": 1, "earlier_round_no": nil, "tester_job_id": nil, "expires_at": c.e.Now.Add(time.Hour).Format(time.RFC3339Nano), "reason_code": "fresh_review"})
	return g
}

func TestDesignSuccessorShadowOpenReservation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verdict  bool
		void     bool
		activate bool
		code     string // "" means the successor open must land
	}{
		{"open_no_verdict_ended_without_activating", false, false, false, ""},
		{"open_no_verdict_sealed_and_activated", false, false, true, ""},
		{"open_then_voided_ended_without_activating", false, true, false, ""},
		{"open_plus_verdict_ended_without_activating", true, false, false, "duplicate_head"},
		{"open_plus_verdict_sealed_and_activated", true, false, true, "duplicate_head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fresh(t)
			c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
			c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
			if tc.verdict {
				c.must("desktop", "subject", "verdict", evidence(1, h1, "/review/"+tc.name, "shadow verdict", nil))
			}
			if tc.void {
				g := c.grant("void", int64(1), nil, nil, nil)
				c.must("desktop", "subject", "void", Object{"round_no": 1, "grant_seq": g.Seq, "reason_code": "aborted"})
			}
			if tc.activate {
				sealAndActivate(c, "subject")
			}
			c.must("director-1", "subject", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
			claimSuccessor(c, "successor", "subject")
			c.must("director-1", "successor", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
			sum, err := c.s.Summary("successor", c.e.Time())
			if err != nil || sum.Used < 3 {
				t.Fatalf("cap must not mask headReserved: %+v %v", sum, err)
			}
			g := extensionFor(c, "successor", h1)
			open := opening(h1, "", g.Seq, nil)
			if tc.code != "" {
				c.want("desktop", "successor", "round_open", tc.code, open)
				return
			}
			r := c.must("desktop", "successor", "round_open", open)
			if Num(r.Payload, "round_no") != sum.Next {
				t.Fatalf("successor open landed on %v, want %d", r.Payload["round_no"], sum.Next)
			}
		})
	}
}

func TestDesignClaimOnlyBridgeKeepsLiveOpenReserved(t *testing.T) {
	c := fresh(t)
	c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	c.must("director-1", "subject", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
	claimSuccessor(c, "bridge", "subject")
	c.must("director-1", "bridge", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
	claimSuccessor(c, "successor", "bridge")
	c.must("director-1", "successor", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	c.want("desktop", "successor", "round_open", "duplicate_head", opening(h1, "", nil, nil))
}

func TestDesignClaimOnlyBridgeVoidedOpenReleases(t *testing.T) {
	c := fresh(t)
	c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	g := c.grant("void", int64(1), nil, nil, nil)
	c.must("desktop", "subject", "void", Object{"round_no": 1, "grant_seq": g.Seq, "reason_code": "aborted"})
	c.must("director-1", "subject", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
	claimSuccessor(c, "bridge", "subject")
	c.must("director-1", "bridge", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
	claimSuccessor(c, "successor", "bridge")
	c.must("director-1", "successor", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	r := c.must("desktop", "successor", "round_open", opening(h1, "", nil, nil))
	if Num(r.Payload, "round_no") != 2 {
		t.Fatalf("successor recycled or skipped an ordinal: %+v", r.Payload)
	}
}

func TestDesignOwnShadowOpenStillReservesHead(t *testing.T) {
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	c.want("desktop", "subject", "round_open", "duplicate_head", opening(h1, "", nil, nil))
}
