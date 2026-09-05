package ui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const csrfCookie = "hk_ui_csrf"

var docKeyRE = regexp.MustCompile(`^[A-Za-z0-9._\-/]{1,512}$`)

type writeOutcome struct {
	action  string
	target  string
	eventID string
	result  string
}

type composeData struct {
	Lanes       []string
	CSRF        string
	CanCompose  bool
	WriteReason string
	Notice      string
	Email       string
}

// csrfForForm renews the session-local form token only while rendering a form.
func (h *Handler) csrfForForm(w http.ResponseWriter, r *http.Request, email string) string {
	if cookie, err := r.Cookie(csrfCookie); err == nil && h.validCSRFToken(cookie.Value, email) {
		return cookie.Value
	}
	token, err := h.newCSRFToken(email)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/ui",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   true,
		Expires:  time.Now().Add(12 * time.Hour),
		MaxAge:   12 * 60 * 60,
	})
	return token
}

func (h *Handler) newCSRFToken(email string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	expires := strconv.FormatInt(time.Now().Add(12*time.Hour).Unix(), 10)
	nonceHex := hex.EncodeToString(nonce)
	mac := h.csrfMAC(expires, nonceHex, email)
	return expires + "." + nonceHex + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

func (h *Handler) validCSRF(r *http.Request, email string) bool {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" || r.FormValue("csrf") == "" {
		return false
	}
	return hmac.Equal([]byte(cookie.Value), []byte(r.FormValue("csrf"))) && h.validCSRFToken(cookie.Value, email)
}

func (h *Handler) validCSRFToken(token, email string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) != 32 {
		return false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() >= expires {
		return false
	}
	given, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	return hmac.Equal(given, h.csrfMAC(parts[0], parts[1], email))
}

func (h *Handler) csrfMAC(expiry, nonce, email string) []byte {
	mac := hmac.New(sha256.New, h.csrfKey)
	_, _ = mac.Write([]byte(expiry + "|" + nonce + "|" + email))
	return mac.Sum(nil)
}

func sameOrigin(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		raw = strings.TrimSpace(r.Referer())
	}
	if raw == "" {
		return false
	}
	origin, err := url.Parse(raw)
	if err != nil || origin.Host == "" {
		return false
	}
	return strings.EqualFold(origin.Host, r.Host)
}

func writeAction(path string) string {
	switch path {
	case "/ui/decisions/answer":
		return "decision"
	case "/ui/compose":
		return "compose"
	default:
		return "invalid"
	}
}

func (h *Handler) audit(email, action, target, eventID, result string) {
	log.Printf("ui.write email=%s action=%s target=%s event_id=%s result=%s", auditField(email), auditField(action), auditField(target), auditField(eventID), auditField(result))
}

func auditField(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
	if value == "" {
		return "-"
	}
	return value
}

