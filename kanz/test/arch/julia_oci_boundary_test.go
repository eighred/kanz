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
		"/opt/julia/share/julia/base/JuliaSyntax/docs", "/opt/julia/share/julia/stdlib/v1.13/TOML/docs",
		"/opt/julia/share/julia/stdlib/v1.13/TOML/benchmark", "/opt/julia/share/julia/stdlib/v1.13/Tar/test",
		"/opt/julia/share/julia/test; do", `test -d "$path" && test ! -L "$path" && rm -rf -- "$path" && test ! -e "$path" || exit 1`,
		"JULIA_PKG_OFFLINE=true", "USER 65532:65532",
		"printf '%s' \"$VERSION\" | grep -Eq '^[0-9a-f]{40}$'",
		`ENTRYPOINT ["/opt/julia/bin/julia", "--startup-file=no", "--history-file=no", "--compiled-modules=existing", "--pkgimages=existing", "--project=/opt/kanz-jl", "/opt/kanz-jl/bin/curve_worker.jl"]`,
	},
	"kanz-jl/qualification/proof.py": {
		"'--network', 'none', '--read-only', '--user', '65532:65532'",
		"'--memory', '1g', '--memory-swap', '1g', '--pids-limit', '64'",
		"'--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--cpus', '1'",
		"container_boundary(info, image, source)", "validate_inventory(inventory, source, runtime_sha256, native_expected)",
		"native_inventory.validate(inventory.get('nativeRuntime'), native_expected)",
		"native_inventory.expected_archive(Path(os.environ['RUNNER_TEMP']) / 'julia.tar.gz')",
		"if inventory.get('excludedDevelopmentEnvironments') != EXCLUDED_DEVELOPMENT_ENVIRONMENTS:",
		"if inventory['generated'] != generated:", "if (inventory['sources'] != sources or",
		"oracle(scratch, 'verify')", "report['passed'] &= clean",
		"'juliaAdvisoryCoverage': 'unqualified-release-blocker'",
	},
	"kanz-jl/qualification/isolation.jl": {
		"bounded_hash(path, 768 * 2^20)", "native_total[] <= 2^31",
		"native_links[relative] = target", "native_jll[name] = info",
		`"share/julia/compiled"`, `haskey(native_files, relpath(path, "/opt/julia"))`,
		`haskey(generated_native, relpath(path, "/opt/julia-depot"))`,
		"@test !isempty(outside)",
		"outside[path] = bounded_hash(path, 128 * 2^20)",
		"@test !ispath(joinpath(\"/opt/julia/share/julia\", path))",
		"@test !islink(joinpath(\"/opt/julia/share/julia\", path))",
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
	"kanz-jl/qualification/native_inventory.py": {
		"hashlib.file_digest(stream, 'sha256').hexdigest() != ARCHIVE_SHA256",
		"member.size > 768 << 20", "total > MAX_BYTES", "name in seen or len(seen) >= MAX_FILES",
		"not safe_link(name, member.linkname)", "resolved not in files",
		"observed[k] != expected[k]", "observed['unassignedFiles'] != sorted(list(expected['files']) + ['depot/' + p for p in generated])",
		"native-library-advisory-ownership-unqualified", "generated = observed['generatedNativeImages']",
		"sum(item['size'] for item in generated.values()) > MAX_BYTES",
	},
	"kanz-jl/qualification/install-audit.sh": {
		"v0.72.0/trivy_0.72.0_Linux-64bit.tar.gz",
		"bbb64b9695866ce4a7a8f5c9592002c5961cab378577fa3f8a040df362b9b2ea",
		"v1.56.0/syft_1.56.0_linux_amd64.tar.gz",
		"1c2493cfcf52d3367d6cb25780d9190c111c2219c34c820574c43e8bae15c528",
		"sha256sum --check --strict",
	},
	"kanz-jl/qualification/advisory.py": {
		"proof.validate_report(qualification, source, digest(Path(shutil.which('julia')).resolve()),",
		"validate_control(control_scan)", "validate_image_scan(scan, image, expected)",
		"validate_spdx(sbom, expected)", "validate_syft(syft, report['archiveConfigDigest'])",
		"identity.archive_config_digest(archive, alias)", "if tagged != image:",
		"image_config_digest(image, scratch)",
		"validate_database(metadata, dt.datetime.now(dt.timezone.utc))",
		"digest(db / 'trivy.db') != db_hash", "validate_report(report, qualification, expected)",
		"proof.native_inventory.expected_archive(Path(os.environ['RUNNER_TEMP']) / 'julia.tar.gz')",
		"'--image-src', 'docker', image", "'--ignore-unfixed=false'",
		"'stdlib-runtime-native-advisory-coverage-unqualified'",
		"actual != expected", "'known vulnerable Julia control not detected'",
		"'reviewed General registry package UUID differs'",
		"'critical actual-image advisory refuses qualification'",
		"tool_identity(executable, archive)", "digest(executable) != expected",
		"scan.get('Metadata', {}).get('ImageID') != image",
	},
	".github/workflows/julia-calibration.yml": {
		"packages: read", "name: Authenticate for private mirrored bases",
		"python3 -m pip install PyYAML==6.0.3",
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
	audit := 0
	tools := 0
	for _, step := range job.Steps {
		if step.Name == "Install checksum-pinned image advisory tools" {
			if step.If != "" || step.Run != "bash kanz-jl/qualification/install-audit.sh" {
				return fmt.Errorf("scanner setup conditional or substituted")
			}
			tools++
		}
		if step.Name == "Qualify actual image Julia advisories and inventory" {
			if found != 1 || tools != 1 || step.If != "" || step.Run != "python3 -B kanz-jl/qualification/advisory.py" {
				return fmt.Errorf("actual image advisory step conditional, reordered or substituted")
			}
			audit++
		}
		if step.Name == "Qualify the actual Julia image without network or write authority" {
			if step.If != "" || step.Run != "python3 -B kanz-jl/qualification/proof.py" {
				return fmt.Errorf("actual OCI step conditional or substituted")
			}
			found++
		}
	}
	if found != 1 || audit != 1 || tools != 1 {
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
		{"      - name: Qualify actual image Julia advisories and inventory\n", "      - name: Qualify actual image Julia advisories and inventory\n        if: false\n"},
		{"      - name: Install checksum-pinned image advisory tools\n", "      - name: Install checksum-pinned image advisory tools\n        if: false\n"},
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
