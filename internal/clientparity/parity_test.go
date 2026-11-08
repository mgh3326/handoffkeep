// Package clientparity holds only tests: it proves the public client's wire
// types (package client, which may not import internal/store) still match the
// store types the server writes and reads, so a renamed or retyped store field
// breaks the hk build here instead of the fleet services at run time.
package clientparity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/client"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// pairs maps each client wire type onto the store type the server encodes
// (answers) or decodes (request bodies) on the same route.
var pairs = []struct {
	client, store any
}{
	{client.Task{}, store.Task{}},
	{client.TaskRefs{}, store.TaskRefs{}},
	{client.TaskClosure{}, store.TaskClosure{}},
	{client.DecisionRequest{}, store.DecisionRequest{}},
	{client.DecisionResolution{}, store.DecisionResolution{}},
	{client.TaskComment{}, store.TaskComment{}},
	{client.ProjectOwner{}, store.ProjectOwnerDetail{}},
	{client.Document{}, store.Document{}},
	{client.Attachment{}, store.Attachment{}},
	// One input type serves both decision routes: POST
	// /v1/tasks/{id}/decision-request and .../decision-request/ask each
	// decode store.DecisionRequestInput.
	{client.DecisionAskInput{}, store.DecisionRequestInput{}},
	{client.DecisionOptions{}, store.DecisionOptions{}},
	{client.DecisionOption{}, store.DecisionOption{}},
	{client.DecisionAskResult{}, store.DecisionAskResult{}},
	{client.DecisionRequestResult{}, store.DecisionRequestResult{}},
	{client.SimilarTask{}, store.SimilarTask{}},
}

// jsonFields lists a struct's encoded fields by JSON name, flattening
// untagged embedded structs the way encoding/json does.
func jsonFields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		if f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct {
			for k, v := range jsonFields(f.Type) {
				if _, shadowed := out[k]; !shadowed {
					out[k] = v
				}
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out[name] = f
	}
	return out
}

var timeType = reflect.TypeOf(time.Time{})

// compatible reports why a client field type cannot carry what the store
// field type encodes, or "" when it can: the same kind at every level, the
// same pointer and slice shape, time.Time only against time.Time, and every
// nested struct field present on the store side with the same JSON tag.
func compatible(c, s reflect.Type, path string) []string {
	if c.Kind() != s.Kind() {
		return []string{fmt.Sprintf("%s: client kind %s, store kind %s", path, c.Kind(), s.Kind())}
	}
	switch c.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return compatible(c.Elem(), s.Elem(), path+"[]")
	case reflect.Map:
		return append(compatible(c.Key(), s.Key(), path+"{key}"), compatible(c.Elem(), s.Elem(), path+"{}")...)
	case reflect.Struct:
		if (c == timeType) != (s == timeType) {
			return []string{fmt.Sprintf("%s: client %s, store %s", path, c, s)}
		}
		if c == timeType {
			return nil
		}
		return structCompatible(c, s, path)
	}
	return nil
}

func structCompatible(c, s reflect.Type, path string) []string {
	var bad []string
	sf := jsonFields(s)
	for name, f := range jsonFields(c) {
		g, ok := sf[name]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s.%s: no store field with this JSON name on %s", path, name, s))
			continue
		}
		if f.Tag.Get("json") != g.Tag.Get("json") {
			bad = append(bad, fmt.Sprintf("%s.%s: client tag %q, store tag %q", path, name, f.Tag.Get("json"), g.Tag.Get("json")))
		}
		bad = append(bad, compatible(f.Type, g.Type, path+"."+name)...)
	}
	return bad
}

// Every JSON field of every client wire type exists on the matching store
// type with the same tag and a compatible kind.
func TestClientWireFieldsExistOnStoreTypes(t *testing.T) {
	for _, p := range pairs {
		ct, st := reflect.TypeOf(p.client), reflect.TypeOf(p.store)
		for _, bad := range structCompatible(ct, st, ct.String()) {
			t.Error(bad)
		}
	}
}

// fill sets every settable field of v to a distinct non-zero value, so a
// decoded field that came from the wrong JSON key shows up as a mismatch.
func fill(v reflect.Value, name string, depth int) {
	if depth > 6 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("v-" + name)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(len(name) + depth + 1))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(len(name) + depth + 1))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(len(name)) + 0.5)
	case reflect.Pointer:
		x := reflect.New(v.Type().Elem())
		fill(x.Elem(), name, depth+1)
		v.Set(x)
	case reflect.Slice:
		if v.Type() == reflect.TypeOf(json.RawMessage(nil)) {
			v.SetBytes([]byte(`{"k":"v-` + name + `"}`))
			return
		}
		x := reflect.MakeSlice(v.Type(), 1, 1)
		fill(x.Index(0), name, depth+1)
		v.Set(x)
	case reflect.Map:
		x := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, name+"-key", depth+1)
		fill(e, name, depth+1)
		x.SetMapIndex(k, e)
		v.Set(x)
	case reflect.Struct:
		if v.Type() == timeType {
			v.Set(reflect.ValueOf(time.Date(2026, 11, 8, depth, len(name)%60, 7, 0, time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				fill(v.Field(i), f.Name, depth+1)
			}
		}
	}
}

