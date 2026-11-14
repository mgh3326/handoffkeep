package youtrack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// serveLinks serves the fake's HTTP front end behind a WriteRemote. wrap, when
// set, sits between the client and the fake (a server that misbehaves).
func serveLinks(t *testing.T, f *ytwritetest.FakeYouTrack, wrap func(http.Handler) http.Handler) *WriteRemote {
	t.Helper()
	var h http.Handler = f.LinkHandler()
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+writeRemoteToken {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: writeRemoteToken, Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return NewWriteRemote(c)
}

// without drops query parameters from the requests whose path ends in suffix
// before the fake sees them: a server that does not know them.
func without(suffix string, params ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, suffix) {
				q := r.URL.Query()
				for _, p := range params {
					q.Del(p)
				}
				r.URL.RawQuery = q.Encode()
			}
			next.ServeHTTP(w, r)
		})
	}
}

// linkReqs is the "METHOD path" of every request the fake received.
func linkReqs(f *ytwritetest.FakeYouTrack) []string {
	var out []string
	for _, r := range f.LinkRequests() {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// skipsOf is the distinct consecutive values of a query parameter over the
// requests for path.
func skipsOf(f *ytwritetest.FakeYouTrack, path, param string) []string {
	var out []string
	for _, r := range f.LinkRequests() {
		if r.Path != path {
			continue
		}
		if v := r.Query.Get(param); len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func countPath(f *ytwritetest.FakeYouTrack, path string) int {
	n := 0
	for _, r := range f.LinkRequests() {
		if r.Path == path {
			n++
		}
	}
	return n
}

// swapRoles turns the named type's role names round: its writer side moves to
// the other direction.
func swapRoles(types []ytwrite.RemoteLinkType, name string) []ytwrite.RemoteLinkType {
	out := append([]ytwrite.RemoteLinkType(nil), types...)
	for i := range out {
		if out[i].Name == name {
			out[i].SourceToTarget, out[i].TargetToSource = out[i].TargetToSource, out[i].SourceToTarget
		}
	}
	return out
}

func fillers(directed, undirected int) []ytwrite.RemoteLinkType {
	var out []ytwrite.RemoteLinkType
	for i := 0; i < directed+undirected; i++ {
		out = append(out, ytwrite.RemoteLinkType{ID: fmt.Sprintf("9-%d", i), Name: fmt.Sprintf("Filler %d", i), Directed: i < directed, SourceToTarget: fmt.Sprintf("out %d", i), TargetToSource: fmt.Sprintf("in %d", i)})
	}
	return out
}

const linksPath = "/api/issues/2-5/links"

// I10: the writer's side comes from the direction field and the role names,
// not from id text, on the default types and on an instance whose Duplicate
// roles are the other way round.
func TestReadIssueLinksClassifiesByDirectionAndRoleNames(t *testing.T) {
	for _, tc := range []struct {
		name          string
		types         []ytwrite.RemoteLinkType
		wantSubtask   ytwrite.ManagedLinkType
		wantDuplicate ytwrite.ManagedLinkType
	}{
		{"default types", ytwritetest.DefaultLinkTypes(),
			ytwrite.ManagedLinkType{ID: "105-3", Direction: ytwrite.LinkInward}, ytwrite.ManagedLinkType{ID: "105-1", Direction: ytwrite.LinkInward}},
		{"duplicate roles swapped", swapRoles(ytwritetest.DefaultLinkTypes(), "Duplicate"),
			ytwrite.ManagedLinkType{ID: "105-3", Direction: ytwrite.LinkInward}, ytwrite.ManagedLinkType{ID: "105-1", Direction: ytwrite.LinkOutward}},
		{"subtask roles swapped", swapRoles(ytwritetest.DefaultLinkTypes(), "Subtask"),
			ytwrite.ManagedLinkType{ID: "105-3", Direction: ytwrite.LinkOutward}, ytwrite.ManagedLinkType{ID: "105-1", Direction: ytwrite.LinkInward}},
		{"both swapped", swapRoles(swapRoles(ytwritetest.DefaultLinkTypes(), "Subtask"), "Duplicate"),
			ytwrite.ManagedLinkType{ID: "105-3", Direction: ytwrite.LinkOutward}, ytwrite.ManagedLinkType{ID: "105-1", Direction: ytwrite.LinkOutward}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ytwritetest.NewFake()
			f.SetLinkTypes(tc.types)
			// The issue 2-5: two parents (a list, never one), an original, two
			// relates links (one stored from the other end), and every link
			// the other way round that must not appear.
			f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-1"})
			f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-2"})
			f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkDuplicate, Target: "2-4"})
			f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "2-6"})
			f.SeedIssueLink("2-3", ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "2-5"})
			f.SeedIssueLink("2-7", ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-5"})   // 2-7 is a child of 2-5
			f.SeedIssueLink("2-8", ytwrite.IssueLink{Kind: ytwrite.LinkDuplicate, Target: "2-5"}) // 2-8 duplicates 2-5
			f.SeedForeignLink("105-2", "2-5", "2-9")                                              // a Depend link
			got, err := serveLinks(t, f, nil).ReadIssueLinks(context.Background(), "2-5")
			if err != nil {
				t.Fatal(err)
			}
			want := ytwrite.IssueLinksRead{
				Types: ytwrite.LinkTypes{
					Subtask:   tc.wantSubtask,
					Relates:   ytwrite.ManagedLinkType{ID: "105-0", Direction: ytwrite.LinkBoth},
					Duplicate: tc.wantDuplicate,
				},
				Links: []ytwrite.IssueLink{
					{Kind: ytwrite.LinkSubtask, Target: "2-1"}, {Kind: ytwrite.LinkSubtask, Target: "2-2"},
					{Kind: ytwrite.LinkRelates, Target: "2-3"}, {Kind: ytwrite.LinkRelates, Target: "2-6"},
					{Kind: ytwrite.LinkDuplicate, Target: "2-4"},
				},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadIssueLinks =\n%+v\nwant\n%+v", got, want)
			}
			// What the other issue sees of a relates link, and of a child.
			other, err := serveLinks(t, f, nil).ReadIssueLinks(context.Background(), "2-6")
			if err != nil || !reflect.DeepEqual(other.Links, []ytwrite.IssueLink{{Kind: ytwrite.LinkRelates, Target: "2-5"}}) {
				t.Fatalf("read of 2-6 = %+v, %v; want the relates link to 2-5", other.Links, err)
			}
			parent, err := serveLinks(t, f, nil).ReadIssueLinks(context.Background(), "2-1")
			if err != nil || len(parent.Links) != 0 {
				t.Fatalf("read of the parent 2-1 = %+v, %v; want no writer-side link", parent.Links, err)
			}
		})
	}
}

