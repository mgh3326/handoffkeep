package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Unit is where a SQL string sits: a file and the function (or package-level
// var or const) that holds it. It is the unit the schema document names.
type Unit struct {
	File string
	Func string
}

func (u Unit) String() string { return u.File + ":" + u.Func }

// Access is one table reference found in one SQL statement.
type Access struct {
	Table string
	Kind  string // read, write or delete
	Unit  Unit
	Stmt  string // the whole statement, whitespace collapsed; kept for deletes
}

// TableUse is everything the scanner derived about one table.
type TableUse struct {
	Reads   map[Unit]bool
	Writes  map[Unit]bool
	Deletes map[Unit][]string
	Joins   map[string]bool
}

// Usage is the scan result for one source tree.
type Usage struct {
	Tables map[string]bool // the tables of the migrated schema: every viewpoint's table
	Use    map[string]*TableUse
	// Triggers are the migrated schema's triggers (hk 6258). Nil until
	// attachTriggersFile reads the live schema; Scan does not derive them.
	Triggers []triggerUse
}

var (
	// refRe finds a table after a keyword that names it. Keywords are upper
	// case: every production statement in this repo writes SQL that way, and
	// lower-case text in Go is prose, not SQL.
	refRe = regexp.MustCompile("\\b(DELETE FROM|INSERT INTO|REPLACE INTO|TRUNCATE TABLE|UPDATE|FROM|JOIN|INTO)\\s+[`\"]?(?:[a-z_][a-z0-9_]*\\.)?([a-z_][a-z0-9_]*)")
)

// Scan walks internal/ and cmd/ under root and derives the usage of each table
// in tables. Test files are skipped, and so are migration bodies (a migration's
// DML backfills a table once); the migration runner is read.
func Scan(root string, tables map[string]bool) (*Usage, error) {
	type unitText struct {
		unit Unit
		text string
	}
	var texts []unitText
	for _, dir := range []string{"internal", "cmd"} {
		if _, err := os.Stat(filepath.Join(root, dir)); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if isMigrationFile(filepath.Base(path)) {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ParseComments)
			if err != nil {
				return err
			}
			for _, ut := range fileSQL(f, rel) {
				texts = append(texts, unitText{ut.unit, ut.text})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	u := &Usage{Tables: tables, Use: map[string]*TableUse{}}
	for _, ut := range texts {
		for _, stmt := range strings.Split(collapse(ut.text), ";") {
			u.add(strings.TrimSpace(stmt), ut.unit)
		}
	}
	return u, nil
}

// migrationRunner are the files that run migrations at start-up; they are code
// that reads and writes tables (schema_migrations), not migration bodies.
var migrationRunner = map[string]bool{"migration.go": true, "migration_ids.go": true, "migration_mysql.go": true}

func isMigrationFile(base string) bool {
	if migrationRunner[base] {
		return false
	}
	return strings.Contains(base, "migration")
}

func (u *Usage) add(stmt string, unit Unit) {
	seen := map[string]bool{}
	var names []string
	for _, m := range refRe.FindAllStringSubmatch(stmt, -1) {
		name := m[2]
		if !u.Tables[name] {
			continue
		}
		kind := "read"
		switch m[1] {
		case "DELETE FROM":
			kind = "delete"
		case "INSERT INTO", "REPLACE INTO", "TRUNCATE TABLE", "UPDATE", "INTO":
			kind = "write"
		}
		use := u.table(name)
		switch kind {
		case "read":
			use.Reads[unit] = true
		case "write":
			use.Writes[unit] = true
		case "delete":
			use.Writes[unit] = true
			use.Deletes[unit] = append(use.Deletes[unit], truncate(stmt, 260))
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, a := range names {
		for _, b := range names {
			if a != b {
				u.table(a).Joins[b] = true
			}
		}
	}
}

func (u *Usage) table(name string) *TableUse {
	t := u.Use[name]
	if t == nil {
		t = &TableUse{Reads: map[Unit]bool{}, Writes: map[Unit]bool{}, Deletes: map[Unit][]string{}, Joins: map[string]bool{}}
		u.Use[name] = t
	}
	return t
}

// fileSQL returns the SQL-bearing strings of one file with the unit each sits
// in. Concatenated literals are joined so a statement split across lines of Go
// code is read whole.
type unitSQL struct {
	unit Unit
	text string
}

func fileSQL(f *ast.File, rel string) []unitSQL {
	var out []unitSQL
	seen := map[token.Pos]bool{}
	for _, decl := range f.Decls {
		var unit Unit
		switch d := decl.(type) {
		case *ast.FuncDecl:
			unit = Unit{File: rel, Func: funcName(d)}
		case *ast.GenDecl:
			vs, ok := firstValueSpec(d)
			if !ok {
				continue
			}
			unit = Unit{File: rel, Func: vs.Names[0].Name}
		default:
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op != token.ADD || seen[x.Pos()] {
					return true
				}
				var parts []string
				flatten(x, &parts, seen)
				out = append(out, unitSQL{unit, strings.Join(parts, " ")})
				return false
			case *ast.BasicLit:
				if x.Kind == token.STRING && !seen[x.Pos()] {
					seen[x.Pos()] = true
					if s, err := strconv.Unquote(x.Value); err == nil {
						out = append(out, unitSQL{unit, s})
					}
				}
			}
			return true
		})
	}
	return out
}

func firstValueSpec(d *ast.GenDecl) (*ast.ValueSpec, bool) {
	if len(d.Specs) == 0 {
		return nil, false
	}
	vs, ok := d.Specs[0].(*ast.ValueSpec)
	if !ok || len(vs.Names) == 0 {
		return nil, false
	}
	return vs, true
}

func flatten(e ast.Expr, parts *[]string, seen map[token.Pos]bool) {
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			seen[x.Pos()] = true
			flatten(x.X, parts, seen)
			flatten(x.Y, parts, seen)
			return
		}
	case *ast.ParenExpr:
		flatten(x.X, parts, seen)
		return
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			seen[x.Pos()] = true
			if s, err := strconv.Unquote(x.Value); err == nil {
				*parts = append(*parts, s)
			}
			return
		}
	}
	*parts = append(*parts, " ")
}

func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	t := d.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + d.Name.Name
	}
	return d.Name.Name
}

var spaceRe = regexp.MustCompile(`\s+`)

func collapse(s string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(strings.ReplaceAll(s, "`", ""), " "))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// sortedUnits returns the units of a set in file, then function order.
func sortedUnits(set map[Unit]bool) []Unit {
	out := make([]Unit, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// groupByFile renders units as "file (Func, Func); file2 (Func)".
func groupByFile(units []Unit) string {
	var files []string
	funcs := map[string][]string{}
	for _, u := range units {
		if _, ok := funcs[u.File]; !ok {
			files = append(files, u.File)
		}
		funcs[u.File] = append(funcs[u.File], u.Func)
	}
	parts := make([]string, 0, len(files))
	for _, f := range files {
		parts = append(parts, f+" ("+strings.Join(funcs[f], ", ")+")")
	}
	return strings.Join(parts, "; ")
}
