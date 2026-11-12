package youtrack

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// The administration calls the schema run makes (hk 3836). They read and add;
// nothing here deletes, renames or detaches.

// schemaPageSize is $top for the admin lists. A list that returns fewer rows
// than this is the last page.
const schemaPageSize = 100

// AdminProject is a project as the admin API lists it.
type AdminProject struct {
	ID        string `json:"id"`
	ShortName string `json:"shortName"`
	Name      string `json:"name"`
	Leader    *User  `json:"leader"`
}

// GlobalField is a custom field definition shared by projects. FieldType is the
// YouTrack type id: state[1], enum[1], date, period.
type GlobalField struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	FieldType struct {
		ID string `json:"id"`
	} `json:"fieldType"`
}

// ProjectField is one field attached to a project. Bundle is nil for a date or
// period field.
type ProjectField struct {
	ID    string `json:"id"`
	Type  string `json:"$type"`
	Field struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		FieldType struct {
			ID string `json:"id"`
		} `json:"fieldType"`
	} `json:"field"`
	Bundle *struct {
		ID   string `json:"id"`
		Type string `json:"$type"`
	} `json:"bundle"`
}

// BundleElement is one value of a bundle. IsResolved is read for state bundles
// only, which are the only ones that carry it.
type BundleElement struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Ordinal    int    `json:"ordinal"`
	IsResolved bool   `json:"isResolved"`
}

// Bundle is an enum or state bundle with its values.
type Bundle struct {
	ID     string          `json:"id"`
	Type   string          `json:"$type"`
	Name   string          `json:"name"`
	Values []BundleElement `json:"values"`
}

// LinkType is one issue link type.
type LinkType struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Directed       bool   `json:"directed"`
	SourceToTarget string `json:"sourceToTarget"`
	TargetToSource string `json:"targetToSource"`
}

