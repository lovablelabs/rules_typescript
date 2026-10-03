package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if content == dirMarker {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadsReportNamesTheNearestPackage(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		"BUILD.bazel":   "",
		"a/BUILD.bazel": "",
		"a/b/c.txt":     "x",
		"a/d.txt":       "x",
		"e.txt":         "x",
		"f/BUILD":       "",
		"f/g.txt":       "x",
	})
	got := ReadsReport(ws, []string{"a/b/c.txt", "e.txt", "a/d.txt", "f/g.txt"})
	want := []string{
		"a/b/c.txt\t//a:b/c.txt",
		"a/d.txt\t//a:d.txt",
		"e.txt\t//:e.txt",
		"f/g.txt\t//f:g.txt",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("report = %q, want %q", got, want)
	}
}

func TestReadsReportIsSortedOnceAndFilesOnly(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		"BUILD.bazel": "",
		"a/b/c.txt":   "x",
		"a/d.txt":     "x",
		"a/e":         dirMarker,
	})
	got := ReadsReport(ws, []string{
		"a/d.txt", "a/b/c.txt", "a/d.txt", "", "a/e", "a/b", "missing.txt",
	})
	want := []string{"a/b/c.txt\t//:a/b/c.txt", "a/d.txt\t//:a/d.txt"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("report = %q, want %q", got, want)
	}
}

