// Package policyindex renders the policy/index document as a tree.
//
// The list (hk document policy/index-list) holds one row per rule area, each
// naming its parent area. Build checks the list and renders the index: a row
// only points to its rule home, and never restates the rule.
package policyindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ListKey and IndexKey are the hk documents: the list is read, the index is
// written.
const (
	ListKey  = "policy/index-list"
	IndexKey = "policy/index"
)

// Row is one area. Parent is the key of its parent row, or "" for a root.
type Row struct {
	Key    string `json:"key"`
	Parent string `json:"parent"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Home   string `json:"home"`
	Editor string `json:"editor"`
}

// List is the body of policy/index-list. Intro and Tail are markdown copied
// into the index before and after the tree.
type List struct {
	Version int    `json:"version"`
	Intro   string `json:"intro"`
	Rows    []Row  `json:"rows"`
	Tail    string `json:"tail"`
}

// Parse reads a list body. Unknown fields, trailing data and any version but
// 1 are refused.
func Parse(body string) (List, error) {
	var l List
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return List{}, fmt.Errorf("policy index list: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return List{}, errors.New("policy index list: trailing data after the object")
	}
	if l.Version != 1 {
		return List{}, fmt.Errorf("policy index list: version %d, want 1", l.Version)
	}
	return l, nil
}

// Validate checks a list: every row has a key, kind, title, home and editor;
// keys are unique; each parent is an existing other row; no parent chain
// loops.
func Validate(l List) error {
	parents := make(map[string]string, len(l.Rows))
	for _, r := range l.Rows {
		if strings.TrimSpace(r.Key) == "" {
			return errors.New("policy index list: a row has an empty key")
		}
		if _, dup := parents[r.Key]; dup {
			return fmt.Errorf("policy index list: duplicate key %q", r.Key)
		}
		for _, f := range []struct{ name, v string }{{"kind", r.Kind}, {"title", r.Title}, {"home", r.Home}, {"editor", r.Editor}} {
			if strings.TrimSpace(f.v) == "" {
				return fmt.Errorf("policy index list: row %q has an empty %s", r.Key, f.name)
			}
		}
		parents[r.Key] = r.Parent
	}
	for _, r := range l.Rows {
		if r.Parent == "" {
			continue
		}
		if r.Parent == r.Key {
			return fmt.Errorf("policy index list: row %q is its own parent", r.Key)
		}
		if _, ok := parents[r.Parent]; !ok {
			return fmt.Errorf("policy index list: row %q names unknown parent %q", r.Key, r.Parent)
		}
	}
	for _, r := range l.Rows {
		cur, steps := r.Key, 0
		for parents[cur] != "" {
			cur = parents[cur]
			steps++
			if steps > len(parents) {
				return fmt.Errorf("policy index list: row %q is in a parent cycle", r.Key)
			}
		}
	}
	return nil
}

// Render writes the index: Intro, an "## Areas" tree (children indented two
// spaces under their parent, in list order), then Tail. The list must be
// valid.
func Render(l List) string {
	children := make(map[string][]Row, len(l.Rows))
	for _, r := range l.Rows {
		children[r.Parent] = append(children[r.Parent], r)
	}
	var b strings.Builder
	b.WriteString(l.Intro)
	b.WriteString("## Areas\n")
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, r := range children[parent] {
			fmt.Fprintf(&b, "%s- `%s` %s | %s | %s | %s\n", strings.Repeat("  ", depth), r.Key, r.Title, r.Kind, r.Home, r.Editor)
			walk(r.Key, depth+1)
		}
	}
	walk("", 0)
	b.WriteString("\n")
	b.WriteString(l.Tail)
	return b.String()
}

// Build parses, validates and renders a list body.
func Build(body string) (string, error) {
	l, err := Parse(body)
	if err != nil {
		return "", err
	}
	if err := Validate(l); err != nil {
		return "", err
	}
	return Render(l), nil
}