func getAll[T any](ctx context.Context, c *Client, path, fields string) ([]T, error) {
	var all []T
	for skip := 0; ; skip += schemaPageSize {
		q := url.Values{}
		q.Set("fields", fields)
		q.Set("$top", strconv.Itoa(schemaPageSize))
		q.Set("$skip", strconv.Itoa(skip))
		var page []T
		if err := c.get(ctx, path, q, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < schemaPageSize {
			return all, nil
		}
	}
}

// ListAdminProjects lists every project with its leader.
func (c *Client) ListAdminProjects(ctx context.Context) ([]AdminProject, error) {
	return getAll[AdminProject](ctx, c, "/api/admin/projects", "id,shortName,name,leader(id,login,name)")
}

// CreateProject creates a project. YouTrack requires a leader.
func (c *Client) CreateProject(ctx context.Context, name, shortName, leaderID string) (AdminProject, error) {
	q := url.Values{}
	q.Set("fields", "id,shortName,name,leader(id,login,name)")
	body := map[string]any{"name": name, "shortName": shortName, "leader": map[string]any{"id": leaderID}}
	var out AdminProject
	if err := c.postOnce(ctx, "/api/admin/projects", q, body, &out); err != nil {
		return AdminProject{}, err
	}
	if out.ID == "" {
		return AdminProject{}, fmt.Errorf("youtrack POST /api/admin/projects: answer has no id")
	}
	return out, nil
}

// ListProjectFields lists the fields attached to one project.
func (c *Client) ListProjectFields(ctx context.Context, projectID string) ([]ProjectField, error) {
	return getAll[ProjectField](ctx, c, "/api/admin/projects/"+url.PathEscape(projectID)+"/customFields", "id,$type,field(id,name,fieldType(id)),bundle(id,$type)")
}

// ListGlobalFields lists the custom field definitions.
func (c *Client) ListGlobalFields(ctx context.Context) ([]GlobalField, error) {
	return getAll[GlobalField](ctx, c, "/api/admin/customFieldSettings/customFields", "id,name,fieldType(id)")
}

// CreateGlobalField defines a custom field of the given type id.
func (c *Client) CreateGlobalField(ctx context.Context, name, fieldType string) (GlobalField, error) {
	q := url.Values{}
	q.Set("fields", "id,name,fieldType(id)")
	body := map[string]any{"name": name, "fieldType": map[string]any{"id": fieldType}}
	var out GlobalField
	if err := c.postOnce(ctx, "/api/admin/customFieldSettings/customFields", q, body, &out); err != nil {
		return GlobalField{}, err
	}
	if out.ID == "" {
		return GlobalField{}, fmt.Errorf("youtrack POST /api/admin/customFieldSettings/customFields: answer has no id")
	}
	return out, nil
}

func bundlePath(kind FieldKind) (string, error) {
	switch kind {
	case KindState:
		return "state", nil
	case KindEnum, KindMultiEnum:
		return "enum", nil
	}
	return "", fmt.Errorf("field kind %d has no bundle", kind)
}

func bundleTypes(kind FieldKind) (bundleType, elementType string) {
	if kind == KindState {
		return "StateBundle", "StateBundleElement"
	}
	return "EnumBundle", "EnumBundleElement"
}

// GetBundle reads one bundle with its values. A state bundle is asked for
// isResolved, which an enum element does not have.
func (c *Client) GetBundle(ctx context.Context, kind FieldKind, id string) (Bundle, error) {
	kindPath, err := bundlePath(kind)
	if err != nil {
		return Bundle{}, err
	}
	fields := "id,$type,name,values(id,name,ordinal)"
	if kind == KindState {
		fields = "id,$type,name,values(id,name,ordinal,isResolved)"
	}
	q := url.Values{}
	q.Set("fields", fields)
	var out Bundle
	if err := c.get(ctx, "/api/admin/customFieldSettings/bundles/"+kindPath+"/"+url.PathEscape(id), q, &out); err != nil {
		return Bundle{}, err
	}
	return out, nil
}

func bundleElementBody(kind FieldKind, v BundleValue, withOrdinal bool) map[string]any {
	_, elementType := bundleTypes(kind)
	body := map[string]any{"$type": elementType, "name": v.Name}
	if kind == KindState {
		body["isResolved"] = v.Resolved
		if withOrdinal {
			body["ordinal"] = v.Ordinal
		}
	}
	return body
}

// CreateBundle creates a bundle holding values, in the order given.
func (c *Client) CreateBundle(ctx context.Context, kind FieldKind, name string, values []BundleValue) (Bundle, error) {
	kindPath, err := bundlePath(kind)
	if err != nil {
		return Bundle{}, err
	}
	bundleType, _ := bundleTypes(kind)
	elements := make([]map[string]any, 0, len(values))
	for _, v := range values {
		elements = append(elements, bundleElementBody(kind, v, false))
	}
	q := url.Values{}
	q.Set("fields", "id,$type,name,values(id,name,ordinal)")
	body := map[string]any{"$type": bundleType, "name": name, "values": elements}
	var out Bundle
	if err := c.postOnce(ctx, "/api/admin/customFieldSettings/bundles/"+kindPath, q, body, &out); err != nil {
		return Bundle{}, err
	}
	if out.ID == "" {
		return Bundle{}, fmt.Errorf("youtrack POST bundles/%s: answer has no id", kindPath)
	}
	return out, nil
}

// AddBundleValue adds one value to an existing bundle. A state value carries
// its resolved flag and ordinal.
func (c *Client) AddBundleValue(ctx context.Context, kind FieldKind, bundleID string, v BundleValue) (BundleElement, error) {
	kindPath, err := bundlePath(kind)
	if err != nil {
		return BundleElement{}, err
	}
	q := url.Values{}
	q.Set("fields", "id,name,ordinal")
	var out BundleElement
	path := "/api/admin/customFieldSettings/bundles/" + kindPath + "/" + url.PathEscape(bundleID) + "/values"
	if err := c.postOnce(ctx, path, q, bundleElementBody(kind, v, true), &out); err != nil {
		return BundleElement{}, err
	}
	return out, nil
}

// SetStateElement updates an existing state value's flag or ordinal. The name
// is never sent, so a value is never renamed.
func (c *Client) SetStateElement(ctx context.Context, bundleID, valueID string, patch map[string]any) error {
	q := url.Values{}
	q.Set("fields", "id,name,ordinal,isResolved")
	path := "/api/admin/customFieldSettings/bundles/state/" + url.PathEscape(bundleID) + "/values/" + url.PathEscape(valueID)
	return c.post(ctx, path, q, patch, nil)
}

// AttachProjectField attaches a field definition to a project. bundleID is
// empty for a field without a bundle.
func (c *Client) AttachProjectField(ctx context.Context, projectID string, kind FieldKind, fieldID, bundleID string) (ProjectField, error) {
	body := map[string]any{"field": map[string]any{"id": fieldID, "$type": "CustomField"}}
	switch kind {
	case KindState:
		body["$type"] = "StateProjectCustomField"
	case KindEnum, KindMultiEnum:
		body["$type"] = "EnumProjectCustomField"
		body["canBeEmpty"] = true
	case KindDate, KindString, KindInt:
		body["$type"] = "SimpleProjectCustomField"
		body["canBeEmpty"] = true
	case KindPeriod:
		body["$type"] = "PeriodProjectCustomField"
		body["canBeEmpty"] = true
	default:
		return ProjectField{}, fmt.Errorf("unknown field kind %d", kind)
	}
	if bundleID != "" {
		bundleType, _ := bundleTypes(kind)
		body["bundle"] = map[string]any{"id": bundleID, "$type": bundleType}
	}
	q := url.Values{}
	q.Set("fields", "id,$type,field(id,name),bundle(id,$type)")
	var out ProjectField
	if err := c.postOnce(ctx, "/api/admin/projects/"+url.PathEscape(projectID)+"/customFields", q, body, &out); err != nil {
		return ProjectField{}, err
	}
	return out, nil
}

// ListLinkTypes lists the issue link types.
func (c *Client) ListLinkTypes(ctx context.Context) ([]LinkType, error) {
	return getAll[LinkType](ctx, c, "/api/issueLinkTypes", "id,name,directed,sourceToTarget,targetToSource")
}