// I11, S1 acceptance: 120 link rows with the managed rows on page 3, and 43+
// rows with managed rows on page 2. The server caps its pages below the $top
// the client asks for, so the pages are 50, 50, 20 and a "short page ends it"
// rule would stop after the first; the read goes on by the rows received and
// ends on an empty page.
func TestReadIssueLinksPagesRowsUntilAnEmptyPage(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		directed, undirected int // filler link types ahead of the four default ones
		cap                  int
		wantSkips            []string
	}{
		// 56 directed + 1 undirected filler = 113 rows, then the default types'
		// 7 rows: 120, the managed ones from row 113 on.
		{"120 rows, server caps pages at 50", 56, 1, 50, []string{"0", "50", "100", "120"}},
		{"120 rows, server honors $top", 56, 1, 0, []string{"0", "100", "120"}},
		// 40 filler rows + 7 = 47: the Duplicate and Subtask rows are on page 2.
		{"47 rows, server caps pages at 42", 20, 0, 42, []string{"0", "42", "47"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ytwritetest.NewFake()
			f.SetLinkTypes(append(fillers(tc.directed, tc.undirected), ytwritetest.DefaultLinkTypes()...))
			f.SetLinkPageCaps(tc.cap, 0)
			f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-1"})
			f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkDuplicate, Target: "2-4"})
			f.SeedForeignLink("9-3", "2-5", "2-9")
			got, err := serveLinks(t, f, nil).ReadIssueLinks(context.Background(), "2-5")
			if err != nil {
				t.Fatal(err)
			}
			want := []ytwrite.IssueLink{{Kind: ytwrite.LinkSubtask, Target: "2-1"}, {Kind: ytwrite.LinkDuplicate, Target: "2-4"}}
			if !reflect.DeepEqual(got.Links, want) {
				t.Fatalf("links %+v, want %+v", got.Links, want)
			}
			if skips := skipsOf(f, linksPath, "$skip"); !reflect.DeepEqual(skips, tc.wantSkips) {
				t.Fatalf("$skip over the reads of %s = %v, want %v", linksPath, skips, tc.wantSkips)
			}
		})
	}
}

