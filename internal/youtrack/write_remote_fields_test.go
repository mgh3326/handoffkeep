package youtrack

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// projectFieldsJSON is a project in the importer's shape: a text hk 프로젝트,
// a multi-value 멈춤, a state bundle.
func projectFieldsJSON() []map[string]any {
	field := func(name, typeID string, values ...string) map[string]any {
		f := map[string]any{"field": map[string]any{"name": name, "fieldType": map[string]any{"id": typeID}}}
		if len(values) > 0 {
			vs := []map[string]any{}
			for i, v := range values {
				vs = append(vs, map[string]any{"id": name + "-v" + strconv.Itoa(i), "name": v, "archived": false})
			}
			vs = append(vs, map[string]any{"id": name + "-varch", "name": "archived-lane", "archived": true})
			f["bundle"] = map[string]any{"id": "b-" + name, "values": vs}
		}
		return f
	}
	return []map[string]any{
		field("hk ID", "integer"), field("hk 프로젝트", "string"), field("hk 상태", "state[1]", "개발 중", "검토 중"),
		field("hk 우선순위", "integer"), field("레인", "enum[1]", "handoffkeep-design"), field("종류", "enum[1]", "implement"),
		field("등급", "enum[1]", "A"), field("티어", "enum[1]", "T2"), field("PR", "string"), field("작업 job", "string"),
		field("멈춤", "enum[*]", "멈춤"), field("마감", "date"), field("영역", "string"), field("마일스톤", "string"), field("예상 소요", "period"),
	}
}

func fieldsSnapshot() ytwrite.IssueSnapshot {
	due := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	return ytwrite.TaskIssueSnapshot(ytwrite.TaskFields{TaskID: 4003, Project: "handoffkeep", Title: "t (grade A, T2)", State: "developing",
		Priority: 2, Lane: "handoffkeep-design", Kind: "implement", DueAt: &due, Expected: 90 * time.Minute})
}

