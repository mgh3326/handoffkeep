package wrk

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	uuidRE   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	jobIDRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// verdictHeaderRE is the design §5 first-line grammar (ASCII only).
	verdictHeaderRE = regexp.MustCompile(`^VERDICT: (PASS|BLOCKER) @([0-9a-fA-F]{40})[ \t]*\r?$`)
)

// ValidUUID reports canonical lowercase UUID text.
func ValidUUID(s string) bool { return uuidRE.MatchString(s) }

// ValidJobID reports a §5 job id (also the path segment).
func ValidJobID(s string) bool { return jobIDRE.MatchString(s) }

// NewUUID returns a random canonical lowercase version 4 UUID.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// FormatTime renders server time as UTC RFC 3339 with microseconds.
func FormatTime(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}

// parseDateTime accepts an RFC 3339 date-time (JSON Schema format
// date-time).
func parseDateTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// DecodeContent decodes RFC 4648 standard padded base64 and requires the
// canonical form (re-encoding equals the input), 1..262144 decoded bytes.
// tooLarge distinguishes the §5 request_too_large case.
func DecodeContent(s string) (b []byte, tooLarge bool, ok bool) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || base64.StdEncoding.EncodeToString(b) != s {
		return nil, false, false
	}
	if len(b) > MaxContentBytes {
		return nil, true, false
	}
	if len(b) == 0 {
		return nil, false, false
	}
	return b, false, true
}

// MaxContentBytes is the decoded evidence/import ceiling (design §2, §5).
const MaxContentBytes = 262144

// MaxRequestBytes is the request body ceiling (design §5).
const MaxRequestBytes = 1048576

// NormalizePath applies the design §2 path identity: POSIX lexical
// normalisation of an absolute path, refusing NUL/control characters,
// backslash, relative paths and more than 4096 UTF-8 bytes. Case and
// Unicode bytes are preserved; nothing touches a disk.
func NormalizePath(p string) (string, bool) {
	if len(p) == 0 || len(p) > 4096 || !utf8.ValidString(p) || p[0] != '/' {
		return "", false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' || (r >= 0x80 && r <= 0x9f) {
			return "", false
		}
	}
	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return "/" + strings.Join(out, "/"), true
}

// ParseVerdict applies the design §5 verdict byte grammar to the exact
// stored bytes and returns the verdict and the lowercase head.
func ParseVerdict(b []byte) (verdict, head string, ok bool) {
	if !utf8.Valid(b) || bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		return "", "", false
	}
	lines := bytes.Split(b, []byte("\n"))
	header := string(lines[0])
	m := verdictHeaderRE.FindStringSubmatch(header)
	if m == nil {
		return "", "", false
	}
	// A CR in the header is valid only directly before the first LF.
	if i := strings.IndexByte(header, '\r'); i >= 0 && (i != len(header)-1 || len(lines) == 1) {
		return "", "", false
	}
	for _, line := range lines[1:] {
		if bytes.HasPrefix(line, []byte("VERDICT:")) {
			return "", "", false
		}
	}
	return m[1], strings.ToLower(m[2]), true
}

// ValidPRURL reports an https URL without userinfo (design §4 join rule).
func ValidPRURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Opaque != "" {
		return false
	}
	return strings.HasPrefix(s, "https://")
}

// ContentSHA256 is the lowercase hex digest of exact bytes.
func ContentSHA256(b []byte) string {
	return fmt.Sprintf("%x", sha256Sum(b))
}
