package arch

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const goSharedImport = "type=gha,scope=${{ matrix.service }}\n${{ startsWith(matrix.dockerfile, 'kanz/') && matrix.service != 'cloudflared' && matrix.service != 'prometheus' && 'type=gha,scope=accounting' || '' }}"
const goOwnExport = "${{ github.event_name == 'push' && github.ref == 'refs/heads/main' && format('type=gha,scope={0},mode=max', matrix.service) || '' }}"

func checkGoSharedCache(raw string, files map[string]string) error {
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			If          string            `yaml:"if"`
			CacheMode   string            `yaml:"cache-mode"`
			Continue    any               `yaml:"continue-on-error"`
			Permissions map[string]string `yaml:"permissions"`
			Strategy    struct {
				Matrix struct {
					Include []struct {
						Service    string `yaml:"service"`
						Dockerfile string `yaml:"dockerfile"`
						CacheProbe bool   `yaml:"cache_probe"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				ID       string            `yaml:"id"`
				Name     string            `yaml:"name"`
				Run      string            `yaml:"run"`
				If       string            `yaml:"if"`
				Continue any               `yaml:"continue-on-error"`
				Uses     string            `yaml:"uses"`
				With     map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(raw), &workflow); err != nil {
		return err
	}
	if workflow.Permissions["contents"] != "read" || workflow.Permissions["packages"] != "read" {
		return fmt.Errorf("PR permissions changed")
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		return fmt.Errorf("manual trigger missing")
	}
	manual, ok := workflow.Jobs["go-cache-qualification"]
	if !ok || manual.If != "github.event_name == 'workflow_dispatch'" || manual.CacheMode != "read" || manual.Continue != nil || !reflect.DeepEqual(manual.Permissions, map[string]string{"contents": "read", "packages": "read"}) || !reflect.DeepEqual(manual.Steps, workflow.Jobs["image"].Steps) {
		return fmt.Errorf("manual qualification authority/steps differ")
	}
	rows := manual.Strategy.Matrix.Include
	if len(rows) != 1 || rows[0].Service != "regulatory" || rows[0].Dockerfile != "kanz/services/regulatory/Dockerfile" || !rows[0].CacheProbe {
		return fmt.Errorf("manual qualification selection differs")
	}
	probe, retain, buildIndex, unit := -1, -1, -1, -1
	for i, step := range manual.Steps {
		if step.ID == "build" {
			buildIndex = i
		}
		if step.Name == "Verify shared-cache controls (manual only)" {
			if unit >= 0 || step.If != "matrix.cache_probe == true" || step.Continue != nil || strings.TrimSpace(step.Run) != `python -m unittest discover -s .github/scripts -p test_go_image_cache_probe.py
python -m unittest discover -s .github/scripts -p test_lake_cache_probe.py` {
				return fmt.Errorf("manual unit controls missing")
			}
			unit = i
		}
		if step.Name == "Qualify shared Go dependency cache (manual only)" {
			if probe >= 0 || step.If != "matrix.cache_probe == true" || step.Continue != nil || step.Uses != "actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3" || strings.TrimSpace(step.With["script"]) != `const cp = require('node:child_process');
const tree = cp.execFileSync('git', ['rev-parse', 'HEAD^{tree}'], {encoding: 'utf8', maxBuffer: 4096, timeout: 10000}).trim();
const env = {};
for (const key of ['PATH', 'HOME', 'DOCKER_CONFIG', 'RUNNER_TEMP', 'TMPDIR', 'ACTIONS_RUNTIME_TOKEN', 'ACTIONS_RESULTS_URL', 'ACTIONS_CACHE_SERVICE_V2', 'ACTIONS_CACHE_MODE']) {
  const value = process.env[key];
  if (value !== undefined) { if (value.length > 16384) throw new Error('cache environment bound exceeded'); env[key] = value; }
}
if (!env.PATH || !env.HOME || !env.ACTIONS_RUNTIME_TOKEN || !env.ACTIONS_RESULTS_URL || env.ACTIONS_CACHE_MODE !== 'read') throw new Error('read-only cache environment missing');
cp.execFileSync('python', ['.github/scripts/go_image_cache_probe.py', '--source', process.env.PROBE_SOURCE, '--tree', tree, '--run', process.env.PROBE_RUN, '--attempt', process.env.PROBE_ATTEMPT, '--output', process.env.RUNNER_TEMP + '/go-cache-proof'], {stdio: 'inherit', env});` {
				return fmt.Errorf("manual probe omitted/bypassed")
			}
			probe = i
		}
		if step.Name == "Retain manual shared-cache evidence" {
			if retain >= 0 || step.If != "always() && matrix.cache_probe == true" || step.Continue != nil || step.Uses != "actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9" || step.With["if-no-files-found"] != "error" || step.With["path"] != "${{ runner.temp }}/go-cache-proof/" || step.With["name"] != "go-cache-${{ github.sha }}-${{ github.run_id }}-${{ github.run_attempt }}" {
				return fmt.Errorf("manual proof retention differs")
			}
			retain = i
		}
	}
	if buildIndex < 0 || unit <= buildIndex || probe <= unit || retain <= probe {
		return fmt.Errorf("manual proof steps absent/reordered")
	}
	for _, name := range []string{"image", "publish"} {
		job, ok := workflow.Jobs[name]
		if !ok {
			return fmt.Errorf("missing image owner")
		}
		if name == "image" && (job.If != "github.event_name == 'pull_request'" || len(job.Permissions) != 0) {
			return fmt.Errorf("PR authority changed")
		}
		if name == "publish" && job.If != "github.event_name == 'push' && github.ref == 'refs/heads/main' && github.repository == 'eighred/kanz'" {
			return fmt.Errorf("publisher authority changed")
		}
		builds := 0
		for _, step := range job.Steps {
			if step.ID != "build" {
				continue
			}
			builds++
			if step.If != "" || step.Continue != nil || step.Uses != "docker/build-push-action@c3c9e263c25d99ce0380d002d59b67737d91b0dc" || strings.TrimSpace(step.With["cache-from"]) != goSharedImport || step.With["cache-to"] != goOwnExport || step.With["context"] != "." || step.With["file"] != "${{ matrix.dockerfile }}" || strings.TrimSpace(step.With["build-args"]) != "VERSION=${{ github.sha }}" || step.With["push"] != "false" {
				return fmt.Errorf("build/cache input or authority changed")
			}
		}
		if builds != 1 || len(job.Strategy.Matrix.Include) != 36 {
			return fmt.Errorf("image inventory changed")
		}
		prefix := ""
		count := 0
		seen := map[string]bool{}
		for _, row := range job.Strategy.Matrix.Include {
			if row.CacheProbe {
				return fmt.Errorf("normal builds must not run qualification")
			}
			if seen[row.Service] {
				return fmt.Errorf("duplicate service")
			}
			seen[row.Service] = true
			body, ok := files[row.Dockerfile]
			if !ok {
				return fmt.Errorf("missing Dockerfile")
			}
			actualGo := strings.Contains(body, "FROM ghcr.io/eighred/base/golang:")
			selected := strings.HasPrefix(row.Dockerfile, "kanz/") && row.Service != "cloudflared" && row.Service != "prometheus"
			if actualGo != selected {
				return fmt.Errorf("shared scope selects wrong image")
			}
			if !actualGo {
				continue
			}
			count++
			if err := checkGoImageDependencyLayer(body); err != nil {
				return err
			}
			var lines []string
			started := false
			for _, line := range strings.Split(body, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "FROM ghcr.io/eighred/base/golang:") {
					started = true
				}
				if !started || line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				lines = append(lines, line)
				if line == "RUN sh /usr/local/bin/go-mod-download.sh" {
					break
				}
			}
			got := strings.Join(lines, "\n")
			if prefix == "" {
				prefix = got
			} else if prefix != got {
				return fmt.Errorf("shared dependency fingerprint differs")
			}
		}
		if count != 31 || !seen["accounting"] {
			return fmt.Errorf("Go/cache owner inventory differs")
		}
	}
	return nil
}

func TestGoImagesImportSharedDependenciesWithoutSharedWrites(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	raw := read(t, filepath.Join(root, ".github/workflows/build.yml"))
	files := map[string]string{}
	// The typed workflow parser owns selection; collect only its declared paths.
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []struct {
						Dockerfile string `yaml:"dockerfile"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(raw), &wf); err != nil {
		t.Fatal(err)
	}
	for _, row := range wf.Jobs["image"].Strategy.Matrix.Include {
		files[row.Dockerfile] = read(t, filepath.Join(root, row.Dockerfile))
	}
	if err := checkGoSharedCache(raw, files); err != nil {
		t.Fatal(err)
	}
	for _, m := range [][2]string{
		{"type=gha,scope=${{ matrix.service }}\n", ""},
		{"&& 'type=gha,scope=accounting'", "&& 'type=gha,scope=oms'"},
		{"matrix.service != 'cloudflared'", "matrix.service != 'inference'"},
		{" && matrix.service != 'prometheus'", ""},
		{"github.event_name == 'push' && github.ref == 'refs/heads/main' && format", "github.event_name == 'pull_request' && format"},
		{"scope={0},mode=max", "scope=accounting,mode=max"},
		{"context: .\n", "context: https://example.invalid\n"},
		{"id: build\n", "id: build\n        if: false\n"},
		{"id: build\n", "id: build\n        continue-on-error: true\n"},
		{"cache-mode: read", "cache-mode: write"},
		{"  workflow_dispatch:\n", ""},
		{"python -m unittest discover -s .github/scripts -p test_go_image_cache_probe.py", "echo omitted-unit"},
		{"if: github.event_name == 'workflow_dispatch'", "if: true"},
		{"cache_probe: true", "cache_probe: false"},
		{"if: matrix.cache_probe == true", "if: false"},
		{"cp.execFileSync('python'", "cp.execFileSync('echo'"},
		{"if: always() && matrix.cache_probe == true", "if: success() && matrix.cache_probe == true"},
	} {
		changed := strings.Replace(raw, m[0], m[1], 1)
		if changed == raw {
			t.Fatal("mutation did not apply")
		}
		if checkGoSharedCache(changed, files) == nil {
			t.Fatalf("accepted cache mutation %s", m[0])
		}
	}
	path := "kanz/services/accounting/Dockerfile"
	original := files[path]
	for _, m := range [][2]string{{"ENV GOFLAGS=-mod=mod", "ENV GOFLAGS=-mod=readonly"}, {"COPY kanz/go.mod kanz/go.sum ./kanz/", "COPY kanz/go.mod ./kanz/"}, {"COPY kanz-schemas/gen/go/ /src/kanz-schemas/gen/go/", "# omitted SDK"}} {
		files[path] = strings.Replace(original, m[0], m[1], 1)
		if files[path] == original {
			t.Fatal("mutation did not apply")
		}
		if checkGoSharedCache(raw, files) == nil {
			t.Fatal("accepted dependency/compiler mutation")
		}
	}
}
