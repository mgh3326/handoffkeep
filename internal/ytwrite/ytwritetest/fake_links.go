package ytwritetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The link side of FakeYouTrack (hk 4636 S1): one store of links, two front
// ends over it.
//
//   - FakeYouTrack implements ytwrite.IssueLinksRemote in memory, for the
//     protocol suites of the slices that follow.
//   - LinkHandler is an http.Handler that answers the YouTrack REST calls the
//     adapter makes, in the documented response shape: link types and /links
//     rows paged with $top/$skip, one row per (link type, direction) of the
//     issue with plain row ids, the `direction` field, the linked issues of a
//     row paged with $topLinks/$skipLinks, and only the attributes `fields`
//     names.
//
// A link is stored once, as hk's view of it (child -> parent, duplicate ->
// original, relates as an ordered pair), and both issues see it. The rows are
// derived from the link types in use, so a type whose roles are swapped puts
// the writer on the other direction.
//
// What the fake assumes without a documented source (design §6, canary S5):
// that a second `subtask of` is allowed, that `issues` of a row is complete
// when $topLinks is absent and is paged by $topLinks/$skipLinks when present,
// that adding a link twice is a 200, and that DELETE of an absent link is a
// 404.

// DefaultLinkTypes are YouTrack's default link types, in the order the
// instance lists them: Relates (undirected), Duplicate, Depend (not managed)
// and Subtask.
func DefaultLinkTypes() []ytwrite.RemoteLinkType {
	return []ytwrite.RemoteLinkType{
		{ID: "105-0", Name: "Relates", Directed: false, SourceToTarget: "relates to", TargetToSource: "relates to"},
		{ID: "105-1", Name: "Duplicate", Directed: true, SourceToTarget: "is duplicated by", TargetToSource: "duplicates"},
		{ID: "105-2", Name: "Depend", Directed: true, SourceToTarget: "is required for", TargetToSource: "depends on"},
		{ID: "105-3", Name: "Subtask", Directed: true, SourceToTarget: "parent for", TargetToSource: "subtask of"},
	}
}

// linkPair is one link as hk sees it. For subtask a is the child and b the
// parent; for duplicate a is the duplicate and b the original; for relates
// a < b.
type linkPair struct {
	kind ytwrite.LinkKind
	a, b string
}

// foreignLink is a link of a type hk does not manage, from -> to along the
// type's outward side.
type foreignLink struct{ typeID, from, to string }

// LinkRequest is one request the link handler received.
type LinkRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   string
}

// LinkFault answers the next Count requests whose method is Method ("" for any)
// with Status and no effect.
type LinkFault struct {
	Method     string
	Status     int
	RetryAfter time.Duration
	Count      int
}

type fakeLinks struct {
	types    []ytwrite.RemoteLinkType
	typesSet bool
	pairs    map[linkPair]bool
	foreign  map[foreignLink]bool
	// rowCap and linksCap are the most rows (or link types) per page and
	// linked issues per row the server returns whatever the client asks for
	// (0: no cap).
	rowCap, linksCap int
	faults           []LinkFault
	requests         []LinkRequest
	// calls and callFaults are the in-memory front end's log and failures.
	calls      []LinkCall
	callFaults []LinkCallFault
}

// LinkOp names one call of the in-memory front end.
type LinkOp string

const (
	LinkOpRead   LinkOp = "read"
	LinkOpAdd    LinkOp = "add"
	LinkOpRemove LinkOp = "remove"
)

// LinkCall is one call the in-memory front end received (Link is zero for a
// read).
type LinkCall struct {
	Op    LinkOp
	Issue string
	Link  ytwrite.IssueLink
}

// LinkCallFault fails the next Count in-memory calls of Op ("" for any): a
// *ytwrite.RemoteError with Status and RetryAfter, or a transport error when
// Status is 0. With Landed the call's effect happens first (an add or remove
// whose reply was lost).
type LinkCallFault struct {
	Op         LinkOp
	Status     int
	RetryAfter time.Duration
	Landed     bool
	Count      int
}

// QueueLinkCallFaults adds failures for the next matching in-memory calls.
func (f *FakeYouTrack) QueueLinkCallFaults(faults ...LinkCallFault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links.callFaults = append(f.links.callFaults, faults...)
}

// LinkCalls returns every in-memory call received, in order.
func (f *FakeYouTrack) LinkCalls() []LinkCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LinkCall(nil), f.links.calls...)
}

