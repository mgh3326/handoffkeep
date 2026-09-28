package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

func planeTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for Plane store tests")
	}
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// planeIsolatedStore gives a test its own schema. ProcessNextPlaneOutbox
// selects the oldest pending row database-wide, so outbox-processing tests
// cannot share the test database with other plane tests or leftovers.
func planeIsolatedStore(t *testing.T) *Store {
	t.Helper()
	baseURL := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if baseURL == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for Plane store tests")
	}
	admin, err := pgx.Connect(t.Context(), baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	schema := fmt.Sprintf("plane_store_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	st, err := Open(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func planeTask(t *testing.T, st *Store, title string) Task {
	t.Helper()
	task, err := st.CreateTask(t.Context(), Task{
		Lane:      fmt.Sprintf("plane-%d", time.Now().UnixNano()),
		Title:     title,
		Kind:      "implement",
		Priority:  2,
		CreatedBy: "plane-test",
		Project:   projectPtr(testProjectName),
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// TestPlaneStateDerivationReadsSoleTable proves every hk state resolves
// through LinearTaskStateMapping alone: Mutate:false states emit no Plane
// state, terminal states stay terminal, and nothing undeclared slips through.
func TestPlaneStateDerivationReadsSoleTable(t *testing.T) {
	for state := range taskStates {
		name, mutate, err := PlaneStateForTask(state)
		mapping := LinearTaskStateMapping[state]
		if err != nil {
			t.Fatalf("state %s errored though it is declared: %v", state, err)
		}
		if !mapping.Mutate {
			if mutate || name != "" {
				t.Fatalf("state %s is Mutate:false but derived name=%q mutate=%t", state, name, mutate)
			}
			continue
		}
		if !mutate || name == "" {
			t.Fatalf("state %s derived empty: name=%q mutate=%t", state, name, mutate)
		}
		if mapping.Terminal && name != "Done" && name != "Cancelled" {
			t.Fatalf("terminal state %s derived non-terminal Plane name %q", state, name)
		}
	}
	if name, mutate, err := PlaneStateForTask("join"); err != nil || !mutate || name != "In Progress" {
		t.Fatalf("join must derive to In Progress via In Review: %q %t %v", name, mutate, err)
	}
	if _, _, err := PlaneStateForTask("not_a_state"); !errors.Is(err, ErrPlaneStateUndeclared) {
		t.Fatalf("undeclared state must fail closed: %v", err)
	}
}

func TestPlanePriorityForBoundaries(t *testing.T) {
	for input, want := range map[int]string{
		-1: "none", 0: "none", 1: "low", 2: "medium", 3: "high", 4: "urgent", 9: "urgent",
	} {
		if got := PlanePriorityFor(input); got != want {
			t.Errorf("priority %d -> %q, want %q", input, got, want)
		}
	}
}

// TestPlaneTitleRedaction covers the withheld classes plus bypass attempts a
// hostile or careless title could use: mixed case flags, CIDR-suffixed IPs,
// Windows paths, token prefixes, and injection fragments.
func TestPlaneTitleRedaction(t *testing.T) {
	sensitive := map[string]string{
		"ipv4":              "db unreachable at 10.0.0.5",
		"ipv4_cidr":         "allow 192.168.0.0/16 in the secgroup",
		"ipv4_port":         "service down on 172.16.4.2:8443",
		"ipv6":              "fe80::1 is flapping",
		"url":               "repro via https://10.0.0.5/x.sh",
		"internal_host":     "db01.corp.internal timed out",
		"prod_host":         "api.prod.example.com is 500ing",
		"email":             "notify alice@example.com on failure",
		"abs_path":          "leak in /home/ops/.ssh/id_rsa",
		"etc_path":          "bad line in /etc/shadow",
		"tmp_path":          "payload staged at /tmp/x.bin",
		"windows_path":      `config at C:\Users\ops\key.pem`,
		"tilde_ssh":         "keys under ~/.ssh/",
		"dotfile_env":       "app reads .env at boot",
		"dotfile_env_prod":  "leaked .env.production",
		"key_filename":      "found id_ed25519 on disk",
		"secret_file":       "secrets.yml committed",
		"credentials":       "rotate the aws credentials",
		"pem_ext":           "rotate cert.pem",
		"kubeconfig":        "old admin.kubeconfig in the repo",
		"assign_token":      "github_token=abc123",
		"assign_secret":     "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY",
		"assign_api_key":    "api_key: sk-live-9",
		"assign_ssh_key":    "ssh_key=AAAAB3",
		"authorization":     "Authorization: Bearer token123",
		"pem_block":         "-----BEGIN RSA PRIVATE KEY-----",
		"aws_key":           "AKIAIOSFODNN7EXAMPLE in logs",
		"gh_token":          "ghp_abcdefghijklmnop1234 leaked",
		"slack_token":       "xoxb-123456789012-abcdefghijkl",
		"jwt":               "eyJhbGciOiJIUzI1NiIs.eyJzdWIiOiIxMjM0NTY3ODkwIn0.sig",
		"ssh_pubkey":        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockkeymaterial hera",
		"pipe_shell":        "fix via curl x.sh | sh",
		"pipe_sudo":         "run get.sh | sudo bash",
		"curl_pipe":         "wget https://x.example/y | python3",
		"destructive":       "accidentally ran rm -rf /data",
		"destructive_upper": "oops RM -Rf /tmp/stage",
		"sudo":              "sudo chmod 777 everything",
		"sudo_upper":        "SUDO apt install",
		"substitution":      "payload $(cat /etc/passwd)",
		"backtick":          "runs `id` on boot",
		"script_tag":        "<script>alert(1)</script> in title",
		"javascript_url":    "open javascript:alert(1)",
		"sqli_union":        "inject ' UNION SELECT password FROM users",
		"sqli_drop":         "x'; DROP TABLE tasks--",
		"sqli_or":           "login ' OR 1=1 --",
		"chmod":             "chmod 0666 /etc/passwd",
	}
	for name, title := range sensitive {
		if !PlaneTitleIsSensitive(title) {
			t.Errorf("%s: %q should be withheld", name, title)
		}
	}
	benign := []string{
		"Mirror connector contract for the outbox",
		"fix regression in internal/store/plane.go",
		"docs: update readme section",
		"token bucket rate limiter is too strict",
		"decide between name and key params in the config spec",
		"review docs/plane-mirror.md",
		"env-based config loading",
		"version 1.2.3 release",
		"search bar: the select statement is slow",
		"bash quoting bug in runner",
	}
	for _, title := range benign {
		if PlaneTitleIsSensitive(title) {
			t.Errorf("benign title %q was withheld", title)
		}
	}
	task := Task{ID: 42, Title: "leak /home/ops/.ssh/id_rsa", Kind: "fix"}
	got := PlaneTaskTitle(task)
	if strings.Contains(got, "id_rsa") || strings.Contains(got, "/home/") {
		t.Fatalf("withheld title leaks content: %q", got)
	}
	if !strings.Contains(got, "hk:task/42") || !strings.Contains(got, "fix") {
		t.Fatalf("withheld title lost the reference or kind: %q", got)
	}
}

func TestPlaneTitleTruncatesRunes(t *testing.T) {
	long := strings.Repeat("가", PlaneTitleMaxRunes+20)
	got := PlaneTaskTitle(Task{Title: long})
	if utf8.RuneCountInString(got) > PlaneTitleMaxRunes {
		t.Fatalf("truncated title is %d runes", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated title lacks ellipsis: %q", got[len(got)-10:])
	}
	short := "  padded title  "
	if got := PlaneTaskTitle(Task{Title: short}); got != "padded title" {
		t.Fatalf("title not trimmed: %q", got)
	}
}

// TestPlanePayloadAllowlistIsStructural marshals a payload built from a task
// carrying every forbidden field and proves the wire shape can only contain
// the allowlisted keys — there is no struct field for notes, refs, report
// paths, or bodies to leak through.
func TestPlanePayloadAllowlistIsStructural(t *testing.T) {
	task := Task{
		ID:       7,
		State:    "in_progress",
		Lane:     "lane-x",
		Title:    "Mirror connector contract",
		Kind:     "implement",
		Priority: 3,
		BodyDoc:  "docs/secret-body",
		Project:  projectPtr("experiment"),
		Refs: TaskRefs{
			Linear: &TaskLinear{
				Brief: "brief/leaky", Report: "report/leaky",
				Decision: "decision/leaky", DeploySHA: "deadbeef",
				Labels: []string{"internal-only"},
			},
		},
	}
	payload, err := PlaneProjectionFor(task)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	want := []string{"external_id", "kind", "lane", "name", "priority", "project", "state"}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("payload keys %v, want exactly %v", got, want)
	}
	for _, forbidden := range []string{"report", "brief", "decision", "notes", "refs", "description", "body", "leaky", "internal-only", "deadbeef", "secret-body"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("payload leaks %q: %s", forbidden, raw)
		}
	}
}

func TestPlaneProjectionNeedsDecisionKeepsStateEmpty(t *testing.T) {
	payload, err := PlaneProjectionFor(Task{ID: 3, State: "needs_decision", Lane: "l", Title: "t", Kind: "decide"})
	if err != nil {
		t.Fatal(err)
	}
	if payload.State != "" {
		t.Fatalf("needs_decision must not set a remote state: %+v", payload)
	}
}

func TestPlaneOutboxLifecycle(t *testing.T) {
	st := planeTestStore(t)
	st.EnablePlaneSync()
	task := planeTask(t, st, "Mirror connector contract")
	rows, err := st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 1 || rows[0].Op != PlaneOpWorkItemCreate || rows[0].State != "pending" {
		t.Fatalf("create outbox=%+v err=%v", rows, err)
	}
	if rows[0].Payload.ExternalID != PlaneExternalID(task.ID) || rows[0].Payload.State != "Backlog" {
		t.Fatalf("create payload=%+v", rows[0].Payload)
	}
	if _, err = st.ClaimTask(t.Context(), task.ID, "builder", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionTask(t.Context(), task.ID, "in_progress", "builder", "started", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.RelaneTask(t.Context(), task.ID, "plane-relaned", "builder", "move", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.SetTaskProject(t.Context(), task.ID, "plane-pilot", "builder", "reclassify"); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 5 {
		t.Fatalf("lifecycle rows=%+v err=%v", rows, err)
	}
	for i, want := range []string{PlaneOpWorkItemCreate, PlaneOpWorkItemUpdate, PlaneOpWorkItemUpdate, PlaneOpWorkItemUpdate, PlaneOpWorkItemUpdate} {
		if rows[i].Op != want || rows[i].Seq != i+1 {
			t.Fatalf("row %d op=%s seq=%d want %s", i, rows[i].Op, rows[i].Seq, want)
		}
	}
	if rows[4].Payload.Project != "plane-pilot" {
		t.Fatalf("project update payload=%+v", rows[4].Payload)
	}
	if _, err = st.TransitionTask(t.Context(), task.ID, "dropped", "builder", "abandoned", nil, ""); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 6 || rows[5].Op != PlaneOpWorkItemRemove {
		t.Fatalf("terminal rows=%+v err=%v", rows, err)
	}
	if rows[5].Payload.ExternalID != PlaneExternalID(task.ID) {
		t.Fatalf("remove payload missing external id: %+v", rows[5].Payload)
	}
}

// TestPlaneFieldUpdateOnTerminalIsSilent proves a relane or reclassify on an
// already-terminal task enqueues nothing — the projection is non-terminal
// only and a metadata edit must not resurrect the item on the board.
func TestPlaneFieldUpdateOnTerminalIsSilent(t *testing.T) {
	st := planeTestStore(t)
	st.EnablePlaneSync()
	task := planeTask(t, st, "Mirror connector contract")
	if _, err := st.ClaimTask(t.Context(), task.ID, "builder", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(t.Context(), task.ID, "dropped", "builder", "abandoned", nil, ""); err != nil {
		t.Fatal(err)
	}
	before, err := st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetTaskProject(t.Context(), task.ID, "plane-pilot", "builder", "reclassify"); err != nil {
		t.Fatal(err)
	}
	after, err := st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("terminal field update enqueued: before=%d after=%d", len(before), len(after))
	}
}

func TestPlaneOptOutWritesNoOutbox(t *testing.T) {
	st := planeTestStore(t)
	task := planeTask(t, st, "Mirror connector contract")
	if _, err := st.ClaimTask(t.Context(), task.ID, "builder", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(t.Context(), task.ID, "dropped", "builder", "abandoned", nil, ""); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("opt-out rows=%+v err=%v", rows, err)
	}
}

// TestPlaneOutboxSharesTaskTransaction forces an outbox insert failure to
// prove the task mutation rolls back with it — a mirrored task can never
// commit without its outbox row.
func TestPlaneOutboxSharesTaskTransaction(t *testing.T) {
	st := planeTestStore(t)
	st.EnablePlaneSync()
	task := planeTask(t, st, "Mirror connector contract")
	if _, err := st.ClaimTask(t.Context(), task.ID, "builder", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	before, found, err := st.GetTask(t.Context(), task.ID)
	if err != nil || !found {
		t.Fatalf("before found=%t err=%v", found, err)
	}
	connection, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	functionName := fmt.Sprintf("fail_plane_outbox_%d", task.ID)
	triggerName := fmt.Sprintf("fail_plane_outbox_trigger_%d", task.ID)
	if _, err = connection.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced plane outbox failure'; END; $$`, functionName)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+functionName+"() CASCADE")
	}()
	if _, err = connection.Exec(t.Context(), fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON plane_outbox FOR EACH ROW WHEN (NEW.task_id=%d) EXECUTE FUNCTION %s()`, triggerName, task.ID, functionName)); err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionTask(t.Context(), task.ID, "in_progress", "builder", "must roll back", nil, ""); err == nil || !strings.Contains(err.Error(), "forced plane outbox failure") {
		t.Fatalf("forced transition error=%v", err)
	}
	after, found, err := st.GetTask(t.Context(), task.ID)
	if err != nil || !found {
		t.Fatalf("after found=%t err=%v", found, err)
	}
	if after.State != before.State {
		t.Fatalf("task state escaped rollback: %s -> %s", before.State, after.State)
	}
	if _, err = connection.Exec(t.Context(), "DROP TRIGGER "+triggerName+" ON plane_outbox"); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(t.Context(), "DROP FUNCTION "+functionName+"()"); err != nil {
		t.Fatal(err)
	}
}

func TestPlaneIssueLinkRoundTrip(t *testing.T) {
	st := planeIsolatedStore(t)
	st.EnablePlaneSync()
	task := planeTask(t, st, "Mirror connector contract")
	if _, found, err := st.GetPlaneIssue(t.Context(), task.ID); err != nil || found {
		t.Fatalf("unexpected link found=%t err=%v", found, err)
	}
	processed, err := st.ProcessNextPlaneOutbox(t.Context(), func(ctx context.Context, item PlaneOutbox) (PlaneOutboxResult, error) {
		return PlaneOutboxResult{State: "sent", RemoteID: "wi-1", ProjectID: "proj-1"}, nil
	})
	if err != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, err)
	}
	link, found, err := st.GetPlaneIssue(t.Context(), task.ID)
	if err != nil || !found || link.WorkItemID != "wi-1" || link.ProjectID != "proj-1" {
		t.Fatalf("link=%+v found=%t err=%v", link, found, err)
	}
	links, err := st.ListPlaneIssues(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, l := range links {
		if l.TaskID == task.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("ListPlaneIssues missing task %d", task.ID)
	}
	// A remove result unlinks the row in the same commit.
	if err = st.EnqueuePlaneUpdate(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	processed, err = st.ProcessNextPlaneOutbox(t.Context(), func(ctx context.Context, item PlaneOutbox) (PlaneOutboxResult, error) {
		return PlaneOutboxResult{State: "sent", Unlink: true}, nil
	})
	if err != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, err)
	}
	if _, found, err = st.GetPlaneIssue(t.Context(), task.ID); err != nil || found {
		t.Fatalf("link still present found=%t err=%v", found, err)
	}
}

func TestPlaneEnqueueUpdateReprojects(t *testing.T) {
	st := planeTestStore(t)
	st.EnablePlaneSync()
	task := planeTask(t, st, "Mirror connector contract")
	if err := st.EnqueuePlaneUpdate(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 2 || rows[1].Op != PlaneOpWorkItemUpdate {
		t.Fatalf("reproject rows=%+v err=%v", rows, err)
	}
	if err := st.EnqueuePlaneUpdate(t.Context(), 999999999); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task err=%v", err)
	}
}
