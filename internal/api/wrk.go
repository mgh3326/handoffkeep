package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

func (s Server) wrk(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor, ok := s.Tokens.Client(r)
	failResponse := func(err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(wrkrecords.Status(err))
		_ = json.NewEncoder(w).Encode(map[string]string{"error": wrkrecords.Code(err)})
	}
	fail := func(err error) {
		if s.Service.Store != nil {
			s.Service.Store.WrkAttempt(r.Context(), actor, "", "", err)
		}
		failResponse(err)
	}
	if !ok {
		fail(wrkrecords.Error("unauthorized"))
		return
	}
	engine := s.Service.Store.WrkEngine()
	if engine.Policy == nil || engine.Mode == "off" || engine.Mode == "" {
		fail(wrkrecords.Error("wrk_unavailable"))
		return
	}
	if !engine.Policy.Known(actor) {
		fail(wrkrecords.Error("scope_denied"))
		return
	}
	if r.URL.RawPath != "" && r.URL.EscapedPath() != r.URL.Path {
		fail(wrkrecords.Error("invalid_request"))
		return
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/wrk/"), "/")
	if r.Method != "GET" && r.Method != "POST" {
		fail(wrkrecords.Error("method_not_allowed"))
		return
	}
	success := func(status int, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			fail(wrkrecords.Error("wrk_unavailable"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(append(b, '\n'))
	}
	if r.Method == "GET" {
		if r.ContentLength > 0 {
			fail(wrkrecords.Error("invalid_request"))
			return
		}
		one := make([]byte, 1)
		if n, _ := r.Body.Read(one); n != 0 {
			fail(wrkrecords.Error("invalid_request"))
			return
		}
	}
	if len(p) == 1 && p[0] == "capabilities" {
		if err := engine.Check(actor, "capabilities.read"); err != nil {
			fail(err)
			return
		}
		if r.Method != "GET" {
			fail(wrkrecords.Error("method_not_allowed"))
			return
		}
		if len(r.URL.Query()) > 0 {
			fail(wrkrecords.Error("invalid_request"))
			return
		}
		success(200, wrkrecords.Capabilities{Contract: wrkrecords.Contract, Generation: engine.Generation, ClientID: actor, PolicySHA: engine.Policy.Digest(), Writes: !engine.Recovery && engine.Mode != "off"})
		return
	}
	if len(p) < 2 || p[0] != "jobs" || !wrkrecords.JobRE.MatchString(p[1]) {
		fail(wrkrecords.Error("not_found"))
		return
	}
	job := p[1]
	switch {
	case len(p) == 2:
		if r.Method != "GET" {
			fail(wrkrecords.Error("method_not_allowed"))
			return
		}
		if len(r.URL.Query()) > 0 {
			fail(wrkrecords.Error("invalid_request"))
			return
		}
		v, e := s.Service.Store.WrkJob(r.Context(), actor, job)
		if e != nil {
			fail(e)
			return
		}
		success(200, v)
	case len(p) == 3 && p[2] == "records":
		if r.Method == "POST" {
			if len(r.URL.Query()) > 0 {
				fail(wrkrecords.Error("invalid_request"))
				return
			}
			if r.Header.Get("Content-Type") != "application/json" {
				fail(wrkrecords.Error("unsupported_media_type"))
				return
			}
			if len(r.Header.Values("Idempotency-Key")) != 1 || !wrkrecords.UUIDRE.MatchString(r.Header.Get("Idempotency-Key")) {
				fail(wrkrecords.Error("invalid_request"))
				return
			}
			b, e := io.ReadAll(io.LimitReader(r.Body, wrkrecords.MaxRequest+1))
			if e != nil {
				fail(wrkrecords.Error("invalid_request"))
				return
			}
			if r.ContentLength > wrkrecords.MaxRequest || len(b) > wrkrecords.MaxRequest {
				fail(wrkrecords.Error("request_too_large"))
				return
			}
			req, e := wrkrecords.ParseRequest(b)
			if e != nil {
				fail(e)
				return
			}
			v, e := s.Service.WrkAppend(r, actor, job, r.Header.Get("Idempotency-Key"), req)
			if e != nil {
				// Direct store callers also need an audit; WrkAppend has already
				// appended this denial's attempt. Do not count it twice here.
				failResponse(e)
				return
			}
			status := 201
			if !v.Created {
				status = 200
			}
			success(status, v)
			return
		}
		q := r.URL.Query()
		for key, values := range q {
			if (key != "after_seq" && key != "snapshot_seq" && key != "limit") || len(values) != 1 {
				fail(wrkrecords.Error("invalid_request"))
				return
			}
		}
		after, snapshot, limit := int64(0), int64(0), int64(100)
		for key, dest := range map[string]*int64{"after_seq": &after, "snapshot_seq": &snapshot, "limit": &limit} {
			if values, ok := q[key]; ok {
				n, e := wrkInteger(values[0], false)
				if e != nil || (key == "snapshot_seq" && n == 0) {
					fail(wrkrecords.Error("invalid_cursor"))
					return
				}
				*dest = n
			}
		}
		v, e := s.Service.Store.WrkRecords(r.Context(), actor, job, after, snapshot, int(limit))
		if e != nil {
			fail(e)
			return
		}
		success(200, v)
	case len(p) == 5 && p[2] == "records" && p[4] == "content":
		if r.Method != "GET" {
			fail(wrkrecords.Error("method_not_allowed"))
			return
		}
		if len(r.URL.Query()) > 0 {
			fail(wrkrecords.Error("invalid_request"))
			return
		}
		n, e := wrkInteger(p[3], true)
		if e != nil {
			fail(wrkrecords.Error("invalid_request"))
			return
		}
		v, e := s.Service.Store.WrkContent(r.Context(), actor, job, n)
		if e != nil {
			fail(e)
			return
		}
		success(200, v)
	default:
		fail(wrkrecords.Error("not_found"))
	}
}
func wrkInteger(s string, positive bool) (int64, error) {
	if s == "" {
		return 0, errors.New("invalid integer")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid integer")
		}
	}
	n, e := strconv.ParseInt(s, 10, 64)
	if e != nil || n > wrkrecords.MaxInt || (positive && n == 0) {
		return 0, errors.New("invalid integer")
	}
	return n, nil
}

// The service check uses the bearer-derived identity, never caller By text;
// Store repeats authorization inside its locked mutation transaction.
func (s Service) WrkAppend(r *http.Request, actor, job, key string, req wrkrecords.Request) (wrkrecords.WriteResponse, error) {
	return s.Store.WrkAppend(r.Context(), actor, job, key, req)
}
