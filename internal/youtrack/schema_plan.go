package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The schema run (hk 3836) brings a YouTrack tenant to the schema Desired
// declares. Plan reads the tenant and the hk store and lists what is missing;
// Apply performs exactly that list, in a fixed order, one request at a time.
// Both only add: no value, field, bundle or project is deleted or renamed and
// no field is detached.

// Phases of a change, in the order Apply runs them: projects, field
// definitions, field attachments, bundle values, then flags and ordinals.
const (
	phaseProject = iota + 1
	phaseFieldDef
	phaseAttach
	phaseValue
	phaseFlag
)

// Change is one planned step. Project is the hk project, empty for the whole
// tenant; Object, Action and Value are the line the operator reads.
type Change struct {
	Project string `json:"project"`
	Object  string `json:"object"`
	Action  string `json:"action"`
	Value   string `json:"value"`

	phase int
	run   func(ctx context.Context) error
}

// Line is the change as one tab-separated text line.
func (c Change) Line() string {
	project := c.Project
	if project == "" {
		project = "-"
	}
	return project + "\t" + c.Object + "\t" + c.Action + "\t" + c.Value
}

// Report is a finding Apply cannot fix: a built-in the tenant should have.
type Report struct {
	Object string `json:"object"`
	Status string `json:"status"`
	Value  string `json:"value"`
}

// Plan is the changes in the order Apply runs them, and the reports.
type Plan struct {
	Changes []Change `json:"changes"`
	Reports []Report `json:"reports"`
}

// Text renders one line per change, then one `report` line per report.
func (p Plan) Text() string {
	var b strings.Builder
	for _, c := range p.Changes {
		b.WriteString(c.Line())
		b.WriteByte('\n')
	}
	for _, r := range p.Reports {
		b.WriteString("report\t" + r.Object + "\t" + r.Status + "\t" + r.Value + "\n")
	}
	return b.String()
}

// JSON renders the plan as one JSON document.
func (p Plan) JSON() ([]byte, error) {
	if p.Changes == nil {
		p.Changes = []Change{}
	}
	if p.Reports == nil {
		p.Reports = []Report{}
	}
	return json.Marshal(p)
}

// SchemaStore is the hk side of the run: the vocabulary the schema is built
// from and the project mapping it writes.
type SchemaStore interface {
	ListTaskProjects(ctx context.Context) ([]string, error)
	ListTaskLanes(ctx context.Context) ([]string, error)
	ListMilestoneNames(ctx context.Context) (map[string][]string, error)
	ListYouTrackProjects(ctx context.Context) ([]store.YouTrackProject, error)
	PutYouTrackProject(ctx context.Context, p store.YouTrackProject) error
}

// SchemaRunner plans and applies the schema.
type SchemaRunner struct {
	Client *Client
	Store  SchemaStore
	Schema Schema
}

// ErrSchemaProject is a --project that is not an hk project.
var ErrSchemaProject = errors.New("youtrack_schema_project")

// reservedShortNames are the short names the import gives its synthetic
// buckets (youtrack/import/client.js getProjects): never handed to a new hk
// project.
var reservedShortNames = []string{"SHRDOC", "HK", "DEC"}

// ShortNameFor is the import's project key rule (youtrack/import/client.js,
// getProjects → addProject): the name upper-cased, everything outside A-Z0-9
// dropped, cut to 10; a key that does not start with a letter becomes "P" plus
// the first nine; under two characters it is padded with "0"; a key already
// taken gets a numeric tail (2, 3, ...) that keeps the total at 10. taken holds
// upper-case keys and is not changed.
func ShortNameFor(name string, taken map[string]bool) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	key := b.String()
	if len(key) > 10 {
		key = key[:10]
	}
	if key == "" {
		return "", fmt.Errorf("%w: cannot form a YouTrack project key from %q", ErrSchemaProject, name)
	}
	if key[0] < 'A' || key[0] > 'Z' {
		if len(key) > 9 {
			key = key[:9]
		}
		key = "P" + key
	}
	if len(key) < 2 {
		key += "0"
	}
	base := key
	for suffix := 2; taken[key]; suffix++ {
		tail := fmt.Sprint(suffix)
		cut := base
		if len(cut) > 10-len(tail) {
			cut = cut[:10-len(tail)]
		}
		key = cut + tail
	}
	return key, nil
}

