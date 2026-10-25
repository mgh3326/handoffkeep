package main

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

type wrkCLIError struct{ code string }

func (e wrkCLIError) Error() string { return e.code }
func wrkCmd(args []string, in io.Reader, out io.Writer) error {
	fail := func(code string) error { return wrkCLIError{code} }
	if len(args) == 0 {
		return fail("cli_usage")
	}
	command := args[0]
	positionals := []string{}
	flags := map[string]string{}
	allowed := map[string]bool{}
	switch command {
	case "capabilities", "job", "content":
	case "append":
		for _, f := range []string{"kind", "idempotency-key", "expected-seq", "claim-seq"} {
			allowed[f] = true
		}
	case "records":
		for _, f := range []string{"after-seq", "snapshot-seq", "limit"} {
			allowed[f] = true
		}
	default:
		return fail("cli_usage")
	}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		if !strings.HasPrefix(arg, "--") || !allowed[name] {
			return fail("cli_usage")
		}
		if _, ok := flags[name]; ok {
			return fail("cli_usage")
		}
		i++
		if i >= len(args) {
			return fail("cli_usage")
		}
		flags[name] = args[i]
	}
	want := 1
	if command == "capabilities" {
		want = 0
	}
	if command == "content" {
		want = 2
	}
	if len(positionals) != want {
		return fail("cli_usage")
	}
	if want > 0 && !wrkrecords.JobRE.MatchString(positionals[0]) {
		return fail("cli_usage")
	}
	integer := func(s string, positive bool) (int64, bool) {
		if s == "" {
			return 0, false
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return 0, false
			}
		}
		n, e := strconv.ParseInt(s, 10, 64)
		return n, e == nil && n <= wrkrecords.MaxInt && (!positive || n > 0)
	}
	method, path, def := "GET", "/v1/wrk/capabilities", "Capabilities"
	if want > 0 {
		path = "/v1/wrk/jobs/" + positionals[0]
	}
	var seq, claim int64
	var key string
	switch command {
	case "job":
		def = "JobResponse"
	case "records":
		def = "RecordsResponse"
		path += "/records"
		query := url.Values{}
		for name, value := range flags {
			n, ok := integer(value, name == "snapshot-seq")
			if !ok || (name == "limit" && (n < 1 || n > 500)) {
				return fail("cli_usage")
			}
			query.Set(strings.ReplaceAll(name, "-", "_"), value)
		}
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
	case "content":
		def = "ContentResponse"
		if _, ok := integer(positionals[1], true); !ok {
			return fail("cli_usage")
		}
		path += "/records/" + positionals[1] + "/content"
	case "append":
		method, def = "POST", "WriteResponse"
		path += "/records"
		if len(flags) != 4 {
			return fail("cli_usage")
		}
		if !wrkrecords.UUIDRE.MatchString(flags["idempotency-key"]) {
			return fail("cli_usage")
		}
		key = flags["idempotency-key"]
		var ok bool
		seq, ok = integer(flags["expected-seq"], false)
		if !ok {
			return fail("cli_usage")
		}
		claim, ok = integer(flags["claim-seq"], false)
		if !ok {
			return fail("cli_usage")
		}
	}
	var input []byte
	// Interactive read commands have no stdin payload. Pipes/files are consumed
	// through EOF so extra data cannot be silently ignored.
	interactive := false
	if file, ok := in.(*os.File); ok {
		if stat, e := file.Stat(); e == nil {
			interactive = stat.Mode()&os.ModeCharDevice != 0
		}
	}
	if !interactive || command == "append" {
		var e error
		input, e = io.ReadAll(io.LimitReader(in, wrkrecords.MaxRequest+1))
		if e != nil {
			return fail("invalid_request")
		}
	}
	if len(input) > wrkrecords.MaxRequest {
		return fail("request_too_large")
	}
	var body []byte
	if command == "append" {
		value, e := wrkrecords.Decode(input)
		if e != nil {
			return fail(wrkrecords.Code(e))
		}
		if e := wrkrecords.ValidateInput(flags["kind"], value); e != nil {
			return fail(wrkrecords.Code(e))
		}
		payload, ok := value.(map[string]any)
		if !ok {
			return fail("invalid_request")
		}
		body, _ = json.Marshal(wrkrecords.Request{Kind: flags["kind"], ExpectedSeq: seq, ClaimSeq: claim, Payload: payload})
		if _, e = wrkrecords.ParseRequest(body); e != nil {
			return fail(wrkrecords.Code(e))
		}
	} else if len(input) > 0 {
		return fail("invalid_request")
	}
	client := configuredClient()
	if client.URL == "" || client.Token == "" {
		return fail("transport_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, code := client.WrkCall(ctx, method, path, body, key, def)
	if code != "" {
		return fail(code)
	}
	if _, e := out.Write(append(result, '\n')); e != nil {
		return fail("protocol_invalid")
	}
	return nil
}
