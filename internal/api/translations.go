package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (s Server) translationsPut(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	var input struct {
		Translations []store.Translation `json:"translations"`
	}
	if err := decode(r, &input, store.TranslationBatchMaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	if len(input.Translations) == 0 || len(input.Translations) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if err := s.Service.Store.PutTranslations(r.Context(), input.Translations); err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"count": len(input.Translations)})
}

func (s Server) translationsMissing(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	entity := r.URL.Query().Get("entity")
	if (entity != "task" && entity != "decision_request" && entity != "task_comment") || r.URL.Query().Get("state") != "open" {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	limit, err := queryLimit(r, 100, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var rows []store.MissingTranslation
	switch entity {
	case "task":
		rows, err = s.Service.Store.MissingTaskTranslations(r.Context(), limit)
	case "decision_request":
		rows, err = s.Service.Store.MissingDecisionTranslations(r.Context(), limit)
	case "task_comment":
		rows, err = s.Service.Store.MissingCommentTranslations(r.Context(), limit)
	}
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"translations": rows})
}