// I11: the linked issues of one row are paged with $skipLinks until a request
// shows nothing new, whether the server caps them below $topLinks or not.
func TestReadIssueLinksPagesLinkedIssuesOfARow(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  int
	}{{"honors $topLinks", 0}, {"caps at 42", 42}} {
		t.Run(tc.name, func(t *testing.T) {
			f := ytwritetest.NewFake()
			f.SetLinkPageCaps(0, tc.cap)
			var want []ytwrite.IssueLink
			for i := 0; i < 250; i++ {
				l := ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: fmt.Sprintf("3-%03d", i)}
				f.SeedIssueLink("2-5", l)
				want = append(want, l)
			}
			got, err := serveLinks(t, f, nil).ReadIssueLinks(context.Background(), "2-5")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Links, want) {
				t.Fatalf("read %d links, want all %d (first %v)", len(got.Links), len(want), got.Links[:min(3, len(got.Links))])
			}
		})
	}
}

// The docs do not say $topLinks and $skipLinks apply to `issues`. A server that
// ignores them answers the same issues again; the read must end, with what the
// first answer held, and not page on to the cap.
func TestReadIssueLinksEndsWhenTheServerIgnoresSkipLinks(t *testing.T) {
	f := ytwritetest.NewFake()
	f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-1"})
	f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "2-6"})
	got, err := serveLinks(t, f, without("/links", "$topLinks", "$skipLinks")).ReadIssueLinks(context.Background(), "2-5")
	if err != nil {
		t.Fatal(err)
	}
	if want := []ytwrite.IssueLink{{Kind: ytwrite.LinkSubtask, Target: "2-1"}, {Kind: ytwrite.LinkRelates, Target: "2-6"}}; !reflect.DeepEqual(got.Links, want) {
		t.Fatalf("links %+v, want %+v", got.Links, want)
	}
	// One page of rows asked for twice (the second shows nothing new), then the
	// empty page: three requests, not a walk to the cap.
	if n := countPath(f, linksPath); n != 3 {
		t.Fatalf("%d requests for %s, want 3", n, linksPath)
	}
}

// A server that ignores $skip repeats the rows; that is an inconsistent read,
// not a loop to the cap.
func TestReadIssueLinksRefusesRowsRepeatedAcrossPages(t *testing.T) {
	f := ytwritetest.NewFake()
	_, err := serveLinks(t, f, without("/links", "$skip")).ReadIssueLinks(context.Background(), "2-5")
	if !errors.Is(err, ytwrite.ErrInconsistentEnumeration) {
		t.Fatalf("ReadIssueLinks error %v, want ErrInconsistentEnumeration", err)
	}
}

// I11: past the cap a read is an error and returns nothing, never a prefix.
func TestReadIssueLinksOverTheCapIsAnErrorNotPartial(t *testing.T) {
	t.Run("rows", func(t *testing.T) {
		f := ytwritetest.NewFake()
		f.SetLinkTypes(append(fillers(5001, 0), ytwritetest.DefaultLinkTypes()...)) // 10010 rows
		f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-1"})
		got, err := serveLinks(t, f, nil).ReadIssueLinks(context.Background(), "2-5")
		if !errors.Is(err, ytwrite.ErrEnumerationTooLarge) || len(got.Links) != 0 {
			t.Fatalf("ReadIssueLinks = %+v, %v; want ErrEnumerationTooLarge and no links", got, err)
		}
	})
	t.Run("linked issues", func(t *testing.T) {
		f := ytwritetest.NewFake()
		for i := 0; i <= ytwrite.MaxEnumeratedLinks; i++ {
			f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "3-" + strconv.Itoa(i)})
		}
		got, err := serveLinks(t, f, nil).ReadIssueLinks(context.Background(), "2-5")
		if !errors.Is(err, ytwrite.ErrEnumerationTooLarge) || len(got.Links) != 0 {
			t.Fatalf("ReadIssueLinks = %d links, %v; want ErrEnumerationTooLarge and no links", len(got.Links), err)
		}
	})
}

