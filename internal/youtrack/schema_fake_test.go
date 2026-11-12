package youtrack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// fakeTenant is a small YouTrack administration API over HTTP: projects,
// custom field definitions, project fields, enum and state bundles and link
// types, with the rules the schema run depends on (a short name is unique, a
// value name is unique in its bundle, a field attaches once, a project field's
// $type matches its definition). It records every request and refuses DELETE.
type fakeTenant struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	nextID   int
	me       User
	projects []*fakeProject
	globals  []*fakeGlobal
	bundles  map[string]*fakeBundle // by id; kind is in the bundle
	links    []LinkType
	requests []string // "METHOD path", in arrival order
	live     atomic.Int32
	maxLive  atomic.Int32
	// hook may answer a request itself: a non-zero status is sent as is.
	hook func(method, path string, n int) (status int, header map[string]string)
}

type fakeProject struct {
	ID, Short, Name, Leader string
	Fields                  []*fakeProjectField
}

type fakeProjectField struct {
	ID, Type, GlobalID, BundleID string
}

type fakeGlobal struct{ ID, Name, Type string }

type fakeBundle struct {
	ID, Name string
	Kind     FieldKind
	Values   []*fakeElement
}

type fakeElement struct {
	ID, Name string
	Ordinal  int
	Resolved bool
}