// callFault logs a call and takes the first fault matching it.
func (l *fakeLinks) callFault(c LinkCall) (fault LinkCallFault, ok bool) {
	l.calls = append(l.calls, c)
	for i := range l.callFaults {
		f := &l.callFaults[i]
		if f.Count <= 0 || (f.Op != "" && f.Op != c.Op) {
			continue
		}
		f.Count--
		return *f, true
	}
	return LinkCallFault{}, false
}

func (f LinkCallFault) err() error {
	if f.Status == 0 {
		return errors.New("ytwritetest: injected transport failure")
	}
	return &ytwrite.RemoteError{Status: f.Status, RetryAfter: f.RetryAfter, Message: "injected fault"}
}

func (l *fakeLinks) init() {
	if l.pairs == nil {
		l.pairs = map[linkPair]bool{}
		l.foreign = map[foreignLink]bool{}
	}
	if !l.typesSet {
		l.types, l.typesSet = DefaultLinkTypes(), true
	}
}

// SetLinkTypes replaces the instance's link types.
func (f *FakeYouTrack) SetLinkTypes(types []ytwrite.RemoteLinkType) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links.init()
	f.links.types = append([]ytwrite.RemoteLinkType(nil), types...)
}

// SetLinkPageCaps makes the HTTP front end return at most rows link types or
// link rows per page and at most linked issues linked issues per row,
// whatever the client asks for (0 = no cap): a server limit below $top.
func (f *FakeYouTrack) SetLinkPageCaps(rows, linkedIssues int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links.rowCap, f.links.linksCap = rows, linkedIssues
}

// QueueLinkFaults adds failures for the next matching requests of the HTTP
// front end.
func (f *FakeYouTrack) QueueLinkFaults(faults ...LinkFault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links.faults = append(f.links.faults, faults...)
}

// LinkRequests returns every request the HTTP front end received, in order.
func (f *FakeYouTrack) LinkRequests() []LinkRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LinkRequest(nil), f.links.requests...)
}

// SeedIssueLink stores a link as the issue's writer side sees it: for a
// subtask the target is the issue's parent, for a duplicate the original, for
// relates the other end.
func (f *FakeYouTrack) SeedIssueLink(issueID string, link ytwrite.IssueLink) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links.init()
	f.links.pairs[writerPair(issueID, link)] = true
}

// SeedForeignLink stores a link of a type hk does not manage (by type id),
// from -> to along the type's outward side.
func (f *FakeYouTrack) SeedForeignLink(typeID, from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links.init()
	f.links.foreign[foreignLink{typeID, from, to}] = true
}

// IssueLinks is the writer side of the issue's links as the fake holds them,
// sorted.
func (f *FakeYouTrack) IssueLinks(issueID string) []ytwrite.IssueLink {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links.init()
	return f.links.writerLinks(issueID)
}

func writerPair(issueID string, link ytwrite.IssueLink) linkPair {
	if link.Kind == ytwrite.LinkRelates && link.Target < issueID {
		return linkPair{link.Kind, link.Target, issueID}
	}
	return linkPair{link.Kind, issueID, link.Target}
}

func (l *fakeLinks) writerLinks(issueID string) []ytwrite.IssueLink {
	out := []ytwrite.IssueLink{}
	for p := range l.pairs {
		switch {
		case p.a == issueID:
			out = append(out, ytwrite.IssueLink{Kind: p.kind, Target: p.b})
		case p.b == issueID && p.kind == ytwrite.LinkRelates:
			out = append(out, ytwrite.IssueLink{Kind: p.kind, Target: p.a})
		}
	}
	ytwrite.SortLinks(out)
	return out
}

var _ ytwrite.IssueLinksRemote = (*FakeYouTrack)(nil)

// ReadIssueLinks is the in-memory read: the resolved types (a refusal when the
// instance's types do not allow it) and the issue's writer-side links.
func (f *FakeYouTrack) ReadIssueLinks(ctx context.Context, issueID string) (ytwrite.IssueLinksRead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ytwrite.IssueLinksRead{}, err
	}
	f.links.init()
	if fault, ok := f.links.callFault(LinkCall{Op: LinkOpRead, Issue: issueID}); ok {
		return ytwrite.IssueLinksRead{}, fault.err()
	}
	types, err := ytwrite.ResolveLinkTypes(f.links.types)
	if err != nil {
		return ytwrite.IssueLinksRead{}, err
	}
	return ytwrite.IssueLinksRead{Types: types, Links: f.links.writerLinks(issueID)}, nil
}

