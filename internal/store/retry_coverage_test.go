package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// retryHelperFiles holds the transaction machinery itself: the dialect-neutral
// begin, the retrying helpers and the driver adapters. A transaction begin in
// these files is the helper, never a call site.
var retryHelperFiles = map[string]bool{
	"tx_helpers.go":        true,
	"mysql_transaction.go": true,
	"dialect.go":           true,
}

// retryExemptOpens lists functions that may open a transaction outside the
// retrying helpers, with the reason. Read-only transactions are out of scope
// for deadlock retries; the copy tool and DDL cannot run inside retryTx.
// Every other write transaction must run through retryTx (or
// retryPinnedTransaction on a session-locked connection), so that a deadlock
// (1213) or lock-wait timeout (1205) retries the whole transaction once.
var retryExemptOpens = map[string]string{
	// Read-only transactions.
	"Store.GetChangeState":          "REPEATABLE READ read-only snapshot",
	"Store.ListTaskComments":        "read-only comments read",
	"Store.beginSnapshotA5":         "read-only area snapshot helper",
	"Store.beginReportRead":         "read-only report helper",
	"Store.listTaskOrigins":         "read-only origins read",
	"Store.checkNewTaskAttachments": "read-only attachment check, PostgreSQL-only caller",
	"Store.DeployLagStatuses":       "read-only lag read",
	"Store.deployLagStatusesMySQL":  "read-only lag read, MySQL branch",
	"Store.RunDriftDelivery":        "operation-lock transaction spans independently retried idempotent delivery writes",
	// Not retryable in place.
	"Store.migrate":     "DDL statements are not transactional on MySQL; never retried",
	"CopyStore":         "offline bulk-load driver; per-table transactions below",
	"beginCopySnapshot": "read-only source snapshot for the copy tool",
	"copyRows":          "copy tool per-table bulk insert: the source rows stream and are not replayable; the tool verifies each table with Match",
}

