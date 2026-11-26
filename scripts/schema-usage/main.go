// Command schema-usage builds the MySQL schema document's per-table facts from
// the code (hk 6233). It derives which production code reads, writes and
// deletes each table from the SQL in internal/ and cmd/, so nobody keeps that
// list by hand.
//
//	schema-usage report [LIVE_JSON]
//	    print the derived usage of every table (for review); with LIVE_JSON,
//	    also the triggers of the migrated schema
//	schema-usage config OUT LIVE_JSON
//	    write OUT: .tbls.yml with the derived usage appended to each table's
//	    comment and the role legend appended to desc; exits non-zero when a
//	    table has no hand comment, no role label, or a comment names no table.
//	    LIVE_JSON is tbls's JSON export of the migrated database (tbls out -t
//	    json); the triggers it defines are stated on the tables they write,
//	    delete or guard (hk 6258)
//	schema-usage summary SCHEMA_JSON README
//	    insert the domain summary (counts and a domain-level diagram, real
//	    foreign keys solid, declared relations dashed) after README's title
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "schema-usage:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: schema-usage report [LIVE_JSON] | config OUT LIVE_JSON | summary SCHEMA_JSON README")
	}
	switch args[0] {
	case "report":
		if len(args) > 2 {
			return errors.New("report takes at most LIVE_JSON")
		}
		cfg, err := loadConfig(".tbls.yml")
		if err != nil {
			return err
		}
		tables, err := tableSet(cfg)
		if err != nil {
			return err
		}
		u, err := Scan(".", tables)
		if err != nil {
			return err
		}
		if len(args) == 2 {
			if err := attachTriggersFile(u, args[1]); err != nil {
				return err
			}
		}
		printReport(out, u)
		return nil
	case "config":
		if len(args) != 3 {
			return errors.New("config needs OUT and LIVE_JSON")
		}
		return writeConfig(".tbls.yml", args[1], args[2])
	case "summary":
		if len(args) != 3 {
			return errors.New("summary needs SCHEMA_JSON and README")
		}
		return writeSummary(".tbls.yml", args[1], args[2])
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func printReport(out io.Writer, u *Usage) {
	names := sortedTables(u.Tables)
	for _, name := range names {
		t := u.Use[name]
		fmt.Fprintf(out, "%s\n", name)
		if t == nil {
			fmt.Fprintf(out, "  no code reads or writes it\n")
		} else {
			fmt.Fprintf(out, "  reads:  %s\n", orNone(groupByFile(sortedUnits(t.Reads))))
			fmt.Fprintf(out, "  writes: %s\n", orNone(groupByFile(sortedUnits(t.Writes))))
			for _, unit := range sortedUnits(mapKeysDeletes(t.Deletes)) {
				for _, stmt := range t.Deletes[unit] {
					fmt.Fprintf(out, "  delete: %s: %s\n", unit, stmt)
				}
			}
			fmt.Fprintf(out, "  joins:  %s\n", orNone(strings.Join(sortedKeys(t.Joins), ", ")))
		}
		for _, line := range triggerFactsOf(name, u.Triggers).lines() {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
}

func mapKeysDeletes(m map[Unit][]string) map[Unit]bool {
	set := make(map[Unit]bool, len(m))
	for u := range m {
		set[u] = true
	}
	return set
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func sortedTables(set map[string]bool) []string {
	return sortedKeys(set)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// loadConfig reads .tbls.yml as a generic map. The file stays the source of
// hand text; the generated config is written to a separate path.
func loadConfig(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

func writeConfig(tblsPath, outPath, livePath string) error {
	cfg, err := loadConfig(tblsPath)
	if err != nil {
		return err
	}
	tables, err := tableSet(cfg)
	if err != nil {
		return err
	}
	u, err := Scan(".", tables)
	if err != nil {
		return err
	}
	if err := attachTriggersFile(u, livePath); err != nil {
		return err
	}
	if err := applyUsage(cfg, u); err != nil {
		return err
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, raw, 0o644)
}

// unlistedRemedy is what a refusal for a table outside every viewpoint tells
// the author to do: the table joins a viewpoint and gets a comments entry.
const unlistedRemedy = "add the table to a viewpoint in .tbls.yml and give it a comments: entry (tableComment and one role label)"

// applyUsage checks the hand comments against the tables the code creates and
// appends the derived usage to each table comment.
func applyUsage(cfg map[string]any, u *Usage) error {
	entries := map[string]map[string]any{}
	comments, _ := cfg["comments"].([]any)
	for _, c := range comments {
		m, ok := c.(map[string]any)
		if !ok {
			return errors.New(".tbls.yml comments entry is not a map")
		}
		name, _ := m["table"].(string)
		entries[name] = m
	}

	var problems []string
	for name := range entries {
		if !u.Tables[name] {
			problems = append(problems, fmt.Sprintf("comments names table %q, which no viewpoint lists: %s", name, unlistedRemedy))
		}
	}
	for _, tr := range u.Triggers {
		if !u.Tables[tr.Table] {
			problems = append(problems, fmt.Sprintf("trigger %q fires on table %q, which no viewpoint lists: %s", tr.Name, tr.Table, unlistedRemedy))
		}
	}
	for _, name := range sortedTables(u.Tables) {
		m := entries[name]
		if m == nil {
			problems = append(problems, fmt.Sprintf("table %q has no comments entry in .tbls.yml", name))
			continue
		}
		hand, _ := m["tableComment"].(string)
		if strings.TrimSpace(hand) == "" {
			problems = append(problems, fmt.Sprintf("table %q has no tableComment", name))
		}
		if roleOf(m) == "" {
			problems = append(problems, fmt.Sprintf("table %q needs exactly one role label (one of %s)", name, strings.Join(roleNames(), ", ")))
		}
		derived := derivedLines(u.Use[name], triggerFactsOf(name, u.Triggers))
		m["tableComment"] = strings.TrimSpace(hand) + "\n\n" + strings.Join(derived, "\n")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}

	desc, _ := cfg["desc"].(string)
	cfg["desc"] = strings.TrimSpace(desc) + "\n\n" + roleLegend()
	return nil
}

// derivedLines are the facts the code and the triggers give a table, one line
// each. A table no code touches but a trigger does gets the trigger lines.
func derivedLines(t *TableUse, f triggerFacts) []string {
	if t == nil {
		if f.none() {
			return []string{"Code (derived from internal/ and cmd/, tests and migrations excluded): no production statement reads or writes this table."}
		}
		t = &TableUse{}
	}
	code := groupByFile(sortedUnits(t.Writes))
	lines := []string{"Read by: " + orNone(groupByFile(sortedUnits(t.Reads))) + "."}
	switch {
	case code != "":
		lines = append(lines, "Written by: "+code+".")
	case len(f.Written)+len(f.Deleted) > 0:
		lines = append(lines, "Written by: no production code.")
	default:
		lines = append(lines, "Written by: none.")
	}
	lines = append(lines, f.Written...)
	var dels []string
	for _, unit := range sortedUnits(mapKeysDeletes(t.Deletes)) {
		for _, stmt := range t.Deletes[unit] {
			dels = append(dels, unit.String()+" ["+stmt+"]")
		}
	}
	if len(dels) > 0 {
		lines = append(lines, "Deleted by: "+strings.Join(dels, "; ")+".")
	}
	lines = append(lines, f.Deleted...)
	lines = append(lines, f.Guarded...)
	lines = append(lines, "Joined in the same statement with: "+orNone(strings.Join(sortedKeys(t.Joins), ", "))+".")
	return lines
}

func roleOf(m map[string]any) string {
	labels, _ := m["labels"].([]any)
	var found []string
	for _, l := range labels {
		s, _ := l.(string)
		if roleSet[s] {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

// tableSet is the migrated schema's table list as .tbls.yml states it: the
// union of the viewpoints. tbls lint requires every table in a viewpoint, and
// the summary step checks this list against the schema tbls reads.
func tableSet(cfg map[string]any) (map[string]bool, error) {
	vps, err := viewpointsOf(cfg)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, vp := range vps {
		for _, t := range vp.tables {
			set[t] = true
		}
	}
	if len(set) == 0 {
		return nil, errors.New(".tbls.yml lists no viewpoint tables")
	}
	return set, nil
}