func readsFixture(t *testing.T, mode string) (*Resolver, map[string]string) {
	t.Helper()
	list := "_main/tests/app/a.test.js\n"
	r, real := fakeRunfiles(t, map[string]string{
		"_main/tests/app/_app.vitest/config.mjs":         "x",
		"_main/tests/app/app_test_files.txt":             list,
		"_main/tests/app/a.test.js":                      "declared test module",
		"_main/tests/app/node_modules":                   dirMarker,
		"_main/tests/app/node_modules/vitest/vitest.mjs": "x",
		"_main/ts/private/reads_hook.cjs":                "x",
		"+node+/bin/node":                                "#!/bin/sh\n",
	})
	var err error
	switch mode {
	case "directory":
		tree := strings.TrimSuffix(real["+node+/bin/node"], "/+node+/bin/node")
		t.Setenv("RUNFILES_MANIFEST_FILE", "")
		t.Setenv("RUNFILES_DIR", tree)
		r, err = newResolver(runfiles.Directory(tree))
	case "manifest_directory":
		manifest, readErr := os.ReadFile(runfilesEnv(r.Env(), "RUNFILES_MANIFEST_FILE"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		program := filepath.Join(t.TempDir(), "launcher")
		writeTree(t, program+".runfiles", map[string]string{"MANIFEST": string(manifest)})
		args := os.Args
		t.Cleanup(func() { os.Args = args })
		os.Args = []string{program}
		r, err = resolverForExecutable(program)
	}
	if err != nil {
		t.Fatal(err)
	}
	return r, real
}

func readsConfig() *Config {
	cfg := vitestConfig()
	cfg.Vitest.ReadsHook = "_main/ts/private/reads_hook.cjs"
	return cfg
}

func TestPlanVitestReadsCannotLoseDeclaredFiles(t *testing.T) {
	for _, mode := range []string{"directory", "manifest", "manifest_directory"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := readsFixture(t, mode)
			ws := t.TempDir()
			t.Setenv("BUILD_WORKSPACE_DIRECTORY", ws)
			t.Setenv("NODE_OPTIONS", "--inspect")
			cfg := readsConfig()
			cfg.RuntimeModules = []string{"_main/tests/app/a.test.js"}
			plan, err := MakePlan(cfg, r, []string{ReadsFlag}, Shard{Total: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Cleanup()
			env := plan.EnvOverrides
			tree := treeRoot(plan)
			module := filepath.Join(tree, cfg.RuntimeModules[0])
			if data, err := os.ReadFile(module); err != nil || string(data) != "declared test module" {
				t.Fatalf("staged module = %q, %v; want the declared File", data, err)
			}
			hook := "--inspect --require " + filepath.Join(tree, "_main/ts/private/reads_hook.cjs")
			if env["NODE_OPTIONS"] != hook {
				t.Errorf("NODE_OPTIONS = %q, want %q", env["NODE_OPTIONS"], hook)
			}
			if root, _ := filepath.EvalSymlinks(ws); env["TS_TEST_READS_ROOT"] != root {
				t.Errorf("TS_TEST_READS_ROOT = %q, want %q", env["TS_TEST_READS_ROOT"], root)
			}
			if got := env["TS_TEST_READS_RUNFILES"]; got != tree {
				t.Errorf("TS_TEST_READS_RUNFILES = %q, want %q", got, tree)
			}
			if _, err := os.Stat(env["TS_TEST_READS_FILE"]); err != nil {
				t.Errorf("TS_TEST_READS_FILE %q: %v", env["TS_TEST_READS_FILE"], err)
			}
			if !plan.Supervise.StdoutToStderr {
				t.Error("vitest's stdout has to leave stdout to the report")
			}
			if want := filepath.Join(tree, "_main/tests/app"); plan.Dir != want {
				t.Errorf("dir = %q, want the config's package %q", plan.Dir, want)
			}
			if plan.PostRun == nil {
				t.Fatal("no PostRun: nothing would print the report")
			}
		})
	}
}

func TestReadsReportPrintsTheRecordedFiles(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		"BUILD.bazel":          "",
		"fixtures/BUILD.bazel": "",
		"fixtures/f.txt":       "x",
	})
	record := filepath.Join(t.TempDir(), "record")
	twice := []byte("fixtures/f.txt\nfixtures/f.txt\n")
	if err := os.WriteFile(record, twice, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	x := &readsRun{workspace: ws, record: record}
	if err := x.report(&out)(0); err != nil {
		t.Fatal(err)
	}
	if want := "fixtures/f.txt\t//fixtures:f.txt\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	empty := &readsRun{workspace: ws, record: filepath.Join(t.TempDir(), "none")}
	out.Reset()
	if err := empty.report(&out)(0); err != nil || out.Len() != 0 {
		t.Errorf("an empty record printed %q, err %v", out.String(), err)
	}
}

func TestPlanVitestReadsNeedsBazelRun(t *testing.T) {
	r, _ := readsFixture(t, "directory")
	t.Setenv("BUILD_WORKSPACE_DIRECTORY", "")
	_, err := MakePlan(readsConfig(), r, []string{ReadsFlag}, Shard{Total: 1})
	if err == nil || !strings.Contains(err.Error(), "bazel run") {
		t.Errorf("err = %v, want one naming `bazel run`", err)
	}
}

// The hook is in every vitest test's config; only the flag installs it.
func TestPlanVitestLeavesTheReadsHookOutWithoutTheFlag(t *testing.T) {
	r, _ := readsFixture(t, "directory")
	t.Setenv("NODE_OPTIONS", "")
	plan, err := MakePlan(readsConfig(), r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	hook := filepath.Join(treeRoot(plan), "_main/ts/private/reads_hook.cjs")
	options := plan.EnvOverrides["NODE_OPTIONS"]
	if strings.Contains(options, hook) {
		t.Errorf("NODE_OPTIONS = %q, want no hook without %s", options,
			ReadsFlag)
	}
}

// The launcher consumes --reads; every other argument is vitest's.
func TestPlanVitestHandsTheOtherArgumentsToVitest(t *testing.T) {
	r, _ := readsFixture(t, "directory")
	t.Setenv("BUILD_WORKSPACE_DIRECTORY", t.TempDir())
	args := []string{ReadsFlag, "--reporter=dot"}
	plan, err := MakePlan(readsConfig(), r, args, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	if slices.Contains(plan.Argv, ReadsFlag) {
		t.Errorf("argv = %q, want %s consumed", plan.Argv, ReadsFlag)
	}
	if !slices.Contains(plan.Argv, "--reporter=dot") {
		t.Errorf("argv = %q, want --reporter=dot handed to vitest", plan.Argv)
	}
}

func TestPlanVitestReadsReleasesUnownedFiles(t *testing.T) {
	for _, outcome := range []string{"missing list", "missing Vitest", "invalid hook", "empty shard", "success"} {
		t.Run(outcome, func(t *testing.T) {
			r, _ := readsFixture(t, "directory")
			t.Setenv("BUILD_WORKSPACE_DIRECTORY", t.TempDir())
			cfg := readsConfig()
			shard := Shard{Total: 1}
			switch outcome {
			case "missing list":
				cfg.Vitest.TestFilesList = "_main/missing.txt"
			case "missing Vitest":
				cfg.Vitest.VitestInTree = "vitest/missing.mjs"
			case "invalid hook":
				cfg.Vitest.ReadsHook = "../missing.cjs"
			case "empty shard":
				shard = Shard{Index: 1, Total: 2}
			}
			scratch := t.TempDir()
			for _, name := range []string{"TMPDIR", "TMP", "TEMP", "TEST_TMPDIR"} {
				t.Setenv(name, scratch)
			}
			plan, err := MakePlan(cfg, r, []string{ReadsFlag}, shard)
			wantSuccess := outcome == "success" || outcome == "empty shard"
			if (err == nil) != wantSuccess {
				t.Fatalf("unexpected plan error: %v", err)
			}
			if outcome == "empty shard" && !plan.ExitEarly {
				t.Fatal("empty partition must exit before starting Vitest")
			}
			if outcome == "success" {
				for _, key := range []string{"TS_TEST_FILES_ROOT", "TS_TEST_READS_FILE"} {
					if _, err := os.Stat(plan.EnvOverrides[key]); err != nil {
						t.Fatalf("successful plan lost %s: %v", key, err)
					}
				}
				plan.Cleanup()
			}
			if files, err := os.ReadDir(scratch); err != nil || len(files) != 0 {
				t.Fatalf("planning left files behind: %v, %v", files, err)
			}
		})
	}
}
