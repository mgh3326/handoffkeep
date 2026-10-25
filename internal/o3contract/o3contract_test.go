// Package o3contract verifies the hk 1924 (O3 M1) contract fixture pack
// under testdata/o3. It tests the pack only: no server code, no
// database, no network. The pack is generated from the fixed design
// hk:doc designs/hk1873/b1873a-sol-r4 (doc 44185, revision 4,
// amendment 1) whose body sha256 is pinned below.
package o3contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	packDir         = "../../testdata/o3"
	designKey       = "designs/hk1873/b1873a-sol-r4"
	designDoc       = 44185
	designRevision  = 4
	designAmendment = 1
	designSHA256    = "ce4f6efbe39514bb0bced2d0a66510e646058a66d0d28bbb417af180f375c19d"
)

// expectedVectors pins the case count and the design-row coverage of
// every vector table. Deleting one design row's cases makes the row
// coverage check fail; deleting a whole file fails both.
var expectedVectors = map[string]struct {
	cases int
	rows  map[string]int // table_name -> required distinct row numbers
}{
	"section4-rules.json":      {46, map[string]int{"errmap": 9, "rules": 9}},
	"transition.json":          {27, map[string]int{"transition": 16}},
	"migration.json":           {10, map[string]int{"migration": 8}},
	"legacy-route-fence.json":  {12, map[string]int{"fence": 9}},
	"r3-regression.json":       {13, map[string]int{"r3": 9}},
	"r4-scoped-claim.json":     {39, map[string]int{"r4claim": 19}},
	"r4-review-lifecycle.json": {10, map[string]int{"r4life": 8}},
	"head-repeat-binding.json": {7, map[string]int{"headrepeat": 7}},
	"transport-intents.json":   {14, map[string]int{"transport": 12}},
	"failure-abuse.json":       {16, map[string]int{"failure": 16}},
}

// errorStatuses is the design's section 4 HTTP-status -> stable-code
// map. It is duplicated here so the pack cannot weaken it unnoticed.
var errorStatuses = map[string]int{
	"invalid_request": 400, "invalid_cursor": 400, "invalid_verdict": 400,
	"invalid_path": 400, "grant_before_cap": 400,
	"unauthorized": 401,
	"scope_denied": 403, "host_mismatch": 403,
	"not_found":            404,
	"idempotency_conflict": 409, "sequence_conflict": 409,
	"claim_conflict": 409, "job_ended": 409, "review_sealed": 409,
	"round_cap": 409, "round_not_open": 409, "round_has_verdict": 409,
	"round_voided": 409, "head_mismatch": 409, "duplicate_path": 409,
	"duplicate_content": 409, "duplicate_head": 409,
	"grant_invalid": 409, "grant_consumed": 409, "grant_expired": 409,
	"zero_rounds": 409, "review_incomplete": 409,
	"blocker_unresolved": 409, "wrk_receipt_required": 409,
	"import_unsealed": 409, "import_sealed": 409,
	"migration_frozen":       409,
	"request_too_large":      413,
	"method_not_allowed":     405,
	"unsupported_media_type": 415,
	"wrk_unavailable":        503, "wrk_recovery_required": 503,
}

// exitMap is the design's section 6b stable-code -> CLI exit map.
var exitMap = map[string]int{
	"round_cap":       78,
	"wrk_unavailable": 75, "wrk_recovery_required": 75,
	"unauthorized": 77, "scope_denied": 77, "host_mismatch": 77,
	"not_found":       66,
	"invalid_request": 65, "invalid_cursor": 65, "invalid_verdict": 65,
	"invalid_path": 65, "grant_before_cap": 65, "request_too_large": 65,
	"method_not_allowed": 65, "unsupported_media_type": 65,
	"idempotency_conflict": 73, "sequence_conflict": 73,
	"claim_conflict": 73, "job_ended": 73, "review_sealed": 73,
	"round_not_open": 73, "round_has_verdict": 73, "round_voided": 73,
	"head_mismatch": 73, "duplicate_path": 73, "duplicate_content": 73,
	"duplicate_head": 73, "grant_invalid": 73, "grant_consumed": 73,
	"grant_expired": 73, "zero_rounds": 73, "review_incomplete": 73,
	"blocker_unresolved": 73, "wrk_receipt_required": 73,
	"import_unsealed": 73, "import_sealed": 73, "migration_frozen": 73,
	"cli_usage":             2,
	"transport_unavailable": 75, "transport_indeterminate": 75,
	"protocol_invalid": 75, "o3_unsupported": 75,
	"history_regressed": 75,
	"intent_invalid":    74, "intent_io": 74,
}

