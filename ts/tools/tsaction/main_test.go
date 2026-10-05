package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainProcess(t *testing.T) {
	if os.Getenv("TSACTION_TEST_PROCESS") != "1" {
		return
	}
	os.Args = append(os.Args[:1], os.Args[3:]...)
	main()
	os.Exit(0)
}

func runMainProcess(t *testing.T, args ...string) (string, int) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, append([]string{"-test.run=^TestMainProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "TSACTION_TEST_PROCESS=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return string(out), exit.ExitCode()
}

func TestMain_CapturedCompilerDiagnosticsAreNotLostAtExit(t *testing.T) {
	e := newExecroot(t, chainLeaf, "")
	writeFile(t, e.tsgo, "#!/bin/sh\necho 'error TS5023: Unknown compiler option x.'\necho 'compiler stderr detail' >&2\nexit 2\n")
	out, code := runMainProcess(t, append([]string{"tsconfig"}, e.tsconfigArgs()...)...)
	if code != 2 {
		t.Errorf("exit = %d, want 2; output: %s", code, out)
	}
	for _, diagnostic := range []string{"TS5023", "compiler stderr detail", "--showConfig"} {
		if strings.Count(out, diagnostic) != 1 {
			t.Errorf("output must contain %q exactly once: %s", diagnostic, out)
		}
	}
}

func TestMain_StreamedCompilerDiagnosticsAreNotDuplicated(t *testing.T) {
	root := t.TempDir()
	tool, _ := fakeTool(t, root, "compiler", "echo 'streamed compiler diagnostic' >&2\nexit 17\n")
	stamp := filepath.Join(root, "success")
	out, code := runMainProcess(t, "stamp", "-stamp="+stamp, "--", tool)
	if code != 17 || strings.Count(out, "streamed compiler diagnostic") != 1 {
		t.Errorf("exit = %d, output = %q; want exit 17 and one diagnostic", code, out)
	}
	if _, err := os.Stat(stamp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed command wrote success stamp: %v", err)
	}
}

func TestMain_SuccessDoesNotReportAnError(t *testing.T) {
	root := t.TempDir()
	tool, _ := fakeTool(t, root, "compiler", "exit 0\n")
	stamp := filepath.Join(root, "success")
	out, code := runMainProcess(t, "stamp", "-stamp="+stamp, "--", tool)
	if code != 0 || out != "" {
		t.Errorf("exit = %d, output = %q; want silent success", code, out)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Fatal(err)
	}
}

func TestStage_PackageDirectoryCopiesWholeTreeAtDot(t *testing.T) {
	pkg := filepath.Join(t.TempDir(), "pkg")
	for _, dir := range []string{"lib/deep", "empty"} {
		if err := os.MkdirAll(filepath.Join(pkg, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, text := range map[string]string{"package.json": "{}", "lib/deep/index.js": "x"} {
		if err := os.WriteFile(filepath.Join(pkg, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("lib/deep/index.js", filepath.Join(pkg, "main.js")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "tree")
	if err := stage([]string{"-out=" + out, pkg, "."}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"package.json": "{}", "lib/deep/index.js": "x", "main.js": "x"} {
		if got, err := os.ReadFile(filepath.Join(out, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if info, err := os.Stat(filepath.Join(out, "empty")); err != nil || !info.IsDir() {
		t.Errorf("empty directory not staged: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(out, "main.js")); err != nil || target != "lib/deep/index.js" {
		t.Errorf("in-package link = %q, %v; want it kept as lib/deep/index.js", target, err)
	}
}

func TestStage_LinkLeavingThePackageOrLoopingFails(t *testing.T) {
	for name, links := range map[string]map[string]string{
		"escape":   {"evil": "../outside"},
		"absolute": {"evil": "/etc/passwd"},
		"loop":     {"a": "b", "b": "a"},
	} {
		t.Run(name, func(t *testing.T) {
			pkg := filepath.Join(t.TempDir(), "pkg")
			if err := os.MkdirAll(pkg, 0o755); err != nil {
				t.Fatal(err)
			}
			for link, target := range links {
				if err := os.Symlink(target, filepath.Join(pkg, link)); err != nil {
					t.Fatal(err)
				}
			}
			if err := stage([]string{"-out=" + filepath.Join(t.TempDir(), "tree"), pkg, "."}); err == nil {
				t.Error("stage succeeded")
			}
		})
	}
}
