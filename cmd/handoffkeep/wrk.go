package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// The handoffkeep wrk namespace (hk 1873 design §6b): the only network
// client for wrk journal operations. It uses the installed remote
// configuration only; no flag selects a token, token path, URL or
// transport. Success prints exactly one newline-terminated JSON object that
// conforms to the §5 schema; every failure prints exactly one line
// "handoffkeep wrk: CODE" on stderr and nothing on stdout.

// Subcommands, each with exactly these arguments:
//
//	wrk capabilities
//	wrk append JOB --kind K --idempotency-key U --expected-seq N --claim-seq C
//	wrk job JOB
//	wrk records JOB [--after-seq N] [--snapshot-seq S] [--limit L]
//	wrk content JOB SEQ

// wrkTimeout bounds the single network attempt of one invocation.
var wrkTimeout = 60 * time.Second

// wrkExit ends the process with code; main prints nothing for it.
type wrkExit struct{ code int }

func (e wrkExit) Error() string { return "" }

type wrkIO struct {
	stdin    io.Reader
	stdinTTY bool
	out      io.Writer
	errout   io.Writer
}

func wrkFail(w wrkIO, code wrk.Code) error {
	exit, ok := wrk.Exit(code)
	if !ok {
		code, exit = wrk.ProtocolInvalid, 75
	}
	fmt.Fprintf(w.errout, "handoffkeep wrk: %s\n", code)
	return wrkExit{code: exit}
}

func wrkCmd(args []string, stdin io.Reader, out, errout io.Writer) error {
	tty := false
	if f, ok := stdin.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			tty = true
		}
	}
	return runWrk(args, wrkIO{stdin: stdin, stdinTTY: tty, out: out, errout: errout}, configuredClient())
}

type wrkCall struct {
	method, path, key string
	body              []byte
	def               string
	append            bool
	afterSeq          int64
}

// parseWrkArgs maps argv to one request. Unknown, missing or repeated flags
// and extra positionals are cli_usage.
func parseWrkArgs(args []string) (wrkCall, []string, bool) {
	if len(args) == 0 {
		return wrkCall{}, nil, false
	}
	sub, rest := args[0], args[1:]
	positional, flags, ok := splitWrkFlags(rest)
	if !ok {
		return wrkCall{}, nil, false
	}
	allowed := map[string][]string{
		"capabilities": nil,
		"append":       {"--kind", "--idempotency-key", "--expected-seq", "--claim-seq"},
		"job":          nil,
		"records":      {"--after-seq", "--snapshot-seq", "--limit"},
		"content":      nil,
	}
	names, known := allowed[sub]
	if !known {
		return wrkCall{}, nil, false
	}
	for f := range flags {
		found := false
		for _, n := range names {
			found = found || n == f
		}
		if !found {
			return wrkCall{}, nil, false
		}
	}
	switch sub {
	case "capabilities":
		if len(positional) != 0 {
			return wrkCall{}, nil, false
		}
		return wrkCall{method: "GET", path: "/v1/wrk/capabilities", def: "Capabilities"}, nil, true
	case "job":
		if len(positional) != 1 || !wrk.ValidJobID(positional[0]) {
			return wrkCall{}, nil, false
		}
		return wrkCall{method: "GET", path: "/v1/wrk/jobs/" + positional[0], def: "JobResponse"}, nil, true
	case "content":
		if len(positional) != 2 || !wrk.ValidJobID(positional[0]) {
			return wrkCall{}, nil, false
		}
		seq, ok := safeInt(positional[1])
		if !ok || seq < 1 {
			return wrkCall{}, nil, false
		}
		return wrkCall{method: "GET", path: "/v1/wrk/jobs/" + positional[0] + "/records/" + strconv.FormatInt(seq, 10) + "/content", def: "ContentResponse"}, nil, true
	case "records":
		if len(positional) != 1 || !wrk.ValidJobID(positional[0]) {
			return wrkCall{}, nil, false
		}
		q := url.Values{}
		for flag, key := range map[string]string{"--after-seq": "after_seq", "--snapshot-seq": "snapshot_seq", "--limit": "limit"} {
			if v, set := flags[flag]; set {
				n, ok := safeInt(v)
				if !ok {
					return wrkCall{}, nil, false
				}
				q.Set(key, strconv.FormatInt(n, 10))
			}
		}
		path := "/v1/wrk/jobs/" + positional[0] + "/records"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		after, _ := safeInt(q.Get("after_seq"))
		return wrkCall{method: "GET", path: path, def: "RecordsResponse", afterSeq: after}, nil, true
	case "append":
		if len(positional) != 1 || !wrk.ValidJobID(positional[0]) || len(flags) != 4 {
			return wrkCall{}, nil, false
		}
		kind := flags["--kind"]
		knownKind := false
		for _, k := range wrk.Kinds {
			knownKind = knownKind || k == kind
		}
		expected, ok1 := safeInt(flags["--expected-seq"])
		claim, ok2 := safeInt(flags["--claim-seq"])
		if !knownKind || !wrk.ValidUUID(flags["--idempotency-key"]) || !ok1 || !ok2 {
			return wrkCall{}, nil, false
		}
		return wrkCall{method: "POST", path: "/v1/wrk/jobs/" + positional[0] + "/records", key: flags["--idempotency-key"], def: "WriteResponse", append: true}, []string{kind, strconv.FormatInt(claim, 10), strconv.FormatInt(expected, 10)}, true
	}
	return wrkCall{}, nil, false
}

func splitWrkFlags(args []string) ([]string, map[string]string, bool) {
	var positional []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 0 && a[0] == '-' {
			if _, dup := flags[a]; dup || i+1 >= len(args) {
				return nil, nil, false
			}
			flags[a] = args[i+1]
			i++
			continue
		}
		positional = append(positional, a)
	}
	return positional, flags, true
}

