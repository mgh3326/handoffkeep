package remote

import (
	"context"
	"errors"
	"net/http"
)

func (c Client) RefreshLinearMetadata(ctx context.Context) error {
	var result struct {
		Status string `json:"status"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/linear/metadata/refresh", struct{}{}, &result); err != nil {
		return err
	}
	if result.Status != "refreshed" {
		return errors.New("unexpected linear metadata refresh response")
	}
	return nil
}

func (c Client) ReconcileLinearBoard(ctx context.Context, taskID int64) error {
	var result struct {
		Status string `json:"status"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/linear/board/reconcile", map[string]int64{"task_id": taskID}, &result); err != nil {
		return err
	}
	if result.Status != "awaiting_webhook" {
		return errors.New("unexpected linear board reconcile response")
	}
	return nil
}
