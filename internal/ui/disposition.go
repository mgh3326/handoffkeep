package ui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// Disposition answers are the operator's alone (hk:doc
// decision/2026-09-21/task493-phase1-review C1). These routes live outside
// /ui/api/, where ServeHTTP admits service identities, and each handler also
// refuses any identity that is not a Cloudflare Access email before touching
// the store.
const dispositionSnapshotTTL = 12 * time.Hour

type dispositionItemView struct {
	Task        store.Task
	Gen         int64
	Question    string
	Recommended string
	Options     []store.DecisionOption
	AgeDays     int
	CSRF        string
	CanWrite    bool
}

type dispositionNoticeView struct {
	EventID string
	IDs     string
	CSRF    string
}

type dispositionSection struct {
	Head       string
	Detail     string
	Items      []dispositionItemView
	BatchN     int
	Counts     string
	NextBatch  int
	BatchID    string
	IssuedAt   string
	Snapshot   string
	Token      string
	CSRF       string
	CanWrite   bool
	Unnotified []dispositionNoticeView
}

func operatorEmail(identity cfaccess.Identity) (string, bool) {
	if identity.ServiceName != "" || identity.Email == "" {
		return "", false
	}
	return identity.Email, true
}

func (h *Handler) dispositionSnapshotMAC(email, batchID, issuedAt, snapshot string) string {
	mac := hmac.New(sha256.New, h.csrfKey)
	_, _ = mac.Write([]byte("disposition-batch|" + email + "|" + batchID + "|" + issuedAt + "|" + snapshot))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func encodeDispositionSnapshot(refs []store.DispositionRef) string {
	parts := make([]string, len(refs))
	for i, ref := range refs {
		parts[i] = strconv.FormatInt(ref.ID, 10) + ":" + strconv.FormatInt(ref.Gen, 10)
	}
	return strings.Join(parts, ",")
}

func parseDispositionSnapshot(raw string) ([]store.DispositionRef, error) {
	if raw == "" {
		return nil, errors.New("empty snapshot")
	}
	parts := strings.Split(raw, ",")
	if len(parts) > store.DispositionBatchLimit {
		return nil, errors.New("snapshot too large")
	}
	out := make([]store.DispositionRef, 0, len(parts))
	for _, part := range parts {
		idText, genText, ok := strings.Cut(part, ":")
		id, idErr := strconv.ParseInt(idText, 10, 64)
		gen, genErr := strconv.ParseInt(genText, 10, 64)
		if !ok || idErr != nil || genErr != nil || id < 1 || gen < 1 {
			return nil, errors.New("invalid snapshot")
		}
		out = append(out, store.DispositionRef{ID: id, Gen: gen})
	}
	return out, nil
}

func (h *Handler) dispositionData(r *http.Request, csrf, email string, canWrite bool) (*dispositionSection, error) {
	now := time.Now().UTC()
	summary, err := h.store.DispositionSummary(r.Context(), now)
	if err != nil {
		return nil, err
	}
	open, err := h.store.ListOpenDispositions(r.Context(), store.DispositionBatchLimit)
	if err != nil {
		return nil, err
	}
	section := &dispositionSection{Head: summary.Line, Detail: summary.Detail, NextBatch: summary.NextBatch, CSRF: csrf, CanWrite: canWrite}
	counts := map[string]int{}
	refs := make([]store.DispositionRef, 0, len(open))
	for _, item := range open {
		d := item.Task.Refs.Disposition
		// The recorded facts always head the card; a director re-ask note (hold
		// -> needs_decision) is shown after them, never instead of them.
		question := store.DispositionQuestion(item.Task.Refs, "")
		if item.Question != question && !strings.HasPrefix(item.Question, question+"\n") {
			question += "\n" + item.Question
		} else {
			question = item.Question
		}
		view := dispositionItemView{Task: item.Task, Gen: item.Gen, Question: question, Recommended: d.Recommended, AgeDays: int(now.Sub(item.Task.CreatedAt) / (24 * time.Hour)), CSRF: csrf, CanWrite: canWrite}
		if item.Task.Refs.DecisionOptions != nil {
			view.Options = item.Task.Refs.DecisionOptions.Options
		}
		section.Items = append(section.Items, view)
		counts[d.Recommended]++
		refs = append(refs, store.DispositionRef{ID: item.Task.ID, Gen: item.Gen})
	}
	section.BatchN = len(refs)
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", key, counts[key]))
	}
	section.Counts = strings.Join(parts, " · ")
	if len(refs) > 0 && email != "" {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
		section.BatchID = hex.EncodeToString(nonce)
		section.IssuedAt = strconv.FormatInt(now.Unix(), 10)
		section.Snapshot = encodeDispositionSnapshot(refs)
		section.Token = h.dispositionSnapshotMAC(email, section.BatchID, section.IssuedAt, section.Snapshot)
	}
	unnotified, err := h.store.ListUnnotifiedDispositions(r.Context(), 200)
	if err != nil {
		return nil, err
	}
	byEvent := map[string][]string{}
	order := []string{}
	for _, task := range unnotified {
		eventID := task.Refs.Disposition.Answer.EventID
		if _, seen := byEvent[eventID]; !seen {
			order = append(order, eventID)
		}
		byEvent[eventID] = append(byEvent[eventID], "#"+strconv.FormatInt(task.ID, 10))
	}
	for _, eventID := range order {
		section.Unnotified = append(section.Unnotified, dispositionNoticeView{EventID: eventID, IDs: strings.Join(byEvent[eventID], " "), CSRF: csrf})
	}
	return section, nil
}

