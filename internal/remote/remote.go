// Package remote adapts the authenticated HTTP API for local CLI and stdio MCP.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type Client struct {
	URL, Token string
	HTTP       *http.Client
}

func (c Client) call(ctx context.Context, method, path string, input, output any) error {
	var body *bytes.Reader
	if input != nil {
		b, e := json.Marshal(input)
		if e != nil {
			return e
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	r, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if e != nil {
		return e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, e := h.Do(r)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct{ Error, Pattern string }
		_ = json.NewDecoder(resp.Body).Decode(&x)
		if x.Pattern != "" {
			return fmt.Errorf("%s:%s", x.Error, x.Pattern)
		}
		return errors.New(x.Error)
	}
	if output != nil {
		return json.NewDecoder(resp.Body).Decode(output)
	}
	return nil
}
func esc(x string) string { return url.PathEscape(x) }
func (c Client) Checkpoint(ctx context.Context, _ string, x store.Checkpoint) (store.Checkpoint, error) {
	var out store.Checkpoint
	e := c.call(ctx, "POST", "/v1/checkpoints", x, &out)
	return out, e
}
func (c Client) Recent(ctx context.Context, session, kind string, limit int) ([]store.Checkpoint, error) {
	var out struct {
		Checkpoints []store.Checkpoint `json:"checkpoints"`
	}
	e := c.call(ctx, "GET", "/v1/checkpoints?session="+url.QueryEscape(session)+"&kind="+url.QueryEscape(kind)+fmt.Sprintf("&limit=%d", limit), nil, &out)
	return out.Checkpoints, e
}
func (c Client) PutMemory(ctx context.Context, _ string, x store.Memory) (store.Memory, error) {
	var out store.Memory
	e := c.call(ctx, "PUT", "/v1/memory/"+esc(x.Agent)+"/"+esc(x.Name), x, &out)
	return out, e
}
func (c Client) GetMemory(ctx context.Context, agent, name string) (store.Memory, bool, error) {
	var out store.Memory
	e := c.call(ctx, "GET", "/v1/memory/"+esc(agent)+"/"+esc(name), nil, &out)
	if e != nil && e.Error() == "not_found" {
		return out, false, nil
	}
	return out, e == nil, e
}
func (c Client) ListMemory(ctx context.Context, agent string, content bool) ([]store.Memory, error) {
	var out struct {
		Memory []store.Memory `json:"memory"`
	}
	e := c.call(ctx, "GET", "/v1/memory/"+esc(agent), nil, &out)
	if e != nil || !content {
		return out.Memory, e
	}
	for i := range out.Memory {
		item, found, err := c.GetMemory(ctx, agent, out.Memory[i].Name)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("memory disappeared during pull")
		}
		out.Memory[i] = item
	}
	return out.Memory, nil
}
func (c Client) PutDocument(ctx context.Context, _ string, x store.Document) (store.Document, bool, error) {
	var out struct {
		Document store.Document `json:"document"`
		Changed  bool           `json:"changed"`
	}
	e := c.call(ctx, "PUT", "/v1/documents/"+esc(x.Key), x, &out)
	return out.Document, out.Changed, e
}
func (c Client) GetDocument(ctx context.Context, key string) (store.Document, bool, error) {
	var out store.Document
	e := c.call(ctx, "GET", "/v1/documents/"+esc(key), nil, &out)
	if e != nil && e.Error() == "not_found" {
		return out, false, nil
	}
	return out, e == nil, e
}
func (c Client) ListDocuments(ctx context.Context, prefix, kind, session string, limit int) ([]store.Document, error) {
	var out struct {
		Documents []store.Document `json:"documents"`
	}
	p := url.Values{"prefix": {prefix}, "kind": {kind}, "session": {session}, "limit": {fmt.Sprint(limit)}}
	e := c.call(ctx, "GET", "/v1/documents?"+p.Encode(), nil, &out)
	return out.Documents, e
}
func (c Client) Search(ctx context.Context, q, scope, session string, limit int) ([]store.SearchResult, error) {
	var out struct {
		Results []store.SearchResult `json:"results"`
	}
	p := url.Values{"q": {q}, "scope": {scope}, "session": {session}, "limit": {fmt.Sprint(limit)}}
	e := c.call(ctx, "GET", "/v1/search?"+p.Encode(), nil, &out)
	return out.Results, e
}
func (c Client) PutAttachment(ctx context.Context, _ string, name, mime, ref string, body []byte) (store.Attachment, bool, error) {
	r, e := http.NewRequestWithContext(ctx, "PUT", strings.TrimRight(c.URL, "/")+"/v1/attachments", bytes.NewReader(body))
	if e != nil {
		return store.Attachment{}, false, e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("X-HK-Name", name)
	r.Header.Set("Content-Type", mime)
	r.Header.Set("X-HK-Ref", ref)
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, e := h.Do(r)
	if e != nil {
		return store.Attachment{}, false, e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return store.Attachment{}, false, errors.New(x.Error)
	}
	var x struct {
		Attachment store.Attachment `json:"attachment"`
		Created    bool             `json:"created"`
	}
	e = json.NewDecoder(resp.Body).Decode(&x)
	return x.Attachment, x.Created, e
}
func (c Client) ListAttachments(ctx context.Context, ref string, limit int) ([]store.Attachment, error) {
	var x struct {
		Attachments []store.Attachment `json:"attachments"`
	}
	e := c.call(ctx, "GET", "/v1/attachments?"+url.Values{"ref": {ref}, "limit": {fmt.Sprint(limit)}}.Encode(), nil, &x)
	return x.Attachments, e
}
func (c Client) AttachmentURL(ctx context.Context, sha string) (string, error) {
	r, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.URL, "/")+"/v1/attachments/"+esc(sha)+"?presign=1", nil)
	if e != nil {
		return "", e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	h := c.HTTP
	if h == nil {
		h = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, e := h.Do(r)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", errors.New("attachment_url_failed")
	}
	return resp.Header.Get("Location"), nil
}
func (c Client) GetAttachment(ctx context.Context, sha string) (store.Attachment, io.ReadCloser, error) {
	r, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.URL, "/")+"/v1/attachments/"+esc(sha), nil)
	if e != nil {
		return store.Attachment{}, nil, e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, e := h.Do(r)
	if e != nil {
		return store.Attachment{}, nil, e
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		var x struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return store.Attachment{}, nil, errors.New(x.Error)
	}
	return store.Attachment{SHA256: sha, MIME: resp.Header.Get("Content-Type")}, resp.Body, nil
}
func (c Client) AttachmentUsage(ctx context.Context) (store.AttachmentUsage, error) {
	var x struct {
		Usage store.AttachmentUsage `json:"usage"`
	}
	e := c.call(ctx, "GET", "/v1/usage", nil, &x)
	return x.Usage, e
}
