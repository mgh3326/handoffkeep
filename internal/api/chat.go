package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (s Service) UpsertChatQuestion(ctx context.Context, x store.ChatQuestion) (store.ChatQuestion, bool, error) {
	return s.Store.UpsertChatQuestion(ctx, x)
}
func (s Service) TransitionChatQuestion(ctx context.Context, id, to string) (store.ChatQuestion, error) {
	return s.Store.TransitionChatQuestion(ctx, id, to)
}
func (s Service) ListChatQuestions(ctx context.Context, lane, state, afterID string, limit int) ([]store.ChatQuestion, error) {
	return s.Store.ListChatQuestions(ctx, lane, state, afterID, limit)
}
func (s Service) CreateChatMessage(ctx context.Context, x store.ChatMessage) (store.ChatMessage, error) {
	return s.Store.CreateChatMessage(ctx, x)
}
func (s Service) MarkChatMessageDelivered(ctx context.Context, id int64) (store.ChatMessage, error) {
	return s.Store.MarkChatMessageDelivered(ctx, id)
}
func (s Service) MarkChatMessageFailed(ctx context.Context, id int64) (store.ChatMessage, error) {
	return s.Store.MarkChatMessageFailed(ctx, id)
}
func (s Service) ListChatMessages(ctx context.Context, author string, undelivered bool, afterID int64, limit int) ([]store.ChatMessage, error) {
	return s.Store.ListChatMessages(ctx, author, undelivered, afterID, limit)
}

type chatQuestionInput struct {
	ID   string `json:"id"`
	Lane string `json:"lane"`
	Body string `json:"body"`
}

func (s Server) chatQuestionUpsert(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input chatQuestionInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	if input.ID != "" {
		if id != "" && input.ID != id {
			jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_context"})
			return
		}
		id = input.ID
	}
	x, created, err := s.Service.UpsertChatQuestion(r.Context(), store.ChatQuestion{ID: id, Lane: input.Lane, Body: input.Body})
	if err != nil {
		appErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, x)
}

func (s Server) chatQuestionPut(w http.ResponseWriter, r *http.Request) {
	s.chatQuestionUpsert(w, r, r.PathValue("id"))
}

func (s Server) chatQuestionPost(w http.ResponseWriter, r *http.Request) {
	s.chatQuestionUpsert(w, r, "")
}

type chatQuestionTransitionInput struct {
	To string `json:"to"`
}

func (s Server) chatQuestionTransition(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input chatQuestionTransitionInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.TransitionChatQuestion(r.Context(), r.PathValue("id"), input.To)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func (s Server) chatQuestionsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 20, 1000)
	if err != nil {
		appErr(w, err)
		return
	}
	xs, err := s.Service.ListChatQuestions(r.Context(), r.URL.Query().Get("lane"), r.URL.Query().Get("state"), r.URL.Query().Get("after_id"), limit)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"questions": xs})
}

type chatMessageInput struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

func (s Server) chatMessageCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input chatMessageInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	if input.Author == "" {
		input.Author = "operator"
	}
	x, err := s.Service.CreateChatMessage(r.Context(), store.ChatMessage{Author: input.Author, Body: input.Body})
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusCreated, x)
}

func (s Server) chatMessagesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 200, 1000)
	if err != nil {
		appErr(w, err)
		return
	}
	afterID, err := queryAfterID(r)
	if err != nil {
		appErr(w, err)
		return
	}
	undeliveredValue := r.URL.Query().Get("undelivered")
	undelivered := undeliveredValue == "1" || undeliveredValue == "true"
	xs, err := s.Service.ListChatMessages(r.Context(), r.URL.Query().Get("author"), undelivered, afterID, limit)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"messages": xs})
}

func chatMessageID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (s Server) chatMessageDelivered(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := chatMessageID(r)
	if err != nil || id < 1 {
		appErr(w, errors.New("chat message id"))
		return
	}
	x, err := s.Service.MarkChatMessageDelivered(r.Context(), id)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func (s Server) chatMessageFailed(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := chatMessageID(r)
	if err != nil || id < 1 {
		appErr(w, errors.New("chat message id"))
		return
	}
	x, err := s.Service.MarkChatMessageFailed(r.Context(), id)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}
