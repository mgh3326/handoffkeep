package ytwrite_test

import (
	"errors"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

func TestResolveLinkTypesFindsTheWriterSideByRoleName(t *testing.T) {
	got, err := ytwrite.ResolveLinkTypes(ytwritetest.DefaultLinkTypes())
	if err != nil {
		t.Fatal(err)
	}
	want := ytwrite.LinkTypes{
		Subtask:   ytwrite.ManagedLinkType{ID: "105-3", Direction: ytwrite.LinkInward},
		Relates:   ytwrite.ManagedLinkType{ID: "105-0", Direction: ytwrite.LinkBoth},
		Duplicate: ytwrite.ManagedLinkType{ID: "105-1", Direction: ytwrite.LinkInward},
	}
	if got != want {
		t.Fatalf("types %+v, want %+v", got, want)
	}
	// Role names compare without case and surrounding space; a swapped type
	// moves the writer to the outward side.
	types := ytwritetest.DefaultLinkTypes()
	for i := range types {
		if types[i].Name == "Duplicate" {
			types[i].SourceToTarget, types[i].TargetToSource = " Duplicates ", "is duplicated by"
		}
	}
	got, err = ytwrite.ResolveLinkTypes(types)
	if err != nil || got.Duplicate != (ytwrite.ManagedLinkType{ID: "105-1", Direction: ytwrite.LinkOutward}) {
		t.Fatalf("swapped Duplicate: %+v, %v", got.Duplicate, err)
	}
}

func TestManagedLinkTypeURLSegment(t *testing.T) {
	for _, tc := range []struct {
		typ  ytwrite.ManagedLinkType
		want string
	}{
		{ytwrite.ManagedLinkType{ID: "105-3", Direction: ytwrite.LinkInward}, "105-3t"},
		{ytwrite.ManagedLinkType{ID: "105-1", Direction: ytwrite.LinkOutward}, "105-1s"},
		{ytwrite.ManagedLinkType{ID: "105-0", Direction: ytwrite.LinkBoth}, "105-0"},
	} {
		if got := tc.typ.LinkID(); got != tc.want {
			t.Errorf("%+v LinkID = %q, want %q", tc.typ, got, tc.want)
		}
	}
}

func TestClassifyLinkRows(t *testing.T) {
	types, err := ytwrite.ResolveLinkTypes(ytwritetest.DefaultLinkTypes())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		typeID   string
		dir      ytwrite.LinkDirection
		wantKind ytwrite.LinkKind
		wantOurs bool
		wantErr  bool
	}{
		{"parent side of Subtask is ours", "105-3", ytwrite.LinkInward, ytwrite.LinkSubtask, true, false},
		{"children side of Subtask is not", "105-3", ytwrite.LinkOutward, ytwrite.LinkSubtask, false, false},
		{"duplicates side of Duplicate is ours", "105-1", ytwrite.LinkInward, ytwrite.LinkDuplicate, true, false},
		{"duplicated-by side is not", "105-1", ytwrite.LinkOutward, ytwrite.LinkDuplicate, false, false},
		{"Relates is ours", "105-0", ytwrite.LinkBoth, ytwrite.LinkRelates, true, false},
		{"Relates is ours whatever its direction", "105-0", ytwrite.LinkOutward, ytwrite.LinkRelates, true, false},
		{"a type hk does not manage", "105-2", ytwrite.LinkInward, "", false, false},
		{"BOTH on a directed type is refused", "105-3", ytwrite.LinkBoth, ytwrite.LinkSubtask, false, true},
		{"an empty direction on a directed type is refused", "105-1", "", ytwrite.LinkDuplicate, false, true},
	} {
		kind, ours, err := types.Classify(tc.typeID, tc.dir)
		if (err != nil) != tc.wantErr || (err != nil && !errors.Is(err, ytwrite.ErrInvalid)) || kind != tc.wantKind || ours != tc.wantOurs {
			t.Errorf("%s: Classify = %q, %v, %v; want %q, %v, err=%v", tc.name, kind, ours, err, tc.wantKind, tc.wantOurs, tc.wantErr)
		}
	}
}
