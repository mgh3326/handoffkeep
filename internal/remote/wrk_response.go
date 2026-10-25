package remote

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

// wrkResponseConsistent rejects structurally valid but contradictory responses
// before any JSON reaches stdout. Retained history across calls belongs to C1.
func wrkResponseConsistent(method, path, key, def string, body []byte, v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	u, e := url.Parse(path)
	if e != nil {
		return false
	}
	parts := strings.Split(u.Path, "/")
	if def == "Capabilities" {
		return true
	}
	if len(parts) < 5 {
		return false
	}
	job := parts[4]
	recordOK := func(r wrkrecords.Object) bool {
		if r == nil || wrkrecords.Text(r, "job_id") != job {
			return false
		}
		hash := wrkrecords.Text(r, "record_hash")
		copy := wrkrecords.Clone(r)
		delete(copy, "record_hash")
		got, e := wrkrecords.Digest(copy)
		return e == nil && got == hash
	}
	summaryOK := func(s wrkrecords.Object) bool { return s != nil && wrkrecords.Text(s, "job_id") == job }
	switch def {
	case "WriteResponse":
		r, s := wrkrecords.Obj(m, "record"), wrkrecords.Obj(m, "summary")
		request, e := wrkrecords.ParseRequest(body)
		if e != nil || !recordOK(r) || !summaryOK(s) {
			return false
		}
		digest, e := wrkrecords.Digest(wrkrecords.Object{"method": method, "path": u.Path, "body": request})
		if e != nil {
			return false
		}
		claim := request.ClaimSeq
		if request.Kind == "claim" {
			claim = wrkrecords.Num(r, "seq")
		}
		return wrkrecords.Text(r, "kind") == request.Kind && wrkrecords.Text(r, "idempotency_key") == key && wrkrecords.Text(r, "request_sha256") == digest && wrkrecords.Num(r, "seq") == request.ExpectedSeq+1 && wrkrecords.Num(r, "claim_seq") == claim && wrkrecords.Num(s, "claim_seq") == claim && wrkrecords.Num(s, "snapshot_seq") == wrkrecords.Num(r, "seq") && wrkrecords.Text(s, "tail_hash") == wrkrecords.Text(r, "record_hash") && wrkrecords.Text(s, "store_generation") == wrkrecords.Text(r, "store_generation")
	case "JobResponse":
		c, s := wrkrecords.Obj(m, "claim"), wrkrecords.Obj(m, "summary")
		return recordOK(c) && summaryOK(s) && wrkrecords.Text(c, "kind") == "claim" && wrkrecords.Num(c, "seq") == wrkrecords.Num(s, "claim_seq") && wrkrecords.Num(c, "seq") <= wrkrecords.Num(s, "snapshot_seq")
	case "RecordsResponse":
		s := wrkrecords.Obj(m, "summary")
		if !summaryOK(s) {
			return false
		}
		after, limit := int64(0), int64(100)
		if q := u.Query().Get("after_seq"); q != "" {
			after, _ = strconv.ParseInt(q, 10, 64)
		}
		if q := u.Query().Get("limit"); q != "" {
			limit, _ = strconv.ParseInt(q, 10, 64)
		}
		snapshot := wrkrecords.Num(m, "snapshot_seq")
		if q := u.Query().Get("snapshot_seq"); q != "" {
			n, _ := strconv.ParseInt(q, 10, 64)
			if n != snapshot {
				return false
			}
		}
		if wrkrecords.Num(s, "snapshot_seq") != snapshot || after > snapshot {
			return false
		}
		rs, ok := m["records"].([]any)
		if !ok {
			return false
		}
		want := snapshot - after
		if want > limit {
			want = limit
		}
		if int64(len(rs)) != want {
			return false
		}
		seq, previous := after, ""
		for _, raw := range rs {
			r, ok := raw.(map[string]any)
			if !ok || !recordOK(r) || wrkrecords.Num(r, "seq") != seq+1 {
				return false
			}
			if previous != "" && wrkrecords.Text(r, "previous_hash") != previous {
				return false
			}
			previous = wrkrecords.Text(r, "record_hash")
			seq++
		}
		more, ok := m["has_more"].(bool)
		if !ok || more != (seq < snapshot) || wrkrecords.Num(m, "next_after_seq") != seq {
			return false
		}
		return seq < snapshot || previous == "" || previous == wrkrecords.Text(s, "tail_hash")
	case "ContentResponse":
		seq, e := strconv.ParseInt(parts[6], 10, 64)
		return e == nil && wrkrecords.Text(m, "job_id") == job && wrkrecords.Num(m, "seq") == seq
	}
	return false
}
