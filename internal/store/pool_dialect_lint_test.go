package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// poolOnlyMethods lists the *Store methods that read the PostgreSQL pool on
// purpose, with the reason no MySQL branch is needed. Every other method that
// reads s.pool must mention the dialect in its body: on a MySQL store the pool
// is nil, so an unbranched read panics (hk 5912, scope "all" and "docs" search).
var poolOnlyMethods = map[string]string{
	"db":                      "returns s.queries, which every MySQL store sets; the pool fallback is PostgreSQL-only",
	"migrate":                 "PostgreSQL migration runner; Open calls it only on the PostgreSQL path",
	"recordSessionDecisionPG": "RecordSessionDecision returns the MySQL twin before calling it",
	"pruneDocumentBatch":      "PruneDocuments returns the MySQL path before its batch loop",
	"pruneRetentionBatchOnce": "PruneRetention returns the MySQL path before its batch loop",
}

// TestPoolUsesAreDialectGuarded is a coarse, untagged check that runs without a
// database. A method passes when its body names the dialect (s.dialect,
// mysqlDialect or mysqlPool). It does not follow control flow: a reviewer still
// reads that the pool read sits in the PostgreSQL branch.
func TestPoolUsesAreDialectGuarded(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, ok := pkgs["store"]
	if !ok {
		t.Fatal("package store not found")
	}
	var unguarded []string
	seen := map[string]bool{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil || !isStoreReceiver(fd.Recv) {
				continue
			}
			name := fd.Name.Name
			usesPool, guarded := false, false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					switch x.Sel.Name {
					case "pool":
						usesPool = true
					case "dialect", "mysqlPool":
						guarded = true
					}
				case *ast.Ident:
					if x.Name == "mysqlDialect" || x.Name == "postgresDialect" {
						guarded = true
					}
				}
				return true
			})
			if !usesPool {
				continue
			}
			seen[name] = true
			if guarded {
				continue
			}
			if _, ok := poolOnlyMethods[name]; ok {
				continue
			}
			unguarded = append(unguarded, name)
		}
	}
	for name := range poolOnlyMethods {
		if !seen[name] {
			t.Errorf("poolOnlyMethods lists %s, which no longer reads s.pool; remove it", name)
		}
	}
	if len(unguarded) > 0 {
		t.Errorf("methods read s.pool with no dialect branch (nil on MySQL): %s", strings.Join(unguarded, ", "))
	}
}

func isStoreReceiver(recv *ast.FieldList) bool {
	if len(recv.List) != 1 {
		return false
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.Name == "Store"
}
