package client

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// Document is an hk document. PutDocument sends Key, Kind, Session, Job,
// Body, TaskID and OriginClass; the server fills in the rest.
type Document struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	Session   string    `json:"session"`
	Job       string    `json:"job"`
	Body      string    `json:"body,omitempty"`
	SHA256    string    `json:"sha256"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// TaskID names the task the document belongs to; nil is unknown or none.
	TaskID *int64 `json:"task_id,omitempty"`
	// OriginClass is authored, job or import; empty is unknown.
	OriginClass string `json:"origin_class,omitempty"`
}

// GetDocument reads one document by key; found is false when the server
// answers not_found.
func (c Client) GetDocument(ctx context.Context, key string) (Document, bool, error) {
	var out Document
	e := c.transport().Call(ctx, "GET", "/v1/documents/"+url.PathEscape(key), nil, &out)
	if ErrorCode(e) == "not_found" {
		return out, false, nil
	}
	return out, e == nil, e
}

// ListDocuments lists documents by key prefix in key order, as the CLI's doc
// list does; an empty prefix lists every document. The server matches the
// prefix with SQL LIKE, so a _ or % in it is a wildcard: check each Key
// against the prefix before acting on it. Limit must be 1 to 1000; the server
// refuses any other value. Listed documents carry no Body: read each one with
// GetDocument.
func (c Client) ListDocuments(ctx context.Context, prefix string, limit int) ([]Document, error) {
	var out struct {
		Documents []Document `json:"documents"`
	}
	q := url.Values{"prefix": {prefix}, "kind": {""}, "session": {""}, "limit": {fmt.Sprint(limit)}}
	if e := c.transport().Call(ctx, "GET", "/v1/documents?"+q.Encode(), nil, &out); e != nil {
		return nil, e
	}
	return out.Documents, nil
}

// PutDocument writes x under x.Key. changed is false when the stored document
// already had this content.
func (c Client) PutDocument(ctx context.Context, x Document) (Document, bool, error) {
	var out struct {
		Document Document `json:"document"`
		Changed  bool     `json:"changed"`
	}
	e := c.transport().Call(ctx, "PUT", "/v1/documents/"+url.PathEscape(x.Key), x, &out)
	return out.Document, out.Changed, e
}
