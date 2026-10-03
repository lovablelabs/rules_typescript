package main

import (
	"os"
	"path/filepath"
	"testing"
)

func cleanupInput(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "owned")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "input"), []byte("live input"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCleanupCannotChangeOwnedResourceIdentity(t *testing.T) {
	t.Run("workspace replacement", func(t *testing.T) {
		root := cleanupInput(t)
		resources := cleanupResources{{Path: root}}
		if err := resources.remove(); err == nil {
			t.Fatal("nonrecursive removal unexpectedly removed a nonempty replacement")
		}
		if data, err := os.ReadFile(filepath.Join(root, "input")); err != nil || string(data) != "live input" {
			t.Fatalf("workspace replacement damaged: %q, %v", data, err)
		}
	})
	t.Run("changed directory", func(t *testing.T) {
		original, unrelated := cleanupInput(t), cleanupInput(t)
		t.Chdir(filepath.Dir(original))
		plan := &Plan{}
		if err := plan.own("owned", true); err != nil {
			t.Fatal(err)
		}
		t.Chdir(filepath.Dir(unrelated))
		plan.Cleanup()
		requireRemoved(t, original)
		if data, err := os.ReadFile(filepath.Join(unrelated, "input")); err != nil || string(data) != "live input" {
			t.Fatalf("chdir redirected cleanup to unrelated input: %q, %v", data, err)
		}
	})
}

func requireRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mutable resource remains after cleanup: %v", err)
	}
}
