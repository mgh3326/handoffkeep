package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/capabilities"
)

// featureConstants reads the Feature constants from the capabilities source,
// so a new constant that is missing from Features fails here.
func featureConstants(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../capabilities/capabilities.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Feature") {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, v)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestCapabilitiesListsEveryFeatureConstant(t *testing.T) {
	h := httptest.NewServer(Server{Tokens: Tokens{"tester": "tok"}}.Handler())
	defer h.Close()
	req, err := http.NewRequest(http.MethodGet, h.URL+"/v1/capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var got struct {
		ProtocolVersion int      `json:"protocol_version"`
		Features        []string `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != capabilities.ProtocolVersion {
		t.Fatalf("protocol_version=%d, want %d", got.ProtocolVersion, capabilities.ProtocolVersion)
	}
	listed := append([]string(nil), got.Features...)
	sort.Strings(listed)
	want := featureConstants(t)
	if len(want) == 0 {
		t.Fatal("no Feature constants found in source")
	}
	if strings.Join(listed, ",") != strings.Join(want, ",") {
		t.Fatalf("features=%v, want every Feature constant %v", listed, want)
	}
}

func TestCapabilitiesRequiresBearer(t *testing.T) {
	h := httptest.NewServer(Server{Tokens: Tokens{"tester": "tok"}}.Handler())
	defer h.Close()
	resp, err := h.Client().Get(h.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
}
