package client

import (
	"context"
	"net/url"
)

// AttachmentUploadTicket is what BeginAttachmentUpload answers: the upload id
// to complete with, and a presigned PUT for the staging object. The URL is a
// bearer credential for that one object.
type AttachmentUploadTicket struct {
	UploadID         string `json:"upload_id"`
	Method           string `json:"method"`
	URL              string `json:"url"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

// BeginAttachmentUpload declares an attachment before its bytes move. hk
// checks the declaration and answers a presigned PUT; the caller PUTs size
// bytes to it, then calls CompleteAttachmentUpload.
func (c Client) BeginAttachmentUpload(ctx context.Context, name, mime string, size int64, sha, ref string) (AttachmentUploadTicket, error) {
	var out AttachmentUploadTicket
	err := c.transport().Call(ctx, "POST", "/v1/attachments/uploads", map[string]any{"name": name, "mime": mime, "size": size, "sha256": sha, "ref": ref}, &out)
	return out, err
}

// CompleteAttachmentUpload checks the staged bytes against the declaration and
// stores them. created is false when the upload was already completed.
func (c Client) CompleteAttachmentUpload(ctx context.Context, id string) (Attachment, bool, error) {
	var out struct {
		Attachment Attachment `json:"attachment"`
		Created    bool       `json:"created"`
	}
	err := c.transport().Call(ctx, "POST", "/v1/attachments/uploads/"+url.PathEscape(id)+"/complete", struct{}{}, &out)
	return out.Attachment, out.Created, err
}
