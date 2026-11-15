package client_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/client"
)

// An archive-route refusal keeps the reason and secret pattern the server sent,
// as hktransport.Call does: the archiver reports the reason and a
// secret_like_content answer names its pattern.
func TestArchiveErrorsCarryReasonAndPattern(t *testing.T) {
	hk := newFakeHK(t)
	hk.on("GET", "/v1/attachments/archive-candidates?limit=100", http.StatusConflict,
		`{"error":"attachment_not_archivable","reason":"a task is still open"}`, nil)
	hk.on("POST", "/v1/attachments/"+attachmentSHA+"/archive", http.StatusBadRequest,
		`{"error":"secret_like_content","pattern":"aws_access_key","reason":"the location holds a key"}`, nil)
	c := client.Client{URL: hk.URL, Token: fixtureToken}

	_, err := c.ArchiveCandidates(context.Background(), 100, "")
	if client.ErrorCode(err) != "attachment_not_archivable" || !strings.Contains(err.Error(), "a task is still open") {
		t.Fatalf("candidates error = %v (%q), want the code and reason", err, client.ErrorCode(err))
	}

	_, err = c.RecordAttachmentArchive(context.Background(), attachmentSHA, "fleet-artifacts/x", 1)
	if p, ok := client.SecretPattern(err); !ok || p != "aws_access_key" {
		t.Fatalf("record error = %v, pattern = %q ok=%t, want aws_access_key", err, p, ok)
	}
	var h *client.HTTPError
	if !errors.As(err, &h) || h.Reason != "the location holds a key" {
		t.Fatalf("record error = %v, want the reason on the HTTPError", err)
	}
}
