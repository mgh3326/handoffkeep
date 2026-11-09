package client_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/client"
)

// exportedAPI is every exported identifier package client may declare: the
// Client struct, its fleet calls, their wire types and the error helpers a
// caller needs. A method is listed as Recv.Name.
var exportedAPI = []string{
	// The client and its fleet calls.
	"Client",
	"Client.AskDecision",
	"Client.AttachmentURL",
	"Client.BeginAttachmentUpload",
	"Client.CompleteAttachmentUpload",
	"Client.ClaimTask",
	"Client.CommentTask",
	"Client.GetAttachment",
	"Client.GetDocument",
	"Client.GetTask",
	"Client.ListDocuments",
	"Client.ListTasks",
	"Client.ProjectOwner",
	"Client.RecordDecisionRequest",
	"Client.PutAttachment",
	"Client.PutDocument",
	"Client.RebindTaskJob",
	"Client.ReopenTask",
	// Wire types.
	"Attachment",
	"AttachmentUploadTicket",
	"DecisionAskInput",
	"DecisionAskResult",
	"DecisionOption",
	"DecisionOptions",
	"DecisionRequest",
	"DecisionRequestResult",
	"DecisionResolution",
	"Document",
	"ListTasksInput",
	"ProjectOwner",
	"SimilarTask",
	"Task",
	"TaskClosure",
	"TaskComment",
	"TaskRefs",
	// Error helpers and the User-Agent.
	"ErrTooManyRedirects",
	"ErrorCode",
	"HTTPError",
	"SecretPattern",
	"UserAgent",
}

const surfaceWhy = "package client offers fleet calls only. A raw request primitive " +
	"(a request builder, a sender, a raw *http.Client, a Call that takes a path) lets an " +
	"importer aim a request that carries the hk bearer token and the CF Access pair at " +
	"another origin, so raw requests stay in internal/hktransport, out of reach of other " +
	"modules (hk 3544). Add a fleet call instead, and list it here only if it is one"

// The exported surface of package client is closed: a new exported name, a
// re-exported transport primitive or a method promoted from an embedded type
// fails this test.
func TestClientExportsOnlyFleetCalls(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, ok := pkgs["client"]
	if !ok || len(pkgs) != 1 {
		t.Fatalf("parsed packages %v, want only client", pkgs)
	}
	var got []string
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					got = append(got, d.Name.Name)
					continue
				}
				// A method on an unexported type is reachable only through an
				// exported name, which this list already covers.
				if recv := receiverName(d.Recv.List[0].Type); ast.IsExported(recv) {
					got = append(got, recv+"."+d.Name.Name)
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							got = append(got, s.Name.Name)
						}
						if st, ok := s.Type.(*ast.StructType); ok {
							for _, fld := range st.Fields.List {
								if len(fld.Names) == 0 {
									t.Errorf("type %s embeds a field (%s), promoting its methods into package client: %s", s.Name.Name, fset.Position(fld.Type.Pos()), surfaceWhy)
								}
							}
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								got = append(got, n.Name)
							}
						}
					}
				}
			}
		}
	}
	want := append([]string(nil), exportedAPI...)
	sort.Strings(got)
	sort.Strings(want)
	if extra, missing := diff(got, want), diff(want, got); len(extra) > 0 || len(missing) > 0 {
		t.Fatalf("exported API of package client changed.\nnot allowed: %v\nmissing: %v\n%s", extra, missing, surfaceWhy)
	}

	// The method sets as the compiler sees them, including any promoted
	// method, are exactly the listed fleet calls.
	var calls []string
	for _, n := range exportedAPI {
		if m, ok := strings.CutPrefix(n, "Client."); ok {
			calls = append(calls, m)
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(client.Client{}), reflect.TypeOf(&client.Client{})} {
		var methods []string
		for i := 0; i < typ.NumMethod(); i++ {
			methods = append(methods, typ.Method(i).Name)
		}
		sort.Strings(methods)
		sort.Strings(calls)
		if extra, missing := diff(methods, calls), diff(calls, methods); len(extra) > 0 || len(missing) > 0 {
			t.Fatalf("method set of %v changed.\nnot allowed: %v\nmissing: %v\n%s", typ, extra, missing, surfaceWhy)
		}
	}
}

func receiverName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return receiverName(e.X)
	case *ast.IndexListExpr:
		return receiverName(e.X)
	}
	return "?"
}

// diff returns the elements of a not in b.
func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}
