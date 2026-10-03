package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func nodeTestFixture(t *testing.T) (*Resolver, map[string]string) {
	t.Helper()
	t.Setenv("TEST_TMPDIR", t.TempDir())
	t.Setenv("TESTBRIDGE_TEST_ONLY", "")
	_, real := fakeRunfiles(t, map[string]string{
		"_main/tests/app/app_test_files.txt": strings.Join([]string{
			"_main/tests/app/a.test.js",
			"_main/tests/app/b.test.js",
			"_main/tests/app/c.test.js",
		}, "\n"),
		"_main/tests/app/a.test.js":                 "x",
		"_main/tests/app/b.test.js":                 "x",
		"_main/tests/app/c.test.js":                 "x",
		"_main/tests/app/node_modules/zod/index.js": "x",
		"_main/ts/private/node_test_hook.mjs":       "export {}",
		"+node+/bin/node":                           "#!/bin/sh\n",
	})
	r, _ := withRunfilesDir(t, real["_main/tests/app/a.test.js"], "_main/tests/app/a.test.js")
	return r, real
}

func nodeTestConfig() *Config {
	return &Config{
		Label:     "//tests/app:app_test",
		Mode:      ModeNodeTest,
		Workspace: "_main",
		Runtime:   "+node+/bin/node",
		RuntimeModules: []string{
			"_main/tests/app/a.test.js",
			"_main/tests/app/b.test.js",
			"_main/tests/app/c.test.js",
		},
		NodeTest: &NodeTestConfig{
			TestFilesList: "_main/tests/app/app_test_files.txt",
			NodeModules:   []string{"_main/tests/app/node_modules"},
			ResolveHook:   "_main/ts/private/node_test_hook.mjs",
		},
	}
}

func TestPlanNodeTestRunsEveryTestFileUnderTheToolchainNode(t *testing.T) {
	r, real := nodeTestFixture(t)
	plan, err := fixturePlan(t, nodeTestConfig(), r, real, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plan.Cleanup)
	want := []string{
		real["+node+/bin/node"],
		"--import", filepath.Join(plan.Dir, "_main/ts/private/node_test_hook.mjs"),
		"--test",
		filepath.Join(plan.Dir, "_main/tests/app/a.test.js"),
		filepath.Join(plan.Dir, "_main/tests/app/b.test.js"),
		filepath.Join(plan.Dir, "_main/tests/app/c.test.js"),
	}
	if !slices.Equal(plan.Argv, want) {
		t.Errorf("argv = %q, want %q", plan.Argv, want)
	}
	if !plan.UseExec {
		t.Error("node:test must preserve exec identity")
	}
	if runfilesEnv(plan.runfilesEnv, "TEST_SRCDIR") != "" {
		t.Error("the directory resolver created test context absent from the caller")
	}
}

