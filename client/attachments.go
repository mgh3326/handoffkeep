package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/hktransport"
)

// Attachment is an attachment row as /v1/attachments answers it. It has the
// store row's exact fields, so internal/remote converts it with a plain type
// conversion.
type Attachment struct {
	SHA256       string    `json:"sha256"`
	SizeBytes    int64     `json:"size_bytes"`
	MIME         string    `json:"mime"`
	OriginalName string    `json:"original_name"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	RefKind      string    `json:"ref_kind,omitempty"`
	RefID        string    `json:"ref_id,omitempty"`
	// ArchivedAt and ArchiveLocation are set once hk records the verified
	// archive copy of the blob; the hot object is then gone when hot_deleted_at
	// was set (design/2026-11-16/hk-attachment-archive).
	ArchivedAt      *time.Time `json:"archived_at"`
	ArchiveLocation string     `json:"archive_location"`
}

// ArchiveCandidateTask is one closed task that refers to a candidate.
type ArchiveCandidateTask struct {
	ID       int64     `json:"id"`
	State    string    `json:"state"`
	ClosedAt time.Time `json:"closed_at"`
}

// ArchiveCandidate is one blob the archiver may copy out. TaskID is the
// storage task, the lowest-id referring task.
type ArchiveCandidate struct {
	SHA256       string                 `json:"sha256"`
	SizeBytes    int64                  `json:"size_bytes"`
	MIME         string                 `json:"mime"`
	OriginalName string                 `json:"original_name"`
	TaskID       int64                  `json:"task_id"`
	Tasks        []ArchiveCandidateTask `json:"tasks"`
}

// ArchiveCandidates is one page of candidates; Next is the cursor for the
// following page, empty when this is the last.
type ArchiveCandidates struct {
	Candidates []ArchiveCandidate `json:"candidates"`
	Next       string             `json:"next"`
}

// AttachmentArchive is the recorded result of one archive call.
type AttachmentArchive struct {
	ArchivedAt time.Time `json:"archived_at"`
	Location   string    `json:"location"`
	HotDeleted bool      `json:"hot_deleted"`
}

// ArchiveCandidates pages attachments whose referring tasks are all closed,
// at least seven days ago. limit is the page size (the server defaults to 100
// and caps at 1000); after is the previous page's Next, empty for the first.
func (c Client) ArchiveCandidates(ctx context.Context, limit int, after string) (ArchiveCandidates, error) {
	t := c.transport()
	path := "/v1/attachments/archive-candidates"
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if after != "" {
		q.Set("after", after)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	r, e := t.NewRequest(ctx, "GET", path, nil)
	if e != nil {
		return ArchiveCandidates{}, e
	}
	resp, e := t.Do(r)
	if e != nil {
		return ArchiveCandidates{}, e
	}
	defer resp.Body.Close()
	if hktransport.IsRedirectStatus(resp.StatusCode) {
		return ArchiveCandidates{}, t.RedirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ArchiveCandidates{}, attachmentHTTPError(resp)
	}
	if e := hktransport.HTMLBodyError(resp); e != nil {
		return ArchiveCandidates{}, e
	}
	var out ArchiveCandidates
	e = json.NewDecoder(resp.Body).Decode(&out)
	return out, e
}

// RecordAttachmentArchive records that the blob at sha was verified at
// location with the given size, after the archiver read the copy back. A
// repeat with the same location answers the recorded result.
func (c Client) RecordAttachmentArchive(ctx context.Context, sha, location string, size int64) (AttachmentArchive, error) {
	t := c.transport()
	body, e := json.Marshal(map[string]any{"location": location, "size_bytes": size, "sha256": sha})
	if e != nil {
		return AttachmentArchive{}, e
	}
	r, e := t.NewRequest(ctx, "POST", "/v1/attachments/"+url.PathEscape(sha)+"/archive", bytes.NewReader(body))
	if e != nil {
		return AttachmentArchive{}, e
	}
	r.Header.Set("Content-Type", "application/json")
	resp, e := t.Do(r)
	if e != nil {
		return AttachmentArchive{}, e
	}
	defer resp.Body.Close()
	if hktransport.IsRedirectStatus(resp.StatusCode) {
		return AttachmentArchive{}, t.RedirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AttachmentArchive{}, attachmentHTTPError(resp)
	}
	if e := hktransport.HTMLBodyError(resp); e != nil {
		return AttachmentArchive{}, e
	}
	var out AttachmentArchive
	e = json.NewDecoder(resp.Body).Decode(&out)
	return out, e
}

// attachmentHTTPError decodes a non-2xx answer into *HTTPError. The body is
// the server's {"error": "<code>"} envelope with no secret in it.
func attachmentHTTPError(resp *http.Response) error {
	var x struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&x)
	return &HTTPError{Status: resp.StatusCode, Code: x.Error}
}

// ArchiveCandidateTask is one closed task that refers to a candidate.
type ArchiveCandidateTask struct {
	ID       int64     `json:"id"`
	State    string    `json:"state"`
	ClosedAt time.Time `json:"closed_at"`
}

// ArchiveCandidate is one blob the archiver may copy out. TaskID is the
// storage task, the lowest-id referring task.
type ArchiveCandidate struct {
	SHA256       string                 `json:"sha256"`
	SizeBytes    int64                  `json:"size_bytes"`
	MIME         string                 `json:"mime"`
	OriginalName string                 `json:"original_name"`
	TaskID       int64                  `json:"task_id"`
	Tasks        []ArchiveCandidateTask `json:"tasks"`
}

// ArchiveCandidates is one page of candidates; Next is the cursor for the
// following page, empty when this is the last.
type ArchiveCandidates struct {
	Candidates []ArchiveCandidate `json:"candidates"`
	Next       string             `json:"next"`
}

// AttachmentArchive is the recorded result of one archive call.
type AttachmentArchive struct {
	ArchivedAt time.Time `json:"archived_at"`
	Location   string    `json:"location"`
	HotDeleted bool      `json:"hot_deleted"`
}

// ArchiveCandidates pages attachments whose referring tasks are all closed,
// at least seven days ago. limit is the page size (the server defaults to 100
// and caps at 1000); after is the previous page's Next, empty for the first.
func (c Client) ArchiveCandidates(ctx context.Context, limit int, after string) (ArchiveCandidates, error) {
	t := c.transport()
	path := "/v1/attachments/archive-candidates"
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if after != "" {
		q.Set("after", after)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	r, e := t.NewRequest(ctx, "GET", path, nil)
	if e != nil {
		return ArchiveCandidates{}, e
	}
	resp, e := t.Do(r)
	if e != nil {
		return ArchiveCandidates{}, e
	}
	defer resp.Body.Close()
	if hktransport.IsRedirectStatus(resp.StatusCode) {
		return ArchiveCandidates{}, t.RedirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ArchiveCandidates{}, attachmentHTTPError(resp)
	}
	if e := hktransport.HTMLBodyError(resp); e != nil {
		return ArchiveCandidates{}, e
	}
	var out ArchiveCandidates
	e = json.NewDecoder(resp.Body).Decode(&out)
	return out, e
}

// RecordAttachmentArchive records that the blob at sha was verified at
// location with the given size, after the archiver read the copy back. A
// repeat with the same location answers the recorded result.
func (c Client) RecordAttachmentArchive(ctx context.Context, sha, location string, size int64) (AttachmentArchive, error) {
	t := c.transport()
	body, e := json.Marshal(map[string]any{"location": location, "size_bytes": size, "sha256": sha})
	if e != nil {
		return AttachmentArchive{}, e
	}
	r, e := t.NewRequest(ctx, "POST", "/v1/attachments/"+url.PathEscape(sha)+"/archive", bytes.NewReader(body))
	if e != nil {
		return AttachmentArchive{}, e
	}
	r.Header.Set("Content-Type", "application/json")
	resp, e := t.Do(r)
	if e != nil {
		return AttachmentArchive{}, e
	}
	defer resp.Body.Close()
	if hktransport.IsRedirectStatus(resp.StatusCode) {
		return AttachmentArchive{}, t.RedirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AttachmentArchive{}, attachmentHTTPError(resp)
	}
	if e := hktransport.HTMLBodyError(resp); e != nil {
		return AttachmentArchive{}, e
	}
	var out AttachmentArchive
	e = json.NewDecoder(resp.Body).Decode(&out)
	return out, e
}

// attachmentHTTPError decodes a non-2xx answer into *HTTPError. The body is
// the server's {"error": "<code>"} envelope with no secret in it.
func attachmentHTTPError(resp *http.Response) error {
	var x struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&x)
	return &HTTPError{Status: resp.StatusCode, Code: x.Error}
}

// PutAttachment uploads body under name with its MIME type and ref
// ("kind:id"). created is false when the same bytes were already stored.
func (c Client) PutAttachment(ctx context.Context, name, mime, ref string, body []byte) (Attachment, bool, error) {
	t := c.transport()
	r, e := t.NewRequest(ctx, "PUT", "/v1/attachments", bytes.NewReader(body))
	if e != nil {
		return Attachment{}, false, e
	}
	r.Header.Set("X-HK-Name", name)
	r.Header.Set("Content-Type", mime)
	r.Header.Set("X-HK-Ref", ref)
	resp, e := t.Do(r)
	if e != nil {
		return Attachment{}, false, e
	}
	defer resp.Body.Close()
	if hktransport.IsRedirectStatus(resp.StatusCode) {
		return Attachment{}, false, t.RedirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Attachment{}, false, attachmentHTTPError(resp)
	}
	if e := hktransport.HTMLBodyError(resp); e != nil {
		return Attachment{}, false, e
	}
	var x struct {
		Attachment Attachment `json:"attachment"`
		Created    bool       `json:"created"`
	}
	e = json.NewDecoder(resp.Body).Decode(&x)
	return x.Attachment, x.Created, e
}

// AttachmentURL asks hk for a presigned storage URL. The presign redirect is
// never followed — no hk header reaches the storage host — and its Location is
// returned unless it points at the Access login or changes scheme on the hk
// host.
func (c Client) AttachmentURL(ctx context.Context, sha string) (string, error) {
	t := c.transport()
	r, e := t.NewRequest(ctx, "GET", "/v1/attachments/"+url.PathEscape(sha)+"?presign=1", nil)
	if e != nil {
		return "", e
	}
	resp, e := t.DoNoFollow(r)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusFound {
		loc := resp.Header.Get("Location")
		// A presign answer pointing at the Access login is the same
		// missing-token failure as on the JSON routes; a same-host scheme
		// change is a downgrade or upgrade outside the configured scheme,
		// not a usable storage URL. The Location value is never printed.
		if u, pe := url.Parse(loc); pe == nil && (hktransport.CFAccessLoginHost(u.Hostname()) || t.SameHostSchemeChange(u)) {
			return "", t.RedirectError(resp)
		}
		return loc, nil
	}
	if hktransport.IsRedirectStatus(resp.StatusCode) {
		return "", t.RedirectError(resp)
	}
	return "", errors.New("attachment_url_failed")
}

// GetAttachment streams an attachment's bytes through hk; the caller closes
// the body. The returned Attachment carries only the SHA-256 and MIME type.
func (c Client) GetAttachment(ctx context.Context, sha string) (Attachment, io.ReadCloser, error) {
	t := c.transport()
	r, e := t.NewRequest(ctx, "GET", "/v1/attachments/"+url.PathEscape(sha), nil)
	if e != nil {
		return Attachment{}, nil, e
	}
	resp, e := t.Do(r)
	if e != nil {
		return Attachment{}, nil, e
	}
	if hktransport.IsRedirectStatus(resp.StatusCode) {
		defer resp.Body.Close()
		return Attachment{}, nil, t.RedirectError(resp)
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		var x struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return Attachment{}, nil, &HTTPError{Status: resp.StatusCode, Code: x.Error}
	}
	// With the pair configured a 200 text/html that lacks an attachment
	// disposition is an Access login page, not file bytes — the real server
	// always sets Content-Disposition: attachment on this route, so a real
	// .html attachment still downloads. Without keys the historical
	// passthrough is unchanged.
	if t.CFAccessConfigured() {
		cd, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		if !strings.EqualFold(cd, "attachment") {
			if e := hktransport.HTMLBodyError(resp); e != nil {
				defer resp.Body.Close()
				return Attachment{}, nil, e
			}
		}
	}
	return Attachment{SHA256: sha, MIME: resp.Header.Get("Content-Type")}, resp.Body, nil
}