func fieldTypeID(kind FieldKind) string {
	switch kind {
	case KindState:
		return "state[1]"
	case KindEnum:
		return "enum[1]"
	case KindDate:
		return "date"
	}
	return "period"
}

func hasBundle(kind FieldKind) bool { return kind == KindState || kind == KindEnum }

// projectState is what one hk project's changes share: the YouTrack project id
// is known at plan time for an existing project and only once the project is
// created for a new one.
type projectState struct {
	hk    string
	short string
	ytID  string
}

// globalRef is a field definition: found at plan time, or created by the
// field-definition change that precedes every attach of it.
type globalRef struct {
	id string
}

// Plan reads the tenant and hk and returns what Apply would do. project limits
// it to one hk project; empty is every hk project.
func (r *SchemaRunner) Plan(ctx context.Context, project string) (Plan, error) {
	vocab, hkProjects, err := r.loadVocabulary(ctx, project)
	if err != nil {
		return Plan{}, err
	}
	rows, err := r.Store.ListYouTrackProjects(ctx)
	if err != nil {
		return Plan{}, err
	}
	rowFor := map[string]store.YouTrackProject{}
	for _, row := range rows {
		rowFor[row.HKProject] = row
	}
	ytProjects, err := r.Client.ListAdminProjects(ctx)
	if err != nil {
		return Plan{}, err
	}
	taken := map[string]bool{}
	for _, name := range reservedShortNames {
		taken[name] = true
	}
	for _, p := range ytProjects {
		taken[strings.ToUpper(p.ShortName)] = true
	}
	globals, err := r.Client.ListGlobalFields(ctx)
	if err != nil {
		return Plan{}, err
	}
	known := map[string]*globalRef{}
	for _, g := range globals {
		known[normalizeLabel(g.Name)] = &globalRef{id: g.ID}
	}

	var plan Plan
	for _, hk := range hkProjects {
		ps, changes, err := r.planProject(hk, rowFor, ytProjects, taken)
		if err != nil {
			return Plan{}, err
		}
		plan.Changes = append(plan.Changes, changes...)
		fieldChanges, err := r.planFields(ctx, ps, vocab, known)
		if err != nil {
			return Plan{}, err
		}
		plan.Changes = append(plan.Changes, fieldChanges...)
	}
	// The sort is stable, so within a phase the order is hk project name, then
	// the order of Desired.
	sort.SliceStable(plan.Changes, func(i, j int) bool { return plan.Changes[i].phase < plan.Changes[j].phase })

	linkTypes, err := r.Client.ListLinkTypes(ctx)
	if err != nil {
		return Plan{}, err
	}
	plan.Reports = linkTypeReports(linkTypes)
	return plan, nil
}

func (r *SchemaRunner) loadVocabulary(ctx context.Context, project string) (Vocabulary, []string, error) {
	all, err := r.Store.ListTaskProjects(ctx)
	if err != nil {
		return Vocabulary{}, nil, err
	}
	sort.Strings(all)
	hkProjects := all
	if project != "" {
		hkProjects = nil
		for _, name := range all {
			if name == project {
				hkProjects = []string{name}
			}
		}
		if hkProjects == nil {
			return Vocabulary{}, nil, fmt.Errorf("%w: %q is not an hk project", ErrSchemaProject, project)
		}
	}
	lanes, err := r.Store.ListTaskLanes(ctx)
	if err != nil {
		return Vocabulary{}, nil, err
	}
	milestones, err := r.Store.ListMilestoneNames(ctx)
	if err != nil {
		return Vocabulary{}, nil, err
	}
	return Vocabulary{Kinds: store.TaskKinds(), Lanes: lanes, Areas: store.TaskAreas(), Milestones: milestones}, hkProjects, nil
}

