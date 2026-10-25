package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

// WrkCall sends exactly one request, buffers and validates its entire response,
// and returns only a closed code on failure. It never forwards a redirect.
func (c Client) WrkCall(ctx context.Context, method, path string, body []byte, key, def string) ([]byte, string) {
	req, e := c.newRequest(ctx, method, path, io.LimitReader(bytes.NewReader(body), int64(len(body))))
	if e != nil {
		return nil, "transport_unavailable"
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
	}
	hc := c.httpClientNoFollow()
	// A fresh connection also prevents net/http's implicit retry on a reused
	// connection; wrk's durable retry policy belongs exclusively to C1.
	transport := hc.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		clone := t.Clone()
		clone.DisableKeepAlives = true
		hc.Transport = clone
		defer clone.CloseIdleConnections()
	}
	response, e := hc.Do(req)
	if e != nil {
		var op *net.OpError
		if errors.As(e, &op) && op.Op == "dial" {
			return nil, "transport_unavailable"
		}
		if method == "POST" {
			return nil, "transport_indeterminate"
		}
		return nil, "transport_unavailable"
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, "protocol_invalid"
	}
	media, _, e := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if e != nil || media != "application/json" {
		if response.StatusCode == 404 {
			return nil, "o3_unsupported"
		}
		return nil, "protocol_invalid"
	}
	b, e := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if e != nil {
		return nil, "protocol_invalid"
	}
	v, e := wrkrecords.DecodeResponse(b)
	if e != nil {
		return nil, "protocol_invalid"
	}
	if response.StatusCode != 200 && response.StatusCode != 201 {
		if wrkrecords.Validate("Error", v) != nil {
			return nil, "protocol_invalid"
		}
		m, _ := v.(map[string]any)
		code := wrkrecords.Text(m, "error")
		if wrkrecords.Status(wrkrecords.Error(code)) != response.StatusCode {
			return nil, "protocol_invalid"
		}
		return nil, code
	}
	if (method == "GET" && response.StatusCode != 200) || (method == "POST" && response.StatusCode != 200 && response.StatusCode != 201) || wrkrecords.Validate(def, v) != nil {
		return nil, "protocol_invalid"
	}
	m, _ := v.(map[string]any)
	if def == "WriteResponse" {
		created, ok := m["created"].(bool)
		if !ok || created != (response.StatusCode == 201) {
			return nil, "protocol_invalid"
		}
	}
	if def == "ContentResponse" {
		data, x := wrkrecords.Evidence(wrkrecords.Text(m, "content_b64"), wrkrecords.Text(m, "content_sha256"), false)
		if x != nil || int64(len(data)) != wrkrecords.Num(m, "size_bytes") {
			return nil, "protocol_invalid"
		}
	}
	if !wrkResponseConsistent(method, path, key, def, body, v) {
		return nil, "protocol_invalid"
	}
	// Never publish the unchecked server byte stream. Compact one validated JSON
	// object; the CLI adds its sole terminating newline.
	clean, e := json.Marshal(v)
	if e != nil {
		return nil, "protocol_invalid"
	}
	return clean, ""
}
