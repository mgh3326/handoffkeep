package main

import (
	"errors"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ytCommentsModeFromEnv reads HANDOFFKEEP_YT_COMMENTS_MODE (slice b1a).
func ytCommentsModeFromEnv() (store.YTCommentsMode, error) {
	return store.YTCommentsOff, nil
}

// ytCommentsWorkerFromEnv sets the store's gate and returns the dispatcher
// for serve's worker list, or nil when the mode is off.
func ytCommentsWorkerFromEnv(st *store.Store) (backgroundWorker, error) {
	return nil, errors.New("not implemented")
}
