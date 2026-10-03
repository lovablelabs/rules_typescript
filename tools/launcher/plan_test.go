package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/mikn/rules_typescript/ts/tools/runtimeview"
)

func TestPlanNodeExecsTheToolchainRuntime(t *testing.T) {
	r, real := fakeRunfiles(t, map[string]string{
		"_main/tests/app/main.js": "x",
		"+node+/bin/node":         "#!/bin/sh\n",
	})
	cfg := &Config{
		Label:   "//tests/app:app",
		Mode:    ModeNode,
		Runtime: "+node+/bin/node",
		RunArgs: []string{"--experimental-vm-modules"},
		Node:    &NodeConfig{Entry: "_main/tests/app/main.js"},
	}
	plan, err := fixturePlan(t, cfg, r, real, []string{"--flag", "a b"}, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		real["+node+/bin/node"], "--experimental-vm-modules",
		filepath.Join(fixtureRuntimeRoot(t, cfg, r, plan), cfg.Node.Entry), "--flag", "a b",
	}
	if strings.Join(plan.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", plan.Argv, want)
	}
	if !plan.UseExec {
		t.Error("an npm CLI without temporary resources should exec")
	}
	if plan.Dir != "" {
		t.Errorf("npm CLI changed the caller working directory to %q", plan.Dir)
	}
}

func TestNativePlansKeepStandardLookupInTheBuiltView(t *testing.T) {
	for _, mode := range []string{ModeNode, ModeNodeTest} {
		for _, layout := range []string{"directory", "manifest"} {
			t.Run(mode+"/"+layout, func(t *testing.T) {
				const entry = "_main/app/main.js"
				r, real := fakeRunfiles(t, map[string]string{
					entry:                  "export {};",
					"_main/test_files.txt": entry + "\n",
					"_repo_mapping":        ",fixtures,fixtures+\n",
					"fixtures+/value.txt":  "external fixture",
				})
				if layout == "directory" {
					r, _ = withRunfilesDir(t, real[entry], entry)
				}
				cfg := &Config{
					Mode:           mode,
					Workspace:      "_main",
					RuntimeModules: []string{entry},
					Node:           &NodeConfig{Entry: entry},
					NodeTest:       &NodeTestConfig{TestFilesList: "_main/test_files.txt"},
				}
				if err := fixtureNativeView(t, cfg, r, real); err != nil {
					t.Fatal(err)
				}
				view := fixtureRuntimeRoot(t, cfg, r, nil)
				plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
				if err != nil {
					t.Fatal(err)
				}
				defer plan.Cleanup()
				if got, want := plan.Argv[len(plan.Argv)-1], filepath.Join(view, entry); got != want {
					t.Fatalf("native entry = %q, want %q", got, want)
				}
				childEnv := Environ(plan.EnvOverrides, plan.runfilesEnv)
				for _, key := range []string{"RUNFILES_DIR", "JAVA_RUNFILES", "RUNFILES_MANIFEST_FILE", "RUNFILES_MANIFEST_ONLY", "TEST_SRCDIR"} {
					t.Setenv(key, runfilesEnv(childEnv, key))
				}
				child, err := runfiles.New(runfiles.SourceRepo(""))
				if err != nil {
					t.Fatal(err)
				}
				module, err := child.Rlocation(entry)
				if err != nil || module != filepath.Join(view, entry) {
					t.Fatalf("child module lookup = %q, %v; want native view entry", module, err)
				}
				path, err := child.Rlocation("fixtures/value.txt")
				if err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != "external fixture" {
					t.Fatalf("apparent repository lookup = %q, %v; path %s", got, err, path)
				}
			})
		}
	}
}

