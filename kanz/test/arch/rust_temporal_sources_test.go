package arch

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func checkedApacheTemporalSources(root string, prepared bool) error {
	base := filepath.Join(root, "temporal_sources")
	raw, err := os.ReadFile(filepath.Join(base, "manifest.json"))
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != "fbfa9668449d7323e889e4aa7c153999aa13490bc6a8ac59d8d3ed5dcf82c059" {
		return fmt.Errorf("Apache source recipe identity differs")
	}
	var manifest struct {
		Packages []struct {
			Name, Version, PatchSHA256, TreeSHA256 string
		}
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	for _, pkg := range manifest.Packages {
		key := pkg.Name + "-" + pkg.Version
		patch, err := os.ReadFile(filepath.Join(base, "patches", key+".patch"))
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(patch)) != pkg.PatchSHA256 {
			return fmt.Errorf("Apache patch differs for %s", key)
		}
		if prepared {
			checksum, err := apacheSourceTree(filepath.Join(base, "generated", key))
			if err != nil {
				return err
			}
			if checksum != pkg.TreeSHA256 {
				return fmt.Errorf("generated Apache source tree differs for %s", key)
			}
		}
	}
	if prepared {
		entries, err := os.ReadDir(filepath.Join(base, "generated"))
		if err != nil || len(entries) != len(manifest.Packages) {
			return fmt.Errorf("generated Apache package set differs")
		}
	}
	return nil
}

func apacheSourceTree(root string) (string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("special file in generated Apache source: %s", path)
		}
		name, err := filepath.Rel(root, path)
		if err == nil {
			paths = append(paths, filepath.ToSlash(name))
		}
		return err
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	result := sha256.New()
	for _, path := range paths {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(raw)
		result.Write([]byte(path + "\x00"))
		result.Write(sum[:])
	}
	return fmt.Sprintf("%x", result.Sum(nil)), nil
}

func TestRustApacheTemporalInputs(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "rust")
	if err := checkedApacheTemporalSources(root, false); err != nil {
		t.Fatal(err)
	}
}
