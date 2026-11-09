package youtrack

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const issueFields = "id,idReadable,summary,description,$type,project(id,shortName,name,$type),customFields(id,name,$type,value)"

// IssueWrite is one create call.
type IssueWrite struct {
	ProjectID   string
	Summary     string
	Description string
	Fields      []CustomField
}

type issueBody struct {
	Project struct {
		ID string `json:"id"`
	} `json:"project"`
	Summary      string        `json:"summary"`
	Description  string        `json:"description,omitempty"`
	CustomFields []CustomField `json:"customFields,omitempty"`
}

// FindIssueByField finds the issue in one project whose integer field equals
// n: the hk ID of a task issue or the 결정 ID of a decision issue. The search
// is scoped to the project and each hit is checked against both the project
// id and the field value, because the same number can exist in another
// project (a DEC issue carries its task's hk ID). It returns (Issue{}, false,
// nil) when none exists, so the caller creates instead of updating.
func (c *Client) FindIssueByField(ctx context.Context, projectShort, projectID, field string, n int64) (Issue, bool, error) {
	q := url.Values{}
	// YouTrack's search grammar uses braces around a field name that contains
	// spaces; a quoted name is parsed as a text search.
	query := fmt.Sprintf(`{%s}: %d`, field, n)
	if projectShort != "" {
		query = fmt.Sprintf(`project: %s %s`, projectShort, query)
	}
	q.Set("query", query)
	q.Set("fields", issueFields)
	q.Set("$top", "10")
	var issues []Issue
	if err := c.get(ctx, "/api/issues", q, &issues); err != nil {
		return Issue{}, false, err
	}
	for _, issue := range issues {
		if projectID != "" && (issue.Project == nil || issue.Project.ID != projectID) {
			continue
		}
		if f, ok := IssueFieldByName(issue.CustomFields, field); ok {
			if v, ok := f.IntValue(); ok && int64(v) == n {
				return issue, true, nil
			}
		}
	}
	return Issue{}, false, nil
}

func (c *Client) GetIssue(ctx context.Context, issueID string) (Issue, error) {
	q := url.Values{}
	q.Set("fields", issueFields)
	var out Issue
	err := c.get(ctx, "/api/issues/"+url.PathEscape(issueID), q, &out)
	if IsNotFound(err) {
		return Issue{}, fmt.Errorf("%w: issue %s", ErrNotFound, issueID)
	}
	return out, err
}

func (c *Client) CreateIssue(ctx context.Context, in IssueWrite) (Issue, error) {
	if strings.TrimSpace(in.ProjectID) == "" || strings.TrimSpace(in.Summary) == "" {
		return Issue{}, fmt.Errorf("%w: project and summary are required", ErrConfig)
	}
	// A nil value is a clear, which only an update can express; a new issue
	// simply has no value for that field.
	fields := make([]CustomField, 0, len(in.Fields))
	for _, f := range in.Fields {
		if f.Value != nil {
			fields = append(fields, f)
		}
	}
	body := issueBody{Summary: in.Summary, Description: in.Description, CustomFields: fields}
	body.Project.ID = in.ProjectID
	q := url.Values{}
	q.Set("fields", issueFields)
	var out Issue
	err := c.postOnce(ctx, "/api/issues", q, body, &out)
	return out, err
}

// WriteIssue updates an existing issue: the description when it is non-nil,
// and the custom fields. A nil description leaves the stored one alone.
func (c *Client) WriteIssue(ctx context.Context, issueID string, description *string, fields []CustomField) (Issue, error) {
	q := url.Values{}
	q.Set("fields", issueFields)
	var out Issue
	body := struct {
		Description  *string       `json:"description,omitempty"`
		CustomFields []CustomField `json:"customFields,omitempty"`
	}{Description: description, CustomFields: fields}
	err := c.post(ctx, "/api/issues/"+url.PathEscape(issueID), q, body, &out)
	return out, err
}

// UpdateFields writes custom fields on an existing issue. YouTrack uses POST
// for an issue update; it is idempotent for the same value.
func (c *Client) UpdateFields(ctx context.Context, issueID string, fields []CustomField) (Issue, error) {
	if len(fields) == 0 {
		return c.GetIssue(ctx, issueID)
	}
	q := url.Values{}
	q.Set("fields", issueFields)
	var out Issue
	err := c.post(ctx, "/api/issues/"+url.PathEscape(issueID), q, struct {
		CustomFields []CustomField `json:"customFields"`
	}{fields}, &out)
	return out, err
}

func (c *Client) AddComment(ctx context.Context, issueID, text string) (Comment, error) {
	q := url.Values{}
	q.Set("fields", "id,text,created,author(id,login,name,$type)")
	var out Comment
	err := c.postOnce(ctx, "/api/issues/"+url.PathEscape(issueID)+"/comments", q, struct {
		Text string `json:"text"`
	}{text}, &out)
	return out, err
}

