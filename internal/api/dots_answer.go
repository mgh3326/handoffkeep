package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type dotsAnswerPayload struct {
	Task int64  `json:"task"`
	Text string `json:"text"`
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
	res, err := s.Service.Store.AnswerDotsQuestion(r.Context(), in.Task, client, store.DotsAnswerInput{Text: in.Text})
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, res)
}
