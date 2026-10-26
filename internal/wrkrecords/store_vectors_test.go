package wrkrecords_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

type contractDB struct {
	owner   *pgxpool.Pool
	runtime *store.Store
}

// Only CI supplies this database. No local fallback, embedded database, or
// shell server startup exists in the harness.
func contractStore(t *testing.T) contractDB {
	t.Helper()
	database := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if database == "" {
		if os.Getenv("CI") == "true" || os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("CI must execute every PostgreSQL vector")
		}
		t.Skip("PostgreSQL contract vectors run only in CI")
	}
	ctx := t.Context()
	admin, e := pgxpool.New(ctx, database)
	if e != nil {
		t.Fatal("CI database configuration")
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	schema, role := "wrk_contract_"+suffix, "wrk_contract_runtime_"+suffix
	if _, e = admin.Exec(ctx, `CREATE SCHEMA `+schema); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(database)
	if e != nil {
		t.Fatal("CI database URL")
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	owner, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	if e = owner.MigrateWrk(ctx); e != nil {
		t.Fatal(e)
	}
	owner.Close()
	pool, e := pgxpool.New(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	for _, sql := range []string{`CREATE ROLE ` + role + ` NOLOGIN NOSUPERUSER NOBYPASSRLS NOINHERIT`, `GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role, `GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role, `GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role, `REVOKE UPDATE,DELETE,TRUNCATE ON ` + schema + `.wrk_records,` + schema + `.wrk_contents,` + schema + `.wrk_attempts FROM ` + role} {
		if _, e = admin.Exec(ctx, sql); e != nil {
			t.Fatal(e)
		}
	}
	q.Set("role", role)
	u.RawQuery = q.Encode()
	runtime, e := store.OpenRuntime(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		runtime.Close()
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_, _ = admin.Exec(context.Background(), `DROP ROLE `+role)
		admin.Close()
	})
	return contractDB{pool, runtime}
}

func (db contractDB) seed(t *testing.T, v vector, catalog map[string][]byte) {
	t.Helper()
	ctx := t.Context()
	snap, p := seedVector(t, v, catalog)
	engine := Engine{Policy: p, Mode: "enforce", Generation: "00000000-0000-4000-8000-ffff00000001", Now: time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)}
	incomplete := fixture_incomplete[v.file+"/"+v.Name]
	if incomplete {
		if e := db.runtime.ConfigureWrkEngine(ctx, engine); e != nil {
			t.Fatal(e)
		}
	}

	// Task bindings are genuine fixture prerequisites, derived only from given
	// claims. The request and expected outcome never determine the seed.
	tasks := map[int64]Record{}
	for job := range snap.Jobs {
		if c := snap.Claim(job); c != nil && !Review(c) {
			tasks[Num(c.Payload, "task_id")] = *c
		}
	}
	// Fresh claims in the pack refer to an existing unbound task.
	for _, id := range []int64{2400, 2401, 2403, 9001, 9002} {
		if _, ok := tasks[id]; !ok {
			tasks[id] = Record{Payload: Object{"owner_lane": "director-1", "parent_lane": "director-1"}}
		}
	}
	for id, c := range tasks {
		refs, _ := json.Marshal(Object{"job_id": c.JobID})
		if _, e := db.owner.Exec(ctx, `INSERT INTO tasks(id,lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at) VALUES($1,$2,$3,'contract task','implement','backlog',0,$4::jsonb,'','fixture',now(),now())`, id, Text(c.Payload, "owner_lane"), Text(c.Payload, "parent_lane"), string(refs)); e != nil {
			t.Fatal(e)
		}
	}
	jobs := make([]string, 0, len(snap.Jobs))
	for job := range snap.Jobs {
		jobs = append(jobs, job)
	}
	sort.Strings(jobs)
	for _, job := range jobs {
		if _, e := db.owner.Exec(ctx, `INSERT INTO wrk_job_locks(job_id) VALUES($1)`, job); e != nil {
			t.Fatal(e)
		}
		for _, r := range snap.Jobs[job] {
			payload, _ := json.Marshal(r.Payload)
			var source any
			if r.Source != nil {
				b, _ := json.Marshal(r.Source)
				source = string(b)
			}
			if _, e := db.owner.Exec(ctx, `INSERT INTO wrk_records(job_id,seq,record_id,store_generation,kind,claim_seq,actor_client_id,idempotency_key,request_sha256,payload,imported,source,received_at,previous_hash,record_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12::jsonb,$13,$14,$15)`, r.JobID, r.Seq, r.RecordID, r.Generation, r.Kind, r.ClaimSeq, r.Actor, r.Key, r.RequestSHA, string(payload), r.Imported, source, r.ReceivedAt, r.PreviousHash, r.RecordHash); e != nil {
				t.Fatal(e)
			}
			if b := snap.Contents[job][r.Seq]; len(b) > 0 {
				if _, e := db.owner.Exec(ctx, `INSERT INTO wrk_contents(job_id,seq,content,content_sha256,size_bytes) VALUES($1,$2,$3,$4,$5)`, job, r.Seq, b, SHA(b), len(b)); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
	if !incomplete {
		if e := db.runtime.ConfigureWrkEngine(ctx, engine); e != nil {
			t.Fatal(e)
		}
	}
	db.seedLegacyMapping(t, v)
}

func vectorBody(t *testing.T, v vector) []byte {
	t.Helper()
	b := Clone(Obj(v.Request, "body"))
	if v.Name == "verdict-body-token-shape-invalid-request" {
		p := Obj(b, "payload")
		delete(p, "content_b64_construct")
		content := []byte("VERDICT: PASS @" + Text(p, "head") + "\n" + "sk-" + strings.Repeat("X", 32) + "\n")
		p["content_b64"], p["content_sha256"] = base64.StdEncoding.EncodeToString(content), SHA(content)
	}
	raw, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}

func TestStoreHTTPVectors(t *testing.T) {
	if os.Getenv("HANDOFFKEEP_TEST_DB_URL") == "" {
		contractStore(t)
	}
	vs := loadVectors(t)
	catalog := contentCatalog(t, vs)
	for _, v := range vs {
		kind := Text(v.Request, "type")
		if kind != "http" && kind != "http_raw" && kind != "legacy" {
			continue
		}
		t.Run(strings.TrimSuffix(v.file, ".json")+"/"+v.Name, func(t *testing.T) {
			// A fresh schema preserves the legacy append-only guards. Never
			// disable a trigger or truncate immutable task history to reset a case.
			db := contractStore(t)
			db.seed(t, v, catalog)
			actor := Text(v.Request, "actor")
			tokens := api.Tokens{actor: NewUUID()}
			handler := api.Server{Service: api.Service{Store: db.runtime}, Tokens: tokens}.Handler()
			target := Text(v.Request, "path")
			query := url.Values{}
			for k, val := range Obj(v.Request, "query") {
				query.Set(k, fmt.Sprint(val))
			}
			if len(query) > 0 {
				target += "?" + query.Encode()
			}
			var body []byte
			if Text(v.Request, "method") == "POST" {
				body = vectorBody(t, v)
				if kind == "legacy" {
					target, body = legacyVectorRequest(t, v)
				}
			}
			req := httptest.NewRequest(Text(v.Request, "method"), target, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tokens[actor])
			req.Header.Set("Content-Type", "application/json")
			if key := Text(v.Request, "idempotency_key"); key != "" {
				req.Header.Set("Idempotency-Key", key)
			}
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			expect := v.Expect
			if Text(expect, "outcome") == "legacy_compatible" {
				// Real unrelated task creation retains its existing 201 behavior.
				if out.Code != 201 {
					t.Fatalf("legacy creation behavior changed: %d", out.Code)
				}
				return
			}
			var got Object
			if json.Unmarshal(out.Body.Bytes(), &got) != nil {
				t.Fatalf("non-JSON status %d", out.Code)
			}
			if int64(out.Code) != Num(expect, "http_status") {
				t.Fatalf("status=%d response=%v expected=%v", out.Code, got, expect)
			}
			if out.Code >= 400 {
				if len(got) != 1 || Text(got, "error") != Text(expect, "error") {
					t.Fatalf("unsafe/wrong error: %v", got)
				}
				var count int
				if e := db.owner.QueryRow(t.Context(), `SELECT count(*) FROM wrk_records`).Scan(&count); e != nil || count != len(v.Given.Records) {
					t.Fatal("rejected write changed business records")
				}
				return
			}
			for _, key := range []string{"created", "record", "summary"} {
				if want, ok := expect[key]; ok {
					subset(t, want, got[key], key)
				}
			}
			if want, ok := expect["record_ref"]; ok {
				subset(t, want, got["record"], "record_ref")
			}
			if want := Obj(expect, "records_response"); want != nil {
				actual := Clone(got)
				rs, _ := got["records"].([]any)
				seqs := []any{}
				for _, x := range rs {
					seqs = append(seqs, Obj(Object{"r": x}, "r")["seq"])
				}
				actual["record_seqs"] = seqs
				subset(t, want, actual, "records_response")
			}
			if kind != "legacy" && out.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
			if Text(v.Request, "method") == "POST" && kind != "legacy" {
				var response WriteResponse
				if json.Unmarshal(out.Body.Bytes(), &response) != nil {
					t.Fatal("write response")
				}
				if response.Created {
					stored, e := db.runtime.WrkJob(t.Context(), actor, response.Record.JobID)
					if e != nil || stored.Summary.SnapshotSeq != response.Record.Seq {
						t.Fatal("reply preceded committed record")
					}
					if response.Record.Kind == "verdict" || response.Record.Kind == "completed" || response.Record.Kind == "joined" {
						content, e := db.runtime.WrkContent(t.Context(), actor, response.Record.JobID, response.Record.Seq)
						if e != nil || content.Size == 0 {
							t.Fatal("record/content not atomic")
						}
					}
				}
			}
		})
	}
}