// §6: a type the instance does not configure as hk needs is refused before any
// link row is read, and never guessed.
func TestReadIssueLinksRefusesUnsupportedLinkTypes(t *testing.T) {
	defaults := ytwritetest.DefaultLinkTypes()
	edit := func(name string, change func(*ytwrite.RemoteLinkType)) []ytwrite.RemoteLinkType {
		out := append([]ytwrite.RemoteLinkType(nil), defaults...)
		for i := range out {
			if out[i].Name == name {
				change(&out[i])
			}
		}
		return out
	}
	drop := func(name string) []ytwrite.RemoteLinkType {
		var out []ytwrite.RemoteLinkType
		for _, typ := range defaults {
			if typ.Name != name {
				out = append(out, typ)
			}
		}
		return out
	}
	for name, types := range map[string][]ytwrite.RemoteLinkType{
		"no duplicates role":             edit("Duplicate", func(x *ytwrite.RemoteLinkType) { x.TargetToSource = "is a duplicate of" }),
		"both sides duplicates":          edit("Duplicate", func(x *ytwrite.RemoteLinkType) { x.SourceToTarget = "duplicates" }),
		"no subtask of role":             edit("Subtask", func(x *ytwrite.RemoteLinkType) { x.TargetToSource = "child of" }),
		"subtask undirected":             edit("Subtask", func(x *ytwrite.RemoteLinkType) { x.Directed = false }),
		"duplicate undirected":           edit("Duplicate", func(x *ytwrite.RemoteLinkType) { x.Directed = false }),
		"relates directed":               edit("Relates", func(x *ytwrite.RemoteLinkType) { x.Directed = true }),
		"no Subtask type":                drop("Subtask"),
		"no Duplicate type":              drop("Duplicate"),
		"no Relates type":                drop("Relates"),
		"two Subtask types":              append(append([]ytwrite.RemoteLinkType(nil), defaults...), ytwrite.RemoteLinkType{ID: "105-9", Name: "subtask", Directed: true, SourceToTarget: "parent for", TargetToSource: "subtask of"}),
		"type without an id":             edit("Relates", func(x *ytwrite.RemoteLinkType) { x.ID = "" }),
		"role spelled without the space": edit("Subtask", func(x *ytwrite.RemoteLinkType) { x.TargetToSource = "subtaskof" }),
	} {
		t.Run(name, func(t *testing.T) {
			f := ytwritetest.NewFake()
			f.SetLinkTypes(types)
			_, err := serveLinks(t, f, nil).ReadIssueLinks(context.Background(), "2-5")
			var refused *ytwrite.RefusedError
			if !errors.As(err, &refused) || refused.Reason != ytwrite.ReasonFieldRefused {
				t.Fatalf("ReadIssueLinks error %v, want a field_refused refusal", err)
			}
			for _, req := range linkReqs(f) {
				if strings.Contains(req, "/links") {
					t.Fatalf("read link rows after refusing the types: %v", linkReqs(f))
				}
			}
		})
	}
}

