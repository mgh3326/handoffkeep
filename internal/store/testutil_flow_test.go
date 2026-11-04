package store

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readRepoFile(rel string) ([]byte, error) { return os.ReadFile(filepath.Join("..", "..", rel)) }
func itoaForTest(n int) string                { return strconv.Itoa(n) }
func contains(s, sub string) bool             { return strings.Contains(s, sub) }
