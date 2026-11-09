package remote

import (
	"context"

	"github.com/mgh3326/handoffkeep/client"
	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// BeginAttachmentUpload is the remote backend's direct-upload begin; the client
// argument is ignored because the server names the caller from its token.
func (c Client) BeginAttachmentUpload(ctx context.Context, _ string, name, mime string, size int64, sha, ref string) (attachments.UploadTicket, error) {
	t, e := client.Client(c).BeginAttachmentUpload(ctx, name, mime, size, sha, ref)
	return attachments.UploadTicket{UploadID: t.UploadID, Method: t.Method, URL: t.URL, ExpiresInSeconds: t.ExpiresInSeconds}, e
}

// CompleteAttachmentUpload is the remote backend's direct-upload completion.
func (c Client) CompleteAttachmentUpload(ctx context.Context, _ string, id string) (store.Attachment, bool, error) {
	x, created, e := client.Client(c).CompleteAttachmentUpload(ctx, id)
	return store.Attachment(x), created, e
}
