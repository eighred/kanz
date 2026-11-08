package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This closed recipe preserves the existing Go owner and ships one optional
// worker. Actual image inventory, entrypoint and confinement remain CI proofs.
func lakeHybridImageRecipe(raw string) error {
	if err := checkGoImageDependencyLayer(raw); err != nil {
		return err
	}
	var stages [][]string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "FROM ") {
			stages = append(stages, nil)
		}
		if len(stages) == 0 {
			return fmt.Errorf("instruction outside image stage")
		}
		stages[len(stages)-1] = append(stages[len(stages)-1], line)
	}
	tools := []string{
		"FROM ghcr.io/eighred/base/rust:1.90.0-bookworm@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f AS rust-tools",
		"RUN cargo install --locked --version 0.7.7 cargo-auditable",
		"RUN cargo install --locked --version 0.1.78 cargo-chef", "WORKDIR /src/kanz-rs",
	}
	planner := []string{
		"FROM rust-tools AS rust-planner", "COPY kanz-rs/ ./",
		"RUN cargo chef prepare --recipe-path /recipe.json",
	}
	rust := []string{
		"FROM rust-tools AS rust-build",
		"COPY --from=rust-planner /recipe.json /recipe.json",
		"RUN cargo auditable chef cook --locked --release --recipe-path /recipe.json -p kanz-lake-snapshot",
		"COPY kanz-rs/ ./", "COPY kanz-schemas/proto/ /src/kanz-schemas/proto/", "ARG VERSION",
		`RUN test -n "$VERSION" && KANZ_BUILD_VERSION="$VERSION" cargo auditable build --locked --release --manifest-path Cargo.toml -p kanz-lake-snapshot`,
	}
	runtime := []string{
		"FROM ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f",
		"COPY --from=build /out/lake-sink /lake-sink",
		"COPY --from=rust-build --chown=0:0 --chmod=0555 /src/kanz-rs/target/release/kanz-lake-snapshot /kanz-lake-snapshot",
		"USER nonroot:nonroot", "EXPOSE 8085", `ENTRYPOINT ["/lake-sink"]`,
	}
	if len(stages) != 5 || !slices.Equal(stages[1], tools) || !slices.Equal(stages[2], planner) || !slices.Equal(stages[3], rust) || !slices.Equal(stages[4], runtime) {
		return fmt.Errorf("lake hybrid image must preserve closed worker and runtime recipe")
	}
	return nil
}