// The mutation URLs: a type id with `t` for the inward side of a directed type,
// `s` for the outward side, bare for an undirected type. The fake answers 404 to
// any other spelling, so a success is also proof of the shape.
func TestIssueLinkMutationURLShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		types []ytwrite.RemoteLinkType
		paths map[ytwrite.LinkKind]string
	}{
		{"default types", ytwritetest.DefaultLinkTypes(), map[ytwrite.LinkKind]string{
			ytwrite.LinkSubtask: "105-3t", ytwrite.LinkDuplicate: "105-1t", ytwrite.LinkRelates: "105-0"}},
		{"duplicate roles swapped", swapRoles(ytwritetest.DefaultLinkTypes(), "Duplicate"), map[ytwrite.LinkKind]string{
			ytwrite.LinkSubtask: "105-3t", ytwrite.LinkDuplicate: "105-1s", ytwrite.LinkRelates: "105-0"}},
		{"subtask roles swapped", swapRoles(ytwritetest.DefaultLinkTypes(), "Subtask"), map[ytwrite.LinkKind]string{
			ytwrite.LinkSubtask: "105-3s", ytwrite.LinkDuplicate: "105-1t", ytwrite.LinkRelates: "105-0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := ytwritetest.NewFake()
			f.SetLinkTypes(tc.types)
			r := serveLinks(t, f, nil)
			read, err := r.ReadIssueLinks(ctx, "2-5")
			if err != nil {
				t.Fatal(err)
			}
			links := []ytwrite.IssueLink{{Kind: ytwrite.LinkSubtask, Target: "2-1"}, {Kind: ytwrite.LinkRelates, Target: "2-3"}, {Kind: ytwrite.LinkDuplicate, Target: "2-4"}}
			before := len(f.LinkRequests())
			for _, l := range links {
				if err := r.AddIssueLink(ctx, "2-5", read.Types, l); err != nil {
					t.Fatalf("add %+v: %v", l, err)
				}
			}
			added := f.LinkRequests()[before:]
			for i, l := range links {
				req := added[i]
				if want := "/api/issues/2-5/links/" + tc.paths[l.Kind] + "/issues"; req.Method != http.MethodPost || req.Path != want {
					t.Errorf("add %s: %s %s, want POST %s", l.Kind, req.Method, req.Path, want)
				}
				if want := `{"id":"` + l.Target + `"}`; strings.TrimSpace(req.Body) != want || req.Query.Get("fields") != "id" {
					t.Errorf("add %s: body %q fields %q, want %s and fields=id", l.Kind, req.Body, req.Query.Get("fields"), want)
				}
			}
			if got := f.IssueLinks("2-5"); !reflect.DeepEqual(got, links) {
				t.Fatalf("after the adds the fake holds %+v, want %+v", got, links)
			}
			after, err := r.ReadIssueLinks(ctx, "2-5")
			if err != nil || !reflect.DeepEqual(after.Links, links) {
				t.Fatalf("read after the adds = %+v, %v", after.Links, err)
			}
			before = len(f.LinkRequests())
			for _, l := range links {
				if err := r.RemoveIssueLink(ctx, "2-5", read.Types, l); err != nil {
					t.Fatalf("remove %+v: %v", l, err)
				}
			}
			removed := f.LinkRequests()[before:]
			if len(removed) != len(links) {
				t.Fatalf("removing %d links took %d requests: %v", len(links), len(removed), removed)
			}
			for i, l := range links {
				req := removed[i]
				if want := "/api/issues/2-5/links/" + tc.paths[l.Kind] + "/issues/" + l.Target; req.Method != http.MethodDelete || req.Path != want {
					t.Errorf("remove %s: %s %s, want DELETE %s", l.Kind, req.Method, req.Path, want)
				}
			}
			if got := f.IssueLinks("2-5"); len(got) != 0 {
				t.Fatalf("after the removes the fake holds %+v", got)
			}
		})
	}
}

// A link the caller names with no managed kind or no target is refused before
// a request is made.
func TestIssueLinkMutationRefusesAnUnnamedLink(t *testing.T) {
	f := ytwritetest.NewFake()
	r := serveLinks(t, f, nil)
	read, err := r.ReadIssueLinks(context.Background(), "2-5")
	if err != nil {
		t.Fatal(err)
	}
	n := len(f.LinkRequests())
	for _, l := range []ytwrite.IssueLink{{Kind: "depend", Target: "2-1"}, {Kind: ytwrite.LinkRelates}} {
		if err := r.AddIssueLink(context.Background(), "2-5", read.Types, l); !errors.Is(err, ytwrite.ErrInvalid) {
			t.Errorf("add %+v: %v, want ErrInvalid", l, err)
		}
		if err := r.RemoveIssueLink(context.Background(), "2-5", read.Types, l); !errors.Is(err, ytwrite.ErrInvalid) {
			t.Errorf("remove %+v: %v, want ErrInvalid", l, err)
		}
	}
	if len(f.LinkRequests()) != n {
		t.Fatalf("requests made for refused links: %v", linkReqs(f)[n:])
	}
}

