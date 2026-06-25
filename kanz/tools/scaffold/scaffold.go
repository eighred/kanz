// Package scaffold generates a new Kanz service skeleton following the EVT-16a
// layout conventions (DEVX-01b): `services/<name>/cmd/<name>/main.go` +
// per-service `internal/config` and `internal/server`, wired with the standard
// observability + probes the other services use. It kills the copy-paste-an-
// existing-service ritual and keeps every new service on-convention from line 1.
package scaffold

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// nameRE constrains a service name to a lowercase DNS-label-ish token: the same
// shape the binary, k8s objects, and the env prefix are derived from.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)

// Data is the template context derived from a service name.
type Data struct {
	Name      string // foo-bar (dir, binary, package main consumes)
	EnvPrefix string // FOO_BAR (env var prefix)
	Title     string // Foo-bar (doc prose)
}

// newData validates name and derives the template fields.
func newData(name string) (Data, error) {
	if !nameRE.MatchString(name) {
		return Data{}, fmt.Errorf("scaffold: invalid service name %q (want lowercase letters, digits, dashes; e.g. risk-engine)", name)
	}
	env := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return Data{Name: name, EnvPrefix: env, Title: strings.ToUpper(name[:1]) + name[1:]}, nil
}

// file is one templated output relative to the service directory.
type file struct {
	rel  string
	tmpl string
}

var files = []file{
	{"cmd/{{.Name}}/main.go", mainTmpl},
	{"internal/config/config.go", configTmpl},
	{"internal/server/server.go", serverTmpl},
	{"README.md", readmeTmpl},
}

// Generate writes a new service skeleton under root/services/<name>/. It refuses
// to overwrite: if any target file already exists, nothing is written and an
// error is returned (a half-scaffolded service is worse than none). Returns the
// created paths, relative to root.
func Generate(root, name string) ([]string, error) {
	data, err := newData(name)
	if err != nil {
		return nil, err
	}
	svcDir := filepath.Join(root, "services", name)

	// Render everything first, and pre-check for collisions, so we don't write a
	// partial tree on the first failure.
	type rendered struct {
		abs, rel string
		content  []byte
	}
	var out []rendered
	for _, f := range files {
		rel := mustRender("path:"+f.rel, f.rel, data)
		abs := filepath.Join(svcDir, rel)
		if _, err := os.Stat(abs); err == nil {
			return nil, fmt.Errorf("scaffold: refusing to overwrite existing file %s", abs)
		}
		content, err := render(f.rel, f.tmpl, data)
		if err != nil {
			return nil, err
		}
		// gofmt the Go outputs — guarantees on-convention formatting and doubles
		// as a syntax check, so a broken template fails generation, not the build.
		if strings.HasSuffix(rel, ".go") {
			formatted, ferr := format.Source(content)
			if ferr != nil {
				return nil, fmt.Errorf("scaffold: generated %s is not valid Go: %w", rel, ferr)
			}
			content = formatted
		}
		out = append(out, rendered{abs: abs, rel: filepath.Join("services", name, rel), content: content})
	}

	var created []string
	for _, r := range out {
		if err := os.MkdirAll(filepath.Dir(r.abs), 0o755); err != nil {
			return created, err
		}
		if err := os.WriteFile(r.abs, r.content, 0o644); err != nil {
			return created, err
		}
		created = append(created, filepath.ToSlash(r.rel))
	}
	return created, nil
}

func render(name, tmpl string, data Data) ([]byte, error) {
	t, err := template.New(name).Parse(tmpl)
	if err != nil {
		return nil, fmt.Errorf("scaffold: parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("scaffold: execute %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

func mustRender(name, tmpl string, data Data) string {
	b, err := render(name, tmpl, data)
	if err != nil {
		panic(err) // path templates are static + trusted
	}
	return string(b)
}
