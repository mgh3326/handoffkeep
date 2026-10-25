// Package wrk is the hk 1873 O3 authority core: the strict wire codec, the
// immutable policy snapshot, the per-job journal rules and the stable error
// map for the /v1/wrk namespace. It is store-free; internal/store supplies
// the PostgreSQL journal and the API layer supplies HTTP.
package wrk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxSafeInteger is the largest wire integer (2^53-1, design §2).
const MaxSafeInteger = 9007199254740991

var errStrictJSON = errors.New("invalid strict json")

// DecodeStrict parses exactly one JSON value. Duplicate object keys,
// invalid UTF-8, trailing data and any number that is not a plain decimal
// integer within the safe range are refused (design §4: duplicate keys are
// invalid_request, never last-key-wins). Objects decode to map[string]any,
// arrays to []any and integers to int64.
func DecodeStrict(b []byte) (any, error) {
	if !utf8.Valid(b) {
		return nil, errStrictJSON
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, errStrictJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errStrictJSON
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			out := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errStrictJSON
				}
				if _, dup := out[k]; dup {
					return nil, errStrictJSON
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				out[k] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return out, nil
		case '[':
			out := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return out, nil
		}
		return nil, errStrictJSON
	case json.Number:
		return strictInteger(string(t))
	case string, bool, nil:
		return t, nil
	}
	return nil, errStrictJSON
}

func strictInteger(s string) (int64, error) {
	digits := strings.TrimPrefix(s, "-")
	if digits == "" || (len(digits) > 1 && digits[0] == '0') || s == "-0" {
		return 0, errStrictJSON
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, errStrictJSON
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > MaxSafeInteger || n < -MaxSafeInteger {
		return 0, errStrictJSON
	}
	return n, nil
}

// Canonical serialises a DecodeStrict-shaped value with RFC 8785 (JCS)
// rules: object members sorted by UTF-16 code units, no insignificant
// whitespace, minimal string escapes and ES6 integer formatting.
func Canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// MustCanonical is Canonical for values built by this package.
func MustCanonical(v any) []byte {
	b, err := Canonical(v)
	if err != nil {
		panic(err)
	}
	return b
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeCanonicalString(b, x)
	case int:
		return writeCanonical(b, int64(x))
	case int64:
		if x > MaxSafeInteger || x < -MaxSafeInteger {
			return fmt.Errorf("integer %d outside the safe range", x)
		}
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > MaxSafeInteger {
			return errors.New("canonical json carries integers only")
		}
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, e)
		}
		b.WriteByte(']')
	case []int64:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("canonical json: unsupported %T", v)
	}
	return nil
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func writeCanonicalString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// toGeneric converts a JSON-marshalable Go value to the DecodeStrict shape
// so typed structs can be canonicalised.
func toGeneric(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return DecodeStrict(raw)
}
