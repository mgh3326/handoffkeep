package ui

import (
	"fmt"
	"html"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"

	"github.com/mgh3326/handoffkeep/internal/attachments"
)

var attachmentSHA256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (h *Handler) fileDownload(w http.ResponseWriter, r *http.Request, email, sha string) {
	result := "internal_error"
	defer func() { log.Printf("ui.file_download email=%q sha=%q result=%q", email, sha, result) }()
	if !attachmentSHA256RE.MatchString(sha) {
		result = "not_found"
		http.NotFound(w, r)
		return
	}
	attachment, found, err := h.store.GetAttachment(r.Context(), sha)
	if err != nil {
		http.Error(w, "file unavailable", http.StatusInternalServerError)
		return
	}
	if !found {
		result = "not_found"
		http.NotFound(w, r)
		return
	}
	if h.attachments == nil {
		http.Error(w, "file unavailable", http.StatusInternalServerError)
		return
	}
	if attachment.ObjectLocation != attachments.ObjectLocation(h.attachments.Config) {
		result = "archived"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		location := attachment.ObjectLocation
		if location == "" {
			location = "unknown location (legacy metadata)"
		}
		_, _ = fmt.Fprintf(w, "<!doctype html><title>파일 보관됨</title><p>archived at %s; restore is not available yet (hk 3680)</p>", html.EscapeString(location))
		return
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": attachment.OriginalName})
	if disposition == "" {
		disposition = "attachment; filename*=UTF-8''" + url.PathEscape(attachment.OriginalName)
	}
	location, err := h.attachments.PresignDownload(r.Context(), sha, disposition)
	if err != nil {
		http.Error(w, "file unavailable", http.StatusInternalServerError)
		return
	}
	if parsed, err := url.ParseRequestURI(location); err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		http.Error(w, "file unavailable", http.StatusInternalServerError)
		return
	}
	result = "redirect"
	http.Redirect(w, r, location, http.StatusFound)
}
