package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	mysqlBulkLoadSet = regexp.MustCompile(`(?i)@hk_bulk_load\s*:?=`)
	goStringConcat   = regexp.MustCompile(`["'\x60]\s*\+\s*["'\x60]`)
)

// Join literal fragments before looking for assignments as well as direct SQL.
func setsMySQLBulkLoad(data []byte) bool {
	return mysqlBulkLoadSet.Match(goStringConcat.ReplaceAll(data, nil))
}

func TestMySQLBulkLoadSetterSpellings(t *testing.T) {
	for _, code := range []string{
		`SET @hk_bulk_load=1`,
		`SET @x=0, @hk_bulk_load=1`,
		`SELECT @hk_bulk_load:=1`,
		`"SET @hk_" + "bulk_load=1"`,
	} {
		if !setsMySQLBulkLoad([]byte(code)) {
			t.Errorf("setter was not detected: %s", code)
		}
	}
	if setsMySQLBulkLoad([]byte(`IF COALESCE(@hk_bulk_load,0) <> 1 THEN`)) {
		t.Fatal("guard read was mistaken for a setter")
	}
}

// Only the copy tool may set the bypass in production code. Tests are
// excluded because they deliberately exercise both sides of the guard.
func TestMySQLBulkLoadSetterIsCopyToolOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "static":
				return filepath.SkipDir
			}
			return nil
		}
		normalizedPath := filepath.ToSlash(path)
		// The single binary dispatches store-copy through cmd/handoffkeep;
		// the store package owns its SQL implementation and session setting.
		if strings.HasSuffix(path, "_test.go") ||
			strings.Contains(normalizedPath, "/cmd/store-copy/") ||
			strings.HasSuffix(normalizedPath, "/internal/store/copy.go") {
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".sql", ".sh", ".py":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if setsMySQLBulkLoad(data) {
			t.Errorf("%s sets @hk_bulk_load outside the copy tool", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
