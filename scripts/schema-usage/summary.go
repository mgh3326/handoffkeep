package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// schemaJSON is the part of tbls's schema.json (tbls out -t json) the summary
// reads: the migrated tables and every relation, real or declared.
type schemaJSON struct {
	Tables []struct {
		Name string `json:"name"`
	} `json:"tables"`
	Relations []struct {
		Table       string `json:"table"`
		ParentTable string `json:"parent_table"`
		Virtual     bool   `json:"virtual"`
	} `json:"relations"`
}

type viewpoint struct {
	id, name string
	tables   []string
}

func viewpointsOf(cfg map[string]any) ([]viewpoint, error) {
	raw, _ := cfg["viewpoints"].([]any)
	var out []viewpoint
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, errors.New("viewpoints entry is not a map")
		}
		vp := viewpoint{}
		vp.id, _ = m["id"].(string)
		vp.name, _ = m["name"].(string)
		tabs, _ := m["tables"].([]any)
		for _, t := range tabs {
			s, _ := t.(string)
			vp.tables = append(vp.tables, s)
		}
		out = append(out, vp)
	}
	return out, nil
}

// writeSummary inserts the domain summary after the title of the tbls README.
// Counts and edges come from the migrated schema (schema.json), so a foreign
// key is drawn solid and a relation declared in .tbls.yml dashed.
func writeSummary(tblsPath, schemaPath, readmePath string) error {
	cfg, err := loadConfig(tblsPath)
	if err != nil {
		return err
	}
	vps, err := viewpointsOf(cfg)
	if err != nil {
		return err
	}
	rawSchema, err := os.ReadFile(schemaPath)
	if err != nil {
		return err
	}
	var s schemaJSON
	if err := json.Unmarshal(rawSchema, &s); err != nil {
		return fmt.Errorf("parse %s: %w", schemaPath, err)
	}
	section, err := domainSummary(vps, s)
	if err != nil {
		return err
	}
	readme, err := os.ReadFile(readmePath)
	if err != nil {
		return err
	}
	return os.WriteFile(readmePath, []byte(insertAfterTitle(string(readme), section)), 0o644)
}

// domainSummary renders the domain table and the domain-level diagram. Each
// table counts once, in its primary domain: the first viewpoint that lists it.
func domainSummary(vps []viewpoint, s schemaJSON) (string, error) {
	tableSet := map[string]bool{}
	for _, t := range s.Tables {
		tableSet[t.Name] = true
	}
	primary := map[string]string{}
	for _, vp := range vps {
		for _, t := range vp.tables {
			if _, ok := primary[t]; !ok {
				primary[t] = vp.id
			}
		}
	}
	var unlisted []string
	for t := range tableSet {
		if _, ok := primary[t]; !ok {
			unlisted = append(unlisted, t)
		}
	}
	if len(unlisted) > 0 {
		sort.Strings(unlisted)
		return "", fmt.Errorf("tables in no viewpoint: %s", strings.Join(unlisted, ", "))
	}

	type count struct{ tables, inside, out, in int }
	counts := map[string]*count{}
	for _, vp := range vps {
		counts[vp.id] = &count{}
	}
	for t := range tableSet {
		counts[primary[t]].tables++
	}

	type edge struct {
		from, to string
		declared bool
	}
	edges := map[edge]int{}
	foreignKeys, declared := 0, 0
	for _, r := range s.Relations {
		if r.Virtual {
			declared++
		} else {
			foreignKeys++
		}
		a, b := primary[r.Table], primary[r.ParentTable]
		if a == "" || b == "" {
			continue
		}
		if a == b {
			counts[a].inside++
			continue
		}
		counts[a].out++
		counts[b].in++
		edges[edge{a, b, r.Virtual}]++
	}

	name := map[string]string{}
	for _, vp := range vps {
		name[vp.id] = vp.name
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Domains\n\n")
	fmt.Fprintf(&b, "The document is split into %d domains, the viewpoints in `.tbls.yml`. A table counts once, in its primary domain (the first viewpoint that lists it); a hub table another domain also lists appears on that domain's page too. The migrated schema has %d tables and %d relations: %d foreign keys (solid) and %d declared in `.tbls.yml` (dashed; the code joins on them, the database has no constraint).\n\n", len(vps), len(tableSet), foreignKeys+declared, foreignKeys, declared)
	fmt.Fprintf(&b, "| Domain | Tables | Relations inside | Out to other domains | In from other domains |\n|---|---:|---:|---:|---:|\n")
	total := 0
	for _, vp := range vps {
		c := counts[vp.id]
		total += c.tables
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d |\n", vp.name, c.tables, c.inside, c.out, c.in)
	}
	fmt.Fprintf(&b, "| **All** | **%d** | | | |\n\n", total)

	fmt.Fprintf(&b, "```mermaid\nflowchart LR\n")
	for _, vp := range vps {
		fmt.Fprintf(&b, "  %s[\"%s\"]\n", vp.id, strings.ReplaceAll(vp.name, "\"", "'"))
	}
	keys := make([]edge, 0, len(edges))
	for e := range edges {
		keys = append(keys, e)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].from != keys[j].from {
			return keys[i].from < keys[j].from
		}
		if keys[i].to != keys[j].to {
			return keys[i].to < keys[j].to
		}
		return !keys[i].declared && keys[j].declared
	})
	for _, e := range keys {
		if e.declared {
			fmt.Fprintf(&b, "  %s -.->|\"%d declared\"| %s\n", e.from, edges[e], e.to)
		} else {
			fmt.Fprintf(&b, "  %s -->|\"%d %s\"| %s\n", e.from, edges[e], plural(edges[e], "foreign key", "foreign keys"), e.to)
		}
	}
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Solid arrows are foreign keys in the database; dashed arrows are relations declared in `.tbls.yml`. An arrow runs from the domain of a table to the domain of the table it refers to.\n\n")
	return b.String(), nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// insertAfterTitle puts section after the first markdown title line, or at the
// top when the document has none.
func insertAfterTitle(doc, section string) string {
	lines := strings.SplitAfter(doc, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "# ") {
			rest := strings.Join(lines[i+1:], "")
			return strings.Join(lines[:i+1], "") + "\n" + section + strings.TrimLeft(rest, "\n")
		}
	}
	return section + doc
}