func lakeHybridCI(raw string) error {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name     string            `yaml:"name"`
				Uses     string            `yaml:"uses"`
				ID       string            `yaml:"id"`
				Env      map[string]string `yaml:"env"`
				If       any               `yaml:"if"`
				Continue any               `yaml:"continue-on-error"`
				Run      string            `yaml:"run"`
				With     map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(raw), &workflow); err != nil {
		return err
	}
	fields := map[string]map[string]string{
		"Install pinned hybrid signature verifier":              {},
		"Build actual hybrid lake image without publishing":     {"context": ".", "file": "kanz/services/lake-sink/Dockerfile", "build-args": "VERSION=${{ github.sha }}", "push": "false", "load": "true", "builder": "${{ steps.lake-builder.outputs.name }}"},
		"Catalog actual hybrid applications and native runtime": {"image": "docker:${{ steps.lake-image.outputs.image }}", "format": "spdx-json", "syft-version": "v1.56.0", "output-file": "snapshot-owner-proof/hybrid.spdx.json"},
		"Scan actual hybrid runtime before execution":           {"image-ref": "${{ steps.lake-image.outputs.image }}", "exit-code": "1", "version": "v0.72.0", "format": "json", "output": "snapshot-owner-proof/hybrid-trivy.json", "list-all-pkgs": "true"},
	}
	scripts := map[string][]string{
		"Verify complete source-bound owner evidence":                     {"check_lake_snapshot_owner_evidence.py --seal"},
		"Bind actual hybrid image identity":                               {"image=$(docker image inspect", "echo \"image=$image\" >> \"$GITHUB_OUTPUT\""},
		"Verify synthetic cache probe controls":                           {"test_lake_cache_probe.py"},
		"Prove joint synthetic source and VERSION dependency cache reuse": {"const env = {};", "ACTIONS_RUNTIME_TOKEN", "ACTIONS_RESULTS_URL", "env.ACTIONS_CACHE_MODE !== 'read'", "'.github/scripts/lake_cache_probe.py', '--source', process.env.PROBE_SOURCE", "'--image', process.env.LAKE_IMAGE, '--builder', process.env.LAKE_BUILDER", "'--output', 'snapshot-owner-proof'], {stdio: 'inherit', env}"},
		"Install reviewed hybrid audit tools":                             {"set -euo pipefail", "bash .github/scripts/install-rust-audit.sh", "govulncheck@v1.6.0"},
		"Require hybrid binary provenance and fresh Rust and Go audits":   {"set -euo pipefail", `rust_release_evidence.py --local --service lake-sink --image "$LAKE_IMAGE" --sbom snapshot-owner-proof/hybrid.spdx.json --output snapshot-owner-proof/hybrid`},
		"Actual finite owner on both broker versions without skips":       {"TEST_LAKE_IMAGE_ID=", "TEST_LAKE_IMAGE_VERSION=", "TEST_LAKE_IMAGE_WORKER_SHA256=", `json.load(open("snapshot-owner-proof/hybrid/evidence.json"))["binarySHA256"]`, "TEST_LAKE_REDPANDA_IMAGE_ID="},
		"Stop owned snapshot fixture dependencies and prove absence":      {"label=kanz.snapshot-image-run=$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT", `test -z "$image_after$image_volumes_after"`, "image_containers_absent", "image_volumes_absent", `^/kanz-lake-image-$nonce-[0-9]+$`, `^kanz-lake-image-$nonce-[0-9]+$`},
	}
	seen := map[string]int{}
	builderIndex := -1
	for i, step := range workflow.Jobs["finite-snapshot-owner"].Steps {
		if step.Uses == "docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069" {
			if builderIndex != -1 || step.ID != "lake-builder" || step.If != nil || step.Continue != nil || step.With["version"] != "v0.37.1" {
				return fmt.Errorf("hybrid builder identity differs")
			}
			builderIndex = i
		}
		if step.Name == "Prove joint synthetic source and VERSION dependency cache reuse" && (step.Env["LAKE_IMAGE"] != "${{ steps.lake-image.outputs.image }}" || step.Env["LAKE_BUILDER"] != "${{ steps.lake-builder.outputs.name }}") {
			return fmt.Errorf("synthetic probe escaped canonical image/builder")
		}
		wanted, action := fields[step.Name]
		markers, script := scripts[step.Name]
		if !action && !script {
			continue
		}
		if _, exists := seen[step.Name]; exists {
			return fmt.Errorf("duplicate hybrid step")
		}
		seen[step.Name] = i
		if step.Continue != nil || (step.Name != "Stop owned snapshot fixture dependencies and prove absence" && step.If != nil) {
			return fmt.Errorf("hybrid step bypass")
		}
		if step.Name == "Install pinned hybrid signature verifier" && step.Uses != "sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6" {
			return fmt.Errorf("hybrid signature verifier identity missing")
		}
		for key, value := range wanted {
			if step.With[key] != value {
				return fmt.Errorf("hybrid action identity missing")
			}
		}
		for _, marker := range markers {
			body := step.Run
			if step.Name == "Prove joint synthetic source and VERSION dependency cache reuse" {
				if step.Uses != "actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3" || step.Run != "" || step.Env["PROBE_SOURCE"] != "${{ github.sha }}" || step.Env["PROBE_RUN"] != "${{ github.run_id }}" || step.Env["PROBE_ATTEMPT"] != "${{ github.run_attempt }}" {
					return fmt.Errorf("read-only cache probe invocation differs")
				}
				body = step.With["script"]
			}
			if !strings.Contains(body, marker) {
				return fmt.Errorf("hybrid script binding missing")
			}
		}
	}
	if len(seen) != len(fields)+len(scripts) {
		return fmt.Errorf("missing hybrid qualification step")
	}
	order := []string{
		"Install pinned hybrid signature verifier", "Install reviewed hybrid audit tools",
		"Build actual hybrid lake image without publishing", "Bind actual hybrid image identity",
		"Catalog actual hybrid applications and native runtime", "Scan actual hybrid runtime before execution",
		"Require hybrid binary provenance and fresh Rust and Go audits",
		"Actual finite owner on both broker versions without skips",
		"Stop owned snapshot fixture dependencies and prove absence",
		"Verify synthetic cache probe controls",
		"Prove joint synthetic source and VERSION dependency cache reuse",
		"Verify complete source-bound owner evidence",
	}
	for i := 1; i < len(order); i++ {
		if seen[order[i-1]] >= seen[order[i]] {
			return fmt.Errorf("hybrid prerequisite/evidence ordering differs")
		}
	}
	if builderIndex < 0 || builderIndex >= seen["Build actual hybrid lake image without publishing"] {
		return fmt.Errorf("builder ordering differs")
	}
	return nil
}

