// Package wrkrecords implements the fixed hk1873.v1 authority contract.
// Transition evaluation is pure; callers must hold the PostgreSQL job locks
// through validation, record/content/relay insertion, and durable commit.
package wrkrecords

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

const Contract = "hk1873.v1"
const MaxRequest = 1048576
const MaxContent = 262144
const MaxInt = 9007199254740991

type Error string

func (e Error) Error() string { return string(e) }

var statuses = map[string]int{}
var schemas = map[string]*jsonschema.Resolved{}
var initOnce sync.Once

func initContract() {
	initOnce.Do(func() {
		var root jsonschema.Schema
		if err := json.Unmarshal([]byte(contractSchema), &root); err != nil {
			panic("invalid compiled wrk contract")
		}
		for name := range root.Defs {
			s, err := (&jsonschema.Schema{Schema: root.Schema, Ref: "#/$defs/" + name, Defs: root.Defs}).Resolve(nil)
			if err != nil {
				panic("invalid compiled wrk schema")
			}
			schemas[name] = s
		}
		var raw struct {
			Errors map[string][]string `json:"errors_by_status"`
		}
		if err := json.Unmarshal([]byte(contractSchema), &raw); err != nil {
			panic("invalid compiled error map")
		}
		for status, codes := range raw.Errors {
			n, _ := strconv.Atoi(status)
			for _, code := range codes {
				statuses[code] = n
			}
		}
	})
}
func Status(err error) int {
	initContract()
	var e Error
	if errors.As(err, &e) {
		if n := statuses[string(e)]; n != 0 {
			return n
		}
	}
	return 503
}
func Code(err error) string {
	initContract()
	var e Error
	if errors.As(err, &e) && statuses[string(e)] != 0 {
		return string(e)
	}
	return "wrk_unavailable"
}
func Exit(code string) int {
	switch code {
	case "round_cap":
		return 78
	case "unauthorized", "scope_denied", "host_mismatch":
		return 77
	case "not_found":
		return 66
	case "cli_usage":
		return 2
	case "intent_invalid", "intent_io":
		return 74
	}
	initContract()
	switch statuses[code] {
	case 400, 405, 413, 415:
		return 65
	case 409:
		return 73
	}
	return 75
}
func Validate(name string, v any) error {
	initContract()
	s := schemas[name]
	if s == nil || s.Validate(v) != nil {
		return Error("invalid_request")
	}
	return nil
}

