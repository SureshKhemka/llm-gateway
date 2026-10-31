//go:build lmstudio

package lmstudio

import (
	"slices"
	"testing"
)

// The user's inventory as of 2026-10-31 (CLAUDE.md).
func TestClassifyInventory(t *testing.T) {
	want := map[string]Role{
		"google/gemma-4-31b":                   RoleTier3,
		"qwen/qwen3.8-27b":                     RoleTier3,
		"qwen3.6-27b-ud-mlx":                   RoleTier3,
		"gemma-4-26b-a4b-it-mlx":               RoleTier3,
		"mistralai/devstral-small-2-2512":      RoleCode,
		"whiterabbitneo-13b-v1-mlx":            RoleTier2,
		"text-embedding-nomic-embed-text-v1.5": RoleEmbedding,
	}
	for id, r := range want {
		if got := classify(id); !slices.Contains(got, r) {
			t.Errorf("%s: got %v, want %s", id, got, r)
		}
	}
	if got := classify("gemma-4-26b-a4b-it-mlx"); slices.Contains(got, RoleTier1) {
		t.Errorf("26B-A4B classified as tier 1: %v", got)
	}
}