func TestWriteRemoteUpdateIssueFields(t *testing.T) {
	var body map[string]any
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/admin/projects/0-7/customFields":
			_ = json.NewEncoder(w).Encode(projectFieldsJSON())
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/2-9":
			_, _ = w.Write([]byte(`{"id":"2-9"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}}
	r := y.serve(t)
	snap := fieldsSnapshot()
	id, err := r.UpdateIssueFields(t.Context(), ytwrite.IssueFieldsUpdate{IssueID: "2-9", ProjectID: "0-7", IssueSnapshot: snap})
	if err != nil || id != "2-9" {
		t.Fatalf("id %q err %v", id, err)
	}
	if y.countOf(http.MethodPost) != 1 || y.countOf(http.MethodGet) != 1 {
		t.Fatalf("requests %v", y.requests)
	}
	if err := json.Unmarshal([]byte(y.bodies[1]), &body); err != nil {
		t.Fatal(err)
	}
	if body["summary"] != snap.Summary || body["description"] != snap.Description {
		t.Fatalf("body %v", body)
	}
	got := map[string]string{}
	for _, f := range body["customFields"].([]any) {
		m := f.(map[string]any)
		raw, _ := json.Marshal(m["value"])
		got[m["name"].(string)] = m["$type"].(string) + " " + string(raw)
	}
	want := map[string]string{
		"hk ID":   `SimpleIssueCustomField 4003`,
		"hk 프로젝트": `SimpleIssueCustomField "handoffkeep"`,
		"hk 상태":   `StateIssueCustomField {"$type":"StateBundleElement","name":"개발 중"}`,
		"hk 우선순위": `SimpleIssueCustomField 2`,
		"레인":      `SingleEnumIssueCustomField {"$type":"EnumBundleElement","name":"handoffkeep-design"}`,
		"종류":      `SingleEnumIssueCustomField {"$type":"EnumBundleElement","name":"implement"}`,
		"등급":      `SingleEnumIssueCustomField {"$type":"EnumBundleElement","name":"A"}`,
		"티어":      `SingleEnumIssueCustomField {"$type":"EnumBundleElement","name":"T2"}`,
		"PR":      `SimpleIssueCustomField null`,
		"작업 job":  `SimpleIssueCustomField null`,
		"멈춤":      `MultiEnumIssueCustomField []`,
		"마감":      `DateIssueCustomField 1795132800000`,
		"영역":      `SimpleIssueCustomField null`,
		"마일스톤":    `SimpleIssueCustomField null`,
		"예상 소요":   `PeriodIssueCustomField {"$type":"PeriodValue","minutes":90}`,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %s, want %s", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("fields %v", got)
	}
}

// A value the bundle lacks (or has archived) and a field the project lacks
// refuse before the POST, naming the field and the value; a failed read of
// the project's fields sends nothing either.
func TestWriteRemoteUpdateIssueFieldsRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*ytwrite.IssueSnapshot)
		want string
	}{
		"missing_closed_value":  {func(s *ytwrite.IssueSnapshot) { setField(s, ytwrite.FieldGrade, "Z") }, `field "등급" value "Z": the value is not in the field's bundle`},
		"archived_closed_value": {func(s *ytwrite.IssueSnapshot) { setField(s, ytwrite.FieldKind, "archived-lane") }, `field "종류" value "archived-lane"`},
		// A closed-set refusal adds nothing, not even the open-set value
		// the same snapshot lacks.
		"closed_with_open_missing": {func(s *ytwrite.IssueSnapshot) {
			setField(s, ytwrite.FieldGrade, "Z")
			setField(s, ytwrite.FieldLane, "routing-policy")
		}, `field "등급" value "Z"`},
		"missing_field": {func(s *ytwrite.IssueSnapshot) {
			s.Fields = append(s.Fields, ytwrite.FieldValue{Name: "없는 필드", Type: ytwrite.FieldTypeText, Value: "x"})
		}, `field "없는 필드" value "x": project 0-7 has no such field`},
	} {
		t.Run(name, func(t *testing.T) {
			y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(projectFieldsJSON())
			}}
			snap := fieldsSnapshot()
			tc.edit(&snap)
			_, err := y.serve(t).UpdateIssueFields(t.Context(), ytwrite.IssueFieldsUpdate{IssueID: "2-9", ProjectID: "0-7", IssueSnapshot: snap})
			var refused *ytwrite.RefusedError
			if !errors.As(err, &refused) || refused.Reason != ytwrite.ReasonFieldRefused || !strings.Contains(refused.Detail, tc.want) {
				t.Fatalf("err %v, want a refusal naming %s", err, tc.want)
			}
			if y.countOf(http.MethodPost) != 0 {
				t.Fatal("posted after a refusal")
			}
		})
	}
	t.Run("open_set_on_string_field", func(t *testing.T) {
		// 영역 is a text field in this project: no bundle, no add.
		y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(projectFieldsJSON())
				return
			}
			_, _ = w.Write([]byte(`{"id":"2-9"}`))
		}}
		snap := fieldsSnapshot()
		setField(&snap, ytwrite.FieldArea, "new-area")
		if _, err := y.serve(t).UpdateIssueFields(t.Context(), ytwrite.IssueFieldsUpdate{IssueID: "2-9", ProjectID: "0-7", IssueSnapshot: snap}); err != nil {
			t.Fatal(err)
		}
		if y.countOf(http.MethodPost) != 1 {
			t.Fatalf("requests %v", y.requests)
		}
	})
	t.Run("field_read_fails", func(t *testing.T) {
		y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }}
		_, err := y.serve(t).UpdateIssueFields(t.Context(), ytwrite.IssueFieldsUpdate{IssueID: "2-9", ProjectID: "0-7", IssueSnapshot: fieldsSnapshot()})
		var ns *ytwrite.NotSentError
		if !errors.As(err, &ns) || y.countOf(http.MethodPost) != 0 {
			t.Fatalf("err %v, requests %v", err, y.requests)
		}
	})
	t.Run("post_4xx", func(t *testing.T) {
		y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(projectFieldsJSON())
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad_request","error_description":"Unknown value"}`))
		}}
		_, err := y.serve(t).UpdateIssueFields(t.Context(), ytwrite.IssueFieldsUpdate{IssueID: "2-9", ProjectID: "0-7", IssueSnapshot: fieldsSnapshot()})
		var re *ytwrite.RemoteError
		if !errors.As(err, &re) || re.Status != http.StatusBadRequest || y.countOf(http.MethodPost) != 1 {
			t.Fatalf("err %v, requests %v", err, y.requests)
		}
	})
}