// Decode rejects duplicate keys, invalid UTF-8, unpaired UTF-16 escapes, and
// extra values before schema validation. No rejected content enters an error.
func Decode(raw []byte) (any, error)         { return decodeLimit(raw, MaxRequest) }
func DecodeResponse(raw []byte) (any, error) { return decodeLimit(raw, 16<<20) }
func decodeLimit(raw []byte, max int) (any, error) {
	if len(raw) > max {
		return nil, Error("request_too_large")
	}
	if !utf8.Valid(raw) || !validEscapes(raw) {
		return nil, Error("invalid_request")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var read func() (any, error)
	read = func() (any, error) {
		t, e := d.Token()
		if e != nil {
			return nil, Error("invalid_request")
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return t, nil
		}
		switch delim {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, Error("invalid_request")
				}
				s, ok := k.(string)
				if !ok {
					return nil, Error("invalid_request")
				}
				if _, ok = m[s]; ok {
					return nil, Error("invalid_request")
				}
				v, e := read()
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return nil, Error("invalid_request")
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				v, e := read()
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return nil, Error("invalid_request")
			}
			return a, nil
		}
		return nil, Error("invalid_request")
	}
	v, e := read()
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, Error("invalid_request")
	}
	return v, nil
}
func validEscapes(b []byte) bool {
	in := false
	for i := 0; i < len(b); i++ {
		if b[i] == '"' {
			in = !in
			continue
		}
		if !in || b[i] != '\\' {
			continue
		}
		i++
		if i >= len(b) {
			return false
		}
		if b[i] != 'u' {
			continue
		}
		if i+4 >= len(b) {
			return false
		}
		n, e := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
			return false
		}
		m, e := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
		if e != nil || m < 0xdc00 || m > 0xdfff {
			return false
		}
		i += 6
	}
	return !in
}
func Canonical(v any) ([]byte, error) {
	// Normalize structs/integers as the IEEE754 JSON data model required by JCS.
	b, e := json.Marshal(v)
	if e != nil {
		return nil, Error("invalid_request")
	}
	v, e = Decode(b)
	if e != nil {
		return nil, e
	}
	var out bytes.Buffer
	var emit func(any) error
	quote := func(s string) {
		out.WriteByte('"')
		for _, r := range s {
			switch r {
			case '"', '\\':
				out.WriteByte('\\')
				out.WriteRune(r)
			case '\b':
				out.WriteString(`\b`)
			case '\t':
				out.WriteString(`\t`)
			case '\n':
				out.WriteString(`\n`)
			case '\f':
				out.WriteString(`\f`)
			case '\r':
				out.WriteString(`\r`)
			default:
				if r < 32 {
					out.WriteString(`\u00`)
					out.WriteByte("0123456789abcdef"[r>>4])
					out.WriteByte("0123456789abcdef"[r&15])
				} else {
					out.WriteRune(r)
				}
			}
		}
		out.WriteByte('"')
	}
	emit = func(v any) error {
		switch x := v.(type) {
		case nil:
			out.WriteString("null")
		case bool:
			out.WriteString(strconv.FormatBool(x))
		case string:
			quote(x)
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return Error("invalid_request")
			}
			if x == 0 {
				out.WriteByte('0')
				break
			}
			a := math.Abs(x)
			if a >= 1e-6 && a < 1e21 {
				out.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
			} else {
				s := strconv.FormatFloat(x, 'e', -1, 64)
				p := strings.IndexByte(s, 'e')
				n, _ := strconv.Atoi(s[p+1:])
				s = s[:p] + "e"
				if n >= 0 {
					s += "+"
				}
				out.WriteString(s + strconv.Itoa(n))
			}
		case []any:
			out.WriteByte('[')
			for i, v := range x {
				if i > 0 {
					out.WriteByte(',')
				}
				if e := emit(v); e != nil {
					return e
				}
			}
			out.WriteByte(']')
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := utf16.Encode([]rune(keys[i])), utf16.Encode([]rune(keys[j]))
				for k := 0; k < len(a) && k < len(b); k++ {
					if a[k] != b[k] {
						return a[k] < b[k]
					}
				}
				return len(a) < len(b)
			})
			out.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					out.WriteByte(',')
				}
				quote(k)
				out.WriteByte(':')
				if e := emit(x[k]); e != nil {
					return e
				}
			}
			out.WriteByte('}')
		default:
			return Error("invalid_request")
		}
		return nil
	}
	if e = emit(v); e != nil {
		return nil, e
	}
	return out.Bytes(), nil
}
func SHA(b []byte) string { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }
func Digest(v any) (string, error) {
	b, e := Canonical(v)
	if e != nil {
		return "", e
	}
	return SHA(b), nil
}

var JobRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
var UUIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var verdictRE = regexp.MustCompile(`^VERDICT: (PASS|BLOCKER) @([0-9a-fA-F]{40})[ \t]*\r?$`)

func ParseVerdict(b []byte) (string, string, error) {
	if !utf8.Valid(b) || bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}) {
		return "", "", Error("invalid_verdict")
	}
	lines := strings.Split(string(b), "\n")
	m := verdictRE.FindStringSubmatch(lines[0])
	if m == nil || (len(lines) == 1 && strings.HasSuffix(lines[0], "\r")) {
		return "", "", Error("invalid_verdict")
	}
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "VERDICT:") {
			return "", "", Error("invalid_verdict")
		}
	}
	return m[1], strings.ToLower(m[2]), nil
}
func NormalizePath(s string) (string, error) {
	if !utf8.ValidString(s) || len(s) > 4096 || !strings.HasPrefix(s, "/") || strings.Contains(s, "\\") {
		return "", Error("invalid_path")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", Error("invalid_path")
		}
	}
	return path.Clean(s), nil
}
func Evidence(encoded, digest string, filter bool) ([]byte, error) {
	b, e := base64.StdEncoding.Strict().DecodeString(encoded)
	if e != nil || len(b) == 0 || base64.StdEncoding.EncodeToString(b) != encoded {
		return nil, Error("invalid_request")
	}
	if len(b) > MaxContent {
		return nil, Error("request_too_large")
	}
	if SHA(b) != digest {
		return nil, Error("invalid_request")
	}
	if filter && guard.Reject(string(b)) != nil {
		return nil, Error("invalid_request")
	}
	return b, nil
}
