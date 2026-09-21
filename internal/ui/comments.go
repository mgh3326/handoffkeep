package ui

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Task comments in the console. A comment is data: the write route calls
// store.CreateTaskComment and nothing else — no task transition, priority,
// task_events row, hub lane event, or relay record — and the body is never
// interpreted (a "[decision] #12: yes" comment is just text).

// boardCommentsPage bounds one page of the comment BFF. One extra row is read
// to tell "this is all of them" from "the page bound was hit".
const boardCommentsPage = 200

// uiCommentFormMax bounds the urlencoded write form: a body at the store
// limit can triple under percent-encoding, plus room for the CSRF field. A
// larger request is answered as comment_too_long, never as a generic error.
const uiCommentFormMax = 3*store.TaskCommentMaxBytes + 4096

var taskCommentsPathRE = regexp.MustCompile(`^/ui/tasks/([1-9][0-9]{0,14})/comments$`)
var boardCommentsPathRE = regexp.MustCompile(`^/ui/api/board/tasks/([1-9][0-9]{0,14})/comments$`)

func pathTaskID(re *regexp.Regexp, path string) (int64, bool) {
	match := re.FindStringSubmatch(path)
	if match == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(match[1], 10, 64)
	return id, err == nil && id > 0
}

type boardComment struct {
	ID        int64     `json:"id"`
	TaskID    int64     `json:"task_id"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

type boardCommentsResponse struct {
	Comments    []boardComment `json:"comments"`
	Truncated   bool           `json:"truncated"`
	NextAfterID int64          `json:"next_after_id,omitempty"`
}

func projectBoardComment(x store.TaskComment) boardComment {
	return boardComment{ID: x.ID, TaskID: x.TaskID, Body: strings.ToValidUTF8(x.Body, "�"), Author: x.Author, CreatedAt: x.CreatedAt.UTC()}
}

// boardTaskComments lists a task's comments in id (= creation) order. A
// missing task is a 404, never an empty list.
func (h *Handler) boardTaskComments(w http.ResponseWriter, r *http.Request, taskID int64) {
	var afterID int64
	if raw := r.URL.Query().Get("after_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			http.Error(w, "invalid after_id", http.StatusBadRequest)
			return
		}
		afterID = parsed
	}
	xs, err := h.store.ListTaskComments(r.Context(), taskID, afterID, boardCommentsPage+1)
	if errors.Is(err, store.ErrTaskNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	response := boardCommentsResponse{Comments: make([]boardComment, 0, min(len(xs), boardCommentsPage))}
	if len(xs) > boardCommentsPage {
		xs = xs[:boardCommentsPage]
		response.Truncated = true
		response.NextAfterID = xs[len(xs)-1].ID
	}
	for _, x := range xs {
		response.Comments = append(response.Comments, projectBoardComment(x))
	}
	writeBoardJSON(w, response)
}

func commentError(w http.ResponseWriter, status int, code string) {
	writeBoardJSONStatus(w, status, map[string]string{"error": code})
}

// createTaskComment is the console's comment write. It takes the same
// authentication, origin, and CSRF path as /ui/decisions/answer: ServeHTTP
// has already required a verified operator email (service principals are
// refused on non-/ui/api routes), then sameOrigin, ParseForm, validCSRF, and
// one audit line. The author is the verified email; the form carries only
// csrf and body, and any other field — author-like or not — is refused.
func (h *Handler) createTaskComment(w http.ResponseWriter, r *http.Request, email string, taskID int64) {
	outcome := writeOutcome{action: "comment", target: "task#" + strconv.FormatInt(taskID, 10), eventID: "-", result: "invalid"}
	defer func() { h.audit(email, outcome.action, outcome.target, outcome.eventID, outcome.result) }()
	if !sameOrigin(r) {
		outcome.result = "origin_reject"
		commentError(w, http.StatusForbidden, "origin_rejected")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uiCommentFormMax)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			outcome.result = "too_long"
			commentError(w, http.StatusRequestEntityTooLarge, "comment_too_long")
			return
		}
		commentError(w, http.StatusBadRequest, "invalid_form")
		return
	}
	if !h.validCSRF(r, email) {
		outcome.result = "csrf_reject"
		commentError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	if r.URL.RawQuery != "" {
		commentError(w, http.StatusBadRequest, "unknown_field")
		return
	}
	for key, values := range r.PostForm {
		switch key {
		case "csrf", "body":
			if len(values) != 1 {
				commentError(w, http.StatusBadRequest, "invalid_form")
				return
			}
		case "author", "created_by":
			outcome.result = "author_reject"
			commentError(w, http.StatusBadRequest, "author_not_accepted")
			return
		default:
			commentError(w, http.StatusBadRequest, "unknown_field")
			return
		}
	}
	comment, err := h.store.CreateTaskComment(r.Context(), taskID, "operator:"+email, r.PostForm.Get("body"))
	switch {
	case err == nil:
	case errors.Is(err, store.ErrTaskCommentEmpty):
		commentError(w, http.StatusBadRequest, "comment_empty")
		return
	case errors.Is(err, store.ErrTaskCommentTooLong):
		outcome.result = "too_long"
		commentError(w, http.StatusRequestEntityTooLarge, "comment_too_long")
		return
	case errors.Is(err, store.ErrTaskNotFound):
		outcome.result = "not_found"
		commentError(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, store.ErrTaskCommentInvalid):
		commentError(w, http.StatusBadRequest, "comment_invalid")
		return
	case strings.HasPrefix(err.Error(), "secret_like_content:"):
		outcome.result = "secret_reject"
		commentError(w, http.StatusBadRequest, "comment_secret_like")
		return
	default:
		outcome.result = "store_error"
		commentError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	outcome.result = "ok"
	outcome.eventID = "comment-" + strconv.FormatInt(comment.ID, 10)
	writeBoardJSONStatus(w, http.StatusCreated, projectBoardComment(comment))
}
