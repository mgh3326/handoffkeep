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
	"github.com/mgh3326/handoffkeep/internal/store"
)

var attachmentSHA256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// inlineMediaTypes is the whole allowlist behind ?inline=1: the console shows
// these in place. Everything else — HTML, SVG, PDF, plain text — keeps the
// download disposition even when asked to go inline, so this origin never
// renders an attachment as a document. The stored type is the one upload
// sniffed, not a client's claim (attachments.validate).
var inlineMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
	"video/mp4":  true,
	"video/webm": true,
}

// attachmentDisposition is the Content-Disposition the object store signs into
// the redirect: inline for an allowlisted image or video requested with
// ?inline=1, attachment for everything else.
func attachmentDisposition(r *http.Request, attachment store.Attachment) string {
	kind := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		if mediaType, _, err := mime.ParseMediaType(attachment.MIME); err == nil && inlineMediaTypes[mediaType] {
			kind = "inline"
		}
	}
	disposition := mime.FormatMediaType(kind, map[string]string{"filename": attachment.OriginalName})
	if disposition == "" {
		disposition = kind + "; filename*=UTF-8''" + url.PathEscape(attachment.OriginalName)
	}
	return disposition
}

func (h *Handler) fileDownload(w http.ResponseWriter, r *http.Request, email, sha string) {
	result := "internal_error"
	var failure string
	defer func() {
		if failure == "" {
			log.Printf("ui.file_download email=%q sha=%q result=%q", email, sha, result)
			return
		}
		log.Printf("ui.file_download email=%q sha=%q result=%q err=%q", email, sha, result, failure)
	}()
	// The redirect itself is the response this handler writes; the object's
	// bytes come from the object store, so this header does not govern them.
	// It still keeps a browser from sniffing this route's own answers.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !attachmentSHA256RE.MatchString(sha) {
		result = "not_found"
		http.NotFound(w, r)
		return
	}
	attachment, found, err := h.store.GetAttachment(r.Context(), sha)
	if err != nil {
		failure = err.Error()
		http.Error(w, "file unavailable", http.StatusInternalServerError)
		return
	}
	if !found {
		result = "not_found"
		http.NotFound(w, r)
		return
	}
	if h.attachments == nil {
		failure = "attachment service unavailable"
		http.Error(w, "file unavailable", http.StatusInternalServerError)
		return
	}
	// An archived blob has no hot object to sign: answer 보관됨 and where the
	// verified copy is (design/2026-11-16/hk-attachment-archive). A row whose
	// object_location is another store is the same answer from the migration
	// that predates archived_at.
	archiveLocation := attachment.ArchiveLocation
	if archiveLocation == "" && attachment.ObjectLocation != attachments.ObjectLocation(h.attachments.Config) {
		archiveLocation = attachment.ObjectLocation
	}
	if archiveLocation != "" || attachment.ArchivedAt != nil {
		result = "archived"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		if archiveLocation == "" {
			archiveLocation = "unknown location (legacy metadata)"
		}
		_, _ = fmt.Fprintf(w, "<!doctype html><title>파일 보관됨</title><p>보관됨 · %s (restore is not available yet, hk 3680)</p>", html.EscapeString(archiveLocation))
		return
	}
	disposition := attachmentDisposition(r, attachment)
	location, err := h.attachments.PresignDownload(r.Context(), sha, disposition)
	if err != nil {
		failure = err.Error()
		http.Error(w, "file unavailable", http.StatusInternalServerError)
		return
	}
	if parsed, err := url.ParseRequestURI(location); err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		failure = "invalid presigned URL"
		http.Error(w, "file unavailable", http.StatusInternalServerError)
		return
	}
	result = "redirect"
	http.Redirect(w, r, location, http.StatusFound)
}
