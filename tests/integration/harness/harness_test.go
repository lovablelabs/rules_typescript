package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBazelStdout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		exit string
	}{
		{name: "failed_command_retains_stdout_diagnostics", exit: "7"},
		{name: "successful_command_preserves_stdout_exactly", exit: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bazel := filepath.Join(dir, "bazel")
			script := "#!/bin/sh\nprintf '  diagnostic 100%% complete\\nnext line\\t \\n'\nexit " + tc.exit + "\n"
			if err := os.WriteFile(bazel, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			// Close every executable writer before a sibling fork can inherit it.
			t.Parallel()
			it := &IT{bazel: bazel, WorkspaceDir: dir}
			var output string
			var caught any
			func() {
				defer func() { caught = recover() }()
				output = it.BazelStdout("run", "//:probe")
			}()
			want := "  diagnostic 100% complete\nnext line\t \n"
			if tc.exit != "0" {
				got, ok := caught.(failure)
				if !ok {
					t.Fatalf("panic = %#v, want harness failure", caught)
				}
				if !strings.Contains(got.msg, "exit status "+tc.exit) || !strings.Contains(got.msg, want) {
					t.Fatalf("failure lost exit status or stdout: %q", got.msg)
				}
			} else if caught != nil || output != want {
				t.Fatalf("stdout = %q, panic = %#v; want %q without panic", output, caught, want)
			}
		})
	}
}

// Ambient Bazel variables would otherwise mask missing fallback behavior.
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, key := range []string{"RULES_TS_IT_SCRATCH", "XDG_CACHE_HOME", "HOME", "TMPDIR", "TEST_TMPDIR", "BAZELISK_HOME"} {
		t.Setenv(key, env[key])
	}
}

func TestCacheRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{{
		name: "RULES_TS_IT_SCRATCH wins over everything",
		env: map[string]string{
			"RULES_TS_IT_SCRATCH": "/ci",
			"XDG_CACHE_HOME":      "/xdg",
			"HOME":                "/home/dev",
			"TMPDIR":              "/tmpfs",
		},
		want: "/ci",
	}, {
		name: "XDG_CACHE_HOME beats HOME",
		env: map[string]string{
			"XDG_CACHE_HOME": "/xdg",
			"HOME":           "/home/dev",
			"TMPDIR":         "/tmpfs",
		},
		want: "/xdg/rules_typescript_it",
	}, {
		name: "HOME beats the temp dir",
		env: map[string]string{
			"HOME":   "/home/dev",
			"TMPDIR": "/tmpfs",
		},
		want: "/home/dev/.cache/rules_typescript_it",
	}, {
		name: "the last resort is os.TempDir()",
		env: map[string]string{
			"TMPDIR": "/tmpfs",
		},
		want: "/tmpfs/rules_typescript_it",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			if got := cacheRoot(); got != tc.want {
				t.Errorf("cacheRoot() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Bazel scopes TEST_TMPDIR to both the target and the output base.
func TestRunRootPrefersTestTmpdir(t *testing.T) {
	tmp := t.TempDir()
	setEnv(t, map[string]string{
		"TEST_TMPDIR":         tmp,
		"RULES_TS_IT_SCRATCH": t.TempDir(),
		"HOME":                t.TempDir(),
	})
	got, err := runRoot("new_project", "/checkout/a")
	if err != nil {
		t.Fatalf("runRoot: %v", err)
	}
	if got != tmp {
		t.Errorf("runRoot() = %q, want the TEST_TMPDIR %q", got, tmp)
	}
}

// Stable paths bound disk leaks after a killed nested server.
func TestRunRootFallbackIsStablePerCheckoutAndName(t *testing.T) {
	home := t.TempDir()
	setEnv(t, map[string]string{"HOME": home, "TMPDIR": t.TempDir()})

	first, err := runRoot("new_project", "/checkout/a")
	if err != nil {
		t.Fatalf("runRoot: %v", err)
	}
	again, err := runRoot("new_project", "/checkout/a")
	if err != nil {
		t.Fatalf("runRoot: %v", err)
	}
	if first != again {
		t.Errorf("two runs of one test from one checkout got %q and %q; a stale output base at the first is unreachable from the second", first, again)
	}

	otherCheckout, err := runRoot("new_project", "/checkout/b")
	if err != nil {
		t.Fatalf("runRoot: %v", err)
	}
	if otherCheckout == first {
		t.Errorf("two checkouts share the run root %q, which is the collision this keying exists to remove", first)
	}

	otherName, err := runRoot("npm_deps", "/checkout/a")
	if err != nil {
		t.Fatalf("runRoot: %v", err)
	}
	if otherName == first {
		t.Errorf("two tests share the run root %q", first)
	}
}

// Nested output bases can exceed tmpfs capacity.
func TestRunRootFallbackIsUnderTheCacheRootAndExists(t *testing.T) {
	home := t.TempDir()
	tmpfs := t.TempDir()
	setEnv(t, map[string]string{"HOME": home, "TMPDIR": tmpfs})

	dir, err := runRoot("new_project", "/checkout/a")
	if err != nil {
		t.Fatalf("runRoot: %v", err)
	}
	if dir == "" {
		t.Fatal("runRoot() returned an empty fallback root")
	}
	root := filepath.Join(home, ".cache", "rules_typescript_it")
	if !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		t.Errorf("fallback run root %q is not under the persistent root %q", dir, root)
	}
	if strings.HasPrefix(dir, tmpfs+string(filepath.Separator)) {
		t.Errorf("fallback run root %q is under os.TempDir(), where a multi-GB output base can hit ENOSPC on a tmpfs", dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("os.Stat(%q) = %v, %v; runRoot must create the directory it names", dir, info, err)
	}
}

func TestCleanupKeepsTheOutputBase(t *testing.T) {
	base := t.TempDir()
	it := &IT{
		WorkspaceDir: filepath.Join(base, "workspace"),
		OutputBase:   filepath.Join(base, "output_base"),
		staged:       filepath.Join(base, "workspace"),
		scratchDir:   filepath.Join(base, "scratch"),
	}
	markDirs(t, it.WorkspaceDir, it.OutputBase, it.scratchDir)

	it.cleanup()

	if _, err := os.Stat(filepath.Join(it.OutputBase, "marker")); err != nil {
		t.Errorf("cleanup() removed the output base: %v", err)
	}
	for _, dir := range []string{it.WorkspaceDir, it.scratchDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("cleanup() left %s behind (err = %v)", dir, err)
		}
	}
}

// A run over the checkout stages nothing, so cleanup() removes the scratch
// directory alone: the checkout it ran in is not this run's to delete.
func TestCleanupKeepsTheCheckout(t *testing.T) {
	base := t.TempDir()
	it := &IT{
		RulesTSRoot:  filepath.Join(base, "checkout"),
		WorkspaceDir: filepath.Join(base, "checkout"),
		OutputBase:   filepath.Join(base, "output_base"),
		scratchDir:   filepath.Join(base, "scratch"),
	}
	markDirs(t, it.WorkspaceDir, it.OutputBase, it.scratchDir)

	it.cleanup()

	for _, dir := range []string{it.WorkspaceDir, it.OutputBase} {
		if _, err := os.Stat(filepath.Join(dir, "marker")); err != nil {
			t.Errorf("cleanup() removed %s: %v", dir, err)
		}
	}
	if _, err := os.Stat(it.scratchDir); !os.IsNotExist(err) {
		t.Errorf("cleanup() left %s behind (err = %v)", it.scratchDir, err)
	}
}

func markDirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, "marker")
		if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func envValue(env []string, key string) (string, int) {
	value, count := "", 0
	for _, entry := range env {
		if rest, ok := strings.CutPrefix(entry, key+"="); ok {
			value, count = rest, count+1
		}
	}
	return value, count
}

// The Bazelisk wrapper otherwise downloads Bazel again in each recreated workspace.
func TestNestedEnvSharesTheBazeliskCache(t *testing.T) {
	scratch := t.TempDir()
	setEnv(t, map[string]string{"RULES_TS_IT_SCRATCH": scratch})

	got, count := envValue(nestedEnv(), "BAZELISK_HOME")
	if want := filepath.Join(scratch, "bazelisk"); got != want {
		t.Errorf("BAZELISK_HOME = %q, want %q", got, want)
	}
	if count != 1 {
		t.Errorf("BAZELISK_HOME appears %d times, want exactly 1", count)
	}
}

// The escape hatch: a developer who already has a populated bazelisk cache, or
// a CI job that caches one elsewhere, keeps it. Only the default is ours.
func TestNestedEnvKeepsAnExplicitBazeliskHome(t *testing.T) {
	setEnv(t, map[string]string{
		"RULES_TS_IT_SCRATCH": t.TempDir(),
		"BAZELISK_HOME":       "/dev/bazelisk",
	})

	got, count := envValue(nestedEnv(), "BAZELISK_HOME")
	if got != "/dev/bazelisk" {
		t.Errorf("BAZELISK_HOME = %q, want the inherited /dev/bazelisk", got)
	}
	if count != 1 {
		t.Errorf("BAZELISK_HOME appears %d times, want exactly 1", count)
	}
}

func TestNestedEnvDropsTestTmpdir(t *testing.T) {
	setEnv(t, map[string]string{
		"TEST_TMPDIR":         t.TempDir(),
		"RULES_TS_IT_SCRATCH": t.TempDir(),
	})

	if got, count := envValue(nestedEnv(), "TEST_TMPDIR"); count != 0 {
		t.Errorf("TEST_TMPDIR = %q (%d entries), want it dropped", got, count)
	}
}
