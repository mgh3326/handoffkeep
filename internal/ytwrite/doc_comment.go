package ytwrite

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Document copies (hk 4xxx, docs/youtrack-write-protocol.md "Document
// comments"): after the import ended, a task-tied report or brief reaches its
// task's YouTrack issue only through comment_append ops. DocComments is the
// import's rendering (youtrack/import/client.js documentIssueComments and
// taskReportKind) as pure text, so the bodies are byte-for-byte what the
// import posted; RenderComment then adds the protocol's provenance line and
// op marker.

// DocCommentPartBytes is the import's SAFE_PART_BYTES: one document comment
// body is at most this many bytes. A report is split at
// DocCommentPartBytes-docCommentHeadingBytes so its heading fits in each part.
const DocCommentPartBytes = 240 << 10

// MaxDocCommentBodyBytes is the cap on one document comment body. It is the
// import's proven limit, larger than the cap on hk's own comments
// (MaxBodyBytes), so a split part is accepted and an hk comment is not.
const MaxDocCommentBodyBytes = DocCommentPartBytes

const (
	docCommentHeadingBytes = 1024
	docArticleTitleRunes   = 250
)

// DocComments returns the comment bodies that a document with this key, kind
// and body gets on its task's issue, in part order. ok is false when the key
// is not a task-tied report or brief (nothing is copied). err is set when a
// part would exceed DocCommentPartBytes, which the import refused too.
func DocComments(key, kind, body string) (bodies []string, ok bool, err error) {
	path := strings.Split(key, "/")
	switch {
	case path[0] == "reports" && len(path) > 1 && strings.HasPrefix(path[1], "mwr-"),
		path[0] == "reports" && len(path) == 3 && path[2] == "report.md":
		return docReportBodies(key, body)
	case path[0] == "brief" && len(path) == 3:
		title := truncateRunes("["+path[0]+"] "+key+" ["+kind+"]", docArticleTitleRunes)
		return []string{"Article: " + title + " (hk document key: " + key + ")"}, true, nil
	}
	return nil, false, nil
}

func docReportBodies(key, body string) ([]string, bool, error) {
	parts := splitDocBody(body, DocCommentPartBytes-docCommentHeadingBytes)
	bodies := make([]string, 0, len(parts))
	for i, part := range parts {
		heading := "# " + key
		if len(parts) > 1 {
			heading += fmt.Sprintf(" (part %d/%d)", i+1, len(parts))
		}
		text := heading + "\n\n" + part
		if len(text) > DocCommentPartBytes {
			return nil, true, fmt.Errorf("%w: part %d of %s is %d bytes, over %d", ErrInvalid, i+1, key, len(text), DocCommentPartBytes)
		}
		bodies = append(bodies, text)
	}
	return bodies, true, nil
}

// splitDocBody splits text into pieces of at most maxBytes, the import's
// splitUtf8: whole lines are packed greedily, and a line longer than maxBytes
// is cut between its characters. Pieces never split a UTF-8 sequence.
func splitDocBody(text string, maxBytes int) []string {
	if len(text) <= maxBytes {
		return []string{text}
	}
	var chunks []string
	var chunk strings.Builder
	chunkBytes := 0
	flush := func() {
		if chunkBytes > 0 {
			chunks = append(chunks, chunk.String())
			chunk.Reset()
			chunkBytes = 0
		}
	}
	for _, line := range splitAfterNewline(text) {
		if len(line) > maxBytes {
			flush()
			var part strings.Builder
			partBytes := 0
			for i := 0; i < len(line); {
				_, w := utf8.DecodeRuneInString(line[i:])
				if partBytes > 0 && partBytes+w > maxBytes {
					chunks = append(chunks, part.String())
					part.Reset()
					partBytes = 0
				}
				part.WriteString(line[i : i+w])
				partBytes += w
				i += w
			}
			if partBytes > 0 {
				chunks = append(chunks, part.String())
			}
			continue
		}
		if chunkBytes > 0 && chunkBytes+len(line) > maxBytes {
			flush()
		}
		chunk.WriteString(line)
		chunkBytes += len(line)
	}
	flush()
	return chunks
}

// splitAfterNewline is JavaScript's text.split(/(?<=\n)/): each piece ends
// with its newline, and the last piece may not.
func splitAfterNewline(text string) []string {
	var out []string
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 || i == len(text)-1 {
			break
		}
		out = append(out, text[:i+1])
		text = text[i+1:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// CheckDocComment is CheckComment with the document cap: the same request
// checks, and a body up to MaxDocCommentBodyBytes.
func CheckDocComment(req CommentRequest) error {
	return checkCommentRequest(req, MaxDocCommentBodyBytes)
}
