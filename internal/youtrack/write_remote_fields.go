package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
// "date", "period", "text", ...) and, for a bundle field, the bundle's id and
// every value it holds, archived ones too.
type fieldShape struct {
	typeID   string
	bundle   bool
	bundleID string
	values   map[string]bundleValue
}

// bundleValue is one value of a field's bundle.
type bundleValue struct {
	id       string
	archived bool
}

// live reports whether the bundle holds the value unarchived.
func (s fieldShape) live(value string) bool {
	v, ok := s.values[value]
	return ok && !v.archived
}

// findValue resolves value to the bundle's own spelling and entry: the exact
// name when the bundle holds it, else the value that differs only in case —
// YouTrack enum names are case-insensitively unique, so a case variant is the
// same value (hk 4234). A live value wins over an archived one; the
// lexicographically smallest name keeps the answer deterministic. ok is
// false when the bundle holds no such value.
func (s fieldShape) findValue(value string) (name string, v bundleValue, ok bool) {
	if v, ok := s.values[value]; ok {
		return value, v, true
	}
	found := false
	for n, cand := range s.values {
		if !strings.EqualFold(n, value) {
			continue
		}
		if !found || (s.values[name].archived && !cand.archived) ||
			(s.values[name].archived == cand.archived && n < name) {
			name, v, found = n, cand, true
		}
	}
	return name, v, found
}

// projectFieldShapes reads the project's fields in one request.
func (r *WriteRemote) projectFieldShapes(ctx context.Context, projectID string) (map[string]fieldShape, error) {
	q := url.Values{}
	q.Set("fields", "field(name,fieldType(id)),bundle(id,values(id,name,archived))")
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
			ID     string `json:"id"`
			Values []struct {
				ID       string `json:"id"`
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
		s := fieldShape{typeID: f.Field.FieldType.ID, values: map[string]bundleValue{}}
		if f.Bundle != nil {
			s.bundle, s.bundleID = true, f.Bundle.ID
			for _, v := range f.Bundle.Values {
				// A live value wins over an archived one of the same name.
				if have, ok := s.values[v.Name]; ok && !have.archived {
					continue
				}
				s.values[v.Name] = bundleValue{id: v.ID, archived: v.Archived}
			}
		}
		out[f.Field.Name] = s
	}
	return out, nil
}

// bundleKindPath is the admin bundles path segment and element type of a
// bundle field's type id; ok is false for a type hk does not add values to.
func bundleKindPath(typeID string) (kindPath, elementType string, ok bool) {
	switch typeID {
	case "enum[1]", "enum[*]":
		return "enum", "EnumBundleElement", true
	case "state[1]":
		return "state", "StateBundleElement", true
	}
	return "", "", false
}

// errNoBundlePermission is a 403 from a bundle value add.
var errNoBundlePermission = errors.New("no permission to add bundle value")

// ensureBundleValue makes value a live value of the field's bundle: an
// archived value is un-archived (never duplicated), a missing one is added
// with one POST to the bundle itself, so a bundle several projects share
// gains it for all of them. A 403 is errNoBundlePermission; another 4xx
// (an "already exists" from a concurrent add) re-reads the project's fields
// and succeeds when the value is now live. Each change is logged.
func (r *WriteRemote) ensureBundleValue(ctx context.Context, projectID, field string, s fieldShape, value string) error {
	kindPath, elementType, ok := bundleKindPath(s.typeID)
	if !ok || s.bundleID == "" {
		return fmt.Errorf("field %q: a %s bundle takes no added value", field, s.typeID)
	}
	base := "/api/admin/customFieldSettings/bundles/" + kindPath + "/" + url.PathEscape(s.bundleID) + "/values"
	q := url.Values{}
	q.Set("fields", "id,name,archived")
	path, body, action := base, any(struct {
		Type string `json:"$type"`
		Name string `json:"name"`
	}{elementType, value}), "added"
	if _, v, ok := s.findValue(value); ok && v.archived {
		path, action = base+"/"+url.PathEscape(v.id), "unarchived"
		body = struct {
			Type     string `json:"$type"`
			Archived bool   `json:"archived"`
		}{elementType, false}
	}
	_, _, err := r.send(ctx, http.MethodPost, path, q, body)
	var re *ytwrite.RemoteError
	switch {
	case err == nil:
	case errors.As(err, &re) && re.Status == http.StatusForbidden:
		return fmt.Errorf("%w: %v", errNoBundlePermission, err)
	case errors.As(err, &re) && re.Status >= 400 && re.Status < 500 &&
		re.Status != http.StatusUnauthorized && re.Status != http.StatusTooManyRequests && re.Status != http.StatusRequestTimeout:
		shapes, rerr := r.projectFieldShapes(ctx, projectID)
		if rerr != nil {
			return rerr
		}
		if _, v, found := shapes[field].findValue(value); !found || v.archived {
			return err
		}
		action = "present"
	default:
		return err
	}
	log.Printf("ytwrite.bundle_value_added project_id=%s field=%q value=%q bundle_id=%s action=%s", projectID, field, value, s.bundleID, action)
	return nil
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
	if s.bundle && fv.Value != "" && !s.live(fv.Value) && !ytwrite.OpenSetField(fv.Name) {
		return refuse("the value is not in the field's bundle (or it is archived)")
	}
	return f, nil
}