// AddIssueLink adds a link; one that exists stays.
func (f *FakeYouTrack) AddIssueLink(ctx context.Context, issueID string, types ytwrite.LinkTypes, link ytwrite.IssueLink) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.checkLinkCall(ctx, types, link); err != nil {
		return err
	}
	if fault, ok := f.links.callFault(LinkCall{Op: LinkOpAdd, Issue: issueID, Link: link}); ok {
		if fault.Landed {
			f.links.pairs[writerPair(issueID, link)] = true
		}
		return fault.err()
	}
	f.links.pairs[writerPair(issueID, link)] = true
	return nil
}

// RemoveIssueLink removes a link; one that is absent is not an error.
func (f *FakeYouTrack) RemoveIssueLink(ctx context.Context, issueID string, types ytwrite.LinkTypes, link ytwrite.IssueLink) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.checkLinkCall(ctx, types, link); err != nil {
		return err
	}
	if fault, ok := f.links.callFault(LinkCall{Op: LinkOpRemove, Issue: issueID, Link: link}); ok {
		if fault.Landed {
			delete(f.links.pairs, writerPair(issueID, link))
		}
		return fault.err()
	}
	delete(f.links.pairs, writerPair(issueID, link))
	return nil
}

func (f *FakeYouTrack) checkLinkCall(ctx context.Context, types ytwrite.LinkTypes, link ytwrite.IssueLink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.links.init()
	current, err := ytwrite.ResolveLinkTypes(f.links.types)
	if err != nil {
		return err
	}
	if types != current {
		return &ytwrite.RemoteError{Status: http.StatusNotFound, Message: "link types differ from the instance's"}
	}
	if _, ok := types.Of(link.Kind); !ok || link.Target == "" {
		return fmt.Errorf("%w: link %+v", ytwrite.ErrInvalid, link)
	}
	return nil
}

// LinkHandler is the HTTP front end (see the file comment).
func (f *FakeYouTrack) LinkHandler() http.Handler {
	return http.HandlerFunc(f.serveLinks)
}

func (f *FakeYouTrack) serveLinks(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	l := &f.links
	l.init()
	l.requests = append(l.requests, LinkRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: string(body)})
	for i := range l.faults {
		fault := &l.faults[i]
		if fault.Count <= 0 || (fault.Method != "" && fault.Method != r.Method) {
			continue
		}
		fault.Count--
		if fault.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(fault.RetryAfter/time.Second)))
		}
		linkError(w, fault.Status, "injected fault")
		return
	}
	var parts []string
	for _, p := range strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/") {
		u, err := url.PathUnescape(p)
		if err != nil {
			linkError(w, http.StatusBadRequest, "bad path")
			return
		}
		parts = append(parts, u)
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "api" && parts[1] == "issueLinkTypes":
		l.serveTypes(w, r)
	case r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "api" && parts[1] == "issues" && parts[3] == "links":
		l.serveRows(w, r, parts[2])
	case r.Method == http.MethodPost && len(parts) == 6 && parts[0] == "api" && parts[1] == "issues" && parts[3] == "links" && parts[5] == "issues":
		var in struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &in) != nil || in.ID == "" {
			linkError(w, http.StatusBadRequest, "the body names no issue id")
			return
		}
		l.mutate(w, r, parts[2], parts[4], in.ID)
	case r.Method == http.MethodDelete && len(parts) == 7 && parts[0] == "api" && parts[1] == "issues" && parts[3] == "links" && parts[5] == "issues":
		l.mutate(w, r, parts[2], parts[4], parts[6])
	default:
		linkError(w, http.StatusNotFound, "no such resource")
	}
}

func linkError(w http.ResponseWriter, status int, description string) {
	writeLinkJSON(w, status, map[string]any{"error": http.StatusText(status), "error_description": description})
}

func writeLinkJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// page reads $top and $skip: the server's own limit of 42 when $top is absent,
// and the cap below whatever is asked.
func (l *fakeLinks) page(q url.Values, key string, def int) (skip, top int, ok bool) {
	top = def
	read := func(name string, into *int) bool {
		v := q.Get(name)
		if v == "" {
			return true
		}
		n, err := strconv.Atoi(v)
		*into = n
		return err == nil && n >= 0
	}
	if !read("$skip"+key, &skip) || !read("$top"+key, &top) {
		return 0, 0, false
	}
	return skip, top, true
}

// window is the part of n items a page of top items after skip shows.
func window(n, skip, top int) (lo, hi int) {
	lo, hi = min(skip, n), n
	if top < n-lo {
		hi = lo + top
	}
	return lo, hi
}

