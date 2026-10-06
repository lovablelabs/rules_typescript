package codegen_tree_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

// Remote execution materializes runfiles as files, so a realpath taken inside the
// test's private tree lands in the launcher's runfiles tree under bazel-out/<cfg>/bin.
func TestTreeImportRunsFromMaterializedRunfiles(t *testing.T) {
	launcher := verify.New(t).File("tests/codegen_tree/tree_import_test_test_launcher")
	if !launcher.Exists() {
		t.FailNow()
	}
	source := os.Getenv("RUNFILES_DIR")
	if source == "" {
		t.Skip("needs a runfiles directory to materialize")
	}
	runfiles := filepath.Join(t.TempDir(), "bazel-out", "k8-fastbuild", "bin", "tree_import_test.runfiles")
	if err := materialize(source, runfiles); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(launcher.Abs())
	cmd.Env = append(withoutRunfilesEnv(os.Environ()),
		"RUNFILES_DIR="+runfiles, "TEST_SRCDIR="+runfiles, "TEST_TMPDIR="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tree_import_test failed from materialized runfiles: %v\n%s", err, out)
	}
}

func withoutRunfilesEnv(env []string) []string {
	kept := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "RUNFILES_DIR", "RUNFILES_MANIFEST_FILE", "TEST_SRCDIR", "TEST_TMPDIR", "JAVA_RUNFILES",
			"XML_OUTPUT_FILE", "TEST_SHARD_INDEX", "TEST_TOTAL_SHARDS", "TEST_SHARD_STATUS_FILE":
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

// materialize recreates src at dst as remote execution stages it: a relative symlink, such as
// pnpm's, stays a link; every other entry becomes a hard link or copy; dangling links are dropped.
func materialize(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, relative)
		if entry.Type()&fs.ModeSymlink != 0 {
			if link, err := os.Readlink(path); err == nil && !filepath.IsAbs(link) {
				return os.Symlink(link, target)
			}
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		real, err := filepath.EvalSymlinks(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		info, err := os.Stat(real)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return materialize(real, target)
		}
		if err := os.Link(real, target); err == nil {
			return nil
		}
		return copyFile(real, target, info.Mode())
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
