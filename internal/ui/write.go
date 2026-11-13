package ui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const csrfCookie = "hk_ui_csrf"

var docKeyRE = regexp.MustCompile(`^[A-Za-z0-9._\-/]{1,512}$`)

type writeOutcome struct {
	action  string
	target  string
	eventID string
	result  string
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
	case "/ui/api/decision-requests/resolve":
		return "decision-resolve"
	case "/ui/api/decision-requests/deny":
		return "decision-deny"
	case "/ui/api/inbox/state":
		return "inbox"
	case "/ui/api/inbox/undo":
		return "inbox-undo"
	default:
		if strings.HasPrefix(path, "/ui/api/nodes/") && strings.HasSuffix(path, "/accepting") {
			return "accepting"
		}
		if taskPriorityPathRE.MatchString(path) {
			return "task-priority"
		}
		if action := lifecycleWriteAction(path); action != "" {
			return action
		}
		if taskCommentsPathRE.MatchString(path) {
			return "comment"
		}
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

func validDocumentKey(key string) bool {
	return docKeyRE.MatchString(key) && !strings.HasPrefix(key, "/") && !strings.Contains(key, "..")
}

func (h *Handler) document(w http.ResponseWriter, r *http.Request, key string) {
	if !validDocumentKey(key) {
		http.Error(w, "invalid document key", http.StatusBadRequest)
		return
	}
	// A missing document is a 404 from the server, as the old page answered,
	// not a shell that later shows an empty viewer.
	if _, found, err := h.store.GetDocument(r.Context(), key); err != nil {
		http.Error(w, "document lookup failed", http.StatusInternalServerError)
		return
	} else if !found {
		http.NotFound(w, r)
		return
	}
	h.shell(w, r)
}