type pack struct {
	root      *jsonschema.Schema
	resolved  *jsonschema.Resolved
	errorEnum map[string]bool // codes in $defs.Error.properties.error.enum
	cliExit   map[string]int  // cli.json exit map
	manifest  manifestDoc
}

type manifestDoc struct {
	Design struct {
		Key       string `json:"key"`
		Doc       int    `json:"doc"`
		Revision  int    `json:"revision"`
		Amendment int    `json:"amendment"`
		BodySHA   string `json:"body_sha256"`
	} `json:"design"`
	Files map[string]struct {
		SHA256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
	} `json:"files"`
}

type vectorFile struct {
	Pack    string       `json:"pack"`
	Section string       `json:"section"`
	Table   string       `json:"table"`
	Cases   []vectorCase `json:"cases"`
}

type vectorCase struct {
	Name   string `json:"name"`
	Source struct {
		Section   string `json:"section"`
		Table     string `json:"table"`
		TableName string `json:"table_name"`
		Row       int    `json:"row"`
		RowText   string `json:"row_text"`
	} `json:"source"`
	Given struct {
		JobID   string           `json:"job_id"`
		Policy  json.RawMessage  `json:"policy"`
		Records []map[string]any `json:"records"`
	} `json:"given"`
	Request  map[string]any `json:"request"`
	Expect   map[string]any `json:"expect"`
	Inferred []string       `json:"inferred"`
}

func readPackFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(packDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read pack file %s: %v", rel, err)
	}
	return b
}

func mustJSON(t *testing.T, b []byte, what string) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", what, err)
	}
	if dec.More() {
		t.Fatalf("%s: trailing data after first JSON value", what)
	}
	return v
}

func loadPack(t *testing.T) *pack {
	t.Helper()
	p := &pack{errorEnum: map[string]bool{}, cliExit: map[string]int{}}

	contractBytes := readPackFile(t, "contract.json")
	var root jsonschema.Schema
	if err := json.Unmarshal(contractBytes, &root); err != nil {
		t.Fatalf("contract.json does not unmarshal as a schema: %v", err)
	}
	if root.Schema != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("contract.json $schema = %q, want Draft 2020-12",
			root.Schema)
	}
	// "contract" is a custom (non-schema) keyword; check the raw value.
	var rawTop map[string]any
	if err := json.Unmarshal(contractBytes, &rawTop); err == nil {
		if rawTop["contract"] != "hk1873.v1" {
			t.Fatalf("contract.json contract = %v, want hk1873.v1",
				rawTop["contract"])
		}
	}
	rs, err := root.Resolve(nil)
	if err != nil {
		t.Fatalf("contract.json fails meta-schema/ref resolution: %v", err)
	}
	p.root, p.resolved = &root, rs

	errDef, ok := root.Defs["Error"]
	if !ok {
		t.Fatal("contract.json has no $defs.Error")
	}
	prop, ok := errDef.Properties["error"]
	if !ok || prop == nil {
		t.Fatal("$defs.Error has no error property enum")
	}
	for _, e := range prop.Enum {
		code, _ := e.(string)
		if code == "" {
			t.Fatalf("$defs.Error enum has a non-string member: %v", e)
		}
		p.errorEnum[code] = true
	}

	var cli struct {
		Subcommands []struct {
			Subcommand string `json:"subcommand"`
		} `json:"subcommands"`
		ExitMap []struct {
			Codes []string `json:"codes"`
			Exit  int      `json:"exit"`
		} `json:"exit_map"`
	}
	if err := json.Unmarshal(readPackFile(t, "cli.json"), &cli); err != nil {
		t.Fatalf("decode cli.json: %v", err)
	}
	for _, m := range cli.ExitMap {
		for _, code := range m.Codes {
			p.cliExit[code] = m.Exit
		}
	}

	if err := json.Unmarshal(readPackFile(t, "MANIFEST.json"),
		&p.manifest); err != nil {
		t.Fatalf("decode MANIFEST.json: %v", err)
	}
	return p
}

