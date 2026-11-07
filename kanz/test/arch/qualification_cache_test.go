package arch

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func qualificationCachePolicy(raw string) error {
	var workflow struct {
		CacheMode string `yaml:"cache-mode"`
		Jobs      map[string]struct {
			CacheMode string `yaml:"cache-mode"`
			Steps     []struct {
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(raw), &workflow); err != nil {
		return err
	}
	if workflow.CacheMode != "read" || len(workflow.Jobs) == 0 {
		return fmt.Errorf("qualification must inherit read-only cache access")
	}
	for _, job := range workflow.Jobs {
		if job.CacheMode != "" && job.CacheMode != "read" {
			return fmt.Errorf("qualification job overrides read-only cache access")
		}
		for _, step := range job.Steps {
			if _, exports := step.With["cache-to"]; exports {
				return fmt.Errorf("qualification must not request cache export with a read-only token")
			}
		}
	}
	return nil
}

func TestRustQualificationCachesOnlyRestore(t *testing.T) {
	for _, name := range []string{"architecture-migration.yml", "rust-oms-fold.yml"} {
		t.Run(name, func(t *testing.T) {
			raw := string(mustReadArchFile(t, filepath.Join(moduleRoot(t), "../.github/workflows", name)))
			if err := qualificationCachePolicy(raw); err != nil {
				t.Fatal(err)
			}
			for _, replacement := range []string{"cache-mode: write", "cache-mode: write-only", "cache-mode: none", ""} {
				if qualificationCachePolicy(strings.Replace(raw, "cache-mode: read", replacement, 1)) == nil {
					t.Fatal("changed cache authority escaped")
				}
			}
			// A job override must not silently regain writes or lose restores.
			for _, mode := range []string{"write", "write-only", "none"} {
				if qualificationCachePolicy("cache-mode: read\njobs:\n  probe:\n    cache-mode: "+mode+"\n") == nil {
					t.Fatal("job cache override escaped")
				}
			}
			if qualificationCachePolicy("cache-mode: read\njobs:\n  probe:\n    steps:\n      - with:\n          cache-to: type=gha,mode=max\n") == nil {
				t.Fatal("read-only BuildKit export escaped")
			}
		})
	}
}
