package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// fakeRunfiles lays out files on disk and returns a resolver backed only by a
// manifest — the layout that broke every hand-rolled ${SCRIPT}.runfiles guess.
func fakeRunfiles(t *testing.T, entries map[string]string) (*Resolver, map[string]string) {
	t.Helper()
	root := t.TempDir()
	real := map[string]string{}
	lines := []string{}
	for rlocation, content := range entries {
		path := filepath.Join(root, "files", filepath.FromSlash(rlocation))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if content == dirMarker {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		real[rlocation] = path
		lines = append(lines, rlocation+" "+path)
	}
	manifest := filepath.Join(root, "MANIFEST")
	if err := os.WriteFile(manifest, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNFILES_DIR", "")
	t.Setenv("TEST_SRCDIR", "")
	t.Setenv("RUNFILES_MANIFEST_FILE", manifest)
	r, err := newResolver(runfiles.ManifestFile(manifest))
	if err != nil {
		t.Fatal(err)
	}
	return r, real
}

const dirMarker = "\x00dir"

func TestResolverWorksFromAManifestAlone(t *testing.T) {
	r, real := fakeRunfiles(t, map[string]string{
		"_main/tests/app/main.js":      "console.log(1)",
		"+npm+npm/node/bin/node":       "#!/bin/sh\n",
		"_main/tests/app/node_modules": dirMarker,
	})

	if r.Dir() != "" {
		t.Errorf("Dir() = %q, want empty: there is no runfiles directory in manifest mode", r.Dir())
	}
	for rlocation, want := range real {
		got, err := r.Path(rlocation)
		if err != nil {
			t.Fatalf("Path(%q): %v", rlocation, err)
		}
		if got != want {
			t.Errorf("Path(%q) = %q, want %q", rlocation, got, want)
		}
	}
}

func TestResolverReportsMissingEntries(t *testing.T) {
	r, _ := fakeRunfiles(t, map[string]string{"_main/a.js": "x"})
	if _, err := r.Path("_main/nope.js"); err == nil {
		t.Fatal("Path succeeded for a path that is not in the manifest")
	} else if !strings.Contains(err.Error(), "_main/nope.js") {
		t.Errorf("error does not name the path: %v", err)
	}
}

func TestResolverEnvCarriesTheManifestToChildren(t *testing.T) {
	r, _ := fakeRunfiles(t, map[string]string{"_main/a.js": "x"})
	joined := strings.Join(r.Env(), " ")
	if !strings.Contains(joined, "RUNFILES_MANIFEST_FILE=") {
		t.Errorf("Env() = %v, want it to carry RUNFILES_MANIFEST_FILE", r.Env())
	}
}

func TestNodeModulesUsesAutoDiscoveredRunfilesWithoutEnvironment(t *testing.T) {
	for _, mode := range []string{"directory", "manifest"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("RUNFILES_DIR", "")
			t.Setenv("RUNFILES_MANIFEST_FILE", "")
			t.Setenv("TEST_SRCDIR", "")
			base := t.TempDir()
			program := filepath.Join(base, "generator")
			rl := "_main/app/node_modules/pkg/package.json"
			tree := program + ".runfiles"
			source := tree
			if mode == "manifest" {
				source = filepath.Join(base, "files")
			}
			file := filepath.Join(source, filepath.FromSlash(rl))
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(`{"name":"pkg"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if mode == "manifest" {
				if err := os.WriteFile(program+".runfiles_manifest", []byte(rl+" "+file+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r, err := newResolver(runfiles.ProgramName(program))
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(base, "staged")
			if _, err := r.Stage(root, nil); err != nil {
				t.Fatal(err)
			}
			plan := &Plan{EnvOverrides: map[string]string{}}
			if _, err := installNodeModules(plan, root, "_main", []string{"_main/app/node_modules"}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rl)))
			if err != nil || string(data) != `{"name":"pkg"}` {
				t.Fatalf("staged package = %q, %v", data, err)
			}
		})
	}
}

func TestAuxiliaryLauncherUsesOwnRunfilesInsteadOfParents(t *testing.T) {
	for _, mode := range []string{"manifest", "manifest_directory", "directory_only"} {
		t.Run(mode, func(t *testing.T) {
			_, parent := fakeRunfiles(t, map[string]string{"_main/tool.js": "parent"})
			root := t.TempDir()
			program := filepath.Join(root, "auxiliary")
			ownFile := filepath.Join(root, "tool.js")
			if err := os.WriteFile(ownFile, []byte("auxiliary"), 0o644); err != nil {
				t.Fatal(err)
			}
			manifest := program + ".runfiles_manifest"
			if mode != "manifest" {
				if err := os.Mkdir(program+".runfiles", 0o755); err != nil {
					t.Fatal(err)
				}
				manifest = filepath.Join(program+".runfiles", "MANIFEST")
			}
			if mode == "directory_only" {
				ownFile = filepath.Join(program+".runfiles", "_main/tool.js")
				if err := os.MkdirAll(filepath.Dir(ownFile), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ownFile, []byte("auxiliary"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(manifest, []byte("_main/tool.js "+ownFile+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := resolverForExecutable(program)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := r.Path("_main/tool.js"); err != nil || got != ownFile {
				t.Fatalf("auxiliary runfile = %q, %v; want %q", got, err, ownFile)
			}
			if (r.Dir() != "") != (mode == "directory_only") {
				t.Fatalf("directory staging changed: %q", r.Dir())
			}
			if mode == "directory_only" {
				for _, invalid := range []string{"../tool.js", "_main/../tool.js", ownFile} {
					if _, err := r.Path(invalid); err == nil {
						t.Fatalf("accepted invalid runfiles path %q", invalid)
					}
				}
				if got := runfilesEnv(r.Env(), "RUNFILES_DIR"); got != program+".runfiles" {
					t.Fatalf("child runfiles directory = %q", got)
				}
				if got := runfilesEnv(Environ(nil, r.Env()), "RUNFILES_MANIFEST_FILE"); got != "" {
					t.Fatalf("child still inherits parent manifest: %q", got)
				}
			}
			inherited, err := resolverForExecutable(filepath.Join(root, "no_own_runfiles"))
			if err != nil {
				t.Fatal(err)
			}
			if got, err := inherited.Path("_main/tool.js"); err != nil || got != parent["_main/tool.js"] {
				t.Fatalf("inherited fallback = %q, %v", got, err)
			}
		})
	}
}
