package api

import (
	"context"
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// RecordLaneLook records that a lane looked at one lane_check notice
// (operator decision 418, hk 5201).
func (s Service) RecordLaneLook(ctx context.Context, lane, notice, note, by string) (store.LaneLook, bool, error) {
	return s.Store.RecordLaneLook(ctx, lane, notice, note, by, time.Now().UTC())
}

// laneLook answers POST /v1/lanes/{lane}/looks with the recorded look and
// changed: whether this call wrote it. The note is optional. A repeat of the
// same notice changes nothing.
func (s Server) laneLook(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	var in struct {
		Notice string `json:"notice"`
		Note   string `json:"note,omitempty"`
	}
	defer r.Body.Close()
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	look, changed, err := s.Service.RecordLaneLook(r.Context(), r.PathValue("lane"), in.Notice, in.Note, client)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"changed": changed, "look": look})
}