func TestPlanNodeFallsBackToSystemNode(t *testing.T) {
	r, real := fakeRunfiles(t, map[string]string{"_main/a.js": "x"})
	cfg := &Config{Mode: ModeNode, Node: &NodeConfig{Entry: "_main/a.js"}}
	plan, err := fixturePlan(t, cfg, r, real, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Argv[0] != "node" {
		t.Errorf("argv[0] = %q, want the system node fallback", plan.Argv[0])
	}
}

func TestPlanNodeAddsNodeModulesToNodePath(t *testing.T) {
	r, real := fakeRunfiles(t, map[string]string{
		"_main/a.js":                            "x",
		"_main/tests/node_modules/zod/index.js": "x",
	})
	t.Setenv("NODE_PATH", "/pre-existing")
	cfg := &Config{Mode: ModeNode, Node: &NodeConfig{
		Entry:       "_main/a.js",
		NodeModules: "_main/tests/node_modules",
	}}
	plan, err := fixturePlan(t, cfg, r, real, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	nodePath := plan.EnvOverrides["NODE_PATH"]
	entries := strings.Split(nodePath, string(os.PathListSeparator))
	importer := filepath.FromSlash("/_main/tests/node_modules")
	if len(entries) != 2 || entries[1] != "/pre-existing" ||
		!strings.HasSuffix(entries[0], importer) {
		t.Fatalf("NODE_PATH = %q, want the staged importer before /pre-existing",
			nodePath)
	}
	got, err := filepath.EvalSymlinks(filepath.Join(entries[0], "zod", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	zod := real["_main/tests/node_modules/zod/index.js"]
	if data, err := os.ReadFile(got); err != nil || string(data) != "x" {
		t.Fatalf("selected npm bytes lost: %q, %v (input %s)", data, err, zod)
	}
}

func npmContextFixture(t *testing.T, mode, module string, contexts []NpmContext, files map[string]string) (*Config, *Resolver, map[string]string) {
	t.Helper()
	files["_main/tests.txt"] = module + "\n"
	files["_main/app/config.mjs"] = "export default {}"
	files["_main/node_modules/vitest/vitest.mjs"] = "vitest"
	r, original := fakeRunfiles(t, files)
	cfg := &Config{Mode: mode, Workspace: "_main", RuntimeModules: []string{module}}
	switch mode {
	case ModeNode:
		cfg.Node = &NodeConfig{Entry: module}
	case ModeNodeTest:
		cfg.NodeTest = &NodeTestConfig{TestFilesList: "_main/tests.txt"}
	case ModeVitest:
		cfg.Vitest = &VitestConfig{
			TestFilesList: "_main/tests.txt", ConfigFile: "_main/app/config.mjs",
			VitestInTree: "vitest/vitest.mjs", NodeModules: []string{"_main/node_modules"},
			NpmContexts: contexts,
		}
	}
	return cfg, r, original
}

func TestPlanKeepsSourceNpmStoresForEveryRuntimeMode(t *testing.T) {
	for _, tc := range []struct{ mode, extension string }{
		{ModeNode, "js"}, {ModeNodeTest, "js"}, {ModeVitest, "js"}, {ModeVitest, "ts"},
	} {
		t.Run(tc.mode+"/"+tc.extension, func(t *testing.T) {
			module := "_main/app/main." + tc.extension
			context := NpmContext{Module: module, Source: "shared/main.ts", Bindings: map[string]string{"pkg": "_main/store/pkg"}}
			cfg, r, original := npmContextFixture(t, tc.mode, module, []NpmContext{context, context}, map[string]string{
				module:                    "import { createRequire } from './factory.js'; createRequire(import.meta.url)('pkg');",
				"_main/node_modules/pkg":  dirMarker,
				"_main/store/pkg":         dirMarker,
				"_main/app/unrelated.txt": "ordinary data",
			})
			plan, err := fixturePlan(t, cfg, r, original, nil, Shard{Total: 1}, context, context)
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Cleanup()
			root := fixtureRuntimeRoot(t, cfg, r, plan)
			wantData, err := os.ReadFile(original["_main/app/unrelated.txt"])
			if err != nil {
				t.Fatal(err)
			}
			gotData, err := os.ReadFile(filepath.Join(root, "_main/app/unrelated.txt"))
			if err != nil || string(gotData) != string(wantData) {
				t.Fatalf("unrelated data provenance lost: %q, %v", gotData, err)
			}
			for path, store := range map[string]string{
				"_main/app/node_modules/pkg": "_main/store/pkg",
			} {
				got, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
				want, sourceErr := os.Stat(filepath.Join(root, filepath.FromSlash(store)))
				if err != nil || sourceErr != nil || !os.SameFile(got, want) {
					t.Fatalf("%s lost source File %s: %v, %v", path, store, err, sourceErr)
				}
			}
		})
	}
}

func TestPlanKeepsDistinctModuleAndPackageScopeNpmStores(t *testing.T) {
	for _, mode := range []string{ModeNode, ModeNodeTest, ModeVitest} {
		for _, scopeOnly := range []bool{false, true} {
			t.Run(mode+"/scopeOnly="+strconv.FormatBool(scopeOnly), func(t *testing.T) {
				module := "_main/app/shared/nested/main.js"
				context := NpmContext{
					Module: module, Source: "shared/nested/main.ts",
					Bindings: map[string]string{"pkg": "_main/store/module-pkg"},
					PackageScope: &NpmPackageScope{
						Manifest: "_main/app/shared/package.json",
						Bindings: map[string]string{"pkg": "_main/store/scope-pkg"},
					},
				}
				if scopeOnly {
					context.Bindings = nil
				}
				cfg, r, original := npmContextFixture(t, mode, module, []NpmContext{context}, map[string]string{
					module:                          "import '#dep';",
					"_main/app/shared/package.json": `{"imports":{"#dep":"pkg"}}`,
					"_main/store/scope-pkg":         dirMarker,
					"_main/store/module-pkg":        dirMarker,
				})
				plan, err := fixturePlan(t, cfg, r, original, nil, Shard{Total: 1}, context)
				if err != nil {
					t.Fatal(err)
				}
				defer plan.Cleanup()
				root := fixtureRuntimeRoot(t, cfg, r, plan)
				bindings := map[string]string{"_main/app/shared/node_modules/pkg": "_main/store/scope-pkg"}
				moduleLookup := "_main/app/shared/nested/node_modules/pkg"
				if !scopeOnly {
					bindings[moduleLookup] = "_main/store/module-pkg"
				} else if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(moduleLookup))); !os.IsNotExist(err) {
					t.Fatalf("scope-only binding acquired a module binding: %v", err)
				}
				for path, store := range bindings {
					got, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
					want, sourceErr := os.Stat(filepath.Join(root, filepath.FromSlash(store)))
					if err != nil || sourceErr != nil || !os.SameFile(got, want) {
						t.Fatalf("%s selected another store instead of %s: %v, %v", path, store, err, sourceErr)
					}
				}
			})
		}
	}
}

func TestPlanNpmConflictsCannotOverwriteDeclaredData(t *testing.T) {
	for _, mode := range []string{ModeNode, ModeNodeTest, ModeVitest} {
		for _, occupied := range []string{"module", "scope", "incompatible stores"} {
			t.Run(mode+"/"+occupied, func(t *testing.T) {
				module := "_main/app/nested/main.js"
				manifest := "_main/app/package.json"
				if occupied == "incompatible stores" {
					manifest = "_main/app/nested/package.json"
				}
				context := NpmContext{
					Module: module, Source: "shared/nested/main.ts",
					Bindings:     map[string]string{"pkg": "_main/store/module-pkg"},
					PackageScope: &NpmPackageScope{Manifest: manifest, Bindings: map[string]string{"pkg": "_main/store/scope-pkg"}},
				}
				files := map[string]string{
					module: "export const value = 1;", manifest: `{}`,
					"_main/store/module-pkg": dirMarker, "_main/store/scope-pkg": dirMarker,
				}
				data := "_main/app/nested/node_modules/pkg"
				if occupied == "scope" {
					data = "_main/app/node_modules/pkg"
				}
				if occupied != "incompatible stores" {
					files[data] = "ordinary data"
				}
				cfg, r, original := npmContextFixture(t, mode, module, []NpmContext{context}, files)
				plan, err := fixturePlan(t, cfg, r, original, nil, Shard{Total: 1}, context)
				if plan != nil && len(plan.cleanup) != 0 {
					defer plan.Cleanup()
				}
				if err == nil || !strings.Contains(err.Error(), "projection destination") || !strings.Contains(err.Error(), context.Source) {
					t.Fatalf("MakePlan = %v, want source-owned npm destination conflict", err)
				}
				if occupied != "incompatible stores" {
					got, readErr := os.ReadFile(original[data])
					if readErr != nil || string(got) != "ordinary data" {
						t.Fatalf("failed npm placement changed data: %q, %v", got, readErr)
					}
				}
			})
		}
	}
}

func TestPlanNodeKeepsOptionalPackageAliasesAndAdjacentAddon(t *testing.T) {
	for _, modules := range []bool{false, true} {
		t.Run(strconv.FormatBool(modules), func(t *testing.T) {
			r, real := fakeRunfiles(t, map[string]string{
				"_main/a.js":                      "x",
				"+npm+/oxlint_linux/package.json": "{}",
				"+npm+/scoped/package.json":       "{}",
				"+npm+/scoped/addon.node":         "native bytes",
			})
			cfg := &Config{Mode: ModeNode, Node: &NodeConfig{
				Entry: "_main/a.js",
				OptionalDeps: []PackageLink{
					{Name: "oxlint-linux-x64", PackageJSON: "+npm+/oxlint_linux/package.json"},
					{Name: "@oxc/linux-x64", PackageJSON: "+npm+/scoped/package.json"},
					{Name: "_main", PackageJSON: "+npm+/scoped/package.json"},
				},
			}}
			if modules {
				cfg.RuntimeModules = []string{cfg.Node.Entry}
			}
			plan, err := fixturePlan(t, cfg, r, real, nil, Shard{Total: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Cleanup()

			if !plan.UseExec {
				t.Error("native execution must preserve exec identity")
			}
			root := plan.EnvOverrides["NODE_PATH"]
			if root == "" {
				t.Fatal("NODE_PATH was not pointed at the prepared optional packages")
			}
			root = strings.Split(root, string(os.PathListSeparator))[0]
			for name, pkg := range map[string]string{
				"oxlint-linux-x64": "+npm+/oxlint_linux/package.json",
				"@oxc/linux-x64":   "+npm+/scoped/package.json",
				"_main":            "+npm+/scoped/package.json",
			} {
				got, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(name)))
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				manifest, resolveErr := filepath.EvalSymlinks(filepath.Join(fixtureRuntimeRoot(t, cfg, r, plan), pkg))
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				want := filepath.Dir(manifest)
				if got != want {
					t.Errorf("%s resolves to %q, want %q", name, got, want)
				}
			}

			if modules {
				entry := filepath.Join(fixtureRuntimeRoot(t, cfg, r, plan), cfg.Node.Entry)
				if got, err := os.ReadFile(entry); err != nil || string(got) != "x" {
					t.Fatalf("optional package replaced the workspace entry: %q, %v", got, err)
				}
			}
			original, err := os.Stat(filepath.Join(fixtureRuntimeRoot(t, cfg, r, plan), "+npm+/scoped/addon.node"))
			if err != nil {
				t.Fatal(err)
			}
			linked, err := os.Stat(filepath.Join(root, "@oxc/linux-x64/addon.node"))
			if err != nil || !os.SameFile(original, linked) {
				t.Fatalf("optional native module lost its internal canonical File: %v", err)
			}
			tmp := filepath.Dir(root)
			plan.Cleanup()
			if _, err := os.Stat(tmp); err != nil {
				t.Errorf("cleanup removed immutable optional packages: %v", err)
			}
		})
	}
}

func vitestFixture(t *testing.T) (*Resolver, map[string]string) {
	t.Helper()
	return fakeRunfiles(t, map[string]string{
		"_main/tests/app/_app.vitest/config.mjs": "export default {}",
		"_main/tests/app/app_test_files.txt": strings.Join([]string{
			"_main/tests/app/a.test.js",
			"_main/tests/app/b.test.js",
			"_main/tests/app/c.test.js",
		}, "\n"),
		"_main/tests/app/a.test.js":                      "x",
		"_main/tests/app/b.test.js":                      "x",
		"_main/tests/app/c.test.js":                      "x",
		"_main/tests/app/node_modules":                   dirMarker,
		"_main/tests/app/node_modules/vitest/vitest.mjs": "x",
		"+node+/bin/node":                                "#!/bin/sh\n",
	})
}

func vitestConfig() *Config {
	return &Config{
		Label:     "//tests/app:app_test",
		Mode:      ModeVitest,
		Workspace: "_main",
		Runtime:   "+node+/bin/node",
		Vitest: &VitestConfig{
			VitestInTree:  "vitest/vitest.mjs",
			ConfigFile:    "_main/tests/app/_app_vitest.config.mjs",
			TestFilesList: "_main/tests/app/app_test_files.txt",
			NodeModules:   []string{"_main/tests/app/node_modules"},
			Stage: map[string]string{
				"_main/tests/app/_app.vitest/config.mjs": "_main/tests/app/" +
					"_app_vitest.config.mjs",
			},
		},
	}
}

// treeRoot is the tree plan.Dir, the config's package, sits under.
func treeRoot(plan *Plan) string {
	return strings.TrimSuffix(plan.Dir, filepath.FromSlash("/_main/tests/app"))
}

// vitest collects the run by its own glob over the root: the launcher names
// no file, so nothing is matched file by file against a list of arguments.
func TestPlanVitestHandsVitestNoFile(t *testing.T) {
	r, _ := vitestFixture(t)
	t.Setenv("COVERAGE_DIR", "")
	plan, err := MakePlan(vitestConfig(), r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	tree := treeRoot(plan)
	want := []string{
		filepath.Join(tree, "+node+/bin/node"),
		filepath.Join(tree, "_main/tests/app/node_modules/vitest/vitest.mjs"),
		"run", "--config",
		filepath.Join(tree, "_main/tests/app/_app_vitest.config.mjs"),
	}
	if strings.Join(plan.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", plan.Argv, want)
	}
	if plan.UseExec {
		t.Error("the test runner has to outlive vitest to post-process coverage")
	}
}

// The target's args follow the launcher's own flags.
func TestPlanVitestPutsArgsAfterItsFlags(t *testing.T) {
	r, _ := vitestFixture(t)
	t.Setenv("COVERAGE_DIR", t.TempDir())
	args := []string{"--reporter=dot"}
	plan, err := MakePlan(vitestConfig(), r, args, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	config := slices.Index(plan.Argv, "--config")
	coverage := slices.Index(plan.Argv, "--coverage.enabled")
	last := len(plan.Argv) - 1
	if !(0 < config && config < coverage && coverage < last) ||
		plan.Argv[last] != args[0] {
		t.Errorf("argv = %q, want %q last, after --config and the coverage flags",
			plan.Argv, args[0])
	}
}

func TestPlanVitestRejectsChangedInsteadOfPassingWithZeroTests(t *testing.T) {
	for _, args := range [][]string{
		{"--changed"},
		{"--changed", "HEAD~1"},
		{"--changed=HEAD~1"},
		{"a.test.js", "--changed"},
	} {
		for _, shard := range []Shard{{Total: 1}, {Index: 3, Total: 4}} {
			t.Run(strings.Join(args, " ")+"/"+strconv.Itoa(shard.Index), func(t *testing.T) {
				r, _ := vitestFixture(t)
				plan, err := MakePlan(vitestConfig(), r, args, shard)
				if err == nil {
					if len(plan.cleanup) != 0 {
						plan.Cleanup()
					}
					t.Fatal("--changed must fail even when this shard has no tests")
				}
				if !strings.Contains(err.Error(), "Git source history") || !strings.Contains(err.Error(), "source-file filter") {
					t.Fatalf("error must explain the unsupported selection and alternative: %v", err)
				}
			})
		}
	}
}

func TestPlanVitestPreservesExplicitFiltersAndPositionalChanged(t *testing.T) {
	for _, args := range [][]string{
		{"a.test.ts", "-t", "adds numbers"},
		{"--testNamePattern=adds numbers"},
		{"--changedness"},
		{"--", "--changed"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r, _ := vitestFixture(t)
			plan, err := MakePlan(vitestConfig(), r, args, Shard{Total: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Cleanup()
			if !slices.Equal(plan.Argv[len(plan.Argv)-len(args):], args) {
				t.Fatalf("argv = %q, want unchanged filter suffix %q", plan.Argv, args)
			}
		})
	}
}

func TestShardFiles(t *testing.T) {
	files := []testFile{{"c", "/third"}, {"a", "/first"}, {"b", "/second"}}
	original := slices.Clone(files)
	for _, classes := range [][][]string{
		{{"a", "b", "c"}}, {{"a", "c"}, {"b"}}, {{"a"}, {"b"}, {"c"}, {}, {}},
	} {
		union := []testFile{}
		for index, want := range classes {
			selected := shardFiles(files, Shard{Index: index, Total: len(classes)})
			union = append(union, selected...)
			got := []string{}
			for _, file := range selected {
				got = append(got, file.rlocation)
			}
			if !slices.Equal(got, want) {
				t.Errorf("shard %d/%d: got %v, want %v", index, len(classes), got, want)
			}
		}
		slices.SortFunc(union, func(a, b testFile) int { return strings.Compare(a.rlocation, b.rlocation) })
		if !slices.Equal(union, []testFile{files[1], files[2], files[0]}) {
			t.Errorf("%d shards: union = %v", len(classes), union)
		}
	}
	if !slices.Equal(files, original) || len(shardFiles(nil, Shard{Total: 1})) != 0 {
		t.Fatal("selection must preserve input and leave an empty input empty")
	}
}

func TestParseShard(t *testing.T) {
	for _, tc := range []struct {
		index, total string
		want         Shard
	}{
		{"", "", Shard{Total: 1}}, {"0", "1", Shard{Total: 1}}, {"2", "3", Shard{Index: 2, Total: 3}},
	} {
		if got, err := parseShard(tc.index, tc.total); err != nil || got != tc.want {
			t.Errorf("parseShard(%q, %q) = %v, %v", tc.index, tc.total, got, err)
		}
	}
	for _, values := range [][2]string{{"x", "1"}, {"0", "x"}, {"0", "0"}, {"-1", "2"}, {"2", "2"}} {
		if _, err := parseShard(values[0], values[1]); err == nil {
			t.Errorf("parseShard(%q, %q) must reject invalid shard settings", values[0], values[1])
		}
	}
}

func TestPlanVitestPartitionsShards(t *testing.T) {
	r, _ := vitestFixture(t)
	plan, err := MakePlan(vitestConfig(), r, nil, Shard{Index: 1, Total: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	files, err := os.ReadDir(filepath.Join(plan.EnvOverrides["TS_TEST_FILES_ROOT"], "_main/tests/app"))
	files = slices.DeleteFunc(files, func(file os.DirEntry) bool { return !strings.HasSuffix(file.Name(), ".test.js") })
	if err != nil || len(files) != 1 || files[0].Name() != "b.test.js" {
		t.Fatalf("staged files = %v, %v; want only b.test.js", files, err)
	}
	if slices.ContainsFunc(plan.Argv, func(arg string) bool { return strings.HasPrefix(arg, "--shard") }) {
		t.Error("Vitest must not partition the staged files again")
	}
}

func TestPlanVitestLeavesEmptySuitePolicyToVitest(t *testing.T) {
	for _, total := range []int{1, 3} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			r, real := vitestFixture(t)
			if err := os.WriteFile(real["_main/tests/app/app_test_files.txt"], nil, 0o644); err != nil {
				t.Fatal(err)
			}
			args := []string{"--passWithNoTests=false"}
			plan, err := MakePlan(vitestConfig(), r, args, Shard{Total: total})
			if err != nil {
				t.Fatal(err)
			}
			if plan.ExitEarly {
				t.Fatal("globally empty suite must reach Vitest's no-tests policy")
			}
			defer plan.Cleanup()
			if !slices.Contains(plan.Argv, args[0]) || !slices.Contains(plan.Argv, "--config") {
				t.Errorf("Vitest must receive its config and arguments: %q", plan.Argv)
			}
		})
	}
}

func TestPlanVitestExitsCleanlyOnAnEmptyShard(t *testing.T) {
	r, _ := vitestFixture(t)
	scratch := t.TempDir()
	t.Setenv("TEST_TMPDIR", scratch)
	plan, err := MakePlan(vitestConfig(), r, nil, Shard{Index: 7, Total: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.ExitEarly {
		t.Fatal("an empty shard must not start vitest")
	}
	if len(plan.Argv) != 0 {
		t.Errorf("argv = %q, want none for an empty shard", plan.Argv)
	}
	if files, err := os.ReadDir(scratch); err != nil || len(files) != 0 {
		t.Fatalf("empty shard staged files: %v, %v", files, err)
	}
	if !slices.ContainsFunc(plan.Messages, func(m string) bool {
		return strings.Contains(m, "no test files assigned to shard 7/8")
	}) {
		t.Errorf("messages = %q, want the empty-shard line", plan.Messages)
	}
}

// collect_coverage.sh merges the .dat files under COVERAGE_DIR into
// COVERAGE_OUTPUT_FILE with the rule's merger; the launcher writes the former.
func TestPlanVitestWritesItsLcovUnderCoverageDir(t *testing.T) {
	_, real := vitestFixture(t)
	r := runfilesTree(t, real)
	dir := filepath.Join(t.TempDir(), "coverage")
	out := filepath.Join(t.TempDir(), "coverage.dat")
	t.Setenv("COVERAGE_DIR", dir)
	t.Setenv("COVERAGE_OUTPUT_FILE", out)
	plan, err := MakePlan(vitestConfig(), r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	joined := strings.Join(plan.Argv, " ")
	reports := filepath.Join(dir, "vitest")
	if !strings.Contains(joined, "--coverage.reportsDirectory "+reports) {
		t.Errorf("argv %q is missing the reports directory %q", joined, reports)
	}
	if plan.PostRun == nil {
		t.Fatal("coverage runs need the lcov post-processing step")
	}
	if err := os.MkdirAll(reports, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reports, "lcov.info"),
		[]byte("SF:a.js\nDA:1,1\nend_of_record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := plan.PostRun(0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "vitest.dat"))
	if err != nil {
		t.Fatalf("the merger reads the .dat files under COVERAGE_DIR: %v", err)
	}
	want := "SF:tests/app/a.js\nDA:1,1\nend_of_record\n"
	if string(got) != want {
		t.Errorf("vitest.dat = %q, want %q", got, want)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("COVERAGE_OUTPUT_FILE is the merger's to write, got %v", err)
	}
}

func runfilesTree(t *testing.T, real map[string]string) *Resolver {
	t.Helper()
	const config = "_main/tests/app/_app.vitest/config.mjs"
	dir := strings.TrimSuffix(real[config], string(filepath.Separator)+
		filepath.FromSlash(config))
	r, err := directoryResolver(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPlanVitestFailsAPassingRunThatWroteNoLcov(t *testing.T) {
	r, _ := vitestFixture(t)
	t.Setenv("COVERAGE_DIR", t.TempDir())
	plan, err := MakePlan(vitestConfig(), r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	if err := plan.PostRun(0); err == nil {
		t.Error("a passing run with no lcov.info would read as a clean report")
	}
	if err := plan.PostRun(1); err != nil {
		t.Errorf("a failed run has no report to write: %v", err)
	}
}

func TestRewriteLcovOnlyTouchesSourceFileLines(t *testing.T) {
	in := []byte("SF:_main/a.js\nFN:1,_main/x\nSF:_mainless/b.js\n")
	got := string(RewriteLcov(in, "_main", "/r", "/r"))
	want := "SF:a.js\nFN:1,_main/x\nSF:_mainless/b.js\n"
	if got != want {
		t.Errorf("RewriteLcov = %q, want %q", got, want)
	}
}

// istanbul writes paths relative to vite's root, the config's package, and a
// module a pool resolved through its realpath as an execroot path.
func TestRewriteLcovResolvesThePathsAgainstTheRoot(t *testing.T) {
	runDir := "/w/execroot/_main/bazel-out/k8-fastbuild/bin/tests/workers/t.runfiles"
	root := runDir + "/_main/tests/vitest/coverage"
	cases := []struct {
		name string
		in   string
		want string
	}{{
		name: "a file in the root's package",
		in:   "SF:same_package.js\n",
		want: "SF:tests/vitest/coverage/same_package.js\n",
	}, {
		name: "a file in an ancestor package",
		in:   "SF:../math.js\n",
		want: "SF:tests/vitest/math.js\n",
	}, {
		name: "escaping relative, as istanbul writes an out-of-root module",
		in: "SF:../../../../../../../../../../" +
			"bazel-out/k8-fastbuild/bin/tests/workers/src/index.js\n",
		want: "SF:tests/workers/src/index.js\n",
	}, {
		name: "absolute, under bazel-out",
		in:   "SF:/w/execroot/_main/bazel-out/k8-fastbuild/bin/tests/workers/src/index.js\n",
		want: "SF:tests/workers/src/index.js\n",
	}, {
		name: "absolute, elsewhere, is left alone",
		in:   "SF:/home/me/src/tests/workers/src/index.js\n",
		want: "SF:/home/me/src/tests/workers/src/index.js\n",
	}, {
		name: "a path into another repository's runfiles is left alone",
		in:   "SF:../../../../_other/x.js\n",
		want: "SF:../../../../_other/x.js\n",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(RewriteLcov([]byte(tc.in), "_main", runDir, root))
			if got != tc.want {
				t.Errorf("RewriteLcov = %q, want %q", got, tc.want)
			}
		})
	}
}

func devServerFixture(t *testing.T) (*Resolver, map[string]string) {
	t.Helper()
	return fakeRunfiles(t, map[string]string{
		"_main/tests/app/dev_vite.config.mjs":           "export default {}",
		"_main/tests/app/node_modules":                  dirMarker,
		"_main/tests/app/node_modules/vite/bin/vite.js": "x",
		"_main/vite/vite_plugin_bazel.mjs":              "x",
		"_main/tools/native_server/serve":               "#!/bin/sh\n",
		"+node+/bin/node":                               "#!/bin/sh\n",
	})
}

// devServerWorkspace is a real directory, because planDevServer links the
// importer's node_modules into it.
func devServerWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("BUILD_WORKSPACE_DIRECTORY", ws)
	return ws
}

func devServerConfig() *Config {
	return &Config{
		Label:   "//tests/app:dev",
		Mode:    ModeDevServer,
		Runtime: "+node+/bin/node",
		DevServer: &DevServerConfig{
			ConfigFile:      "_main/tests/app/dev_vite.config.mjs",
			PackageDir:      "tests/app",
			NodeModules:     "_main/tests/app/node_modules",
			ServerInTree:    "vite/bin/vite.js",
			Argv:            []string{"dev", "--config", "{config}"},
			RunsInJsRuntime: true,
			Plugin:          "_main/vite/vite_plugin_bazel.mjs",
			Port:            5173,
		},
	}
}

func TestPlanDevServerRunsViteFromTheNodeModulesTree(t *testing.T) {
	r, real := devServerFixture(t)
	ws := devServerWorkspace(t)
	plan, err := MakePlan(devServerConfig(), r, []string{"--host"}, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	vite := filepath.Join(real["_main/tests/app/node_modules"], "vite", "bin", "vite.js")
	// The port is passed on the command line even though the config carries it:
	// only the flag survives a server that does not read the config.
	want := []string{
		real["+node+/bin/node"], vite, "dev", "--config",
		real["_main/tests/app/dev_vite.config.mjs"], "--port", "5173", "--host",
	}
	if strings.Join(plan.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", plan.Argv, want)
	}
	if plan.Dir != filepath.Join(ws, "tests", "app") {
		t.Errorf("dir = %q, want the app package", plan.Dir)
	}
	if plan.EnvOverrides["BAZEL_BIN_DIR"] != filepath.Join(ws, "bazel-bin") {
		t.Errorf("BAZEL_BIN_DIR = %q", plan.EnvOverrides["BAZEL_BIN_DIR"])
	}
	if plan.EnvOverrides["VITE_PLUGIN_PATH"] != real["_main/vite/vite_plugin_bazel.mjs"] {
		t.Errorf("VITE_PLUGIN_PATH = %q", plan.EnvOverrides["VITE_PLUGIN_PATH"])
	}
	if !plan.Supervise.IgnoreTerm {
		t.Error("ibazel SIGTERMs the runner on rebuild; vite must survive it")
	}
	if !plan.Supervise.TerminateOnInterrupt {
		t.Error("the dev server must explicitly retain its interrupt-to-termination behavior")
	}
}

func TestPlanDevServerExplainsAMissingVite(t *testing.T) {
	r, _ := devServerFixture(t)
	cfg := devServerConfig()
	cfg.DevServer.ServerInTree = "vite/bin/absent.js"
	_, err := MakePlan(cfg, r, nil, Shard{Total: 1})
	if err == nil {
		t.Fatal("a node_modules without vite must fail")
	}
	if !strings.Contains(err.Error(), "node_modules() target") {
		t.Errorf("error is not actionable: %v", err)
	}
}

func nativeDevServerConfig() *Config {
	cfg := devServerConfig()
	cfg.DevServer.ServerInTree = ""
	cfg.DevServer.ServerBinary = "_main/tools/native_server/serve"
	cfg.DevServer.Argv = []string{"dev", "--config", "{config}", "{root}"}
	cfg.DevServer.RunsInJsRuntime = false
	return cfg
}

func TestPlanDevServerRunsANativeServerWithoutTheJsRuntime(t *testing.T) {
	r, real := devServerFixture(t)
	ws := devServerWorkspace(t)
	plan, err := MakePlan(nativeDevServerConfig(), r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		real["_main/tools/native_server/serve"], "dev", "--config",
		real["_main/tests/app/dev_vite.config.mjs"], filepath.Join(ws, "tests", "app"), "--port", "5173",
	}
	if strings.Join(plan.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", plan.Argv, want)
	}
	// The plugin host is a Node process the server spawns by name, so a native
	// server that cannot see the toolchain node would fall back to a host one.
	nodeDir := filepath.Dir(real["+node+/bin/node"])
	if !strings.HasPrefix(plan.EnvOverrides["PATH"], nodeDir+string(os.PathListSeparator)) {
		t.Errorf("PATH = %q, want the toolchain node dir %q first",
			plan.EnvOverrides["PATH"], nodeDir)
	}
}

func TestPlanDevServerExplainsAMissingNodeModules(t *testing.T) {
	r, _ := devServerFixture(t)
	cfg := devServerConfig()
	cfg.DevServer.NodeModules = ""
	_, err := MakePlan(cfg, r, nil, Shard{Total: 1})
	if err == nil || !strings.Contains(err.Error(), "node_modules") {
		t.Fatalf("want an actionable node_modules error, got %v", err)
	}
}

// Without a runfiles tree the files live in bazel-bin, beside every sibling
// target's identically named copy, which vitest's root would glob in.
func TestPlanVitestStagesAPrivateRootWithoutARunfilesDirectory(t *testing.T) {
	r, real := vitestFixture(t)
	cfg := vitestConfig()
	cfg.RuntimeModules = []string{
		"_main/tests/app/a.test.js",
		"_main/tests/app/b.test.js",
		"_main/tests/app/c.test.js",
	}
	plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	root := plan.EnvOverrides["TS_TEST_FILES_ROOT"]
	if root == "" || root == plan.Dir ||
		strings.HasPrefix(real["_main/tests/app/a.test.js"], root) {
		t.Fatalf("dir = %q, want a package under a staged root of its own", plan.Dir)
	}

	last := plan.Argv[len(plan.Argv)-1]
	if !strings.HasSuffix(last, "config.mjs") {
		t.Errorf("argv = %q, want it to end at the flags", plan.Argv)
	}
	want := []string{
		filepath.Join(root, "_main/tests/app/a.test.js"),
		filepath.Join(root, "_main/tests/app/b.test.js"),
		filepath.Join(root, "_main/tests/app/c.test.js"),
	}
	for i, link := range want {
		resolved, err := filepath.EvalSymlinks(link)
		if err != nil {
			t.Fatalf("%s: %v", link, err)
		}
		rlocation := []string{"a", "b", "c"}[i]
		if expected := filepath.Join(resolvedPath(t, treeRoot(plan)), "_main/tests/app/"+rlocation+".test.js"); resolved != expected {
			t.Errorf("%s resolves to %q, want %q", link, resolved, expected)
		}
	}

	files := []string{}
	walk := func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	}
	if err := filepath.WalkDir(root, walk); err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	wantStaged := []string{
		"_main/tests/app/a.test.js",
		"_main/tests/app/b.test.js",
		"_main/tests/app/c.test.js",
	}
	if strings.Join(files, ",") != strings.Join(wantStaged, ",") {
		t.Errorf("staged root holds %q, want exactly %q", files, wantStaged)
	}

	plan.Cleanup()
	for _, path := range []string{root, treeRoot(plan)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("cleanup left %s behind", path)
		}
	}
}

// vitest walks the root the launcher staged, the program's compiled files at
// their runfiles paths and nothing else, while vite's root stays in the tree.
func TestPlanVitestStagesTheFilesVitestWalksBesideTheTree(t *testing.T) {
	_, real := vitestFixture(t)
	r := runfilesTree(t, real)
	cfg := vitestConfig()
	cfg.RuntimeModules = []string{
		"_main/tests/app/a.test.js",
		"_main/tests/app/b.test.js",
		"_main/tests/app/c.test.js",
	}
	plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	filesRoot := plan.EnvOverrides["TS_TEST_FILES_ROOT"]
	tree := runfilesEnv(plan.runfilesEnv, "RUNFILES_DIR")
	if filesRoot == "" || strings.HasPrefix(filesRoot, tree) {
		t.Fatalf("TS_TEST_FILES_ROOT = %q, want a root outside the tree %q",
			filesRoot, tree)
	}
	if tree == r.Dir() {
		t.Fatal("the execution tree must not modify the input runfiles directory")
	}
	if want := filepath.Join(tree, "_main/tests/app"); plan.Dir != want {
		t.Errorf("dir = %q, want vite's root in the tree, %q", plan.Dir, want)
	}
	staged := []string{}
	walk := func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(filesRoot, p)
			staged = append(staged, filepath.ToSlash(rel))
		}
		return nil
	}
	if err := filepath.WalkDir(filesRoot, walk); err != nil {
		t.Fatal(err)
	}
	slices.Sort(staged)
	want := []string{
		"_main/tests/app/a.test.js",
		"_main/tests/app/b.test.js",
		"_main/tests/app/c.test.js",
	}
	if strings.Join(staged, ",") != strings.Join(want, ",") {
		t.Errorf("the staged root holds %q, want exactly %q", staged, want)
	}
	for _, rel := range want {
		got, err := filepath.EvalSymlinks(filepath.Join(filesRoot, rel))
		expected := filepath.Join(resolvedPath(t, tree), rel)
		if err != nil || got != expected {
			t.Errorf("%s resolves to %q, %v; want %q", rel, got, err, expected)
		}
	}
	if len(plan.cleanup) == 0 {
		t.Fatal("the staged root has to come back off")
	}
	plan.Cleanup()
	for _, path := range []string{filesRoot, tree} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("cleanup left %s behind", path)
		}
	}
}

// A test outside its importer's package meets no node_modules on the walk up
// before the workspace's root, so the importer's is linked in there.
func TestPlanVitestLinksTheImporterAtTheWorkspaceRoot(t *testing.T) {
	const importer = "_main/tests/npm/node_modules"
	_, real := fakeRunfiles(t, map[string]string{
		"_main/tests/app/_app.vitest/config.mjs": "export default {}",
		"_main/tests/app/app_test_files.txt":     "_main/tests/app/a.test.js",
		"_main/tests/app/a.test.js":              "x",
		importer:                                 dirMarker,
		importer + "/vitest/vitest.mjs":          "x",
		"+node+/bin/node":                        "#!/bin/sh\n",
	})
	r, _ := withRunfilesDir(t, real[importer], importer)
	cfg := vitestConfig()
	cfg.Vitest.NodeModules = []string{importer}
	plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	root := treeRoot(plan)
	got, err := os.Readlink(filepath.Join(root, "_main", "node_modules"))
	if err != nil {
		t.Fatalf("no node_modules link at the workspace root: %v", err)
	}
	if want := filepath.Join(root, filepath.FromSlash(importer)); got != want {
		t.Errorf("link -> %q, want the importer's node_modules %q", got, want)
	}
	vitest := filepath.Join(root, importer, "vitest/vitest.mjs")
	if !slices.Contains(plan.Argv, vitest) {
		t.Errorf("argv = %q, want vitest from the importer %q", plan.Argv, vitest)
	}
}

// vitest may be an importer above's: the chain is walked nearest first, as
// node walks up, and NODE_PATH names it in that order.
func TestPlanVitestWalksTheChainForVitest(t *testing.T) {
	_, real := fakeRunfiles(t, map[string]string{
		"_main/tests/app/_app.vitest/config.mjs":     "export default {}",
		"_main/tests/app/app_test_files.txt":         "_main/tests/app/a.test.js",
		"_main/tests/app/a.test.js":                  "x",
		"_main/tests/app/node_modules/zod/i.js":      "x",
		"_main/tests/node_modules/vitest/vitest.mjs": "x",
		"+node+/bin/node":                            "#!/bin/sh\n",
	})
	const test = "_main/tests/app/a.test.js"
	r, _ := withRunfilesDir(t, real[test], test)
	cfg := vitestConfig()
	near, above := "_main/tests/app/node_modules", "_main/tests/node_modules"
	cfg.Vitest.NodeModules = []string{near, above}
	plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	root := treeRoot(plan)
	vitest := filepath.Join(root, above, "vitest/vitest.mjs")
	if !slices.Contains(plan.Argv, vitest) {
		t.Errorf("argv = %q, want vitest from the importer above %q",
			plan.Argv, vitest)
	}
	want := filepath.Join(root, near) + string(os.PathListSeparator) +
		filepath.Join(root, above)
	if got := plan.EnvOverrides["NODE_PATH"]; got != want {
		t.Errorf("NODE_PATH = %q, want the chain nearest first %q", got, want)
	}
	link := filepath.Join(root, "_main", "node_modules")
	if got, _ := os.Readlink(link); got != filepath.Join(root, above) {
		t.Errorf("%s -> %q, want the chain's root", link, got)
	}
}

func TestPlanVitestPreservesSupplierScopeAndStoreWithoutDiscoveringHelpers(t *testing.T) {
	base := filepath.Join(t.TempDir(), "var")
	if err := os.Symlink(t.TempDir(), base); err != nil {
		t.Fatal(err)
	}
	const store = "bin/tests/app/node_modules/.pnpm/vitest@4/node_modules/vitest"
	tree := filepath.Join(base, store)
	const helper = "export const answer = 42;\n"
	const scope = `{"type":"module","imports":{"#helper":"./helper.mjs"}}`
	for rel, body := range map[string]string{
		"files/app_test_files.txt": "_main/tests/app/a.test.js\n_main/tests/app/b.test.js\n",
		"files/a.test.js":          "x",
		"files/b.test.js":          "x",
		"files/config.mjs":         "export default {}",
		"files/helper.mjs":         helper,
		"files/package.json":       scope,
		"files/payload.txt":        "linked data\n",
		"files/node":               "#!/bin/sh\n",
		store + "/vitest.mjs":      "x",
	} {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := filepath.Join(base, "files")
	entries := []string{
		"_main/tests/app/app_test_files.txt " + files + "/app_test_files.txt",
		"_main/tests/app/a.test.js " + files + "/a.test.js",
		"_main/tests/app/b.test.js " + files + "/b.test.js",
		"_main/tests/app/_app.vitest/config.mjs " + files + "/config.mjs",
		"_main/tests/config/helper.mjs " + files + "/helper.mjs",
		"_main/tests/config/package.json " + files + "/package.json",
		"_main/tests/app/fixture_links/payload.txt " + files + "/payload.txt",
		"_main/tests/app/fixture_links/valid.json payload.txt",
		"_main/tests/app/fixture_links/valid.js payload.txt",
		"_main/tests/app/fixture_links/dangling.json absent.txt",
		"_main/tests/app/fixture_links/dangling.js absent.txt",
		"_main/tests/app/node_modules/vitest .pnpm/vitest@4/node_modules/vitest",
		"_main/tests/app/node_modules/.pnpm/vitest@4/node_modules/vitest " + tree,
		"+node+/bin/node " + files + "/node",
	}
	for _, mode := range []string{"manifest", "manifest_directory", "directory"} {
		t.Run(mode, func(t *testing.T) {
			var r *Resolver
			if mode != "directory" {
				r = manifestResolver(t, entries)
				if mode == "manifest_directory" {
					program := filepath.Join(t.TempDir(), "launcher")
					directory := program + ".runfiles"
					if err := os.Mkdir(directory, 0o755); err != nil {
						t.Fatal(err)
					}
					manifest, err := os.ReadFile(runfilesEnv(r.Env(), "RUNFILES_MANIFEST_FILE"))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(directory, "MANIFEST"), manifest, 0o644); err != nil {
						t.Fatal(err)
					}
					r, err = resolverForExecutable(program)
					if err != nil {
						t.Fatal(err)
					}
				}
			} else {
				dir := t.TempDir()
				for _, entry := range entries {
					rel, target, _ := strings.Cut(entry, " ")
					path := filepath.Join(dir, filepath.FromSlash(rel))
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.FromSlash(target), path); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				r, err = directoryResolver(dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg := vitestConfig()
			cfg.RuntimeModules = []string{
				"_main/tests/app/a.test.js",
				"_main/tests/app/b.test.js",
				"_main/tests/config/helper.mjs",
			}
			plan, err := MakePlan(cfg, r, nil, Shard{Total: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Cleanup()
			root := treeRoot(plan)
			for rel, want := range map[string]string{
				"_main/tests/config/helper.mjs": helper,
				"_main/tests/app/b.test.js":     "x",
			} {
				path := filepath.Join(root, rel)
				info, err := os.Lstat(path)
				if err != nil || !info.Mode().IsRegular() {
					t.Fatalf("supplier file %s is not materialized: %v, %v", rel, info, err)
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Fatalf("supplier file %s = %q, %v; want %q", rel, got, err, want)
				}
			}
			manifest := filepath.Join(root, "_main/tests/config/package.json")
			if got, err := os.Readlink(manifest); err != nil || got != filepath.Join(files, "package.json") {
				t.Fatalf("scope link = %q, %v; want the original package scope", got, err)
			}
			if got, err := os.ReadFile(manifest); err != nil || string(got) != scope {
				t.Fatalf("scope contents = %q, %v; want %q", got, err, scope)
			}
			for _, extension := range []string{"json", "js"} {
				for name, target := range map[string]string{"valid": "payload.txt", "dangling": "absent.txt"} {
					link := filepath.Join(root, "_main/tests/app/fixture_links", name+"."+extension)
					if got, err := os.Readlink(link); err != nil || got != target {
						t.Fatalf("data link %s = %q, %v; want %q", link, got, err, target)
					}
					if name == "valid" {
						if got, err := os.ReadFile(link); err != nil || string(got) != "linked data\n" {
							t.Fatalf("data link contents = %q, %v", got, err)
						}
					} else if _, err := os.Stat(link); !os.IsNotExist(err) {
						t.Fatalf("dangling data link acquired a target: %v", err)
					}
				}
			}
			discovery := plan.EnvOverrides["TS_TEST_FILES_ROOT"]
			if discovery == root {
				t.Fatal("supplier runtime files must not enter Vitest discovery")
			}
			selected, err := os.ReadDir(filepath.Join(discovery, "_main/tests/app"))
			if err != nil || len(selected) != 1 || selected[0].Name() != "a.test.js" {
				t.Fatalf("discovery files = %v, %v; want only the assigned test", selected, err)
			}
			if _, err := os.Stat(filepath.Join(discovery, "_main/tests/config")); !os.IsNotExist(err) {
				t.Fatalf("supplier config entered test discovery: %v", err)
			}
			if got := runfilesEnv(plan.runfilesEnv, "RUNFILES_DIR"); got != root {
				t.Fatalf("child runfiles directory = %q, want %q", got, root)
			}
			if got := runfilesEnv(plan.runfilesEnv, "RUNFILES_MANIFEST_FILE"); got != "" {
				t.Fatalf("child retained the unstaged manifest %q", got)
			}
			link := filepath.Join(root, "_main/tests/app/node_modules/vitest")
			relative := filepath.FromSlash(".pnpm/vitest@4/node_modules/vitest")
			if got, err := os.Readlink(link); err != nil || got != relative {
				t.Errorf("readlink %s = %q, %v; want the manifest's target verbatim",
					link, got, err)
			}
			got, err := filepath.EvalSymlinks(link)
			if err != nil {
				t.Fatal(err)
			}
			want, err := filepath.EvalSymlinks(tree)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("%s resolves to %q, want the store tree %q", link, got, want)
			}
			entry := filepath.Join(link, "vitest.mjs")
			if !slices.Contains(plan.Argv, entry) {
				t.Errorf("argv = %q, want vitest through the staged link %q",
					plan.Argv, entry)
			}
			nodeModules := filepath.Join(root, "_main/tests/app/node_modules")
			if got := plan.EnvOverrides["NODE_PATH"]; got != nodeModules {
				t.Errorf("NODE_PATH = %q, want the staged importer directory", got)
			}
		})
	}
}

// manifestResolver is a manifest-only layout over the given manifest lines.
func manifestResolver(t *testing.T, lines []string) *Resolver {
	t.Helper()
	manifest := filepath.Join(t.TempDir(), "MANIFEST")
	data := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(manifest, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNFILES_DIR", "")
	t.Setenv("TEST_SRCDIR", "")
	t.Setenv("RUNFILES_MANIFEST_FILE", manifest)
	r, err := newResolver(runfiles.ManifestFile(manifest))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestManifestStagingCannotChangeDirectoryEntries(t *testing.T) {
	for _, overlap := range []string{"same", "different"} {
		t.Run(overlap, func(t *testing.T) {
			source := t.TempDir()
			child := filepath.Join(source, "child")
			if err := os.WriteFile(child, []byte("original"), 0o644); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(t.TempDir(), "other file")
			if err := os.WriteFile(other, []byte("other"), 0o644); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "child-alias")
			if err := os.Symlink(child, target); err != nil {
				t.Fatal(err)
			}
			if overlap == "different" {
				target = other
			}
			const base = "_main/node_modules/"
			r := manifestResolver(t, []string{
				base + "data/child " + target,
				base + "data " + source,
				base + "data-tail " + other,
				" " + base + "space\\sfile " + strings.ReplaceAll(other, " ", `\s`),
				base + "empty ",
			})
			root := t.TempDir()
			_, err := r.Stage(root, nil)
			if overlap == "different" {
				if err == nil || !strings.Contains(err.Error(), "conflicts with directory entry") {
					t.Fatalf("conflicting exact child was accepted: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				for path, want := range map[string]string{
					"data/child": "original",
					"data-tail":  "other",
					"space file": "other",
					"empty":      "",
				} {
					got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(base+path)))
					if err != nil || string(got) != want {
						t.Errorf("staged %s = %q, %v; want %q", path, got, err, want)
					}
				}
			}
			if got, err := os.ReadFile(child); err != nil || string(got) != "original" {
				t.Errorf("manifest overlay changed source directory: %q, %v", got, err)
			}
		})
	}
}

func TestManifestStagingRejectsPathsOutsideItsTree(t *testing.T) {
	for _, path := range []string{"../escaped", "."} {
		t.Run(path, func(t *testing.T) {
			r := manifestResolver(t, []string{path + " /unused"})
			if _, err := r.Stage(t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "non-normalized runfiles path") {
				t.Fatalf("escaping manifest path was accepted: %v", err)
			}
		})
	}
}

func TestNodeModulesCannotWriteThroughWorkspaceDirectoryEntry(t *testing.T) {
	for _, mode := range []string{"missing", "existing"} {
		t.Run(mode, func(t *testing.T) {
			existing := mode == "existing"
			source, packages := t.TempDir(), t.TempDir()
			const importer = "external/node_modules"
			r := manifestResolver(t, []string{"_main " + source, importer + " " + packages})
			root := t.TempDir()
			_, err := r.Stage(root, nil)
			if err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(source, "node_modules")
			if existing {
				if err := os.Symlink(packages, link); err != nil {
					t.Fatal(err)
				}
			}
			_, err = installNodeModules(&Plan{}, root, "_main", []string{importer})
			if existing {
				if err != nil {
					t.Fatal(err)
				}
				if got, err := os.Readlink(link); err != nil || got != packages {
					t.Fatalf("existing mapping was changed: %q, %v", got, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "workspace directory alias") {
					t.Fatalf("write through workspace alias was accepted: %v", err)
				}
				if _, err := os.Lstat(link); !os.IsNotExist(err) {
					t.Fatalf("workspace alias modified source tree: %v", err)
				}
			}
		})
	}
}

// withRunfilesDir rebuilds the resolver with RUNFILES_DIR at the fixture's tree
// root: fakeRunfiles clears it, and a resolver reads it once, at construction.
func withRunfilesDir(t *testing.T, real, rlocation string) (*Resolver, string) {
	t.Helper()
	root := strings.TrimSuffix(filepath.ToSlash(real), "/"+rlocation)
	t.Setenv("RUNFILES_DIR", root)
	t.Setenv("RUNFILES_MANIFEST_FILE", "")
	t.Setenv("RUNFILES_MANIFEST_ONLY", "")
	r, err := directoryResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	return r, root
}

func TestEnvironCollapsesDuplicateKeys(t *testing.T) {
	t.Setenv("RUNFILES_DIR", "relative/path")
	t.Setenv("KEEP_ME", "yes")
	got := Environ(map[string]string{"RUNFILES_DIR": "/absolute/path"}, []string{"RUNFILES_DIR=from/library"})

	seen := 0
	for _, entry := range got {
		if strings.HasPrefix(entry, "RUNFILES_DIR=") {
			seen++
			if entry != "RUNFILES_DIR=/absolute/path" {
				t.Errorf("RUNFILES_DIR = %q, want the rule's value to win", entry)
			}
		}
	}
	if seen != 1 {
		t.Errorf("RUNFILES_DIR appears %d times; getenv would answer with the first", seen)
	}
	if !slices.Contains(got, "KEEP_ME=yes") {
		t.Error("Environ dropped an inherited variable")
	}
}

// The launcher links the importer's node_modules in at the workspace root;
// these pin what it does with whatever already sits on that name.

func TestPlanDevServerLinksTheNpmTreeIntoTheWorkspace(t *testing.T) {
	r, real := devServerFixture(t)
	ws := devServerWorkspace(t)
	plan, err := MakePlan(devServerConfig(), r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "node_modules")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("no node_modules link at the workspace root: %v", err)
	}
	if target != real["_main/tests/app/node_modules"] {
		t.Errorf("link -> %q, want the node_modules %q", target,
			real["_main/tests/app/node_modules"])
	}
	if len(plan.cleanup) == 0 {
		t.Fatal("a link the launcher made has to come back off")
	}
	plan.Cleanup()
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind (%v)", link, err)
	}
}

func TestPlanDevServerKeepsAnIdenticalLinkAndDoesNotRemoveIt(t *testing.T) {
	r, real := devServerFixture(t)
	ws := devServerWorkspace(t)
	link := filepath.Join(ws, "node_modules")
	if err := os.Symlink(real["_main/tests/app/node_modules"], link); err != nil {
		t.Fatal(err)
	}
	plan, err := MakePlan(devServerConfig(), r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Another dev server on the same directory may own it; removing it breaks a
	// server this process never started.
	if len(plan.cleanup) != 0 {
		plan.Cleanup()
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("a link it did not create was removed anyway: %v", err)
	}
}

func TestPlanDevServerRefusesALinkToAnotherNodeModules(t *testing.T) {
	r, _ := devServerFixture(t)
	ws := devServerWorkspace(t)
	other := t.TempDir()
	link := filepath.Join(ws, "node_modules")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	_, err := MakePlan(devServerConfig(), r, nil, Shard{Total: 1})
	if err == nil {
		t.Fatal("two node_modules cannot both be at the workspace root")
	}
	if !strings.Contains(err.Error(), other) {
		t.Errorf("the error does not name the directory already there: %v", err)
	}
	if target, _ := os.Readlink(link); target != other {
		t.Errorf("the existing link was replaced with %q", target)
	}
}

func TestPlanDevServerRefusesToDeleteAnInstalledNodeModules(t *testing.T) {
	r, _ := devServerFixture(t)
	ws := devServerWorkspace(t)
	link := filepath.Join(ws, "node_modules")
	if err := os.MkdirAll(filepath.Join(link, "left-behind"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := MakePlan(devServerConfig(), r, nil, Shard{Total: 1})
	if err == nil {
		t.Fatal("a real node_modules directory must not be taken over silently")
	}
	if _, statErr := os.Stat(filepath.Join(link, "left-behind")); statErr != nil {
		t.Errorf("the existing install was disturbed: %v", statErr)
	}
}

func TestPlanDevServerSubstitutesThePortIntoArgvWithoutRepeatingIt(t *testing.T) {
	r, real := devServerFixture(t)
	devServerWorkspace(t)
	cfg := devServerConfig()
	cfg.DevServer.Argv = []string{"dev", "--config", "{config}", "--port", "{port}", "{root}"}

	plan, err := MakePlan(cfg, r, []string{"--port", "4321"}, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.Join(plan.Argv, " "), "--port"); got != 1 {
		t.Errorf("argv names --port %d times, want once: %q", got, plan.Argv)
	}
	// And the override is what it was given, not the port the rule configured.
	if !slices.Contains(plan.Argv, "4321") || slices.Contains(plan.Argv, "5173") {
		t.Errorf("argv = %q, want the overriding port 4321", plan.Argv)
	}
	_ = real
}

// A dev server told where to put its scratch has to find the directory there:
// a tool handed a path that does not exist may or may not create one.
func TestPlanDevServerCreatesTheScratchDirectoriesItNames(t *testing.T) {
	r, _ := devServerFixture(t)
	ws := devServerWorkspace(t)
	cfg := devServerConfig()
	cfg.DevServer.ScratchDir = "tests/app/dev_dev"

	plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TSR_TMP_DIR", "OJ_CACHE_DIR"} {
		dir := plan.EnvOverrides[name]
		if dir == "" {
			t.Errorf("%s is unset", name)
			continue
		}
		if !strings.HasPrefix(dir, filepath.Join(ws, "bazel-bin")) {
			t.Errorf("%s = %q, want it under bazel-bin, not in the source tree", name, dir)
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Errorf("%s names %q, which is not a directory (%v)", name, dir, err)
		}
	}
}

// pnpm runs vitest from the package, and a test's relative read or a config's
// __dirname names files there: the run's directory is the config's package.
func TestPlanVitestRunsFromTheConfigsPackage(t *testing.T) {
	_, real := vitestFixture(t)
	r := runfilesTree(t, real)
	plan, err := MakePlan(vitestConfig(), r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	if want := filepath.Join(runfilesEnv(plan.runfilesEnv, "RUNFILES_DIR"), "_main/tests/app"); plan.Dir != want {
		t.Errorf("dir = %q, want the config's package %q", plan.Dir, want)
	}
}

func TestPlanVitestRunsFromAnAncestorConfigsPackage(t *testing.T) {
	_, real := vitestFixture(t)
	r := runfilesTree(t, real)
	cfg := vitestConfig()
	cfg.Vitest.RootRel = ".."
	plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	if want := filepath.Join(runfilesEnv(plan.runfilesEnv, "RUNFILES_DIR"), "_main/tests"); plan.Dir != want {
		t.Errorf("dir = %q, want the ancestor package %q", plan.Dir, want)
	}
}

// Vite bundles a config from its realpath, which for a runfiles symlink is
// bazel-out: the config vitest is handed is a regular file in the package.
func TestPlanVitestWritesTheConfigAsARegularFileInThePackage(t *testing.T) {
	_, real := vitestFixture(t)
	r := runfilesTree(t, real)
	plan, err := MakePlan(vitestConfig(), r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	dst := filepath.Join(treeRoot(plan), "_main/tests/app/_app_vitest.config.mjs")
	st, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("no config at the package path: %v", err)
	}
	if !st.Mode().IsRegular() {
		t.Errorf("%s is %v, want a regular file", dst, st.Mode())
	}
	if got, _ := os.ReadFile(dst); string(got) != "export default {}" {
		t.Errorf("content = %q, want the generated config's", got)
	}
	joined := strings.Join(plan.Argv, " ")
	if !strings.Contains(joined, "--config "+dst) {
		t.Errorf("argv %q does not hand vitest %q", joined, dst)
	}
}

// A config that is also a package src has a runfiles symlink at its own path;
// the copy replaces it, and is written from its private entry every run.
func TestPlanVitestReplacesASymlinkAtAStagedPath(t *testing.T) {
	const from = "_main/tests/app/_app.vitest/tests/app/vitest.config.mts"
	const to = "_main/tests/app/vitest.config.mts"
	const fresh = "export default { fresh: true }"
	_, real := fakeRunfiles(t, map[string]string{
		"_main/tests/app/_app.vitest/config.mjs":         "export default {}",
		from:                                             fresh,
		"_main/tests/app/app_test_files.txt":             "_main/tests/app/a.test.js",
		"_main/tests/app/a.test.js":                      "x",
		"_main/tests/app/node_modules":                   dirMarker,
		"_main/tests/app/node_modules/vitest/vitest.mjs": "x",
		"+node+/bin/node":                                "#!/bin/sh\n",
	})
	r := runfilesTree(t, real)
	stale := filepath.Join(t.TempDir(), "stale.mts")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(r.Dir(), filepath.FromSlash(to))
	if err := os.Symlink(stale, original); err != nil {
		t.Fatal(err)
	}
	cfg := vitestConfig()
	cfg.Vitest.Stage[from] = to
	plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	dst := filepath.Join(treeRoot(plan), filepath.FromSlash(to))
	st, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mode().IsRegular() {
		t.Errorf("%s is %v, want a regular file over the symlink", dst, st.Mode())
	}
	if got, _ := os.ReadFile(dst); string(got) != fresh {
		t.Errorf("content = %q, want the private entry's", got)
	}
	if got, _ := os.ReadFile(stale); string(got) != "stale" {
		t.Errorf("the symlink's target was written through: %q", got)
	}
	if got, err := os.Readlink(original); err != nil || got != stale {
		t.Errorf("the input runfiles symlink changed: %q, %v", got, err)
	}
}

func TestPlanVitestConfigStagingCannotWriteThroughDirectoryEntries(t *testing.T) {
	for _, mode := range []string{"manifest", "directory", "directory_internal_alias"} {
		t.Run(mode, func(t *testing.T) {
			const private = "_main/tests/app/_app.vitest/tests/config/"
			const destination = "_main/tests/config"
			const wrapper = "_main/tests/app/_app_vitest.config.mjs"
			r, real := fakeRunfiles(t, map[string]string{
				"_main/tests/app/_app.vitest/config.mjs":         "export default {}",
				private + "vitest.config.mjs":                    "export default { fresh: true }",
				private + "nested/helper.mjs":                    "export const fresh = true",
				wrapper:                                          "previous wrapper",
				"_main/tests/app/app_test_files.txt":             "_main/tests/app/a.test.js",
				"_main/tests/app/a.test.js":                      "x",
				"_main/tests/app/node_modules/vitest/vitest.mjs": "x",
				"+node+/bin/node":                                "#!/bin/sh\n",
			})
			original := t.TempDir()
			if mode == "directory_internal_alias" {
				original = filepath.Join(filepath.Dir(real[wrapper]), "original_config")
			}
			contents := map[string]string{
				"vitest.config.mjs": "original config",
				"nested/helper.mjs": "original helper",
				"sentinel.txt":      "original sentinel",
			}
			identities := map[string]os.FileInfo{}
			for path, content := range contents {
				file := filepath.Join(original, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				identities[path] = info
			}
			if mode != "manifest" {
				r = runfilesTree(t, real)
				if err := os.Symlink(original, filepath.Join(r.Dir(), filepath.FromSlash(destination))); err != nil {
					t.Fatal(err)
				}
			} else {
				var lines []string
				for path, file := range real {
					lines = append(lines, path+" "+file)
				}
				lines = append(lines, destination+" "+original)
				r = manifestResolver(t, lines)
			}
			cfg := vitestConfig()
			cfg.Vitest.Stage[private+"vitest.config.mjs"] = destination + "/vitest.config.mjs"
			cfg.Vitest.Stage[private+"nested/helper.mjs"] = destination + "/nested/helper.mjs"
			scratch := t.TempDir()
			t.Setenv("TEST_TMPDIR", scratch)
			plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
			if plan != nil && len(plan.cleanup) != 0 {
				defer plan.Cleanup()
			}
			if err == nil || !strings.Contains(err.Error(), "beneath non-directory or symlink entry") {
				t.Fatalf("config directory alias was accepted: %v", err)
			}
			for path, content := range contents {
				file := filepath.Join(original, filepath.FromSlash(path))
				if got, err := os.ReadFile(file); err != nil || string(got) != content {
					t.Errorf("config staging changed original %s: %q, %v", path, got, err)
				}
				if info, err := os.Stat(file); err != nil || !os.SameFile(identities[path], info) {
					t.Errorf("config staging replaced original %s: %v", path, err)
				}
			}
			if got, err := os.ReadFile(real[wrapper]); err != nil || string(got) != "previous wrapper" {
				t.Errorf("config staging wrote an earlier destination before rejecting the alias: %q, %v", got, err)
			}
			if files, err := os.ReadDir(scratch); err != nil || len(files) != 0 {
				t.Fatalf("failed config staging left temporary trees: %v, %v", files, err)
			}
		})
	}
}

func TestAdvertiseSharding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status")
	if err := advertiseSharding(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := advertiseSharding(""); err != nil {
		t.Fatal(err)
	}
	if err := advertiseSharding(filepath.Join(path, "missing")); err == nil {
		t.Fatal("failed handshake must not be ignored")
	}
}

func TestStageTestRootRemovesPartialRoot(t *testing.T) {
	for _, failure := range []string{"directory", "symlink"} {
		t.Run(failure, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "test.js")
			if err := os.WriteFile(source, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			scratch := t.TempDir()
			t.Setenv("TEST_TMPDIR", scratch)
			next := "test.js/child.js"
			if failure == "symlink" {
				next = "invalid\x00"
			}
			if _, err := stageTestRoot([]testFile{{"test.js", source}, {next, source}}); err == nil {
				t.Fatal("conflicting stage path must fail")
			}
			if files, err := os.ReadDir(scratch); err != nil || len(files) != 0 {
				t.Fatalf("staging left files behind: %v, %v", files, err)
			}
		})
	}
}

func TestPlanVitestRemovesRootOnFailure(t *testing.T) {
	for _, phase := range []string{"staging", "vitest lookup"} {
		t.Run(phase, func(t *testing.T) {
			r, real := vitestFixture(t)
			cfg := vitestConfig()
			if phase == "staging" {
				if err := os.Remove(real["_main/tests/app/_app.vitest/config.mjs"]); err != nil {
					t.Fatal(err)
				}
			} else {
				cfg.Vitest.VitestInTree = "vitest/missing.mjs"
			}
			scratch := t.TempDir()
			t.Setenv("TEST_TMPDIR", scratch)
			if _, err := MakePlan(cfg, r, nil, Shard{Total: 1}); err == nil {
				t.Fatalf("missing input must fail during %s", phase)
			}
			if files, err := os.ReadDir(scratch); err != nil || len(files) != 0 {
				t.Fatalf("failed plan left files behind: %v, %v", files, err)
			}
		})
	}
}

func TestNativeViewCannotChangeControlSignals(t *testing.T) {
	const fixtureRole = "TS_LAUNCHER_SIGNAL_FIXTURE"
	const fixtureRoot = "TS_LAUNCHER_SIGNAL_ROOT"
	const fixtureMode = "TS_LAUNCHER_SIGNAL_MODE"
	const fixtureConfig = "TS_LAUNCHER_SIGNAL_CONFIG"
	const entry = "_main/entry.js"
	const testPattern = "^TestNativeViewCannotChangeControlSignals$"
	controls := []os.Signal{
		syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM,
		syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGCONT,
	}
	switch os.Getenv(fixtureRole) {
	case "application":
		received := make(chan os.Signal, len(controls))
		signal.Notify(received, controls...)
		fmt.Printf("ready %d %d\n", os.Getpid(), syscall.Getpgrp())
		for range controls {
			fmt.Printf("signal %d\n", (<-received).(syscall.Signal))
		}
		os.Exit(0)
	case "launcher":
		r, err := directoryResolver(os.Getenv(fixtureRoot))
		if err != nil {
			t.Fatal(err)
		}
		cfg := &Config{
			Mode:           os.Getenv(fixtureMode),
			Workspace:      "_main",
			Runtime:        "_main/launcher_test",
			RunArgs:        []string{"-test.run=" + testPattern, "--"},
			RuntimeModules: []string{entry},
			Env:            map[string]string{fixtureRole: "application"},
			Node:           &NodeConfig{Entry: entry},
			NodeTest:       &NodeTestConfig{TestFilesList: "_main/entry_files.txt"},
		}
		cfg.NativeViewAnchor = os.Getenv(fixtureConfig)
		plan, err := MakePlan(cfg, r, nil, Shard{Total: 1})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("staged " + fixtureRuntimeRoot(t, cfg, r, plan))
		code, err := Run(plan)
		if err != nil {
			t.Fatal(err)
		}
		os.Exit(code)
	}

	deadline, bounded := t.Deadline()
	if !bounded {
		t.Fatal("signal subprocesses require the test runner's deadline")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "_main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(root, "_main/launcher_test")); err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{"_main/launcher_test": executable}
	for file, contents := range map[string]string{
		entry:                   "declared module",
		"_main/entry_files.txt": entry + "\n",
	} {
		entries[file] = filepath.Join(root, file)
		if err := os.WriteFile(entries[file], []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{ModeNode, ModeNodeTest} {
		t.Run(mode, func(t *testing.T) {
			resolver, err := directoryResolver(root)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &Config{Mode: mode, Workspace: "_main", RuntimeModules: []string{entry}, Node: &NodeConfig{Entry: entry}, NodeTest: &NodeTestConfig{TestFilesList: "_main/entry_files.txt"}}
			if err := fixtureNativeView(t, cfg, resolver, entries); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithDeadline(t.Context(), deadline)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run="+testPattern)
			cmd.Env = Environ(map[string]string{
				fixtureRole:   "launcher",
				fixtureRoot:   root,
				fixtureMode:   mode,
				fixtureConfig: cfg.NativeViewAnchor,
			}, nil)
			cmd.Stderr = os.Stderr
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			cmd.Cancel = func() error {
				_ = stdout.Close()
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				cancel()
				if !waited {
					_ = cmd.Wait()
				}
			}()
			lines := bufio.NewScanner(stdout)
			readLine := func() string {
				t.Helper()
				if !lines.Scan() {
					t.Fatalf("signal fixture ended before its acknowledgement: %v", lines.Err())
				}
				return lines.Text()
			}
			staged, found := strings.CutPrefix(readLine(), "staged ")
			if !found || staged == "" || staged == root {
				t.Fatalf("launcher did not select the prepared view: %q", staged)
			}

			var runtimePID, runtimeGroup int
			got := readLine()
			if _, err := fmt.Sscanf(got, "ready %d %d", &runtimePID, &runtimeGroup); err != nil || runtimePID != cmd.Process.Pid || runtimeGroup != cmd.Process.Pid {
				t.Fatalf("staging changed the executable PID or process group: %q, launcher %d", got, cmd.Process.Pid)
			}
			for i, control := range controls {
				var err error
				if i%2 == 0 {
					err = cmd.Process.Signal(control)
				} else {
					err = syscall.Kill(-cmd.Process.Pid, control.(syscall.Signal))
				}
				if err != nil {
					t.Fatal(err)
				}
				if got, want := readLine(), fmt.Sprintf("signal %d", control.(syscall.Signal)); got != want {
					t.Fatalf("application received %q after sending %v to its launcher, want %q", got, control, want)
				}
			}
			err = cmd.Wait()
			waited = true
			if err != nil {
				t.Fatalf("application's signal handlers did not exit successfully: %v", err)
			}
			if _, err := os.Stat(filepath.Join(staged, entry)); err != nil {
				t.Fatalf("exec removed immutable module: %v", err)
			}
		})
	}
}

// Bazel may transport a regular File as a symlink to another artifact.
func TestStageRegularFileTransportCannotCreateCanonicalAlias(t *testing.T) {
	for _, layout := range []string{"directory", "manifest"} {
		for _, transport := range []string{"regular", "symlink"} {
			t.Run(layout+"/"+transport, func(t *testing.T) {
				const module = "_main/a.js"
				const data = "_main/b.js"
				const contents = "export const value = 42;\n"
				resolver, inputs := fakeRunfiles(t, map[string]string{module: contents, data: contents})
				if transport == "symlink" {
					if err := os.Remove(inputs[data]); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(inputs[module], inputs[data]); err != nil {
						t.Fatal(err)
					}
				}
				if layout == "directory" {
					directory := t.TempDir()
					for _, name := range []string{module, data} {
						path := filepath.Join(directory, name)
						if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(inputs[name], path); err != nil {
							t.Fatal(err)
						}
					}
					var err error
					resolver, err = directoryResolver(directory)
					if err != nil {
						t.Fatal(err)
					}
				}
				root := t.TempDir()
				if _, err := resolver.Stage(root, []string{module}); err != nil {
					t.Fatal(err)
				}
				canonical, err := os.Stat(filepath.Join(root, module))
				if err != nil {
					t.Fatal(err)
				}
				ordinary, err := os.Stat(filepath.Join(root, data))
				if err != nil {
					t.Fatal(err)
				}
				if os.SameFile(canonical, ordinary) {
					t.Error("regular File transport created an undeclared canonical alias")
				}
				if got, err := os.ReadFile(filepath.Join(root, data)); err != nil || string(got) != contents {
					t.Errorf("ordinary data bytes = %q, %v; want %q", got, err, contents)
				}
				if got, err := os.ReadFile(inputs[module]); err != nil || string(got) != contents {
					t.Errorf("canonical input changed: %q, %v", got, err)
				}
				if transport == "symlink" {
					if target, err := os.Readlink(inputs[data]); err != nil || target != inputs[module] {
						t.Errorf("transport symlink changed: %q, %v", target, err)
					}
				}
			})
		}
	}
}

func TestStageDependencyAliasUsesCanonicalModulePath(t *testing.T) {
	for _, mode := range []string{"manifest", "directory"} {
		t.Run(mode, func(t *testing.T) {
			files := t.TempDir()
			canonical := filepath.Join(files, "dep", "index.js")
			if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(canonical, []byte("export { value } from './value.js';"), 0o644); err != nil {
				t.Fatal(err)
			}
			entries := []string{"_main/dep/index.js " + canonical, "_main/app/dep/index.js " + canonical}
			var resolver *Resolver
			if mode == "manifest" {
				resolver = manifestResolver(t, entries)
			} else {
				directory := t.TempDir()
				for _, entry := range entries {
					name, target, _ := strings.Cut(entry, " ")
					at := filepath.Join(directory, name)
					if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, at); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				resolver, err = directoryResolver(directory)
				if err != nil {
					t.Fatal(err)
				}
			}
			root := t.TempDir()
			if _, err := resolver.Stage(root, []string{"_main/dep/index.js"}); err != nil {
				t.Fatal(err)
			}
			resolved, err := filepath.EvalSymlinks(filepath.Join(root, "_main/app/dep/index.js"))
			want := filepath.Join(resolvedPath(t, root), "_main/dep/index.js")
			if err != nil || resolved != want {
				t.Fatalf("alias resolves to %q, %v; want canonical staged module %q", resolved, err, want)
			}
			if info, err := os.Lstat(want); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("canonical module is not materialized: %v, %v", info, err)
			}
		})
	}
}

func fixtureRuntimeRoot(t *testing.T, cfg *Config, original *Resolver, plan *Plan) string {
	t.Helper()
	if cfg.NativeViewAnchor != "" {
		view, err := nativeResolver(cfg, original, &Plan{})
		if err != nil {
			t.Fatal(err)
		}
		return view.Dir()
	}
	return runfilesEnv(plan.runfilesEnv, "RUNFILES_DIR")
}

func fixtureNativeView(t *testing.T, cfg *Config, original *Resolver, entries map[string]string, contexts ...NpmContext) error {
	t.Helper()
	if cfg.Mode != ModeNode && cfg.Mode != ModeNodeTest {
		return nil
	}
	modules := slices.Clone(cfg.RuntimeModules)
	var chain []string
	var optional []runtimeview.PackageLink
	if cfg.Mode == ModeNode {
		modules = append(modules, cfg.Node.Entry)
		if cfg.Node.NodeModules != "" {
			chain = []string{cfg.Node.NodeModules}
		}
		optional = cfg.Node.OptionalDeps
	} else {
		files, err := testFiles(original, cfg.NodeTest.TestFilesList)
		if err != nil {
			return err
		}
		for _, file := range files {
			modules = append(modules, file.rlocation)
		}
		chain = cfg.NodeTest.NodeModules
	}
	base := t.TempDir()
	spec := runtimeview.Spec{
		Root: filepath.Join(base, "app_launcher.runtime", "view"), Modules: modules,
		Entries: map[string]string{}, Links: map[string]string{}, NpmContexts: contexts, OptionalDeps: optional,
	}
	selected := map[string]string{}
	for _, module := range modules {
		selected[entries[module]] = module
	}
	inputs := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		source := entries[name]
		if source != "" && !filepath.IsAbs(source) {
			spec.Links[name] = filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(name), source)))
			continue
		}
		spec.Entries[name] = source
		if source == "" || inputs[source] {
			continue
		}
		inputs[source] = true
		input := runtimeview.Input{Path: source, Output: filepath.Join(base, "app_launcher.runtime", "files", filepath.FromSlash(name)), Kind: "file"}
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		switch {
		case selected[source] != "":
			input.Kind, input.Target = "alias", filepath.Join(spec.Root, filepath.FromSlash(selected[source]))
		case info.Mode()&os.ModeSymlink != 0:
			input.Kind = "symlink"
		case info.IsDir():
			input.Kind = "directory"
		}
		spec.Inputs = append(spec.Inputs, input)
	}
	if len(chain) > 0 {
		destination := filepath.ToSlash(filepath.Join(cfg.Workspace, "node_modules"))
		available := true
		for name := range entries {
			if name == destination || strings.HasPrefix(name, destination+"/") || strings.HasPrefix(destination, name+"/") {
				available = false
				break
			}
		}
		if available {
			spec.Links[destination] = chain[len(chain)-1]
		}
	}
	if err := runtimeview.Build(spec); err != nil {
		return err
	}
	cfg.RuntimeModules = nil
	cfg.NativeViewAnchor = "_main/launcher_fixture/" + filepath.Base(filepath.Dir(base)) + "/" + filepath.Base(base) + "/app_launcher.json"
	config := filepath.Join(base, "app_launcher.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(config, data, 0o644); err != nil {
		return err
	}
	manifest := runfilesEnv(original.Env(), "RUNFILES_MANIFEST_FILE")
	if manifest != "" {
		file, err := os.OpenFile(manifest, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(file, cfg.NativeViewAnchor+" "+config)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		resolved, err := newResolver(runfiles.ManifestFile(manifest))
		if err != nil {
			return err
		}
		original.rf = resolved.rf
		return nil
	}
	alias, err := original.Path(cfg.NativeViewAnchor)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
		return err
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	return os.Symlink(config, alias)
}

func fixturePlan(t *testing.T, cfg *Config, r *Resolver, entries map[string]string, args []string, shard Shard, contexts ...NpmContext) (*Plan, error) {
	t.Helper()
	if err := fixtureNativeView(t, cfg, r, entries, contexts...); err != nil {
		return nil, err
	}
	return MakePlan(cfg, r, args, shard)
}