// planProject finds or plans the YouTrack project of one hk project and its
// youtrack_projects row. An existing mapping row wins: its short name is what
// sync uses. Without a row a YouTrack project named like the hk project (what
// the import created) is adopted; only then is one created.
func (r *SchemaRunner) planProject(hk string, rowFor map[string]store.YouTrackProject, yt []AdminProject, taken map[string]bool) (*projectState, []Change, error) {
	row, hasRow := rowFor[hk]
	var found *AdminProject
	for i := range yt {
		p := &yt[i]
		if !hasRow {
			if p.Name == hk {
				found = p
				break
			}
			continue
		}
		if row.ProjectID != "" && p.ID == row.ProjectID {
			found = p
			break
		}
		if found == nil && strings.EqualFold(p.ShortName, row.ShortName) {
			found = p
		}
	}
	ps := &projectState{hk: hk}
	var changes []Change
	if found != nil {
		ps.ytID, ps.short = found.ID, found.ShortName
	} else {
		if hasRow {
			ps.short = row.ShortName
		} else {
			short, err := ShortNameFor(hk, taken)
			if err != nil {
				return nil, nil, err
			}
			ps.short = short
		}
		taken[strings.ToUpper(ps.short)] = true
		changes = append(changes, Change{Project: hk, Object: "project", Action: "create", Value: ps.short, phase: phaseProject,
			run: func(ctx context.Context) error {
				me, err := r.Client.Me(ctx)
				if err != nil {
					return err
				}
				created, err := r.Client.CreateProject(ctx, hk, ps.short, me.ID)
				if err != nil {
					return err
				}
				ps.ytID = created.ID
				return nil
			}})
	}
	if !hasRow || found == nil {
		changes = append(changes, Change{Project: hk, Object: "mapping", Action: "put", Value: ps.short, phase: phaseProject,
			run: func(ctx context.Context) error {
				return r.Store.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: hk, ProjectID: ps.ytID, ShortName: ps.short})
			}})
	}
	return ps, changes, nil
}

// planFields plans the fields of one project. A project that does not exist
// yet has none, so every field is attached with its whole value set.
func (r *SchemaRunner) planFields(ctx context.Context, ps *projectState, vocab Vocabulary, known map[string]*globalRef) ([]Change, error) {
	var attached []ProjectField
	if ps.ytID != "" {
		var err error
		if attached, err = r.Client.ListProjectFields(ctx, ps.ytID); err != nil {
			return nil, err
		}
	}
	var changes []Change
	for _, spec := range r.Schema.Desired(vocab, ps.hk) {
		spec := spec
		var have *ProjectField
		for i := range attached {
			if sameFieldName(attached[i].Field.Name, spec.Name) {
				have = &attached[i]
				break
			}
		}
		if have == nil {
			changes = append(changes, r.planAttach(ps, spec, known)...)
			continue
		}
		if !hasBundle(spec.Kind) || have.Bundle == nil {
			continue
		}
		valueChanges, err := r.planValues(ctx, ps, spec, have.Bundle.ID)
		if err != nil {
			return nil, err
		}
		changes = append(changes, valueChanges...)
	}
	return changes, nil
}

func (r *SchemaRunner) planAttach(ps *projectState, spec FieldSpec, known map[string]*globalRef) []Change {
	key := normalizeLabel(spec.Name)
	ref := known[key]
	var changes []Change
	if ref == nil {
		ref = &globalRef{}
		known[key] = ref
		changes = append(changes, Change{Object: spec.Name, Action: "create-field", Value: fieldTypeID(spec.Kind), phase: phaseFieldDef,
			run: func(ctx context.Context) error {
				g, err := r.Client.CreateGlobalField(ctx, spec.Name, fieldTypeID(spec.Kind))
				if err != nil {
					return err
				}
				ref.id = g.ID
				return nil
			}})
	}
	value := fieldTypeID(spec.Kind)
	if hasBundle(spec.Kind) {
		value = fmt.Sprintf("%s with %d values", fieldTypeID(spec.Kind), len(spec.Values))
	}
	changes = append(changes, Change{Project: ps.hk, Object: spec.Name, Action: "attach", Value: value, phase: phaseAttach,
		run: func(ctx context.Context) error {
			if ps.ytID == "" || ref.id == "" {
				return fmt.Errorf("attach %q to %s: project or field definition missing", spec.Name, ps.hk)
			}
			bundleID := ""
			if hasBundle(spec.Kind) {
				b, err := r.Client.CreateBundle(ctx, spec.Kind, ps.short+" "+spec.Name, spec.Values)
				if err != nil {
					return err
				}
				bundleID = b.ID
			}
			_, err := r.Client.AttachProjectField(ctx, ps.ytID, spec.Kind, ref.id, bundleID)
			return err
		}})
	return changes
}