func newFakeTenant(t *testing.T) *fakeTenant {
	f := &fakeTenant{t: t, nextID: 100, bundles: map[string]*fakeBundle{}, me: User{ID: "1-1", Login: "hk-bot", Name: "hk bot"}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTenant) id(prefix string) string {
	f.nextID++
	return prefix + "-" + strconv.Itoa(f.nextID)
}

func (f *fakeTenant) client(t *testing.T, cfg Config) *Client {
	t.Helper()
	cfg.BaseURL, cfg.Token = f.srv.URL, "token"
	if cfg.Sleep == nil {
		cfg.Sleep = func(context.Context, time.Duration) error { return nil }
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeTenant) global(name string) *fakeGlobal {
	for _, g := range f.globals {
		if sameFieldName(g.Name, name) {
			return g
		}
	}
	return nil
}

func (f *fakeTenant) addGlobal(name, typ string) *fakeGlobal {
	g := &fakeGlobal{ID: f.id("58"), Name: name, Type: typ}
	f.globals = append(f.globals, g)
	return g
}

func (f *fakeTenant) addProject(short, name string) *fakeProject {
	p := &fakeProject{ID: f.id("0"), Short: short, Name: name, Leader: f.me.ID}
	f.projects = append(f.projects, p)
	return p
}

func (f *fakeTenant) newBundle(kind FieldKind, name string, values []BundleValue) *fakeBundle {
	if kind == KindMultiEnum {
		kind = KindEnum // the enum bundle path serves single and multi alike
	}
	b := &fakeBundle{ID: f.id("67"), Name: name, Kind: kind}
	for i, v := range values {
		b.Values = append(b.Values, &fakeElement{ID: f.id("68"), Name: v.Name, Ordinal: i, Resolved: v.Resolved})
	}
	f.bundles[b.ID] = b
	return b
}

// attach puts a field on a project as the import did: its own bundle.
func (f *fakeTenant) attach(p *fakeProject, name string, kind FieldKind, values []BundleValue) *fakeBundle {
	g := f.global(name)
	if g == nil {
		g = f.addGlobal(name, fieldTypeID(kind))
	}
	pf := &fakeProjectField{ID: f.id("90"), GlobalID: g.ID, Type: projectFieldType(kind)}
	var b *fakeBundle
	if hasBundle(kind) {
		b = f.newBundle(kind, p.Short+" "+name, values)
		pf.BundleID = b.ID
	}
	p.Fields = append(p.Fields, pf)
	return b
}

func projectFieldType(kind FieldKind) string {
	return map[FieldKind]string{KindState: "StateProjectCustomField", KindEnum: "EnumProjectCustomField", KindMultiEnum: "EnumProjectCustomField", KindDate: "SimpleProjectCustomField", KindPeriod: "PeriodProjectCustomField", KindString: "SimpleProjectCustomField", KindInt: "SimpleProjectCustomField"}[kind]
}

// seedBuiltinLinks adds the three link types YouTrack ships.
func (f *fakeTenant) seedBuiltinLinks() {
	f.links = []LinkType{
		{ID: "105-0", Name: "Relates", Directed: false, SourceToTarget: "relates to", TargetToSource: "relates to"},
		{ID: "105-1", Name: "Duplicate", Directed: true, SourceToTarget: "duplicates", TargetToSource: "is duplicated by"},
		{ID: "105-3", Name: "Subtask", Directed: true, SourceToTarget: "parent for", TargetToSource: "subtask of"},
	}
}

func (f *fakeTenant) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func page[T any](r *http.Request, all []T) []T {
	skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
	top, err := strconv.Atoi(r.URL.Query().Get("$top"))
	if err != nil || top < 1 {
		top = len(all)
	}
	if skip > len(all) {
		skip = len(all)
	}
	end := skip + top
	if end > len(all) {
		end = len(all)
	}
	return all[skip:end]
}

func (f *fakeTenant) serve(w http.ResponseWriter, r *http.Request) {
	// Counted before the lock, so two requests in flight at once show.
	n := f.live.Add(1)
	defer f.live.Add(-1)
	for {
		m := f.maxLive.Load()
		if n <= m || f.maxLive.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	f.requests = append(f.requests, r.Method+" "+path)
	if r.Method == http.MethodDelete {
		f.t.Errorf("DELETE %s: the schema run never deletes", path)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "no delete"})
		return
	}
	if f.hook != nil {
		if status, header := f.hook(r.Method, path, len(f.requests)); status != 0 {
			for k, v := range header {
				w.Header().Set(k, v)
			}
			writeJSON(w, status, map[string]string{"error": "hooked"})
			return
		}
	}
	var body map[string]any
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/") // api admin ...
	switch {
	case path == "/api/users/me" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, f.me)
	case path == "/api/admin/projects" && r.Method == http.MethodGet:
		out := []map[string]any{}
		for _, p := range f.projects {
			out = append(out, f.projectJSON(p))
		}
		writeJSON(w, http.StatusOK, page(r, out))
	case path == "/api/admin/projects" && r.Method == http.MethodPost:
		f.createProject(w, body)
	case len(parts) == 5 && parts[2] == "projects" && parts[4] == "customFields":
		f.projectFields(w, r, parts[3], body)
	case path == "/api/admin/customFieldSettings/customFields" && r.Method == http.MethodGet:
		out := []map[string]any{}
		for _, g := range f.globals {
			out = append(out, map[string]any{"id": g.ID, "name": g.Name, "fieldType": map[string]any{"id": g.Type}})
		}
		writeJSON(w, http.StatusOK, page(r, out))
	case path == "/api/admin/customFieldSettings/customFields" && r.Method == http.MethodPost:
		name, _ := body["name"].(string)
		typ, _ := body["fieldType"].(map[string]any)["id"].(string)
		if name == "" || f.global(name) != nil || typ == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad field"})
			return
		}
		g := f.addGlobal(name, typ)
		writeJSON(w, http.StatusOK, map[string]any{"id": g.ID, "name": g.Name, "fieldType": map[string]any{"id": g.Type}})
	case len(parts) >= 5 && parts[2] == "customFieldSettings" && parts[3] == "bundles":
		f.bundleRoute(w, r, parts[4:], body)
	case path == "/api/issueLinkTypes" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, page(r, f.links))
	default:
		f.t.Errorf("fake tenant: unrouted %s %s", r.Method, path)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not routed"})
	}
}

func (f *fakeTenant) projectJSON(p *fakeProject) map[string]any {
	return map[string]any{"id": p.ID, "shortName": p.Short, "name": p.Name, "leader": map[string]any{"id": p.Leader}}
}

