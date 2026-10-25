package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/wrk"
)

func runWrkFor(t *testing.T, client remote.Client, args []string, stdin string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	err := runWrk(args, wrkIO{stdin: strings.NewReader(stdin), out: &out, errout: &errb}, client)
	code := 0
	if err != nil {
		we, ok := err.(wrkExit)
		if !ok {
			t.Fatalf("runWrk returned %v", err)
		}
		code = we.code
	}
	return code, out.String(), errb.String()
}

// Mutant "follow one redirect in the wrk CLI client": every redirect is
// refused, same host included, and evidence is never resent.
func TestWrkCLIRefusesEveryRedirect(t *testing.T) {
	var hits, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method == "POST" {
			posts.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/v1/wrk/") && !strings.HasPrefix(r.URL.Path, "/v1/wrk/moved") {
			http.Redirect(w, r, "/v1/wrk/moved"+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"contract":"hk1873.v1"}`))
	}))
	defer srv.Close()
	client := remote.Client{URL: srv.URL, Token: "tok"}
	code, out, errout := runWrkFor(t, client, []string{"capabilities"}, "")
	if code != 75 || out != "" || errout != "handoffkeep wrk: protocol_invalid\n" || hits.Load() != 1 {
		t.Fatalf("GET redirect: exit=%d out=%q err=%q hits=%d", code, out, errout, hits.Load())
	}
	payload := `{"head":"` + strings.Repeat("1", 40) + `","tester_job_id":null,"extension_grant_seq":null,"repeat_grant_seq":null}`
	code, out, errout = runWrkFor(t, client, []string{"append", "job-a", "--kind", "round_open", "--idempotency-key", wrk.NewUUID(), "--expected-seq", "2", "--claim-seq", "1"}, payload)
	if code != 75 || out != "" || errout != "handoffkeep wrk: protocol_invalid\n" || posts.Load() != 1 || hits.Load() != 2 {
		t.Fatalf("POST redirect: exit=%d err=%q posts=%d hits=%d", code, errout, posts.Load(), hits.Load())
	}
}

func TestWrkCLIArgumentsAndStdin(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	client := remote.Client{URL: srv.URL, Token: "tok"}
	key := wrk.NewUUID()
	for _, args := range [][]string{
		nil,
		{"frobnicate"},
		{"capabilities", "extra"},
		{"job"},
		{"job", "Job-Upper"},
		{"job", "job-a", "--url", "https://elsewhere.invalid"},
		{"job", "job-a", "--token", "x"},
		{"records", "job-a", "--limit", "5", "--limit", "6"},
		{"records", "job-a", "--after-seq", "-1"},
		{"records", "job-a", "--after-seq", "01"},
		{"content", "job-a", "0"},
		{"append", "job-a", "--kind", "round_open", "--idempotency-key", key, "--expected-seq", "1"},
		{"append", "job-a", "--kind", "promote", "--idempotency-key", key, "--expected-seq", "1", "--claim-seq", "1"},
		{"append", "job-a", "--kind", "round_open", "--idempotency-key", "not-a-uuid", "--expected-seq", "1", "--claim-seq", "1"},
		{"append", "job-a", "job-b", "--kind", "round_open", "--idempotency-key", key, "--expected-seq", "1", "--claim-seq", "1"},
	} {
		code, out, errout := runWrkFor(t, client, args, "")
		if code != 2 || out != "" || errout != "handoffkeep wrk: cli_usage\n" {
			t.Errorf("%v: exit=%d out=%q err=%q", args, code, out, errout)
		}
	}
	appendArgs := []string{"append", "job-a", "--kind", "round_open", "--idempotency-key", key, "--expected-seq", "1", "--claim-seq", "1"}
	for name, stdin := range map[string]string{
		"empty":         "",
		"array":         "[]",
		"duplicate key": `{"head":"` + strings.Repeat("1", 40) + `","head":"` + strings.Repeat("1", 40) + `","tester_job_id":null,"extension_grant_seq":null,"repeat_grant_seq":null}`,
		"unknown field": `{"head":"` + strings.Repeat("1", 40) + `","tester_job_id":null,"extension_grant_seq":null,"repeat_grant_seq":null,"x":1}`,
		"trailing data": `{"head":"` + strings.Repeat("1", 40) + `","tester_job_id":null,"extension_grant_seq":null,"repeat_grant_seq":null} {}`,
	} {
		if code, _, errout := runWrkFor(t, client, appendArgs, stdin); code != 65 || errout != "handoffkeep wrk: invalid_request\n" {
			t.Errorf("stdin %s: exit=%d err=%q", name, code, errout)
		}
	}
	if code, _, errout := runWrkFor(t, client, []string{"job", "job-a"}, "{}"); code != 65 || errout != "handoffkeep wrk: invalid_request\n" {
		t.Errorf("read with stdin: exit=%d err=%q", code, errout)
	}
	if hits.Load() != 0 {
		t.Fatalf("a refused invocation reached the server %d times", hits.Load())
	}
	if code, _, errout := runWrkFor(t, remote.Client{}, []string{"capabilities"}, ""); code != 75 || errout != "handoffkeep wrk: transport_unavailable\n" {
		t.Errorf("no installed config: exit=%d err=%q", code, errout)
	}
}

// Error hygiene: a server reply that is not exactly a stable error body is
// protocol_invalid, and no server text reaches stderr or stdout.
func TestWrkCLIErrorHygiene(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		ct     string
		body   string
		code   string
	}{
		"stable code":            {409, "application/json", `{"error":"round_cap"}`, "round_cap"},
		"wrong status for code":  {400, "application/json", `{"error":"round_cap"}`, "protocol_invalid"},
		"extra field":            {409, "application/json", `{"error":"round_cap","detail":"/home/x/secret"}`, "protocol_invalid"},
		"unknown code":           {409, "application/json", `{"error":"bogus"}`, "protocol_invalid"},
		"plain 404":              {404, "text/plain", "404 page not found", "o3_unsupported"},
		"other contract":         {200, "application/json", `{"contract":"hk1873.v2"}`, "o3_unsupported"},
		"unavailable":            {503, "application/json", `{"error":"wrk_unavailable"}`, "wrk_unavailable"},
		"schema-invalid success": {200, "application/json", `{"contract":"hk1873.v1"}`, "protocol_invalid"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", tc.ct)
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		code, out, errout := runWrkFor(t, remote.Client{URL: srv.URL, Token: "tok"}, []string{"capabilities"}, "")
		srv.Close()
		want, _ := wrk.Exit(wrk.Code(tc.code))
		if code != want || out != "" || errout != "handoffkeep wrk: "+tc.code+"\n" {
			t.Errorf("%s: exit=%d out=%q err=%q", name, code, out, errout)
		}
	}
}

func TestWrkCLIExitMapMatchesPack(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(wrkPackRoot, "cli.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cli struct {
		ExitMap []struct {
			Codes []string `json:"codes"`
			Exit  int      `json:"exit"`
		} `json:"exit_map"`
	}
	if err := json.Unmarshal(b, &cli); err != nil {
		t.Fatal(err)
	}
	for _, m := range cli.ExitMap {
		for _, code := range m.Codes {
			switch code {
			case "intent_invalid", "intent_io", "history_regressed":
				continue // C1-local codes; the S1 CLI never emits them
			}
			if got, ok := wrk.Exit(wrk.Code(code)); !ok || got != m.Exit {
				t.Errorf("exit(%s) = %d %v, pack says %d", code, got, ok, m.Exit)
			}
		}
	}
}

// Startup (director-1 off-mode ruling, four cases) and the enabled-mode
// refusals. Cases needing PostgreSQL run in CI only.
func TestWrkStartupCases(t *testing.T) {
	base := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if base == "" {
		if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatal("CI must provide HANDOFFKEEP_TEST_DB_URL")
		}
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for startup tests (CI only)")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	nano := time.Now().UnixNano()
	schema, role := fmt.Sprintf("wrks_%d", nano), fmt.Sprintf("wrksrt_%d", nano)
	password := cliToken(t)
	for _, q := range []string{`CREATE SCHEMA ` + schema, `CREATE ROLE ` + role + ` LOGIN PASSWORD '` + password + `'`, `GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_, _ = admin.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = admin.Exec(context.Background(), `DROP ROLE `+role)
	})
	ownerURL := schemaURL(t, base, schema, "", "")
	runtimeURL := schemaURL(t, base, schema, role, password)

	// Case 1: off mode, no migrate URL, no journal: legacy migrations only.
	st, err := openServeStore(ctx, ownerURL, "")
	if err != nil {
		t.Fatalf("case 1: %v", err)
	}
	if st.HasWrkTables() {
		t.Fatal("case 1: a single-URL startup created O3 tables")
	}
	if cfg, err := wrkFromEnv(ctx, api.WrkOff, st); err != nil || cfg.Mode != api.WrkOff {
		t.Fatalf("case 1 off config: %v", err)
	}
	st.Close()
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name LIKE 'wrk_%')`, schema).Scan(&exists); err != nil || exists {
		t.Fatalf("case 1: wrk tables exist=%v err=%v", exists, err)
	}
	if _, err := admin.Exec(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA `+schema+` TO `+role); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA `+schema+` TO `+role); err != nil {
		t.Fatal(err)
	}

	// Case 2: the O3 schema comes only from the separate owner URL; the
	// runtime role passes the role check.
	st, err = openServeStore(ctx, runtimeURL, ownerURL)
	if err != nil {
		t.Fatalf("case 2: %v", err)
	}
	if !st.HasWrkTables() {
		t.Fatal("case 2: owner startup did not create the journal")
	}
	if _, err := openServeStore(ctx, runtimeURL, runtimeURL); err == nil {
		t.Fatal("case 2: an owner URL equal to the runtime URL was accepted")
	}

	// Case 4: enabled mode needs the owner path, a store generation and a
	// valid policy, and never falls back to a single URL.
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyFile, packPolicy(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HANDOFFKEEP_WRK_POLICY_FILE", policyFile)
	t.Setenv("WRK_STORE_GENERATION", "00000000-0000-4000-8000-ffff00000001")
	t.Setenv("HANDOFFKEEP_DB_MIGRATE_URL", "")
	if _, err := wrkFromEnv(ctx, api.WrkEnforce, st); err == nil {
		t.Fatal("case 4: enabled without HANDOFFKEEP_DB_MIGRATE_URL")
	}
	t.Setenv("HANDOFFKEEP_DB_MIGRATE_URL", ownerURL)
	if _, err := wrkFromEnv(ctx, api.WrkEnforce, st); err != nil {
		t.Fatalf("case 4 valid: %v", err)
	}
	t.Setenv("WRK_STORE_GENERATION", "")
	if _, err := wrkFromEnv(ctx, api.WrkShadow, st); err == nil {
		t.Fatal("case 4: enabled without a store generation")
	}
	t.Setenv("WRK_STORE_GENERATION", "00000000-0000-4000-8000-ffff00000001")
	if err := os.WriteFile(policyFile, []byte(`{"version":"hk1873.policy.v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := wrkFromEnv(ctx, api.WrkEnforce, st); err == nil {
		t.Fatal("case 4: enabled with an invalid policy")
	}
	st.Close()

	// Case 3: once the journal exists, a startup in any mode on a role that
	// owns it (here the superuser owner) is refused.
	if st, err := openServeStore(ctx, ownerURL, ""); err == nil {
		st.Close()
		t.Fatal("case 3: off-mode startup on the journal owner role was accepted")
	}
}

func schemaURL(t *testing.T, base, schema, user, password string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	return u.String()
}
