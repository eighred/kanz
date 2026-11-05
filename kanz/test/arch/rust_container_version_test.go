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
func rustContainerVersionBoundary(docker, source string) error {
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
	if !strings.Contains(joined, "\nARG VERSION\n") || !strings.Contains(joined, `RUN test -n "$VERSION" && KANZ_BUILD_VERSION="$VERSION" cargo build --locked --release --manifest-path kanz-rs/Cargo.toml -p kanz-binance-depth`) {
		return fmt.Errorf("Rust release build must require and compile VERSION")
	}
	if !strings.Contains(joined, "\nUSER 65532:65532\n") {
		return fmt.Errorf("Rust runtime must retain nonroot identity")
	}
	start := strings.Index(source, "fn producer_version(")
	if start < 0 {
		return fmt.Errorf("missing version helper")
	}
	end := strings.Index(source[start:], "impl Config")
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
 .ok_or_else(|| { "BINANCE_DEPTH_VERSION required for an unstamped development build".into() }),
 None => Err("production binary requires a build-time KANZ_BUILD_VERSION".into()),
 }
 }`
	if compact(source[start:start+end]) != compact(expected) {
		return fmt.Errorf("version helper must prefer compiled stamp and restrict fallback to development")
	}
	call := `version: producer_version(option_env!("KANZ_BUILD_VERSION"), development, env::var("BINANCE_DEPTH_VERSION").ok())?,`
	if !strings.Contains(compact(source), compact(call)) {
		return fmt.Errorf("configuration must use reviewed build-time version helper")
	}
	return nil
}

func rustBoundaryInputs(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Dir(moduleRoot(t))
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return read("kanz-rs/services/binance-depth/Dockerfile"), read("kanz-rs/services/binance-depth/src/main.rs")
}

func TestRustContainerVersionBoundary(t *testing.T) {
	d, s := rustBoundaryInputs(t)
	if err := rustContainerVersionBoundary(d, s); err != nil {
		t.Fatal(err)
	}
}

func TestRustContainerVersionMutationReach(t *testing.T) {
	d, s := rustBoundaryInputs(t)
	if err := rustContainerVersionBoundary(d, s); err != nil {
		t.Fatalf("mutation baseline invalid: %v", err)
	}
	for _, tc := range []struct {
		name, old, new string
		source         bool
	}{
		{"upstream-builder", "ghcr.io/eighred/base/rust:", "docker.io/library/rust:", false},
		{"mutable-runtime", rustRuntimePin, strings.Split(rustRuntimePin, "@")[0], false},
		{"wrong-digest", "9dac0a791", "0dac0a791", false},
		{"missing-arg", "ARG VERSION", "# ARG VERSION", false},
		{"blank-allowed", `test -n "$VERSION" && `, "", false},
		{"stamp-removed", `KANZ_BUILD_VERSION="$VERSION" `, "", false},
		{"root-runtime", "USER 65532:65532", "USER 0", false},
		{"runtime-override", `version: producer_version(`, `version: another_version(`, true},
		{"nondevelopment-fallback", "None if development =>", "None =>", true},
		{"stamp-ignored", "Some(value) => Ok(value.to_owned())", "Some(value) => Ok(fallback.unwrap())", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := d, s
			if tc.source {
				b = strings.Replace(b, tc.old, tc.new, 1)
				if b == s {
					t.Fatal("mutation missed")
				}
			} else {
				a = strings.Replace(a, tc.old, tc.new, 1)
				if a == d {
					t.Fatal("mutation missed")
				}
			}
			if rustContainerVersionBoundary(a, b) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}