// TestWriteTransactionsRunThroughRetryTx fails when a write transaction
// begins outside the store's retrying helpers. Every function that opens a
// transaction must either live in a helper file, run under retryTx (directly
// or through a caller that does), or be listed in retryExemptOpens.
func TestWriteTransactionsRunThroughRetryTx(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(thisFile)
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	type funcInfo struct {
		file     string
		callees  map[string]bool
		closure  map[string]bool
		root     bool
		pinned   bool
		opens    []string
		directRt bool
	}
	funcs := map[string]*funcInfo{}
	get := func(name string) *funcInfo {
		fi, ok := funcs[name]
		if !ok {
			fi = &funcInfo{callees: map[string]bool{}, closure: map[string]bool{}}
			funcs[name] = fi
		}
		return fi
	}
	isRecv := func(id *ast.Ident) bool { return len(id.Name) == 1 }
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Base(path)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				name = "Store." + name
			}
			fi := get(name)
			fi.file = base
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if idfun, ok := call.Fun.(*ast.Ident); ok {
					fi.callees[idfun.Name] = true
					if idfun.Name == "retryPinnedTransaction" {
						fi.pinned = true
					}
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				callee := sel.Sel.Name
				if id, ok := sel.X.(*ast.Ident); ok && isRecv(id) {
					fi.callees["Store."+callee] = true
				}
				switch callee {
				case "retryTx", "mysqlWriteTransaction", "mysqlWriteTransactionWithLock":
					// The session-lock helpers retry the whole transaction like
					// retryTx (through the one retryTransaction predicate), so a
					// function that calls them runs under a retry helper.
					fi.root = true
					for _, arg := range call.Args {
						lit, ok := arg.(*ast.FuncLit)
						if !ok {
							continue
						}
						ast.Inspect(lit.Body, func(m ast.Node) bool {
							cc, ok := m.(*ast.CallExpr)
							if !ok {
								return true
							}
							if idf, ok := cc.Fun.(*ast.Ident); ok {
								fi.closure[idf.Name] = true
								return true
							}
							ss, ok := cc.Fun.(*ast.SelectorExpr)
							if !ok {
								return true
							}
							if id, ok := ss.X.(*ast.Ident); ok && isRecv(id) {
								fi.closure["Store."+ss.Sel.Name] = true
							}
							return true
						})
					}
				case "retryPinnedTransaction":
					fi.pinned = true
				case "retryTransaction":
					fi.directRt = true
				case "Begin", "BeginTx", "begin":
					pos := fset.Position(call.Pos())
					fi.opens = append(fi.opens, fmt.Sprintf("%s:%d", base, pos.Line))
				}
				return true
			})
		}
	}
	// retryPinnedTransaction runs its own attempt loop through the one retry
	// decision point, so its users count as retry roots.
	for _, fi := range funcs {
		if fi.pinned {
			fi.root = true
		}
	}
	covered := map[string]bool{}
	var queue []string
	for name, fi := range funcs {
		if fi.root {
			covered[name] = true
			queue = append(queue, name)
		}
		for c := range fi.closure {
			if !covered[c] {
				covered[c] = true
				queue = append(queue, c)
			}
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		fi, ok := funcs[n]
		if !ok {
			continue
		}
		for _, set := range []map[string]bool{fi.callees, fi.closure} {
			for c := range set {
				if !covered[c] {
					covered[c] = true
					queue = append(queue, c)
				}
			}
		}
	}
	// No nested retries: a second 1213 after one retry must be an error,
	// never a second retry of an outer transaction.
	for name, fi := range funcs {
		if !fi.root {
			continue
		}
		seen := map[string]bool{name: true}
		stack := []string{}
		for c := range fi.closure {
			stack = append(stack, c)
		}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[n] {
				continue
			}
			seen[n] = true
			f2, ok := funcs[n]
			if !ok {
				continue
			}
			if f2.root {
				t.Errorf("nested retry: %s reaches retry root %s; a persistent 1213 would retry more than once", name, n)
			}
			for c := range f2.callees {
				stack = append(stack, c)
			}
			for c := range f2.closure {
				stack = append(stack, c)
			}
		}
	}
	// The retry decision lives in exactly one predicate: only the two
	// retrying helpers may consult retryTransaction directly.
	for name, fi := range funcs {
		if fi.directRt && name != "Store.retryTx" && name != "Store.retryPinnedTransaction" {
			t.Errorf("%s calls retryTransaction directly; route it through retryTx or retryPinnedTransaction", name)
		}
	}
	callers := map[string]map[string]bool{}
	for name, fi := range funcs {
		for c := range fi.callees {
			if callers[c] == nil {
				callers[c] = map[string]bool{}
			}
			callers[c][name] = true
		}
		for c := range fi.closure {
			if callers[c] == nil {
				callers[c] = map[string]bool{}
			}
			callers[c][name] = true
		}
	}
	for name, fi := range funcs {
		if len(fi.opens) == 0 || retryHelperFiles[fi.file] {
			continue
		}
		if reason, exempt := retryExemptOpens[name]; exempt {
			if covered[name] {
				t.Errorf("%s is listed in retryExemptOpens (%s) but now runs under a retry helper; remove it from the exemption", name, reason)
			}
			continue
		}
		if !covered[name] {
			t.Errorf("%s opens a transaction outside the retrying helpers at %v; wrap it in retryTx (or retryPinnedTransaction on a pinned connection)", name, fi.opens)
			continue
		}
		if !fi.root {
			for caller := range callers[name] {
				if !covered[caller] {
					t.Errorf("%s opens a transaction and is called from %s outside any retry helper; wrap the entry point in retryTx", name, caller)
				}
			}
			if len(callers[name]) == 0 {
				t.Errorf("%s opens a transaction with no retry helper on its path; wrap it in retryTx", name)
			}
		}
	}
}
