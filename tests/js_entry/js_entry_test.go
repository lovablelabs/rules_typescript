package js_entry_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/mikn/rules_typescript/tests/verify"
	"github.com/mikn/rules_typescript/ts/tools/runtimeview"
)

func TestSourceEntryPointRuns(t *testing.T) {
	tree := verify.New(t)

	runner := tree.File("tests/js_entry/source_data_links_launcher")
	if !runner.Exists() {
		t.FailNow()
	}

	out, err := exec.Command(runner.Abs(), "--check-data-links").CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", runner.Name(), err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "hello, runfiles" {
		t.Errorf("printed %q, want %q", got, "hello, runfiles")
	}

	// Manifest-only runfiles need explicit sibling membership even when a local
	// source tree lets Node find it through the entry's realpath.
	tree.File("tests/js_entry/greet.mjs").Exists()
}

func TestSourceEntryPointGenerates(t *testing.T) {
	tree := verify.New(t)

	generated := tree.File("tests/js_entry/generated.ts")
	generated.Exists()
	generated.Contains(
		`export const greeting: string = "hello, codegen";`,
		"export const nodeBinary: boolean = true;",
		"export const nodeModulesDir: boolean = true;",
	)
}

func TestEntryIdentityPreservesJavaScriptDataSiblings(t *testing.T) {
	for _, identity := range []string{"file", "tsinfo"} {
		t.Run(identity, func(t *testing.T) {
			runner := verify.New(t).File("tests/js_entry/" + identity + "_data_entry_launcher")
			if !runner.Exists() {
				t.FailNow()
			}
			out, err := exec.Command(runner.Abs()).CombinedOutput()
			if err != nil {
				t.Fatalf("%s lost the JavaScript data helper's generated sibling: %v\n%s", runner.Name(), err, out)
			}
			if got := strings.TrimSpace(string(out)); got != "42" {
				t.Errorf("printed %q, want generated payload 42", got)
			}
		})
	}
}

func TestRunfilesLookupCannotSplitNativeModuleIdentity(t *testing.T) {
	tree := verify.New(t)
	for _, mode := range []string{"node", "node_test"} {
		t.Run(mode, func(t *testing.T) {
			var fixture struct {
				Launcher, Manifest, Helper, Module, Package, Data string
				OriginalData                                      string `json:"original_data"`
			}
			data, err := os.ReadFile(tree.Path("tests/js_entry/" + mode + "_runfiles_fixture.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatal(err)
			}
			outer, err := runfiles.New()
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := outer.Rlocation(fixture.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			helper, err := outer.Rlocation(fixture.Helper)
			if err != nil {
				t.Fatal(err)
			}
			originalData, err := outer.Rlocation(fixture.OriginalData)
			if err != nil {
				t.Fatal(err)
			}
			for _, layout := range []string{"directory", "manifest", "inherited"} {
				t.Run(layout, func(t *testing.T) {
					rf := outer
					if layout != "inherited" {
						rf, err = runfiles.New(runfiles.ManifestFile(manifest), runfiles.SourceRepo(""))
						if err != nil {
							t.Fatal(err)
						}
					}
					if layout == "directory" {
						directory := t.TempDir()
						if err := runtimeview.StageManifest(directory, manifest, func(string) bool { return true }, nil); err != nil {
							t.Fatal(err)
						}
						t.Setenv("RUNFILES_MANIFEST_FILE", "")
						rf, err = runfiles.New(runfiles.Directory(directory), runfiles.SourceRepo(""))
						if err != nil {
							t.Fatal(err)
						}
					}
					lookup := func(name string) string {
						t.Helper()
						path, err := rf.Rlocation(name)
						if err != nil {
							t.Fatal(err)
						}
						return path
					}
					keys, err := json.Marshal([]string{fixture.Module, fixture.Package, fixture.Data, "rules_typescript/" + strings.SplitN(fixture.Module, "/", 2)[1]})
					if err != nil {
						t.Fatal(err)
					}
					// A copied launcher cannot discover its own manifest beside the executable and bypass this case's environment.
					program := filepath.Join(t.TempDir(), filepath.Base(fixture.Launcher))
					binary, err := os.ReadFile(lookup(fixture.Launcher))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(program, binary, 0o755); err != nil {
						t.Fatal(err)
					}
					cmd := exec.CommandContext(t.Context(), program)
					cmd.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
						key, _, _ := strings.Cut(entry, "=")
						return slices.Contains([]string{"RUNFILES_DIR", "JAVA_RUNFILES", "RUNFILES_MANIFEST_FILE", "RUNFILES_MANIFEST_ONLY", "TEST_SRCDIR", "TESTBRIDGE_TEST_ONLY", "TEST_TOTAL_SHARDS", "TEST_SHARD_INDEX", "TEST_SHARD_STATUS_FILE"}, key)
					})
					cmd.Env = append(cmd.Env, rf.Env()...)
					cmd.Env = append(cmd.Env,
						"RUNFILES_LOOKUP_HELPER="+helper,
						"RUNFILES_LOOKUP_KEYS="+string(keys),
						"RUNFILES_ORIGINAL_DATA="+originalData,
					)
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("%s runfiles lookup split native module identity: %v\n%s", mode, err, out)
					}
					for _, body := range []string{"runfiles lookup retains repository mapping", "runfiles lookup retains declared data bytes", "runfiles lookup cannot create a second declared module instance", "runfiles lookup cannot create a second npm store instance"} {
						if !strings.Contains(string(out), body) {
							t.Errorf("native runtime did not execute %q:\n%s", body, out)
						}
					}
				})
			}
		})
	}
}