// S1 acceptance: a DELETE answered 404 is re-read. Absent is done; present is
// the 404; a re-read that fails is that failure, never success.
func TestRemoveIssueLinkAnswered404IsReread(t *testing.T) {
	ctx := context.Background()
	link := ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "2-3"}
	setup := func(t *testing.T, seed bool) (*ytwritetest.FakeYouTrack, *WriteRemote, ytwrite.LinkTypes) {
		f := ytwritetest.NewFake()
		if seed {
			f.SeedIssueLink("2-5", link)
		}
		r := serveLinks(t, f, nil)
		read, err := r.ReadIssueLinks(ctx, "2-5")
		if err != nil {
			t.Fatal(err)
		}
		return f, r, read.Types
	}
	t.Run("the link is absent", func(t *testing.T) {
		f, r, types := setup(t, false)
		f.SeedIssueLink("2-5", ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "2-8"}) // another link stays
		n := len(f.LinkRequests())
		if err := r.RemoveIssueLink(ctx, "2-5", types, link); err != nil {
			t.Fatalf("remove of an absent link: %v", err)
		}
		if got := linkReqs(f)[n]; got != "DELETE /api/issues/2-5/links/105-0/issues/2-3" {
			t.Fatalf("first request %q", got)
		}
		if skips := skipsOf(f, linksPath, "$skip"); len(skips) == 0 {
			t.Fatalf("no re-read of the links after the 404: %v", linkReqs(f))
		}
	})
	t.Run("the link is present", func(t *testing.T) {
		f, r, types := setup(t, true)
		f.QueueLinkFaults(ytwritetest.LinkFault{Method: http.MethodDelete, Status: http.StatusNotFound, Count: 1})
		err := r.RemoveIssueLink(ctx, "2-5", types, link)
		var remote *ytwrite.RemoteError
		if !errors.As(err, &remote) || remote.Status != http.StatusNotFound {
			t.Fatalf("remove with the link present: %v, want the 404", err)
		}
		if got := f.IssueLinks("2-5"); len(got) != 1 {
			t.Fatalf("the link did not stay: %+v", got)
		}
	})
	t.Run("the re-read fails", func(t *testing.T) {
		f, r, types := setup(t, false)
		f.QueueLinkFaults(ytwritetest.LinkFault{Method: http.MethodGet, Status: http.StatusServiceUnavailable, Count: 1})
		err := r.RemoveIssueLink(ctx, "2-5", types, link)
		var remote *ytwrite.RemoteError
		if !errors.As(err, &remote) || remote.Status != http.StatusServiceUnavailable {
			t.Fatalf("remove with a failing re-read: %v, want the 503", err)
		}
	})
	t.Run("other answers are not re-read", func(t *testing.T) {
		f, r, types := setup(t, true)
		f.QueueLinkFaults(ytwritetest.LinkFault{Method: http.MethodDelete, Status: http.StatusBadRequest, Count: 1})
		n := len(f.LinkRequests())
		var remote *ytwrite.RemoteError
		if err := r.RemoveIssueLink(ctx, "2-5", types, link); !errors.As(err, &remote) || remote.Status != http.StatusBadRequest {
			t.Fatalf("remove answered 400: %v", err)
		}
		if got := f.LinkRequests()[n:]; len(got) != 1 {
			t.Fatalf("requests after a 400: %v", got)
		}
	})
}

