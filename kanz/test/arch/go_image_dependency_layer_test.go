package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Restrict dependency downloads to module metadata. The full generated SDK
// still enters the compiler layer, so schema changes rebuild every consumer.
// This checks Dockerfile inputs/order; actual image builds remain separate proof.
func checkGoImageDependencyLayer(raw string) error {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	start, download, sdk, source, compile := -1, -1, -1, -1, -1
	const metadata = "COPY kanz-schemas/gen/go/go.mod kanz-schemas/gen/go/go.sum ./kanz-schemas/gen/go/"
	allowed := map[string]bool{metadata: true, "COPY kanz/go.mod kanz/go.sum ./kanz/": true, "COPY kanz/build/go-mod-download.sh /usr/local/bin/go-mod-download.sh": true}
	seen := map[string]int{}
	for i, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "FROM ") && strings.Contains(line, "/golang:") {
			if start >= 0 {
				return fmt.Errorf("multiple Go builders need reviewed dependency ownership")
			}
			start = i
			continue
		}
		if start < 0 || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "FROM ") {
			break
		}
		if line == "RUN sh /usr/local/bin/go-mod-download.sh" {
			if download >= 0 {
				return fmt.Errorf("duplicate dependency download")
			}
			download = i
		}
		if download < 0 && (strings.HasPrefix(line, "COPY ") || strings.HasPrefix(line, "ADD ")) {
			if !allowed[line] {
				return fmt.Errorf("source invalidates metadata-only dependency layer: %s", line)
			}
			seen[line]++
		}
		if line == "COPY kanz-schemas/gen/go/ /src/kanz-schemas/gen/go/" {
			sdk = i
		}
		if line == "COPY kanz/ ./" {
			source = i
		}
		if compile < 0 && strings.HasPrefix(line, "RUN ") && strings.Contains(line, "go build") {
			compile = i
		}
	}
	if start < 0 || download <= start || sdk <= download || source <= download || compile <= sdk || compile <= source {
		return fmt.Errorf("complete SDK and source must follow download and precede compile")
	}
	for line := range allowed {
		if seen[line] != 1 {
			return fmt.Errorf("missing/duplicate dependency metadata: %s", line)
		}
	}
	return nil
}

func TestGoImageDependencyLayersPreserveCompleteSDK(t *testing.T) {
	root := moduleRoot(t)
	count := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".gotmp" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "Dockerfile" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(raw), "FROM ghcr.io/eighred/base/golang:") {
			return nil
		}
		count++
		if err := checkGoImageDependencyLayer(string(raw)); err != nil {
			t.Errorf("%s: %v", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count < 30 {
		t.Fatalf("incomplete Go image inventory: %d", count)
	}
	fixture := read(t, filepath.Join(root, "services/api-gateway/Dockerfile"))
	for _, mutation := range [][2]string{
		{"COPY kanz-schemas/gen/go/go.mod kanz-schemas/gen/go/go.sum ./kanz-schemas/gen/go/", "COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/"},
		{"kanz-schemas/gen/go/go.sum ", ""},
		{"COPY kanz/go.mod kanz/go.sum ./kanz/", "COPY kanz/ ./kanz/"},
		{"COPY kanz-schemas/gen/go/ /src/kanz-schemas/gen/go/", "# no generated source"},
		{"COPY kanz-schemas/gen/go/ /src/kanz-schemas/gen/go/", "COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/"},
		{"RUN sh /usr/local/bin/go-mod-download.sh", "RUN go mod download"},
		{"COPY kanz/build/go-mod-download.sh", "COPY kanz/services/ /src/services/\nCOPY kanz/build/go-mod-download.sh"},
	} {
		changed := strings.Replace(fixture, mutation[0], mutation[1], 1)
		if changed == fixture {
			t.Fatal("mutation did not apply")
		}
		if checkGoImageDependencyLayer(changed) == nil {
			t.Fatalf("accepted broken dependency boundary: %s", mutation[0])
		}
	}
	// Full SDK before download still rebuilds consumers, but destroys reuse.
	before := strings.Replace(fixture, "COPY kanz-schemas/gen/go/ /src/kanz-schemas/gen/go/\n", "", 1)
	before = strings.Replace(before, "RUN sh /usr/local/bin/go-mod-download.sh", "COPY kanz-schemas/gen/go/ /src/kanz-schemas/gen/go/\nRUN sh /usr/local/bin/go-mod-download.sh", 1)
	if checkGoImageDependencyLayer(before) == nil {
		t.Fatal("early SDK source accepted")
	}
	t.Logf("%d complete Go image dependency/compiler boundaries checked", count)
}
