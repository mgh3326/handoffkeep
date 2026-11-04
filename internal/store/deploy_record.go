package store

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
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
	var reasons []string
	if _, ok := DeployRecordKeyTime(key); !ok {
		reasons = append(reasons, "key")
	}
	var rec DeployRecord
	if err := json.Unmarshal([]byte(body), &rec); err != nil {
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
	return &rec, deployedAt, nil
}

// IsDeployRecordKey reports whether key has the deploy record shape.
func IsDeployRecordKey(key string) bool { return deployRecordKeyRE.MatchString(key) }

// deployedPRs returns the PRs a document deploys: the key has the record
// shape, the body is a valid record for the key's service, the result is
// success and included_prs is not null. ok is false for anything else, which
// moves nothing.
func deployedPRs(key, body string) ([]string, bool) {
	m := deployRecordKeyRE.FindStringSubmatch(key)
	if m == nil {
		return nil, false
	}
	rec, _, reasons := ClassifyDeployRecord(key, m[1], body)
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
