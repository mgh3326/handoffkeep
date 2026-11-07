package parity

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestConsoleWritesCoveredOrExcepted reads the console write routes from the
// source and requires each one in ConsoleWrites with an MCP tool or a reason.
// A new route with neither fails here before it can ship silently.
func TestConsoleWritesCoveredOrExcepted(t *testing.T) {
	discovered := map[string]string{}
	for _, source := range apiWriteSources(t) {
		discovered[source] = "internal/ui/api.go"
	}
	for _, source := range formWriteSources(t) {
		discovered[source] = "internal/ui/ui.go"
	}
	if len(discovered) == 0 {
		t.Fatal("no console write routes found; the route extractor no longer matches internal/ui")
	}

	table := map[string]ConsoleWrite{}
	for _, write := range ConsoleWrites {
		if write.Source == "" || write.Route == "" {
			t.Errorf("parity entry %+v needs both Source and Route", write)
			continue
		}
		if _, seen := table[write.Source]; seen {
			t.Errorf("duplicate parity entry for %s", write.Source)
			continue
		}
		table[write.Source] = write
	}

	tools := registeredMCPTools(t)
	for source, file := range discovered {
		write, ok := table[source]
		if !ok {
			t.Errorf("%s routes %q to a console write with no parity entry: map it to an MCP tool or add a reasoned exception under internal/parity", file, source)
			continue
		}
		switch {
		case len(write.MCP) == 0 && write.Except == "":
			t.Errorf("%s (%s) has neither an MCP tool nor an exception", write.Route, source)
		case len(write.MCP) > 0 && write.Except != "":
			t.Errorf("%s (%s) has both an MCP tool and an exception", write.Route, source)
		case len(write.MCP) == 0 && !strings.HasPrefix(write.Except, "follow-up: hk ") && !strings.HasPrefix(write.Except, "operator-only: "):
			t.Errorf("%s exception must start with \"follow-up: hk \" or \"operator-only: \": %q", write.Route, write.Except)
		}
		for _, tool := range write.MCP {
			if !tools[tool] {
				t.Errorf("%s maps to MCP tool %q, which internal/mcp does not register", write.Route, tool)
			}
		}
	}
	for source, write := range table {
		if _, ok := discovered[source]; !ok {
			t.Errorf("stale parity entry %q (%s): internal/ui no longer routes that console write", source, write.Route)
		}
	}
}

// TestParityDocMentionsEveryConsoleWrite keeps docs/parity.md in step with the
// enforced table: every route must be named in the capability matrix.
func TestParityDocMentionsEveryConsoleWrite(t *testing.T) {
	raw, err := os.ReadFile("../../docs/parity.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, write := range ConsoleWrites {
		if !strings.Contains(doc, write.Route) {
			t.Errorf("docs/parity.md does not mention the console write route %s", write.Route)
		}
	}
}

// TestParityDocMentionsEveryMCPTool keeps the tool appendix complete: a new
// MCP tool must appear in the matrix.
func TestParityDocMentionsEveryMCPTool(t *testing.T) {
	raw, err := os.ReadFile("../../docs/parity.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for tool := range registeredMCPTools(t) {
		if !strings.Contains(doc, tool) {
			t.Errorf("docs/parity.md does not mention MCP tool %q", tool)
		}
	}
}

// apiWriteSources returns the route identity of every write case in the
// serveAPI dispatcher (internal/ui/api.go).
func apiWriteSources(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../ui/api.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == "serveAPI" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("internal/ui/api.go has no serveAPI function")
	}
	var dispatch *ast.SwitchStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if dispatch != nil {
			return false
		}
		if sw, ok := n.(*ast.SwitchStmt); ok && sw.Tag == nil {
			dispatch = sw
			return false
		}
		return true
	})
	if dispatch == nil {
		t.Fatal("serveAPI has no method dispatch switch")
	}

	var sources []string
	for _, stmt := range dispatch.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok || clause.List == nil {
			continue
		}
		for _, expr := range clause.List {
			if !containsWriteMethod(expr) {
				continue
			}
			source, ok := routeSource(expr)
			if !ok {
				t.Errorf("cannot read the route of console write case %q; extend routeSource", exprText(fset, expr))
				continue
			}
			sources = append(sources, source)
		}
	}
	return sources
}

