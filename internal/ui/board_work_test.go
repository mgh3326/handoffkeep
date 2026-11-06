package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBoardWorkRejectsInvalidProject(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.boardTasks(rec, httptest.NewRequest(http.MethodGet, "/ui/api/board/tasks?project=bad/name", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
}
