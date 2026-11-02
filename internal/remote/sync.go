package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const syncDeltaMaxBytes = 2 << 20

// SyncDelta validates a complete bounded NDJSON page, including its trailer.
func (c Client) SyncDelta(ctx context.Context, after int64, limit int) ([]store.SyncChange, store.SyncMetadata, error) {
	var meta store.SyncMetadata
	if after < 0 || limit < 1 || limit > store.SyncLimit {
		return nil, meta, store.ErrInvalidSync
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/sync/delta?after="+strconv.FormatInt(after, 10)+"&limit="+strconv.Itoa(limit), nil)
	if err != nil {
		return nil, meta, err
	}
	resp, err := c.do(c.httpClient(), req)
	if err != nil {
		return nil, meta, err
	}
	defer resp.Body.Close()
	if isRedirectStatus(resp.StatusCode) {
		return nil, meta, c.redirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body struct {
			Error     string
			Reset     bool
			MinSyncID int64 `json:"min_sync_id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode == http.StatusGone && body.Reset {
			return nil, meta, &store.SyncReset{MinSyncID: body.MinSyncID}
		}
		return nil, meta, &HTTPError{Status: resp.StatusCode, Code: body.Error}
	}
	if err = htmlBodyError(resp); err != nil {
		return nil, meta, err
	}
	kind, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || kind != "application/x-ndjson" {
		return nil, meta, fmt.Errorf("unexpected sync content type")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, syncDeltaMaxBytes+1))
	if err != nil {
		return nil, meta, fmt.Errorf("read sync page: %w", err)
	}
	if len(body) > syncDeltaMaxBytes {
		return nil, meta, fmt.Errorf("sync page exceeds %d bytes", syncDeltaMaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	changes := make([]store.SyncChange, 0)
	last := after
	for {
		var line struct {
			store.SyncChange
			Metadata *store.SyncMetadata `json:"_metadata_"`
		}
		if err = dec.Decode(&line); err != nil {
			return nil, meta, fmt.Errorf("incomplete sync page: %w", err)
		}
		if line.Metadata != nil {
			meta = *line.Metadata
			if meta.LastSyncID != last || meta.MaxSyncID < last || meta.SchemaVersion < 1 {
				return nil, meta, fmt.Errorf("invalid sync metadata")
			}
			var extra any
			if err = dec.Decode(&extra); err != io.EOF {
				return nil, meta, fmt.Errorf("data after sync trailer")
			}
			return changes, meta, nil
		}
		if len(changes) >= limit || line.SyncID <= last || line.ModelID == "" || line.At.IsZero() ||
			(line.Model != "task" && line.Model != "document" && line.Model != "task_comment" && line.Model != "decision") ||
			(line.Action != "I" && line.Action != "U" && line.Action != "D") {
			return nil, meta, fmt.Errorf("invalid sync change")
		}
		changes = append(changes, line.SyncChange)
		last = line.SyncID
	}
}