func (f *fakeTenant) createProject(w http.ResponseWriter, body map[string]any) {
	name, _ := body["name"].(string)
	short, _ := body["shortName"].(string)
	leader, _ := body["leader"].(map[string]any)
	leaderID, _ := leader["id"].(string)
	if name == "" || short == "" || leaderID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, shortName and leader are required"})
		return
	}
	for _, p := range f.projects {
		if strings.EqualFold(p.Short, short) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "short name taken"})
			return
		}
	}
	p := f.addProject(short, name)
	p.Leader = leaderID
	writeJSON(w, http.StatusOK, f.projectJSON(p))
}

func (f *fakeTenant) projectFields(w http.ResponseWriter, r *http.Request, projectID string, body map[string]any) {
	var p *fakeProject
	for _, x := range f.projects {
		if x.ID == projectID {
			p = x
		}
	}
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no project"})
		return
	}
	render := func(pf *fakeProjectField) map[string]any {
		g := f.globalByID(pf.GlobalID)
		out := map[string]any{"id": pf.ID, "$type": pf.Type, "field": map[string]any{"id": g.ID, "name": g.Name, "fieldType": map[string]any{"id": g.Type}}}
		if pf.BundleID != "" {
			out["bundle"] = map[string]any{"id": pf.BundleID, "$type": bundleTypeName(f.bundles[pf.BundleID].Kind)}
		}
		return out
	}
	if r.Method == http.MethodGet {
		out := []map[string]any{}
		for _, pf := range p.Fields {
			out = append(out, render(pf))
		}
		writeJSON(w, http.StatusOK, page(r, out))
		return
	}
	typ, _ := body["$type"].(string)
	fieldRef, _ := body["field"].(map[string]any)
	g := f.globalByID(stringOf(fieldRef["id"]))
	if g == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown field"})
		return
	}
	for _, pf := range p.Fields {
		if pf.GlobalID == g.ID {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already attached"})
			return
		}
	}
	want := map[string]string{"state[1]": "StateProjectCustomField", "enum[1]": "EnumProjectCustomField", "enum[*]": "EnumProjectCustomField", "date": "SimpleProjectCustomField", "period": "PeriodProjectCustomField", "string": "SimpleProjectCustomField", "integer": "SimpleProjectCustomField"}[g.Type]
	if typ != want {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project field type " + typ + " does not fit " + g.Type})
		return
	}
	pf := &fakeProjectField{ID: f.id("90"), GlobalID: g.ID, Type: typ}
	if bundle, ok := body["bundle"].(map[string]any); ok {
		b := f.bundles[stringOf(bundle["id"])]
		if b == nil || bundleTypeName(b.Kind) != stringOf(bundle["$type"]) || (g.Type == "state[1]") != (b.Kind == KindState) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bundle does not fit"})
			return
		}
		pf.BundleID = b.ID
	} else if hasBundle(kindOfType(g.Type)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bundle required"})
		return
	}
	p.Fields = append(p.Fields, pf)
	writeJSON(w, http.StatusOK, render(pf))
}

func kindOfType(typ string) FieldKind {
	switch typ {
	case "state[1]":
		return KindState
	case "enum[1]":
		return KindEnum
	case "enum[*]":
		return KindMultiEnum
	case "date":
		return KindDate
	case "string":
		return KindString
	case "integer":
		return KindInt
	}
	return KindPeriod
}

func bundleTypeName(kind FieldKind) string {
	if kind == KindState {
		return "StateBundle"
	}
	return "EnumBundle"
}

func stringOf(v any) string { s, _ := v.(string); return s }

func (f *fakeTenant) globalByID(id string) *fakeGlobal {
	for _, g := range f.globals {
		if g.ID == id {
			return g
		}
	}
	return nil
}

func (f *fakeTenant) bundleJSON(b *fakeBundle) map[string]any {
	values := []map[string]any{}
	for _, v := range b.Values {
		el := map[string]any{"id": v.ID, "name": v.Name, "ordinal": v.Ordinal}
		if b.Kind == KindState {
			el["isResolved"] = v.Resolved
		}
		values = append(values, el)
	}
	return map[string]any{"id": b.ID, "$type": bundleTypeName(b.Kind), "name": b.Name, "values": values}
}

