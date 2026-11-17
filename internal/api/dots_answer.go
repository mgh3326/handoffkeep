package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type dotsAnswerPayload struct {
	Task          int64  `json:"task"`
	Text          string `json:"text"`
	RequesterLane string `json:"requester_lane,omitempty"`
	QuestionID    int64  `json:"question_id,omitempty"`
}

func (s Server) dotsAnswer(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var in dotsAnswerPayload
	if err := decode(r, &in, bodyLarge); err != nil {
		appErr(w, r, err)
		return
	}
	if in.Task < 1 || in.Text == "" {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	res, err := s.Service.Store.AnswerDotsQuestion(r.Context(), in.Task, client, store.DotsAnswerInput{Text: in.Text, RequesterLane: in.RequesterLane, QuestionID: in.QuestionID})
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, res)
}
