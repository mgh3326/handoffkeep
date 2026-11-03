package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestR2FreshBoardReservationSurvivesAbsentRepairAndWebhook(t *testing.T) {
	for _, route := range []string{"board", "reservations"} {
		for _, awaiting := range []bool{false, true} {
			for _, legacyRejected := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/awaiting=%t/legacy_rejected=%t", route, awaiting, legacyRejected), func(t *testing.T) {
					_, url := testdb.Schema(t, "r2_reservation_grace")
					st, err := store.Open(t.Context(), url)
					if err != nil {
						t.Fatal(err)
					}
					defer st.Close()
					task, err := st.CreateTask(t.Context(), store.Task{Lane: "grace", Title: "delayed create webhook", Kind: "implement", Priority: 2, CreatedBy: "test", Project: projectStr("experiment")})
					if err != nil {
						t.Fatal(err)
					}
					id := store.LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", task.ID))
					if err = st.SerializeLinearCreate(t.Context(), func(lease store.LinearCreateLease) error {
						if reserved, err := lease.ReserveBoard(t.Context(), id, task.ID); err != nil || !reserved {
							return fmt.Errorf("reserve: %t, %v", reserved, err)
						}
						if awaiting {
							if err := lease.AwaitBoardWebhook(t.Context(), id); err != nil {
								return err
							}
						}
						if legacyRejected {
							// A legacy known-refusal row can be backfilled as a board
							// reservation. Board grace must not depend on uncertain.
							return lease.Reject(t.Context(), id)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					calls := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						calls++
						fmt.Fprint(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`)
					}))
					defer srv.Close()
					client := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Metadata: boardMetadataFixture(), Admission: st, CapacityScopeVerified: true}
					h := Server{Service: Service{Store: st}, Tokens: Tokens{"operator": "fake-token"}, LinearBoard: &client}.Handler()
					body := fmt.Sprintf(`{"issue_id":%q}`, id)
					if route == "board" {
						body = fmt.Sprintf(`{"task_id":%d}`, task.ID)
					}
					req := httptest.NewRequest(http.MethodPost, "/v1/linear/"+route+"/reconcile", strings.NewReader(body))
					req.Header.Set("Authorization", "Bearer fake-token")
					out := httptest.NewRecorder()
					h.ServeHTTP(out, req)
					if out.Code != http.StatusConflict || !strings.Contains(out.Body.String(), "linear_reservation_too_recent") || calls != 1 {
						t.Fatalf("fresh reservation released: status=%d body=%s calls=%d", out.Code, out.Body.String(), calls)
					}
					if found, err := st.HasLinearCreateReservation(t.Context(), id); err != nil || !found {
						t.Fatal("fresh reservation disappeared", found, err)
					}
					payload, err := json.Marshal(map[string]any{"type": "Issue", "action": "create", "webhookTimestamp": time.Now().UnixMilli(), "data": map[string]any{"id": id, "identifier": "ROB-321", "description": fmt.Sprintf("<!-- work-board:task:%d -->", task.ID), "updatedAt": time.Now().UTC()}})
					if err != nil {
						t.Fatal(err)
					}
					hook := LinearWebhook{Secret: fakeLinearSigningValue, Board: st, Store: st, Issues: &fakeIssues{}}
					if status, _ := postLinear(t, hook, payload, linearSign(fakeLinearSigningValue, payload)); status != http.StatusOK {
						t.Fatal("signed webhook rejected", status)
					}
					if linked, found, err := st.GetLinearIssue(t.Context(), task.ID); err != nil || !found || linked.IssueID != id {
						t.Fatal("late webhook lost mapping", linked, found, err)
					}
					if reserved, err := st.HasLinearCreateReservation(t.Context(), id); err != nil || reserved {
						t.Fatal("webhook did not complete reservation", reserved, err)
					}
				})
			}
		}
	}
}

func TestR2TransientLabelHTTPFailuresStayRetryable(t *testing.T) {
	for _, status := range []int{401, 403, 408, 409, 425} {
		for _, readStatus := range []int{status, http.StatusOK} {
			t.Run(fmt.Sprintf("create=%d/read=%d", status, readStatus), func(t *testing.T) {
				meta := boardMetadataFixture()
				calls := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var in struct{ Query string }
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
						t.Error(err)
					}
					if strings.Contains(in.Query, "HkBoardLabelByID") {
						w.WriteHeader(readStatus)
						fmt.Fprint(w, `{"data":{"issueLabel":null}}`)
					} else {
						w.WriteHeader(status)
					}
				}))
				defer srv.Close()
				client := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Metadata: meta}
				for range 2 {
					if _, err := client.BoardLabel(t.Context(), "team", "fleet"); err == nil || !strings.Contains(err.Error(), "status ") {
						t.Fatal("HTTP error hidden by durable label failure", err)
					}
				}
				if meta.failed["fleet"] || calls != 4 {
					t.Fatal("transient refusal became permanent", meta.failed, calls)
				}
			})
		}
	}
}

func TestR2AuthFailuresDoNotPersistLabelFailures(t *testing.T) {
	pool, schema := testdb.SchemaPool(t, "r2_label_http")
	st, err := store.Open(t.Context(), testdb.WithSearchPath(t, testdb.URL(t), schema+",public"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, status := range []int{401, 403, 408, 409, 425} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(status)
			}))
			defer srv.Close()
			client := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Metadata: st}
			for range 2 {
				if _, err := client.BoardLabel(t.Context(), "team", "fleet-"+strconv.Itoa(status)); err == nil || !strings.Contains(err.Error(), "status ") {
					t.Fatal("auth/timeout/conflict error hidden", err)
				}
			}
			var failures int
			if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM linear_label_failures`).Scan(&failures); err != nil || failures != 0 || calls != 4 {
				t.Fatal("retryable HTTP wrote linear_label_failures", failures, calls, err)
			}
		})
	}
}

func TestR2RetainedCreateIsDistinctFromWebhookWait(t *testing.T) {
	for _, awaiting := range []bool{false, true} {
		t.Run(fmt.Sprintf("awaiting=%t", awaiting), func(t *testing.T) {
			id := store.LinearDecisionUUID("work-board:task:42")
			gate := newAdmissionFake()
			gate.pending[id], gate.board, gate.awaiting = true, map[string]bool{id: true}, map[string]bool{id: awaiting}
			client := LinearAPI{Admission: gate, CapacityScopeVerified: true}
			_, err := client.CreateLinearIssue(t.Context(), LinearDecisionIssue{Board: true, ID: id, TaskID: 42})
			if awaiting {
				if !errors.Is(err, errBoardAwaitWebhook) {
					t.Fatal("accepted create no longer awaiting webhook", err)
				}
			} else if err == nil || errors.Is(err, errBoardAwaitWebhook) || !strings.Contains(err.Error(), "unresolved") {
				t.Fatal("retained ambiguous create hidden as normal webhook wait", err)
			}
		})
	}
}

func TestR2RetainedBoardCreateLogsAfterThreeAttempts(t *testing.T) {
	for _, awaiting := range []bool{false, true} {
		for _, attempts := range []int{0, 3, 9} {
			t.Run(fmt.Sprintf("awaiting=%t/attempts=%d", awaiting, attempts), func(t *testing.T) {
				q := &boardQueue{reservations: map[string]bool{store.LinearDecisionUUID("work-board:task:42"): awaiting}}
				q.add(boardFixture(), store.LinearBoardEvent{})
				op := q.rows[0]
				op.Attempts = attempts
				var out bytes.Buffer
				old := log.Writer()
				log.SetOutput(&out)
				defer log.SetOutput(old)
				result, err := (LinearBoardWorker{Store: q, Linear: newBoardFake()}).send(t.Context(), op)
				wantLog := !awaiting && attempts >= 3
				if err != nil || result.State != "pending" || strings.Contains(out.String(), "linear_board.create_unresolved") != wantLog {
					t.Fatal(result, err, out.String())
				}
				if wantLog && !strings.Contains(out.String(), "handoffkeep linear board reconcile --task 42") {
					t.Fatal("missing repair command", out.String())
				}
			})
		}
	}
}