// dispositionEventText is deterministic in the recorded answers, so a
// re-notification sends the same text under the same event ID.
func dispositionEventText(tasks []store.Task) (string, error) {
	if len(tasks) == 0 {
		return "", errors.New("no answered items")
	}
	first := tasks[0].Refs.Disposition.Answer
	email := strings.TrimPrefix(first.By, "operator:")
	if len(tasks) == 1 && first.BatchID == "" {
		label, _ := store.DispositionOptionLabel(first.Key)
		return fmt.Sprintf("[decision] #%d: %s: %s (from operator(web) %s)", tasks[0].ID, first.Key, label, email), nil
	}
	parts := make([]string, 0, len(tasks))
	for _, task := range tasks {
		parts = append(parts, fmt.Sprintf("#%d=%s", task.ID, task.Refs.Disposition.Answer.Key))
	}
	return fmt.Sprintf("[decision] disposition-batch %s: %s (from operator(web) %s)", first.BatchID, strings.Join(parts, " "), email), nil
}

func (h *Handler) dispositionWriteGate(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity, action string) (string, bool) {
	email, ok := operatorEmail(identity)
	if !ok {
		h.audit(identity.ServiceName, action, "-", "-", "service_reject")
		http.Error(w, "operator identity required", http.StatusForbidden)
		return "", false
	}
	if !sameOrigin(r) {
		h.audit(email, action, "-", "-", "origin_reject")
		http.Error(w, "origin rejected", http.StatusForbidden)
		return "", false
	}
	if err := r.ParseForm(); err != nil {
		h.audit(email, action, "-", "-", "invalid")
		http.Error(w, "invalid form", http.StatusBadRequest)
		return "", false
	}
	if !h.validCSRF(r, email) {
		h.audit(email, action, "-", "-", "csrf_reject")
		http.Error(w, "CSRF rejected", http.StatusForbidden)
		return "", false
	}
	if !h.hub.configured() {
		h.audit(email, action, "-", "-", "hub_unconfigured")
		http.Error(w, "Hub is not configured.", http.StatusBadRequest)
		return "", false
	}
	return email, true
}

func (h *Handler) answerDisposition(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	email, ok := h.dispositionWriteGate(w, r, identity, "disposition")
	if !ok {
		return
	}
	id, idErr := strconv.ParseInt(strings.TrimSpace(r.Form.Get("id")), 10, 64)
	gen, genErr := strconv.ParseInt(strings.TrimSpace(r.Form.Get("gen")), 10, 64)
	key := strings.TrimSpace(r.Form.Get("key"))
	if _, valid := store.DispositionOptionLabel(key); idErr != nil || genErr != nil || id < 1 || gen < 1 || !valid {
		h.audit(email, "disposition", "-", "-", "invalid")
		http.Error(w, "Invalid disposition answer.", http.StatusBadRequest)
		return
	}
	eventID := fmt.Sprintf("web-disposition-%d-g%d", id, gen)
	target := "#" + strconv.FormatInt(id, 10)
	// Record first (the authoritative answer), then notify. A failed emit
	// leaves a recorded answer awaiting re-notification, never an unrecorded
	// answer in the director's lane (ESC-5).
	task, err := h.store.AnswerDisposition(r.Context(), store.DispositionAnswerInput{ID: id, Gen: gen, Key: key, OperatorEmail: email, EventID: eventID})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDispositionStale):
			h.audit(email, "disposition", target, eventID, "stale")
			http.Error(w, "질문이 바뀌었습니다. 새로 고친 뒤 다시 처분하세요.", http.StatusConflict)
		case errors.Is(err, store.ErrTaskConflict), errors.Is(err, store.ErrTaskNotFound):
			h.audit(email, "disposition", target, eventID, "conflict")
			http.Error(w, "이미 처분됐거나 처분 항목이 아닙니다.", http.StatusConflict)
		default:
			h.audit(email, "disposition", target, eventID, "error")
			http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		}
		return
	}
	text, err := dispositionEventText([]store.Task{task})
	if err == nil {
		_, err = h.hub.emitLaneEvent(r.Context(), task.Lane, eventID, text)
	}
	if err != nil {
		h.audit(email, "disposition", target, eventID, "recorded_notify_failed")
		h.decisionResult(w, r, email, "처분 기록됨 · 레인 통지 실패: "+hubReason(err)+" — 통지 대기에서 재통지하세요(event_id="+eventID+")", http.StatusOK)
		return
	}
	h.audit(email, "disposition", target, eventID, "ok")
	h.decisionResult(w, r, email, "처분 기록·전송됨(event_id="+eventID+")", http.StatusOK)
}