func (h *Handler) answerDecision(w http.ResponseWriter, r *http.Request, email string) {
	outcome := writeOutcome{action: "decision", target: "-", eventID: "-", result: "invalid"}
	defer func() { h.audit(email, outcome.action, outcome.target, outcome.eventID, outcome.result) }()
	if !sameOrigin(r) {
		outcome.result = "origin_reject"
		http.Error(w, "origin rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !h.validCSRF(r, email) {
		outcome.result = "csrf_reject"
		http.Error(w, "CSRF rejected", http.StatusForbidden)
		return
	}
	if !h.hub.configured() {
		h.decisionResult(w, r, email, "Hub is not configured.", http.StatusBadRequest)
		return
	}

	kind := strings.TrimSpace(r.Form.Get("type"))
	id, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("id")), 10, 64)
	answer := strings.TrimSpace(r.Form.Get("answer"))
	note := strings.TrimSpace(r.Form.Get("note"))
	if err != nil || id < 1 || (kind != "task" && kind != "escalation" && kind != "lane") || !validWriteText(answer) || (note != "" && !validWriteText(note)) {
		h.decisionResult(w, r, email, "Invalid decision response.", http.StatusBadRequest)
		return
	}

	var lane string
	if kind == "task" {
		task, found, taskErr := h.store.GetTask(r.Context(), id)
		if taskErr != nil {
			h.decisionResult(w, r, email, "Fleet console unavailable.", http.StatusInternalServerError)
			return
		}
		if !found {
			h.decisionResult(w, r, email, "Task not found.", http.StatusBadRequest)
			return
		}
		if task.State != "needs_decision" {
			h.decisionResult(w, r, email, "Task has already been answered.", http.StatusConflict)
			return
		}
		lane = task.Lane
	} else {
		event, found, eventErr := h.openDecisionEvent(r, kind, id)
		if eventErr != nil {
			h.decisionResult(w, r, email, "Fleet console unavailable.", http.StatusInternalServerError)
			return
		}
		if !found {
			h.decisionResult(w, r, email, "Decision not found.", http.StatusBadRequest)
			return
		}
		lane = event.OwnerLane
	}
	outcome.target = lane + "#" + strconv.FormatInt(id, 10)
	eventID, err := randomEventID("web-decision-" + kind + "-" + strconv.FormatInt(id, 10) + "-")
	if err != nil {
		h.decisionResult(w, r, email, "Unable to create event ID.", http.StatusInternalServerError)
		return
	}
	outcome.eventID = eventID
	prefix := "[decision]"
	if kind == "lane" {
		prefix = "[decision-answered]"
	}
	text := fmt.Sprintf("%s #%d: %s (from operator(web) %s)", prefix, id, answer, email)
	if note != "" {
		text += " — " + note
	}
	if len([]byte(text)) > store.RelayLaneEventMaxBytes {
		h.decisionResult(w, r, email, "Decision response is too long.", http.StatusBadRequest)
		return
	}
	duplicate, emitErr := h.hub.emitLaneEvent(r.Context(), lane, eventID, text)
	if emitErr != nil {
		outcome.result = "hub_error"
		h.decisionResult(w, r, email, "레인 전송 실패: "+hubReason(emitErr), http.StatusOK)
		return
	}
	if kind != "task" {
		if duplicate {
			outcome.result = "duplicate"
			h.decisionResult(w, r, email, "이미 전송됨", http.StatusOK)
			return
		}
		outcome.result = "ok"
		h.decisionResult(w, r, email, "전송됨(event_id="+eventID+")", http.StatusOK)
		return
	}
	transitionNote := answer
	if note != "" {
		transitionNote += " — " + note
	}
	transitionNote += " — via web by " + email
	if _, err = h.store.TransitionTask(r.Context(), id, "claimed", "operator:"+email, transitionNote, nil); err != nil {
		if errors.Is(err, store.ErrTaskConflict) {
			outcome.result = "task_conflict"
			h.decisionResult(w, r, email, "이미 답변됨", http.StatusOK)
			return
		}
		outcome.result = "task_error"
		h.decisionResult(w, r, email, "레인 전송됨(event_id="+eventID+") · 수동 전이 필요: "+safeTaskReason(err), http.StatusOK)
		return
	}
	if duplicate {
		outcome.result = "duplicate"
		h.decisionResult(w, r, email, "이미 전송됨", http.StatusOK)
		return
	}
	outcome.result = "ok"
	h.decisionResult(w, r, email, "전송됨(event_id="+eventID+")", http.StatusOK)
}

func (h *Handler) openDecisionEvent(r *http.Request, kind string, id int64) (store.RelayEvent, bool, error) {
	var events []store.RelayEvent
	var err error
	if kind == "escalation" {
		events, err = h.store.ListOpenEscalations(r.Context(), 1000)
	} else {
		events, err = h.store.ListOpenLaneDecisions(r.Context(), 1000)
	}
	if err != nil {
		return store.RelayEvent{}, false, err
	}
	for _, event := range events {
		if event.ID == id {
			return event, true, nil
		}
	}
	return store.RelayEvent{}, false, nil
}

func (h *Handler) compose(w http.ResponseWriter, r *http.Request, fragment bool, email, notice string) {
	data := composeData{Lanes: h.lanes, CSRF: h.csrfForForm(w, r, email), Notice: notice, Email: email}
	data.CanCompose = h.hub.configured() && len(data.Lanes) > 0
	switch {
	case !h.hub.configured() && len(data.Lanes) == 0:
		data.WriteReason = "Hub is not configured and no destination lanes are configured."
	case !h.hub.configured():
		data.WriteReason = "Hub is not configured."
	case len(data.Lanes) == 0:
		data.WriteReason = "No destination lanes are configured."
	}
	if fragment {
		h.render(w, "compose_content", data)
		return
	}
	h.render(w, "page", pageData{Title: "Compose", Page: "compose", Body: data})
}

