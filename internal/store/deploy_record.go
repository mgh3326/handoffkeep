package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeployRecordSchema is the schema value of installer R12's deploy record.
// The console's deploy view and the hk.deploy event read records through the
// one parser in this file.
const DeployRecordSchema = "deploy-record/v0"

// deployRecordKeyRE is the record key contract:
// deploy/<service>/<YYYYMMDDTHHMMSSZ>.
var deployRecordKeyRE = regexp.MustCompile(`^deploy/([A-Za-z0-9._-]{1,128})/([0-9]{8}T[0-9]{6}Z)$`)

// DeployRecord mirrors the installer R12 body. Unknown fields are ignored;
// fields the writer did not know arrive as JSON null. IncludedPRs is nil when
// the record carries null, which moves nothing.
type DeployRecord struct {
	Schema      string   `json:"schema"`
	Service     string   `json:"service"`
	Target      string   `json:"target"`
	HeadSHA     *string  `json:"head_sha"`
	DeployedRef *string  `json:"deployed_ref"`
	PreviousRef *string  `json:"previous_ref"`
	DeployedAt  *string  `json:"deployed_at"`
	Result      string   `json:"result"`
	FailedStep  *int     `json:"failed_step"`
	JobID       *string  `json:"job_id"`
	ApprovalRef *string  `json:"approval_ref"`
	IncludedPRs []string `json:"included_prs"`
	Source      string   `json:"source"`
}

// DeployRecordKeyTime extracts the UTC timestamp embedded in a
// deploy/<service>/<YYYYMMDDTHHMMSSZ> record key. It orders records when a
// record's own deployed_at is absent.
func DeployRecordKeyTime(key string) (time.Time, bool) {
	stamp := key[strings.LastIndex(key, "/")+1:]
	t, err := time.Parse("20060102T150405Z", stamp)
	return t, err == nil
}

// ClassifyDeployRecord validates one document as a deploy record for the
// expected service and reports every contract violation it can determine:
// "key" (the key's timestamp is not the contract YYYYMMDDTHHMMSSZ), "schema"
// (unparseable JSON, wrong schema/service, or result outside the enum) and
// "deployed_at" (present but not RFC3339). A document carrying any reason is
// not a deploy record.
func ClassifyDeployRecord(key, service, body string) (*DeployRecord, *time.Time, []string) {
	var rec DeployRecord
	err := json.Unmarshal([]byte(body), &rec)
	return classifyParsedDeployRecord(key, service, &rec, err == nil)
}

func classifyParsedDeployRecord(key, service string, rec *DeployRecord, decoded bool) (*DeployRecord, *time.Time, []string) {
	var reasons []string
	if _, ok := DeployRecordKeyTime(key); !ok {
		reasons = append(reasons, "key")
	}
	if !decoded {
		return nil, nil, append(reasons, "schema")
	}
	if rec.Schema != DeployRecordSchema || rec.Service != service {
		return nil, nil, append(reasons, "schema")
	}
	switch rec.Result {
	case "success", "failed", "rolled_back":
	default:
		return nil, nil, append(reasons, "schema")
	}
	var deployedAt *time.Time
	if rec.DeployedAt != nil {
		parsed, err := time.Parse(time.RFC3339, *rec.DeployedAt)
		if err != nil {
			reasons = append(reasons, "deployed_at")
		} else {
			deployedAt = &parsed
		}
	}
	if len(reasons) > 0 {
		return nil, nil, reasons
	}
	return rec, deployedAt, nil
}

// IsDeployRecordKey reports whether key has the deploy record shape.
func IsDeployRecordKey(key string) bool { return deployRecordKeyRE.MatchString(key) }

// deployedPRs returns the PRs a document deploys: the key has the record
// shape, the body is a valid record for the key's service, the result is
// success and included_prs is not null. ok is false for anything else, which
// moves nothing.
func deployedPRs(x StoredDeployRecord) ([]string, bool) {
	m := deployRecordKeyRE.FindStringSubmatch(x.Key)
	if m == nil {
		return nil, false
	}
	rec, _, reasons := x.Classify(m[1])
	if len(reasons) > 0 || rec.Result != "success" || rec.IncludedPRs == nil {
		return nil, false
	}
	seen := map[string]bool{}
	out := []string{}
	for _, pr := range rec.IncludedPRs {
		if !taskEventPRRE.MatchString(pr) || seen[pr] {
			continue
		}
		seen[pr] = true
		out = append(out, pr)
	}
	return out, true
}

// StoredDeployRecord contains only the fields used by deploy-state readers.
// A missing or stale projection is parsed from the document for compatibility
// with the previous release, which can still write during blue-green rollout.
type StoredDeployRecord struct {
	Key       string
	SHA256    string
	CreatedAt time.Time
	Record    DeployRecord
	Decoded   bool
}

func parseStoredDeployRecord(key, sha, body string, created time.Time) StoredDeployRecord {
	x := StoredDeployRecord{Key: key, SHA256: sha, CreatedAt: created}
	x.Decoded = json.Unmarshal([]byte(body), &x.Record) == nil
	return x
}

func (x StoredDeployRecord) Classify(service string) (*DeployRecord, *time.Time, []string) {
	return classifyParsedDeployRecord(x.Key, service, &x.Record, x.Decoded)
}

func writeDeployProjection(ctx context.Context, tx pgx.Tx, x StoredDeployRecord) error {
	data, err := json.Marshal(x.Record)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO deploy_record_projection (key, sha256, record, decoded) VALUES ($1,$2,$3,$4) ON CONFLICT (key) DO UPDATE SET sha256=EXCLUDED.sha256, record=EXCLUDED.record, decoded=EXCLUDED.decoded`, x.Key, x.SHA256, data, x.Decoded)
	return err
}

// ListDeployRecords pages projected deploy state. The fallback is deliberately
// limited to missing or stale projections from previous-release writers.
func (s *Store) ListDeployRecords(ctx context.Context, prefix, before string, limit int) ([]StoredDeployRecord, error) {
	if prefix == "" || !validText(prefix, maxDocumentPrefixBytes) || !validText(before, 512) {
		return nil, errors.New("invalid deploy record prefix or cursor")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT d.key,d.sha256,d.created_at,p.record,p.decoded,CASE WHEN p.key IS NULL THEN d.body ELSE NULL END FROM documents d LEFT JOIN deploy_record_projection p ON p.key=d.key AND p.sha256=d.sha256 WHERE strpos(d.key,$1)=1 AND ($2='' OR d.key<$2) ORDER BY d.key DESC LIMIT $3`, prefix, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StoredDeployRecord{}
	for rows.Next() {
		var x StoredDeployRecord
		var raw []byte
		var decoded *bool
		var body *string
		if err = rows.Scan(&x.Key, &x.SHA256, &x.CreatedAt, &raw, &decoded, &body); err != nil {
			return nil, err
		}
		if body != nil {
			x = parseStoredDeployRecord(x.Key, x.SHA256, *body, x.CreatedAt)
		} else {
			if err = json.Unmarshal(raw, &x.Record); err != nil {
				return nil, err
			}
			x.Decoded = *decoded
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
