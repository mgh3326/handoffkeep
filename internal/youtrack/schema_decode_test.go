package youtrack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One decode test per administration endpoint the schema run calls: the
// client reads YouTrack's JSON for that endpoint from testdata/schema (no
// network) and the test checks what it decoded, and that the request asked for
// the fields the decoder needs.

func TestSchemaEndpointsDecodeRecordedJSON(t *testing.T) {
	type call struct {
		name, method, path, fixture string
		wantQuery                   map[string]string // substring of the query value
		do                          func(t *testing.T, c *Client)
	}
	ctx := context.Background()
	calls := []call{
		{"list projects", "GET", "/api/admin/projects", "admin_projects.json", map[string]string{"fields": "leader("}, func(t *testing.T, c *Client) {
			got, err := c.ListAdminProjects(ctx)
			if err != nil || len(got) != 2 || got[0].ID != "0-3" || got[0].ShortName != "HANDOFFKEE" || got[0].Name != "handoffkeep" || got[0].Leader == nil || got[0].Leader.ID != "1-5" {
				t.Fatalf("projects=%+v err=%v", got, err)
			}
		}},
		{"create project", "POST", "/api/admin/projects", "admin_project_create.json", nil, func(t *testing.T, c *Client) {
			got, err := c.CreateProject(ctx, "factortrail", "FACTORTRAI", "1-5")
			if err != nil || got.ID != "0-31" || got.ShortName != "FACTORTRAI" || got.Leader.ID != "1-5" {
				t.Fatalf("project=%+v err=%v", got, err)
			}
		}},
		{"list project fields", "GET", "/api/admin/projects/0-3/customFields", "project_custom_fields.json", map[string]string{"fields": "fieldType(id)"}, func(t *testing.T, c *Client) {
			got, err := c.ListProjectFields(ctx, "0-3")
			if err != nil || len(got) != 3 {
				t.Fatalf("fields=%+v err=%v", got, err)
			}
			if got[0].Field.Name != "hk 상태" || got[0].Bundle == nil || got[0].Bundle.ID != "67-40" || got[0].Bundle.Type != "StateBundle" || got[0].Field.FieldType.ID != "state[1]" || got[0].Type != "StateProjectCustomField" {
				t.Fatalf("state field=%+v", got[0])
			}
			if got[2].Field.Name != "마감" || got[2].Field.FieldType.ID != "date" || got[2].Bundle != nil {
				t.Fatalf("date field=%+v", got[2])
			}
		}},
		{"get state bundle", "GET", "/api/admin/customFieldSettings/bundles/state/67-40", "bundle_state.json", map[string]string{"fields": "isResolved"}, func(t *testing.T, c *Client) {
			got, err := c.GetBundle(ctx, KindState, "67-40")
			if err != nil || got.ID != "67-40" || len(got.Values) != 2 || got.Values[1].Name != "머지됨" || got.Values[1].Ordinal != 1 || got.Values[1].IsResolved {
				t.Fatalf("bundle=%+v err=%v", got, err)
			}
		}},
		{"get enum bundle", "GET", "/api/admin/customFieldSettings/bundles/enum/67-41", "bundle_enum.json", map[string]string{"fields": "ordinal"}, func(t *testing.T, c *Client) {
			got, err := c.GetBundle(ctx, KindEnum, "67-41")
			if err != nil || len(got.Values) != 2 || got.Values[0].Name != "lane-a" || got.Values[1].ID != "68-202" {
				t.Fatalf("bundle=%+v err=%v", got, err)
			}
		}},
		{"list field definitions", "GET", "/api/admin/customFieldSettings/customFields", "global_fields.json", map[string]string{"fields": "fieldType(id)"}, func(t *testing.T, c *Client) {
			got, err := c.ListGlobalFields(ctx)
			if err != nil || len(got) != 2 || got[0].Name != "레인" || got[0].FieldType.ID != "enum[1]" || got[1].FieldType.ID != "state[1]" {
				t.Fatalf("fields=%+v err=%v", got, err)
			}
		}},
		{"create field definition", "POST", "/api/admin/customFieldSettings/customFields", "global_field_create.json", nil, func(t *testing.T, c *Client) {
			got, err := c.CreateGlobalField(ctx, "마감", "date")
			if err != nil || got.ID != "58-30" || got.FieldType.ID != "date" {
				t.Fatalf("field=%+v err=%v", got, err)
			}
		}},
		{"create bundle", "POST", "/api/admin/customFieldSettings/bundles/enum", "bundle_create.json", nil, func(t *testing.T, c *Client) {
			got, err := c.CreateBundle(ctx, KindEnum, "FACTORTRAI 영역", []BundleValue{{Name: "A"}})
			if err != nil || got.ID != "67-50" || len(got.Values) != 1 || got.Values[0].ID != "68-301" {
				t.Fatalf("bundle=%+v err=%v", got, err)
			}
		}},
		{"add bundle value", "POST", "/api/admin/customFieldSettings/bundles/state/67-40/values", "bundle_value_add.json", nil, func(t *testing.T, c *Client) {
			got, err := c.AddBundleValue(ctx, KindState, "67-40", BundleValue{Name: "착수 전", Ordinal: 2})
			if err != nil || got.ID != "68-109" || got.Ordinal != 2 {
				t.Fatalf("element=%+v err=%v", got, err)
			}
		}},
		{"update state value", "POST", "/api/admin/customFieldSettings/bundles/state/67-40/values/68-102", "bundle_value_update.json", nil, func(t *testing.T, c *Client) {
			if err := c.SetStateElement(ctx, "67-40", "68-102", map[string]any{"isResolved": true}); err != nil {
				t.Fatal(err)
			}
		}},
		{"attach project field", "POST", "/api/admin/projects/0-3/customFields", "project_field_attach.json", nil, func(t *testing.T, c *Client) {
			got, err := c.AttachProjectField(ctx, "0-3", KindEnum, "58-31", "67-50")
			if err != nil || got.ID != "90-210" || got.Field.Name != "영역" || got.Bundle == nil || got.Bundle.ID != "67-50" {
				t.Fatalf("attached=%+v err=%v", got, err)
			}
		}},
		{"list link types", "GET", "/api/issueLinkTypes", "link_types.json", map[string]string{"fields": "directed"}, func(t *testing.T, c *Client) {
			got, err := c.ListLinkTypes(ctx)
			if err != nil || len(got) != 3 || got[2].Name != "Subtask" || !got[2].Directed || got[2].SourceToTarget != "parent for" || got[0].Directed {
				t.Fatalf("link types=%+v err=%v", got, err)
			}
			if r := linkTypeReports(got); len(r) != 0 {
				t.Fatalf("the built-in link types report %+v", r)
			}
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "schema", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Errorf("request %s %s, want %s %s", r.Method, r.URL.Path, tc.method, tc.path)
				}
				for k, v := range tc.wantQuery {
					if got := r.URL.Query().Get(k); !strings.Contains(got, v) {
						t.Errorf("query %s=%q lacks %q", k, got, v)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			}))
			defer srv.Close()
			c, err := New(Config{BaseURL: srv.URL, Token: "token"})
			if err != nil {
				t.Fatal(err)
			}
			tc.do(t, c)
		})
	}
}
