package client

import (
	"context"
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
