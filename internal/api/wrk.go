package api

import (
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// WrkMode is the WRK_RECORDS_API deployment flag (design §7).
type WrkMode string

const (
	WrkOff     WrkMode = "off"
	WrkShadow  WrkMode = "shadow"
	WrkEnforce WrkMode = "enforce"
)

// ParseWrkMode reads WRK_RECORDS_API; empty is off.
func ParseWrkMode(v string) (WrkMode, error) {
	switch WrkMode(v) {
	case "", WrkOff:
		return WrkOff, nil
	case WrkShadow, WrkEnforce:
		return WrkMode(v), nil
	}
	return "", errors.New("WRK_RECORDS_API must be off, shadow or enforce")
}

// Wrk is the /v1/wrk namespace configuration. A nil Wrk, or Mode off,
// leaves every /v1/wrk route answering wrk_unavailable after
// authentication; legacy routes are unaffected.
type Wrk struct {
	Mode     WrkMode
	Recovery bool
	Policy   *wrk.Policy
	Engine   *wrk.Engine
	Backend  wrk.Backend
}

func wrkErr(w http.ResponseWriter, code wrk.Code) {
	status, ok := wrk.Status(code)
	if !ok {
		code, status = wrk.InvalidRequest, 400
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonOut(w, status, map[string]string{"error": string(code)})
}

func wrkOK(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	jsonOut(w, status, v)
}

func codeOf(err error) wrk.Code {
	var we *wrk.Error
	if errors.As(err, &we) {
		return we.Code
	}
	return wrk.Unavailable
}

// wrk serves /v1/wrk. It authenticates first, then applies the flag,
// policy and route checks; no response carries a Location header or any
// request-derived text.
func (s Server) wrk(w http.ResponseWriter, r *http.Request) {
	client, ok := s.Tokens.Client(r)
	if !ok {
		s.wrkAttempt(r, "", wrk.Unauthorized)
		wrkErr(w, wrk.Unauthorized)
		return
	}
	cfg := s.Wrk
	if cfg == nil || cfg.Mode == WrkOff || cfg.Policy == nil || cfg.Engine == nil || cfg.Backend == nil {
		wrkErr(w, wrk.Unavailable)
		return
	}
	if !cfg.Policy.Known(client) {
		s.wrkAttempt(r, client, wrk.ScopeDenied)
		wrkErr(w, wrk.ScopeDenied)
		return
	}
	segs, ok := wrkPath(r)
	if !ok {
		wrkErr(w, wrk.NotFound)
		return
	}
	switch {
	case len(segs) == 1 && segs[0] == "capabilities":
		if r.Method != http.MethodGet {
			wrkErr(w, wrk.MethodNotAllowed)
			return
		}
		s.wrkCapabilities(w, r, client)
		return
	case len(segs) >= 2 && segs[0] == "jobs":
		job := segs[1]
		switch {
		case len(segs) == 2:
			if r.Method != http.MethodGet {
				wrkErr(w, wrk.MethodNotAllowed)
				return
			}
			s.wrkRead(w, r, client, wrk.PermJobRead, func() { s.wrkJob(w, r, job) })
			return
		case len(segs) == 3 && segs[2] == "records":
			switch r.Method {
			case http.MethodGet:
				s.wrkRead(w, r, client, wrk.PermRecordsRead, func() { s.wrkRecords(w, r, job) })
			case http.MethodPost:
				s.wrkAppend(w, r, client, job)
			default:
				wrkErr(w, wrk.MethodNotAllowed)
			}
			return
		case len(segs) == 5 && segs[2] == "records" && segs[4] == "content":
			if r.Method != http.MethodGet {
				wrkErr(w, wrk.MethodNotAllowed)
				return
			}
			s.wrkRead(w, r, client, wrk.PermContentRead, func() { s.wrkContent(w, r, job, segs[3]) })
			return
		}
	}
	wrkErr(w, wrk.NotFound)
}

// wrkPath splits the raw path under /v1/wrk/. Percent-encoded aliases are
// refused so the job id in the digest path is the literal ASCII segment.
func wrkPath(r *http.Request) ([]string, bool) {
	raw := r.URL.EscapedPath()
	if strings.Contains(raw, "%") {
		return nil, false
	}
	rest, ok := strings.CutPrefix(raw, "/v1/wrk/")
	if !ok || rest == "" {
		return nil, false
	}
	segs := strings.Split(rest, "/")
	for _, s := range segs {
		if s == "" {
			return nil, false
		}
	}
	return segs, true
}

// RecoveryCentralReads keeps central (director-1/operator-desk) reads
// available while WRK_RECORDS_RECOVERY_REQUIRED freezes the store; every
// other read and every write answers wrk_recovery_required (operator answer
// dr-1925-1 = A).
const RecoveryCentralReads = true

func (s Server) wrkRecoveryBlocks(client string) bool {
	return s.Wrk.Recovery && !(RecoveryCentralReads && s.Wrk.Policy.Central(client))
}

func (s Server) wrkRead(w http.ResponseWriter, r *http.Request, client, perm string, next func()) {
	if s.wrkRecoveryBlocks(client) {
		wrkErr(w, wrk.RecoveryRequired)
		return
	}
	if !s.Wrk.Policy.Has(client, perm) {
		wrkErr(w, wrk.ScopeDenied)
		return
	}
	if r.ContentLength > 0 {
		wrkErr(w, wrk.InvalidRequest)
		return
	}
	if r.Body != nil {
		var probe [1]byte
		if n, _ := r.Body.Read(probe[:]); n > 0 {
			wrkErr(w, wrk.InvalidRequest)
			return
		}
	}
	next()
}

// Capabilities is the GET /v1/wrk/capabilities body.
type Capabilities struct {
	Contract        string `json:"contract"`
	StoreGeneration string `json:"store_generation"`
	ClientID        string `json:"client_id"`
	PolicySHA256    string `json:"policy_sha256"`
	WritesAvailable bool   `json:"writes_available"`
}

func (s Server) wrkCapabilities(w http.ResponseWriter, r *http.Request, client string) {
	if s.wrkRecoveryBlocks(client) {
		wrkErr(w, wrk.RecoveryRequired)
		return
	}
	if !s.Wrk.Policy.Has(client, wrk.PermCapabilitiesRead) {
		wrkErr(w, wrk.ScopeDenied)
		return
	}
	if len(r.URL.Query()) > 0 || r.ContentLength > 0 {
		wrkErr(w, wrk.InvalidRequest)
		return
	}
	wrkOK(w, http.StatusOK, Capabilities{Contract: wrk.ContractVersion, StoreGeneration: s.Wrk.Engine.Generation, ClientID: client, PolicySHA256: s.Wrk.Policy.SHA256(), WritesAvailable: !s.Wrk.Recovery && s.Wrk.Mode != WrkOff})
}

func (s Server) wrkJob(w http.ResponseWriter, r *http.Request, job string) {
	if !wrk.ValidJobID(job) || len(r.URL.Query()) > 0 {
		wrkErr(w, wrk.InvalidRequest)
		return
	}
	var claim wrk.Record
	var sum wrk.Summary
	found := false
	err := s.Wrk.Backend.View(r.Context(), func(tx wrk.Tx) error {
		j, err := tx.Journal(job)
		if err != nil {
			return err
		}
		for _, rec := range j.Records {
			if rec.Kind == "claim" {
				claim, found = rec, true
			}
		}
		if !found {
			return nil
		}
		tail, _ := j.Tail()
		sum, err = s.Wrk.Engine.SummaryAt(tx, job, tail)
		return err
	})
	if err != nil {
		wrkErr(w, wrk.Unavailable)
		return
	}
	if !found {
		wrkErr(w, wrk.NotFound)
		return
	}
	wrkOK(w, http.StatusOK, map[string]any{"contract": wrk.ContractVersion, "claim": claim, "summary": sum})
}

// cursorInt parses one numeric query value (safe nonnegative decimal).
func cursorInt(q url.Values, key string, def int64) (int64, bool, bool) {
	vals, present := q[key]
	if !present {
		return def, false, true
	}
	if len(vals) != 1 || vals[0] == "" {
		return 0, true, false
	}
	for _, c := range vals[0] {
		if c < '0' || c > '9' {
			return 0, true, false
		}
	}
	n, err := strconv.ParseInt(vals[0], 10, 64)
	if err != nil || n > wrk.MaxSafeInteger {
		return 0, true, false
	}
	return n, true, true
}

func (s Server) wrkRecords(w http.ResponseWriter, r *http.Request, job string) {
	if !wrk.ValidJobID(job) {
		wrkErr(w, wrk.InvalidRequest)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		wrkErr(w, wrk.InvalidRequest)
		return
	}
	for k := range q {
		if k != "after_seq" && k != "snapshot_seq" && k != "limit" {
			wrkErr(w, wrk.InvalidRequest)
			return
		}
	}
	after, _, okA := cursorInt(q, "after_seq", 0)
	limit, _, okL := cursorInt(q, "limit", 100)
	snap, snapSet, okS := cursorInt(q, "snapshot_seq", 0)
	if !okA || !okL || !okS || limit < 1 || limit > 500 || (snapSet && snap < 1) {
		wrkErr(w, wrk.InvalidCursor)
		return
	}
	var page []wrk.Record
	var sum wrk.Summary
	var code wrk.Code
	err = s.Wrk.Backend.View(r.Context(), func(tx wrk.Tx) error {
		j, err := tx.Journal(job)
		if err != nil {
			return err
		}
		tail, _ := j.Tail()
		if tail == 0 {
			code = wrk.NotFound
			return nil
		}
		if !snapSet {
			snap = tail
		}
		if snap > tail || after > snap {
			code = wrk.InvalidCursor
			return nil
		}
		page = []wrk.Record{}
		for _, rec := range j.Records {
			if rec.Seq > after && rec.Seq <= snap && int64(len(page)) < limit {
				page = append(page, rec)
			}
		}
		sum, err = s.Wrk.Engine.SummaryAt(tx, job, snap)
		return err
	})
	if err != nil {
		wrkErr(w, wrk.Unavailable)
		return
	}
	if code != "" {
		wrkErr(w, code)
		return
	}
	next := after
	if len(page) > 0 {
		next = page[len(page)-1].Seq
	}
	wrkOK(w, http.StatusOK, map[string]any{"contract": wrk.ContractVersion, "snapshot_seq": snap, "next_after_seq": next, "has_more": next < snap, "records": page, "summary": sum})
}

var contentKinds = map[string]bool{"verdict": true, "completed": true, "joined": true, "import": true}

func (s Server) wrkContent(w http.ResponseWriter, r *http.Request, job, seqText string) {
	if !wrk.ValidJobID(job) || len(r.URL.Query()) > 0 {
		wrkErr(w, wrk.InvalidRequest)
		return
	}
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if err != nil || seq < 1 || strconv.FormatInt(seq, 10) != seqText {
		wrkErr(w, wrk.NotFound)
		return
	}
	var body []byte
	found := false
	err = s.Wrk.Backend.View(r.Context(), func(tx wrk.Tx) error {
		j, err := tx.Journal(job)
		if err != nil {
			return err
		}
		for _, rec := range j.Records {
			if rec.Seq == seq && contentKinds[rec.Kind] {
				body, found, err = tx.Content(job, seq)
				return err
			}
		}
		return nil
	})
	if err != nil {
		wrkErr(w, wrk.Unavailable)
		return
	}
	if !found {
		wrkErr(w, wrk.NotFound)
		return
	}
	wrkOK(w, http.StatusOK, map[string]any{"job_id": job, "seq": seq, "content_b64": encodeB64(body), "content_sha256": wrk.ContentSHA256(body), "size_bytes": len(body)})
}

// wrkAttempt audits a write refused before the engine saw it: only the
// authenticated id (if any), the path job id when valid and the stable code
// are kept (design §2 wrk_attempts). Failures to audit never change the
// refusal.
func (s Server) wrkAttempt(r *http.Request, client string, code wrk.Code) {
	if r.Method != http.MethodPost || s.Wrk == nil || s.Wrk.Backend == nil || s.Wrk.Mode == WrkOff {
		return
	}
	job := ""
	if segs, ok := wrkPath(r); ok && len(segs) >= 2 && segs[0] == "jobs" && wrk.ValidJobID(segs[1]) {
		job = segs[1]
	}
	_ = s.Wrk.Backend.RecordAttempt(r.Context(), wrk.Attempt{AttemptID: wrk.NewUUID(), ClientID: client, JobID: job, Code: code, ReceivedAt: time.Now().UTC()})
}

func (s Server) wrkAppend(w http.ResponseWriter, r *http.Request, client, job string) {
	refuse := func(code wrk.Code) {
		s.wrkAttempt(r, client, code)
		wrkErr(w, code)
	}
	if s.Wrk.Recovery {
		refuse(wrk.RecoveryRequired)
		return
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" || (len(params) > 0 && !(len(params) == 1 && strings.EqualFold(params["charset"], "utf-8"))) {
		refuse(wrk.UnsupportedMediaType)
		return
	}
	if r.ContentLength > wrk.MaxRequestBytes {
		refuse(wrk.RequestTooLarge)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, wrk.MaxRequestBytes+1))
	if err != nil {
		refuse(wrk.InvalidRequest)
		return
	}
	if len(raw) > wrk.MaxRequestBytes {
		refuse(wrk.RequestTooLarge)
		return
	}
	if len(r.URL.Query()) > 0 || !wrk.ValidJobID(job) {
		refuse(wrk.InvalidRequest)
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || !wrk.ValidUUID(keys[0]) {
		refuse(wrk.InvalidRequest)
		return
	}
	res, err := s.Wrk.Engine.Append(r.Context(), s.Wrk.Backend, client, job, keys[0], raw)
	if err != nil {
		wrkErr(w, codeOf(err))
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	wrkOK(w, status, map[string]any{"contract": wrk.ContractVersion, "created": res.Created, "record": res.Record, "summary": res.Summary})
}

func encodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// IsWrkPath reports a request path in the /v1/wrk namespace. Callers that
// mount the API under another ServeMux route these paths around it: a
// ServeMux answers non-canonical paths with a redirect, and the namespace
// never emits a Location.
func IsWrkPath(path string) bool {
	return path == "/v1/wrk" || strings.HasPrefix(path, "/v1/wrk/")
}
