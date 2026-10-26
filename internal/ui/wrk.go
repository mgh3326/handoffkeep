package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

// Access-authenticated UI identities do not become O3 central principals.
// Precheck before publishing a decision notification; Store repeats the same
// check under its mutation lock.
func (h *Handler) wrkTaskCheck(ctx context.Context, task store.Task, to string) error {
	return h.store.WrkFence(ctx, wrkrecords.LegacyIntent{TaskID: task.ID, CurrentJob: task.Refs.JobID, To: to})
}
func uiWrkError(w http.ResponseWriter, err error) bool {
	var denied wrkrecords.Error
	if !errors.As(err, &denied) {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(wrkrecords.Status(denied))
	_ = json.NewEncoder(w).Encode(map[string]string{"error": wrkrecords.Code(denied)})
	return true
}
