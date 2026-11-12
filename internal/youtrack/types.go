package youtrack

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// User is a YouTrack account. ID and login are the two fields that separate
// hk-bot's writes from the operator's.
type User struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Type  string `json:"$type"`
}

// ProjectRef is the project an issue belongs to.
type ProjectRef struct {
	ID        string `json:"id"`
	ShortName string `json:"shortName"`
	Name      string `json:"name"`
	Type      string `json:"$type"`
}

// IssueField is one custom field value on an issue. Value stays raw because a
// simple field is a scalar while state and enum fields are objects.
type IssueField struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Type  string          `json:"$type"`
	Value json.RawMessage `json:"value"`
}

// Issue is the bounded issue projection the sync reads and writes.
type Issue struct {
	ID           string       `json:"id"`
	IDReadable   string       `json:"idReadable"`
	Summary      string       `json:"summary"`
	Description  string       `json:"description"`
	Created      int64        `json:"created"`
	Project      *ProjectRef  `json:"project"`
	CustomFields []IssueField `json:"customFields"`
	Type         string       `json:"$type"`
}

// Comment is one YouTrack comment.
type Comment struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Created int64  `json:"created"`
	Author  *User  `json:"author"`
	Deleted bool   `json:"deleted"`
	Type    string `json:"$type"`
}

// ActivityField is the field a CustomFieldCategory activity changed.
type ActivityField struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"$type"`
}

// ActivityCategory groups one kind of change in an activity.
type ActivityCategory struct {
	Type    string            `json:"$type"`
	Field   *ActivityField    `json:"field"`
	Added   []json.RawMessage `json:"added"`
	Removed []json.RawMessage `json:"removed"`
}

// Activity is one activitiesPage row.
type Activity struct {
	ID         string             `json:"id"`
	Timestamp  int64              `json:"timestamp"`
	Author     *User              `json:"author"`
	Target     *Issue             `json:"target"`
	Categories []ActivityCategory `json:"categories"`
	Added      []json.RawMessage  `json:"added"`
	Removed    []json.RawMessage  `json:"removed"`
}

// ActivitiesPage is one cursor page from /api/activitiesPage.
type ActivitiesPage struct {
	Activities  []Activity `json:"activities"`
	AfterCursor string     `json:"afterCursor"`
	HasAfter    bool       `json:"hasAfter"`
}

// Me returns the account behind the configured token. The sync calls it once
// so it can tell hk-bot's own writes from the operator's by ID, not by a
// display name that an operator could change.
func (c *Client) Me(ctx context.Context) (User, error) {
	c.meOnce.Do(func() {
		c.me, c.meErr = c.fetchMe(ctx)
	})
	return c.me, c.meErr
}

func (c *Client) fetchMe(ctx context.Context) (User, error) {
	q := url.Values{}
	q.Set("fields", "id,login,name,$type")
	var out User
	if err := c.get(ctx, "/api/users/me", q, &out); err != nil {
		return User{}, err
	}
	return out, nil
}

// CustomField is one field to write on an issue. Type is the issue-field
// $type; Value is a string, an int, or an enum object. A nil Value is sent as
// JSON null, which clears the field on an update (see EmptyEnumField).
type CustomField struct {
	Type  string `json:"$type"`
	Name  string `json:"name"`
	Value any    `json:"value"`
}

type EnumValue struct {
	Type string `json:"$type"`
	Name string `json:"name"`
}

const (
	fieldSimple     = "SimpleIssueCustomField"
	fieldSingleEnum = "SingleEnumIssueCustomField"
	fieldState      = "StateIssueCustomField"
)

func StringField(name, value string) CustomField {
	return CustomField{Type: fieldSimple, Name: name, Value: value}
}

func IntField(name string, value int) CustomField {
	return CustomField{Type: fieldSimple, Name: name, Value: value}
}

func EnumField(name, value string) CustomField {
	return CustomField{Type: fieldSingleEnum, Name: name, Value: EnumValue{Type: "EnumBundleElement", Name: value}}
}

// EmptyEnumField clears an enum field on an update.
func EmptyEnumField(name string) CustomField {
	return CustomField{Type: fieldSingleEnum, Name: name, Value: nil}
}

// DateField writes a date as epoch milliseconds, or clears it when unset.
func DateField(name string, ms *int64) CustomField {
	f := CustomField{Type: "DateIssueCustomField", Name: name}
	if ms != nil {
		f.Value = *ms
	}
	return f
}

func StateField(name, value string, resolved bool) CustomField {
	return CustomField{Type: fieldState, Name: name, Value: struct {
		Type       string `json:"$type"`
		Name       string `json:"name"`
		IsResolved bool   `json:"isResolved,omitempty"`
	}{Type: "StateBundleElement", Name: value, IsResolved: resolved}}
}

// FieldString reads a scalar or object field value as a trimmed string.
func (f IssueField) StringValue() string {
	value := strings.TrimSpace(string(f.Value))
	if value == "" || value == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(f.Value, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Name         string `json:"name"`
		Text         string `json:"text"`
		Presentation string `json:"presentation"`
	}
	if json.Unmarshal(f.Value, &obj) == nil {
		for _, candidate := range []string{obj.Name, obj.Text, obj.Presentation} {
			if strings.TrimSpace(candidate) != "" {
				return strings.TrimSpace(candidate)
			}
		}
	}
	return ""
}

// IntValue reads a simple integer field.
func (f IssueField) IntValue() (int, bool) {
	value := strings.TrimSpace(string(f.Value))
	if value == "" || value == "null" {
		return 0, false
	}
	if n, err := strconv.Atoi(value); err == nil {
		return n, true
	}
	var obj struct {
		Value *int `json:"value"`
	}
	if json.Unmarshal(f.Value, &obj) == nil && obj.Value != nil {
		return *obj.Value, true
	}
	return 0, false
}

// IssueField finds a custom field by name.
func IssueFieldByName(fields []IssueField, name string) (IssueField, bool) {
	for _, f := range fields {
		if sameFieldName(f.Name, name) {
			return f, true
		}
	}
	return IssueField{}, false
}

func sameFieldName(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// ActivityValueName reads the name of an added/removed activity value.
func ActivityValueName(raw json.RawMessage) string {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	f := IssueField{Value: raw}
	return f.StringValue()
}

// ActivityValueInt reads an integer activity value, whether YouTrack sent a
// bare number or an object with a value member.
func ActivityValueInt(raw json.RawMessage) (int, bool) {
	f := IssueField{Value: raw}
	return f.IntValue()
}

// ActivityValueText reads a comment's text from an activity value.
func ActivityValueText(raw json.RawMessage) string {
	var c Comment
	if json.Unmarshal(raw, &c) == nil && c.Text != "" {
		return c.Text
	}
	var obj struct {
		Text    string `json:"text"`
		Comment struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"comment"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Text != "" {
			return obj.Text
		}
		if obj.Comment.Text != "" {
			return obj.Comment.Text
		}
	}
	return ""
}

// ActivityValueID reads the object id of a comment activity value.
func ActivityValueID(raw json.RawMessage) string {
	var obj struct {
		ID      string `json:"id"`
		Comment struct {
			ID string `json:"id"`
		} `json:"comment"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.ID != "" {
			return obj.ID
		}
		if obj.Comment.ID != "" {
			return obj.Comment.ID
		}
	}
	return ""
}
