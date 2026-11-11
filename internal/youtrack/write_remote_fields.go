package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.IssueFieldsRemote = (*WriteRemote)(nil)

// fieldShape is one project field as the issue_fields update needs it: its
// global type id ("string", "integer", "enum[1]", "enum[*]", "state[1]",
// "date", "period", "text", ...) and, for a bundle field, its live values.
type fieldShape struct {
	typeID string
	values map[string]bool
	bundle bool
}

// projectFieldShapes reads the project's fields in one request.
func (r *WriteRemote) projectFieldShapes(ctx context.Context, projectID string) (map[string]fieldShape, error) {
	q := url.Values{}
	q.Set("fields", "field(name,fieldType(id)),bundle(values(name,archived))")
	q.Set("$top", strconv.Itoa(issueFieldsTop))
	path := "/api/admin/projects/" + url.PathEscape(projectID) + "/customFields"
	data, readErr, err := r.send(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	var fields []struct {
		Field struct {
			Name      string `json:"name"`
			FieldType struct {
				ID string `json:"id"`
			} `json:"fieldType"`
		} `json:"field"`
		Bundle *struct {
			Values []struct {
				Name     string `json:"name"`
				Archived bool   `json:"archived"`
			} `json:"values"`
		} `json:"bundle"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("youtrack GET %s: decode: %w", path, err)
	}
	if len(fields) >= issueFieldsTop {
		return nil, fmt.Errorf("youtrack GET %s: %d or more fields; the listing is not complete", path, issueFieldsTop)
	}
	out := make(map[string]fieldShape, len(fields))
	for _, f := range fields {
		s := fieldShape{typeID: f.Field.FieldType.ID, values: map[string]bool{}}
		if f.Bundle != nil {
			s.bundle = true
			for _, v := range f.Bundle.Values {
				if !v.Archived {
					s.values[v.Name] = true
				}
			}
		}
		out[f.Field.Name] = s
	}
	return out, nil
}

// issueFieldValue is one field of the update in the wire shape the
// project's field takes. A refusal names the field and the value.
func issueFieldValue(fv ytwrite.FieldValue, s fieldShape) (CustomField, error) {
	refuse := func(why string) (CustomField, error) {
		return CustomField{}, fmt.Errorf("field %q value %q: %s", fv.Name, fv.Value, why)
	}
	f := CustomField{Name: fv.Name}
	typeID := s.typeID
	switch {
	case typeID == "enum[1]" || typeID == "state[1]":
		elem := "EnumBundleElement"
		f.Type = fieldSingleEnum
		if typeID == "state[1]" {
			f.Type, elem = fieldState, "StateBundleElement"
		}
		if fv.Type != ytwrite.FieldTypeEnum && fv.Type != ytwrite.FieldTypeState && fv.Type != ytwrite.FieldTypeText {
			return refuse("the field is " + typeID)
		}
		if fv.Value != "" {
			f.Value = EnumValue{Type: elem, Name: fv.Value}
		}
	case typeID == "enum[*]":
		f.Type = "MultiEnumIssueCustomField"
		if fv.Type != ytwrite.FieldTypeEnum {
			return refuse("the field is " + typeID)
		}
		values := []EnumValue{}
		if fv.Value != "" {
			values = append(values, EnumValue{Type: "EnumBundleElement", Name: fv.Value})
		}
		f.Value = values
	case typeID == "string":
		f.Type = fieldSimple
		if fv.Value != "" {
			f.Value = fv.Value
		}
	case typeID == "integer":
		f.Type = fieldSimple
		if fv.Type != ytwrite.FieldTypeInt {
			return refuse("the field is " + typeID)
		}
		if fv.Value != "" {
			n, err := strconv.ParseInt(fv.Value, 10, 64)
			if err != nil {
				return refuse("not an integer")
			}
			f.Value = n
		}
	case typeID == "date" || typeID == "date and time":
		f.Type = "DateIssueCustomField"
		if fv.Type != ytwrite.FieldTypeDate {
			return refuse("the field is " + typeID)
		}
		if fv.Value != "" {
			ms, err := strconv.ParseInt(fv.Value, 10, 64)
			if err != nil {
				return refuse("not epoch milliseconds")
			}
			f.Value = ms
		}
	case typeID == "period":
		f.Type = "PeriodIssueCustomField"
		if fv.Type != ytwrite.FieldTypePeriod {
			return refuse("the field is " + typeID)
		}
		if fv.Value != "" {
			minutes, err := strconv.ParseInt(fv.Value, 10, 64)
			if err != nil {
				return refuse("not whole minutes")
			}
			f.Value = struct {
				Type    string `json:"$type"`
				Minutes int64  `json:"minutes"`
			}{"PeriodValue", minutes}
		}
	case typeID == "text":
		f.Type = "TextIssueCustomField"
		if fv.Value != "" {
			f.Value = struct {
				Type string `json:"$type"`
				Text string `json:"text"`
			}{"TextFieldValue", fv.Value}
		}
	default:
		return refuse("the field type " + strconv.Quote(typeID) + " is not one hk writes")
	}
	if s.bundle && fv.Value != "" && !s.values[fv.Value] {
		return refuse("the value is not in the field's bundle (or it is archived)")
	}
	return f, nil
}

// UpdateIssueFields reads the project's fields (one GET; a failure is a
// *ytwrite.NotSentError), builds every snapshot field in the shape the
// project's field takes, and refuses (*ytwrite.RefusedError,
// ytwrite.ReasonFieldRefused) a value its bundle lacks, a field the project
// lacks with a value to set, or a type the snapshot cannot be written as, so
// last_error names the field and the value. Then it makes one POST
// /api/issues/{id} with the summary, the description and every field ("" is
// sent as null, or [] for a multi-value field, which clears it). A field the
// project lacks with no value is left out.
func (r *WriteRemote) UpdateIssueFields(ctx context.Context, in ytwrite.IssueFieldsUpdate) (string, error) {
	shapes, err := r.projectFieldShapes(ctx, in.ProjectID)
	if err != nil {
		return "", &ytwrite.NotSentError{Err: err}
	}
	fields := make([]CustomField, 0, len(in.Fields))
	var refused []string
	for _, fv := range in.Fields {
		s, ok := shapes[fv.Name]
		if !ok {
			if fv.Value != "" {
				refused = append(refused, fmt.Sprintf("field %q value %q: project %s has no such field", fv.Name, fv.Value, in.ProjectID))
			}
			continue
		}
		f, ferr := issueFieldValue(fv, s)
		if ferr != nil {
			refused = append(refused, ferr.Error())
			continue
		}
		fields = append(fields, f)
	}
	if len(refused) > 0 {
		return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonFieldRefused, Detail: strings.Join(refused, "; ")}
	}
	body := struct {
		Summary      string        `json:"summary"`
		Description  string        `json:"description"`
		CustomFields []CustomField `json:"customFields"`
	}{in.Summary, in.Description, fields}
	q := url.Values{}
	q.Set("fields", "id")
	data, readErr, err := r.send(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(in.IssueID), q, body)
	if err != nil {
		return "", err
	}
	if readErr != nil {
		return "", nil
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &out) != nil {
		return "", nil
	}
	return out.ID, nil
}

// ReadIssueFields reads the issue's summary, description and custom fields
// in one GET, each field's value as canonical text: a number as decimal (a
// date as epoch milliseconds), an enum or state as its name, several enum
// values as their names sorted and joined by ",", a period as whole
// minutes, a text field as its text, null as "".
func (r *WriteRemote) ReadIssueFields(ctx context.Context, issueID string) (ytwrite.IssueSnapshot, error) {
	q := url.Values{}
	q.Set("fields", "summary,description,customFields(name,value(name,minutes,text))")
	data, readErr, err := r.send(ctx, http.MethodGet, "/api/issues/"+url.PathEscape(issueID), q, nil)
	if err != nil {
		return ytwrite.IssueSnapshot{}, err
	}
	if readErr != nil {
		return ytwrite.IssueSnapshot{}, readErr
	}
	var issue struct {
		Summary      string `json:"summary"`
		Description  string `json:"description"`
		CustomFields []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"customFields"`
	}
	if err := json.Unmarshal(data, &issue); err != nil {
		return ytwrite.IssueSnapshot{}, fmt.Errorf("youtrack GET issue %s: decode: %w", issueID, err)
	}
	out := ytwrite.IssueSnapshot{Summary: issue.Summary, Description: issue.Description, Fields: []ytwrite.FieldValue{}}
	for _, f := range issue.CustomFields {
		out.Fields = append(out.Fields, ytwrite.FieldValue{Name: f.Name, Value: canonicalFieldValue(f.Value)})
	}
	return out, nil
}

// canonicalFieldValue is the FieldValue text of one raw field value. A value
// it cannot read is returned as its raw JSON, which matches no snapshot.
func canonicalFieldValue(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			names := make([]string, 0, len(items))
			for _, it := range items {
				if v := canonicalFieldValue(it); v != "" {
					names = append(names, v)
				}
			}
			sort.Strings(names)
			return strings.Join(names, ",")
		}
	case '{':
		var obj struct {
			Name    *string `json:"name"`
			Minutes *int64  `json:"minutes"`
			Text    *string `json:"text"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			switch {
			case obj.Name != nil:
				return *obj.Name
			case obj.Minutes != nil:
				return strconv.FormatInt(*obj.Minutes, 10)
			case obj.Text != nil:
				return *obj.Text
			}
		}
	default:
		var n json.Number
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&n) == nil {
			return n.String()
		}
	}
	return string(raw)
}