// What ReadIssueFields returns for a written issue is what was written, so
// the evidence of an applied update is an exact match.
func TestWriteRemoteReadIssueFieldsRoundTrip(t *testing.T) {
	snap := fieldsSnapshot()
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"summary":` + quoteJSON(snap.Summary) + `,"description":` + quoteJSON(snap.Description) + `,"customFields":[
			{"name":"hk ID","value":4003},{"name":"hk 프로젝트","value":"handoffkeep"},
			{"name":"hk 상태","value":{"name":"개발 중","$type":"StateBundleElement"}},{"name":"hk 우선순위","value":2},
			{"name":"레인","value":{"name":"handoffkeep-design"}},{"name":"종류","value":{"name":"implement"}},
			{"name":"등급","value":{"name":"A"}},{"name":"티어","value":{"name":"T2"}},{"name":"PR","value":null},
			{"name":"작업 job","value":null},{"name":"멈춤","value":[]},{"name":"마감","value":1795132800000},
			{"name":"영역","value":null},{"name":"마일스톤","value":null},{"name":"예상 소요","value":{"minutes":90,"presentation":"1h 30m"}},
			{"name":"Assignee","value":{"name":"someone"}}]}`))
	}}
	got, err := y.serve(t).ReadIssueFields(t.Context(), "2-9")
	if err != nil {
		t.Fatal(err)
	}
	if d := ytwrite.SnapshotDiff(snap, got); d != "" {
		t.Fatalf("read differs at %s: %+v", d, got)
	}
	for raw, want := range map[string]string{`[{"name":"b"},{"name":"a"}]`: "a,b", `{"text":"x"}`: "x", `null`: "", `12.5`: "12.5"} {
		if v := canonicalFieldValue(json.RawMessage(raw)); v != want {
			t.Errorf("canonical(%s) = %q, want %q", raw, v, want)
		}
	}
}

// setField sets one snapshot field's value by name.
func setField(s *ytwrite.IssueSnapshot, name, value string) {
	for i := range s.Fields {
		if s.Fields[i].Name == name {
			s.Fields[i].Value = value
			return
		}
	}
	panic("no field " + name)
}

// An open-set value (레인) the project's bundle lacks is added to that bundle,
// or un-archived, before the one issue update; a 403 on the add refuses, any
// other failure sends nothing and is retried (hk 4051).
func TestWriteRemoteUpdateIssueFieldsAddsOpenSetValue(t *testing.T) {
	const addPath = "/api/admin/customFieldSettings/bundles/enum/b-레인/values"
	type want struct {
		requests []string
		err      func(error) bool
	}
	refused := func(detail string) func(error) bool {
		return func(err error) bool {
			var re *ytwrite.RefusedError
			return errors.As(err, &re) && re.Reason == ytwrite.ReasonFieldRefused && strings.Contains(re.Detail, detail)
		}
	}
	notSent := func(err error) bool { var ns *ytwrite.NotSentError; return errors.As(err, &ns) }
	ok := func(err error) bool { return err == nil }
	get, post := "GET /api/admin/projects/0-7/customFields", "POST /api/issues/2-9"
	for name, tc := range map[string]struct {
		lane    string
		addCode int
		// already: the bundle holds the value once the add answers (a
		// concurrent add won).
		already bool
		want    want
	}{
		"missing_added":     {lane: "routing-policy", addCode: http.StatusOK, want: want{[]string{get, "POST " + addPath, post}, ok}},
		"existing_no_add":   {lane: "handoffkeep-design", want: want{[]string{get, post}, ok}},
		"archived_restored": {lane: "archived-lane", addCode: http.StatusOK, want: want{[]string{get, "POST " + addPath + "/레인-varch", post}, ok}},
		"add_403":           {lane: "routing-policy", addCode: http.StatusForbidden, want: want{[]string{get, "POST " + addPath}, refused(`field "레인" value "routing-policy": no permission to add bundle value`)}},
		"add_5xx":           {lane: "routing-policy", addCode: http.StatusBadGateway, want: want{[]string{get, "POST " + addPath}, notSent}},
		"add_already_exists": {lane: "routing-policy", addCode: http.StatusBadRequest, already: true,
			want: want{[]string{get, "POST " + addPath, get, post}, ok}},
		"add_400_still_missing": {lane: "routing-policy", addCode: http.StatusBadRequest,
			want: want{[]string{get, "POST " + addPath, get}, notSent}},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			added := false
			y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/admin/projects/0-7/customFields":
					fields := projectFieldsJSON()
					if added {
						for _, f := range fields {
							if f["field"].(map[string]any)["name"] == ytwrite.FieldLane {
								b := f["bundle"].(map[string]any)
								b["values"] = append(b["values"].([]map[string]any), map[string]any{"id": "new", "name": tc.lane, "archived": false})
							}
						}
					}
					_ = json.NewEncoder(w).Encode(fields)
				case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, addPath):
					added = tc.already || tc.addCode < 300
					w.WriteHeader(tc.addCode)
					_, _ = w.Write([]byte(`{"id":"new","name":"x","archived":false}`))
				case r.Method == http.MethodPost && r.URL.Path == "/api/issues/2-9":
					_, _ = w.Write([]byte(`{"id":"2-9"}`))
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}}
			snap := fieldsSnapshot()
			setField(&snap, ytwrite.FieldLane, tc.lane)
			_, err := y.serve(t).UpdateIssueFields(t.Context(), ytwrite.IssueFieldsUpdate{IssueID: "2-9", ProjectID: "0-7", IssueSnapshot: snap})
			if !tc.want.err(err) {
				t.Fatalf("err %v", err)
			}
			if strings.Join(y.requests, "|") != strings.Join(tc.want.requests, "|") {
				t.Fatalf("requests %q, want %q", y.requests, tc.want.requests)
			}
			for i, req := range y.requests {
				if !strings.HasPrefix(req, "POST "+addPath) {
					continue
				}
				var body map[string]any
				if err := json.Unmarshal([]byte(y.bodies[i]), &body); err != nil {
					t.Fatal(err)
				}
				wantBody := map[string]any{"$type": "EnumBundleElement", "name": tc.lane}
				if strings.HasSuffix(req, "/레인-varch") {
					wantBody = map[string]any{"$type": "EnumBundleElement", "archived": false}
				}
				if !reflect.DeepEqual(body, wantBody) {
					t.Fatalf("add body %v, want %v", body, wantBody)
				}
			}
			if last := len(y.requests) - 1; last >= 0 && y.requests[last] == post {
				var body struct {
					CustomFields []struct {
						Name  string          `json:"name"`
						Value json.RawMessage `json:"value"`
					} `json:"customFields"`
				}
				_ = json.Unmarshal([]byte(y.bodies[last]), &body)
				for _, f := range body.CustomFields {
					if f.Name == ytwrite.FieldLane && string(f.Value) != `{"$type":"EnumBundleElement","name":`+quoteJSON(tc.lane)+`}` {
						t.Fatalf("레인 sent as %s", f.Value)
					}
				}
			}
		})
	}
}

func quoteJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