func TestLakeHybridCIWiring(t *testing.T) {
	raw := string(mustReadArchFile(t, filepath.Join(moduleRoot(t), "../.github/workflows/architecture-migration.yml")))
	if err := lakeHybridCI(raw); err != nil {
		t.Fatal(err)
	}
	installer := "      - name: Install pinned hybrid signature verifier\n        uses: sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6 # v4.1.2\n"
	without := strings.Replace(raw, installer, "", 1)
	late := strings.Replace(without, "      - name: Require hybrid binary provenance and fresh Rust and Go audits\n", installer+"      - name: Require hybrid binary provenance and fresh Rust and Go audits\n", 1)
	if without == raw || late == without || lakeHybridCI(without) == nil || lakeHybridCI(late) == nil {
		t.Fatal("signature verifier after its consumer escaped")
	}
	moveBefore := func(name, before string) string {
		marker := "      - name: " + name + "\n"
		start := strings.Index(raw, marker)
		if start < 0 {
			t.Fatal("step anchor missing")
		}
		next := strings.Index(raw[start+len(marker):], "      - ")
		if next < 0 {
			t.Fatal("step end missing")
		}
		end := start + len(marker) + next
		block := raw[start:end]
		remaining := raw[:start] + raw[end:]
		return strings.Replace(remaining, "      - name: "+before+"\n", block+"      - name: "+before+"\n", 1)
	}
	for _, pair := range [][2]string{
		{"Build actual hybrid lake image without publishing", "Install reviewed hybrid audit tools"},
		{"Actual finite owner on both broker versions without skips", "Require hybrid binary provenance and fresh Rust and Go audits"},
		{"Prove joint synthetic source and VERSION dependency cache reuse", "Actual finite owner on both broker versions without skips"},
		{"Prove joint synthetic source and VERSION dependency cache reuse", "Stop owned snapshot fixture dependencies and prove absence"},
		{"Verify complete source-bound owner evidence", "Prove joint synthetic source and VERSION dependency cache reuse"},
	} {
		if changed := moveBefore(pair[0], pair[1]); changed == raw || lakeHybridCI(changed) == nil {
			t.Fatal("hybrid order mutation escaped")
		}
	}
	for _, m := range [][2]string{
		{"env.ACTIONS_CACHE_MODE !== 'read'", "env.ACTIONS_CACHE_MODE !== 'write'"},
		{"const env = {};", "const env = process.env;"},
		{"name: Verify synthetic cache probe controls", "name: Missing synthetic controls"},
		{"name: Prove joint synthetic source and VERSION dependency cache reuse", "name: Missing synthetic cache probe"},
		{"name: Prove joint synthetic source and VERSION dependency cache reuse", "name: Prove joint synthetic source and VERSION dependency cache reuse\n        if: false"},
		{"id: lake-builder", "id: unrelated-builder"},
		{`builder: ${{ steps.lake-builder.outputs.name }}`, "builder: other-builder"},
		{`LAKE_BUILDER: ${{ steps.lake-builder.outputs.name }}`, "LAKE_BUILDER: other-builder"},
		{"name: Install pinned hybrid signature verifier", "name: Missing hybrid signature verifier"},
		{"name: Install pinned hybrid signature verifier", "name: Install pinned hybrid signature verifier\n        if: false"},
		{"name: Install pinned hybrid signature verifier", "name: Install pinned hybrid signature verifier\n        continue-on-error: true"},
		{"sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6", "sigstore/cosign-installer@main"},
		{"name: Build actual hybrid lake image without publishing", "name: Missing hybrid image"},
		{"name: Require hybrid binary provenance and fresh Rust and Go audits", "name: Require hybrid binary provenance and fresh Rust and Go audits\n        continue-on-error: true"},
		{"govulncheck@v1.6.0", "govulncheck@latest"},
		{`json.load(open("snapshot-owner-proof/hybrid/evidence.json"))["binarySHA256"]`, `json.load(open("snapshot-owner-proof/binaries-before.json"))["worker"]`},
		{`test -z "$image_after$image_volumes_after"`, `true`},
		{"list-all-pkgs: true", "list-all-pkgs: false"},
	} {
		changed := strings.Replace(raw, m[0], m[1], 1)
		if changed == raw || lakeHybridCI(changed) == nil {
			t.Fatal("hybrid CI weakening escaped")
		}
	}
}

