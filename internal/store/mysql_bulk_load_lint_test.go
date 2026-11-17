package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var mysqlBulkLoadSet = regexp.MustCompile(`(?i)\bset\s+@hk_bulk_load\s*(=|:=)`)

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
		if strings.HasSuffix(path, "_test.go") || strings.Contains(filepath.ToSlash(path), "/cmd/store-copy/") {
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
		if mysqlBulkLoadSet.Match(data) {
			t.Errorf("%s sets @hk_bulk_load outside the copy tool", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