func TestNativeViewCannotLoseBorrowedRootsOrPackageScope(t *testing.T) {
	for _, tc := range []struct{ launcher, layout string }{
		{ModeNodeTest, "manifest"},
		{ModeNodeTest, "manifest_with_tree"},
		{ModeNodeTest, "manifest_directory"},
		{ModeNodeTest, "directory"},
		{ModeNode, "manifest"},
		{ModeNode, "manifest_with_tree"},
		{ModeNode, "manifest_directory"},
		{ModeNode, "directory"},
	} {
		t.Run(tc.launcher+"/"+tc.layout, func(t *testing.T) {
			launcher, mode := tc.launcher, tc.layout
			directory := mode == "directory"
			_, real := nodeTestFixture(t)
			const local = "_main/tests/app/a.test.js"
			const foreign = "_main/shared/foreign.test.js"
			const foreignAlias = "_main/tests/app/b.test.js"
			const helper = "_main/shared/helper.js"
			const scope = "_main/shared/package.json"
			const fixture = "_main/shared/fixture.txt"
			const dataTree = "_main/tests/app/fixtures.json"
			const linkedJSON = "_main/tests/app/data.json"
			const linkedJS = "_main/tests/app/data.js"
			const danglingJSON = "_main/tests/app/missing.json"
			const danglingJS = "_main/tests/app/missing.js"
			const npmAlias = "_main/tests/app/node_modules/alias"
			cfg := nodeTestConfig()
			cfg.Runtime = "+node+/bin/wrapper.js"
			cfg.RuntimeModules = []string{local, foreign, helper}
			hook := cfg.NodeTest.ResolveHook
			commandPaths := []string{hook, local, foreign}
			if launcher == ModeNode {
				cfg.Mode = ModeNode
				cfg.Node = &NodeConfig{Entry: foreign, NodeModules: cfg.NodeTest.NodeModules[0]}
				cfg.NodeTest = nil
				commandPaths = []string{foreign}
			}
			copied := map[string]string{local: real[local], foreign: real[foreignAlias], helper: real["_main/tests/app/c.test.js"]}
			linked := []string{cfg.Runtime, hook, scope, fixture, dataTree, "_main/tests/app/node_modules/zod/index.js"}
			moduleAliases := map[string][]string{
				local: {linkedJSON, linkedJS}, foreign: {foreignAlias}, helper: {"_main/tests/app/c.test.js"},
			}
			list := real["_main/tests/app/app_test_files.txt"]
			if err := os.WriteFile(list, []byte(local+"\n"+foreign+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			entries := map[string]string{}
			for path, target := range real {
				entries[path] = target
			}
			entries[cfg.Runtime] = real[nodeTestConfig().Runtime]
			entries[npmAlias] = "zod"
			entries[foreign] = real[foreignAlias]
			entries[helper] = real["_main/tests/app/c.test.js"]
			entries[scope] = filepath.Join(t.TempDir(), "package.json")
			entries[fixture] = filepath.Join(t.TempDir(), "fixture.txt")
			entries[dataTree] = t.TempDir()
			entries[linkedJSON] = "a.test.js"
			entries[linkedJS] = "a.test.js"
			entries[danglingJSON] = "absent.json"
			entries[danglingJS] = "absent.js"
			if err := os.WriteFile(filepath.Join(entries[dataTree], "note.txt"), []byte("opaque data"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entries[scope], []byte(`{"type":"module","imports":{"#helper":"./helper.js"}}`), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entries[fixture], []byte("declared adjacent data"), 0o644); err != nil {
				t.Fatal(err)
			}
			lines := []string{}
			for path, target := range entries {
				lines = append(lines, path+" "+target)
			}
			r := manifestResolver(t, lines)
			if mode != "manifest" {
				r.dir = t.TempDir()
				if err := os.WriteFile(filepath.Join(r.dir, "MANIFEST"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if directory || mode == "manifest_with_tree" {
				for path, target := range entries {
					link := filepath.Join(r.dir, filepath.FromSlash(path))
					if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
						t.Fatal(err)
					}
					if path == scope {
						var err error
						target, err = filepath.Rel(filepath.Dir(link), target)
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Symlink(target, link); err != nil {
						t.Fatal(err)
					}
				}
			}
			if directory {
				var err error
				r, err = directoryResolver(r.dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("RUNFILES_MANIFEST_ONLY", "1")
			t.Setenv("TEST_SRCDIR", t.TempDir())
			originalDir := r.Dir()
			originalEnv := r.Env()
			t.Setenv("TEST_TMPDIR", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			plan, err := fixturePlan(t, cfg, r, entries, []string{"argument with spaces"}, Shard{Total: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.cleanup) != 0 {
				t.Cleanup(plan.Cleanup)
			}
			if r.Dir() != originalDir || !slices.Equal(r.Env(), originalEnv) {
				t.Fatal("staging mutated the original resolver")
			}
			root := fixtureRuntimeRoot(t, cfg, r, plan)
			if cfg.Mode == ModeNode && plan.Dir != "" {
				t.Fatalf("binary changed the caller working directory to %q", plan.Dir)
			}
			if cfg.Mode == ModeNodeTest && plan.Dir != root {
				t.Fatalf("node:test working directory %q differs from runtime root %q", plan.Dir, root)
			}
			if cfg.Mode == ModeNode {
				runtime, err := r.Path(cfg.Runtime)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{runtime, filepath.Join(root, foreign), "argument with spaces"}
				if !slices.Equal(plan.Argv, want) {
					t.Fatalf("binary arguments = %q, want %q", plan.Argv, want)
				}
			}
			if root == originalDir {
				t.Fatalf("selected backend used the wrong tree: %s -> %s", originalDir, root)
			}
			for _, path := range commandPaths {
				if !slices.Contains(plan.Argv, filepath.Join(root, filepath.FromSlash(path))) {
					t.Errorf("argv %q lost logical runtime path %s", plan.Argv, path)
				}
			}
			for path := range copied {
				staged := filepath.Join(root, filepath.FromSlash(path))
				got, err := filepath.EvalSymlinks(staged)
				if err != nil || got != staged {
					t.Errorf("module %s lost its logical identity: %q, %v", path, got, err)
				}
				info, err := os.Lstat(staged)
				if err != nil || !info.Mode().IsRegular() {
					t.Fatalf("module %s is not a regular file: %v, %v", path, info, err)
				}
			}
			for _, path := range linked {
				staged := filepath.Join(root, filepath.FromSlash(path))
				if info, err := os.Lstat(staged); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("non-module %s lost its authority link: %v, %v", path, info, err)
				}
				actual, err := os.Stat(staged)
				want, sourceErr := os.Stat(entries[path])
				if err != nil || sourceErr != nil || actual.IsDir() != want.IsDir() || os.SameFile(actual, want) {
					t.Fatalf("non-module %s lost its copied authority: %v, %v", path, err, sourceErr)
				}
				if !want.IsDir() {
					got, err := os.ReadFile(staged)
					want, sourceErr := os.ReadFile(entries[path])
					if err != nil || sourceErr != nil || string(got) != string(want) {
						t.Fatalf("non-module %s changed declared bytes: %q, %v, %v", path, got, err, sourceErr)
					}
				}
			}
			aliases := map[string]string{npmAlias: "_main/tests/app/node_modules/zod"}
			for canonical, names := range moduleAliases {
				for _, alias := range names {
					aliases[alias] = canonical
				}
			}
			for alias, canonical := range aliases {
				link := filepath.Join(root, filepath.FromSlash(alias))
				if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("alias %s is not a symlink: %v, %v", alias, info, err)
				}
				actual, err := os.Stat(link)
				want, targetErr := os.Stat(filepath.Join(root, filepath.FromSlash(canonical)))
				if err != nil || targetErr != nil || !os.SameFile(actual, want) {
					t.Fatalf("alias %s lost canonical File %s: %v, %v", alias, canonical, err, targetErr)
				}
			}
			for _, name := range []string{danglingJSON, danglingJS} {
				link := filepath.Join(root, filepath.FromSlash(name))
				got, err := os.Readlink(link)
				want := filepath.Join(filepath.Dir(link), entries[name])
				if err != nil || filepath.Clean(filepath.Join(filepath.Dir(link), got)) != want {
					t.Fatalf("dangling alias %s changed destination: %q, %v", name, got, err)
				}
				if _, err := os.Stat(link); !os.IsNotExist(err) {
					t.Fatalf("dangling alias %s unexpectedly resolves: %v", name, err)
				}
			}
			if got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dataTree), "note.txt")); err != nil || string(got) != "opaque data" {
				t.Fatalf("module-shaped data directory lost its child: %q, %v", got, err)
			}
			entry, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(foreign)))
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"package.json": `{"type":"module","imports":{"#helper":"./helper.js"}}`, "fixture.txt": "declared adjacent data"} {
				if got, err := os.ReadFile(filepath.Join(filepath.Dir(entry), name)); err != nil || string(got) != want {
					t.Fatalf("resolved entry lost adjacent %s: %q, %v", name, got, err)
				}
			}
			for path, original := range copied {
				staged := filepath.Join(root, filepath.FromSlash(path))
				before, err := os.ReadFile(original)
				if err != nil {
					t.Fatal(err)
				}
				originalInfo, err := os.Stat(original)
				if err != nil {
					t.Fatal(err)
				}
				stagedInfo, err := os.Stat(staged)
				if err != nil || stagedInfo.Mode().Perm() != originalInfo.Mode().Perm() {
					t.Fatalf("staged %s changed file permissions: %v", path, err)
				}
				if got, err := os.ReadFile(staged); err != nil || string(got) != string(before) {
					t.Fatalf("staged %s changed source bytes: %q, %v", path, got, err)
				}
				if err := os.WriteFile(staged, []byte("test mutation"), 0o644); err != nil {
					t.Fatal(err)
				}
				for _, alias := range moduleAliases[path] {
					if got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(alias))); err != nil || string(got) != "test mutation" {
						t.Fatalf("alias %s and canonical module hold separate bytes: %q, %v", alias, got, err)
					}
				}
				if got, err := os.ReadFile(original); err != nil || string(got) != string(before) {
					t.Fatalf("staged write changed source %s: %q, %v", path, got, err)
				}
			}
			for _, flag := range []string{"--preserve-symlinks", "--preserve-symlinks-main"} {
				if slices.Contains(plan.Argv, flag) {
					t.Errorf("argv changes native npm identity: %q", plan.Argv)
				}
			}
			if runfilesEnv(plan.runfilesEnv, "RUNFILES_DIR") != root || runfilesEnv(plan.runfilesEnv, "RUNFILES_MANIFEST_FILE") != "" {
				t.Fatalf("native child lookup does not select its immutable view: %q", plan.runfilesEnv)
			}
			if !plan.UseExec || len(plan.cleanup) != 0 {
				t.Fatalf("native plan owns mutable resources: exec=%t cleanup=%v", plan.UseExec, plan.cleanup)
			}
			plan.Cleanup()
			if _, err := os.Stat(root); err != nil {
				t.Errorf("native cleanup removed immutable view: %v", err)
			}
			if _, err := os.Stat(entries[scope]); err != nil {
				t.Errorf("cleanup removed original scope: %v", err)
			}
			if originalDir != "" {
				if _, err := os.Stat(originalDir); err != nil {
					t.Errorf("cleanup removed original runfiles directory: %v", err)
				}
			}
		})
	}
}

