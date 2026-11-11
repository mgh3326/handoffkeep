package youtrack

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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
			for _, v := range values {
				vs = append(vs, map[string]any{"name": v, "archived": false})
			}
			vs = append(vs, map[string]any{"name": "archived-lane", "archived": true})
			f["bundle"] = map[string]any{"values": vs}
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
		"missing_value":  {func(s *ytwrite.IssueSnapshot) { s.Fields[4].Value = "no-such-lane" }, `field "레인" value "no-such-lane"`},
		"archived_value": {func(s *ytwrite.IssueSnapshot) { s.Fields[4].Value = "archived-lane" }, `field "레인" value "archived-lane"`},
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

func quoteJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
