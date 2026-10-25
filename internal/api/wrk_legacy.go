package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

func (s Service) wrkTaskPrecheck(ctx context.Context, id int64, to string, binding bool) error {
	task, found, e := s.Store.GetTask(ctx, id)
	if e != nil || !found {
		return e
	}
	return s.Store.WrkFence(ctx, wrkrecords.LegacyIntent{TaskID: id, CurrentJob: task.Refs.JobID, To: to, Binding: binding})
}
func (s Server) legacyWrkFence(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Service.Store == nil || r.Method != "POST" || strings.HasPrefix(r.URL.Path, "/v1/wrk/") {
			next.ServeHTTP(w, r)
			return
		}
		route := r.URL.Path
		listed := strings.HasPrefix(route, "/v1/tasks") || route == "/v1/decisions/resolve" || route == "/v1/assistant/decisions/resolve" || route == "/v1/relay/events"
		if !listed {
			next.ServeHTTP(w, r)
			return
		}
		actor, ok := s.Tokens.Client(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		raw, e := io.ReadAll(io.LimitReader(r.Body, store.MaxBytes+4097))
		if e != nil {
			appErr(w, wrkrecords.Error("invalid_request"))
			return
		}
		// Preserve unread bytes for the existing route's own size/error contract.
		// The fence must not turn an oversized legacy request into truncated JSON.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
		var body wrkrecords.Object
		if json.Unmarshal(raw, &body) != nil {
			next.ServeHTTP(w, r)
			return
		}
		var receipt *wrkrecords.Receipt
		if value, present := body["wrk_receipt"]; present {
			b, _ := json.Marshal(value)
			var v wrkrecords.Receipt
			m, ok := value.(map[string]any)
			if !ok || len(m) != 4 || json.Unmarshal(b, &v) != nil || !wrkrecords.JobRE.MatchString(v.JobID) || v.Seq < 1 || v.Seq > wrkrecords.MaxInt || !wrkrecords.UUIDRE.MatchString(v.Generation) || len(v.Hash) != 64 {
				appErr(w, wrkrecords.Error("invalid_request"))
				return
			}
			receipt = &v
			delete(body, "wrk_receipt")
			raw, _ = json.Marshal(body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		r = r.WithContext(store.WithWrkPrincipal(r.Context(), actor, receipt))
		intent := wrkrecords.LegacyIntent{Receipt: receipt}
		parts := strings.Split(strings.Trim(route, "/"), "/")
		if route == "/v1/tasks" {
			intent.JobID = wrkrecords.Text(wrkrecords.Obj(body, "refs"), "job_id")
			intent.Binding = true
		}
		if len(parts) >= 3 && parts[1] == "tasks" {
			if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				intent.TaskID = n
				intent.Binding = true
				if len(parts) > 3 && parts[3] == "transition" {
					intent.Binding = body["refs"] != nil || body["claimed_by"] != nil
					intent.To = wrkrecords.Text(body, "to")
				}
				if len(parts) > 3 && parts[3] == "decision-request" {
					intent.Binding = false
					intent.To = "claimed"
				}
			}
		}
		if len(parts) == 5 && parts[2] == "dispositions" {
			intent.TaskID, _ = strconv.ParseInt(parts[3], 10, 64)
			intent.To = "merged"
			intent.Binding = body["action"] == "relink"
		}
		if route == "/v1/decisions/resolve" && (body["type"] == "task" || body["task_id"] != nil) {
			intent.TaskID = wrkrecords.Num(body, "id")
			if intent.TaskID == 0 {
				intent.TaskID = wrkrecords.Num(body, "task_id")
			}
			intent.To = "claimed"
		}
		if route == "/v1/assistant/decisions/resolve" {
			intent.TaskID, _, _ = store.ParseDecisionRequestID(wrkrecords.Text(body, "request_id"))
			intent.To = "claimed"
		}
		if route == "/v1/relay/events" {
			intent.JobID = wrkrecords.Text(body, "job_id")
			intent.Kind = wrkrecords.Text(body, "kind")
			intent.Head = wrkrecords.Text(body, "head")
			intent.Digest = wrkrecords.Text(body, "report_sha256")
		}
		if intent.TaskID > 0 {
			task, found, err := s.Service.Store.GetTask(r.Context(), intent.TaskID)
			if err != nil {
				appErr(w, err)
				return
			}
			if found {
				intent.CurrentJob = task.Refs.JobID
			}
		}
		if err := s.Service.Store.WrkFence(r.Context(), intent); err != nil {
			appErr(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}
