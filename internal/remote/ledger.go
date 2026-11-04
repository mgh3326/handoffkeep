package remote

import (
	"context"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Ledger reads the desk ledger for consumer. It never moves the cursor. A
// cursor past the change feed's reset boundary returns *store.SyncReset.
func (c Client) Ledger(ctx context.Context, consumer string) (store.Ledger, error) {
	var out store.Ledger
	err := c.call(ctx, "GET", "/v1/ledger?consumer="+url.QueryEscape(consumer), nil, &out)
	return out, err
}

// AckLedger moves consumer's cursor forward to syncID. The server refuses a
// lower syncID with ledger_ack_backward; an unknown consumer's first ack
// starts its cursor at the feed's current watermark.
func (c Client) AckLedger(ctx context.Context, consumer string, syncID int64) (store.LedgerCursor, error) {
	var out store.LedgerCursor
	err := c.call(ctx, "POST", "/v1/ledger/ack", map[string]any{"consumer": consumer, "sync_id": syncID}, &out)
	return out, err
}

// CreateStatusUpdate posts a project's health.
func (c Client) CreateStatusUpdate(ctx context.Context, project, health, body string) (store.StatusUpdate, error) {
	var out store.StatusUpdate
	err := c.call(ctx, "POST", "/v1/projects/"+esc(project)+"/status-updates", map[string]string{"health": health, "body": body}, &out)
	return out, err
}

// ListStatusUpdates lists a project's updates, newest first. limit 0 leaves
// the server default.
func (c Client) ListStatusUpdates(ctx context.Context, project string, limit int) (store.StatusUpdateList, error) {
	path := "/v1/projects/" + esc(project) + "/status-updates"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var out store.StatusUpdateList
	err := c.call(ctx, "GET", path, nil, &out)
	return out, err
}
