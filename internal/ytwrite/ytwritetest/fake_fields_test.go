package ytwritetest_test

import (
	"context"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// Reads report the documented fieldType mapping production uses: a state
// bundle field reads back as FieldTypeState, an enum bundle field as
// FieldTypeEnum, and a field with no bundle as "".
func TestFakeIssueFieldTypes(t *testing.T) {
	f := ytwritetest.NewFake()
	issue := f.InjectIssue("0-7", 4003, "s", "d")
	f.SetBundle(ytwrite.FieldLane, "handoffkeep-design")
	f.SetStateBundle(ytwrite.FieldState, "개발 중")
	f.SetIssueField(issue, ytwrite.FieldLane, "handoffkeep-design")
	f.SetIssueField(issue, ytwrite.FieldState, "개발 중")
	f.SetIssueField(issue, ytwrite.FieldArea, "youtrack")
	got, err := f.ReadIssueFields(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]ytwrite.FieldType{}
	for _, fv := range got.Fields {
		types[fv.Name] = fv.Type
	}
	if types[ytwrite.FieldLane] != ytwrite.FieldTypeEnum || types[ytwrite.FieldState] != ytwrite.FieldTypeState || types[ytwrite.FieldArea] != "" {
		t.Fatalf("field types=%v", types)
	}
}