// subset reports where client JSON c differs from store JSON s: every key the
// client encodes must carry the value the store encoded under that key.
func subset(c, s any, path string) []string {
	switch cv := c.(type) {
	case map[string]any:
		sv, ok := s.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: client object, store %T", path, s)}
		}
		var bad []string
		for k, x := range cv {
			y, ok := sv[k]
			if !ok {
				bad = append(bad, fmt.Sprintf("%s.%s: absent from store JSON", path, k))
				continue
			}
			bad = append(bad, subset(x, y, path+"."+k)...)
		}
		return bad
	case []any:
		sv, ok := s.([]any)
		if !ok || len(sv) != len(cv) {
			return []string{fmt.Sprintf("%s: client %v, store %v", path, c, s)}
		}
		var bad []string
		for i := range cv {
			bad = append(bad, subset(cv[i], sv[i], fmt.Sprintf("%s[%d]", path, i))...)
		}
		return bad
	}
	if !reflect.DeepEqual(c, s) {
		return []string{fmt.Sprintf("%s: client %v, store %v", path, c, s)}
	}
	return nil
}

func generic(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A fully populated store value, encoded the way the server answers, decodes
// into the client type with every modelled field carrying the store's value.
func TestServerShapedJSONDecodesIntoClientTypes(t *testing.T) {
	for _, p := range pairs {
		st := reflect.New(reflect.TypeOf(p.store))
		fill(st.Elem(), "root", 0)
		wire, err := json.Marshal(st.Interface())
		if err != nil {
			t.Fatalf("%T: %v", p.store, err)
		}
		ct := reflect.New(reflect.TypeOf(p.client))
		if err := json.Unmarshal(wire, ct.Interface()); err != nil {
			t.Fatalf("%T from %T JSON: %v", p.client, p.store, err)
		}
		if reflect.DeepEqual(ct.Elem().Interface(), reflect.Zero(ct.Elem().Type()).Interface()) {
			t.Fatalf("%T decoded to its zero value", p.client)
		}
		for _, bad := range subset(generic(t, ct.Interface()), generic(t, st.Interface()), reflect.TypeOf(p.client).String()) {
			t.Error(bad)
		}
	}

	// The fields the fleet branches on, named explicitly.
	var s store.Task
	fill(reflect.ValueOf(&s).Elem(), "task", 0)
	wire, _ := json.Marshal(s)
	var c client.Task
	if err := json.Unmarshal(wire, &c); err != nil {
		t.Fatal(err)
	}
	if c.ID != s.ID || c.Lane != s.Lane || c.State != s.State || c.ClaimedBy != s.ClaimedBy || c.Refs.JobID != s.Refs.JobID ||
		c.Refs.PR != s.Refs.PR || c.Refs.Closure.Type != s.Refs.Closure.Type || c.AwaitingDecision != s.AwaitingDecision ||
		c.Refs.DecisionRequest.ID != s.Refs.DecisionRequest.ID || c.Refs.DecisionRequest.HumanOnly != s.Refs.DecisionRequest.HumanOnly ||
		!c.Refs.DecisionRequest.DueAt.Equal(*s.Refs.DecisionRequest.DueAt) ||
		c.Refs.DecisionRequest.Resolution.Option != s.Refs.DecisionRequest.Resolution.Option || !c.UpdatedAt.Equal(s.UpdatedAt) {
		t.Fatalf("client task %+v does not carry the store task's values", c)
	}
}

// The bodies the client sends decode on the server's strict decoder (unknown
// fields refused) into the store type the handler reads, with every value.
func TestClientRequestBodiesDecodeStrictlyIntoStoreTypes(t *testing.T) {
	for _, p := range []struct {
		client, store any
	}{
		{&client.DecisionAskInput{}, &store.DecisionRequestInput{}},
		{&client.Document{}, &store.Document{}},
	} {
		fill(reflect.ValueOf(p.client).Elem(), "body", 0)
		wire, err := json.Marshal(p.client)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(wire))
		dec.DisallowUnknownFields()
		if err := dec.Decode(p.store); err != nil {
			t.Fatalf("%T body refused as %T: %v", p.client, p.store, err)
		}
		for _, bad := range subset(generic(t, p.client), generic(t, p.store), fmt.Sprintf("%T", p.client)) {
			t.Error(bad)
		}
	}
}

// A 409 similar_tasks refusal reads the same through the client's HTTPError
// as the store's own error text.
func TestSimilarTasksErrorTextMatchesStore(t *testing.T) {
	xs := []store.SimilarTask{{ID: 4, Title: "x again", State: "backlog", Lane: "l", Similarity: 0.8}, {ID: 5, Title: `q"uote`, State: "developing"}}
	cs := make([]client.SimilarTask, len(xs))
	for i, x := range xs {
		cs[i] = client.SimilarTask(x)
	}
	want := (&store.SimilarTasksError{Tasks: xs}).Error()
	if got := (&client.HTTPError{Status: 409, Code: "similar_tasks", SimilarTasks: cs}).Error(); got != want {
		t.Fatalf("client text %q, store text %q", got, want)
	}
}