func (f *fakeTenant) bundleRoute(w http.ResponseWriter, r *http.Request, rest []string, body map[string]any) {
	kind := map[string]FieldKind{"state": KindState, "enum": KindEnum}[rest[0]]
	if _, ok := map[string]bool{"state": true, "enum": true}[rest[0]]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bundle kind"})
		return
	}
	if len(rest) == 1 && r.Method == http.MethodPost {
		name, _ := body["name"].(string)
		var values []BundleValue
		raw, _ := body["values"].([]any)
		for _, e := range raw {
			m, _ := e.(map[string]any)
			resolved, _ := m["isResolved"].(bool)
			values = append(values, BundleValue{Name: stringOf(m["name"]), Resolved: resolved})
		}
		if name == "" || stringOf(body["$type"]) != bundleTypeName(kind) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad bundle"})
			return
		}
		writeJSON(w, http.StatusOK, f.bundleJSON(f.newBundle(kind, name, values)))
		return
	}
	b := f.bundles[rest[1]]
	if b == nil || b.Kind != kind {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no bundle"})
		return
	}
	switch {
	case len(rest) == 2 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, f.bundleJSON(b))
	case len(rest) == 3 && rest[2] == "values" && r.Method == http.MethodPost:
		name := stringOf(body["name"])
		for _, v := range b.Values {
			if normalizeLabel(v.Name) == normalizeLabel(name) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "value exists"})
				return
			}
		}
		el := &fakeElement{ID: f.id("68"), Name: name, Ordinal: len(b.Values)}
		if n, ok := body["ordinal"].(float64); ok {
			el.Ordinal = int(n)
		}
		el.Resolved, _ = body["isResolved"].(bool)
		b.Values = append(b.Values, el)
		writeJSON(w, http.StatusOK, map[string]any{"id": el.ID, "name": el.Name, "ordinal": el.Ordinal})
	case len(rest) == 4 && rest[2] == "values" && r.Method == http.MethodPost:
		if _, renamed := body["name"]; renamed {
			f.t.Errorf("value update carries a name: a value is never renamed")
		}
		for _, v := range b.Values {
			if v.ID != rest[3] {
				continue
			}
			if n, ok := body["ordinal"].(float64); ok {
				v.Ordinal = int(n)
			}
			if res, ok := body["isResolved"].(bool); ok {
				v.Resolved = res
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": v.ID, "name": v.Name, "ordinal": v.Ordinal, "isResolved": v.Resolved})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no value"})
	default:
		f.t.Errorf("fake tenant: unrouted bundle %s %v", r.Method, rest)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not routed"})
	}
}

// fakeSchemaStore is the hk side: a vocabulary and the mapping table.
type fakeSchemaStore struct {
	mu         sync.Mutex
	projects   []string
	lanes      map[string][]string
	milestones map[string][]string
	rows       map[string]store.YouTrackProject
	puts       []store.YouTrackProject
}

func (s *fakeSchemaStore) ListTaskProjects(context.Context) ([]string, error) {
	return append([]string(nil), s.projects...), nil
}
func (s *fakeSchemaStore) ListTaskLanes(context.Context) (map[string][]string, error) {
	return s.lanes, nil
}
func (s *fakeSchemaStore) ListMilestoneNames(context.Context) (map[string][]string, error) {
	return s.milestones, nil
}
func (s *fakeSchemaStore) ListYouTrackProjects(context.Context) ([]store.YouTrackProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.YouTrackProject
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}
func (s *fakeSchemaStore) PutYouTrackProject(_ context.Context, p store.YouTrackProject) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]store.YouTrackProject{}
	}
	s.rows[p.HKProject] = p
	s.puts = append(s.puts, p)
	return nil
}

// share attaches the bundle another project's field already uses.
func (f *fakeTenant) share(p *fakeProject, name string, kind FieldKind, bundle *fakeBundle) {
	g := f.global(name)
	p.Fields = append(p.Fields, &fakeProjectField{ID: f.id("90"), GlobalID: g.ID, Type: projectFieldType(kind), BundleID: bundle.ID})
}