// UpdateIssueFields reads the project's fields (one GET; a failure is a
// *ytwrite.NotSentError), builds every snapshot field in the shape the
// project's field takes, and refuses (*ytwrite.RefusedError,
// ytwrite.ReasonFieldRefused) a closed-set value its bundle lacks, a field the
// project lacks with a value to set, or a type the snapshot cannot be written
// as, so last_error names the field and the value. Only when nothing is
// refused, an open-set value (ytwrite.OpenSetField) missing from or
// archived in its bundle is added or un-archived (ensureBundleValue): a 403
// is refused, any other failure is a *ytwrite.NotSentError. An open-set
// value that differs from a bundle value only in case is sent with the
// bundle's spelling, and an archived one is un-archived, never duplicated
// (hk 4234). Then it makes one POST
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
	type bundleAdd struct {
		field, value string
		shape        fieldShape
	}
	var adds []bundleAdd
	for _, fv := range in.Fields {
		s, ok := shapes[fv.Name]
		if !ok {
			if fv.Value != "" {
				refused = append(refused, fmt.Sprintf("field %q value %q: project %s has no such field", fv.Name, fv.Value, in.ProjectID))
			}
			continue
		}
		if s.bundle && fv.Value != "" && ytwrite.OpenSetField(fv.Name) {
			if name, _, found := s.findValue(fv.Value); found {
				// The bundle's spelling goes on the wire: YouTrack resolves
				// enum names case-insensitively but refuses a case variant
				// as a new bundle value (hk 4234).
				fv.Value = name
			}
		}
		f, ferr := issueFieldValue(fv, s)
		if ferr != nil {
			refused = append(refused, ferr.Error())
			continue
		}
		if s.bundle && fv.Value != "" && !s.live(fv.Value) {
			if _, _, ok := bundleKindPath(s.typeID); !ok || s.bundleID == "" {
				refused = append(refused, fmt.Sprintf("field %q value %q: the value is not in the field's bundle and a %s bundle takes no added value", fv.Name, fv.Value, s.typeID))
				continue
			}
			adds = append(adds, bundleAdd{fv.Name, fv.Value, s})
		}
		fields = append(fields, f)
	}
	if len(refused) > 0 {
		return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonFieldRefused, Detail: strings.Join(refused, "; ")}
	}
	done := map[string]bool{}
	for _, a := range adds {
		key := a.shape.bundleID + "\x00" + a.value
		if done[key] {
			continue
		}
		if err := r.ensureBundleValue(ctx, in.ProjectID, a.field, a.shape, a.value); err != nil {
			if errors.Is(err, errNoBundlePermission) {
				return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonFieldRefused,
					Detail: fmt.Sprintf("field %q value %q: %v", a.field, a.value, err)}
			}
			return "", &ytwrite.NotSentError{Err: err}
		}
		done[key] = true
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
	q.Set("fields", "summary,description,customFields(name,value(name,minutes,text),field(fieldType(id)))")
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
			Field struct {
				FieldType struct {
					ID string `json:"id"`
				} `json:"fieldType"`
			} `json:"field"`
		} `json:"customFields"`
	}
	if err := json.Unmarshal(data, &issue); err != nil {
		return ytwrite.IssueSnapshot{}, fmt.Errorf("youtrack GET issue %s: decode: %w", issueID, err)
	}
	out := ytwrite.IssueSnapshot{Summary: issue.Summary, Description: issue.Description, Fields: []ytwrite.FieldValue{}}
	for _, f := range issue.CustomFields {
		out.Fields = append(out.Fields, ytwrite.FieldValue{Name: f.Name, Type: fieldValueType(f.Field.FieldType.ID), Value: canonicalFieldValue(f.Value)})
	}
	return out, nil
}

// fieldValueType maps a YouTrack field type id to the snapshot FieldType the
// evidence comparison reads: enum and state are the bundle-backed kinds an
// open-set value may fold case on.
func fieldValueType(typeID string) ytwrite.FieldType {
	if _, _, ok := bundleKindPath(typeID); ok {
		if typeID == "state[1]" {
			return ytwrite.FieldTypeState
		}
		return ytwrite.FieldTypeEnum
	}
	switch typeID {
	case "integer":
		return ytwrite.FieldTypeInt
	case "date", "date and time":
		return ytwrite.FieldTypeDate
	case "period":
		return ytwrite.FieldTypePeriod
	case "string", "text":
		return ytwrite.FieldTypeText
	}
	return ""
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
