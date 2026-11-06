package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// #1413: scoped recipe/qualification checks, not a Julia semantic audit or proof
// of OS isolation. The mandatory actual-image Linux lane owns those assertions.
var juliaOCIMarkers = map[string][]string{
	"kanz-jl/Dockerfile": {
		"FROM " + rustBuilderPin + " AS build",
		"FROM " + rustRuntimePin,
		"5c61cb4b40332cb5a3504e7779e2f5dac093caa4a1116ded19f01413f85c63e4  /tmp/julia.tar.gz",
		"sha256sum --check --strict /tmp/locked.sha256",
		"COPY kanz-schemas/proto/ /opt/kanz-schemas/proto/",
		"JULIA_PKG_OFFLINE=true", "USER 65532:65532",
		"printf '%s' \"$VERSION\" | grep -Eq '^[0-9a-f]{40}$'",
		`ENTRYPOINT ["/opt/julia/bin/julia", "--startup-file=no", "--history-file=no", "--compiled-modules=existing", "--pkgimages=existing", "--project=/opt/kanz-jl", "/opt/kanz-jl/bin/curve_worker.jl"]`,
	},
	"kanz-jl/qualification/proof.py": {
		"'--network', 'none', '--read-only', '--user', '65532:65532'",
		"'--memory', '1g', '--memory-swap', '1g', '--pids-limit', '64'",
		"'--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--cpus', '1'",
		"container_boundary(info, image, source)", "validate_inventory(inventory, source, runtime_sha256)",
		"if inventory['generated'] != generated:", "if (inventory['sources'] != sources or",
		"oracle(scratch, 'verify')", "report['passed'] &= clean",
		"'juliaAdvisoryCoverage': 'unqualified-release-blocker'",
	},
	"kanz-jl/qualification/isolation.jl": {
		"ccall(:geteuid, Cuint, ()) == 65532", "@test isempty(Sockets.getipaddrs())",
		"@test_throws SystemError open(path, \"w\")", "Pkg.GitTools.tree_hash(root)",
		`Base.locate_package(Base.PkgId(Base.UUID(entry["uuid"]), name))`,
		`source = realpath(source)`,
		`dirname(dirname(root)) == "/opt/julia-depot/packages"`,
		`basename(dirname(root)) == name`,
		`root == "/opt/julia/share/julia/stdlib/v1.13/" * name`,
		`installed["uuid"] == entry["uuid"]`,
		`installed["version"] == entry["version"]`,
		"@test actual == entry[\"git-tree-sha1\"]",
	},
	".github/workflows/julia-calibration.yml": {
		"packages: read", "name: Authenticate for private mirrored bases",
		"run: python3 -B kanz-jl/qualification/proof.py", "if-no-files-found: error",
	},
}

func checkJuliaOCI(files map[string]string) error {
	for path, markers := range juliaOCIMarkers {
		for _, marker := range markers {
			if !strings.Contains(files[path], marker) {
				return fmt.Errorf("%s missing %q", path, marker)
			}
		}
	}
	docker := files["kanz-jl/Dockerfile"]
	if strings.Count(docker, "\nFROM ") != 2 || strings.Contains(docker, "EXPOSE") {
		return fmt.Errorf("Julia image cannot introduce another stage or network service")
	}
	wf := files[".github/workflows/julia-calibration.yml"]
	if strings.Contains(wf, "continue-on-error") || strings.Contains(wf, "packages: write") || strings.Contains(wf, "id-token:") {
		return fmt.Errorf("Julia OCI qualification cannot bypass refusal or publish")
	}
	var workflow struct {
		Jobs map[string]struct {
			If    string `yaml:"if"`
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
				If   string `yaml:"if"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(wf), &workflow); err != nil {
		return err
	}
	job, ok := workflow.Jobs["shadow-calibration"]
	if !ok || job.If != "" {
		return fmt.Errorf("mandatory Julia job missing or conditional")
	}
	found := 0
	for _, step := range job.Steps {
		if step.Name == "Qualify the actual Julia image without network or write authority" {
			if step.If != "" || step.Run != "python3 -B kanz-jl/qualification/proof.py" {
				return fmt.Errorf("actual OCI step conditional or substituted")
			}
			found++
		}
	}
	if found != 1 {
		return fmt.Errorf("actual OCI qualification step missing/duplicated")
	}
	return nil
}

func TestJuliaOCIBoundary(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	files := map[string]string{}
	for path := range juliaOCIMarkers {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(b)
	}
	if err := checkJuliaOCI(files); err != nil {
		t.Fatal(err)
	}
	for path, markers := range juliaOCIMarkers {
		for _, marker := range markers {
			t.Run(path+"/"+marker, func(t *testing.T) {
				mutated := map[string]string{}
				for name, body := range files {
					mutated[name] = body
				}
				mutated[path] = strings.ReplaceAll(mutated[path], marker, "removed-boundary")
				if checkJuliaOCI(mutated) == nil {
					t.Fatal("boundary mutation accepted")
				}
			})
		}
	}
	for _, mutation := range []struct{ old, new string }{
		{"  shadow-calibration:\n", "  shadow-calibration:\n    if: false\n"},
		{"      - name: Qualify the actual Julia image without network or write authority\n", "      - name: Qualify the actual Julia image without network or write authority\n        if: false\n"},
	} {
		mutated := map[string]string{}
		for name, body := range files {
			mutated[name] = body
		}
		path := ".github/workflows/julia-calibration.yml"
		mutated[path] = strings.Replace(strings.ReplaceAll(mutated[path], "\r\n", "\n"), mutation.old, mutation.new, 1)
		if checkJuliaOCI(mutated) == nil {
			t.Fatal("conditional proof mutation accepted")
		}
	}
}
