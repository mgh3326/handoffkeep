package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Triggers (hk 6258). A table that only a database trigger writes has no SQL
// in internal/ or cmd/, so the code scan cannot see its writer. The migrated
// database can: tbls exports each trigger's definition (tbls out -t json). This
// file reads those definitions and states, per table, which triggers write,
// delete and guard it. Nothing here is written by hand.

// triggerUse is one trigger of the migrated schema and what its body does.
type triggerUse struct {
	Name    string   // the trigger's name
	Table   string   // the table it fires on
	On      string   // timing and event on that table: "AFTER UPDATE ON changes"
	Writes  []string // tables its body inserts into or updates
	Deletes []string // tables its body deletes from
	Guard   bool     // its body SIGNALs: it refuses the statement that fired it
}

var (
	// triggerHeadRe reads a definition as tbls writes it for MySQL:
	// "CREATE TRIGGER name AFTER UPDATE ON changes", "FOR EACH ROW", then the body.
	triggerHeadRe = regexp.MustCompile(`(?is)^\s*CREATE\s+TRIGGER\s+\S+\s+((?:BEFORE|AFTER)\s+(?:INSERT|UPDATE|DELETE))\s+ON\s+\S+\s+FOR\s+EACH\s+ROW\s+(.*)$`)
	// triggerWriteRe finds the tables a trigger body writes. Unlike refRe it
	// ignores keyword case: a stored body is SQL, not Go prose.
	triggerWriteRe = regexp.MustCompile("(?i)\\b(INSERT\\s+INTO|REPLACE\\s+INTO|UPDATE|DELETE\\s+FROM)\\s+[`\"]?([a-z_][a-z0-9_]*)")
	signalRe       = regexp.MustCompile(`(?i)\bSIGNAL\b`)
)

// triggersFromLive reads the triggers of tbls's JSON export of the migrated
// database, ordered by the table they fire on, then by name.
func triggersFromLive(raw []byte) ([]triggerUse, error) {
	var live struct {
		Tables []struct {
			Name     string `json:"name"`
			Triggers []struct {
				Name string `json:"name"`
				Def  string `json:"def"`
			} `json:"triggers"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(raw, &live); err != nil {
		return nil, fmt.Errorf("parse the live schema: %w", err)
	}
	var out []triggerUse
	for _, t := range live.Tables {
		for _, tr := range t.Triggers {
			use, err := parseTrigger(t.Name, tr.Name, tr.Def)
			if err != nil {
				return nil, err
			}
			out = append(out, use)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// parseTrigger reads one trigger definition. A definition that is not the
// header tbls writes is an error: a silently missed trigger is the bug this
// file exists to end.
func parseTrigger(table, name, def string) (triggerUse, error) {
	m := triggerHeadRe.FindStringSubmatch(def)
	if m == nil {
		return triggerUse{}, fmt.Errorf("trigger %s on %s: definition is not CREATE TRIGGER ... FOR EACH ROW", name, table)
	}
	use := triggerUse{
		Name:  name,
		Table: table,
		On:    strings.Join(strings.Fields(m[1]), " ") + " ON " + table,
		Guard: signalRe.MatchString(m[2]),
	}
	writes, deletes := map[string]bool{}, map[string]bool{}
	for _, w := range triggerWriteRe.FindAllStringSubmatch(m[2], -1) {
		target := strings.ToLower(w[2])
		if strings.EqualFold(strings.Join(strings.Fields(w[1]), " "), "DELETE FROM") {
			deletes[target] = true
		} else {
			writes[target] = true
		}
	}
	use.Writes = sortedKeys(writes)
	use.Deletes = sortedKeys(deletes)
	return use, nil
}

// attachTriggersFile reads the live schema at path into u.
func attachTriggersFile(u *Usage, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ts, err := triggersFromLive(raw)
	if err != nil {
		return err
	}
	u.Triggers = ts
	return nil
}

// triggerFacts are the trigger lines of one table, in the document's words.
type triggerFacts struct {
	Written []string
	Deleted []string
	Guarded []string
}

// triggerFactsOf states the triggers that write, delete or guard table.
func triggerFactsOf(table string, all []triggerUse) triggerFacts {
	var f triggerFacts
	for _, tr := range all {
		if slices.Contains(tr.Writes, table) {
			f.Written = append(f.Written, "Written by trigger: "+tr.Name+" ("+tr.On+").")
		}
		if slices.Contains(tr.Deletes, table) {
			f.Deleted = append(f.Deleted, "Deleted by trigger: "+tr.Name+" ("+tr.On+").")
		}
		if tr.Guard && tr.Table == table {
			f.Guarded = append(f.Guarded, "Guarded by trigger: "+tr.Name+" ("+tr.On+": refuses).")
		}
	}
	return f
}

// lines is every trigger line, writers first, for the report.
func (f triggerFacts) lines() []string {
	out := append([]string{}, f.Written...)
	out = append(out, f.Deleted...)
	return append(out, f.Guarded...)
}

func (f triggerFacts) none() bool {
	return len(f.Written)+len(f.Deleted)+len(f.Guarded) == 0
}
