package linear

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLinearAllowedOperationSurface(t *testing.T) {
	want := []Operation{
		"issue_search",
		"issue_create",
		"issue_update",
		"comment_create",
		"comment_list",
		"issue_archive",
		"issue_get",
		"team_lookup",
	}
	if !reflect.DeepEqual(AllowedOperations, want) {
		t.Fatalf("operations=%v want=%v", AllowedOperations, want)
	}
	source, err := os.ReadFile("client.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"issue" + "Delete", "comment" + "Delete"} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("forbidden mutation surface %q is present", forbidden)
		}
	}
}

func TestLinearFixturesAreFileBackedGraphQLShapes(t *testing.T) {
	names := []string{
		"team_lookup.json",
		"issue_search_empty.json",
		"issue_search_found.json",
		"issue_create.json",
		"issue_update.json",
		"issue_get_active.json",
		"issue_get_archived.json",
		"comment_list_empty.json",
		"comment_list_found.json",
		"comment_create.json",
		"issue_archive.json",
		"authentication_error.json",
		"forbidden_error.json",
		"ratelimited_error.json",
	}
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join("testdata", "linear", name))
		if err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		var envelope graphqlEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("fixture %s is not JSON: %v", name, err)
		}
		if len(envelope.Data) == 0 && len(envelope.Errors) == 0 {
			t.Fatalf("fixture %s has neither GraphQL data nor errors", name)
		}
	}
}