func TestContractBundle(t *testing.T) {
	p := loadPack(t)
	// Resolve() above already ran the meta-schema check and resolved
	// every internal $ref; here assert the normative surface exists.
	for _, def := range []string{"Request", "Record", "Summary",
		"WriteResponse", "JobResponse", "RecordsResponse",
		"ContentResponse", "Capabilities", "Error", "Policy"} {
		if _, ok := p.root.Defs[def]; !ok {
			t.Errorf("contract.json missing $defs.%s", def)
		}
	}
	// Every error code the design's section 4 map lists must exist in
	// the bundle's Error enumeration, and vice versa.
	for code, status := range errorStatuses {
		if !p.errorEnum[code] {
			t.Errorf("section 4 code %q (status %d) missing from "+
				"Error enum", code, status)
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(readPackFile(t, "contract.json"), &raw); err != nil {
		t.Fatal(err)
	}
	byStatus, _ := raw["errors_by_status"].(map[string]any)
	seenCodes := map[string]bool{}
	for statusStr, codesAny := range byStatus {
		status, _ := strconv.Atoi(statusStr)
		for _, c := range codesAny.([]any) {
			code := c.(string)
			seenCodes[code] = true
			if errorStatuses[code] != status {
				t.Errorf("contract errors_by_status maps %q to %s; "+
					"design map says %d", code, statusStr,
					errorStatuses[code])
			}
		}
	}
	for code, status := range errorStatuses {
		if !seenCodes[code] {
			t.Errorf("section 4 code %q (status %d) missing from "+
				"errors_by_status", code, status)
		}
	}
}

// resolveDef compiles a validator for one $defs entry by wrapping it in
// a root schema that shares the contract's $defs context.
func resolveDef(t *testing.T, p *pack, def string) *jsonschema.Resolved {
	t.Helper()
	wrapper := &jsonschema.Schema{
		Schema: "https://json-schema.org/draft/2020-12/schema",
		Ref:    "#/$defs/" + def,
		Defs:   p.root.Defs,
	}
	rs, err := wrapper.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve $defs/%s: %v", def, err)
	}
	return rs
}

func validateDef(t *testing.T, p *pack, def string, v any,
	context string) {
	t.Helper()
	if err := resolveDef(t, p, def).Validate(v); err != nil {
		t.Fatalf("%s: instance fails $defs/%s: %v", context, def, err)
	}
}

func TestManifest(t *testing.T) {
	p := loadPack(t)
	d := p.manifest.Design
	if d.Key != designKey || d.Doc != designDoc ||
		d.Revision != designRevision || d.Amendment != designAmendment {
		t.Fatalf("manifest design %v not the pinned %s rev %d amend %d",
			d, designKey, designRevision, designAmendment)
	}
	if d.BodySHA != designSHA256 {
		t.Fatalf("manifest design sha %s != pinned %s",
			d.BodySHA, designSHA256)
	}
	// Every file in the pack must be listed with a correct sha256/size;
	// every listed file must exist.
	onDisk := map[string]bool{}
	err := filepath.WalkDir(packDir, func(path string, e os.DirEntry,
		err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(packDir, path)
		rel = filepath.ToSlash(rel)
		if rel == "MANIFEST.json" || rel == "README.md" {
			return nil
		}
		onDisk[rel] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// README.md is part of the pack and is hashed too.
	readme := "README.md"
	if _, ok := p.manifest.Files[readme]; !ok {
		t.Errorf("manifest missing README.md")
	} else {
		onDisk[readme] = true
	}
	for rel, want := range p.manifest.Files {
		b := readPackFile(t, rel)
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want.SHA256 {
			t.Errorf("manifest sha256 for %s = %s, file is %s",
				rel, want.SHA256, got)
		}
		if int64(len(b)) != want.Bytes {
			t.Errorf("manifest size for %s = %d, file is %d",
				rel, want.Bytes, len(b))
		}
		delete(onDisk, rel)
	}
	for rel := range onDisk {
		t.Errorf("pack file %s missing from MANIFEST.json", rel)
	}
}

func TestVectors(t *testing.T) {
	p := loadPack(t)
	reqSchema := resolveDef(t, p, "Request")
	recSchema := resolveDef(t, p, "Record")
	polSchema := resolveDef(t, p, "Policy")

	files, err := filepath.Glob(filepath.Join(packDir, "vectors",
		"*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	seenFiles := map[string]bool{}
	for _, path := range files {
		base := filepath.Base(path)
		seenFiles[base] = true
		want, ok := expectedVectors[base]
		if !ok {
			t.Errorf("unlisted vector file %s", base)
			continue
		}
		var vf vectorFile
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &vf); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if vf.Pack != "hk1873.v1" {
			t.Errorf("%s: pack=%q", base, vf.Pack)
		}
		if len(vf.Cases) != want.cases {
			t.Errorf("%s: %d cases, want %d", base, len(vf.Cases),
				want.cases)
		}
		// Row coverage: every design row keeps at least one case.
		gotRows := map[string]map[int]bool{}
		for _, c := range vf.Cases {
			tn := c.Source.TableName
			if gotRows[tn] == nil {
				gotRows[tn] = map[int]bool{}
			}
			gotRows[tn][c.Source.Row] = true
			if c.Source.RowText == "" {
				t.Errorf("%s case %s: empty source.row_text",
					base, c.Name)
			}
		}
		for tn, n := range want.rows {
			got := gotRows[tn]
			if len(got) != n {
				t.Errorf("%s table %s covers %d rows, want %d",
					base, tn, len(got), n)
			}
			for r := 1; r <= n; r++ {
				if !got[r] {
					t.Errorf("%s table %s missing design row %d",
						base, tn, r)
				}
			}
		}
		for i := range vf.Cases {
			checkCase(t, p, reqSchema, recSchema, polSchema, base,
				&vf.Cases[i])
		}
	}
	for base := range expectedVectors {
		if !seenFiles[base] {
			t.Errorf("expected vector file %s missing from pack", base)
		}
	}
}

func checkCase(t *testing.T, p *pack, reqSchema, recSchema,
	polSchema *jsonschema.Resolved, file string, c *vectorCase) {
	t.Helper()
	ctx := fmt.Sprintf("%s case %s", file, c.Name)

	// given policy validates against $defs.Policy when present.
	if len(c.Given.Policy) > 0 {
		v := mustJSON(t, c.Given.Policy, ctx+" given.policy")
		if err := polSchema.Validate(v); err != nil {
			t.Fatalf("%s: given.policy fails $defs/Policy: %v",
				ctx, err)
		}
	}
	// Every given journal record is a valid Record.
	for i, r := range c.Given.Records {
		if err := recSchema.Validate(r); err != nil {
			t.Fatalf("%s: given record %d (kind=%v) fails "+
				"$defs/Record: %v", ctx, i, r["kind"], err)
		}
	}

	// Reviewer binding: every round_open/verdict/grant record or request
	// that names a tester_job_id must be preceded by that reviewer's
	// claim (the assignment must exist when the open is accepted).
	claimsSeen := map[string]bool{}
	for i, r := range c.Given.Records {
		if r["kind"] == "claim" {
			if jid, _ := r["job_id"].(string); jid != "" {
				claimsSeen[jid] = true
			}
			continue
		}
		pl, _ := r["payload"].(map[string]any)
		tid, _ := pl["tester_job_id"].(string)
		if tid != "" && !claimsSeen[tid] {
			t.Errorf("%s: given record %d (kind=%v) names "+
				"tester_job_id %q before any claim for it",
				ctx, i, r["kind"], tid)
		}
	}
	checkNamedTester := func(v any, where string) {
		m, _ := v.(map[string]any)
		tid, _ := m["tester_job_id"].(string)
		if tid != "" && !claimsSeen[tid] {
			t.Errorf("%s: %s names tester_job_id %q with no "+
				"earlier-received claim in given", ctx, where, tid)
		}
	}
	if body, ok := c.Request["body"].(map[string]any); ok {
		checkNamedTester(body["payload"], "request payload")
	}
	if rec, ok := c.Expect["record"].(map[string]any); ok {
		checkNamedTester(rec["payload"], "expect.record.payload")
	}

	// Hash chain: within each job journal, every record's previous_hash
	// must equal the record_hash of the prior record of that job
	// (genesis links to 64 zeroes). Reordering or rebuilding a record
	// without relinking breaks the journal a server would replay.
	zero := "0000000000000000000000000000000000000000000000000000000000000000"
	tail := map[string]string{}
	checkLink := func(rec map[string]any, i int, where string) {
		prev, hasPrev := rec["previous_hash"].(string)
		hash, _ := rec["record_hash"].(string)
		jid, _ := rec["job_id"].(string)
		if !hasPrev {
			return
		}
		want, ok := tail[jid]
		if !ok {
			want = zero
		}
		if prev != want {
			t.Errorf("%s: %s record %d (job=%s seq=%v) "+
				"previous_hash %.12s; want %.12s",
				ctx, where, i, jid, rec["seq"], prev, want)
		}
		if hash != "" {
			tail[jid] = hash
		}
	}
	for i, r := range c.Given.Records {
		checkLink(r, i, "given")
	}
	if rec, ok := c.Expect["record"].(map[string]any); ok {
		checkLink(rec, -1, "expect")
	}

	reqType, _ := c.Request["type"].(string)
	switch reqType {
	case "http", "http_raw":
		method, _ := c.Request["method"].(string)
		if method == "POST" {
			body, ok := c.Request["body"].(map[string]any)
			if !ok {
				t.Fatalf("%s: POST request has no object body", ctx)
			}
			err := reqSchema.Validate(body)
			expErr, _ := c.Expect["error"].(string)
			if err != nil && expErr != "invalid_request" {
				t.Fatalf("%s: request body fails $defs/Request but "+
					"expected error is %q, not invalid_request: %v",
					ctx, expErr, err)
			}
		}
		// Expected appended record's payload must match Payload_kind.
		if rec, ok := c.Expect["record"].(map[string]any); ok {
			kind, _ := rec["kind"].(string)
			if payload, ok := rec["payload"]; ok && kind != "" {
				validateDef(t, p, "Payload_"+kind, payload,
					ctx+" expect.record.payload")
			}
		}
	case "cli":
		argv, _ := c.Request["argv"].([]any)
		if len(argv) == 0 {
			t.Fatalf("%s: cli case has empty argv", ctx)
		}
		if code, _ := c.Expect["stderr_code"].(string); code != "" {
			checkExit(t, p, ctx, code, c.Expect["exit_code"])
		}
	case "legacy", "meta", "none", "policy":
		// non-request expectations: only error/status checks apply
	default:
		t.Fatalf("%s: unknown request type %q", ctx, reqType)
	}

	// Expected error code must exist in the Error enum, the CLI exit
	// map, and the design status map.
	if code, _ := c.Expect["error"].(string); code != "" {
		if !p.errorEnum[code] {
			t.Errorf("%s: expected error %q not in Error enum",
				ctx, code)
		}
		if _, ok := p.cliExit[code]; !ok {
			t.Errorf("%s: expected error %q not in cli.json exit map",
				ctx, code)
		}
		if status, _ := c.Expect["http_status"].(float64); status != 0 {
			if errorStatuses[code] != int(status) {
				t.Errorf("%s: error %q at status %v, design map "+
					"says %d", ctx, code, status,
					errorStatuses[code])
			}
		}
	}
	// RecordsResponse-ish expectations keep their declared fields.
	if rr, ok := c.Expect["records_response"].(map[string]any); ok {
		for _, k := range []string{"snapshot_seq", "next_after_seq",
			"has_more"} {
			if _, ok := rr[k]; !ok {
				t.Errorf("%s: records_response missing %s", ctx, k)
			}
		}
	}
	if sc, ok := c.Expect["server_after_freeze"].(map[string]any); ok {
		if code, _ := sc["error"].(string); code != "" &&
			!p.errorEnum[code] {
			t.Errorf("%s: server_after_freeze error %q not in "+
				"Error enum", ctx, code)
		}
	}
	if loser, ok := c.Expect["loser_expect"].(map[string]any); ok {
		if code, _ := loser["error"].(string); code != "" &&
			!p.errorEnum[code] {
			t.Errorf("%s: loser_expect error %q not in Error enum",
				ctx, code)
		}
	}
}

func checkExit(t *testing.T, p *pack, ctx, code string,
	exitAny any) {
	t.Helper()
	want, ok := p.cliExit[code]
	if !ok {
		t.Errorf("%s: stderr code %q not in cli.json exit map", ctx, code)
		return
	}
	// Cross-check against the hardcoded design map.
	if exitMap[code] != want {
		t.Errorf("%s: cli.json maps %q to %d, design table says %d",
			ctx, code, want, exitMap[code])
	}
	if got, _ := exitAny.(float64); int(got) != want {
		t.Errorf("%s: expected exit %v, design map says %d for %q",
			ctx, exitAny, want, code)
	}
}
