package remote

import (
	"context"

	"github.com/mgh3326/handoffkeep/client"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// ArchiveCandidates is the remote backend's candidate page for the archiver.
// next is the cursor for the following page, empty on the last one.
func (c Client) ArchiveCandidates(ctx context.Context, limit int, after string) ([]store.AttachmentArchiveCandidate, string, error) {
	page, e := client.Client(c).ArchiveCandidates(ctx, limit, after)
	if e != nil {
		return nil, "", e
	}
	out := make([]store.AttachmentArchiveCandidate, 0, len(page.Candidates))
	for _, x := range page.Candidates {
		tasks := make([]store.AttachmentArchiveCandidateTask, 0, len(x.Tasks))
		for _, t := range x.Tasks {
			tasks = append(tasks, store.AttachmentArchiveCandidateTask{ID: t.ID, State: t.State, ClosedAt: t.ClosedAt})
		}
		out = append(out, store.AttachmentArchiveCandidate{SHA256: x.SHA256, SizeBytes: x.SizeBytes, MIME: x.MIME, OriginalName: x.OriginalName, TaskID: x.TaskID, Tasks: tasks})
	}
	return out, page.Next, nil
}

// RecordAttachmentArchive is the remote backend's archive record.
func (c Client) RecordAttachmentArchive(ctx context.Context, sha, location string, size int64) (store.AttachmentArchive, error) {
	rec, e := client.Client(c).RecordAttachmentArchive(ctx, sha, location, size)
	return store.AttachmentArchive{ArchivedAt: rec.ArchivedAt, Location: rec.Location, HotDeleted: rec.HotDeleted}, e
}
