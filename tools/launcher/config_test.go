package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func TestParseConfigAcceptsVitestContextsWithoutRequiringNewFields(t *testing.T) {
	for _, context := range []string{"", `,"npm_contexts":[{"module":"_main/main.js","source":"shared/main.ts","bindings":{"pkg":"_main/store/pkg"}}]`, `,"npm_contexts":[{"module":"_main/nested/main.js","source":"shared/nested/main.ts","bindings":{},"package_scope":{"manifest":"_main/package.json","bindings":{"pkg":"_main/store/pkg"}}}]`} {
		if _, err := ParseConfig([]byte(`{"mode":"vitest","vitest":{"test_files_list":"_main/tests.txt","config_file":"_main/config.mjs"` + context + `}}`)); err != nil {
			t.Fatalf("optional Vitest context rejected: %v", err)
		}
	}
}

func TestParseConfigRejectsBadDocuments(t *testing.T) {
	cases := map[string]string{
		"missing mode":        `{"label":"//a:b"}`,
		"unknown mode":        `{"label":"//a:b","mode":"bash"}`,
		"node without entry":  `{"label":"//a:b","mode":"node","node":{}}`,
		"node without body":   `{"label":"//a:b","mode":"node"}`,
		"vitest without body": `{"label":"//a:b","mode":"vitest"}`,
		"devserver no config": `{"label":"//a:b","mode":"devserver","dev_server":{}}`,
		// Exactly one of the two server forms: a config naming both leaves which
		// executable actually serves the app up to whichever the launcher reads
		// first, and naming neither has nothing to run at all.
		"devserver both server forms": `{"label":"//a:b","mode":"devserver","dev_server":{"config_file":"c","server_binary":"b","server_in_tree":"vite/bin/vite.js"}}`,
		"devserver no server form":    `{"label":"//a:b","mode":"devserver","dev_server":{"config_file":"c"}}`,
		"unknown field":               `{"label":"//a:b","mode":"node","node":{"entry":"a"},"shell":"bash"}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(doc)); err == nil {
				t.Fatalf("ParseConfig(%s) accepted an invalid config", doc)
			}
		})
	}
}

func TestParseConfigKeepsValuesShellWouldHaveMangled(t *testing.T) {
	doc := `{
	  "label": "//a:b",
	  "mode": "node",
	  "workspace": "_main",
	  "runtime_args": ["--title=$(id -u)", "--x=` + "`" + `whoami` + "`" + `"],
	  "env": {"MSG": "a \"quoted\" $HOME \\ value"},
	  "node": {"entry": "_main/a/b.js"}
	}`
	cfg, err := ParseConfig([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.RunArgs[0], "--title=$(id -u)"; got != want {
		t.Errorf("runtime_args[0] = %q, want %q", got, want)
	}
	if got, want := cfg.Env["MSG"], `a "quoted" $HOME \ value`; got != want {
		t.Errorf("env[MSG] = %q, want %q", got, want)
	}
}

func TestConfigPathFindsSiblingOfArgv0(t *testing.T) {
	t.Setenv(ConfigEnvVar, "")
	dir := t.TempDir()
	launcher := filepath.Join(dir, "app_launcher")
	if err := os.WriteFile(launcher+".json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := configPath(launcher, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != launcher+".json" {
		t.Errorf("configPath = %q, want %q", got, launcher+".json")
	}
}

func TestConfigPathPrefersEnvOverride(t *testing.T) {
	t.Setenv(ConfigEnvVar, "/somewhere/explicit.json")
	got, err := configPath("/ignored/app_launcher", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/somewhere/explicit.json" {
		t.Errorf("configPath = %q, want the %s override", got, ConfigEnvVar)
	}
}

func TestConfigPathErrorNamesWhatItTried(t *testing.T) {
	t.Setenv(ConfigEnvVar, "")
	_, err := configPath(filepath.Join(t.TempDir(), "app_launcher"), nil)
	if err == nil {
		t.Fatal("configPath succeeded with no config on disk")
	}
	if !strings.Contains(err.Error(), "app_launcher.json") || !strings.Contains(err.Error(), ConfigEnvVar) {
		t.Errorf("error is not actionable: %v", err)
	}
}

func TestConfigPathFindsTheRootSymlinkInRunfiles(t *testing.T) {
	t.Setenv(ConfigEnvVar, "")
	r, real := fakeRunfiles(t, map[string]string{
		"app_launcher.json": `{"label":"//a:b","mode":"node","node":{"entry":"x"}}`,
	})
	// argv[0] is an exec-root path with no config beside it, which is how a
	// launcher used as a tool inside another rule's action is invoked.
	got, err := configPath("bazel-out/k8-opt-exec/bin/external/pkg/app_launcher", r)
	if err != nil {
		t.Fatal(err)
	}
	if got != real["app_launcher.json"] {
		t.Errorf("configPath = %q, want %q", got, real["app_launcher.json"])
	}
}

func TestNativeViewCannotFollowDetachedConfigAliases(t *testing.T) {
	const canonical = "+npm+fixture/pkg/app_launcher.json"
	const view = "+npm+fixture/pkg/app_launcher.runtime/view"
	const entry = "_main/app/main.js"
	for _, mode := range []string{ModeNode, ModeNodeTest} {
		for _, layout := range []string{"directory", "manifest"} {
			for _, discovery := range []string{"root", "sibling", "environment"} {
				t.Run(mode+"/"+layout+"/"+discovery, func(t *testing.T) {
					t.Setenv(ConfigEnvVar, "")
					t.Setenv("COVERAGE_DIR", "")
					cfg := &Config{
						Mode:             mode,
						Workspace:        "_main",
						NativeViewAnchor: canonical,
						RuntimeModules:   []string{entry},
						Node:             &NodeConfig{Entry: entry},
						NodeTest:         &NodeTestConfig{TestFilesList: "_main/tests.txt"},
					}
					data, err := json.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					base := t.TempDir()
					group := filepath.Join(base, "outputs")
					for path, contents := range map[string]string{
						canonical:                 string(data),
						view + "/" + entry:        "export {};",
						view + "/_main/tests.txt": entry + "\n",
					} {
						path = filepath.Join(group, filepath.FromSlash(path))
						if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					alias := filepath.Join(base, "app_launcher.json")
					if discovery == "root" {
						alias = filepath.Join(group, "app_launcher.json")
					}
					if err := os.WriteFile(alias, data, 0o644); err != nil {
						t.Fatal(err)
					}
					argv0 := strings.TrimSuffix(alias, ".json")
					if discovery == "root" {
						argv0 = "bazel-out/k8-opt-exec/bin/external/pkg/app_launcher"
					} else if discovery == "environment" {
						t.Setenv(ConfigEnvVar, alias)
						argv0 = "unused_launcher"
					}
					for _, relocated := range []bool{false, true} {
						if relocated {
							moved := filepath.Join(base, "relocated")
							if err := os.Rename(group, moved); err != nil {
								t.Fatal(err)
							}
							group = moved
							if discovery == "root" {
								alias = filepath.Join(group, "app_launcher.json")
							}
						}
						var r *Resolver
						if layout == "directory" {
							r, err = directoryResolver(group)
						} else {
							manifest := filepath.Join(base, "MANIFEST")
							contents := canonical + " " + filepath.Join(group, canonical) + "\n"
							if discovery == "root" {
								contents += "app_launcher.json " + alias + "\n"
							}
							if err := os.WriteFile(manifest, []byte(contents), 0o644); err != nil {
								t.Fatal(err)
							}
							r, err = newResolver(runfiles.ManifestFile(manifest))
						}
						if err != nil {
							t.Fatal(err)
						}
						loaded, path, err := LoadConfig(argv0, r)
						if err != nil || path != alias {
							t.Fatalf("LoadConfig = %q, %v; want detached alias %q", path, err, alias)
						}
						plan, err := MakePlan(loaded, r, nil, Shard{Total: 1})
						if err != nil {
							t.Fatalf("relocated=%t: %v", relocated, err)
						}
						want := filepath.Join(resolvedPath(t, group), view, entry)
						if got := plan.Argv[len(plan.Argv)-1]; got != want {
							t.Fatalf("entry = %q, want canonical grouped output %q", got, want)
						}
						plan.Cleanup()
						if _, err := os.Stat(want); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		}
	}
}
