package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installerFixture(t *testing.T, content, outputRoot string) (workspace, artifact string) {
	t.Helper()
	root := t.TempDir()
	workspace = filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact = filepath.Join(root, "output.json")
	manifest := filepath.Join(root, "copy.json")
	files := filepath.Join(root, "runfiles_manifest")
	entries, err := json.Marshal([]entry{{Rlocation: "_main/output.json", Dest: "project.json", OutputRoot: outputRoot}})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		artifact: []byte(content), manifest: entries,
		files: []byte(fmt.Sprintf("manifest %s\n_main/output.json %s\n", manifest, artifact)),
	} {
		if err := os.WriteFile(name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BUILD_WORKSPACE_DIRECTORY", workspace)
	t.Setenv("COPY_TO_WORKSPACE_MANIFEST", "manifest")
	t.Setenv("RUNFILES_MANIFEST_FILE", files)
	t.Setenv("RUNFILES_DIR", "")
	return workspace, artifact
}

func TestRunPublishesWithoutTruncatingOpenProjects(t *testing.T) {
	for _, name := range []string{"file", "symlink"} {
		t.Run(name, func(t *testing.T) {
			const before = `{"files":["previous.ts"]}`
			const after = `{"files":["current.ts"]}`
			workspace, artifact := installerFixture(t, after, "")
			dest := filepath.Join(workspace, "project.json")
			target := dest
			if name == "symlink" {
				target = filepath.Join(workspace, "target.json")
				if err := os.Symlink("target.json", dest); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(target, []byte(before), 0o640); err != nil {
				t.Fatal(err)
			}
			mode, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := os.Open(dest)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if err := run(); err != nil {
				t.Fatal(err)
			}
			if got, err := io.ReadAll(reader); err != nil || string(got) != before {
				t.Fatalf("publication changed an already-open project: %q, %v", got, err)
			}
			if got, err := os.ReadFile(dest); err != nil || string(got) != after {
				t.Fatalf("new reader did not receive the complete project: %q, %v", got, err)
			}
			if got, err := os.Stat(target); err != nil || got.Mode() != mode.Mode() {
				t.Fatalf("publication changed destination mode: %v, %v", got, err)
			}
			if name == "symlink" {
				if got, err := os.Readlink(dest); err != nil || got != "target.json" {
					t.Fatalf("publication replaced destination symlink: %q, %v", got, err)
				}
			}
			if err := os.Remove(artifact); err != nil {
				t.Fatal(err)
			}
			if err := run(); err == nil {
				t.Fatal("missing replacement artifact was accepted")
			}
			if got, err := os.ReadFile(dest); err != nil || string(got) != after {
				t.Fatalf("failed refresh changed the installed project: %q, %v", got, err)
			}
		})
	}
	for _, test := range []struct {
		name, rootDir, outputOptions, sources, rejectOption string
	}{
		{"generated_source_widens_without_output_context", ".", `"outDir":null,"declarationDir":null`, `["bazel-out/value.ts"]`, ""},
		{"active_outdir_preserves_previous_project", ".", `"outDir":"./dist","declarationDir":null`, `["bazel-out/value.ts"]`, "outDir"},
		{"active_declarationdir_preserves_previous_project", ".", `"outDir":null,"declarationDir":"./types"`, `["bazel-out/value.ts"]`, "declarationDir"},
		{"inherited_outdir_preserves_previous_project", ".", `"declarationDir":null`, `["bazel-out/value.ts"]`, "outDir"},
		{"inherited_declarationdir_preserves_previous_project", ".", `"outDir":null`, `["bazel-out/value.ts"]`, "declarationDir"},
		{"equal_root_keeps_output_context", "..", `"outDir":"./dist","declarationDir":"./types"`, `["bazel-out/value.ts"]`, ""},
		{"ancestor_root_keeps_output_context", "../..", `"outDir":"./dist","declarationDir":"./types"`, `["bazel-out/value.ts"]`, ""},
		{"empty_program_and_config_do_not_widen_root", ".", `"outDir":"./dist","declarationDir":"./types"`, `[]`, ""},
		{"missing_containment_facts_preserves_previous_project", "..", `"outDir":null,"declarationDir":null`, "", "must be an array"},
		{"null_containment_facts_preserves_previous_project", "..", `"outDir":null,"declarationDir":null`, `null`, "must be an array"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sourceFacts := ""
			if test.sources != "" {
				sourceFacts = `,"rootDirSources":` + test.sources
			}
			content := fmt.Sprintf(`{"compilerOptions":{"rootDir":%q,%s},"extends":["bazel-out/baseline.json"],"files":[]%s}`, test.rootDir, test.outputOptions, sourceFacts)
			workspace, _ := installerFixture(t, content, "bazel-out")
			dest := filepath.Join(workspace, "project.json")
			const previous = `{"files":["previous.ts"]}`
			if err := os.WriteFile(dest, []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
			err := run()
			if test.rejectOption != "" {
				if err == nil || !strings.Contains(err.Error(), "rootDir") || !strings.Contains(err.Error(), test.rejectOption) {
					t.Fatalf("accepted or misreported an incompatible resolver context: %v", err)
				}
				if got, readErr := os.ReadFile(dest); readErr != nil || string(got) != previous {
					t.Fatalf("resolver context conflict replaced the previous project: %q, %v", got, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			installed, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				CompilerOptions map[string]any `json:"compilerOptions"`
			}
			if err := json.Unmarshal(installed, &config); err != nil {
				t.Fatal(err)
			}
			wantRoot := test.rootDir
			if wantRoot == "." && test.sources != `[]` {
				physical, err := filepath.EvalSymlinks(workspace)
				if err != nil {
					t.Fatal(err)
				}
				wantRoot = filepath.ToSlash(filepath.VolumeName(physical) + string(filepath.Separator))
			}
			if got := config.CompilerOptions["rootDir"]; got != wantRoot {
				t.Fatalf("canonical output has the wrong containing root: got %v, want %s", got, wantRoot)
			}
			var original struct {
				CompilerOptions map[string]any `json:"compilerOptions"`
			}
			if err := json.Unmarshal([]byte(content), &original); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"outDir", "declarationDir"} {
				if got, want := config.CompilerOptions[key], original.CompilerOptions[key]; got != want {
					t.Fatalf("publication changed %s: got %v, want %v", key, got, want)
				}
			}
			if strings.Contains(string(installed), "rootDirSources") {
				t.Fatal("installation retained internal compiler containment facts")
			}
		})
	}
}

func TestRunWorkspaceAliasesCannotWidenCompilerRootOrChangeRelativeOptions(t *testing.T) {
	for _, test := range []struct {
		name, rootDir, outputs string
	}{
		{"generated_source_widens", ".", `"outDir":null,"declarationDir":null`},
		{"equal_root_preserves_outputs", "..", `"outDir":"./dist","declarationDir":"./types"`},
		{"ancestor_root_preserves_outputs", "../..", `"outDir":"./dist","declarationDir":"./types"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := fmt.Sprintf(`{"compilerOptions":{"rootDir":%q,%s},"files":["./authored.ts","bazel-out/value.ts"],"rootDirSources":["bazel-out/value.ts"]}`, test.rootDir, test.outputs)
			workspace, artifact := installerFixture(t, content, "bazel-out")
			physical, err := filepath.EvalSymlinks(workspace)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"dist", "types"} {
				if err := os.Mkdir(filepath.Join(physical, path), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{filepath.Join(physical, "authored.ts"), filepath.Join(filepath.Dir(artifact), "value.ts")} {
				if err := os.WriteFile(path, []byte("export {};\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			aliases := []string{physical}
			for _, name := range []string{"first", "second"} {
				alias := filepath.Join(t.TempDir(), name)
				if err := os.Symlink(physical, alias); err != nil {
					t.Fatal(err)
				}
				aliases = append(aliases, alias)
			}
			var previous string
			for _, alias := range aliases {
				t.Setenv("BUILD_WORKSPACE_DIRECTORY", alias)
				if err := run(); err != nil {
					t.Fatalf("workspace %s: %v", alias, err)
				}
				installed, err := os.ReadFile(filepath.Join(physical, "project.json"))
				if err != nil {
					t.Fatal(err)
				}
				if previous != "" && string(installed) != previous {
					t.Fatalf("workspace alias changed installed project:\n%s\nprevious:\n%s", installed, previous)
				}
				previous = string(installed)
				var config struct {
					CompilerOptions map[string]any `json:"compilerOptions"`
					Files           []string       `json:"files"`
				}
				if err := json.Unmarshal(installed, &config); err != nil {
					t.Fatal(err)
				}
				wantRoot := test.rootDir
				if wantRoot == "." {
					wantRoot = filepath.ToSlash(filepath.VolumeName(physical) + string(filepath.Separator))
				}
				if config.CompilerOptions["rootDir"] != wantRoot {
					t.Fatalf("rootDir = %v, want %s", config.CompilerOptions["rootDir"], wantRoot)
				}
				for key, relative := range map[string]string{"outDir": "./dist", "declarationDir": "./types"} {
					if test.rootDir == "." {
						if config.CompilerOptions[key] != nil {
							t.Fatalf("root widening introduced %s = %v", key, config.CompilerOptions[key])
						}
						continue
					}
					if config.CompilerOptions[key] != relative {
						t.Fatalf("%s = %v, want retained relative option %s", key, config.CompilerOptions[key], relative)
					}
					got, err := filepath.EvalSymlinks(filepath.Join(alias, relative))
					if err != nil || got != filepath.Join(physical, relative) {
						t.Fatalf("%s changed filesystem identity through %s: %s, %v", key, alias, got, err)
					}
				}
				wantGenerated := filepath.ToSlash(filepath.Join(filepath.Dir(physical), "value.ts"))
				if len(config.Files) != 2 || config.Files[0] != "./authored.ts" || config.Files[1] != wantGenerated {
					t.Fatalf("workspace binding changed source identities: %v", config.Files)
				}
			}
		})
	}
}

func TestRunFailedPublicationRemovesTemporaryFile(t *testing.T) {
	workspace, _ := installerFixture(t, `{"files":[]}`, "")
	dest := filepath.Join(workspace, "project.json")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dest, "existing.json")
	const original = `{"files":["previous.ts"]}`
	if err := os.WriteFile(marker, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("publication replaced a directory")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != original {
		t.Fatalf("failed publication changed the previous destination: %q, %v", got, err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil || len(entries) != 1 || entries[0].Name() != "project.json" {
		t.Fatalf("failed publication left temporary files: %v, %v", entries, err)
	}
}

func TestRunKeepsDanglingDestinationSymlink(t *testing.T) {
	const content = `{"files":[]}`
	workspace, _ := installerFixture(t, content, "")
	dest := filepath.Join(workspace, "project.json")
	if err := os.Symlink("target.json", dest); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(dest); err != nil || got != "target.json" {
		t.Fatalf("publication replaced dangling destination symlink: %q, %v", got, err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != content {
		t.Fatalf("publication did not create the linked project: %q, %v", got, err)
	}
}