// ListComments returns one bounded page, oldest first.
func (c *Client) ListComments(ctx context.Context, issueID string, skip, top int) ([]Comment, error) {
	if top < 1 || top > 1000 {
		top = 100
	}
	q := url.Values{}
	q.Set("fields", "id,text,created,author(id,login,name,$type)")
	q.Set("$top", strconv.Itoa(top))
	if skip > 0 {
		q.Set("$skip", strconv.Itoa(skip))
	}
	var out []Comment
	err := c.get(ctx, "/api/issues/"+url.PathEscape(issueID)+"/comments", q, &out)
	return out, err
}

func (c *Client) SetStateValue(ctx context.Context, issueID, field, value string, resolved bool) error {
	_, err := c.UpdateFields(ctx, issueID, []CustomField{StateField(field, value, resolved)})
	return err
}

func (c *Client) SetPriority(ctx context.Context, issueID, field string, value int) error {
	_, err := c.UpdateFields(ctx, issueID, []CustomField{IntField(field, value)})
	return err
}

func (c *Client) SetLane(ctx context.Context, issueID, field, value string) error {
	_, err := c.UpdateFields(ctx, issueID, []CustomField{EnumField(field, value)})
	return err
}

// MoveIssue puts an issue in another project. The issue's stable internal id
// and its hk link do not change; only the display number does.
func (c *Client) MoveIssue(ctx context.Context, issueID, projectID string) (Issue, error) {
	if strings.TrimSpace(projectID) == "" {
		return Issue{}, fmt.Errorf("%w: project id is empty", ErrConfig)
	}
	q := url.Values{}
	q.Set("fields", issueFields)
	var out Issue
	err := c.post(ctx, "/api/issues/"+url.PathEscape(issueID), q, struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}{Project: struct {
		ID string `json:"id"`
	}{ID: projectID}}, &out)
	return out, err
}

// FieldInfo is the project custom field and the values its bundle accepts.
type FieldInfo struct {
	Name       string
	BundleID   string
	BundleType string
	Values     map[string]string
}

// ProjectID resolves a project short name or name to its YouTrack id.
func (c *Client) ProjectID(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: project name is empty", ErrConfig)
	}
	q := url.Values{}
	q.Set("fields", "id,shortName,name,$type")
	q.Set("$top", "100")
	var projects []ProjectRef
	if err := c.get(ctx, "/api/admin/projects", q, &projects); err != nil {
		return "", err
	}
	for _, p := range projects {
		if strings.EqualFold(p.ShortName, name) || strings.EqualFold(p.Name, name) {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrProjectMissing, name)
}

// ProjectFieldValues reads the field definition and its bundle values.
func (c *Client) ProjectFieldValues(ctx context.Context, projectID, fieldName string) (FieldInfo, error) {
	q := url.Values{}
	q.Set("fields", "id,field(id,name,$type),bundle(id,$type,values(id,name,$type))")
	var rows []struct {
		Field struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"$type"`
		} `json:"field"`
		Bundle *struct {
			ID     string `json:"id"`
			Type   string `json:"$type"`
			Values []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Type string `json:"$type"`
			} `json:"values"`
		} `json:"bundle"`
	}
	path := "/api/admin/projects/" + url.PathEscape(projectID) + "/customFields"
	if err := c.get(ctx, path, q, &rows); err != nil {
		return FieldInfo{}, err
	}
	for _, row := range rows {
		if !sameFieldName(row.Field.Name, fieldName) {
			continue
		}
		info := FieldInfo{Name: row.Field.Name, Values: map[string]string{}}
		if row.Bundle == nil {
			return info, nil
		}
		info.BundleID = row.Bundle.ID
		info.BundleType = row.Bundle.Type
		for _, v := range row.Bundle.Values {
			info.Values[normalizeLabel(v.Name)] = v.Name
		}
		return info, nil
	}
	return FieldInfo{}, fmt.Errorf("%w: %q on project %s", ErrFieldNotFound, fieldName, projectID)
}

// AddEnumValue adds a missing value to the field's bundle. The caller decides
// whether the value is resolved (state bundles only).
func (c *Client) AddEnumValue(ctx context.Context, info FieldInfo, value string, resolved bool) error {
	if info.BundleID == "" {
		return fmt.Errorf("%w: %q has no bundle", ErrFieldNotFound, info.Name)
	}
	bundlePath, elementType := "", ""
	switch info.BundleType {
	case "StateBundle":
		bundlePath, elementType = "state", "StateBundleElement"
	case "EnumBundle":
		bundlePath, elementType = "enum", "EnumBundleElement"
	default:
		return fmt.Errorf("%w: %q has bundle type %q", ErrFieldNotFound, info.Name, info.BundleType)
	}
	body := map[string]any{"$type": elementType, "name": value}
	if bundlePath == "state" && resolved {
		body["isResolved"] = true
	}
	path := "/api/admin/customFieldSettings/bundles/" + bundlePath + "/" + url.PathEscape(info.BundleID) + "/values"
	return c.postOnce(ctx, path, nil, body, nil)
}

// WriteFields is the one update helper the service uses after it has resolved
// every field's $type and ensured its enum values exist.
func (c *Client) WriteFields(ctx context.Context, issueID string, fields []CustomField) error {
	_, err := c.UpdateFields(ctx, issueID, fields)
	return err
}