func (h *Handler) composePost(w http.ResponseWriter, r *http.Request, email string) {
	outcome := writeOutcome{action: "compose", target: "-", eventID: "-", result: "invalid"}
	defer func() { h.audit(email, outcome.action, outcome.target, outcome.eventID, outcome.result) }()
	if !sameOrigin(r) {
		outcome.result = "origin_reject"
		http.Error(w, "origin rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !h.validCSRF(r, email) {
		outcome.result = "csrf_reject"
		http.Error(w, "CSRF rejected", http.StatusForbidden)
		return
	}
	if !h.hub.configured() || len(h.lanes) == 0 {
		h.composeResult(w, r, email, "Compose is not configured.", http.StatusBadRequest)
		return
	}
	lane := strings.TrimSpace(r.Form.Get("lane"))
	textInput := r.Form.Get("text")
	if !h.laneSet[lane] || !validWriteText(textInput) {
		h.composeResult(w, r, email, "Invalid destination lane or message.", http.StatusBadRequest)
		return
	}
	outcome.target = lane + "#-"
	text := "[event] " + textInput + " (from operator(web) " + email + ")"
	if len([]byte(text)) > store.RelayLaneEventMaxBytes {
		h.composeResult(w, r, email, "Message is too long (maximum final size is 2048 bytes).", http.StatusBadRequest)
		return
	}
	eventID, err := randomEventID("web-msg-")
	if err != nil {
		h.composeResult(w, r, email, "Unable to create event ID.", http.StatusInternalServerError)
		return
	}
	outcome.eventID = eventID
	duplicate, err := h.hub.emitLaneEvent(r.Context(), lane, eventID, text)
	if err != nil {
		outcome.result = "hub_error"
		h.composeResult(w, r, email, "레인 전송 실패: "+hubReason(err), http.StatusOK)
		return
	}
	if duplicate {
		outcome.result = "duplicate"
		h.composeResult(w, r, email, "이미 전송됨. Delivered status is shown by Timeline's ✓ marker.", http.StatusOK)
		return
	}
	outcome.result = "ok"
	h.composeResult(w, r, email, "전송됨(event_id="+eventID+"). Delivered status is shown by Timeline's ✓ marker.", http.StatusOK)
}

func (h *Handler) decisionResult(w http.ResponseWriter, r *http.Request, email, notice string, status int) {
	if status >= http.StatusBadRequest {
		http.Error(w, notice, status)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		h.decisions(w, r, true, email, notice)
		return
	}
	http.Redirect(w, r, "/ui/decisions?result="+url.QueryEscape(notice), http.StatusSeeOther)
}

func (h *Handler) composeResult(w http.ResponseWriter, r *http.Request, email, notice string, status int) {
	if status >= http.StatusBadRequest {
		http.Error(w, notice, status)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		h.compose(w, r, true, email, notice)
		return
	}
	http.Redirect(w, r, "/ui/compose?result="+url.QueryEscape(notice), http.StatusSeeOther)
}

func validWriteText(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if r == 0 || r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

func randomEventID(prefix string) (string, error) {
	value := make([]byte, 4)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}

func hubReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "hub request timed out"
	}
	var status hubStatusError
	if errors.As(err, &status) {
		return status.Error()
	}
	return "hub request failed"
}

func safeTaskReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	return "task transition failed"
}

func decisionOptions(question string) []string {
	for _, line := range strings.Split(question, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "options:") {
			continue
		}
		var options []string
		for _, option := range strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "options:")), "|") {
			option = strings.TrimSpace(option)
			if option != "" {
				options = append(options, option)
				if len(options) == 8 {
					return options
				}
			}
		}
		return options
	}
	return nil
}

func escalationText(event store.RelayEvent) string {
	if event.Question != "" {
		return event.Question
	}
	if event.Text != "" {
		return event.Text
	}
	return event.ReportLastLine
}

// isSignalEscalation is deliberately pure so the signal vocabulary has a
// table-driven test independent of database or HTTP setup.
func isSignalEscalation(event store.RelayEvent) bool {
	text := escalationText(event)
	if strings.Contains(strings.ToLower(text), "(ignore)") {
		return true
	}
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	upper := strings.ToUpper(trimmed)
	for _, signal := range []string{"PING", "LANE-OK", "LANE-FAIL", "READY", "BOUNCE", "MERGED-CLEANUP", "FLEET-OK", "FLEET-FAIL", "LOST-RELAY", "REPORT"} {
		if !strings.HasPrefix(upper, signal) {
			continue
		}
		if len(upper) == len(signal) {
			return true
		}
		next, _ := utf8DecodeRuneInString(upper[len(signal):])
		return !unicode.IsLetter(next) && !unicode.IsNumber(next)
	}
	return false
}

// kept as a variable so the boundary operation is easy to read and test.
var utf8DecodeRuneInString = func(value string) (rune, int) { return utf8.DecodeRuneInString(value) }

type messagePart struct {
	Text   string
	DocKey string
}

func messageParts(event store.RelayEvent) []messagePart {
	text := eventMessage(event)
	var parts []messagePart
	textStart, search := 0, 0
	for search < len(text) {
		at := strings.Index(text[search:], "doc:")
		if at < 0 {
			break
		}
		at += search
		end := at + len("doc:")
		for end < len(text) && documentKeyByte(text[end]) {
			end++
		}
		key := text[at+len("doc:") : end]
		if validDocumentKey(key) {
			if textStart < at {
				parts = append(parts, messagePart{Text: text[textStart:at]})
			}
			parts = append(parts, messagePart{DocKey: key})
			textStart, search = end, end
			continue
		}
		search = at + len("doc:")
	}
	if textStart < len(text) || len(parts) == 0 {
		parts = append(parts, messagePart{Text: text[textStart:]})
	}
	return parts
}

func documentKeyByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '.' || value == '_' || value == '-' || value == '/'
}

func validDocumentKey(key string) bool {
	return docKeyRE.MatchString(key) && !strings.HasPrefix(key, "/") && !strings.Contains(key, "..")
}

func (h *Handler) document(w http.ResponseWriter, r *http.Request, key string) {
	if !validDocumentKey(key) {
		http.Error(w, "invalid document key", http.StatusBadRequest)
		return
	}
	document, found, err := h.store.GetDocument(r.Context(), key)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.render(w, "page", pageData{Title: "Document", Page: "document", Body: document})
}