// One request per call and no retry: a 401 or 429 comes back as the server's
// answer with its wait, for a read and for a mutation.
func TestIssueLinkCallsDoNotRetry(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		method string
		status int
		call   func(r *WriteRemote, types ytwrite.LinkTypes) error
	}{
		{"read 429", http.MethodGet, http.StatusTooManyRequests, func(r *WriteRemote, _ ytwrite.LinkTypes) error { _, err := r.ReadIssueLinks(ctx, "2-5"); return err }},
		{"read 401", http.MethodGet, http.StatusUnauthorized, func(r *WriteRemote, _ ytwrite.LinkTypes) error { _, err := r.ReadIssueLinks(ctx, "2-5"); return err }},
		{"add 429", http.MethodPost, http.StatusTooManyRequests, func(r *WriteRemote, ty ytwrite.LinkTypes) error {
			return r.AddIssueLink(ctx, "2-5", ty, ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-1"})
		}},
		{"add 400", http.MethodPost, http.StatusBadRequest, func(r *WriteRemote, ty ytwrite.LinkTypes) error {
			return r.AddIssueLink(ctx, "2-5", ty, ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-1"})
		}},
		{"add 503", http.MethodPost, http.StatusServiceUnavailable, func(r *WriteRemote, ty ytwrite.LinkTypes) error {
			return r.AddIssueLink(ctx, "2-5", ty, ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-1"})
		}},
		{"remove 429", http.MethodDelete, http.StatusTooManyRequests, func(r *WriteRemote, ty ytwrite.LinkTypes) error {
			return r.RemoveIssueLink(ctx, "2-5", ty, ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-1"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ytwritetest.NewFake()
			r := serveLinks(t, f, nil)
			read, err := r.ReadIssueLinks(ctx, "2-5")
			if err != nil {
				t.Fatal(err)
			}
			n := len(f.LinkRequests())
			f.QueueLinkFaults(ytwritetest.LinkFault{Method: tc.method, Status: tc.status, RetryAfter: 7 * time.Second, Count: 5})
			err = tc.call(r, read.Types)
			var remote *ytwrite.RemoteError
			if !errors.As(err, &remote) || remote.Status != tc.status {
				t.Fatalf("error %v, want the server's %d", err, tc.status)
			}
			if tc.status == http.StatusTooManyRequests && remote.RetryAfter != 7*time.Second {
				t.Fatalf("RetryAfter %v, want 7s", remote.RetryAfter)
			}
			if got := f.LinkRequests()[n:]; len(got) != 1 {
				t.Fatalf("%d requests for one call: %v", len(got), got)
			}
		})
	}
}

// The in-memory front end and the HTTP front end hold one store: what either
// writes the other reads, and a relates link is one link seen from both ends.
func TestLinkFakeFrontEndsAgree(t *testing.T) {
	ctx := context.Background()
	f := ytwritetest.NewFake()
	overHTTP := serveLinks(t, f, nil)
	var mem ytwrite.IssueLinksRemote = f

	types := func(remote ytwrite.IssueLinksRemote) ytwrite.LinkTypes {
		read, err := remote.ReadIssueLinks(ctx, "2-5")
		if err != nil {
			t.Fatal(err)
		}
		return read.Types
	}
	if a, b := types(overHTTP), types(mem); a != b {
		t.Fatalf("types differ: http %+v, memory %+v", a, b)
	}
	ty := types(mem)
	steps := []struct {
		remote ytwrite.IssueLinksRemote
		issue  string
		link   ytwrite.IssueLink
		remove bool
	}{
		{overHTTP, "2-5", ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-1"}, false},
		{mem, "2-5", ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "2-6"}, false},
		{overHTTP, "2-7", ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "2-5"}, false}, // 2-7 > 2-5: stored once
		{mem, "2-5", ytwrite.IssueLink{Kind: ytwrite.LinkDuplicate, Target: "2-4"}, false},
		{overHTTP, "2-5", ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "2-6"}, true},
		{mem, "2-5", ytwrite.IssueLink{Kind: ytwrite.LinkDuplicate, Target: "2-9"}, true}, // absent: not an error
	}
	for i, s := range steps {
		var err error
		if s.remove {
			err = s.remote.RemoveIssueLink(ctx, s.issue, ty, s.link)
		} else {
			err = s.remote.AddIssueLink(ctx, s.issue, ty, s.link)
		}
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	for _, issue := range []string{"2-1", "2-4", "2-5", "2-6", "2-7"} {
		a, err := overHTTP.ReadIssueLinks(ctx, issue)
		if err != nil {
			t.Fatal(err)
		}
		b, err := mem.ReadIssueLinks(ctx, issue)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("issue %s: http %+v, memory %+v", issue, a, b)
		}
	}
	want := []ytwrite.IssueLink{{Kind: ytwrite.LinkSubtask, Target: "2-1"}, {Kind: ytwrite.LinkRelates, Target: "2-7"}, {Kind: ytwrite.LinkDuplicate, Target: "2-4"}}
	if got := f.IssueLinks("2-5"); !reflect.DeepEqual(got, want) {
		t.Fatalf("2-5 holds %+v, want %+v", got, want)
	}
	if got := f.IssueLinks("2-7"); !reflect.DeepEqual(got, []ytwrite.IssueLink{{Kind: ytwrite.LinkRelates, Target: "2-5"}}) {
		t.Fatalf("2-7 holds %+v, want the one relates link to 2-5", got)
	}
}