// planValues compares one attached field's bundle with the spec: missing
// values are added; a state bundle's existing values also get their resolved
// flag and ordinal corrected. Values the spec does not name are left alone.
func (r *SchemaRunner) planValues(ctx context.Context, ps *projectState, spec FieldSpec, bundleID string) ([]Change, error) {
	bundle, err := r.Client.GetBundle(ctx, spec.Kind, bundleID)
	if err != nil {
		return nil, err
	}
	have := map[string]BundleElement{}
	for _, el := range bundle.Values {
		have[normalizeLabel(el.Name)] = el
	}
	var changes []Change
	for _, want := range spec.Values {
		want := want
		el, ok := have[normalizeLabel(want.Name)]
		if !ok {
			value := want.Name
			if spec.Kind == KindState {
				value = fmt.Sprintf("%s (resolved=%t ordinal=%d)", want.Name, want.Resolved, want.Ordinal)
			}
			changes = append(changes, Change{Project: ps.hk, Object: spec.Name, Action: "add-value", Value: value, phase: phaseValue,
				run: func(ctx context.Context) error {
					_, err := r.Client.AddBundleValue(ctx, spec.Kind, bundleID, want)
					return err
				}})
			continue
		}
		if spec.Kind != KindState {
			continue
		}
		if el.IsResolved != want.Resolved {
			changes = append(changes, Change{Project: ps.hk, Object: spec.Name, Action: "set-resolved", Value: fmt.Sprintf("%s=%t", el.Name, want.Resolved), phase: phaseFlag,
				run: func(ctx context.Context) error {
					return r.Client.SetStateElement(ctx, bundleID, el.ID, map[string]any{"isResolved": want.Resolved})
				}})
		}
		if el.Ordinal != want.Ordinal {
			changes = append(changes, Change{Project: ps.hk, Object: spec.Name, Action: "set-ordinal", Value: fmt.Sprintf("%s=%d", el.Name, want.Ordinal), phase: phaseFlag,
				run: func(ctx context.Context) error {
					return r.Client.SetStateElement(ctx, bundleID, el.ID, map[string]any{"ordinal": want.Ordinal})
				}})
		}
	}
	return changes, nil
}

// builtinLinkTypes are the YouTrack link types hk relies on. They are only
// checked: the run never creates a link type.
var builtinLinkTypes = []struct {
	label    string
	names    []string
	directed bool
}{
	{"Subtask", []string{"subtask"}, true},
	{"Relates", []string{"relates", "relate"}, false},
	{"Duplicate", []string{"duplicate"}, true},
}

func linkTypeReports(types []LinkType) []Report {
	var reports []Report
	for _, want := range builtinLinkTypes {
		var found *LinkType
		for i := range types {
			for _, name := range want.names {
				if strings.EqualFold(strings.TrimSpace(types[i].Name), name) {
					found = &types[i]
				}
			}
		}
		switch {
		case found == nil:
			reports = append(reports, Report{Object: "link-type", Status: "missing", Value: want.label})
		case found.Directed != want.directed:
			reports = append(reports, Report{Object: "link-type", Status: "mismatch", Value: fmt.Sprintf("%s directed=%t want %t", want.label, found.Directed, want.directed)})
		}
	}
	return reports
}

// Apply runs the plan's changes in order and stops at the first error, so a
// later change never lands after a failed one. Every change is logged.
func (r *SchemaRunner) Apply(ctx context.Context, plan Plan) (int, error) {
	for _, rep := range plan.Reports {
		log.Printf("youtrack.schema_report object=%q status=%q value=%q", rep.Object, rep.Status, rep.Value)
	}
	applied := 0
	for _, c := range plan.Changes {
		if err := c.run(ctx); err != nil {
			log.Printf("youtrack.schema_apply_failed project=%q object=%q action=%q value=%q err=%q", c.Project, c.Object, c.Action, c.Value, err)
			return applied, fmt.Errorf("schema %s %s %s %q: %w", c.Project, c.Object, c.Action, c.Value, err)
		}
		applied++
		log.Printf("youtrack.schema_applied project=%q object=%q action=%q value=%q", c.Project, c.Object, c.Action, c.Value)
	}
	return applied, nil
}