// formWriteSources returns the route identity of every write case in the
// console's HTML form dispatcher (internal/ui/ui.go, ServeHTTP).
func formWriteSources(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../ui/ui.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == "ServeHTTP" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("internal/ui/ui.go has no ServeHTTP method")
	}

	var sources []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || !isMethodCheck(ifs.Cond, "MethodPost") {
			return true
		}
		var dispatch *ast.SwitchStmt
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			if dispatch != nil {
				return false
			}
			if sw, ok := m.(*ast.SwitchStmt); ok {
				dispatch = sw
				return false
			}
			return true
		})
		if dispatch == nil {
			return true
		}
		for _, stmt := range dispatch.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			if clause.List == nil {
				ast.Inspect(clause, func(m ast.Node) bool {
					call, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "pathTaskID" && len(call.Args) > 0 {
						if arg, ok := call.Args[0].(*ast.Ident); ok {
							sources = append(sources, arg.Name)
						}
					}
					return true
				})
				continue
			}
			for _, expr := range clause.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if value, err := strconv.Unquote(lit.Value); err == nil {
						sources = append(sources, value)
					}
				}
			}
		}
		return true
	})
	return sources
}

// registeredMCPTools lists every tool name internal/mcp registers.
func registeredMCPTools(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, name := range []string{"server.go", "operator.go", "meta.go"} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "../mcp/"+name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Tool" {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Name" {
					continue
				}
				if value, ok := kv.Value.(*ast.BasicLit); ok && value.Kind == token.STRING {
					if tool, err := strconv.Unquote(value.Value); err == nil {
						out[tool] = true
					}
				}
			}
			return true
		})
	}
	return out
}

var writeMethods = map[string]bool{
	"MethodPost":   true,
	"MethodPut":    true,
	"MethodPatch":  true,
	"MethodDelete": true,
}

// containsWriteMethod reports whether the case condition checks for a write
// HTTP method.
func containsWriteMethod(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !writeMethods[sel.Sel.Name] {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" {
			found = true
			return false
		}
		return true
	})
	return found
}

// routeSource identifies the route in a write case condition. It accepts the
// shapes the console uses: a path comparison, a *PathRE MatchString call, and
// a strings.HasPrefix call.
func routeSource(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return routeSource(e.X)
	case *ast.UnaryExpr:
		return routeSource(e.X)
	case *ast.BinaryExpr:
		if e.Op == token.LAND || e.Op == token.LOR {
			for _, side := range []ast.Expr{e.X, e.Y} {
				if containsWriteMethod(side) {
					continue
				}
				if source, ok := routeSource(side); ok {
					return source, true
				}
			}
			return "", false
		}
		if e.Op == token.EQL || e.Op == token.NEQ {
			for _, side := range []ast.Expr{e.X, e.Y} {
				switch v := side.(type) {
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						if value, err := strconv.Unquote(v.Value); err == nil {
							return value, true
						}
					}
				case *ast.Ident:
					return v.Name, true
				}
			}
		}
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		if sel.Sel.Name == "MatchString" {
			return recv.Name, true
		}
		if recv.Name == "strings" && sel.Sel.Name == "HasPrefix" && len(e.Args) == 2 {
			if lit, ok := e.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil {
					return "prefix:" + value, true
				}
			}
		}
	}
	return "", false
}

func isMethodCheck(expr ast.Expr, method string) bool {
	be, ok := expr.(*ast.BinaryExpr)
	if !ok || be.Op != token.EQL {
		return false
	}
	return (isRequestMethod(be.X) && isHTTPMethod(be.Y, method)) ||
		(isHTTPMethod(be.X, method) && isRequestMethod(be.Y))
}

func isRequestMethod(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Method" {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	return ok && recv.Name == "r"
}

func isHTTPMethod(expr ast.Expr, method string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

func exprText(fset *token.FileSet, expr ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, expr); err != nil {
		return "<unprintable>"
	}
	return b.String()
}