func (l *fakeLinks) serveTypes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	skip, top, ok := l.page(q, "", 42)
	if !ok {
		linkError(w, http.StatusBadRequest, "bad $top or $skip")
		return
	}
	if l.rowCap > 0 {
		top = min(top, l.rowCap)
	}
	lo, hi := window(len(l.types), skip, top)
	out := []any{}
	for _, t := range l.types[lo:hi] {
		out = append(out, selectLinkFields(map[string]any{"$type": "IssueLinkType", "id": t.ID, "name": t.Name, "directed": t.Directed, "sourceToTarget": t.SourceToTarget, "targetToSource": t.TargetToSource}, q.Get("fields")))
	}
	writeLinkJSON(w, http.StatusOK, out)
}

// linkRowData is one IssueLink row before paging its linked issues.
type linkRowData struct {
	id        string
	direction ytwrite.LinkDirection
	typ       ytwrite.RemoteLinkType
	targets   []string
}

// writerDirection is the direction of the type on which the writer sits: the
// side whose role name is `subtask of` / `duplicates`. A type with neither is
// not one hk can write; its writer side is irrelevant to the fake.
func writerDirection(t ytwrite.RemoteLinkType) ytwrite.LinkDirection {
	role := "subtask of"
	if strings.EqualFold(strings.TrimSpace(t.Name), "Duplicate") {
		role = "duplicates"
	}
	if strings.EqualFold(strings.TrimSpace(t.SourceToTarget), role) {
		return ytwrite.LinkOutward
	}
	return ytwrite.LinkInward
}

func managedKind(t ytwrite.RemoteLinkType) (ytwrite.LinkKind, bool) {
	for _, k := range []ytwrite.LinkKind{ytwrite.LinkSubtask, ytwrite.LinkRelates, ytwrite.LinkDuplicate} {
		if strings.EqualFold(strings.TrimSpace(t.Name), string(k)) {
			return k, true
		}
	}
	return "", false
}

// rowsOf is every row of the issue's links, in the order of the link types: a
// row for each direction of each type, links or not, as YouTrack lists them.
func (l *fakeLinks) rowsOf(issueID string) []linkRowData {
	var rows []linkRowData
	for _, t := range l.types {
		dirs := []ytwrite.LinkDirection{ytwrite.LinkBoth}
		if t.Directed {
			dirs = []ytwrite.LinkDirection{ytwrite.LinkOutward, ytwrite.LinkInward}
		}
		kind, managed := managedKind(t)
		for _, dir := range dirs {
			row := linkRowData{id: strconv.Itoa(900 + len(rows)), direction: dir, typ: t}
			if managed {
				row.targets = l.managedTargets(kind, t, dir, issueID)
			} else {
				for fl := range l.foreign {
					switch {
					case fl.typeID != t.ID:
					case dir == ytwrite.LinkOutward && fl.from == issueID:
						row.targets = append(row.targets, fl.to)
					case dir == ytwrite.LinkInward && fl.to == issueID:
						row.targets = append(row.targets, fl.from)
					}
				}
			}
			sort.Strings(row.targets)
			rows = append(rows, row)
		}
	}
	return rows
}

func (l *fakeLinks) managedTargets(kind ytwrite.LinkKind, t ytwrite.RemoteLinkType, dir ytwrite.LinkDirection, issueID string) []string {
	var out []string
	for p := range l.pairs {
		if p.kind != kind {
			continue
		}
		switch {
		case kind == ytwrite.LinkRelates:
			if p.a == issueID {
				out = append(out, p.b)
			} else if p.b == issueID {
				out = append(out, p.a)
			}
		case dir == writerDirection(t) && p.a == issueID:
			out = append(out, p.b)
		case dir != writerDirection(t) && p.b == issueID:
			out = append(out, p.a)
		}
	}
	return out
}

func (l *fakeLinks) serveRows(w http.ResponseWriter, r *http.Request, issueID string) {
	q := r.URL.Query()
	skip, top, ok := l.page(q, "", 42)
	if !ok {
		linkError(w, http.StatusBadRequest, "bad $top or $skip")
		return
	}
	skipLinks, topLinks, ok2 := l.page(q, "Links", int(^uint(0)>>1))
	if !ok2 {
		linkError(w, http.StatusBadRequest, "bad $topLinks or $skipLinks")
		return
	}
	if l.rowCap > 0 {
		top = min(top, l.rowCap)
	}
	if l.linksCap > 0 {
		topLinks = min(topLinks, l.linksCap)
	}
	rows := l.rowsOf(issueID)
	lo, hi := window(len(rows), skip, top)
	out := []any{}
	for _, row := range rows[lo:hi] {
		issues := []any{}
		a, b := window(len(row.targets), skipLinks, topLinks)
		for _, id := range row.targets[a:b] {
			issues = append(issues, map[string]any{"$type": "Issue", "id": id})
		}
		out = append(out, selectLinkFields(map[string]any{
			"$type":     "IssueLink",
			"id":        row.id,
			"direction": string(row.direction),
			"linkType":  map[string]any{"$type": "IssueLinkType", "id": row.typ.ID, "name": row.typ.Name, "directed": row.typ.Directed, "sourceToTarget": row.typ.SourceToTarget, "targetToSource": row.typ.TargetToSource},
			"issues":    issues,
		}, q.Get("fields")))
	}
	writeLinkJSON(w, http.StatusOK, out)
}

