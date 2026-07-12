package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// walkSQL visits every .sql file under a tree.
func walkSQL(visit func(path string, body []byte)) fs.WalkDirFunc {
	return func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".sql") {
			return nil //nolint:nilerr // a missing tree is not this test's business
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		visit(path, b)
		return nil
	}
}

// serviceOf names the service a migration belongs to (services/<svc>/migrations/...).
func serviceOf(root, path string) string {
	rel, err := filepath.Rel(filepath.Join(root, "services"), path)
	if err != nil {
		return path
	}
	return strings.Split(filepath.ToSlash(rel), "/")[0]
}
