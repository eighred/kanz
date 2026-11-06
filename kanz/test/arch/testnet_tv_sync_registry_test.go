package arch

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTokyoTVSyncRegistryUsesCanonicalInventory(t *testing.T) {
	raw := string(mustReadArchFile(t, filepath.Join(moduleRoot(t), "infra", "terraform-testnet", "main.tf")))
	check := func(s string) bool {
		_, rest, ok := strings.Cut(s, "capital_path_images = toset([")
		if !ok {
			return false
		}
		inventory, _, ok := strings.Cut(rest, "])")
		return ok && strings.Contains(inventory, `"tv-sync"`) &&
			strings.Contains(s, "for_each = local.capital_path_images") &&
			strings.Count(s, "Resource = [for repository in aws_ecr_repository.capital_path : repository.arn]") == 2
	}
	if !check(raw) {
		t.Fatal("tv-sync registry must use the canonical repository and scoped publisher/pull inventory")
	}
	if check(strings.Replace(raw, `"tv-sync"`, `"removed"`, 1)) {
		t.Fatal("missing tv-sync repository accepted")
	}
}