func safeInt(s string) (int64, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n <= wrk.MaxSafeInteger
}

func runWrk(args []string, w wrkIO, client remote.Client) error {
	call, appendArgs, ok := parseWrkArgs(args)
	if !ok {
		return wrkFail(w, wrk.CLIUsage)
	}
	var stdin []byte
	if !w.stdinTTY && w.stdin != nil {
		b, err := io.ReadAll(io.LimitReader(w.stdin, wrk.MaxRequestBytes+1))
		if err != nil {
			return wrkFail(w, wrk.InvalidRequest)
		}
		stdin = b
	}
	if call.append {
		if len(stdin) > wrk.MaxRequestBytes {
			return wrkFail(w, wrk.RequestTooLarge)
		}
		payload, err := wrk.ParseInput(appendArgs[0], stdin)
		if err != nil {
			return wrkFail(w, wrk.InvalidRequest)
		}
		for _, k := range []string{"content_b64", "report_b64"} {
			if s, ok := payload[k].(string); ok {
				if _, tooLarge, ok := wrk.DecodeContent(s); tooLarge {
					return wrkFail(w, wrk.RequestTooLarge)
				} else if !ok {
					return wrkFail(w, wrk.InvalidRequest)
				}
			}
		}
		claim, _ := strconv.ParseInt(appendArgs[1], 10, 64)
		expected, _ := strconv.ParseInt(appendArgs[2], 10, 64)
		body, err := json.Marshal(map[string]any{"kind": appendArgs[0], "claim_seq": claim, "expected_seq": expected, "payload": payload})
		if err != nil || len(body) > wrk.MaxRequestBytes {
			return wrkFail(w, wrk.RequestTooLarge)
		}
		call.body = body
	} else if len(stdin) > 0 {
		return wrkFail(w, wrk.InvalidRequest)
	}
	if client.URL == "" || client.Token == "" || remote.CFAccessPairError(client.CFAccessClientID, client.CFAccessClientSecret) != nil {
		return wrkFail(w, wrk.TransportUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wrkTimeout)
	defer cancel()
	resp, err := client.Wrk(ctx, call.method, call.path, call.key, call.body)
	if err != nil {
		if remote.IsWrkTransportError(err) {
			return wrkFail(w, wrk.TransportUnavailable)
		}
		return wrkFail(w, wrk.TransportIndeterminate)
	}
	out, code := checkWrkResponse(call, resp)
	if code != "" {
		return wrkFail(w, code)
	}
	if _, err := w.out.Write(append(out, '\n')); err != nil {
		return wrkExit{code: 74}
	}
	return nil
}

func isJSONContent(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// checkWrkResponse validates one reply completely before anything is
// published: a contract success body, or a stable {"error": code} whose
// status matches the §4 map. Everything else is protocol_invalid (or
// o3_unsupported for a missing namespace or another contract).
func checkWrkResponse(call wrkCall, resp remote.WrkResponse) ([]byte, wrk.Code) {
	if resp.Redirect || resp.Status == 0 {
		return nil, wrk.ProtocolInvalid
	}
	v, err := wrk.DecodeStrict(bytes.TrimRight(resp.Body, "\n"))
	if err != nil || !isJSONContent(resp.ContentType) {
		if resp.Status == 404 {
			return nil, wrk.O3Unsupported
		}
		return nil, wrk.ProtocolInvalid
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, wrk.ProtocolInvalid
	}
	if resp.Status >= 400 {
		code, _ := obj["error"].(string)
		status, known := wrk.Status(wrk.Code(code))
		if len(obj) != 1 || !known || status != resp.Status {
			return nil, wrk.ProtocolInvalid
		}
		return nil, wrk.Code(code)
	}
	if c, present := obj["contract"]; present && c != wrk.ContractVersion {
		return nil, wrk.O3Unsupported
	}
	switch {
	case call.append && resp.Status == 201:
		if obj["created"] != true {
			return nil, wrk.ProtocolInvalid
		}
	case call.append && resp.Status == 200:
		if obj["created"] != false {
			return nil, wrk.ProtocolInvalid
		}
	case !call.append && resp.Status == 200:
	default:
		return nil, wrk.ProtocolInvalid
	}
	if err := wrk.ValidateDef(call.def, obj); err != nil {
		return nil, wrk.ProtocolInvalid
	}
	if call.def == "RecordsResponse" && !recordsPageConsistent(obj, call.afterSeq) {
		return nil, wrk.ProtocolInvalid
	}
	canon, err := json.Marshal(obj)
	if err != nil {
		return nil, wrk.ProtocolInvalid
	}
	return canon, ""
}

// recordsPageConsistent checks one page's cursor arithmetic against the
// request: journal seqs are gap-free, so a page starts at after_seq+1 and is
// consecutive within the snapshot, next_after_seq is the last returned seq
// (after_seq for an empty page), an empty page only ends the snapshot, and
// has_more holds exactly when the page falls short of it.
func recordsPageConsistent(obj map[string]any, after int64) bool {
	snap, _ := obj["snapshot_seq"].(int64)
	next, _ := obj["next_after_seq"].(int64)
	more, _ := obj["has_more"].(bool)
	recs, _ := obj["records"].([]any)
	sum, _ := obj["summary"].(map[string]any)
	if s, _ := sum["snapshot_seq"].(int64); s != snap {
		return false
	}
	last := after
	for _, r := range recs {
		seq, _ := r.(map[string]any)["seq"].(int64)
		if seq != last+1 || seq > snap {
			return false
		}
		last = seq
	}
	if last != next || (len(recs) == 0 && next != snap) {
		return false
	}
	return more == (next < snap) && next <= snap
}
