package api

import (
	"net/http"
	"strings"
)

// taskDocRefInput names one document a task cites.
type taskDocRefInput struct {
	Doc string `json:"doc"`
}

// taskDocRefsList lists the documents a task cites by reference (hk 6125). A
// binding is not a reference and is listed on the document routes instead.
func (s Server) taskDocRefsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	xs, err := s.Service.Store.ListTaskDocRefs(r.Context(), id)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"doc_refs": xs})
}

// taskDocRefAdd records a reference. 201 is a new reference, 200 one that
// already existed. A document key with no stored document is 400 doc_not_found.
func (s Server) taskDocRefAdd(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input taskDocRefInput
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	changed, err := s.Service.Store.AddTaskDocRef(r.Context(), id, input.Doc, client)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if changed {
		status = http.StatusCreated
	}
	jsonOut(w, status, map[string]any{"changed": changed, "task_id": id, "doc": input.Doc})
}

// taskDocRefRemove drops a reference named by the doc query parameter. Removing
// an absent reference changes nothing.
func (s Server) taskDocRefRemove(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	doc := r.URL.Query().Get("doc")
	if strings.TrimSpace(doc) == "" {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	changed, err := s.Service.Store.RemoveTaskDocRef(r.Context(), id, doc)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"changed": changed, "task_id": id, "doc": doc})
}
