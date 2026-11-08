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
	"strings"
	"time"
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
}

// PutAttachment uploads body under name with its MIME type and ref
// ("kind:id"). created is false when the same bytes were already stored.
func (c Client) PutAttachment(ctx context.Context, name, mime, ref string, body []byte) (Attachment, bool, error) {
	r, e := c.NewRequest(ctx, "PUT", "/v1/attachments", bytes.NewReader(body))
	if e != nil {
		return Attachment{}, false, e
	}
	r.Header.Set("X-HK-Name", name)
	r.Header.Set("Content-Type", mime)
	r.Header.Set("X-HK-Ref", ref)
	resp, e := c.Do(r)
	if e != nil {
		return Attachment{}, false, e
	}
	defer resp.Body.Close()
	if IsRedirectStatus(resp.StatusCode) {
		return Attachment{}, false, c.RedirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return Attachment{}, false, &HTTPError{Status: resp.StatusCode, Code: x.Error}
	}
	if e := HTMLBodyError(resp); e != nil {
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
	r, e := c.NewRequest(ctx, "GET", "/v1/attachments/"+url.PathEscape(sha)+"?presign=1", nil)
	if e != nil {
		return "", e
	}
	resp, e := c.do(c.HTTPClientNoFollow(), r)
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
		if u, pe := url.Parse(loc); pe == nil && (cfAccessLoginHost(u.Hostname()) || c.sameHostSchemeChange(u)) {
			return "", c.RedirectError(resp)
		}
		return loc, nil
	}
	if IsRedirectStatus(resp.StatusCode) {
		return "", c.RedirectError(resp)
	}
	return "", errors.New("attachment_url_failed")
}

// GetAttachment streams an attachment's bytes through hk; the caller closes
// the body. The returned Attachment carries only the SHA-256 and MIME type.
func (c Client) GetAttachment(ctx context.Context, sha string) (Attachment, io.ReadCloser, error) {
	r, e := c.NewRequest(ctx, "GET", "/v1/attachments/"+url.PathEscape(sha), nil)
	if e != nil {
		return Attachment{}, nil, e
	}
	resp, e := c.Do(r)
	if e != nil {
		return Attachment{}, nil, e
	}
	if IsRedirectStatus(resp.StatusCode) {
		defer resp.Body.Close()
		return Attachment{}, nil, c.RedirectError(resp)
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
	if c.cfAccessConfigured() {
		cd, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		if !strings.EqualFold(cd, "attachment") {
			if e := HTMLBodyError(resp); e != nil {
				defer resp.Body.Close()
				return Attachment{}, nil, e
			}
		}
	}
	return Attachment{SHA256: sha, MIME: resp.Header.Get("Content-Type")}, resp.Body, nil
}