func (h *Handler) acceptDispositionBatch(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	email, ok := h.dispositionWriteGate(w, r, identity, "disposition-batch")
	if !ok {
		return
	}
	batchID, issuedAt, snapshot, token := r.Form.Get("batch_id"), r.Form.Get("issued_at"), r.Form.Get("snapshot"), r.Form.Get("token")
	issued, err := strconv.ParseInt(issuedAt, 10, 64)
	if err != nil || len(batchID) != 32 || token == "" || !hmac.Equal([]byte(token), []byte(h.dispositionSnapshotMAC(email, batchID, issuedAt, snapshot))) {
		h.audit(email, "disposition-batch", "-", "-", "snapshot_reject")
		http.Error(w, "snapshot rejected", http.StatusForbidden)
		return
	}
	if age := time.Since(time.Unix(issued, 0)); age < -time.Minute || age > dispositionSnapshotTTL {
		h.audit(email, "disposition-batch", "-", "-", "snapshot_expired")
		http.Error(w, "snapshot expired — reload", http.StatusForbidden)
		return
	}
	refs, err := parseDispositionSnapshot(snapshot)
	if err != nil {
		h.audit(email, "disposition-batch", "-", "-", "invalid")
		http.Error(w, "invalid snapshot", http.StatusBadRequest)
		return
	}
	eventFor := func(lane string) string { return "web-disposition-batch-" + batchID + "-" + lane }
	answered, skipped, err := h.store.AnswerDispositionBatch(r.Context(), refs, email, batchID, eventFor)
	if err != nil {
		h.audit(email, "disposition-batch", "-", "-", "error")
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	byLane := map[string][]store.Task{}
	lanes := []string{}
	for _, task := range answered {
		if _, seen := byLane[task.Lane]; !seen {
			lanes = append(lanes, task.Lane)
		}
		byLane[task.Lane] = append(byLane[task.Lane], task)
	}
	sort.Strings(lanes)
	failed := []string{}
	for _, lane := range lanes {
		text, err := dispositionEventText(byLane[lane])
		if err == nil {
			_, err = h.hub.emitLaneEvent(r.Context(), lane, eventFor(lane), text)
		}
		result := "ok"
		if err != nil {
			result = "recorded_notify_failed"
			failed = append(failed, lane+"("+hubReason(err)+")")
		}
		h.audit(email, "disposition-batch", lane, eventFor(lane), result)
	}
	notice := fmt.Sprintf("권고 일괄 수락 %d건 기록(batch %s)", len(answered), batchID[:8])
	if len(skipped) > 0 {
		notice += fmt.Sprintf(" · 건너뜀 %d건(이미 처분됐거나 질문이 바뀜)", len(skipped))
	}
	if len(failed) > 0 {
		notice += " · 레인 통지 실패: " + strings.Join(failed, ", ") + " — 통지 대기에서 재통지하세요"
	}
	h.decisionResult(w, r, email, notice, http.StatusOK)
}

func (h *Handler) renotifyDisposition(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	email, ok := h.dispositionWriteGate(w, r, identity, "disposition-renotify")
	if !ok {
		return
	}
	eventID := strings.TrimSpace(r.Form.Get("event_id"))
	if !strings.HasPrefix(eventID, "web-disposition-") {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	pending, err := h.store.ListUnnotifiedDispositions(r.Context(), 1000)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	stillPending := false
	for _, task := range pending {
		if task.Refs.Disposition.Answer.EventID == eventID {
			stillPending = true
			break
		}
	}
	if !stillPending {
		h.audit(email, "disposition-renotify", "-", eventID, "already_notified")
		h.decisionResult(w, r, email, "이미 통지됨", http.StatusOK)
		return
	}
	tasks, err := h.store.ListDispositionsByAnswerEvent(r.Context(), eventID)
	if err != nil || len(tasks) == 0 {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	text, err := dispositionEventText(tasks)
	if err == nil {
		_, err = h.hub.emitLaneEvent(r.Context(), tasks[0].Lane, eventID, text)
	}
	if err != nil {
		h.audit(email, "disposition-renotify", tasks[0].Lane, eventID, "hub_error")
		h.decisionResult(w, r, email, "재통지 실패: "+hubReason(err), http.StatusOK)
		return
	}
	h.audit(email, "disposition-renotify", tasks[0].Lane, eventID, "ok")
	h.decisionResult(w, r, email, "재통지됨(event_id="+eventID+")", http.StatusOK)
}
