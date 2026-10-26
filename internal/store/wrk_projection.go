package store

import (
	"context"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

// Legacy relay columns contain no authority or report digest. Hydrate only an
// exact projection of a receipt-eligible immutable record and its stored bytes.
// Missing/mismatching records leave the notification visible without a receipt.
func hydrateWrkRelay(ctx context.Context, q wrkQuerier, x *RelayEvent) {
	digest := x.ReportSHA
	x.WrkReceipt, x.ReportSHA = nil, ""
	if x.Kind != "job.completed" && x.Kind != "job.joined" {
		return
	}
	prefix := "wrk:" + x.JobID + ":"
	if !strings.HasPrefix(x.ReportPath, prefix) || x.Epoch != 0 || x.Reason != "" {
		return
	}
	seq, err := strconv.ParseInt(strings.TrimPrefix(x.ReportPath, prefix), 10, 64)
	if err != nil || seq < 1 || x.ReportPath != prefix+intString(seq) || !wrkTablesExist(ctx, q) {
		return
	}
	var kind, owner, head, pr, reportSHA, contentSHA, role, generation, hash string
	var review bool
	var activation int64
	var content []byte
	err = q.QueryRow(ctx, `SELECT r.kind,c.payload->>'owner_lane',COALESCE(r.payload->>'head',''),COALESCE(r.payload->>'pr',''),r.payload->>'report_sha256',b.content_sha256,b.content,c.payload->>'role',c.payload->'review_of' IS NOT NULL AND c.payload->'review_of'<>'null'::jsonb,r.store_generation::text,r.record_hash,COALESCE((SELECT min(a.seq) FROM wrk_records a WHERE a.job_id=r.job_id AND a.kind='activate'),0)
		FROM wrk_records r JOIN wrk_records c ON c.job_id=r.job_id AND c.seq=r.claim_seq AND c.kind='claim'
		JOIN wrk_contents b ON b.job_id=r.job_id AND b.seq=r.seq
		WHERE r.job_id=$1 AND r.seq=$2 AND NOT r.imported AND r.kind IN ('completed','joined')
		AND NOT EXISTS(SELECT 1 FROM wrk_records newer WHERE newer.job_id=r.job_id AND newer.kind='claim' AND newer.seq>c.seq)`, x.JobID, seq).Scan(&kind, &owner, &head, &pr, &reportSHA, &contentSHA, &content, &role, &review, &generation, &hash, &activation)
	if err != nil {
		return
	}
	reviewer := review && (role == "tester" || role == "reviewer" || role == "counsel")
	if (!reviewer && (activation == 0 || seq <= activation)) || x.Kind != "job."+kind || x.OwnerLane != owner || x.Head != head || x.PR != pr || reportSHA != contentSHA || reportSHA != wrkrecords.SHA(content) || (digest != "" && digest != reportSHA) {
		return
	}
	x.ReportSHA = reportSHA
	x.WrkReceipt = &wrkrecords.Receipt{JobID: x.JobID, Seq: seq, Hash: hash, Generation: generation}
}

func (s *Store) hydrateWrkRelays(ctx context.Context, rows []RelayEvent) {
	for i := range rows {
		hydrateWrkRelay(ctx, s.pool, &rows[i])
	}
}
