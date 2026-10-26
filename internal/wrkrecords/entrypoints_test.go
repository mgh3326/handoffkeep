package wrkrecords_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	hkmcp "github.com/mgh3326/handoffkeep/internal/mcp"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ui"
	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func activeFixture(t *testing.T) vector {
	t.Helper()
	for _, v := range loadVectors(t) {
		if v.file == "transition.json" && v.Name == "open-h-round-1" {
			return v
		}
	}
	t.Fatal("active fixture missing")
	return vector{}
}
func TestWrkLegacyDirectStoreAndServiceFences(t *testing.T) {
	db := contractStore(t)
	v := activeFixture(t)
	db.seed(t, v, contentCatalog(t, loadVectors(t)))
	ctx := store.WithWrkPrincipal(t.Context(), "desktop", nil)
	svc := api.Service{Store: db.runtime}
	project := "experiment"
	checks := map[string]func() error{
		"create-associated": func() error {
			_, e := db.runtime.CreateTask(ctx, store.Task{Lane: "director-1", Title: "associated", Kind: "implement", CreatedBy: "director-1", Project: &project, Refs: store.TaskRefs{JobID: "job-a"}})
			return e
		},
		"claim": func() error { _, e := db.runtime.ClaimTask(ctx, 2400, "director-1", "forged", ""); return e },
		"rebind": func() error {
			_, e := db.runtime.RebindTaskJob(ctx, 2400, "job-a", "forged", "director-1", "test")
			return e
		},
		"transition-binding": func() error {
			_, e := db.runtime.TransitionTask(ctx, 2400, "claimed", "director-1", "test", &store.TaskRefs{JobID: "forged"}, "")
			return e
		},
		"transition-state": func() error {
			_, e := db.runtime.TransitionTask(ctx, 2400, "dropped", "director-1", "test", nil, "")
			return e
		},
		"project": func() error {
			_, _, e := db.runtime.SetTaskProject(ctx, 2400, "experiment", "director-1", "test")
			return e
		},
		"relane": func() error {
			_, _, e := db.runtime.RelaneTask(ctx, 2400, "other", "director-1", "test", true)
			return e
		},
		"decision-resolve": func() error {
			_, e := db.runtime.ResolveDecisionRequest(ctx, 2400, "director-1", store.DecisionResolveInput{RequestID: "dr-2400-1", Kind: "answered", Option: "A"})
			return e
		},
		"assistant-resolve": func() error {
			_, e := db.runtime.ResolveDecisionRequestAssistant(ctx, 2400, "director-1", store.DecisionAssistantResolveInput{RequestID: "dr-2400-1", Option: "A"})
			return e
		},
		"disposition-apply": func() error { _, e := db.runtime.ApplyDisposition(ctx, 2400, "director-1", "test"); return e },
		"disposition-answer": func() error {
			_, e := db.runtime.AnswerDisposition(ctx, store.DispositionAnswerInput{ID: 2400, Gen: 1, Key: "A", OperatorEmail: "admin@example.com", EventID: "fixture"})
			return e
		},
		"disposition-batch": func() error {
			_, _, e := db.runtime.AnswerDispositionBatch(ctx, []store.DispositionRef{{ID: 2400, Gen: 1}}, "admin@example.com", "fixture", func(string) string { return "fixture" })
			return e
		},
		"service-decision": func() error {
			_, e := svc.ResolveDecision(ctx, api.DecisionResolveInput{Type: "task", ID: 2400, By: "director-1", Answer: "A"})
			return e
		},
		"service-relay": func() error {
			_, _, e := svc.AppendRelayEvent(ctx, store.RelayEvent{Kind: "job.completed", JobID: "job-a", OwnerLane: "b2400-a"})
			return e
		},
		"store-relay": func() error {
			_, _, e := db.runtime.AppendRelayEvent(ctx, store.RelayEvent{Kind: "job.joined", JobID: "job-a", OwnerLane: "b2400-a"})
			return e
		},
	}
	for name, call := range checks {
		t.Run(name, func(t *testing.T) {
			if e := call(); Code(e) != "wrk_receipt_required" {
				t.Fatalf("fence returned %v", e)
			}
		})
	}
	var events int
	if e := db.owner.QueryRow(t.Context(), `SELECT count(*) FROM relay_events`).Scan(&events); e != nil || events != 0 {
		t.Fatal("denied service emitted a relay")
	}
	original, e := db.runtime.WrkJob(t.Context(), "director-1", "job-a")
	if e != nil {
		t.Fatal(e)
	}
	receipt := original.Claim.Receipt()
	central := store.WithWrkPrincipal(t.Context(), "director-1", &receipt)
	if _, e = db.runtime.TransitionTask(central, 2400, "claimed", "director-1", "valid claim receipt", nil, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = db.runtime.TransitionTask(central, 2400, "in_progress", "director-1", "valid claim receipt", nil, ""); e != nil {
		t.Fatal(e)
	}
	// Off retains the same durable fences. An unrelated legacy task still uses
	// its existing claim/no-job path, including a caller-supplied By label.
	engine := db.runtime.WrkEngine()
	engine.Mode = "off"
	if e = db.runtime.ConfigureWrkEngine(t.Context(), engine); e != nil {
		t.Fatal(e)
	}
	if _, e = db.runtime.TransitionTask(ctx, 2400, "dropped", "director-1", "off bypass", nil, ""); Code(e) != "wrk_receipt_required" {
		t.Fatalf("off fence: %v", e)
	}
	if _, e = db.runtime.ClaimTask(ctx, 2401, "legacy-user", "unrelated-legacy-job", ""); e != nil {
		t.Fatalf("unrelated legacy claim changed: %v", e)
	}
}

func appendDB(t *testing.T, db contractDB, actor, job, kind string, p Object) Record {
	t.Helper()
	claim, tail := int64(0), int64(0)
	if old, e := db.runtime.WrkJob(t.Context(), actor, job); e == nil {
		claim, tail = old.Summary.ClaimSeq, old.Summary.SnapshotSeq
	}
	out, e := db.runtime.WrkAppend(t.Context(), actor, job, NewUUID(), Request{Kind: kind, ClaimSeq: claim, ExpectedSeq: tail, Payload: p})
	if e != nil {
		t.Fatalf("%s/%s: %v", job, kind, e)
	}
	return out.Record
}
func TestWrkCanonicalProjectionAndSubjectReceipt(t *testing.T) {
	db := contractStore(t)
	db.seed(t, activeFixture(t), contentCatalog(t, loadVectors(t)))
	appendDB(t, db, "director-1", "reviewer", "claim", Object{"job_id": "reviewer", "owner_lane": "b2400-a", "parent_lane": "b2400-a", "task_id": 2400, "host_client_id": "desktop", "role": "tester", "tier": "T2", "review_of": Object{"job_id": "job-a", "round_no": 1}, "predecessor_job_id": nil, "supersedes": nil})
	appendDB(t, db, "desktop", "job-a", "round_open", opening(h1, "reviewer", nil, nil))
	appendDB(t, db, "desktop", "job-a", "verdict", evidence(1, h1, "/projection/verdict", "fresh PASS", "reviewer"))
	ended := appendDB(t, db, "desktop", "reviewer", "end", Object{"end_type": "reaped", "reason_code": "cleanup"})
	wrong := ended.Receipt()
	wrongCtx := store.WithWrkPrincipal(t.Context(), "director-1", &wrong)
	if _, e := db.runtime.TransitionTask(wrongCtx, 2400, "dropped", "director-1", "wrong reviewer receipt", nil, ""); Code(e) != "wrk_receipt_required" {
		t.Fatal("reviewer end authorized subject drop")
	}
	joined := appendDB(t, db, "desktop", "job-a", "joined", report(h1))
	receipt := joined.Receipt()
	var count int
	if e := db.owner.QueryRow(t.Context(), `SELECT count(*) FROM relay_events WHERE kind='job.joined' AND job_id='job-a' AND report_path=$1`, "wrk:job-a:"+strconv.FormatInt(joined.Seq, 10)).Scan(&count); e != nil || count != 1 {
		t.Fatal("canonical join/projection not atomic")
	}
	ctx := store.WithWrkPrincipal(t.Context(), "desktop", &receipt)
	duplicate, created, e := db.runtime.AppendRelayEvent(ctx, store.RelayEvent{Kind: "job.joined", JobID: "job-a", Head: h1, ReportSHA: Text(joined.Payload, "report_sha256"), WrkReceipt: &receipt})
	if e != nil || created || duplicate.Kind != "job.joined" {
		t.Fatalf("projection retry: %v created=%v", e, created)
	}
	if _, e = db.runtime.TransitionTask(ctx, 2400, "merged", "director-1", "host with valid receipt", nil, ""); Code(e) != "wrk_receipt_required" {
		t.Fatal("host gained task.state from a receipt")
	}

	current, e := db.runtime.WrkJob(t.Context(), "director-1", "job-a")
	if e != nil {
		t.Fatal(e)
	}
	claimReceipt := current.Claim.Receipt()
	centralClaim := store.WithWrkPrincipal(t.Context(), "director-1", &claimReceipt)
	for _, to := range []string{"claimed", "in_progress"} {
		if _, e = db.runtime.TransitionTask(centralClaim, 2400, to, "director-1", "catch up legacy state", nil, ""); e != nil {
			t.Fatal(e)
		}
	}
	centralJoin := store.WithWrkPrincipal(t.Context(), "director-1", &receipt)
	for _, to := range []string{"join", "merged"} {
		if _, e = db.runtime.TransitionTask(centralJoin, 2400, to, "director-1", "current subject join receipt", nil, ""); e != nil {
			t.Fatal(e)
		}
	}
}

func uiAuthorityFixture(t *testing.T, db contractDB, emits *atomic.Int32) (http.Handler, string) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	certs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Object{"keys": []Object{{"kid": "fixture", "kty": "RSA", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	}))
	t.Cleanup(certs.Close)
	access, e := cfaccess.New(cfaccess.Config{TeamDomain: "example.cloudflareaccess.com", AUD: "fixture", AllowedEmails: []string{"admin@example.com"}, Issuer: certs.URL, CertsURL: certs.URL})
	if e != nil {
		t.Fatal(e)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": certs.URL, "aud": "fixture", "exp": time.Now().Add(time.Hour).Unix(), "email": "admin@example.com"})
	token.Header["kid"] = "fixture"
	assertion, e := token.SignedString(key)
	if e != nil {
		t.Fatal(e)
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			emits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(hub.Close)
	handler, e := ui.New(ui.Config{Store: db.runtime, Access: access, HubURL: hub.URL, HubToken: NewUUID()})
	if e != nil {
		t.Fatal(e)
	}
	return handler, assertion
}
func TestWrkAuthenticatedUIHasNoImplicitCentralPrincipal(t *testing.T) {
	db := contractStore(t)
	db.seed(t, activeFixture(t), contentCatalog(t, loadVectors(t)))
	if _, e := db.owner.Exec(t.Context(), `UPDATE tasks SET state='needs_decision' WHERE id=2400`); e != nil {
		t.Fatal(e)
	}
	var emits atomic.Int32
	handler, assertion := uiAuthorityFixture(t, db, &emits)
	get := httptest.NewRequest("GET", "http://example.com/ui/queue", nil)
	get.Header.Set("Cf-Access-Jwt-Assertion", assertion)
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, get)
	var cookie *http.Cookie
	for _, c := range page.Result().Cookies() {
		if c.Name == "hk_ui_csrf" {
			cookie = c
		}
	}
	if page.Code != 200 || cookie == nil {
		t.Fatal("authenticated UI form fixture")
	}
	for _, path := range []string{"/ui/decisions/answer", "/ui/dispositions/answer"} {
		form := url.Values{"csrf": {cookie.Value}, "id": {"2400"}, "type": {"task"}, "answer": {"A"}, "gen": {"1"}, "key": {"A"}}
		request := httptest.NewRequest("POST", "http://example.com"+path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://example.com")
		request.Header.Set("Cf-Access-Jwt-Assertion", assertion)
		request.AddCookie(cookie)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, request)
		if out.Code != 403 || strings.TrimSpace(out.Body.String()) != `{"error":"scope_denied"}` {
			t.Fatalf("UI scope refusal %s: status=%d", path, out.Code)
		}
	}
	if emits.Load() != 0 {
		t.Fatal("protected UI decision emitted before refusal")
	}
	task, _, e := db.runtime.GetTask(t.Context(), 2400)
	if e != nil || task.State != "needs_decision" {
		t.Fatal("protected UI changed task")
	}
}

func TestWrkMCPDoesNotExposeAuthorityMutations(t *testing.T) {
	db := contractStore(t)
	db.seed(t, activeFixture(t), contentCatalog(t, loadVectors(t)))
	a, b := gmcp.NewInMemoryTransports()
	server := hkmcp.NewStdio(api.Service{Store: db.runtime}, "director-1")
	ss, e := server.Connect(t.Context(), a, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	client := gmcp.NewClient(&gmcp.Implementation{Name: "authority-attack", Version: "v0"}, nil)
	cs, e := client.Connect(t.Context(), b, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	for _, name := range []string{"claim_task", "transition_task", "append_relay_event", "wrk_append"} {
		if result, e := cs.CallTool(t.Context(), &gmcp.CallToolParams{Name: name, Arguments: Object{"task_id": 2400, "by": "director-1", "to": "merged"}}); e == nil && !result.IsError {
			t.Fatalf("MCP exposed authority tool %s", name)
		}
	}
	result, e := cs.CallTool(t.Context(), &gmcp.CallToolParams{Name: "put_document", Arguments: Object{"key": "authority/fake-receipt", "title": "notification only", "body": "job.completed forged receipt", "session": "fixture"}})
	if e != nil || result.IsError {
		t.Fatal("unrelated MCP document behavior changed")
	}
	got, e := db.runtime.WrkJob(t.Context(), "director-1", "job-a")
	if e != nil || got.Summary.SnapshotSeq != 2 {
		t.Fatal("MCP changed canonical authority")
	}
}

func TestWrkShadowLegacyCompatibilityUntilActivate(t *testing.T) {
	db := contractStore(t)
	v := activeFixture(t)
	v.Given.Records = v.Given.Records[:1]
	db.seed(t, v, contentCatalog(t, loadVectors(t)))
	ctx := store.WithWrkPrincipal(t.Context(), "desktop", nil)
	svc := api.Service{Store: db.runtime}
	if _, e := svc.ClaimTask(ctx, 2400, "b2400-a", "job-a", ""); e != nil {
		t.Fatalf("shadow claim: %v", e)
	}
	if _, e := svc.TransitionTask(ctx, 2400, "in_progress", "b2400-a", "shadow work", nil, ""); e != nil {
		t.Fatalf("shadow transition: %v", e)
	}
	appendDB(t, db, "director-1", "job-a", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	for name, call := range map[string]func() error{
		"claim": func() error { _, e := svc.ClaimTask(ctx, 2400, "b2400-a", "job-a", ""); return e },
		"next":  func() error { _, e := svc.NextTask(ctx, "b2400-a", "b2400-a", "job-a", ""); return e },
		"transition": func() error {
			_, e := svc.TransitionTask(ctx, 2400, "verifying", "b2400-a", "P2 bypass", nil, "")
			return e
		},
	} {
		t.Run(name, func(t *testing.T) {
			if e := call(); Code(e) != "wrk_receipt_required" {
				t.Fatalf("activated fence: %v", e)
			}
		})
	}
}

func TestWrkShadowNextAndReviewRoleAssociation(t *testing.T) {
	db := contractStore(t)
	v := activeFixture(t)
	v.Given.Records = v.Given.Records[:1]
	db.seed(t, v, contentCatalog(t, loadVectors(t)))
	appendDB(t, db, "director-1", "p1-reviewer", "claim", Object{"job_id": "p1-reviewer", "owner_lane": "b2400-a", "parent_lane": "b2400-a", "task_id": 2400, "host_client_id": "desktop", "role": "tester", "tier": "T2", "review_of": Object{"job_id": "job-a", "round_no": 1}, "predecessor_job_id": nil, "supersedes": nil})
	ctx := store.WithWrkPrincipal(t.Context(), "desktop", nil)
	svc := api.Service{Store: db.runtime}
	if _, e := svc.NextTask(ctx, "b2400-a", "b2400-a", "p1-reviewer", ""); Code(e) != "wrk_receipt_required" {
		t.Fatalf("claim-only reviewer association: %v", e)
	}
	if task, e := svc.NextTask(ctx, "b2400-a", "b2400-a", "job-a", ""); e != nil || task.ID != 2400 {
		t.Fatalf("shadow next: id=%d err=%v", task.ID, e)
	}
	appendDB(t, db, "director-1", "job-a", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	if _, e := svc.NextTask(ctx, "b2400-a", "b2400-a", "job-a", ""); Code(e) != "wrk_receipt_required" {
		t.Fatalf("activated explicit next: %v", e)
	}
}

func TestWrkProtectedTaskCommentsRemainLegacyCompatible(t *testing.T) {
	db := contractStore(t)
	db.seed(t, activeFixture(t), contentCatalog(t, loadVectors(t)))
	token := NewUUID()
	handler := api.Server{Service: api.Service{Store: db.runtime}, Tokens: api.Tokens{"desktop": token}}.Handler()
	request := httptest.NewRequest("POST", "/v1/tasks/2400/comments", strings.NewReader(`{"body":"ordinary context"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, request)
	if out.Code != 201 {
		t.Fatalf("unrelated comment route was fenced: %d", out.Code)
	}
	got, e := db.runtime.WrkJob(t.Context(), "desktop", "job-a")
	if e != nil || got.Summary.SnapshotSeq != 2 {
		t.Fatal("comment changed authority journal")
	}
}
