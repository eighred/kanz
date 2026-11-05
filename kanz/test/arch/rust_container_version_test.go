package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const rustBuilderPin = "ghcr.io/eighred/base/rust:1.90.0-bookworm@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f"
const rustRuntimePin = "ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f"

// This is a deliberately narrow source-shape guard for the first Rust runtime
// (#1413), not a Rust semantic verifier. Runtime unit tests prove the helper's
// behavior; mirror/release jobs prove registry custody. Neither is replaced here.
func rustContainerVersionBoundary(service, docker, source, runtime string) error {
	var instructions []string
	for _, line := range strings.Split(docker, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			instructions = append(instructions, line)
		}
	}
	var bases []string
	for _, line := range instructions {
		if strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			bases = append(bases, line)
		}
	}
	if len(bases) != 2 || bases[0] != "FROM "+rustBuilderPin+" AS build" || bases[1] != "FROM "+rustRuntimePin {
		return fmt.Errorf("Rust bases must be the reviewed mirrored digest pair")
	}
	joined := "\n" + strings.Join(instructions, "\n") + "\n"
	if !strings.Contains(joined, "\nARG VERSION\n") || !strings.Contains(joined, `RUN test -n "$VERSION" && KANZ_BUILD_VERSION="$VERSION" cargo build --locked --release --manifest-path kanz-rs/Cargo.toml -p kanz-`+service) {
		return fmt.Errorf("Rust release build must require and compile VERSION")
	}
	if !strings.Contains(joined, "\nUSER 65532:65532\n") {
		return fmt.Errorf("Rust runtime must retain nonroot identity")
	}
	start := strings.Index(runtime, "fn producer_version(")
	if start < 0 {
		return fmt.Errorf("missing version helper")
	}
	end := strings.Index(runtime[start:], "impl Config")
	if end < 0 {
		return fmt.Errorf("missing version/config boundary")
	}
	compact := func(s string) string {
		return strings.ReplaceAll(regexp.MustCompile(`\s+`).ReplaceAllString(s, ""), ",)", ")")
	}
	const expected = `fn producer_version(stamped: Option<&str>, development: bool, fallback: Option<String>) -> Result<String> {
 match stamped.filter(|value| !value.trim().is_empty()) {
 Some(value) => Ok(value.to_owned()),
 None if development => fallback.filter(|value| !value.trim().is_empty())
 .ok_or_else(|| "venue VERSION required for an unstamped development build".into()),
 None => Err("production binary requires a build-time KANZ_BUILD_VERSION".into()),
 }
 }`
	if compact(runtime[start:start+end]) != compact(expected) {
		return fmt.Errorf("version helper must prefer compiled stamp and restrict fallback to development")
	}

	call := `version: producer_version(build_version, development, env::var(field("VERSION")).ok())?,`
	if !strings.Contains(compact(runtime), compact(call)) {
		return fmt.Errorf("configuration must use reviewed version helper")
	}
	venue, prefix := "Binance", "BINANCE_DEPTH"
	if service == "okx-depth" {
		venue, prefix = "Okx", "OKX_DEPTH"
	} else if service != "binance-depth" {
		return fmt.Errorf("unreviewed runtime")
	}
	wrapper := fmt.Sprintf(`Config::from_env("%s",Venue::%s,option_env!("KANZ_BUILD_VERSION"))?`, prefix, venue)
	if !strings.Contains(compact(source), compact(wrapper)) || !strings.Contains(compact(source), `runtime::run(config,`) {
		return fmt.Errorf("binary must pass its compiled stamp to the shared runtime")
	}
	if !strings.Contains(joined, "\nCOPY kanz-rs/ kanz-rs/\n") || !strings.Contains(joined, `ENTRYPOINT ["/kanz-`+service+`"]`) {
		return fmt.Errorf("build must include complete workspace and launch its own binary")
	}
	return nil
}

func rustBoundaryInputs(t *testing.T, service string) (string, string, string) {
	t.Helper()
	root := filepath.Dir(moduleRoot(t))
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return read("kanz-rs/services/" + service + "/Dockerfile"), read("kanz-rs/services/" + service + "/src/main.rs"), read("kanz-rs/crates/market-feed/src/runtime.rs")
}
func TestRustContainerVersionBoundary(t *testing.T) {
	for _, service := range []string{"binance-depth", "okx-depth"} {
		d, s, r := rustBoundaryInputs(t, service)
		if err := rustContainerVersionBoundary(service, d, s, r); err != nil {
			t.Fatalf("%s: %v", service, err)
		}
	}
}
func TestRustContainerVersionMutationReach(t *testing.T) {
	for _, service := range []string{"binance-depth", "okx-depth"} {
		d, s, r := rustBoundaryInputs(t, service)
		if err := rustContainerVersionBoundary(service, d, s, r); err != nil {
			t.Fatalf("baseline: %v", err)
		}
		for _, tc := range []struct {
			name, old, new string
			target         int
		}{
			{"upstream-builder", "ghcr.io/eighred/base/rust:", "docker.io/library/rust:", 0},
			{"mutable-runtime", rustRuntimePin, strings.Split(rustRuntimePin, "@")[0], 0},
			{"wrong-digest", "9dac0a791", "0dac0a791", 0},
			{"missing-arg", "ARG VERSION", "# ARG VERSION", 0},
			{"blank-allowed", `test -n "$VERSION" && `, "", 0},
			{"stamp-removed", `KANZ_BUILD_VERSION="$VERSION" `, "", 0},
			{"root-runtime", "USER 65532:65532", "USER 0", 0},
			{"runtime-override", `version: producer_version(`, `version: another_version(`, 2},
			{"nondevelopment-fallback", "None if development =>", "None =>", 2},
			{"stamp-ignored", "Some(value) => Ok(value.to_owned())", "Some(value) => Ok(fallback.unwrap())", 2},
			{"binary-stamp-missing", `option_env!("KANZ_BUILD_VERSION")`, `None`, 1},
			{"incomplete-workspace", "COPY kanz-rs/ kanz-rs/", "COPY kanz-rs/src/ kanz-rs/src/", 0},
		} {
			t.Run(service+"/"+tc.name, func(t *testing.T) {
				values := []string{d, s, r}
				before := values[tc.target]
				values[tc.target] = strings.Replace(before, tc.old, tc.new, 1)
				if before == values[tc.target] {
					t.Fatal("mutation missed")
				}
				if rustContainerVersionBoundary(service, values[0], values[1], values[2]) == nil {
					t.Fatal("mutation accepted")
				}
			})
		}
	}
}