func TestLakeHybridImageRecipe(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(moduleRoot(t), "services/lake-sink/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(body)
	if err := lakeHybridImageRecipe(raw); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ old, replacement string }{
		{"cargo auditable build --locked", "cargo build --locked"},
		{"cargo install --locked --version 0.1.78 cargo-chef", "cargo install cargo-chef"},
		{"cargo auditable chef cook --locked", "cargo chef cook --locked"},
		{"cargo chef prepare --recipe-path /recipe.json", "cargo chef prepare --bin kanz-lake-snapshot --recipe-path /recipe.json"},
		{"COPY --from=rust-planner /recipe.json /recipe.json", "COPY kanz-rs/ ./"},
		{"RUN cargo auditable chef cook", "ARG VERSION\nRUN cargo auditable chef cook"},
		{"WORKDIR /src/kanz-rs", "WORKDIR /other"},
		{"RUN cargo auditable chef cook", "RUN --mount=type=cache,target=/src/kanz-rs/target cargo auditable chef cook"},
		{"--chown=0:0", "--chown=65532:65532"},
		{"--chmod=0555", "--chmod=0755"},
		{"COPY kanz-schemas/proto/ /src/kanz-schemas/proto/", ""},
		{"-p kanz-lake-snapshot", "-p kanz-okx-depth"},
		{"USER nonroot:nonroot", "USER root:root"},
		{`ENTRYPOINT ["/lake-sink"]`, `ENTRYPOINT ["/kanz-lake-snapshot"]`},
		{"EXPOSE 8085", "ENV LAKE_SNAPSHOT_ENABLED=true\nEXPOSE 8085"},
		{"9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f", strings.Repeat("a", 64)},
		{"COPY kanz-schemas/gen/go/go.mod kanz-schemas/gen/go/go.sum ./kanz-schemas/gen/go/", "COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/"},
	} {
		changed := strings.Replace(raw, mutation.old, mutation.replacement, 1)
		if changed == raw || lakeHybridImageRecipe(changed) == nil {
			t.Fatalf("hybrid recipe mutation escaped: %s", mutation.old)
		}
	}
}
