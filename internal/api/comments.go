package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// CreateTaskComment appends a comment authored by the authenticated client.
// It is data only: no task state, priority, event, or lane event changes.
func (s Service) CreateTaskComment(ctx context.Context, client string, taskID int64, body string) (store.TaskComment, error) {
	return s.Store.CreateTaskComment(ctx, taskID, client, body)
}
func (s Service) ListTaskComments(ctx context.Context, taskID, afterID int64, limit int) ([]store.TaskComment, error) {
	return s.Store.ListTaskComments(ctx, taskID, afterID, limit)
}

// taskCommentInput accepts only a body. Author-like fields are decoded so a
// forged author is refused with its own error rather than silently dropped.
type taskCommentInput struct {
	Body      string           `json:"body"`
	Author    *json.RawMessage `json:"author,omitempty"`
	CreatedBy *json.RawMessage `json:"created_by,omitempty"`
}

// taskCommentRequestMax leaves room for JSON escaping (up to six bytes per
// body byte) so an oversized body is reported as too long, not as malformed.
const taskCommentRequestMax = 6*store.TaskCommentMaxBytes + 4096

func taskCommentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrTaskCommentEmpty):
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "comment_empty"})
	case errors.Is(err, store.ErrTaskCommentTooLong):
		jsonOut(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "comment_too_long"})
	case errors.Is(err, store.ErrTaskNotFound):
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	default:
		appErr(w, err)
	}
}

func (s Server) taskCommentCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		appErr(w, errors.New("task id"))
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, taskCommentRequestMax)
	var input taskCommentInput
	de := json.NewDecoder(r.Body)
	de.DisallowUnknownFields()
	if err := de.Decode(&input); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			taskCommentErr(w, store.ErrTaskCommentTooLong)
			return
		}
		appErr(w, err)
		return
	}
	if input.Author != nil || input.CreatedBy != nil {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "author_not_accepted"})
		return
	}
	x, err := s.Service.CreateTaskComment(r.Context(), client, id, input.Body)
	if err != nil {
		taskCommentErr(w, err)
		return
	}
	jsonOut(w, http.StatusCreated, x)
}

func (s Server) taskCommentsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		appErr(w, errors.New("task id"))
		return
	}
	limit, err := queryLimit(r, 100, store.TaskCommentListMax)
	if err != nil {
		appErr(w, err)
		return
	}
	var afterID int64
	if r.URL.Query().Has("after_id") {
		if afterID, err = queryAfterID(r); err != nil {
			appErr(w, err)
			return
		}
	}
	xs, err := s.Service.ListTaskComments(r.Context(), id, afterID, limit)
	if err != nil {
		taskCommentErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"comments": xs})
}