// mutate answers POST (add) and DELETE (remove) of target on the issue's side
// linkID names: a type id, with `s` for the outward and `t` for the inward side
// of a directed type and nothing for an undirected one.
func (l *fakeLinks) mutate(w http.ResponseWriter, r *http.Request, issueID, linkID, target string) {
	var typ ytwrite.RemoteLinkType
	var dir ytwrite.LinkDirection
	found := false
	for _, t := range l.types {
		switch {
		case !t.Directed && linkID == t.ID:
			typ, dir, found = t, ytwrite.LinkBoth, true
		case t.Directed && linkID == t.ID+"s":
			typ, dir, found = t, ytwrite.LinkOutward, true
		case t.Directed && linkID == t.ID+"t":
			typ, dir, found = t, ytwrite.LinkInward, true
		}
	}
	if !found {
		linkError(w, http.StatusNotFound, "link type "+linkID+" not found")
		return
	}
	if target == issueID {
		linkError(w, http.StatusBadRequest, "an issue cannot be linked to itself")
		return
	}
	var present bool
	var set func(bool)
	if kind, managed := managedKind(typ); managed {
		p := linkPair{kind: kind, a: issueID, b: target}
		switch {
		case kind == ytwrite.LinkRelates && target < issueID:
			p = linkPair{kind: kind, a: target, b: issueID}
		case kind != ytwrite.LinkRelates && dir != writerDirection(typ):
			p = linkPair{kind: kind, a: target, b: issueID}
		}
		present = l.pairs[p]
		set = func(on bool) {
			if on {
				l.pairs[p] = true
			} else {
				delete(l.pairs, p)
			}
		}
	} else {
		fl := foreignLink{typeID: typ.ID, from: issueID, to: target}
		if dir == ytwrite.LinkInward {
			fl = foreignLink{typeID: typ.ID, from: target, to: issueID}
		}
		present = l.foreign[fl]
		set = func(on bool) {
			if on {
				l.foreign[fl] = true
			} else {
				delete(l.foreign, fl)
			}
		}
	}
	if r.Method == http.MethodDelete && !present {
		linkError(w, http.StatusNotFound, "issue "+target+" is not linked")
		return
	}
	set(r.Method == http.MethodPost)
	writeLinkJSON(w, http.StatusOK, selectLinkFields(map[string]any{"$type": "Issue", "id": target}, r.URL.Query().Get("fields")))
}

// selectLinkFields keeps only the attributes of the `fields` query, as
// YouTrack does: nested ones as name(sub,sub); no fields at all leaves the id.
func selectLinkFields(v map[string]any, fields string) map[string]any {
	if strings.TrimSpace(fields) == "" {
		return map[string]any{"id": v["id"]}
	}
	sel, _ := parseLinkFields(fields)
	return sel.apply(v)
}

type linkFieldSel map[string]linkFieldSel

func parseLinkFields(s string) (linkFieldSel, string) {
	sel := linkFieldSel{}
	for s != "" {
		end := strings.IndexAny(s, ",()")
		if end < 0 {
			end = len(s)
		}
		name := strings.TrimSpace(s[:end])
		s = s[end:]
		var sub linkFieldSel
		if strings.HasPrefix(s, "(") {
			sub, s = parseLinkFields(s[1:])
		}
		if name != "" {
			sel[name] = sub
		}
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case strings.HasPrefix(s, ")"):
			return sel, s[1:]
		}
	}
	return sel, ""
}

func (sel linkFieldSel) apply(v map[string]any) map[string]any {
	out := map[string]any{}
	for name, sub := range sel {
		val, ok := v[name]
		if !ok {
			continue
		}
		switch x := val.(type) {
		case map[string]any:
			if sub != nil {
				val = sub.apply(x)
			}
		case []any:
			if sub != nil {
				list := make([]any, len(x))
				for i, e := range x {
					list[i] = sub.apply(e.(map[string]any))
				}
				val = list
			}
		}
		out[name] = val
	}
	return out
}
