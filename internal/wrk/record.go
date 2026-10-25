package wrk

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ZeroHash is previous_hash for seq 1.
var ZeroHash = strings.Repeat("0", 64)

// Kinds is the closed record kind set of design §2.
var Kinds = []string{"claim", "activate", "round_open", "verdict", "void", "joined", "completed", "grant", "grant_cancel", "end", "import", "import_begin", "import_seal"}

// Source is import provenance; every other kind has a null source.
type Source struct {
	SourceLocator string `json:"source_locator"`
	SourceSHA256  string `json:"source_sha256"`
}

// Record is one append-only journal row. Every field except Payload's
// request-derived members is server-owned.
type Record struct {
	JobID           string         `json:"job_id"`
	Seq             int64          `json:"seq"`
	RecordID        string         `json:"record_id"`
	StoreGeneration string         `json:"store_generation"`
	ClaimSeq        int64          `json:"claim_seq"`
	ActorClientID   string         `json:"actor_client_id"`
	IdempotencyKey  string         `json:"idempotency_key"`
	RequestSHA256   string         `json:"request_sha256"`
	Imported        bool           `json:"imported"`
	Source          *Source        `json:"source"`
	ReceivedAt      string         `json:"received_at"`
	PreviousHash    string         `json:"previous_hash"`
	RecordHash      string         `json:"record_hash"`
	Kind            string         `json:"kind"`
	Payload         map[string]any `json:"payload"`
}

// hashInput is the record as a generic object without record_hash.
func (r Record) hashInput() map[string]any {
	var source any
	if r.Source != nil {
		source = map[string]any{"source_locator": r.Source.SourceLocator, "source_sha256": r.Source.SourceSHA256}
	}
	return map[string]any{
		"job_id": r.JobID, "seq": r.Seq, "record_id": r.RecordID, "store_generation": r.StoreGeneration,
		"claim_seq": r.ClaimSeq, "actor_client_id": r.ActorClientID, "idempotency_key": r.IdempotencyKey,
		"request_sha256": r.RequestSHA256, "imported": r.Imported, "source": source,
		"received_at": r.ReceivedAt, "previous_hash": r.PreviousHash, "kind": r.Kind, "payload": r.Payload,
	}
}

// ComputeHash returns SHA256 over the RFC 8785 form of every record field
// except record_hash (design §2).
func (r Record) ComputeHash() (string, error) {
	canon, err := Canonical(r.hashInput())
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sha256Sum(canon)), nil
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// RequestDigest is the design §5 request_sha256: SHA256 over RFC 8785 of
// {method:"POST", path:"/v1/wrk/jobs/"+job_id+"/records", body}. The
// Idempotency-Key and bearer headers are not part of it.
func RequestDigest(jobID string, body any) (string, error) {
	canon, err := Canonical(map[string]any{"method": "POST", "path": "/v1/wrk/jobs/" + jobID + "/records", "body": body})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sha256Sum(canon)), nil
}

// Payload accessors over the DecodeStrict shape.

func pStr(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return s
}

func pStrPtr(p map[string]any, k string) *string {
	s, ok := p[k].(string)
	if !ok {
		return nil
	}
	return &s
}

func pInt(p map[string]any, k string) (int64, bool) {
	switch v := p[k].(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		if v == float64(int64(v)) {
			return int64(v), true
		}
	}
	return 0, false
}

func pIntPtr(p map[string]any, k string) *int64 {
	n, ok := pInt(p, k)
	if !ok {
		return nil
	}
	return &n
}

func intOrNil(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
