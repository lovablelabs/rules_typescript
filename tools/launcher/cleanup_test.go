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

func TestCleanupSkipTmpdirOptInKeepsOnlyTreesUnderTestTmpdir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TEST_TMPDIR", tmp)
	inside := filepath.Join(tmp, "ts_test_runfiles123")
	if err := os.MkdirAll(filepath.Join(inside, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := cleanupInput(t)
	sibling := tmp + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })
	resources := cleanupResources{{Path: inside, Recursive: true}, {Path: outside, Recursive: true}, {Path: sibling, Recursive: true}}

	t.Setenv("TEST_TMPDIR", "")
	t.Setenv("TS_TEST_SKIP_TMPDIR_CLEANUP", "1")
	t.Chdir(filepath.Dir(outside))
	if keptForBazel(outside) {
		t.Fatal("an unset TEST_TMPDIR kept a tree under the working directory")
	}
	t.Setenv("TEST_TMPDIR", tmp)

	t.Setenv("TS_TEST_SKIP_TMPDIR_CLEANUP", "1")
	if err := resources.remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inside, "node_modules")); err != nil {
		t.Fatalf("opted-in cleanup removed the tree under TEST_TMPDIR: %v", err)
	}
	requireRemoved(t, outside)
	requireRemoved(t, sibling)

	t.Setenv("TS_TEST_SKIP_TMPDIR_CLEANUP", "")
	if err := resources.remove(); err != nil {
		t.Fatal(err)
	}
	requireRemoved(t, inside)
}