// The hook has to reach the children node --test spawns, which inherit the
// parent's execArgv but not a flag the launcher appended after the file list.
func TestPlanNodeTestPutsTheResolveHookBeforeTheTestFlag(t *testing.T) {
	r, real := nodeTestFixture(t)
	plan, err := fixturePlan(t, nodeTestConfig(), r, real, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	hook := slices.Index(plan.Argv, "--import")
	test := slices.Index(plan.Argv, "--test")
	if hook < 0 || test < 0 || hook > test {
		t.Errorf("argv = %q, want --import before --test", plan.Argv)
	}
}

// node reads its flags up to the first file, so the target's args go between
// the launcher's own flags and --test, where the children inherit them too.
func TestPlanNodeTestPutsArgsBeforeTheTestFlag(t *testing.T) {
	r, real := nodeTestFixture(t)
	args := []string{"--experimental-test-module-mocks"}
	plan, err := fixturePlan(t, nodeTestConfig(), r, real, args, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	flag := slices.Index(plan.Argv, args[0])
	test := slices.Index(plan.Argv, "--test")
	hook := slices.Index(plan.Argv, "--import")
	if flag < 0 || !(hook < flag && flag < test) {
		t.Errorf("argv = %q, want %q between --import and --test", plan.Argv, args[0])
	}
	if slices.Index(plan.Argv, filepath.Join(plan.Dir, "_main/tests/app/a.test.js")) < test {
		t.Errorf("argv = %q, want the files after --test", plan.Argv)
	}
}

func TestPlanNodeTestOmitsTheHookWhenTheConfigHasNone(t *testing.T) {
	r, real := nodeTestFixture(t)
	cfg := nodeTestConfig()
	cfg.NodeTest.ResolveHook = ""
	plan, err := fixturePlan(t, cfg, r, real, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plan.Argv, "--import") {
		t.Errorf("argv = %q, want no --import", plan.Argv)
	}
	if slices.Contains(plan.Argv, "--preserve-symlinks") {
		t.Errorf("argv = %q: the launcher must not change symlink handling without runtime_args", plan.Argv)
	}
}

func TestPlanNodeTestPartitionsShards(t *testing.T) {
	r, real := nodeTestFixture(t)
	plan, err := fixturePlan(t, nodeTestConfig(), r, real, nil, Shard{Index: 1, Total: 2})
	if err != nil {
		t.Fatal(err)
	}
	files := plan.Argv[slices.Index(plan.Argv, "--test")+1:]
	if !slices.Equal(files, []string{filepath.Join(plan.Dir, "_main/tests/app/b.test.js")}) {
		t.Errorf("shard 1/2 files = %q; want only b.test.js", files)
	}
}

func TestPlanNodeTestExitsCleanlyOnAnEmptyShard(t *testing.T) {
	r, real := nodeTestFixture(t)
	plan, err := fixturePlan(t, nodeTestConfig(), r, real, nil, Shard{Index: 7, Total: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.ExitEarly {
		t.Fatal("an empty shard must not start node")
	}
}

func TestPlanNodeTestForwardsTestFilterAsANamePattern(t *testing.T) {
	r, real := nodeTestFixture(t)
	t.Setenv("TESTBRIDGE_TEST_ONLY", "bumps twice")
	plan, err := fixturePlan(t, nodeTestConfig(), r, real, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(plan.Argv, "--test-name-pattern")
	if i < 0 || plan.Argv[i+1] != "bumps twice" {
		t.Errorf("argv = %q, want --test-name-pattern 'bumps twice'", plan.Argv)
	}
}

func TestPlanNodeTestOmitsTheNamePatternWithoutAFilter(t *testing.T) {
	r, real := nodeTestFixture(t)
	plan, err := fixturePlan(t, nodeTestConfig(), r, real, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plan.Argv, "--test-name-pattern") {
		t.Errorf("argv = %q, want no --test-name-pattern", plan.Argv)
	}
}

// Silently handing Bazel an empty lcov would report full coverage loss as a
// clean run, so the plan refuses instead.
func TestPlanNodeTestRefusesACoverageRun(t *testing.T) {
	r, real := nodeTestFixture(t)
	t.Setenv("COVERAGE_DIR", t.TempDir())
	_, err := fixturePlan(t, nodeTestConfig(), r, real, nil, Shard{Total: 1})
	if err == nil || !strings.Contains(err.Error(), "does not report coverage") {
		t.Fatalf("want a coverage refusal naming the runner, got %v", err)
	}
}

func TestPlanNodeTestNamesTheImportersNodeModulesOnNodePath(t *testing.T) {
	r, real := nodeTestFixture(t)
	plan, err := fixturePlan(t, nodeTestConfig(), r, real, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	const importer = "/_main/tests/app/node_modules"
	nodePath := plan.EnvOverrides["NODE_PATH"]
	dir := strings.Split(nodePath, string(os.PathListSeparator))[0]
	if !strings.HasSuffix(dir, filepath.FromSlash(importer)) {
		t.Fatalf("NODE_PATH = %q, want the importer's staged directory first", dir)
	}
	got, err := filepath.EvalSymlinks(filepath.Join(dir, "zod", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	zod := real["_main/tests/app/node_modules/zod/index.js"]
	if data, err := os.ReadFile(got); err != nil || string(data) != "x" {
		t.Fatalf("npm input %s lost bytes: %q, %v", zod, data, err)
	}
	root := strings.TrimSuffix(dir, filepath.FromSlash(importer))
	link := filepath.Join(root, "_main", "node_modules")
	if target, err := filepath.EvalSymlinks(link); err != nil || target != dir {
		t.Errorf("%s -> %q, %v; want the importer's %q", link, target, err, dir)
	}
}

func TestParseConfigRejectsANodeTestSectionWithoutAFileList(t *testing.T) {
	_, err := ParseConfig([]byte(`{"mode":"node_test","node_test":{}}`))
	if err == nil || !strings.Contains(err.Error(), "test_files_list") {
		t.Fatalf("want an error naming test_files_list, got %v", err)
	}
}

func TestRunAdvertisesShardingOnlyWhenExecuting(t *testing.T) {
	for _, mode := range []string{"run", "dump flag", "dump environment"} {
		for _, badPath := range []bool{false, true} {
			name := mode
			if badPath {
				name += " with invalid status path"
			}
			t.Run(name, func(t *testing.T) {
				r, real := nodeTestFixture(t)
				cfg := nodeTestConfig()
				if err := fixtureNativeView(t, cfg, r, real); err != nil {
					t.Fatal(err)
				}
				config, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				path, err := r.Path(cfg.NativeViewAnchor)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, config, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Setenv(ConfigEnvVar, path)
				t.Setenv("COVERAGE_DIR", "")
				t.Setenv("TEST_TOTAL_SHARDS", "8")
				t.Setenv("TEST_SHARD_INDEX", "7")
				status := filepath.Join(t.TempDir(), "status")
				if badPath {
					status = filepath.Join(status, "missing")
				}
				t.Setenv("TEST_SHARD_STATUS_FILE", status)
				t.Setenv(DumpEnvVar, "")
				args := os.Args
				t.Cleanup(func() { os.Args = args })
				os.Args = []string{strings.TrimSuffix(path, ".json")}
				if mode == "dump flag" {
					os.Args = append(os.Args, DumpFlag)
				} else if mode == "dump environment" {
					t.Setenv(DumpEnvVar, "1")
				}
				want := 0
				if mode == "run" && badPath {
					want = 1
				}
				if got := run(); got != want {
					t.Fatalf("run() = %d, want %d", got, want)
				}
				_, err = os.Stat(status)
				if mode == "run" && !badPath {
					if err != nil {
						t.Fatal(err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("unexpected status file: %v", err)
				}
			})
		}
	}
}
