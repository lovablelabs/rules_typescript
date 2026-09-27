package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

func editorTestCompiler(t *testing.T) string {
	t.Helper()
	compiler, err := runfiles.Rlocation(os.Getenv("TSGO_RLOCATION"))
	if err != nil {
		t.Fatal(err)
	}
	compiler, err = filepath.Abs(compiler)
	if err != nil {
		t.Fatal(err)
	}
	return compiler
}

func editorTestChdir(t *testing.T) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(directory)
}

func editorTestFileIdentity(t *testing.T, file string) string {
	t.Helper()
	absolute, err := filepath.Abs(file)
	if err != nil {
		t.Fatal(err)
	}
	return realpath(t, absolute)
}

func editorTestListing(t *testing.T, compiler, project string, codes ...string) *explainfiles.Listing {
	t.Helper()
	output, runErr := exec.CommandContext(t.Context(), compiler, "-p", project, "--noEmit", "--skipDefaultLibCheck", "--explainFiles", "--pretty", "false").CombinedOutput()
	listing, err := explainfiles.Parse(string(output))
	if err != nil {
		t.Fatal(err)
	}
	if (runErr != nil) != (len(codes) != 0) || len(listing.Diagnostics) != len(codes) {
		t.Fatalf("%s diagnostics, want %v: %v\n%s", project, codes, runErr, output)
	}
	for i, code := range codes {
		if !strings.Contains(listing.Diagnostics[i], "error "+code+":") {
			t.Fatalf("%s diagnostic, want %s: %s", project, code, listing.Diagnostics[i])
		}
	}
	return listing
}

func editorTestMkdir(t *testing.T, directories ...string) {
	t.Helper()
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
}

func editorTestRemove(t *testing.T, file string) {
	t.Helper()
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
}

func editorTestSymlinkInput(t *testing.T, file string) {
	t.Helper()
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(t.TempDir(), filepath.FromSlash(file))
	writeFile(t, physical, string(content))
	editorTestRemove(t, file)
	if err := os.Symlink(physical, file); err != nil {
		t.Fatal(err)
	}
}

func editorTestWriteJSON(t *testing.T, file string, value any) {
	t.Helper()
	editorTestMkdir(t, filepath.Dir(file))
	if err := writeJSON(file, value); err != nil {
		t.Fatal(err)
	}
}

func editorTestConfigArgs(compiler, project, build string, inputs ...string) []string {
	options := filepath.ToSlash(filepath.Join(filepath.Dir(build), "options.json"))
	args := []string{"-tsgo=" + compiler, "-tsconfig=" + project, "-baseline=baseline.json", "-out=" + build, "-options=" + options, "-bin_dir=" + binDir}
	return append(args, inputs...)
}

func editorTestCommand(compiler, project string) []string {
	return []string{compiler, "-p", project, "--noEmit", "--skipDefaultLibCheck", "--traceResolution", "--explainFiles", "--pretty", "false"}
}

func editorTestInstaller(t *testing.T) string {
	t.Helper()
	installer, err := runfiles.Rlocation(os.Getenv("COPY_TO_WORKSPACE_RLOCATION"))
	if err != nil {
		t.Fatal(err)
	}
	installer, err = filepath.Abs(installer)
	if err != nil {
		t.Fatal(err)
	}
	return installer
}

func editorTestInstall(t *testing.T, installer, editor string) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(cwd, binDir, "editor-fixture.json")
	content, err := os.ReadFile(editor)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, artifact, string(content))
	fixture := t.TempDir()
	manifest, mappings := filepath.Join(fixture, "copy.json"), filepath.Join(fixture, "runfiles_manifest")
	editorTestWriteJSON(t, manifest, []map[string]string{{"rlocation": "_main/editor-fixture.json", "dest": editor, "output_root": binDir}})
	writeFile(t, mappings, "manifest "+manifest+"\n_main/editor-fixture.json "+artifact+"\n")
	command := exec.CommandContext(t.Context(), installer)
	command.Env = append(os.Environ(), "BUILD_WORKSPACE_DIRECTORY="+cwd, "COPY_TO_WORKSPACE_MANIFEST=manifest", "RUNFILES_MANIFEST_FILE="+mappings, "RUNFILES_DIR=")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("installing editor project: %v\n%s", err, output)
	}
}

func TestEditorInstalledNoEmitPreventsFalseExtensionDiagnostic(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name            string
		noEmit          any
		declarationOnly bool
	}{
		{name: "noEmit_absent"},
		{name: "noEmit_false", noEmit: false},
		{name: "authored_declaration_only", noEmit: false, declarationOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			editorTestChdir(t)
			writeFile(t, "baseline.json", `{}`)
			options := map[string]any{"strict": true, "module": "preserve", "allowImportingTsExtensions": true, "paths": map[string][]string{"#generated": {"../gen/value.d.ts"}}}
			if test.noEmit != nil {
				options["noEmit"] = test.noEmit
			}
			if test.declarationOnly {
				options["declaration"] = true
				options["emitDeclarationOnly"] = true
			}
			editorTestWriteJSON(t, "app/tsconfig.json", map[string]any{"compilerOptions": options, "files": []string{"consumer.ts", "local.ts"}})
			writeFile(t, "app/consumer.ts", "import { value } from './local.ts';\nimport type { Generated } from '#generated';\nexport const result: Generated = value;\n")
			writeFile(t, "app/local.ts", "export const value: string = 'authored';\n")
			writeFile(t, binDir+"/gen/value.d.ts", "export type Generated = string;\n")
			build, editor := binDir+"/app/program.tsconfig.json", "app/.bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(build))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "app/consumer.ts", "app/local.ts"))
			editorTestListing(t, compiler, build)
			original := map[string]string{}
			for _, file := range []string{"app/tsconfig.json", build} {
				contents, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				original[file] = string(contents)
			}
			editorTestMkdir(t, filepath.Dir(editor))
			args := []string{"-root=" + binDir + "/editor-program", "-tsconfig=app/tsconfig.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor, "-source=baseline.json", "-source=app/tsconfig.json", "-source=app/consumer.ts", "-source=app/local.ts", "-editor-generated-file=gen/value.d.ts", "--"}
			if err := runTsgo(append(args, editorTestCommand(compiler, build)...)); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.CommandContext(t.Context(), compiler, "--skipDefaultLibCheck", "-p", editor, "--pretty", "false").CombinedOutput(); err != nil {
				t.Fatalf("installed editor lost the checked no-emit program: %v\n%s", err, output)
			}
			effective, _, err := showConfig(compiler, editor)
			if err != nil {
				t.Fatal(err)
			}
			for option, want := range map[string]bool{"noEmit": true, "allowImportingTsExtensions": true, "strict": true, "emitDeclarationOnly": false} {
				var actual any
				if err := json.Unmarshal(effective.compilerOptions[option], &actual); err != nil || actual != want {
					t.Fatalf("installed editor changed %s: %s, %v", option, effective.compilerOptions[option], err)
				}
			}
			for file, contents := range original {
				if actual, err := os.ReadFile(file); err != nil || string(actual) != contents {
					t.Fatalf("editor changed build input %s: %v", file, err)
				}
			}
		})
	}
}

func TestEditorInstalledWorkspaceAliasCannotExcludeAuthoredOrGeneratedSources(t *testing.T) {
	compiler := editorTestCompiler(t)
	installer := editorTestInstaller(t)
	aliases := t.TempDir()
	for _, test := range []struct {
		name               string
		composite, outside bool
	}{
		{name: "ordinary_inside"},
		{name: "ordinary_outside", outside: true},
		{name: "composite_inside", composite: true},
		{name: "composite_outside", composite: true, outside: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			editorTestChdir(t)
			workspace, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if test.outside {
				editorTestMkdir(t, filepath.Dir(binDir))
				if err := os.Symlink(t.TempDir(), binDir); err != nil {
					t.Fatal(err)
				}
			}
			generated := binDir + "/generated/value.ts"
			writeFile(t, generated, "export const value = 'generated' as const;\n")
			writeFile(t, "generated/value.ts", "export const value = 'checkout' as const;\n")
			consumer := func(want string) {
				writeFile(t, "authored.ts", "import { value } from '#generated';\nexport const result: "+want+" = value;\n")
			}
			consumer("'generated'")
			editorTestWriteJSON(t, "base.json", map[string]any{"compilerOptions": map[string]any{
				"rootDir": "./restricted", "composite": test.composite, "strict": true,
				"module": "preserve", "moduleResolution": "bundler", "sourceMap": true, "sourceRoot": "./sources",
			}})
			const editor = "project.json"
			editorTestWriteJSON(t, editor, map[string]any{
				"extends": []string{"./base.json"},
				"compilerOptions": map[string]any{
					"noEmit": true, "rootDir": ".", "outDir": nil, "declarationDir": nil,
					"paths": map[string][]string{"#generated": {"./" + generated}},
				},
				"files":          []string{"./authored.ts", generated},
				"rootDirSources": []string{generated},
			})
			editorTestInstall(t, installer, editor)
			alias := filepath.Join(aliases, test.name)
			if err := os.Symlink(workspace, alias); err != nil {
				t.Fatal(err)
			}
			wantRoots := []string{editorTestFileIdentity(t, "authored.ts"), editorTestFileIdentity(t, generated)}
			check := func(directory string, codes ...string) {
				t.Helper()
				listing := editorTestListing(t, compiler, filepath.Join(directory, editor), codes...)
				var roots []string
				for _, file := range listing.Roots {
					roots = append(roots, editorTestFileIdentity(t, file))
				}
				slices.Sort(roots)
				if !slices.Equal(roots, wantRoots) {
					t.Fatalf("installed project changed authored/generated root identities through %s: got %v, want %v", directory, roots, wantRoots)
				}
			}
			slices.Sort(wantRoots)
			check(workspace)
			check(alias)
			effective, _, err := showConfig(compiler, filepath.Join(alias, editor))
			if err != nil {
				t.Fatal(err)
			}
			for option, want := range map[string]bool{"composite": test.composite, "noEmit": true} {
				var got bool
				if raw := effective.compilerOptions[option]; len(raw) != 0 {
					if err := json.Unmarshal(raw, &got); err != nil {
						t.Fatal(err)
					}
				}
				if got != want {
					t.Fatalf("installed alias changed %s: got %t, want %t", option, got, want)
				}
			}
			consumer("number")
			check(workspace, "TS2322")
			check(alias, "TS2322")
		})
	}
}

func TestEditorPackageGeneratedTargetsPreserveOutputAuthority(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, field, specifier                                  string
		scalar, canonical, alias, failedPaths, authoredFallback bool
		packageOptions                                          map[string]bool
		scope                                                   string
		acceptScope, unnamed, empty                             bool
	}{
		{name: "imports_tree", field: "imports", specifier: "#generated"},
		{name: "exports_tree", field: "exports", specifier: "fixture/generated"},
		{name: "imports_scalar", field: "imports", specifier: "#generated", scalar: true},
		{name: "exports_scalar", field: "exports", specifier: "fixture/generated", scalar: true},
		{name: "canonical_imports", field: "imports", specifier: "#generated", canonical: true},
		{name: "canonical_exports", field: "exports", specifier: "fixture/generated", canonical: true},
		{name: "unobserved_imports_tree", field: "imports", specifier: "#alias", alias: true},
		{name: "unobserved_self_reference_scalar", field: "exports", specifier: "#alias", alias: true, scalar: true},
		{name: "empty_imports_cannot_block_generated_alias", field: "imports", specifier: "#alias", alias: true, scope: "preserved", acceptScope: true, empty: true},
		{name: "empty_named_exports_cannot_block_generated_alias", field: "exports", specifier: "#alias", alias: true, scope: "preserved", acceptScope: true, empty: true},
		{name: "disabled_imports_allow_alias_with_exports_enabled", field: "imports", specifier: "#alias", alias: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": true}, scope: "preserved", acceptScope: true},
		{name: "disabled_exports_allow_alias_with_imports_enabled", field: "exports", specifier: "#alias", alias: true, packageOptions: map[string]bool{"resolvePackageJsonImports": true, "resolvePackageJsonExports": false}, scope: "preserved", acceptScope: true, unnamed: true},
		{name: "disabled_maps_allow_imports_metadata", field: "imports", specifier: "#alias", alias: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": false}, scope: "preserved", acceptScope: true},
		{name: "disabled_maps_allow_exports_metadata", field: "exports", specifier: "#alias", alias: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": false}, scope: "preserved", acceptScope: true, unnamed: true},
		{name: "enabled_imports_still_reject_with_exports_disabled", field: "imports", specifier: "#alias", alias: true, packageOptions: map[string]bool{"resolvePackageJsonImports": true, "resolvePackageJsonExports": false}, scope: "preserved"},
		{name: "enabled_exports_still_reject_with_imports_disabled", field: "exports", specifier: "#alias", alias: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": true}, scope: "preserved"},
		{name: "disabled_exports_cannot_hide_unobserved_self_reference", field: "exports", specifier: "#alias", alias: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": false}, scope: "preserved"},
		{name: "disabled_exports_cannot_hide_self_reference", field: "exports", specifier: "fixture/generated", packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": false}},
		{name: "disabled_maps_cannot_lose_package_scope", field: "imports", specifier: "#alias", alias: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": false}, scope: "missing"},
		{name: "disabled_maps_accept_owner_as_built_scope", field: "exports", specifier: "#alias", alias: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": false}, scope: "changed", acceptScope: true, unnamed: true},
		{name: "failed_paths_imports_tree", field: "imports", specifier: "#generated", failedPaths: true},
		{name: "failed_paths_exports_tree", field: "exports", specifier: "fixture/generated", failedPaths: true},
		{name: "failed_paths_imports_scalar", field: "imports", specifier: "#generated", failedPaths: true, scalar: true},
		{name: "failed_paths_exports_scalar", field: "exports", specifier: "fixture/generated", failedPaths: true, scalar: true},
		{name: "failed_paths_canonical_imports", field: "imports", specifier: "#generated", failedPaths: true, canonical: true},
		{name: "failed_paths_canonical_exports", field: "exports", specifier: "fixture/generated", failedPaths: true, canonical: true},
		{name: "failed_paths_imports_authored_fallback", field: "imports", specifier: "#generated", failedPaths: true, authoredFallback: true},
		{name: "failed_paths_exports_authored_fallback", field: "exports", specifier: "fixture/generated", failedPaths: true, authoredFallback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "app/consumer.ts", "import { value } from '"+test.specifier+"';\nexport const result: string = value;\n")
			options := map[string]any{"strict": true, "module": "preserve", "moduleResolution": "bundler"}
			for option, enabled := range test.packageOptions {
				options[option] = enabled
			}
			if test.alias {
				target := "./generated/value.d.ts"
				if test.scalar {
					target = "../" + binDir + "/app/generated/value.d.ts"
				}
				options["paths"] = map[string][]string{"#alias": {target}}
			}
			if test.failedPaths {
				options["paths"] = map[string][]string{test.specifier: {"./nonexistent.d.ts"}}
			}
			if test.canonical {
				if options["paths"] == nil {
					options["paths"] = map[string][]string{}
				}
				options["paths"].(map[string][]string)["#entry"] = []string{"../" + binDir + "/app/generated/entry.js"}
			}
			editorTestWriteJSON(t, "app/tsconfig.json", map[string]any{"compilerOptions": options, "files": []string{"consumer.ts"}})
			generated := "app/generated/value.d.ts"
			canonical := binDir + "/" + generated
			writeFile(t, canonical, "export declare const value: string;\n")
			const stale = "export declare const value: number;\n"
			writeFile(t, generated, stale)
			key, target, manifest := "#generated", "./generated/value.d.ts", "app/package.json"
			if test.field == "exports" {
				key = "./generated"
			}
			consumer := "app/consumer.ts"
			if test.canonical {
				manifest = binDir + "/app/generated/package.json"
				target = "./value.d.ts"
				consumer = binDir + "/app/generated/entry.d.ts"
				writeFile(t, consumer, "export { value } from '"+test.specifier+"';\n")
				writeFile(t, "app/consumer.ts", "import { value } from '#entry';\nexport const result: string = value;\n")
			}
			var packageTarget any = target
			if test.authoredFallback {
				packageTarget = []string{target, "./fallback.d.ts"}
				writeFile(t, "app/fallback.d.ts", "export declare const value: string;\n")
			}
			metadata := map[string]any{"name": "fixture", "type": "module", test.field: map[string]any{key: packageTarget}}
			missingSpecifier := "#generated"
			if test.field == "exports" {
				missingSpecifier = "fixture/generated"
			}
			if test.empty {
				metadata[test.field] = map[string]any{}
				writeFile(t, "app/consumer.ts", "import { value } from '"+test.specifier+"';\nexport const result: string = value;\nimport { missing } from '"+missingSpecifier+"';\nexport const unresolved = missing;\n")
			}
			if test.unnamed {
				delete(metadata, "name")
			}
			editorTestWriteJSON(t, manifest, metadata)
			if test.scope == "preserved" || test.scope == "changed" {
				raw, err := os.ReadFile(manifest)
				if err != nil {
					t.Fatal(err)
				}
				contents := string(raw)
				if test.scope == "changed" {
					contents = strings.Replace(contents, `"module"`, `"commonjs"`, 1)
				}
				writeFile(t, binDir+"/"+manifest, contents)
			}
			build := binDir + "/app/program.tsconfig.json"
			editor := "app/.bazel/tsconfig/project.json"
			const installed = "previous editor project\n"
			writeFile(t, editor, installed)
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "app/consumer.ts"))
			compiledOptions := readJSON(t, build)["compilerOptions"].(map[string]any)
			for option, enabled := range test.packageOptions {
				if compiledOptions[option] != enabled {
					t.Fatalf("resolved compiler configuration changed %s: %v", option, compiledOptions[option])
				}
			}
			root := binDir + "/editor-program"
			sources := []string{"baseline.json", "app/tsconfig.json", "app/consumer.ts"}
			if !test.canonical {
				sources = append(sources, manifest)
			}
			if test.authoredFallback {
				sources = append(sources, "app/fallback.d.ts")
			}
			a := actionConfig{project: "app/tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor}
			if test.scalar {
				a.generatedFiles = []string{generated}
			} else {
				a.generatedDirectories = []string{"app/generated"}
			}
			command := editorTestCommand(compiler, build)
			checkMissingQuery := func(listing *explainfiles.Listing) {
				t.Helper()
				if test.empty && (!slices.ContainsFunc(listing.Diagnostics, func(diagnostic string) bool {
					return strings.Contains(diagnostic, "error TS2307:") && strings.Contains(diagnostic, "'"+missingSpecifier+"'")
				}) || slices.ContainsFunc(listing.Edges, func(edge explainfiles.Edge) bool { return edge.Specifier == missingSpecifier })) {
					t.Fatalf("empty %s changed failed query %s: %+v", test.field, missingSpecifier, listing)
				}
			}
			for _, missing := range []bool{false, true} {
				if missing {
					editorTestRemove(t, canonical)
					if !test.canonical {
						editorTestRemove(t, filepath.Dir(canonical))
					}
				}
				a.out = ""
				if err := a.layOutProgramRoot(root, sources, nil, nil, overlayPairs([]string{binDir + "/app/generated"}...), nil); err != nil {
					t.Fatal(err)
				}
				codes := []string{}
				if missing && !test.authoredFallback {
					codes = []string{"TS2307"}
				}
				if test.empty {
					codes = append(codes, "TS2307")
				}
				t.Chdir(root)
				listing := editorTestListing(t, compiler, build, codes...)
				checkMissingQuery(listing)
				t.Chdir(cwd)
				selected := ""
				for _, edge := range listing.Edges {
					if edge.Kind.ModuleSpecifier() && edge.Specifier == test.specifier {
						selected = throughRoot(filepath.Join(cwd, root), cwd, canonicalDir(cwd), edge.To)
					}
				}
				want := canonical
				if missing {
					want = ""
					if test.authoredFallback {
						want = "app/fallback.d.ts"
					}
				}
				if selected != want {
					t.Fatalf("missing=%t: compiler selected %q, want %q", missing, selected, want)
				}
				a.out = build
				err := editorRun(root, command, a, editor)
				if test.canonical || test.alias && (missing || test.acceptScope) {
					if err != nil {
						t.Fatalf("missing=%t: supported resolution rejected: %v", missing, err)
					}
					listing := editorTestListing(t, compiler, editor, codes...)
					checkMissingQuery(listing)
					if slices.Contains(listing.Files, generated) || !missing && !slices.Contains(listing.Files, canonical) {
						t.Fatalf("missing=%t: editor lost canonical output authority: %+v", missing, listing)
					}
					if test.empty && !missing && !slices.Contains(listing.Edges, explainfiles.Edge{From: "app/consumer.ts", Specifier: test.specifier, To: canonical}) {
						t.Fatalf("empty %s changed generated definition identity: %v", test.field, listing.Edges)
					}
				} else if test.scope == "missing" {
					if err == nil || !strings.Contains(err.Error(), "package scope is not preserved") {
						t.Fatalf("disabled maps lost the package scope preservation guard: %v", err)
					}
					if got, readErr := os.ReadFile(editor); readErr != nil || string(got) != installed {
						t.Fatalf("scope rejection replaced the installed project: %q, %v", got, readErr)
					}
				} else if test.alias {
					if err == nil || !strings.Contains(err.Error(), "package-relative resolution context") {
						t.Fatalf("accepted an unobserved package import in relocated scope: %v", err)
					}
					if got, readErr := os.ReadFile(editor); readErr != nil || string(got) != installed {
						t.Fatalf("scope rejection replaced the installed project: %q, %v", got, readErr)
					}
				} else {
					if err == nil {
						t.Fatalf("missing=%t: accepted package resolution through a stale checkout twin", missing)
					}
					for _, required := range []string{editor, test.specifier, consumer, "generated output", "stale checkout twin", "paths", "authored editor project", "ordinary builds and type checking remain supported"} {
						if !strings.Contains(err.Error(), required) {
							t.Fatalf("missing=%t: rejection omits %q: %v", missing, required, err)
						}
					}
					if got, err := os.ReadFile(editor); err != nil || string(got) != installed {
						t.Fatalf("rejection replaced the installed project: %q, %v", got, err)
					}
					if test.name == "imports_tree" && !missing {
						editorTestListing(t, compiler, "app/tsconfig.json", "TS2322")
					}
				}
				if got, err := os.ReadFile(generated); err != nil || string(got) != stale {
					t.Fatalf("refresh changed the checkout twin: %q, %v", got, err)
				}
			}
		})
	}
	for _, test := range []struct {
		name, outerScope                                        string
		authoredChild, authoredMember, scalar, renamed, outside bool
		output, configInScope, canonical                        bool
	}{
		{name: "generated_scope_cannot_lose_authored_child", authoredChild: true},
		{name: "generated_scope_cannot_leave_authored_module_context", authoredMember: true},
		{name: "scalar_scope_cannot_lose_authored_child", authoredChild: true, scalar: true},
		{name: "renamed_scope_cannot_escape_canonical_namespace", renamed: true},
		{name: "complete_generated_tree_keeps_package_import", output: true},
		{name: "complete_generated_scalars_keep_package_import", scalar: true, output: true},
		{name: "generated_tree_scope_shadows_outer_authored_imports", outerScope: "imports"},
		{name: "generated_scalar_scope_shadows_outer_authored_exports", outerScope: "exports", scalar: true, canonical: true},
		{name: "scalar_scope_cannot_change_config_context", scalar: true, output: true, configInScope: true},
		{name: "authored_sibling_outside_scope_does_not_block_package_import", outside: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			editorTestChdir(t)
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, "baseline.json", `{}`)
			entry, child, scope := "app/generated/entry.d.ts", "app/generated/authored.d.ts", "app/generated/package.json"
			manifest := scope
			if test.renamed {
				manifest = "app/generated/owner.package.json"
			}
			metadata := map[string]any{"type": "module", "imports": map[string]string{"#dep": "./authored.d.ts"}}
			specifier := "#dep"
			if test.outerScope == "exports" {
				metadata["name"] = "generated-fixture"
				metadata["exports"] = map[string]string{"./value": "./authored.d.ts"}
				specifier = "generated-fixture/value"
			}
			entryText := "export { value } from '" + specifier + "';\n"
			files := []string{"consumer.mts"}
			sources := []string{"baseline.json", "app/tsconfig.json", "app/consumer.mts"}
			if test.outerScope != "" {
				key := "#dep"
				if test.outerScope == "exports" {
					key = "./value"
				}
				editorTestWriteJSON(t, "app/package.json", map[string]any{"name": "outer-fixture", "type": "module", test.outerScope: map[string]string{key: "./unrelated.d.ts"}})
				sources = append(sources, "app/package.json")
			}
			if test.authoredMember {
				delete(metadata, "imports")
				entryText = "export declare const value: string;\n"
				files = append(files, "generated/retained.ts")
				sources = append(sources, "app/generated/retained.ts")
				writeFile(t, "app/generated/retained.ts", "export const url: string = import.meta.url;\n")
			}
			if test.outside {
				files = append(files, "outside.ts")
				sources = append(sources, "app/outside.ts")
				writeFile(t, "app/outside.ts", "export const outside: string = 'authored';\n")
			}
			options := map[string]any{"strict": true, "module": "nodenext", "moduleResolution": "nodenext", "paths": map[string][]string{"#entry": {"./generated/entry.d.ts"}}}
			if test.canonical {
				options["paths"] = map[string][]string{"#entry": {"../" + binDir + "/" + entry}}
			}
			if test.output {
				options["outDir"] = "./dist"
			}
			editorTestWriteJSON(t, "app/tsconfig.json", map[string]any{"compilerOptions": options, "files": files})
			writeFile(t, "app/consumer.mts", "import { value } from '#entry';\nexport const result: string = value;\n")
			writeFile(t, binDir+"/"+entry, entryText)
			selected := binDir + "/" + child
			if test.authoredChild {
				selected = child
				sources = append(sources, child)
			}
			writeFile(t, selected, "export declare const value: string;\n")
			editorTestWriteJSON(t, binDir+"/"+manifest, metadata)
			build, editor := binDir+"/app/program.tsconfig.json", "app/.bazel/tsconfig/project.json"
			if test.configInScope {
				build, editor = binDir+"/app/generated/program.tsconfig.json", "app/generated/.bazel/tsconfig/project.json"
			}
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "app/consumer.mts"))
			root := binDir + "/app/editor-program"
			manifests := []string{}
			if test.renamed {
				manifests = append(manifests, binDir+"/"+manifest)
			}
			overlays := []string{binDir + "/app/generated"}
			layoutScope := scope
			if test.canonical {
				overlays = nil
				layoutScope = binDir + "/" + scope
			}
			if err := layOutProgramRoot(root, sources, nil, nil, overlayPairs(overlays...), manifests); err != nil {
				t.Fatal(err)
			}
			if got, want := realpath(t, filepath.Join(cwd, root, layoutScope)), realpath(t, filepath.Join(cwd, binDir, manifest)); got != want {
				t.Fatalf("compiler package scope selected %s, want producer manifest %s", got, want)
			}
			checkPackageChild := func(listing *explainfiles.Listing) {
				t.Helper()
				for _, edge := range listing.Edges {
					if edge.Specifier == specifier && throughRoot(filepath.Join(cwd, root), cwd, canonicalDir(cwd), edge.To) == selected {
						return
					}
				}
				t.Fatalf("compiler did not select package child %s through %s: %+v", selected, specifier, listing)
			}
			t.Chdir(root)
			listing := editorTestListing(t, compiler, build)
			t.Chdir(cwd)
			if !test.authoredMember {
				checkPackageChild(listing)
			} else if !slices.Contains(listing.Files, "app/generated/retained.ts") {
				t.Fatalf("ordinary compiler omitted authored member inheriting generated module scope: %+v", listing)
			}
			const previous = "previous installed project\n"
			writeFile(t, editor, previous)
			args := []string{"-root=" + root, "-tsconfig=app/tsconfig.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor}
			for _, overlay := range overlays {
				args = append(args, overlayFlag(overlay))
			}
			for _, source := range sources {
				args = append(args, "-source="+source)
			}
			for _, manifest := range manifests {
				args = append(args, "-manifest="+manifest)
			}
			if test.scalar {
				for _, file := range []string{entry, manifest, child} {
					if file != child || !test.authoredChild {
						args = append(args, "-editor-generated-file="+file)
					}
				}
			} else {
				args = append(args, "-editor-generated-directory=app/generated")
			}
			args = append(args, "--")
			args = append(args, editorTestCommand(compiler, build)...)
			err = runTsgo(args)
			if test.authoredChild || test.authoredMember || test.renamed || test.configInScope {
				reason := "namespace"
				if test.configInScope {
					reason = "config placement"
				}
				if err == nil || !strings.Contains(err.Error(), "package scope") || !strings.Contains(err.Error(), reason) {
					t.Fatalf("accepted a generated scope without a compatible canonical namespace: %v", err)
				}
				if actual, readErr := os.ReadFile(editor); readErr != nil || string(actual) != previous {
					t.Fatalf("namespace rejection replaced the previous project: %q, %v", actual, readErr)
				}
			} else {
				if err != nil {
					t.Fatalf("complete generated namespace rejected: %v", err)
				}
				listing = editorTestListing(t, compiler, editor)
				checkPackageChild(listing)
				if !slices.Contains(listing.Files, binDir+"/"+entry) || !slices.Contains(listing.Files, selected) || slices.Contains(listing.Files, child) {
					t.Fatalf("complete canonical package namespace changed: %+v", listing)
				}
			}
		})
	}
}

func TestEditorConfigInputsCannotBecomeAuthoredPackageMembers(t *testing.T) {
	compiler := editorTestCompiler(t)
	installer := editorTestInstaller(t)
	for _, test := range []struct {
		name, extra string
		loaded      bool
	}{
		{name: "config_only"},
		{name: "declared_but_unloaded_module", extra: "pkg/unloaded.ts"},
		{name: "config_is_loaded_json", extra: "pkg/tsconfig.build.json", loaded: true},
		{name: "authored_module_with_unloaded_generated_peer", extra: "pkg/retained.ts", loaded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			editorTestChdir(t)
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			const project = "pkg/tsconfig.build.json"
			const value = "pkg/value.ts"
			const scope = "pkg/package.json"
			writeFile(t, "baseline.json", `{}`)
			files := []string{"value.ts"}
			if test.loaded {
				if test.extra == project {
					files = append(files, "tsconfig.build.json")
				} else {
					files = []string{"retained.ts"}
				}
			}
			editorTestWriteJSON(t, project, map[string]any{
				"compilerOptions": map[string]any{"strict": true, "module": "nodenext", "moduleResolution": "nodenext", "resolveJsonModule": true, "types": []string{}},
				"files":           files,
			})
			writeFile(t, binDir+"/"+value, "export const value: string = 'generated';\n")
			writeFile(t, binDir+"/"+scope, `{"type":"module"}`)
			sources := []string{"baseline.json", project}
			if test.extra != "" && test.extra != project {
				content := "export const unused: string = 0;\n"
				if test.loaded {
					content = "export const retained: string = import.meta.url;\n"
				}
				writeFile(t, test.extra, content)
				sources = append(sources, test.extra)
			}
			build, editor := binDir+"/pkg/program.tsconfig.json", "pkg/.bazel/tsconfig/project.json"
			root := binDir + "/pkg/editor-program"
			rootInput := value
			if !slices.Contains(files, "value.ts") {
				rootInput = test.extra
			}
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, project, build, "-source_only", rootInput))
			if err := layOutProgramRoot(root, sources, nil, nil, overlayPairs([]string{binDir + "/pkg"}...), nil); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			ordinary := editorTestListing(t, compiler, build)
			t.Chdir(cwd)
			generated, authored := false, false
			for _, file := range ordinary.Files {
				selected := throughRoot(filepath.Join(cwd, root), cwd, canonicalDir(cwd), file)
				generated = generated || selected == binDir+"/"+value
				authored = authored || selected == test.extra
				if test.extra != "" && !test.loaded && selected == test.extra {
					t.Fatalf("ordinary compiler loaded the excluded authored module: %+v", ordinary)
				}
			}
			if generated != slices.Contains(files, "value.ts") || authored != test.loaded {
				t.Fatalf("ordinary compiler membership differs from the fixture: generated=%t, config=%t, listing=%+v", generated, authored, ordinary)
			}
			const previous = "previous installed project\n"
			writeFile(t, editor, previous)
			args := []string{"-root=" + root, "-tsconfig=" + project, "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor, overlayFlag(binDir + "/pkg"), "-editor-generated-file=" + value, "-editor-generated-file=" + scope}
			for _, source := range sources {
				args = append(args, "-source="+source)
			}
			args = append(args, "--")
			args = append(args, editorTestCommand(compiler, build)...)
			err = runTsgo(args)
			if test.loaded {
				if err == nil || !strings.Contains(err.Error(), "mixed package namespace") || !strings.Contains(err.Error(), test.extra) {
					t.Fatalf("compiler-loaded authored input escaped membership validation: %v", err)
				}
				if actual, readErr := os.ReadFile(editor); readErr != nil || string(actual) != previous {
					t.Fatalf("namespace rejection replaced the previous project: %q, %v", actual, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("config-only inputs rejected the generated program: %v", err)
			}
			editorTestInstall(t, installer, editor)
			installed := editorTestListing(t, compiler, editor)
			if len(installed.Roots) != 1 || editorTestFileIdentity(t, installed.Roots[0]) != editorTestFileIdentity(t, binDir+"/"+value) {
				t.Fatalf("installed program lost its sole generated root: %+v", installed)
			}
			writeFile(t, binDir+"/"+value, "export const value: string = 0;\n")
			editorTestListing(t, compiler, editor, "TS2322")
		})
	}
}

func TestEditorNpmFallbackPreservesImporterResolutionModes(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, alias  string
		consumers    []string
		augmentation bool
	}{
		{"imports", "choice", []string{"consumer.mts", "consumer.cts"}, false},
		{"wildcard", "*", []string{"consumer.mts", "consumer.cts"}, false},
		{"import_and_augmentation", "choice", []string{"consumer.mts", "consumer.cts"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			editorTestChdir(t)
			writeFile(t, "baseline.json", `{}`)
			files := slices.Clone(test.consumers)
			if test.augmentation {
				files = append(files, "consumer-model.cts")
			}
			editorTestWriteJSON(t, "tsconfig.json", map[string]any{
				"compilerOptions": map[string]any{"strict": true, "module": "nodenext", "moduleResolution": "nodenext", "paths": map[string][]string{test.alias: {"./generated/*", "./missing"}}},
				"files":           files,
			})
			writeFile(t, "node_modules/choice/package.json", `{"name":"choice","exports":{"import":"./import.d.mts","require":"./require.d.cts"}}`)
			for _, file := range []string{"node_modules/choice/import.d.mts", "node_modules/choice/require.d.cts"} {
				writeFile(t, file, "export declare const value: string;\nexport interface Model { value: string; }\n")
			}
			want := map[string]string{}
			for _, file := range test.consumers {
				writeFile(t, file, "import { value } from 'choice';\nexport const result: string = value;\n")
				want[file] = "node_modules/choice/import.d.mts"
				if file == "consumer.cts" {
					want[file] = "node_modules/choice/require.d.cts"
				}
			}
			if test.augmentation {
				writeFile(t, "consumer.cts", "export {};\ndeclare module 'choice' { interface Model { extra: string } }\n")
				writeFile(t, "consumer-model.cts", "import type { Model } from 'choice';\nexport const model: Model = { value: 'valid', extra: 'augmented' };\n")
				delete(want, "consumer.cts")
				want["consumer-model.cts"] = "node_modules/choice/require.d.cts"
			}
			build := binDir + "/program.tsconfig.json"
			editor := ".bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(build), filepath.Dir(editor))
			args := append(editorTestConfigArgs(compiler, "tsconfig.json", build), files...)
			mustWriteTsconfig(t, args)
			check := func(project string) {
				t.Helper()
				listing := editorTestListing(t, compiler, project)
				actual := map[string]string{}
				for _, edge := range listing.Edges {
					if edge.Kind == explainfiles.Import && edge.Specifier == "choice" {
						actual[edge.From] = edge.To
					}
				}
				if !reflect.DeepEqual(actual, want) {
					t.Fatalf("%s changed npm fallback destinations: %v, want %v", project, actual, want)
				}
				t.Logf("%s selected=%v", project, actual)
			}
			check(build)
			command := editorTestCommand(compiler, build)
			if err := editorRun(".", command, actionConfig{project: "tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor, generatedDirectories: []string{"generated"}}, editor); err != nil {
				t.Fatal(err)
			}
			check(editor)
		})
	}
	for _, moduleType := range []string{"module", "commonjs"} {
		t.Run("generated_declaration_"+moduleType, func(t *testing.T) {
			editorTestChdir(t)
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, "baseline.json", `{}`)
			manifest := "app/package.json"
			writeFile(t, manifest, `{"type":"`+moduleType+`"}`)
			writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"nodenext","moduleResolution":"nodenext","outDir":"./dist","paths":{"#generated":["./generated/index.js"]}},"files":["consumer.mts"]}`)
			branch := "import"
			if moduleType == "commonjs" {
				branch = "require"
			}
			writeFile(t, "app/consumer.mts", "import { value } from '#generated';\nexport const result: '"+branch+"' = value;\n")
			generated := "app/generated/index.d.ts"
			writeFile(t, binDir+"/"+generated, "import type { Branch } from 'choice';\nexport declare const value: Branch;\n")
			writeFile(t, "node_modules/choice/package.json", `{"name":"choice","exports":{"import":"./import.d.mts","require":"./require.d.cts"}}`)
			writeFile(t, "node_modules/choice/import.d.mts", "export type Branch = 'import';\n")
			writeFile(t, "node_modules/choice/require.d.cts", "export type Branch = 'require';\n")
			for _, link := range []struct{ source, target string }{{"node_modules", binDir + "/node_modules"}, {manifest, binDir + "/" + manifest}} {
				if err := os.Symlink(filepath.Join(cwd, link.source), link.target); err != nil {
					t.Fatal(err)
				}
			}
			build, editor := binDir+"/app/program.tsconfig.json", "app/.bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(editor))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "app/consumer.mts"))
			root := binDir + "/app/editor-program"
			sources := []string{"baseline.json", "app/tsconfig.json", "app/consumer.mts", manifest}
			a := actionConfig{project: "app/tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor, generatedDirectories: []string{"app/generated"}}
			check := func(project string) {
				t.Helper()
				listing := editorTestListing(t, compiler, project)
				found := false
				for _, edge := range listing.Edges {
					if edge.Specifier == "choice" {
						found = true
						if !strings.HasSuffix(edge.To, "/"+branch+map[string]string{"import": ".d.mts", "require": ".d.cts"}[branch]) {
							t.Fatalf("%s changed generated declaration resolution mode: %+v", project, edge)
						}
					}
				}
				if !found {
					t.Fatalf("%s did not resolve generated declaration's conditional npm types", project)
				}
			}
			for _, state := range []string{"clean_refresh", "after_ordinary_program"} {
				if err := a.layOutProgramRoot(root, sources, []string{binDir + "/node_modules"}, nil, overlayPairs([]string{binDir + "/app/generated"}...), nil); err != nil {
					t.Fatal(err)
				}
				if state == "after_ordinary_program" {
					t.Chdir(root)
					check(build)
					t.Chdir(cwd)
				}
				if err := editorRun(root, editorTestCommand(compiler, build), a, editor); err != nil {
					t.Fatalf("%s: %v", state, err)
				}
				check(editor)
				for _, file := range []string{manifest, binDir + "/" + manifest} {
					if raw, err := os.ReadFile(file); err != nil || string(raw) != `{"type":"`+moduleType+`"}` {
						t.Fatalf("%s changed package scope provenance: %q, %v", state, raw, err)
					}
				}
			}
		})
	}
}

func TestEditorAuthoredAliasesKeepNativeSuccessAndFailureWithGeneratedInputs(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, resolution := range []string{"node16", "nodenext", "bundler"} {
		for _, suffixes := range []string{"", `[".native"]`, `[".native", ""]`} {
			t.Run(resolution+"/"+suffixes, func(t *testing.T) {
				t.Chdir(t.TempDir())
				suffix := ""
				if strings.Contains(suffixes, ".native") {
					suffix = ".native"
				}
				suffixOption := ""
				if suffixes != "" {
					suffixOption = `,"moduleSuffixes":` + suffixes
				}
				module := resolution
				if resolution == "bundler" {
					module = "preserve"
				}
				writeFile(t, "baseline.json", `{}`)
				writeFile(t, "tsconfig.json", `{"compilerOptions":{"strict":true,"module":"`+module+`","moduleResolution":"`+resolution+`"`+suffixOption+`,"paths":{"#local/*":["./src/*","./fallback/*"],"#mode":["./src/mode"],"#missing/*":["./absent/*"],"#canonical":["./`+binDir+`/generated/canonical.js","./fallback/canonical.js"],"#generated":["./`+binDir+`/generated/value.js"]}},"files":["consumer.cts","consumer.mts"],"include":["./`+binDir+`/generated/value`+suffix+`.ts"]}`)
				for _, file := range []string{"src/value" + suffix + ".ts", "fallback/value" + suffix + ".ts", "src/mode" + suffix + ".ts", "fallback/canonical" + suffix + ".ts"} {
					writeFile(t, file, "export const value: string = 'authored';\n")
				}
				generated := "generated/value" + suffix + ".ts"
				canonical := "generated/canonical" + suffix + ".d.ts"
				writeFile(t, binDir+"/"+canonical, "export declare const value: string;\n")
				writeFile(t, canonical, "export declare const value: boolean;\n")
				writeFile(t, binDir+"/"+generated, "export const value: string = 'generated';\n")
				for _, file := range []string{"consumer.cts", "consumer.mts"} {
					writeFile(t, file, "import { value } from '#local/value.js';\nimport { value as mode } from '#mode';\nimport { value as missing } from '#missing/value.js';\nimport { value as generated } from '#generated';\nimport { value as canonical } from '#canonical';\nexport const result: string[] = [value, mode, missing, generated, canonical];\n")
				}
				build := binDir + "/program.tsconfig.json"
				editor := ".bazel/tsconfig/project.json"
				editorTestMkdir(t, filepath.Dir(editor))
				args := editorTestConfigArgs(compiler, "tsconfig.json", build)
				if resolution != "bundler" {
					args = append(args, "-module="+module)
				}
				mustWriteTsconfig(t, append(args, "consumer.cts", "consumer.mts", generated))
				check := func(project, local, selectedCanonical string) {
					t.Helper()
					codes := []string{"TS2307", "TS2307"}
					if resolution != "bundler" {
						codes = append(codes, "TS2307")
					}
					listing := editorTestListing(t, compiler, project, codes...)
					actual := map[string]string{}
					want := map[string]string{}
					for _, file := range []string{"consumer.cts", "consumer.mts"} {
						want[file+":#local/value.js"] = local
						want[file+":#generated"] = binDir + "/" + generated
						want[file+":#canonical"] = selectedCanonical
						if file == "consumer.cts" || resolution == "bundler" {
							want[file+":#mode"] = "src/mode" + suffix + ".ts"
						}
					}
					for _, edge := range listing.Edges {
						if edge.Kind.ModuleSpecifier() {
							actual[edge.From+":"+edge.Specifier] = edge.To
						}
					}
					if !reflect.DeepEqual(actual, want) {
						t.Fatalf("%s changed native alias success/failure: got %v, want %v", project, actual, want)
					}
				}
				check(build, "src/value"+suffix+".ts", binDir+"/"+canonical)
				command := editorTestCommand(compiler, build)
				if err := editorRun(".", command, actionConfig{project: "tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor, generatedFiles: []string{generated, canonical}}, editor); err != nil {
					t.Fatal(err)
				}
				check(editor, "src/value"+suffix+".ts", binDir+"/"+canonical)
				installed, err := os.ReadFile(editor)
				if err != nil {
					t.Fatal(err)
				}
				editorTestRemove(t, binDir+"/"+canonical)
				for _, project := range []string{build, editor} {
					check(project, "src/value"+suffix+".ts", "fallback/canonical"+suffix+".ts")
				}
				writeFile(t, binDir+"/"+canonical, "export declare const value: string;\n")
				for _, project := range []string{build, editor} {
					check(project, "src/value"+suffix+".ts", binDir+"/"+canonical)
				}
				editorTestRemove(t, "src/value"+suffix+".ts")
				check(build, "fallback/value"+suffix+".ts", binDir+"/"+canonical)
				check(editor, "fallback/value"+suffix+".ts", binDir+"/"+canonical)
				if got, err := os.ReadFile(editor); err != nil || string(got) != string(installed) {
					t.Fatalf("native fallback transitions changed the installed project: %v", err)
				}
			})
		}
	}
}

func TestEditorDeclaredAuthoredDeclarationKeepsIdentityBesideEmittedOwner(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name        string
		files, tree bool
	}{
		{"scalar_files", true, false},
		{"scalar_include", false, false},
		{"partial_directory", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, "baseline.json", `{}`)
			config := map[string]any{
				"compilerOptions": map[string]any{"strict": true, "module": "preserve", "paths": map[string][]string{
					"#legacy":    {"../lib/value.d.ts"},
					"#local":     {"../lib/local"},
					"#local-js":  {"../lib/local.js"},
					"#lib/*":     {"../lib/*"},
					"#src/*":     {"../*"},
					"#emitted":   {"../" + binDir + "/lib/value.js"},
					"#generated": {"../lib/stale.d.ts"},
				}},
				"include": []string{"consumer.ts", "../lib/*.d.ts"},
				"exclude": []string{"../lib/excluded.d.ts"},
			}
			if test.files {
				config["files"] = []string{"consumer.ts", "../lib/value.d.ts"}
			}
			editorTestWriteJSON(t, "app/tsconfig.json", config)
			const consumer = "import { other } from '../lib/other';\nimport { value } from '#emitted';\nimport { stale } from '#generated';\nimport { local as exact } from '#local';\nimport { local as js } from '#local-js';\nimport { local as wildcard } from '#lib/local';\nimport { local as wildcardJS } from '#lib/local.js';\nimport { local as broad } from '#src/lib/local';\nimport { local as broadJS } from '#src/lib/local.js';\nexport const result: string[] = [legacyGlobal, other, value, stale, exact, js, wildcard, wildcardJS, broad, broadJS];\n"
			writeFile(t, "app/consumer.ts", consumer)
			writeFile(t, "lib/value.ts", "export const value: string = 'emitted';\n")
			writeFile(t, "lib/other.ts", "export const other: string = 'other';\n")
			for _, name := range []string{"value", "other"} {
				writeFile(t, binDir+"/lib/"+name+".d.ts", "export declare const "+name+": string;\n")
			}
			const authored = "declare const legacyGlobal: string;\n"
			writeFile(t, "lib/value.d.ts", authored)
			writeFile(t, "lib/local.d.ts", "export declare const local: string;\n")
			writeFile(t, binDir+"/lib/local.d.ts", "export declare const local: number;\nexport declare const generatedOnly: boolean;\n")
			writeFile(t, "lib/excluded.d.ts", "declare const excluded: Missing;\n")
			writeFile(t, "lib/stale.d.ts", "export declare const stale: boolean;\n")
			for _, file := range []string{"stale", "excluded"} {
				writeFile(t, binDir+"/lib/"+file+".d.ts", "export declare const "+file+": string;\n")
			}
			build := binDir + "/app/program.tsconfig.json"
			editor := "app/.bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(build), filepath.Dir(editor))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "-type_input="+binDir+"/lib/value.d.ts", "-type_input="+binDir+"/lib/other.d.ts", "app/consumer.ts", "lib/value.d.ts", "lib/excluded.d.ts"))
			aliases := false
			wildcardDeclaration := "lib/value.d.ts"
			check := func(project, other string, deleted bool) {
				t.Helper()
				codes := []string{}
				if deleted {
					codes = []string{"TS2307"}
				}
				listing := editorTestListing(t, compiler, project, codes...)
				if !slices.Contains(listing.Roots, "lib/value.d.ts") {
					t.Fatalf("%s lost authored ambient root identity: %+v", project, listing)
				}
				for _, file := range []string{"lib/excluded.d.ts", "lib/stale.d.ts", binDir + "/lib/excluded.d.ts"} {
					if slices.Contains(listing.Files, file) {
						t.Fatalf("%s read excluded or undeclared input %s: %+v", project, file, listing)
					}
				}
				actual := map[string]string{}
				for _, edge := range listing.Edges {
					if edge.From == "app/consumer.ts" && edge.Kind.ModuleSpecifier() {
						actual[edge.Specifier] = edge.To
					}
				}
				if aliases && project == build {
					wildcardDeclaration = actual["#lib/value.d.ts"]
					if wildcardDeclaration != "lib/value.d.ts" && wildcardDeclaration != "lib/value.ts" {
						t.Fatalf("compiler wildcard alias escaped its declared authored siblings: %v", actual)
					}
					t.Logf("compiler selected #lib/value.d.ts -> %s before refresh", wildcardDeclaration)
				}
				want := map[string]string{"../lib/other": other, "#emitted": binDir + "/lib/value.d.ts"}
				for _, specifier := range []string{"#local", "#local-js", "#lib/local", "#lib/local.js", "#src/lib/local", "#src/lib/local.js"} {
					want[specifier] = "lib/local.d.ts"
				}
				if aliases {
					want["#legacy"] = "lib/value.d.ts"
					want["#lib/value.d.ts"] = wildcardDeclaration
				}
				if !deleted {
					want["#generated"] = binDir + "/lib/stale.d.ts"
				}
				if !reflect.DeepEqual(actual, want) {
					t.Fatalf("%s changed authored or canonical alias identity: got %v, want %v", project, actual, want)
				}
			}
			root := binDir + "/app/editor-program"
			sources := []string{"baseline.json", "app/tsconfig.json", "app/consumer.ts", "lib/value.d.ts", "lib/local.d.ts", "lib/excluded.d.ts"}
			if err := layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			check(build, binDir+"/lib/other.d.ts", false)
			t.Chdir(cwd)
			args := []string{"-root=" + root, "-tsconfig=app/tsconfig.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor}
			for _, file := range append(sources, "lib/value.ts", "lib/other.ts") {
				args = append(args, "-source="+file)
			}
			if test.tree {
				args = append(args, "-editor-generated-directory=lib")
			} else {
				for _, file := range []string{"value", "other", "stale", "excluded"} {
					args = append(args, "-editor-generated-file=lib/"+file+".d.ts")
				}
			}
			args = append(args, "--")
			args = append(args, editorTestCommand(compiler, build)...)
			if err := runTsgo(args); err != nil {
				t.Fatal(err)
			}
			check(editor, "lib/other.ts", false)
			editorTestRemove(t, binDir+"/lib/local.d.ts")
			check(editor, "lib/other.ts", false)
			writeFile(t, "app/consumer.ts", consumer+"import '#legacy';\nimport '#lib/value.d.ts';\n")
			aliases = true
			if err := layOutProgramRoot(root, append(sources, "lib/value.ts", "lib/other.ts"), nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			check(build, "lib/other.ts", false)
			t.Chdir(cwd)
			if err := runTsgo(args); err != nil {
				t.Fatal(err)
			}
			check(editor, "lib/other.ts", false)
			editorTestRemove(t, binDir+"/lib/stale.d.ts")
			check(editor, "lib/other.ts", true)
			writeFile(t, binDir+"/lib/stale.d.ts", "export declare const stale: string;\n")
			check(editor, "lib/other.ts", false)
			if got, err := os.ReadFile("lib/value.d.ts"); err != nil || string(got) != authored {
				t.Fatalf("refresh changed authored declaration: %q, %v", got, err)
			}
		})
	}
}

func TestEditorAliasProjectionCannotTurnFailedImportsIntoSuccessfulImports(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, module, resolution, target             string
		consumers                                    []string
		suppressed, failed, rejected, inferred, tree bool
	}{
		{"nodenext_failed_esm", "nodenext", "nodenext", "./generated/value", []string{"consumer.cts", "consumer.mts"}, false, true, true, false, false},
		{"nodenext_suppressed_failed_esm", "nodenext", "nodenext", "./generated/value", []string{"consumer.cts", "consumer.mts"}, true, true, true, false, false},
		{"node16_failed_esm", "node16", "node16", "./generated/value", []string{"consumer.cts", "consumer.mts"}, false, true, true, false, false},
		{"nodenext_valid_commonjs", "nodenext", "nodenext", "./generated/value", []string{"consumer.cts"}, false, false, true, false, false},
		{"nodenext_valid_explicit_extension", "nodenext", "nodenext", "./generated/value.ts", []string{"consumer.cts", "consumer.mts"}, false, false, false, false, false},
		{"node16_valid_explicit_extension", "node16", "node16", "./generated/value.ts", []string{"consumer.cts", "consumer.mts"}, false, false, false, false, false},
		{"bundler_valid_both_modes", "preserve", "bundler", "./generated/value", []string{"consumer.cts", "consumer.mts"}, false, false, false, false, false},
		{"nodenext_inferred_failed_esm", "nodenext", "nodenext", "./generated/value", []string{"consumer.cts", "consumer.mts"}, false, true, true, true, false},
		{"node16_inferred_failed_esm", "node16", "node16", "./generated/value", []string{"consumer.cts", "consumer.mts"}, false, true, true, true, false},
		{"bundler_inferred_both_modes", "preserve", "bundler", "./generated/value", []string{"consumer.cts", "consumer.mts"}, false, false, false, true, false},
		{"tree_nodenext_failed_esm", "nodenext", "nodenext", "./generated/value", []string{"consumer.cts", "consumer.mts"}, false, true, false, false, true},
		{"tree_nodenext_suppressed_failed_esm", "nodenext", "nodenext", "./generated/value", []string{"consumer.cts", "consumer.mts"}, true, true, false, false, true},
		{"tree_node16_inferred_failed_esm", "node16", "node16", "./generated/value", []string{"consumer.cts", "consumer.mts"}, false, true, false, true, true},
		{"tree_nodenext_valid_explicit_extension", "nodenext", "nodenext", "./generated/value.ts", []string{"consumer.cts", "consumer.mts"}, false, false, false, false, true},
		{"tree_node16_valid_explicit_extension", "node16", "node16", "./generated/value.ts", []string{"consumer.cts", "consumer.mts"}, false, false, false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeFile(t, "baseline.json", `{}`)
			candidates := []string{test.target}
			if test.target == "./generated/value.ts" {
				candidates = []string{"./preferred/value.ts", test.target, "./fallback/value.ts"}
			}
			options := map[string]any{"strict": true, "module": test.module, "paths": map[string][]string{"choice": candidates}}
			if !test.inferred {
				options["moduleResolution"] = test.resolution
			}
			editorTestWriteJSON(t, "tsconfig.json", map[string]any{
				"compilerOptions": options,
				"files":           test.consumers,
				"include":         []string{"./" + binDir + "/generated/value.ts"},
			})
			for _, file := range test.consumers {
				body := "import { value } from 'choice';\nexport const result: string = value;\n"
				if file == "consumer.mts" && test.suppressed {
					body = "// @ts-ignore\n" + body
				}
				writeFile(t, file, body)
			}
			generated := "generated/value.ts"
			writeFile(t, binDir+"/"+generated, "export const value: string = 'generated';\n")
			build := binDir + "/program.tsconfig.json"
			editor := ".bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(editor))
			args := editorTestConfigArgs(compiler, "tsconfig.json", build)
			if test.module != "preserve" {
				args = append(args, "-module="+test.module)
			}
			args = append(args, test.consumers...)
			args = append(args, generated)
			if err := writeTsconfig(args); err != nil {
				t.Fatalf("ordinary TsConfig rejected the program: %v", err)
			}
			selectedFile := binDir + "/" + generated
			selected := func(project string, wantFailure bool) map[string]string {
				t.Helper()
				codes := []string{}
				if wantFailure && !test.suppressed {
					codes = append(codes, "TS2307")
				}
				listing := editorTestListing(t, compiler, project, codes...)
				actual := map[string]string{}
				for _, edge := range listing.Edges {
					if edge.Kind == explainfiles.Import && edge.Specifier == "choice" {
						actual[edge.From] = edge.To
					}
				}
				want := map[string]string{}
				for _, file := range test.consumers {
					if file != "consumer.mts" || !wantFailure {
						want[file] = selectedFile
					}
				}
				if !reflect.DeepEqual(actual, want) {
					t.Fatalf("%s changed successful/failed alias resolution: got %v, want %v", project, actual, want)
				}
				t.Logf("project=%s selected=%v diagnostics=%v", project, actual, listing.Diagnostics)
				return actual
			}
			selected(build, test.failed)
			buildBefore, err := os.ReadFile(build)
			if err != nil {
				t.Fatal(err)
			}
			command := editorTestCommand(compiler, build)
			a := actionConfig{project: "tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor}
			if test.tree {
				a.generatedDirectories = []string{"generated"}
			} else {
				a.generatedFiles = []string{generated}
			}
			err = editorRun(".", command, a, editor)
			if test.rejected {
				if err == nil {
					selected(editor, test.failed)
					t.Fatal("generated editor accepted a resolution mode whose failed aliases cannot be represented by exact paths")
				}
				for _, required := range []string{editor, "choice", "moduleResolution", test.resolution, "failed", "authored editor project", "without generated_sources", "ordinary builds and type checking remain supported"} {
					if !strings.Contains(err.Error(), required) {
						t.Fatalf("unsupported projection diagnostic omits %q: %v", required, err)
					}
				}
				if _, err := os.Stat(editor); !os.IsNotExist(err) {
					t.Fatalf("rejected projection wrote an editor project: %v", err)
				}
				if after, err := os.ReadFile(build); err != nil || string(after) != string(buildBefore) {
					t.Fatalf("rejected projection changed the already-checked compiler project: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			selected(editor, test.failed)
			if test.target == "./generated/value.ts" {
				writeFile(t, "preferred/value.ts", "export const value: string = 'preferred';\n")
				selectedFile = "preferred/value.ts"
				selected(build, false)
				selected(editor, false)
				editorTestRemove(t, selectedFile)
				selectedFile = binDir + "/" + generated
				writeFile(t, generated, "export const value: boolean = true;\n")
				selected(editor, false)
				editorTestRemove(t, binDir+"/"+generated)
				editorTestListing(t, compiler, editor, "TS6053")
				root := binDir + "/editor-program"
				sources := append([]string{"baseline.json", "tsconfig.json"}, test.consumers...)
				if err := a.layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := editorRun(root, command, a, editor); err != nil {
					t.Fatal(err)
				}
				listing := editorTestListing(t, compiler, editor, "TS2307", "TS2307")
				for _, edge := range listing.Edges {
					if edge.Kind.ModuleSpecifier() && edge.Specifier == "choice" {
						t.Fatalf("deleted output resolved through checkout twin: %+v", edge)
					}
				}
				writeFile(t, "fallback/value.ts", "export const value: string = 'fallback';\n")
				selectedFile = "fallback/value.ts"
				selected(editor, false)
			}
		})
	}
}

func TestEditorParsedSubstitutionsCannotPromotePackageFallback(t *testing.T) {
	const trace = `======== Resolving module '#choice' from '/work/implicit.ts'. ========
Trying substitution '../generated/model', candidate module location: '../generated/model'.
Trying substitution './generated/model', candidate module location: './generated/model'.
Trying substitution './types/v2/index.d.ts', candidate module location: './types/v2/index.d.ts'.
======== Module name '#choice' was successfully resolved to '/work/build/generated/model/types/v2/index.d.ts'. ========
======== Resolving module '#choice' from '/work/authored.ts'. ========
Trying substitution './generated/model', candidate module location: './generated/model'.
Trying substitution '../fallback/model', candidate module location: '../fallback/model'.
======== Module name '#choice' was successfully resolved to '/work/fallback/model.d.ts'. ========
======== Resolving module '#choice' from '/work/package.ts'. ========
Trying substitution './generated/model', candidate module location: './generated/model'.
Using 'imports' subpath '#choice' with target 'dependency'.
======== Resolving module 'dependency' from '/work/'. ========
======== Module name '#choice' was successfully resolved to '/work/build/generated/model/types/v2/index.d.ts'. ========
`
	listing, err := explainfiles.Parse(trace)
	if err != nil {
		t.Fatal(err)
	}
	a := actionConfig{out: "build/program.tsconfig.json", binDir: "build", generatedFiles: []string{"generated/model/types/v2/index.d.ts"}}
	projection := editorProjection{actionConfig: &a}
	chain := &tsconfig.Resolved{Paths: &tsconfig.Paths{{Key: "#choice", Values: []string{"./generated/model", "./fallback/model"}}}}
	listedPath := func(file string) string { return strings.TrimPrefix(file, "/work/") }
	implicit := projection.editorImplicitSelections(listing.Resolutions, chain, listedPath)
	want := explainfiles.Edge{Kind: explainfiles.Import, From: "implicit.ts", Specifier: "#choice", To: "build/generated/model/types/v2/index.d.ts"}
	if !reflect.DeepEqual(implicit, map[explainfiles.Edge]bool{want: true}) {
		t.Fatalf("parsed substitutions confused generated selection, authored fallback or package fallback: %v", implicit)
	}
	for _, from := range []string{"implicit.ts", "package.ts"} {
		edge := want
		edge.From = from
		err := projection.projectEditorEdges(&tsconfig.Paths{}, chain, ".bazel/tsconfig", []explainfiles.Edge{edge}, implicit, "nodenext", nil)
		if from == "implicit.ts" {
			if err == nil || !strings.Contains(err.Error(), "resolution mode") {
				t.Fatalf("parsed generated selection bypassed the exact-projection guard: %v", err)
			}
		} else if err != nil {
			t.Fatalf("package fallback was promoted to exact alias projection: %v", err)
		}
	}
}

func TestEditorMixedAliasSelectionsCannotOverrideAnotherImporter(t *testing.T) {
	selected := binDir + "/generated/model.d.ts"
	for _, test := range []struct{ name, other string }{
		{"generated_and_npm", "node_modules/choice/index.d.ts"},
		{"different_generated_files", binDir + "/other/model.d.ts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain := &tsconfig.Resolved{Paths: &tsconfig.Paths{{Key: "choice", Values: []string{"./generated/model", "./missing"}}}}
			edges := []explainfiles.Edge{
				{Kind: explainfiles.Import, From: "consumer.mts", Specifier: "choice", To: selected},
				{Kind: explainfiles.Import, From: "consumer.cts", Specifier: "choice", To: test.other},
			}
			a := actionConfig{binDir: binDir, generatedFiles: []string{"generated/model.d.ts", "other/model.d.ts"}}
			projection := editorProjection{actionConfig: &a}
			err := projection.projectEditorEdges(&tsconfig.Paths{}, chain, ".bazel/tsconfig", edges, map[explainfiles.Edge]bool{edges[0]: true}, "bundler", nil)
			if err == nil {
				t.Fatalf("accepted conflicting alias selections: %v", edges)
			}
			for _, required := range []string{"choice", "consumer.mts", "consumer.cts", selected, test.other, "distinct aliases", "separate editor projects", "without generated_sources", "ordinary builds and type checking remain supported"} {
				if !strings.Contains(err.Error(), required) {
					t.Fatalf("conflicting resolution diagnostic omits %q: %v", required, err)
				}
			}
		})
	}
}

func TestEditorAuthoredTreeAliasesShareExactProjectionGuards(t *testing.T) {
	for _, test := range []struct {
		name, resolution, other, required string
		suffixes                          []any
	}{
		{name: "suffix", resolution: "bundler", suffixes: []any{".native", ""}, required: "moduleSuffixes"},
		{name: "node16", resolution: "node16", required: "resolution mode"},
		{name: "nodenext", resolution: "nodenext", required: "resolution mode"},
		{name: "conflicting_importer", resolution: "bundler", other: binDir + "/generated/local.d.ts", required: "different files"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const selected = "generated/local.d.ts"
			a := actionConfig{binDir: binDir, generatedDirectories: []string{"generated"}, srcs: []string{selected}}
			projection := editorProjection{actionConfig: &a}
			chain := &tsconfig.Resolved{Paths: &tsconfig.Paths{{Key: "#local", Values: []string{"./generated/local.js"}}}}
			edges := []explainfiles.Edge{{Kind: explainfiles.Import, From: "consumer.ts", To: selected, Specifier: "#local"}}
			if test.other != "" {
				edges = append(edges, explainfiles.Edge{Kind: explainfiles.Import, From: "other.ts", To: test.other, Specifier: "#local"})
			}
			err := projection.projectEditorEdges(&tsconfig.Paths{}, chain, ".bazel/tsconfig", edges, nil, test.resolution, test.suffixes)
			if err == nil {
				t.Fatal("authored selection bypassed the exact-file projection guard")
			}
			for _, required := range []string{"#local", test.required, "without generated_sources", "ordinary builds and type checking remain supported"} {
				if !strings.Contains(err.Error(), required) {
					t.Fatalf("unsafe authored projection diagnostic omits %q: %v", required, err)
				}
			}
		})
	}
}

func TestEditorBroadSourceAliasCannotReadStaleGeneratedTwins(t *testing.T) {
	compiler := editorTestCompiler(t)
	t.Chdir(t.TempDir())
	writeFile(t, "baseline.json", `{}`)
	writeFile(t, "tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler","paths":{"#src/*":["./src/*"]}},"files":["consumer.ts"]}`)
	writeFile(t, "consumer.ts", `import { value } from '#src/generated/value';
import { value as authored } from '#src/plain';
export const values: string[] = [value, authored];
`)
	writeFile(t, "src/plain.ts", "export const value: string = 'authored';\n")
	generated := "src/generated/value.d.ts"
	writeFile(t, binDir+"/"+generated, "export declare const value: string;\n")
	stale := "export declare const value: boolean;\n"
	writeFile(t, generated, stale)
	build := binDir + "/program.tsconfig.json"
	editor := ".bazel/tsconfig/project.json"
	editorTestMkdir(t, filepath.Dir(editor))
	mustWriteTsconfig(t, editorTestConfigArgs(compiler, "tsconfig.json", build, "consumer.ts"))
	want := map[string]string{"#src/generated/value": binDir + "/" + generated, "#src/plain": "src/plain.ts"}
	selected := func(t *testing.T, project string, codes ...string) {
		t.Helper()
		listing := editorTestListing(t, compiler, project, codes...)
		actual := map[string]string{}
		for _, edge := range listing.Edges {
			if edge.Kind.ModuleSpecifier() && edge.From == "consumer.ts" {
				actual[edge.Specifier] = edge.To
			}
		}
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("%s selected %v, want %v", project, actual, want)
		}
	}
	sources := []string{"baseline.json", "tsconfig.json", "consumer.ts", "src/plain.ts"}
	root := binDir + "/reference-program"
	if err := layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	selected(t, build)
	t.Chdir(cwd)
	runArgs := []string{"-root=" + binDir + "/editor-program", "-tsconfig=tsconfig.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-generated-directory=src/generated", "-editor-out=" + editor, "-editor-path=" + editor}
	for _, file := range sources {
		runArgs = append(runArgs, "-source="+file)
	}
	runArgs = append(runArgs, "--")
	runArgs = append(runArgs, editorTestCommand(compiler, build)...)
	if err := runTsgo(runArgs); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(editor)
	if err != nil {
		t.Fatal(err)
	}
	selected(t, editor)
	consumer, err := os.ReadFile("consumer.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ specifier, selected, normalized string }{
		{"#src/./generated/value", binDir + "/" + generated, "#src/generated/value"},
		{"#src/plain/../generated/value", binDir + "/" + generated, "#src/generated/value"},
		{"#src/generated/../plain", "src/plain.ts", "#src/plain"},
	} {
		specifier := test.specifier
		t.Run("uncovered_"+specifier+"_must_not_replace_project", func(t *testing.T) {
			writeFile(t, "consumer.ts", strings.Replace(string(consumer), "#src/generated/value", specifier, 1))
			delete(want, "#src/generated/value")
			want[specifier] = test.selected
			t.Chdir(root)
			selected(t, build)
			t.Chdir(cwd)
			err := runTsgo(runArgs)
			if err == nil {
				t.Fatal("accepted an alias spelling whose projection crosses the generated directory boundary")
			}
			for _, required := range []string{editor, specifier, "consumer.ts", test.selected, "generated directory", "normalized", test.normalized, "authored editor project", "ordinary builds and type checking remain supported"} {
				if !strings.Contains(err.Error(), required) {
					t.Fatalf("uncovered alias diagnostic omits %q: %v", required, err)
				}
			}
			if got, err := os.ReadFile(editor); err != nil || string(got) != string(installed) {
				t.Fatalf("rejected alias replaced the installed project: %q, %v", got, err)
			}
		})
		delete(want, specifier)
	}
	writeFile(t, "consumer.ts", string(consumer))
	want["#src/generated/value"] = binDir + "/" + generated
	selected(t, editor)
	writeFile(t, binDir+"/"+generated, "export declare const value: number;\n")
	selected(t, editor, "TS2322")
	editorTestRemove(t, binDir+"/"+generated)
	delete(want, "#src/generated/value")
	selected(t, editor, "TS2307")
	for _, specifier := range []string{"#src/./generated/value", "#src/plain/../generated/value"} {
		for _, suppressed := range []bool{false, true} {
			name := "missing_" + specifier
			if suppressed {
				name += "_suppressed"
			}
			t.Run(name, func(t *testing.T) {
				source := strings.Replace(string(consumer), "#src/generated/value", specifier, 1)
				codes := []string{"TS2307"}
				if suppressed {
					source = "// @ts-ignore\n" + source
					codes = nil
				}
				writeFile(t, "consumer.ts", source)
				t.Chdir(root)
				selected(t, build, codes...)
				t.Chdir(cwd)
				err := runTsgo(runArgs)
				if err == nil {
					t.Fatal("accepted an unresolved alias spelling that can read a stale checkout twin")
				}
				for _, required := range []string{editor, specifier, "consumer.ts", "unresolved", "generated directory", "normalized", "#src/generated/value", "authored editor project", "ordinary builds and type checking remain supported"} {
					if !strings.Contains(err.Error(), required) {
						t.Fatalf("unresolved alias diagnostic omits %q: %v", required, err)
					}
				}
				if got, err := os.ReadFile(editor); err != nil || string(got) != string(installed) {
					t.Fatalf("rejected unresolved alias replaced the installed project: %q, %v", got, err)
				}
			})
		}
	}
	writeFile(t, "consumer.ts", string(consumer))
	if got, err := os.ReadFile(editor); err != nil || string(got) != string(installed) {
		t.Fatalf("generated transitions changed the installed project: %v", err)
	}
	if got, err := os.ReadFile(generated); err != nil || string(got) != stale {
		t.Fatalf("generated transitions changed the checkout twin: %q, %v", got, err)
	}
}

func TestEditorRefreshedAliasesPreserveAuthoredSiblingsAndRejectDeletedTwins(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, suffix, treeSuffix, overlay                      string
		scalar, stale, generatedFirst, authoredSibling         bool
		explicitSibling                                        bool
		authoredAlias, authoredImport, authoredFile, rejection string
	}{
		{name: "tree_declaration_suffix", suffix: "/index.d.ts", stale: true},
		{name: "tree_directory_index_suffix", suffix: "/client", treeSuffix: "/client", stale: true},
		{name: "tree_directory", stale: true},
		{name: "tree_explicit_authored_sibling_candidate", stale: true, authoredSibling: true, explicitSibling: true},
		{name: "tree_directory_index_suffix_with_authored_sibling", suffix: "/client", treeSuffix: "/client", stale: true, authoredSibling: true, rejection: "directory candidate"},
		{name: "tree_directory_with_authored_sibling", stale: true, authoredSibling: true, rejection: "directory candidate"},
		{name: "tree_generated_first_declaration_suffix", suffix: "/index.d.ts", stale: true, generatedFirst: true},
		{name: "tree_generated_first_directory_index_suffix", suffix: "/client", treeSuffix: "/client", stale: true, generatedFirst: true},
		{name: "tree_generated_first_directory", stale: true, generatedFirst: true},
		{name: "tree_generated_first_directory_index_suffix_with_authored_sibling", suffix: "/client", treeSuffix: "/client", stale: true, generatedFirst: true, authoredSibling: true, rejection: "directory candidate"},
		{name: "tree_generated_first_with_authored_sibling", stale: true, generatedFirst: true, authoredSibling: true, rejection: "directory candidate"},
		{name: "scalar_directory_index_without_source_twin", scalar: true, authoredSibling: true},
		{name: "scalar_directory_index_with_stale_source_twin", scalar: true, stale: true, authoredSibling: true},
		{name: "scalar_same_package_overlay_with_stale_source_twin", scalar: true, stale: true, authoredSibling: true, overlay: binDir + "/app"},
		{name: "scalar_ancestor_overlay_with_stale_source_twin", scalar: true, stale: true, authoredSibling: true, overlay: binDir},
		{name: "overlapping_suffix_alias", suffix: "/index.d.ts", stale: true, authoredAlias: "#types/f*/special", authoredImport: "#types/foo/value/special", authoredFile: "oo/value.ts", rejection: "suffix alias"},
		{name: "overlapping_suffix_alias_at_boundary", suffix: "/index.d.ts", stale: true, authoredAlias: "#types/foo*/special", authoredImport: "#types/foo/value/special", authoredFile: "value.ts", rejection: "suffix alias"},
		{name: "equal_prefix_suffix_alias", suffix: "/index.d.ts", stale: true, authoredAlias: "#types/foo/*/special", authoredImport: "#types/foo/value/special", authoredFile: "value.ts"},
		{name: "unrelated_suffix_alias", suffix: "/index.d.ts", stale: true, authoredAlias: "#other/*/special", authoredImport: "#other/value/special", authoredFile: "value.ts"},
		{name: "disjoint_suffix_alias", suffix: "/index.d.ts", stale: true, authoredAlias: "#types/bar*/special", authoredImport: "#types/barvalue/special", authoredFile: "value.ts"},
		{name: "more_specific_suffix_alias", suffix: "/index.d.ts", stale: true, authoredAlias: "#types/foo/v*/special", authoredImport: "#types/foo/value/special", authoredFile: "alue.ts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeFile(t, "baseline.json", `{}`)
			paths := map[string][]string{"#types/*": {"./first/*" + test.suffix, "./authored/*" + test.suffix, "./generated/*" + test.suffix}, "#types/exact": {"./overrides/exact.ts"}, "#types/specific/*": {"./overrides/*.ts"}}
			if test.explicitSibling {
				paths["#types/*"] = append([]string{"./generated/*.ts"}, paths["#types/*"]...)
			}
			if test.authoredAlias != "" {
				paths[test.authoredAlias] = []string{"./overrides/*"}
			}
			config := map[string]any{
				"compilerOptions": map[string]any{"strict": true, "module": "preserve", "moduleResolution": "bundler", "paths": paths},
				"files":           []string{"consumer.ts"},
			}
			consumer := `import { value as observed } from '#types/foo';
import { value as preferred } from '#types/preferred';
import { value as exact } from '#types/exact';
import { value as specific } from '#types/specific/foo';
import type { Model } from '#types/augmentation';
export const values: string[] = [observed, preferred, exact, specific];
declare module '#types/augmentation' { interface Model { extra: string } }
export const model: Model = { value: 'current', extra: 'augmented' };
`
			writeFile(t, "app/consumer.ts", consumer)
			build := binDir + "/app/program.tsconfig.json"
			editor := "app/.bazel/tsconfig/project.json"
			generated := []string{}
			names := []string{"foo", "late", "preferred", "exact", "specific/foo", "augmentation"}
			if test.authoredSibling {
				names = append(names, "authored-observed", "authored-late")
				consumer += "import { value as authoredObserved } from '#types/authored-observed';\nexport const authoredObservedValue: string = authoredObserved;\n"
				writeFile(t, "app/consumer.ts", consumer)
			}
			for _, name := range names {
				tree := "app/generated/" + name + test.treeSuffix
				artifact := tree
				if test.scalar {
					artifact += "/index.d.ts"
				}
				generated = append(generated, artifact)
				typ := "string"
				if strings.HasPrefix(name, "authored-") {
					typ = "number"
				}
				writeFile(t, binDir+"/"+tree+"/index.d.ts", "export declare const value: "+typ+";\n")
				if test.stale {
					writeFile(t, tree+"/index.d.ts", "export declare const value: number;\n")
				}
				if name == "augmentation" {
					writeFile(t, binDir+"/"+tree+"/index.d.ts", "export interface Model { value: string; }\n")
					if test.stale {
						writeFile(t, tree+"/index.d.ts", "export interface Model { extra: number; }\n")
					}
				}
			}
			preferred := "app/authored/preferred" + test.treeSuffix + "/index.d.ts"
			want := map[string]string{
				"#types/foo":          binDir + "/app/generated/foo" + test.treeSuffix + "/index.d.ts",
				"#types/augmentation": binDir + "/app/generated/augmentation" + test.treeSuffix + "/index.d.ts",
				"#types/preferred":    preferred,
				"#types/exact":        "app/overrides/exact.ts",
				"#types/specific/foo": "app/overrides/foo.ts",
			}
			sources := []string{"baseline.json", "app/tsconfig.build.json", "app/consumer.ts", "app/overrides/exact.ts", "app/overrides/foo.ts"}
			if test.generatedFirst {
				want["#types/preferred"] = binDir + "/app/generated/preferred" + test.treeSuffix + "/index.d.ts"
			} else {
				sources = append(sources, preferred)
			}
			srcs := []string{"app/consumer.ts"}
			for _, file := range sources[3:] {
				writeFile(t, file, "export declare const value: string;\n")
			}
			if test.authoredAlias != "" {
				file := "app/overrides/" + test.authoredFile
				sources = append(sources, file)
				writeFile(t, file, "export declare const value: string;\n")
				writeFile(t, binDir+"/app/generated/foo/value/special/index.d.ts", "export declare const value: number;\n")
				consumer += "import { value as suffix } from '" + test.authoredImport + "';\nexport const suffixValue: string = suffix;\n"
				writeFile(t, "app/consumer.ts", consumer)
				want[test.authoredImport] = file
			}
			if test.authoredSibling {
				for _, name := range []string{"authored-observed", "authored-late"} {
					file := "app/generated/" + name + test.treeSuffix + ".ts"
					sources = append(sources, file)
					srcs = append(srcs, file)
					want["#types/"+name] = file
					writeFile(t, file, "export declare const value: string;\n")
				}
			}
			if test.scalar {
				for name, selected := range map[string]string{"ambiguous": "ambiguous/index.ts", "file-first": "file-first.ts", "package-first": "package-first/entry.d.ts", "emitted.js": "emitted.ts"} {
					index := "app/generated/" + name + "/index.d.ts"
					selected = "app/generated/" + selected
					generated = append(generated, index, selected)
					writeFile(t, binDir+"/"+index, "export declare const value: number;\n")
					writeFile(t, binDir+"/"+selected, "export declare const value: string;\n")
					if test.stale {
						writeFile(t, index, "export declare const value: number;\n")
					}
					want["#types/"+name] = binDir + "/" + selected
				}
				writeFile(t, binDir+"/app/generated/emitted.d.ts", "export declare const value: number;\n")
				manifest := "app/generated/package-first/package.json"
				generated = append(generated, manifest)
				writeFile(t, binDir+"/"+manifest, `{"types":"./entry.d.ts"}`)
			}
			include := []string{}
			for _, file := range generated {
				if strings.HasSuffix(file, ".ts") && !strings.HasSuffix(file, ".d.ts") {
					include = append(include, fileRelative("app", binDir+"/"+file))
				}
			}
			config["include"] = include
			editorTestWriteJSON(t, "app/tsconfig.build.json", config)
			if err := os.Symlink(binDir, "bazel-bin"); err != nil {
				t.Fatal(err)
			}
			editorTestMkdir(t, filepath.Dir(editor))
			args := editorTestConfigArgs(compiler, "app/tsconfig.build.json", build)
			args = append(args, srcs...)
			mustWriteTsconfig(t, args)
			runArgs := []string{"-root=" + binDir + "/app/editor-program", "-tsconfig=app/tsconfig.build.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor}
			if test.overlay != "" {
				runArgs = append(runArgs, overlayFlag(test.overlay))
			}
			for _, artifact := range generated {
				flag := "-editor-generated-directory="
				if test.scalar {
					flag = "-editor-generated-file="
				}
				runArgs = append(runArgs, flag+artifact)
			}
			for _, file := range sources {
				runArgs = append(runArgs, "-source="+file)
			}
			runArgs = append(runArgs, "--")
			runArgs = append(runArgs, editorTestCommand(compiler, build)...)
			if test.rejection != "" {
				// The build's program root excludes checkout twins, but retains
				// authored siblings and the authored suffix winner.
				root := binDir + "/app/reference-program"
				if err := layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				t.Chdir(root)
				listing := editorTestListing(t, compiler, build)
				t.Chdir(cwd)
				actual := map[string]string{}
				for _, edge := range listing.Edges {
					if edge.Kind.ModuleSpecifier() && edge.From == "app/consumer.ts" {
						actual[edge.Specifier] = edge.To
					}
				}
				delete(want, "#types/authored-late")
				if !reflect.DeepEqual(actual, want) {
					t.Fatalf("build lost authored precedence: got %v, want %v", actual, want)
				}
				installed := "{\"files\":[],\"include\":[]}\n"
				writeFile(t, editor, installed)
				err = runTsgo(runArgs)
				if err == nil {
					editorTestListing(t, compiler, editor)
					t.Fatal("refresh accepted an alias projection that loses authored precedence")
				}
				for _, required := range []string{editor, test.rejection, "authored editor project", "without generated_sources", "ordinary builds and type checking remain supported"} {
					if !strings.Contains(err.Error(), required) {
						t.Fatalf("projection rejection omits %q: %v", required, err)
					}
				}
				if test.authoredSibling {
					for _, required := range []string{"#types/authored-observed", "app/consumer.ts", "authored sibling " + want["#types/authored-observed"]} {
						if !strings.Contains(err.Error(), required) {
							t.Fatalf("boundary rejection omits observed resolution %q: %v", required, err)
						}
					}
				}
				if got, err := os.ReadFile(editor); err != nil || string(got) != installed {
					t.Fatalf("rejected projection replaced the installed project: %q, %v", got, err)
				}
				return
			}
			if err := runTsgo(runArgs); err != nil {
				t.Fatal(err)
			}
			writeFile(t, "app/tsconfig.json", `{"files":[],"include":[],"references":[{"path":"./.bazel/tsconfig/project.json"}]}`)
			consumer += "import { value as late } from '#types/late';\nexport const added: string = late;\n"
			if test.authoredSibling {
				consumer += "import { value as authoredLate } from '#types/authored-late';\nexport const authoredLateValue: string = authoredLate;\n"
			}
			if test.scalar {
				consumer += `import { value as ambiguous } from '#types/ambiguous';
import { value as fileFirst } from '#types/file-first';
import { value as packageFirst } from '#types/package-first';
import { value as emitted } from '#types/emitted.js';
export const selected: string[] = [ambiguous, fileFirst, packageFirst, emitted];
`
			}
			writeFile(t, "app/consumer.ts", consumer)
			if err := runTsgo(runArgs); err != nil {
				t.Fatal(err)
			}
			want["#types/late"] = binDir + "/app/generated/late" + test.treeSuffix + "/index.d.ts"
			if !test.scalar {
				installed, err := os.ReadFile(editor)
				if err != nil {
					t.Fatal(err)
				}
				checkPreferred := func(want string) {
					t.Helper()
					listing := editorTestListing(t, compiler, editor)
					for _, edge := range listing.Edges {
						if edge.From == "app/consumer.ts" && edge.Specifier == "#types/preferred" && edge.To == want {
							return
						}
					}
					t.Fatalf("authored fallback did not select %s: %v", want, listing.Edges)
				}
				checkPreferred(want["#types/preferred"])
				if test.generatedFirst {
					writeFile(t, preferred, "export declare const value: string;\n")
					checkPreferred(preferred)
				}
				editorTestRemove(t, preferred)
				canonical := binDir + "/app/generated/preferred" + test.treeSuffix + "/index.d.ts"
				checkPreferred(canonical)
				writeFile(t, preferred, "export declare const value: string;\n")
				checkPreferred(preferred)
				first := "app/first/preferred" + test.treeSuffix + "/index.d.ts"
				writeFile(t, first, "export declare const value: string;\n")
				want["#types/preferred"] = first
				if got, err := os.ReadFile(editor); err != nil || string(got) != string(installed) {
					t.Fatalf("alias transitions changed the installed project: %v", err)
				}
			}
			installed, err := os.ReadFile(editor)
			if err != nil {
				t.Fatal(err)
			}
			states := []string{"initial", "deleted"}
			if test.scalar {
				states = []string{"initial", "replaced", "deleted"}
			}
			for _, state := range states {
				codes := []string{}
				if state == "replaced" {
					writeFile(t, want["#types/foo"], "export declare const value: boolean;\n")
					codes = []string{"TS2322"}
				}
				if state == "deleted" {
					for _, specifier := range []string{"#types/foo", "#types/late"} {
						editorTestRemove(t, want[specifier])
						delete(want, specifier)
					}
					codes = []string{"TS2307", "TS2307"}
				}
				listing := editorTestListing(t, compiler, editor, codes...)
				actual := map[string]string{}
				for _, edge := range listing.Edges {
					if edge.Kind.ModuleSpecifier() && edge.From == "app/consumer.ts" {
						actual[edge.Specifier] = edge.To
					}
				}
				if !reflect.DeepEqual(actual, want) {
					t.Fatalf("generated resolution with state=%s selected %v, want %v", state, actual, want)
				}
				t.Logf("state=%s compiler selected=%v", state, actual)
			}
			if got, err := os.ReadFile(editor); err != nil || string(got) != string(installed) {
				t.Fatalf("generated transitions changed the installed project: %v", err)
			}
			if test.stale {
				if got, err := os.ReadFile("app/generated/foo" + test.treeSuffix + "/index.d.ts"); err != nil || string(got) != "export declare const value: number;\n" {
					t.Fatalf("generated transitions changed the checkout twin: %q, %v", got, err)
				}
			}
		})
	}
}

func TestEditorGeneratedRelativeEdgesCannotRecoverDeletedCheckoutTwins(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, index string
		scalar      bool
	}{
		{"tree_reexport", "export { value } from './child';\n", false},
		{"scalar_reexport", "export { value } from './child';\n", true},
		{"tree_import", "import { value as child } from './child';\nexport declare const value: typeof child;\n", false},
		{"scalar_import", "import { value as child } from './child';\nexport declare const value: typeof child;\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "tsconfig.json", `{"compilerOptions":{"strict":true,"skipLibCheck":false,"module":"preserve","moduleResolution":"bundler","paths":{"#generated":["./generated/index.d.ts"]}},"files":["consumer.ts"]}`)
			writeFile(t, "consumer.ts", "import { value } from '#generated';\nexport const result: 'current' = value;\n")
			index, child := "generated/index.d.ts", "generated/child.d.ts"
			for _, prefix := range []string{"", binDir + "/"} {
				writeFile(t, prefix+index, test.index)
				writeFile(t, prefix+child, "export declare const value: 'current';\n")
			}
			build := binDir + "/program.tsconfig.json"
			editor := ".bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(editor))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "tsconfig.json", build, "consumer.ts"))
			args := []string{"-root=" + binDir + "/editor-program", "-tsconfig=tsconfig.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor, "-source=baseline.json", "-source=tsconfig.json", "-source=consumer.ts"}
			if test.scalar {
				args = append(args, "-editor-generated-file="+index, "-editor-generated-file="+child)
			} else {
				args = append(args, "-editor-generated-directory=generated")
			}
			args = append(args, "--")
			args = append(args, editorTestCommand(compiler, build)...)
			if err := runTsgo(args); err != nil {
				t.Fatal(err)
			}
			for _, state := range []string{"initial", "replaced", "deleted", "recovered"} {
				codes := []string{}
				switch state {
				case "replaced":
					writeFile(t, binDir+"/"+child, "export declare const value: 'replacement';\n")
					codes = []string{"TS2322"}
				case "deleted":
					editorTestRemove(t, binDir+"/"+child)
					codes = []string{"TS2307"}
				case "recovered":
					writeFile(t, binDir+"/"+child, "export declare const value: 'current';\n")
				}
				listing := editorTestListing(t, compiler, editor, codes...)
				want := map[string]string{"consumer.ts:#generated": binDir + "/" + index}
				if state != "deleted" {
					want[binDir+"/"+index+":./child"] = binDir + "/" + child
				}
				actual := map[string]string{}
				for _, edge := range listing.Edges {
					if edge.Kind.ModuleSpecifier() {
						actual[edge.From+":"+edge.Specifier] = edge.To
					}
				}
				if !reflect.DeepEqual(actual, want) || slices.Contains(listing.Files, child) || slices.Contains(listing.Files, index) {
					t.Fatalf("%s regained checkout twins: edges=%v, want %v; files=%v", state, actual, want, listing.Files)
				}
			}
			for file, want := range map[string]string{index: test.index, child: "export declare const value: 'current';\n"} {
				got, err := os.ReadFile(file)
				if err != nil || string(got) != want {
					t.Fatalf("checkout input %s changed: %q, %v", file, got, err)
				}
			}
		})
	}
}

func TestEditorAuthoredDependencyStaysNativeBesideGeneratedTree(t *testing.T) {
	compiler := editorTestCompiler(t)
	t.Chdir(t.TempDir())
	writeFile(t, "baseline.json", `{}`)
	writeFile(t, "lib/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve"},"files":["value.ts"]}`)
	writeFile(t, "lib/value.ts", "export const value: string = 'authored';\n")
	writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve","paths":{"#generated":["../gen/tree/index.d.ts"]}},"files":["a.ts"]}`)
	writeFile(t, "app/a.ts", "import { value } from '../lib/value';\nimport { generated } from '#generated';\nexport const result: string = value;\nexport const current: 'current' = generated;\n")
	writeFile(t, binDir+"/gen/tree/index.d.ts", "export declare const generated: 'current';\n")
	if output, err := exec.CommandContext(t.Context(), compiler, "--skipDefaultLibCheck", "-p", "lib/tsconfig.json", "--declaration", "--emitDeclarationOnly", "--outDir", binDir+"/lib").CombinedOutput(); err != nil {
		t.Fatalf("emit authored dependency: %v\n%s", err, output)
	}
	build := binDir + "/app/program.tsconfig.json"
	editor := "app/.bazel/tsconfig/project.json"
	editorTestMkdir(t, filepath.Dir(build), filepath.Dir(editor))
	mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "-type_input="+binDir+"/lib/value.d.ts", "app/a.ts"))
	args := []string{"-root=" + binDir + "/editor-program", "-source=baseline.json", "-source=app/tsconfig.json", "-source=app/a.ts"}
	command := []string{"--", compiler, "--skipDefaultLibCheck", "-p", build, "--noEmit", "--pretty", "false"}
	if err := runTsgo(append(slices.Clone(args), command...)); err != nil {
		t.Fatalf("declaration-backed build failed: %v", err)
	}
	args = append(args, "-tsconfig=app/tsconfig.json", "-editor-config="+build, "-editor-baseline=baseline.json", "-editor-bin-dir="+binDir, "-editor-out="+editor, "-editor-path="+editor, "-source=lib/value.ts", "-editor-generated-file=lib/value.d.ts", "-editor-generated-directory=gen/tree")
	command = append(command, "--explainFiles", "--traceResolution")
	if err := runTsgo(append(args, command...)); err != nil {
		t.Fatalf("authored dependency blocked generated refresh: %v", err)
	}
	listing := editorTestListing(t, compiler, editor)
	for _, want := range []explainfiles.Edge{
		{From: "app/a.ts", Specifier: "../lib/value", To: "lib/value.ts"},
		{From: "app/a.ts", Specifier: "#generated", To: binDir + "/gen/tree/index.d.ts"},
	} {
		if !slices.Contains(listing.Edges, want) {
			t.Fatalf("editor lost native/generated resolution %v: %v", want, listing.Edges)
		}
	}
	writeFile(t, "lib/value.ts", "export const value: number = 1;\n")
	editorTestListing(t, compiler, editor, "TS2322")
}

func TestEditorAuthoredAncestorManifestStaysNativeBesideGeneratedTree(t *testing.T) {
	compiler := editorTestCompiler(t)
	t.Chdir(t.TempDir())
	writeFile(t, "baseline.json", `{}`)
	writeFile(t, "app/value.ts", "export const value: string = 'authored';\n")
	writeFile(t, "app/package.json", `{"name":"fixture","type":"module","imports":{"#value":{"types":"./value.ts","default":"./value.ts"}},"exports":{"./value":{"types":"./value.ts","default":"./value.ts"}}}`)
	writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve"},"files":["value.ts"]}`)
	writeFile(t, "app/nested/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve","paths":{"#generated":["../../gen/tree/index.d.ts"]}},"files":["consumer.ts"]}`)
	writeFile(t, "app/nested/consumer.ts", "import { value as imported } from '#value';\nimport { value as exported } from 'fixture/value';\nimport { generated } from '#generated';\nexport const values: string[] = [imported, exported, generated];\n")
	writeFile(t, binDir+"/gen/tree/index.d.ts", "export declare const generated: 'current';\n")
	if output, err := exec.CommandContext(t.Context(), compiler, "--skipDefaultLibCheck", "-p", "app/tsconfig.json", "--declaration", "--emitDeclarationOnly", "--outDir", binDir+"/app").CombinedOutput(); err != nil {
		t.Fatalf("emit ancestor declarations: %v\n%s", err, output)
	}
	manifest := binDir + "/app/app.package.json"
	if err := runManifest([]string{"app/package.json", manifest}); err != nil {
		t.Fatal(err)
	}
	build := binDir + "/app/nested/program.tsconfig.json"
	editor := "app/nested/.bazel/tsconfig/project.json"
	writeFile(t, build, `{}`)
	writeFile(t, editor, "previous editor project\n")
	mustWriteTsconfig(t, []string{"-tsgo=" + compiler, "-tsconfig=app/nested/tsconfig.json", "-baseline=baseline.json", "-out=" + build, "-options=" + binDir + "/options.json", "-bin_dir=" + binDir, "-type_input=" + binDir + "/app/value.d.ts", "app/nested/consumer.ts"})
	args := []string{"-root=" + binDir + "/editor-program", "-source=baseline.json", "-source=app/nested/tsconfig.json", "-source=app/nested/consumer.ts"}
	command := []string{"--", compiler, "--skipDefaultLibCheck", "-p", build, "--noEmit", "--pretty", "false"}
	buildArgs := append(slices.Clone(args), overlayFlag(binDir+"/app"), "-manifest="+manifest)
	if err := runTsgo(append(buildArgs, command...)); err != nil {
		t.Fatalf("emitted ancestor manifest broke ordinary type checking: %v", err)
	}
	args = append(args, "-tsconfig=app/nested/tsconfig.json", "-editor-config="+build, "-editor-baseline=baseline.json", "-editor-bin-dir="+binDir, "-editor-out="+editor, "-editor-path="+editor, "-source=app/value.ts", "-source=app/package.json", "-editor-generated-file=app/value.d.ts", "-editor-generated-file=app/app.package.json", "-editor-generated-directory=gen/tree")
	command = append(command, "--explainFiles", "--traceResolution")
	if err := runTsgo(append(slices.Clone(args), command...)); err != nil {
		t.Fatalf("authored ancestor manifest blocked refresh: %v", err)
	}
	if got := readJSON(t, editor)["compilerOptions"].(map[string]any)["outDir"]; got != nil {
		t.Fatalf("editor introduced output context: outDir = %v", got)
	}
	listing := editorTestListing(t, compiler, editor)
	for _, want := range []explainfiles.Edge{
		{From: "app/nested/consumer.ts", Specifier: "#value", To: "app/value.ts"},
		{From: "app/nested/consumer.ts", Specifier: "fixture/value", To: "app/value.ts"},
		{From: "app/nested/consumer.ts", Specifier: "#generated", To: binDir + "/gen/tree/index.d.ts"},
	} {
		if !slices.Contains(listing.Edges, want) {
			t.Fatalf("editor lost authored package or canonical generated resolution %v: %v", want, listing.Edges)
		}
	}
	writeFile(t, "app/value.ts", "export const value: number = 1;\n")
	if err := runTsgo(append(args, command...)); err != nil {
		t.Fatalf("authored source edit blocked refresh: %v", err)
	}
	editorTestListing(t, compiler, editor, "TS2322", "TS2322")
}

func TestEditorDependencyJSONIdentityAcrossPackagePlacements(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, placement := range []struct{ name, consumerDir, ownerDir string }{
		{"sibling", "app", "lib"},
		{"same_package", "app", "app"},
		{"ancestor", "app/nested", "app"},
	} {
		for _, origin := range []string{"authored", "generated_scalar", "generated_tree"} {
			t.Run(placement.name+"/"+origin, func(t *testing.T) {
				t.Chdir(t.TempDir())
				consumer := placement.consumerDir + "/consumer.ts"
				project := placement.consumerDir + "/tsconfig.json"
				settings := placement.ownerDir + "/data/settings.json"
				specifier := fileRelative(placement.consumerDir, settings)
				writeFile(t, "baseline.json", `{}`)
				writeFile(t, consumer, "import settings from '"+specifier+"';\nimport { generated } from '#generated';\nexport const setting: string = settings.value;\nexport const current: 'current' = generated;\n")
				writeFile(t, settings, `{"value":"authored"}`)
				writeFile(t, binDir+"/"+settings, `{"value":"staged"}`)
				writeFile(t, binDir+"/gen/tree/index.d.ts", "export declare const generated: 'current';\n")
				editorTestWriteJSON(t, project, map[string]any{
					"compilerOptions": map[string]any{
						"strict": true, "module": "preserve", "resolveJsonModule": true,
						"paths": map[string][]string{"#generated": {fileRelative(placement.consumerDir, "gen/tree/index.d.ts")}},
					},
					"files": []string{"consumer.ts"},
				})
				build := binDir + "/" + placement.consumerDir + "/program.tsconfig.json"
				editor := placement.consumerDir + "/.bazel/tsconfig/project.json"
				const installed = "previous editor project\n"
				writeFile(t, editor, installed)
				editorTestMkdir(t, filepath.Dir(build))
				mustWriteTsconfig(t, []string{"-tsgo=" + compiler, "-tsconfig=" + project, "-baseline=baseline.json", "-out=" + build, "-options=" + binDir + "/options.json", "-bin_dir=" + binDir, consumer})
				args := []string{"-root=" + binDir + "/editor-program", "-tsconfig=" + project, "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor, "-source=baseline.json", "-source=" + project, "-source=" + consumer, "-editor-generated-directory=gen/tree"}
				switch origin {
				case "authored":
					args = append(args, "-source="+settings)
				case "generated_scalar":
					args = append(args, "-editor-generated-file="+settings)
				case "generated_tree":
					args = append(args, "-editor-generated-directory="+placement.ownerDir+"/data")
				}
				args = append(args, "--")
				args = append(args, editorTestCommand(compiler, build)...)
				err := runTsgo(args)
				if origin != "authored" {
					if err == nil || !strings.Contains(err.Error(), "stale checkout twin") {
						t.Fatalf("generated relative JSON accepted the checkout twin: %v", err)
					}
					if got, err := os.ReadFile(editor); err != nil || string(got) != installed {
						t.Fatalf("rejection replaced installed project: %q, %v", got, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("authored JSON passthrough blocked refresh: %v", err)
				}
				assertOriginal := func(codes ...string) {
					listing := editorTestListing(t, compiler, editor, codes...)
					want := explainfiles.Edge{From: consumer, Specifier: specifier, To: settings}
					if !slices.Contains(listing.Edges, want) || slices.Contains(listing.Files, binDir+"/"+settings) {
						t.Fatalf("editor lost authored JSON identity %v: %+v", want, listing)
					}
				}
				assertOriginal()
				writeFile(t, settings, `{"value":42}`)
				if err := runTsgo(args); err != nil {
					t.Fatalf("authored JSON edit blocked refresh: %v", err)
				}
				assertOriginal("TS2322")
				if got, err := os.ReadFile(settings); err != nil || string(got) != `{"value":42}` {
					t.Fatalf("refresh changed authored JSON: %q, %v", got, err)
				}
			})
		}
	}
}

func TestEditorGeneratedJSONAliasIgnoresCheckoutTwin(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, module string
		stale        bool
	}{
		{name: "absent", module: "preserve"},
		{name: "stale", module: "preserve", stale: true},
		{name: "esnext_cannot_fall_back_to_classic", module: "esnext"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"`+test.module+`","resolveJsonModule":true,"paths":{"#schema":["../gen/schema.json"]}},"files":["a.ts"]}`)
			writeFile(t, "app/a.ts", "import schema from '#schema';\nexport const current: string = schema.current;\n")
			writeFile(t, binDir+"/gen/schema.json", `{"current":"canonical"}`)
			if test.stale {
				writeFile(t, "gen/schema.json", `{"stale":true}`)
			}
			build := binDir + "/app/program.tsconfig.json"
			editor := "app/.bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(build), filepath.Dir(editor))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "app/a.ts"))
			args := []string{"-root=" + binDir + "/editor-program", "-tsconfig=app/tsconfig.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor, "-source=baseline.json", "-source=app/tsconfig.json", "-source=app/a.ts", "-editor-generated-file=gen/schema.json", "--"}
			args = append(args, editorTestCommand(compiler, build)...)
			if err := runTsgo(args); err != nil {
				t.Fatal(err)
			}
			if mode := readJSON(t, editor)["compilerOptions"].(map[string]any)["moduleResolution"]; mode != "bundler" {
				t.Fatalf("editor config leaves tsserver to infer a different resolution mode: moduleResolution = %v", mode)
			}
			listing := editorTestListing(t, compiler, editor)
			want := explainfiles.Edge{From: "app/a.ts", Specifier: "#schema", To: binDir + "/gen/schema.json"}
			if !slices.Contains(listing.Edges, want) || slices.Contains(listing.Files, "gen/schema.json") {
				t.Fatalf("JSON alias did not select the compiler input: %+v", listing)
			}
			editorTestRemove(t, binDir+"/gen/schema.json")
			editorTestListing(t, compiler, editor, "TS2307")
		})
	}
}

func TestEditorExtensionlessScalarReferenceCannotReadStaleTwin(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, kind, extension, options, diagnostic string
	}{
		{"path_dts", "path", ".d.ts", "", "TS6231"},
		{"path_dmts", "path", ".d.mts", "", "TS6231"},
		{"types_dts", "types", ".d.ts", "", "TS2688"},
		{"types_dmts", "types", ".d.mts", "", "TS2688"},
		{"types_unset_root_dts", "types", ".d.ts", `,"typeRoots":[]`, "TS2688"},
		{"types_unset_root_dmts", "types", ".d.mts", `,"typeRoots":[]`, "TS2688"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve"`+test.options+`},"files":["a.ts"]}`)
			writeFile(t, "app/a.ts", "/// <reference "+test.kind+"=\"../gen/globals\" />\nexport {};\n")
			generated := "gen/globals" + test.extension
			const declaration = "declare const stale: number;\n"
			writeFile(t, generated, declaration)
			writeFile(t, binDir+"/"+generated, declaration)
			build := binDir + "/app/program.tsconfig.json"
			editor := "app/.bazel/tsconfig/project.json"
			const installed = "previous editor project\n"
			writeFile(t, editor, installed)
			editorTestMkdir(t, filepath.Dir(build))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "-type_input="+binDir+"/"+generated, "app/a.ts"))
			args := []string{"-root=" + binDir + "/editor-program", "-tsconfig=app/tsconfig.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor, "-source=baseline.json", "-source=app/tsconfig.json", "-source=app/a.ts", "-editor-generated-file=" + generated, "--"}
			args = append(args, editorTestCommand(compiler, build)...)
			err := runTsgo(args)
			if test.extension == ".d.ts" {
				if err == nil || !strings.Contains(err.Error(), "stale checkout twin") || !strings.Contains(err.Error(), "../gen/globals") {
					t.Fatalf("extensionless scalar reference was not rejected: %v", err)
				}
				if got, err := os.ReadFile(editor); err != nil || string(got) != installed {
					t.Fatalf("rejection replaced installed project: %q, %v", got, err)
				}
				// Native resolution would turn this failed build reference into success.
				editorTestListing(t, compiler, "app/tsconfig.json")
			} else {
				if err != nil {
					t.Fatalf("an unprobed extension blocked refresh: %v", err)
				}
				editorTestListing(t, compiler, editor, test.diagnostic)
			}
			if got, err := os.ReadFile(generated); err != nil || string(got) != declaration {
				t.Fatalf("refresh changed the stale twin: %q, %v", got, err)
			}
		})
	}
}

func TestEditorScalarImportsCannotRecoverCheckoutTwins(t *testing.T) {
	compiler := editorTestCompiler(t)
	editorTestChdir(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, "baseline.json", `{}`)
	writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler","types":[],"paths":{"#value":["../gen/value"],"#safe":["../gen/value.ts","./absent.ts"],"#authored":["./authored.ts","../gen/value"],"#canonical":["../`+binDir+`/gen/value"],"#fallback":["../gen/value","./authored.ts"],"#js-fallback":["../gen/value.js","./authored.ts"],"#explicit-fallback":["../gen/value.ts","./authored.ts"],"#canonical-fallback":["../`+binDir+`/gen/value","./authored.ts"]}},"files":["consumer.ts"]}`)
	writeFile(t, "app/consumer.ts", "export {};\n")
	const stale = "export const value: string = 'stale';\n"
	writeFile(t, "gen/value.ts", stale)
	writeFile(t, "gen/neighbor.ts", "export {};\n")
	writeFile(t, "app/authored.ts", "export const value: string = 'authored';\n")
	writeFile(t, binDir+"/lib/gen/value.d.ts", "export declare const value: string;\n")
	build := binDir + "/app/program.tsconfig.json"
	editor := "app/.bazel/tsconfig/project.json"
	editorTestMkdir(t, filepath.Dir(build))
	mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "-type_input="+binDir+"/lib/gen/value.d.ts", "app/consumer.ts"))
	root := binDir + "/editor-program"
	a := actionConfig{project: "app/tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir,
		editorPath: editor, generatedFiles: []string{"gen/value.ts", "lib/gen/value.d.ts"}}
	for _, test := range []struct {
		specifier, selected string
		accepted            bool
	}{
		{specifier: "../gen/value"},
		{specifier: "#value"},
		{specifier: "#authored", selected: "app/authored.ts", accepted: true},
		{specifier: "#fallback", selected: "app/authored.ts"},
		{specifier: "#js-fallback", selected: "app/authored.ts"},
		{specifier: "#explicit-fallback", selected: "app/authored.ts", accepted: true},
		{specifier: "#canonical-fallback", selected: "app/authored.ts", accepted: true},
		{specifier: "../gen/value", selected: "gen/value.d.ts"},
	} {
		t.Run(test.specifier, func(t *testing.T) {
			sources := []string{"baseline.json", "app/tsconfig.json", "app/consumer.ts", "app/authored.ts", "gen/neighbor.ts"}
			if test.selected == "gen/value.d.ts" {
				writeFile(t, test.selected, "export declare const value: string;\n")
				sources = append(sources, test.selected)
				defer editorTestRemove(t, test.selected)
			}
			if err := a.layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			const installed = "previous editor project\n"
			writeFile(t, editor, installed)
			source := "// @ts-ignore\nimport { value } from '" + test.specifier + "';\nexport const result: string = value;\n"
			codes := []string{}
			if test.specifier == "#authored" {
				source += "import { value as missing } from '#canonical'; export { missing };\nimport { value as safe } from '#safe'; export { safe };\n"
				codes = []string{"TS2307", "TS2307"}
			}
			writeFile(t, "app/consumer.ts", source)
			if test.selected != "" {
				t.Chdir(root)
				listing := editorTestListing(t, compiler, build, codes...)
				t.Chdir(cwd)
				selected := explainfiles.Edge{Kind: explainfiles.Import, From: "app/consumer.ts", To: test.selected, Specifier: test.specifier}
				if !slices.Contains(listing.Edges, selected) {
					t.Fatalf("fixture did not reach the compiler-selected fallback: want %+v, got %v", selected, listing.Edges)
				}
			}
			err := editorRun(root, editorTestCommand(compiler, build), a, editor)
			if test.accepted {
				if err != nil {
					t.Fatalf("safe alias fallback, authored precedence, or canonical absence blocked refresh: %v", err)
				}
				listing := editorTestListing(t, compiler, editor, codes...)
				if slices.Contains(listing.Files, "gen/value.ts") || !slices.Contains(listing.Files, "app/authored.ts") {
					t.Fatalf("projection changed native input ownership: %+v", listing)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "stale checkout twin") || !strings.Contains(err.Error(), test.specifier) {
					t.Fatalf("generated scalar probe could still recover its checkout twin: %v", err)
				}
				if got, err := os.ReadFile(editor); err != nil || string(got) != installed {
					t.Fatalf("rejection replaced installed project: %q, %v", got, err)
				}
				editorTestListing(t, compiler, "app/tsconfig.json")
			}
			if got, err := os.ReadFile("gen/value.ts"); err != nil || string(got) != stale {
				t.Fatalf("refresh changed the checkout twin: %q, %v", got, err)
			}
		})
	}
}

func TestEditorEmittedGeneratedSourceCannotMergeStaleGlobalMembers(t *testing.T) {
	compiler := editorTestCompiler(t)
	t.Chdir(t.TempDir())
	writeFile(t, "baseline.json", `{}`)
	writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve","paths":{"#globals":["../gen/globals"]}},"include":["*.ts","../gen/**/*.ts"]}`)
	writeFile(t, "app/a.ts", "import '#globals';\nexport const removed: GeneratedGlobals['removed'] = 'stale';\n")
	writeFile(t, "gen/globals.ts", "export {};\ndeclare global { interface GeneratedGlobals { current: string; removed: string; } }\n")
	writeFile(t, binDir+"/gen/globals.d.ts", "export {};\ndeclare global { interface GeneratedGlobals { current: string; } }\n")
	build := binDir + "/app/program.tsconfig.json"
	editor := "app/.bazel/tsconfig/project.json"
	editorTestMkdir(t, filepath.Dir(build), filepath.Dir(editor))
	mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "-type_input="+binDir+"/gen/globals.d.ts", "app/a.ts"))
	args := []string{"-root=" + binDir + "/editor-program", "-tsconfig=app/tsconfig.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor, "-source=baseline.json", "-source=app/tsconfig.json", "-source=app/a.ts", "-editor-generated-file=gen/globals.d.ts", "-editor-generated-file=gen/globals.ts", "--"}
	if err := runTsgo(append(args, editorTestCommand(compiler, build)...)); err != nil {
		t.Fatal(err)
	}
	listing := editorTestListing(t, compiler, editor, "TS2339")
	if slices.Contains(listing.Files, "gen/globals.ts") || !slices.Contains(listing.Files, binDir+"/gen/globals.d.ts") {
		t.Fatalf("original generated identity did not exclude the stale source: %+v", listing)
	}
	if _, err := os.Stat(binDir + "/gen/globals.ts"); !os.IsNotExist(err) {
		t.Fatalf("editor needed the original generated output: %v", err)
	}
}

func TestEditorRejectsRelativeEdgesNeedingTheOutputOverlay(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_checkout_twin", true: "with_checkout_twin"}[stale], func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler"},"files":["consumer.ts"]}`)
			writeFile(t, "consumer.ts", "import { value } from './generated/value.js';\nexport const result: string = value;\n")
			generated := "generated/value.ts"
			writeFile(t, binDir+"/"+generated, "export const value: string = 'canonical';\n")
			if stale {
				writeFile(t, generated, "export const value: number = 1;\n")
			}
			build := binDir + "/program.tsconfig.json"
			editor := ".bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(editor))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "tsconfig.json", build, "consumer.ts"))
			args := []string{"-root=" + binDir + "/editor-program", "-source=baseline.json", "-source=tsconfig.json", "-source=consumer.ts"}
			command := []string{"--", compiler, "--skipDefaultLibCheck", "-p", build, "--noEmit", "--pretty", "false"}
			if err := runTsgo(append(slices.Clone(args), command...)); err != nil {
				t.Fatalf("ordinary compilation failed: %v", err)
			}
			args = append(args, "-tsconfig=tsconfig.json", "-editor-config="+build, "-editor-baseline=baseline.json", "-editor-bin-dir="+binDir, "-editor-out="+editor, "-editor-path="+editor, "-editor-generated-directory=generated")
			command = append(command, "--explainFiles", "--traceResolution")
			for _, state := range []string{"present", "absent", "suppressed_absent", "absent_reference_path", "absent_reference_without_extension"} {
				reference := "./generated/value.js"
				if state == "absent" {
					editorTestRemove(t, binDir+"/"+generated)
				}
				if state == "suppressed_absent" {
					writeFile(t, "consumer.ts", "// @ts-ignore\nimport { value } from './generated/value.js';\nexport const result: string = value;\n")
				}
				if state == "absent_reference_path" {
					writeFile(t, "consumer.ts", "/// <reference path=\"generated/value.ts\" />\nexport {};\n")
					reference = "generated/value.ts"
				}
				if state == "absent_reference_without_extension" {
					writeFile(t, "consumer.ts", "/// <reference path=\"generated/value\" />\nexport {};\n")
					reference = "generated/value"
				}
				err := runTsgo(append(slices.Clone(args), command...))
				if err == nil || !strings.Contains(err.Error(), "stale checkout twin") || !strings.Contains(err.Error(), reference) {
					t.Fatalf("%s: expected the relative generated edge rejection, got %v", state, err)
				}
				if _, err := os.Stat(editor); !os.IsNotExist(err) {
					t.Fatalf("rejected projection installed an editor project: %v", err)
				}
			}
			if stale {
				got, err := os.ReadFile(generated)
				if err != nil || string(got) != "export const value: number = 1;\n" {
					t.Fatalf("rejection changed the checkout twin: %q, %v", got, err)
				}
			}
			writeFile(t, "consumer.ts", "import { value } from './authored/missing.js';\nexport const result: number = 'unrelated semantic error';\n")
			if err := runTsgo(append(args, command...)); err != nil {
				t.Fatalf("unrelated consumer errors blocked editor refresh: %v", err)
			}
			editorTestListing(t, compiler, editor, "TS2307", "TS2322")
		})
	}
	t.Run("installed_output_boundary", func(t *testing.T) {
		installer := editorTestInstaller(t)
		editorTestChdir(t)
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, "baseline.json", `{}`)
		writeFile(t, "tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler","paths":{"#generated":["./`+binDir+`/generated/index.d.ts"],"#authored":["./authored/value.d.ts"]}},"files":["consumer.ts"]}`)
		const consumer = "import { value } from '#generated';\nexport const result: string = value;\n"
		const generated = "export { value } from '#authored';\n"
		writeFile(t, "consumer.ts", consumer)
		writeFile(t, "authored/value.d.ts", "export declare const value: string;\n")
		output := binDir + "/generated/index.d.ts"
		writeFile(t, output, generated)
		build, editor := binDir+"/program.tsconfig.json", ".bazel/tsconfig/project.json"
		editorTestMkdir(t, filepath.Dir(editor))
		configArgs := editorTestConfigArgs(compiler, "tsconfig.json", build, "consumer.ts")
		mustWriteTsconfig(t, configArgs)
		root := binDir + "/editor-program"
		sources := []string{"baseline.json", "tsconfig.json", "consumer.ts", "authored/value.d.ts"}
		a := actionConfig{project: "tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor, generatedDirectories: []string{"generated"}}
		command := editorTestCommand(compiler, build)
		if err := a.layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := editorRun(root, command, a, editor); err != nil {
			t.Fatal(err)
		}
		physical := filepath.Join(t.TempDir(), "outputs")
		if err := os.Rename(binDir, physical); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(physical, binDir); err != nil {
			t.Fatal(err)
		}
		editorTestInstall(t, installer, editor)
		editorTestRemove(t, binDir)
		if _, err := os.Stat(binDir); !os.IsNotExist(err) {
			t.Fatalf("fixture retained a checkout output link: %v", err)
		}
		listing := editorTestListing(t, compiler, editor)
		canonical := editorTestFileIdentity(t, filepath.Join(physical, "generated/index.d.ts"))
		if !slices.ContainsFunc(listing.Files, func(file string) bool { return editorTestFileIdentity(t, file) == canonical }) {
			t.Fatalf("installed alias did not retain physical output without a checkout link: %v", listing.Files)
		}
		previous, err := os.ReadFile(editor)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, output, generated)
		mustWriteTsconfig(t, configArgs)
		for _, test := range []struct {
			name, from, specifier, to, consumer, generated string
		}{
			{
				name: "authored_to_output", from: "consumer.ts", specifier: "./" + binDir + "/generated/index.js", to: output,
				consumer: "import { value } from './" + binDir + "/generated/index.js';\nexport const result: string = value;\n", generated: generated,
			},
			{
				name: "output_to_authored", from: output, specifier: fileRelative(filepath.ToSlash(filepath.Dir(output)), "authored/value.js"), to: "authored/value.d.ts",
				consumer: consumer, generated: "export { value } from '" + fileRelative(filepath.ToSlash(filepath.Dir(output)), "authored/value.js") + "';\n",
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				writeFile(t, "consumer.ts", test.consumer)
				writeFile(t, output, test.generated)
				if err := a.layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				t.Chdir(root)
				listing := editorTestListing(t, compiler, build)
				t.Chdir(cwd)
				if !slices.ContainsFunc(listing.Edges, func(edge explainfiles.Edge) bool {
					return edge.Kind.ModuleSpecifier() && edge.Specifier == test.specifier &&
						throughRoot(filepath.Join(cwd, root), cwd, canonicalDir(cwd), edge.From) == test.from &&
						throughRoot(filepath.Join(cwd, root), cwd, canonicalDir(cwd), edge.To) == test.to
				}) {
					t.Fatalf("ordinary compiler did not select the crossing: %v", listing.Edges)
				}
				err := editorRun(root, command, a, editor)
				if err == nil {
					t.Fatal("refresh accepted a relative edge across the installed output boundary")
				}
				for _, required := range []string{editor, test.from, test.to, test.specifier, "authored/output namespace boundary", "compilerOptions.paths", "ordinary builds and type checking remain supported"} {
					if !strings.Contains(err.Error(), required) {
						t.Fatalf("relative boundary refusal omits %q: %v", required, err)
					}
				}
				if actual, err := os.ReadFile(editor); err != nil || string(actual) != string(previous) {
					t.Fatalf("relative boundary refusal replaced the installed project: %v", err)
				}
			})
		}
	})
}

func TestEditorModuleSuffixesRetainNativeTreeResolution(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, base, inherited, override string
		rejected, tree, canonical       bool
	}{
		{"relative_suffix_only", "./base.json", `[".native"]`, "", true, false, false},
		{"npm_suffix_with_empty_fallback", "config-package", `[".native", ""]`, "", true, false, false},
		{"leaf_clears_inherited_suffixes", "config-package", `[".native"]`, `,"moduleSuffixes":[]`, false, false, false},
		{"empty_suffix", "./base.json", `[".native"]`, `,"moduleSuffixes":[""]`, false, false, false},
		{"absent_suffixes", "./base.json", "", "", false, false, false},
		{"tree_native_suffix_only", "./base.json", `[".native"]`, "", false, true, false},
		{"tree_native_suffix_with_fallback", "config-package", `[".native", ""]`, "", false, true, false},
		{"scalar_canonical_alias_needs_no_projection", "./base.json", `[".native", ""]`, "", false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			base := `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler"`
			if test.inherited != "" {
				base += `,"moduleSuffixes":` + test.inherited
			}
			base += `}}`
			writeFile(t, "base.json", base)
			writeFile(t, "node_modules/config-package/package.json", `{"name":"config-package","tsconfig":"config.json"}`)
			writeFile(t, "node_modules/config-package/config.json", base)
			writeFile(t, "baseline.json", `{}`)
			target := "./generated/*"
			if test.canonical {
				target = "./" + binDir + "/generated/*"
			}
			writeFile(t, "tsconfig.json", `{"extends":"`+test.base+`","compilerOptions":{"paths":{"#generated/*":["`+target+`"],"#authored/*":["./authored/*"]}`+test.override+`},"files":["consumer.ts"]}`)
			consumer := "import { value } from '#generated/value';\nimport { value as authored } from '#authored/keep';\nexport const kept: string = authored;\n"
			writeFile(t, "consumer.ts", consumer+"export const result: string = value;\n")
			suffix := ""
			if test.inherited != "" && test.override == "" {
				suffix = ".native"
			}
			generated := "generated/value" + suffix + ".d.ts"
			authored := "authored/keep" + suffix + ".ts"
			writeFile(t, authored, "export const value: string = 'authored';\n")
			writeFile(t, binDir+"/"+generated, "export declare const value: string;\n")
			build := binDir + "/program.tsconfig.json"
			editor := ".bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(build), filepath.Dir(editor))
			args := editorTestConfigArgs(compiler, "tsconfig.json", build, "consumer.ts")
			command := editorTestCommand(compiler, build)
			selected := func(project, want string, codes ...string) {
				t.Helper()
				listing := editorTestListing(t, compiler, project, codes...)
				actual := map[string]string{}
				for _, edge := range listing.Edges {
					if edge.Kind == explainfiles.Import && edge.From == "consumer.ts" {
						actual[edge.Specifier] = edge.To
					}
				}
				expected := map[string]string{"#authored/keep": authored}
				if want != "" {
					expected["#generated/value"] = want
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Fatalf("%s selected %v, want %v", project, actual, expected)
				}
			}
			if err := writeTsconfig(args); err != nil {
				t.Fatalf("ordinary TsConfig rejected valid suffix options: %v", err)
			}
			selected(build, binDir+"/"+generated)
			if test.rejected {
				doubled := binDir + "/generated/value.native.native.d.ts"
				writeFile(t, doubled, "export declare const value: number;\n")
				exact := ".bazel/tsconfig/exact.json"
				editorTestWriteJSON(t, exact, map[string]any{
					"extends": "../../tsconfig.json",
					"compilerOptions": map[string]any{"paths": map[string][]string{
						"#generated/value": {fileRelative(filepath.Dir(exact), binDir+"/"+generated)},
						"#authored/*":      {"../../authored/*"},
					}},
				})
				selected(exact, doubled, "TS2322")
			}
			writeFile(t, generated, "export declare const value: number;\n")
			root := binDir + "/editor-program"
			a := actionConfig{project: "tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor}
			if test.tree {
				a.generatedDirectories = []string{"generated"}
			} else {
				a.generatedFiles = []string{generated}
			}
			if err := a.layOutProgramRoot(root, []string{"tsconfig.json", "baseline.json", "base.json", "node_modules/config-package/package.json", "node_modules/config-package/config.json", "consumer.ts", authored}, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			installed := "{\"files\":[],\"include\":[]}\n"
			writeFile(t, editor, installed)
			err := editorRun(root, command, a, editor)
			if test.rejected {
				if err == nil {
					t.Fatal("generated editor accepted unsupported moduleSuffixes")
				}
				for _, required := range []string{editor, "#generated/value", "exact", "moduleSuffixes", ".native", "authored editor project", "without generated_sources", "ordinary builds and type checking remain supported"} {
					if !strings.Contains(err.Error(), required) {
						t.Fatalf("unsupported configuration diagnostic omits %q: %v", required, err)
					}
				}
				if got, err := os.ReadFile(editor); err != nil || string(got) != installed {
					t.Fatalf("rejected configuration replaced the installed project: %q, %v", got, err)
				}
				t.Logf("ordinary compilation succeeded; generated editor rejected: %v", err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			selected(editor, binDir+"/"+generated)
			writeFile(t, "consumer.ts", consumer+"export function invalid(argument) { return value + argument; }\n")
			editorTestListing(t, compiler, editor, "TS7006")
			writeFile(t, "consumer.ts", consumer+"export const result: string = value;\n")
			if test.tree {
				fallback := binDir + "/generated/value.d.ts"
				writeFile(t, fallback, "export declare const value: number;\n")
				selected(editor, binDir+"/"+generated)
				editorTestRemove(t, binDir+"/"+generated)
				if test.inherited == `[".native", ""]` {
					selected(editor, fallback, "TS2322")
				} else {
					selected(editor, "", "TS2307")
				}
				editorTestRemove(t, fallback)
				selected(editor, "", "TS2307")
				if got, err := os.ReadFile(generated); err != nil || string(got) != "export declare const value: number;\n" {
					t.Fatalf("checkout twin changed: %q, %v", got, err)
				}
			}
		})
	}
}

func TestEditorTreeAliasReadsAuthoredSuffixChanges(t *testing.T) {
	compiler := editorTestCompiler(t)
	t.Chdir(t.TempDir())
	writeFile(t, "baseline.json", `{}`)
	const base = `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler","moduleSuffixes":[".native",""]}}`
	writeFile(t, "shared/base.json", base)
	writeFile(t, "tsconfig.json", `{"extends":"./shared/base.json","compilerOptions":{"paths":{"#value":["./generated/value"]}},"files":["consumer.ts"]}`)
	writeFile(t, "consumer.ts", "import { value } from '#value';\nexport const result: string = value;\n")
	for _, suffix := range []string{"native", "ios"} {
		writeFile(t, binDir+"/generated/value."+suffix+".d.ts", "export declare const value: string;\n")
		writeFile(t, "generated/value."+suffix+".d.ts", "export declare const value: number;\n")
	}
	build, editor := binDir+"/program.tsconfig.json", ".bazel/tsconfig/project.json"
	mustWriteTsconfig(t, editorTestConfigArgs(compiler, "tsconfig.json", build, "consumer.ts"))
	editorTestMkdir(t, filepath.Dir(editor))
	root := binDir + "/editor-program"
	a := actionConfig{project: "tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor, generatedDirectories: []string{"generated"}}
	if err := a.layOutProgramRoot(root, []string{"baseline.json", "shared/base.json", "tsconfig.json", "consumer.ts"}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := editorRun(root, editorTestCommand(compiler, build), a, editor); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(editor)
	if err != nil {
		t.Fatal(err)
	}
	if value, known := readJSON(t, editor)["compilerOptions"].(map[string]any)["outDir"]; !known || value != nil {
		t.Fatalf("editor leaves output context inherited: outDir = %v, present = %t", value, known)
	}
	for _, suffix := range []string{"native", "ios"} {
		writeFile(t, "shared/base.json", strings.Replace(base, ".native", "."+suffix, 1))
		listing := editorTestListing(t, compiler, editor)
		want := explainfiles.Edge{Kind: explainfiles.Import, From: "consumer.ts", Specifier: "#value", To: binDir + "/generated/value." + suffix + ".d.ts"}
		if !slices.Contains(listing.Edges, want) {
			t.Fatalf("authored suffix .%s did not select its canonical file: %v", suffix, listing.Edges)
		}
		if got, err := os.ReadFile(editor); err != nil || string(got) != string(installed) {
			t.Fatalf("authored option edit changed the installed project: %v", err)
		}
	}
}

func TestEditorPlacementPreservesLeafAmbientTypesWithSharedAuthoredConfig(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, types, roots, source, selected, diagnostic string
	}{
		{"nested", `["choice"]`, "", `export const value: "nested" = ambient;`, "pkg/nested/node_modules/@types/choice/index.d.ts", ""},
		{"empty", `[]`, "", `export {};`, "", ""},
		{"missing", `["absent"]`, "", `export {};`, "", "TS2688"},
		{"custom", `["choice"]`, `,"typeRoots":["./typings"]`, `export const value: "custom" = ambient;`, "shared/typings/choice/index.d.ts", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeFile(t, "node_modules/@types/choice/index.d.ts", `declare const ambient: "root";`)
			writeFile(t, "pkg/nested/node_modules/@types/choice/index.d.ts", `declare const ambient: "nested";`)
			writeFile(t, "shared/typings/choice/index.d.ts", `declare const ambient: "custom";`)
			writeFile(t, "shared/tsconfig.json", `{"compilerOptions":{"strict":true,"types":`+test.types+test.roots+`}}`)
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "pkg/nested/input.ts", test.source)
			writeFile(t, "pkg/nested/reference.json", `{"extends":"../../shared/tsconfig.json","files":["input.ts"]}`)
			check := func(project string) string {
				t.Helper()
				output, err := exec.CommandContext(t.Context(), compiler, "--skipDefaultLibCheck", "--project", project, "--noEmit", "--pretty", "false", "--listFiles").CombinedOutput()
				text := filepath.ToSlash(string(output))
				if test.diagnostic == "" && err != nil || test.diagnostic != "" && (err == nil || !strings.Contains(text, test.diagnostic)) {
					t.Fatalf("%s: %v\n%s", project, err, output)
				}
				if test.selected != "" && !strings.Contains(text, test.selected) {
					t.Fatalf("%s lost selected ambient file %s:\n%s", project, test.selected, text)
				}
				if test.name == "empty" && strings.Contains(text, "/@types/choice/") {
					t.Fatalf("empty types gained ambient package: %s", text)
				}
				t.Logf("project=%s\n%s", project, text)
				return text
			}
			check("pkg/nested/reference.json")
			var types []string
			if err := json.Unmarshal([]byte(test.types), &types); err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{"library", "tests"} {
				a := actionConfig{project: "shared/tsconfig.json", baseline: "baseline.json", out: "bazel-out/test/bin/pkg/nested/" + target + ".tsconfig.json", binDir: "bazel-out/test/bin", editorPath: "pkg/nested/.bazel/tsconfig/" + target + ".json"}
				config := &tsconfigFile{CompilerOptions: map[string]any{"types": types}}
				projection := editorProjection{actionConfig: &a}
				editor := projection.editorConfig(config, nil)
				editor["files"] = []string{"../../input.ts"}
				raw, err := json.Marshal(editor)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, a.editorPath, string(raw))
				check(a.editorPath)
			}
		})
	}
}

func TestEditorPlacementPreservesAbsoluteAuthoredPaths(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, checkout := range []string{"canonical", "symlink_checkout"} {
		t.Run(checkout, func(t *testing.T) {
			if checkout == "symlink_checkout" {
				directory := linkedDir(t)
				t.Chdir(directory)
				if cwd, err := os.Getwd(); err != nil || cwd != directory {
					t.Fatalf("fixture lost its checkout alias: %q, %v", cwd, err)
				}
			} else {
				editorTestChdir(t)
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			abs := func(file string) string { return filepath.ToSlash(filepath.Join(cwd, file)) }
			typeRoot := filepath.ToSlash(t.TempDir())
			declaration := typeRoot + "/choice/index.d.ts"
			writeFile(t, declaration, `declare const ambient: "absolute";`)
			externalRoot := typeRoot + "/input.d.ts"
			writeFile(t, externalRoot, `declare const outsideRoot: "outside";`)
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "app/input.ts", `export const value: "absolute" = ambient; export const other: "included" = included; export const outside: "outside" = outsideRoot;`)
			writeFile(t, "app/ambient/included.ts", `declare const included: "included";`)
			writeFile(t, "app/ambient/excluded.ts", `const excluded: never = "must stay excluded";`)
			config := &tsconfigFile{
				CompilerOptions: map[string]any{"strict": true, "types": []string{"choice"}, "typeRoots": []any{typeRoot}},
				Files:           []string{abs("app/input.ts"), externalRoot},
				Include:         []string{abs("app/ambient/**/*.ts")},
				Exclude:         []string{abs("app/ambient/excluded.ts")},
			}
			a := actionConfig{project: "app/tsconfig.build.json", baseline: "baseline.json", out: binDir + "/app/program.tsconfig.json", binDir: binDir, editorPath: "app/.bazel/tsconfig/project.json"}
			editorTestWriteJSON(t, a.project, config)
			check := func(project string) {
				t.Helper()
				listing := editorTestListing(t, compiler, project)
				files := map[string]bool{}
				for _, file := range listing.Files {
					files[filepath.ToSlash(editorTestFileIdentity(t, file))] = true
				}
				for _, file := range []string{abs("app/input.ts"), abs("app/ambient/included.ts"), declaration, externalRoot} {
					if !files[filepath.ToSlash(editorTestFileIdentity(t, file))] {
						t.Fatalf("%s lost absolute authored input %s: %v", project, file, listing.Files)
					}
				}
				if files[filepath.ToSlash(editorTestFileIdentity(t, abs("app/ambient/excluded.ts")))] {
					t.Fatalf("%s lost absolute authored exclusion: %v", project, listing.Files)
				}
			}
			check(a.project)
			editorTestMkdir(t, filepath.Dir(a.editorPath))
			editorTestWriteJSON(t, a.out, config)
			root := binDir + "/app/editor-program"
			sources := []string{"baseline.json", a.project, "app/input.ts", "app/ambient/included.ts", "app/ambient/excluded.ts"}
			if err := a.layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := editorRun(root, editorTestCommand(compiler, a.out), a, a.editorPath); err != nil {
				t.Fatal(err)
			}
			check(a.editorPath)
			installed := readJSON(t, a.editorPath)
			if got := installed["compilerOptions"].(map[string]any)["typeRoots"].([]any); len(got) != 1 || got[0] != typeRoot {
				t.Fatalf("editor rewrote external authored type root: %v", got)
			}
		})
	}
}

func TestEditorLiteralRootsCannotAdmitGlobMatchingSiblings(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, literal, sibling string
		generated              bool
	}{
		{"authored_question_mark", "app/src/a?.d.ts", "app/src/ab.d.ts", false},
		{"generated_asterisk", "app/src/generated/a*.d.ts", "app/src/generated/ab.d.ts", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			editorTestChdir(t)
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true},"files":["pinned.ts"],"include":["src/**/*.ts"],"exclude":["pinned.ts","src/excluded.ts"]}`)
			writeFile(t, "app/pinned.ts", "export {};\n")
			writeFile(t, "app/src/consumer.ts", "export const value: 'selected' = literalRoot;\n")
			writeFile(t, "app/src/excluded.ts", "export const invalid: string = 0;\n")
			build := binDir + "/app/program.tsconfig.json"
			editor := "app/.bazel/tsconfig/project.json"
			root := binDir + "/app/editor-program"
			sources := []string{"baseline.json", "app/tsconfig.json", "app/pinned.ts", "app/src/consumer.ts", "app/src/excluded.ts"}
			a := actionConfig{project: "app/tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor}
			selected, sibling := test.literal, test.sibling
			var overlays []string
			if test.generated {
				selected, sibling = binDir+"/"+selected, binDir+"/"+sibling
				a.generatedDirectories = []string{"app/src/generated"}
				overlays = []string{binDir + "/app/src/generated"}
				writeFile(t, test.literal, "declare const literalRoot: 'stale';\n")
			} else {
				sources = append(sources, test.literal)
			}
			writeFile(t, selected, "declare const literalRoot: 'selected';\n")
			editorTestMkdir(t, filepath.Dir(build))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, a.project, build, "app/src/consumer.ts"))
			if err := a.layOutProgramRoot(root, sources, nil, nil, overlayPairs(overlays...), nil); err != nil {
				t.Fatal(err)
			}
			writeFile(t, sibling, "declare const unexpected: Missing;\n")
			t.Chdir(root)
			observed := editorTestListing(t, compiler, build)
			want := []string{"app/pinned.ts", "app/src/consumer.ts", test.literal}
			slices.Sort(want)
			slices.Sort(observed.Roots)
			if !slices.Equal(observed.Roots, want) {
				t.Fatalf("compiler selected roots %v, want %v", observed.Roots, want)
			}
			t.Chdir(cwd)
			editorTestMkdir(t, filepath.Dir(editor))
			if err := editorRun(root, editorTestCommand(compiler, build), a, editor); err != nil {
				t.Fatal(err)
			}
			installed := editorTestListing(t, compiler, editor)
			want = []string{"app/pinned.ts", "app/src/consumer.ts", selected}
			slices.Sort(want)
			slices.Sort(installed.Roots)
			if !slices.Equal(installed.Roots, want) {
				t.Fatalf("editor changed literal root membership: got %v, want %v", installed.Roots, want)
			}
		})
	}
}

// A generated type root sits at its compiler path, so a source exclude of that path drops it too.
func TestEditorExclusionsDropGeneratedTypeRootAtItsCompilerPath(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, exclude string
		included      bool
	}{
		{"source_exclusion_drops_ambient_input", "generated/types", false},
		{"output_exclusion_drops_ambient_input", "../" + binDir + "/app/generated/types", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "app/tsconfig.build.json", `{"compilerOptions":{"strict":true,"types":["./generated/types"]},"include":["input.ts"],"exclude":["`+test.exclude+`"]}`)
			writeFile(t, "app/input.ts", "export const value: string = generatedValue;\n")
			declaration := "app/generated/types/index.d.ts"
			writeFile(t, binDir+"/"+declaration, "declare const generatedValue: string;\n")
			if err := os.Symlink(binDir, "bazel-bin"); err != nil {
				t.Fatal(err)
			}
			build := binDir + "/app/program.tsconfig.json"
			editor := "app/.bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(editor))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.build.json", build, "app/input.ts"))
			check := func(project, canonical string) {
				t.Helper()
				codes := []string{}
				if !test.included {
					codes = []string{"TS2304"}
				}
				listing := editorTestListing(t, compiler, project, codes...)
				if slices.Contains(listing.Files, canonical) != test.included {
					t.Fatalf("ambient membership for %s must be included=%v: %v", project, test.included, listing.Files)
				}
				t.Logf("ambient membership project=%s canonical=%s included=%v", project, canonical, test.included)
			}
			check(build, binDir+"/"+declaration)
			args := []string{"-root=" + binDir + "/app/editor-program", "-tsconfig=app/tsconfig.build.json", "-editor-config=" + build, "-editor-baseline=baseline.json", "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor, "-editor-generated-directory=app/generated/types"}
			for _, file := range []string{"baseline.json", "app/tsconfig.build.json", "app/input.ts"} {
				args = append(args, "-source="+file)
			}
			args = append(args, "--")
			args = append(args, editorTestCommand(compiler, build)...)
			if err := runTsgo(args); err != nil {
				t.Fatal(err)
			}
			check(editor, binDir+"/"+declaration)

		})
	}
}

func TestEditorBroadIncludeRetainsOnlyCompilerGeneratedRoots(t *testing.T) {
	compiler := editorTestCompiler(t)
	installer := editorTestInstaller(t)
	for _, test := range []struct {
		name               string
		canonical, outputs bool
	}{
		{"overlay", false, true},
		{"canonical", true, true},
		{"overlay_without_output", false, false},
		{"canonical_without_output", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			editorTestChdir(t)
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, "baseline.json", `{}`)
			generated := "pkg/src/generated"
			declaration := generated + "/ambient.d.ts"
			imported := generated + "/ignored/imported.d.ts"
			excluded := generated + "/ignored/excluded.d.ts"
			include := []string{"src/**/*.ts"}
			exclude := []string{"pinned.ts", "src/excluded.ts", "src/generated/ignored"}
			overlays := []string{binDir + "/" + generated}
			if test.canonical {
				include = append(include, "../"+binDir+"/pkg/src/**/*.ts")
				exclude = append(exclude, "../"+binDir+"/"+generated+"/ignored")
				overlays = nil
			}
			options := map[string]any{"strict": true, "module": "preserve", "moduleResolution": "bundler", "paths": map[string][]string{"#generated/*": {"../" + binDir + "/" + generated + "/*"}}}
			if test.outputs {
				options["outDir"] = "./dist"
			}
			editorTestWriteJSON(t, "pkg/tsconfig.json", map[string]any{
				"compilerOptions": options,
				"files":           []string{"pinned.ts"},
				"include":         include,
				"exclude":         exclude,
			})
			consumer := func(member string) {
				writeFile(t, "pkg/src/consumer.ts", "import { api } from 'generated-api';\nimport type { Model } from '#generated/"+strings.TrimPrefix(imported, generated+"/")+"';\nexport const value: Model = api."+member+";\n")
			}
			consumer("added")
			writeFile(t, "pkg/pinned.ts", "export {};\n")
			writeFile(t, "pkg/src/excluded.ts", "export const invalid: string = 0;\n")
			writeFile(t, "pkg/src/sibling.test.ts", "export const invalid: string = 0;\n")
			const initial = "declare module 'generated-api' { export const api: { added: string }; }\n"
			const replacement = "declare module 'generated-api' { export const api: { replacement: string }; }\n"
			writeFile(t, binDir+"/"+declaration, initial)
			writeFile(t, binDir+"/"+imported, "export type Model = string;\n")
			writeFile(t, binDir+"/"+excluded, "declare const invalid: Missing;\n")
			build := binDir + "/pkg/program.tsconfig.json"
			editor := "pkg/.bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(editor))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "pkg/tsconfig.json", build, "pkg/src/consumer.ts"))
			writeFile(t, declaration, initial)
			writeFile(t, generated+"/stale-only.ts", "export const invalid: string = 0;\n")
			root := binDir + "/pkg/editor-program"
			sources := []string{"baseline.json", "pkg/tsconfig.json", "pkg/pinned.ts", "pkg/src/consumer.ts", "pkg/src/excluded.ts"}
			a := actionConfig{project: "pkg/tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor, generatedDirectories: []string{generated}}
			command := editorTestCommand(compiler, build)
			check := func(listing *explainfiles.Listing, ambient string, present bool) {
				t.Helper()
				for _, file := range []string{"pkg/pinned.ts", "pkg/src/consumer.ts", "pkg/src/added.ts"} {
					if slices.Contains(listing.Roots, file) != slices.Contains(sources, file) {
						t.Fatalf("authored root %s differs from action inputs %v: %v", file, sources, listing.Roots)
					}
				}
				if slices.Contains(listing.Roots, ambient) != present || !slices.Contains(listing.Files, binDir+"/"+imported) || slices.Contains(listing.Roots, binDir+"/"+imported) {
					t.Fatalf("ambient root present=%t or imported-only membership changed: %+v", present, listing)
				}
				for _, file := range []string{"pkg/src/excluded.ts", "pkg/src/sibling.test.ts", excluded, binDir + "/" + excluded, generated + "/stale-only.ts"} {
					if slices.Contains(listing.Files, file) {
						t.Fatalf("excluded file %s entered the program: %v", file, listing.Files)
					}
				}
				if ambient != declaration && slices.Contains(listing.Files, declaration) {
					t.Fatalf("editor read stale checkout declaration: %v", listing.Files)
				}
			}
			refresh := func(present bool, codes ...string) {
				t.Helper()
				if err := a.layOutProgramRoot(root, sources, nil, nil, overlayPairs(overlays...), nil); err != nil {
					t.Fatal(err)
				}
				t.Chdir(root)
				ambient := declaration
				if test.canonical {
					ambient = binDir + "/" + declaration
				}
				check(editorTestListing(t, compiler, build, codes...), ambient, present)
				t.Chdir(cwd)
				if err := editorRun(root, command, a, editor); err != nil {
					t.Fatal(err)
				}
				var projected struct {
					RootDirSources []string `json:"rootDirSources"`
				}
				raw, err := os.ReadFile(editor)
				if err != nil || json.Unmarshal(raw, &projected) != nil || projected.RootDirSources == nil || len(projected.RootDirSources) != 0 {
					t.Fatalf("declaration-only generated program claimed rootDir containment: %s, %v", raw, err)
				}
			}
			refresh(true)
			check(editorTestListing(t, compiler, editor), binDir+"/"+declaration, true)
			writeFile(t, binDir+"/"+declaration, replacement)
			check(editorTestListing(t, compiler, editor, "TS2339"), binDir+"/"+declaration, true)
			consumer("replacement")
			check(editorTestListing(t, compiler, editor), binDir+"/"+declaration, true)
			writeFile(t, "pkg/src/added.ts", "export const invalid: string = 0;\n")
			check(editorTestListing(t, compiler, editor), binDir+"/"+declaration, true)
			editorTestRemove(t, binDir+"/"+declaration)
			check(editorTestListing(t, compiler, editor, "TS6053"), binDir+"/"+declaration, false)
			writeFile(t, "pkg/src/added.ts", "export const added: string = 'declared';\n")
			sources = append(sources, "pkg/src/added.ts")
			refresh(false, "TS2307")
			check(editorTestListing(t, compiler, editor, "TS2307"), binDir+"/"+declaration, false)
			writeFile(t, binDir+"/"+declaration, replacement)
			refresh(true)
			check(editorTestListing(t, compiler, editor), binDir+"/"+declaration, true)
			if got, err := os.ReadFile(declaration); err != nil || string(got) != initial {
				t.Fatalf("checkout twin changed: %q, %v", got, err)
			}
			explicit := "pkg/explicit/ambient.d.ts"
			const staleExplicit = "declare const explicitRoot: 'stale';\n"
			writeFile(t, explicit, staleExplicit)
			authored := readJSON(t, "pkg/tsconfig.json")
			authored["files"] = []string{"pinned.ts", "src/sibling.test.ts", "explicit/ambient.d.ts"}
			editorTestWriteJSON(t, "pkg/tsconfig.json", authored)
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "pkg/tsconfig.json", build, "pkg/src/consumer.ts"))
			overlays = append(overlays, binDir+"/pkg/explicit")
			for _, step := range []struct {
				present, declaredTree, extensionless bool
			}{
				{present: true},
				{},
				{present: true},
				{present: true, declaredTree: true},
				{declaredTree: true},
				{present: true, declaredTree: true},
				{present: true, declaredTree: true, extensionless: true},
			} {
				a.generatedFiles = nil
				a.generatedDirectories = []string{generated}
				if step.declaredTree {
					a.generatedDirectories = append(a.generatedDirectories, "pkg/explicit")
				}
				if step.extensionless {
					authored["files"] = []string{"pinned.ts", "src/sibling.test.ts", "explicit/ambient"}
					editorTestWriteJSON(t, "pkg/tsconfig.json", authored)
					mustWriteTsconfig(t, editorTestConfigArgs(compiler, "pkg/tsconfig.json", build, "pkg/src/consumer.ts"))
				}
				codes := []string{"TS6053"}
				if step.present {
					writeFile(t, binDir+"/"+explicit, "declare const explicitRoot: 'current';\n")
					if !step.declaredTree {
						a.generatedFiles = []string{explicit}
					}
				} else {
					editorTestRemove(t, binDir+"/"+explicit)
					codes = append(codes, "TS6053")
				}
				if err := a.layOutProgramRoot(root, sources, nil, nil, overlayPairs(overlays...), nil); err != nil {
					t.Fatal(err)
				}
				t.Chdir(root)
				observed := editorTestListing(t, compiler, build, codes...)
				if slices.Contains(observed.Roots, explicit) != step.present || slices.Contains(observed.Files, "pkg/src/sibling.test.ts") {
					t.Fatalf("compiler explicit-root membership: step=%+v, roots=%v, files=%v", step, observed.Roots, observed.Files)
				}
				t.Chdir(cwd)
				previous, err := os.ReadFile(editor)
				if err != nil {
					t.Fatal(err)
				}
				err = editorRun(root, command, a, editor)
				if step.declaredTree && (!step.present || step.extensionless) {
					if err == nil || !strings.Contains(err.Error(), "no exact compiler-loaded identity") || !strings.Contains(err.Error(), "selected filename and extension") {
						t.Fatalf("unprovable explicit generated root was accepted or lacked recovery guidance: step=%+v, error=%v", step, err)
					}
					if actual, err := os.ReadFile(editor); err != nil || string(actual) != string(previous) {
						t.Fatalf("unprovable explicit generated root replaced the previous editor project: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				var installedCodes []string
				if step.declaredTree && !step.present {
					installedCodes = []string{"TS6053"}
				}
				installed := editorTestListing(t, compiler, editor, installedCodes...)
				if len(installedCodes) != 0 && !strings.Contains(installed.Diagnostics[0], binDir+"/"+explicit) {
					t.Fatalf("missing explicit root lost canonical output identity: %v", installed.Diagnostics)
				}
				check(installed, binDir+"/"+declaration, true)
				if slices.Contains(installed.Roots, binDir+"/"+explicit) != step.present || slices.Contains(installed.Files, explicit) {
					t.Fatalf("editor admitted an unobserved explicit root or lost a generated root: step=%+v, roots=%v, files=%v", step, installed.Roots, installed.Files)
				}
			}
			if got, err := os.ReadFile(explicit); err != nil || string(got) != staleExplicit {
				t.Fatalf("explicit checkout twin changed: %q, %v", got, err)
			}
			authored["files"] = []string{"pinned.ts"}
			editorTestWriteJSON(t, "pkg/tsconfig.json", authored)
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "pkg/tsconfig.json", build, "pkg/src/consumer.ts"))
			a.generatedFiles = nil
			a.generatedDirectories = []string{generated}
			overlays = overlays[:len(overlays)-1]
			previous, err := os.ReadFile(editor)
			if err != nil {
				t.Fatal(err)
			}
			imported = generated + "/ignored/nonroot.ts"
			writeFile(t, binDir+"/"+imported, "export type Model = string;\n")
			consumer("replacement")
			if err := a.layOutProgramRoot(root, sources, nil, nil, overlayPairs(overlays...), nil); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			listing := editorTestListing(t, compiler, build)
			t.Chdir(cwd)
			if !slices.Contains(listing.Files, binDir+"/"+imported) || slices.Contains(listing.Roots, binDir+"/"+imported) {
				t.Fatalf("fixture did not select an imported-only generated source: %+v", listing)
			}
			err = editorRun(root, command, a, editor)
			if test.outputs {
				if err == nil || !strings.Contains(err.Error(), "cannot establish rootDir containment") {
					t.Fatalf("unknown external-library membership with output directories was accepted: %v", err)
				}
				if actual, err := os.ReadFile(editor); err != nil || string(actual) != string(previous) {
					t.Fatalf("ambiguous containment replaced the previous editor project: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("imported generated source without output directories was rejected: %v", err)
			}
			var projected struct {
				CompilerOptions map[string]json.RawMessage `json:"compilerOptions"`
				RootDirSources  []string                   `json:"rootDirSources"`
			}
			raw, err := os.ReadFile(editor)
			if err != nil || json.Unmarshal(raw, &projected) != nil {
				t.Fatalf("cannot read projected containment: %v", err)
			}
			for _, option := range []string{"outDir", "declarationDir"} {
				if string(projected.CompilerOptions[option]) != "null" {
					t.Fatalf("%s must explicitly disable output-to-source mapping: %s", option, raw)
				}
			}
			wantContainment := []string{fileRelative(filepath.ToSlash(filepath.Dir(editor)), binDir+"/"+imported)}
			if !slices.Equal(projected.RootDirSources, wantContainment) {
				t.Fatalf("imported generated source lost containment: %v, want %v", projected.RootDirSources, wantContainment)
			}
			editorTestInstall(t, installer, editor)
			installed := editorTestListing(t, compiler, editor)
			ambient, err := filepath.Rel(cwd, editorTestFileIdentity(t, binDir+"/"+declaration))
			if err != nil {
				t.Fatal(err)
			}
			check(installed, filepath.ToSlash(ambient), true)
			if slices.Contains(installed.Roots, binDir+"/"+imported) {
				t.Fatal("containment promoted an imported source to a program root")
			}
		})
	}
}

func TestEditorGeneratedTypeRootsCannotReadCheckoutTwins(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, checkout := range []string{"canonical", "symlink_checkout", "sandbox_inputs"} {
		t.Run(checkout, func(t *testing.T) {
			for _, test := range []struct {
				name, artifact                         string
				scalar, rejected, defaultRoots, absent bool
			}{
				{name: "whole_root", artifact: "app/generated-types"},
				{name: "child_tree", artifact: "app/generated-types/api", rejected: true},
				{name: "scalar_package", artifact: "app/generated-types/api/index.d.ts", scalar: true, rejected: true},
				{name: "default_root_absent", artifact: "app/node_modules/@types", defaultRoots: true, rejected: true, absent: true},
				{name: "default_root_stale", artifact: "app/node_modules/@types", defaultRoots: true, rejected: true},
			} {
				for _, explicit := range []bool{false, true} {
					t.Run(test.name+map[bool]string{false: "/implicit", true: "/explicit"}[explicit], func(t *testing.T) {
						if checkout == "symlink_checkout" {
							directory := linkedDir(t)
							t.Chdir(directory)
							if cwd, err := os.Getwd(); err != nil || cwd != directory {
								t.Fatalf("fixture lost its checkout alias: %q, %v", cwd, err)
							}
						} else {
							editorTestChdir(t)
						}
						writeFile(t, "baseline.json", `{}`)
						types := `,"types":["*"]`
						if explicit {
							types = `,"types":["api","local"]`
						}
						roots := `,"typeRoots":["../app/generated-types","./typings"]`
						typeRoot := "app/generated-types"
						local := "shared/typings/local/index.d.ts"
						if test.defaultRoots {
							roots = ""
							typeRoot = "app/node_modules/@types"
							local = "node_modules/@types/local/index.d.ts"
						}
						writeFile(t, "shared/base.json", `{"compilerOptions":{"strict":true`+roots+types+`}}`)
						writeFile(t, "app/tsconfig.build.json", `{"extends":"../shared/base.json","files":["input.ts"]}`)
						writeFile(t, "app/input.ts", "export const value: string = api.added;\nexport const local: string = authoredValue;\n")
						writeFile(t, local, "declare const authoredValue: string;\n")
						if checkout == "sandbox_inputs" {
							editorTestSymlinkInput(t, local)
						}
						declaration := typeRoot + "/api/index.d.ts"
						if !test.absent {
							writeFile(t, declaration, "declare const api: { stale: number };\n")
						}
						writeFile(t, binDir+"/"+declaration, "declare const api: { added: string };\n")
						build := binDir + "/app/program.tsconfig.json"
						editor := "app/.bazel/tsconfig/project.json"
						editorTestMkdir(t, filepath.Dir(editor))
						mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.build.json", build, "app/input.ts"))
						args := []string{"-root=" + binDir + "/app/editor-program", overlayFlag(binDir + "/app")}
						for _, file := range []string{"baseline.json", "shared/base.json", "app/tsconfig.build.json", "app/input.ts", local} {
							args = append(args, "-source="+file)
						}
						command := []string{"--", compiler, "--skipDefaultLibCheck", "-p", build, "--noEmit", "--pretty", "false"}
						buildArgs := append(slices.Clone(args), command...)
						if err := runTsgo(buildArgs); err != nil {
							t.Fatalf("ordinary compilation failed: %v", err)
						}
						args = append(args, "-tsconfig=app/tsconfig.build.json", "-editor-config="+build, "-editor-baseline=baseline.json", "-editor-bin-dir="+binDir, "-editor-out="+editor, "-editor-path="+editor)
						flag := "-editor-generated-directory="
						if test.scalar {
							flag = "-editor-generated-file="
						}
						args = append(args, flag+test.artifact)
						args = append(args, append(command, "--explainFiles", "--traceResolution")...)
						if test.rejected {
							for _, existing := range []bool{false, true} {
								const previous = "previous editor project\n"
								if existing {
									writeFile(t, editor, previous)
								}
								err := runTsgo(args)
								if err == nil {
									t.Fatal("generated editor accepted a type package that can read a stale checkout twin")
								}
								for _, required := range []string{editor, "typeRoots", typeRoot, test.artifact, "stale checkout", "generate the whole type root", "without generated_sources", "ordinary builds and type checking remain supported"} {
									if !strings.Contains(err.Error(), required) {
										t.Fatalf("unsupported type root diagnostic omits %q: %v", required, err)
									}
								}
								got, err := os.ReadFile(editor)
								if existing {
									if err != nil || string(got) != previous {
										t.Fatalf("rejection replaced the installed project: %q, %v", got, err)
									}
								} else if !os.IsNotExist(err) {
									t.Fatalf("rejection created an editor project: %q, %v", got, err)
								}
							}
							if err := runTsgo(buildArgs); err != nil {
								t.Fatalf("editor rejection blocked ordinary compilation: %v", err)
							}
						} else {
							if err := runTsgo(args); err != nil {
								t.Fatal(err)
							}
							for _, state := range []string{"initial", "replacement", "deletion"} {
								codes := []string{}
								switch state {
								case "replacement":
									writeFile(t, binDir+"/"+declaration, "declare const api: { replacement: string };\n")
									codes = []string{"TS2339"}
								case "deletion":
									editorTestRemove(t, binDir+"/"+declaration)
									codes = []string{"TS2688"}
									if !explicit {
										editorTestRemove(t, filepath.Dir(binDir+"/"+declaration))
										codes = []string{"TS2304"}
									}
								}
								listing := editorTestListing(t, compiler, editor, codes...)
								files := map[string]bool{}
								for _, file := range listing.Files {
									files[editorTestFileIdentity(t, file)] = true
								}
								workspace := editorTestFileIdentity(t, ".")
								if files[filepath.Join(workspace, declaration)] || !files[editorTestFileIdentity(t, local)] || files[filepath.Join(workspace, binDir, declaration)] != (state != "deletion") {
									t.Fatalf("%s: generated type root lost authority or authored root changed: %v", state, listing.Files)
								}
							}
						}
						if got, err := os.ReadFile(declaration); test.absent {
							if !os.IsNotExist(err) {
								t.Fatalf("refresh created a checkout twin: %q, %v", got, err)
							}
						} else if err != nil || string(got) != "declare const api: { stale: number };\n" {
							t.Fatalf("checkout twin changed: %q, %v", got, err)
						}
					})
				}
			}
		})
	}
}

func TestEditorEmptyTypesAllowsAliasOnlyGeneratedTypeRoot(t *testing.T) {
	compiler := editorTestCompiler(t)
	t.Chdir(t.TempDir())
	writeFile(t, "baseline.json", `{}`)
	writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true,"module":"preserve","types":[],"typeRoots":["./typings"],"paths":{"#api":["./typings/api/index.d.ts"]}},"files":["consumer.ts"]}`)
	const consumer = "import { value } from '#api';\nexport const result: string = value;\n"
	writeFile(t, "app/consumer.ts", consumer)
	const generated = "app/typings/api/index.d.ts"
	const stale = "export declare const value: number;\n"
	writeFile(t, generated, stale)
	writeFile(t, binDir+"/"+generated, "export declare const value: string;\n")
	build, editor := binDir+"/app/program.tsconfig.json", "app/.bazel/tsconfig/project.json"
	mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "app/consumer.ts"))
	editorTestMkdir(t, filepath.Dir(editor))
	root := binDir + "/editor-program"
	a := actionConfig{project: "app/tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor, generatedDirectories: []string{"app/typings/api"}}
	if err := a.layOutProgramRoot(root, []string{"baseline.json", "app/tsconfig.json", "app/consumer.ts"}, nil, nil, overlayPairs([]string{binDir + "/app"}...), nil); err != nil {
		t.Fatal(err)
	}
	command := editorTestCommand(compiler, build)
	if err := editorRun(root, command, a, editor); err != nil {
		t.Fatal(err)
	}
	listing := editorTestListing(t, compiler, editor)
	want := explainfiles.Edge{Kind: explainfiles.Import, From: "app/consumer.ts", Specifier: "#api", To: binDir + "/" + generated}
	if !slices.Contains(listing.Edges, want) || slices.Contains(listing.Files, generated) {
		t.Fatalf("alias-only program lost canonical output authority: %+v", listing)
	}
	installed, err := os.ReadFile(editor)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, "app/consumer.ts", "/// <reference types=\"api\" />\n"+consumer)
	if err := editorRun(root, command, a, editor); err == nil || !strings.Contains(err.Error(), "stale checkout packages") {
		t.Fatalf("types: [] hid a source type-reference lookup: %v", err)
	}
	if got, err := os.ReadFile(editor); err != nil || string(got) != string(installed) {
		t.Fatalf("refused type-reference lookup replaced the installed project: %v", err)
	}
	writeFile(t, "app/consumer.ts", consumer)
	editorTestRemove(t, binDir+"/"+generated)
	listing = editorTestListing(t, compiler, editor, "TS2307")
	if slices.Contains(listing.Files, generated) {
		t.Fatalf("deleted output selected a stale type-root twin: %v", listing.Files)
	}
	if got, err := os.ReadFile(generated); err != nil || string(got) != stale {
		t.Fatalf("refresh changed the checkout twin: %q, %v", got, err)
	}
}

func TestEditorMixedTypeRootRejectsBeforeInstallation(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, checkout := range []string{"canonical", "symlink_checkout", "sandbox_inputs", "sandbox_aliases"} {
		t.Run(checkout, func(t *testing.T) {
			for _, mode := range []string{"implicit", "explicit", "source_directive"} {
				for _, twin := range []bool{false, true} {
					name := mode + map[bool]string{false: "/absent", true: "/conflicting"}[twin]
					t.Run(name, func(t *testing.T) {
						if checkout == "symlink_checkout" {
							directory := linkedDir(t)
							t.Chdir(directory)
							if cwd, err := os.Getwd(); err != nil || cwd != directory {
								t.Fatalf("fixture lost its checkout alias: %q, %v", cwd, err)
							}
						} else {
							editorTestChdir(t)
						}
						cwd, err := os.Getwd()
						if err != nil {
							t.Fatal(err)
						}
						writeFile(t, "baseline.json", `{}`)
						types := `,"types":["*"]`
						consumer := "export const values: string[] = [apiValue, authoredValue];\n"
						switch mode {
						case "explicit":
							types = `,"types":["api","local"]`
						case "source_directive":
							types = `,"types":[]`
							consumer = "/// <reference types=\"api\" />\n/// <reference types=\"local\" />\n" + consumer
						}
						writeFile(t, "app/tsconfig.json", `{"compilerOptions":{"strict":true,"typeRoots":["./types"]`+types+`},"files":["consumer.ts"],"include":[]}`)
						writeFile(t, "app/consumer.ts", consumer)
						const local = "app/types/local/index.d.ts"
						const authored = "declare const authoredValue: string;\n"
						writeFile(t, local, authored)
						sources := []string{"baseline.json", "app/tsconfig.json", "app/consumer.ts", local}
						if checkout == "sandbox_inputs" || checkout == "sandbox_aliases" {
							editorTestSymlinkInput(t, local)
						}
						const alias = "app/types/alias/index.d.ts"
						if checkout == "sandbox_aliases" {
							editorTestMkdir(t, filepath.Dir(alias))
							if err := os.Symlink(editorTestFileIdentity(t, local), alias); err != nil {
								t.Fatal(err)
							}
							sources = append(sources, alias)
						}
						writeFile(t, binDir+"/app/types/api/index.d.ts", "declare const apiValue: string;\n")
						if twin {
							writeFile(t, binDir+"/"+local, "declare const authoredValue: number;\ndeclare const generatedOnly: boolean;\n")
						}
						build := binDir + "/app/program.tsconfig.json"
						editor := "app/.bazel/tsconfig/project.json"
						editorTestMkdir(t, filepath.Dir(editor))
						mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "app/consumer.ts"))
						root := binDir + "/editor-program"
						a := actionConfig{project: "app/tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor, generatedDirectories: []string{"app/types"}}
						if err := a.layOutProgramRoot(root, sources, nil, nil, overlayPairs([]string{binDir + "/app"}...), nil); err != nil {
							t.Fatal(err)
						}
						t.Chdir(root)
						listing := editorTestListing(t, compiler, build)
						t.Chdir(cwd)
						entries := listing.Implicit
						if mode == "explicit" {
							entries = listing.Types
						}
						identity := func(file string) string {
							t.Helper()
							if !filepath.IsAbs(file) {
								file = filepath.Join(cwd, root, file)
							}
							real, err := filepath.EvalSymlinks(file)
							if err != nil {
								t.Fatal(err)
							}
							return real
						}
						want := identity(filepath.Join(cwd, local))
						selected := slices.ContainsFunc(entries, func(entry explainfiles.TypeEntry) bool {
							return entry.Entry == "local" && identity(entry.File) == want
						})
						if mode == "source_directive" {
							if len(listing.Types) != 0 || len(listing.Implicit) != 0 {
								t.Fatalf("types: [] admitted option-selected type packages: %+v", listing)
							}
							selected = slices.ContainsFunc(listing.Edges, func(edge explainfiles.Edge) bool {
								return edge.Kind == explainfiles.TypeReference && edge.Specifier == "local" && identity(edge.To) == want
							})
						}
						if !selected || slices.ContainsFunc(listing.Roots, func(file string) bool { return identity(file) == want }) {
							t.Fatalf("authored dependency must be selected only as a type package: %+v", listing)
						}
						if checkout == "sandbox_inputs" || checkout == "sandbox_aliases" {
							physicalSelected := slices.ContainsFunc(listing.Files, func(file string) bool {
								if !filepath.IsAbs(file) {
									file = filepath.Join(cwd, root, file)
								}
								return filepath.Clean(file) == want
							})
							if !physicalSelected {
								t.Fatalf("compiler did not report the physical sandbox input: %v", listing.Files)
							}
						}
						command := editorTestCommand(compiler, build)
						for _, existing := range []bool{false, true} {
							const previous = "previous editor project\n"
							if existing {
								writeFile(t, editor, previous)
							}
							err := editorRun(root, command, a, editor)
							if err == nil {
								t.Fatal("accepted a relocated typeRoot that hides an authored package")
							}
							required := []string{editor, "typeRoots", "app/types", local, "separate authored and generated type roots", "without generated_sources", "ordinary builds and type checking remain supported"}
							if checkout == "sandbox_aliases" {
								required = []string{editor, "ambiguous authored input identities", local, alias, "without generated_sources", "ordinary builds and type checking remain supported"}
							}
							for _, part := range required {
								if !strings.Contains(err.Error(), part) {
									t.Fatalf("mixed type root diagnostic omits %q: %v", part, err)
								}
							}
							got, err := os.ReadFile(editor)
							if existing {
								if err != nil || string(got) != previous {
									t.Fatalf("rejection replaced the installed project: %q, %v", got, err)
								}
							} else if !os.IsNotExist(err) {
								t.Fatalf("rejection created an editor project: %q, %v", got, err)
							}
						}
						if got, err := os.ReadFile(local); err != nil || string(got) != authored {
							t.Fatalf("refresh changed authored package: %q, %v", got, err)
						}
					})
				}
			}
		})
	}
}

func TestEditorEqualPrefixAliasOrderCannotReplaceNativeSelection(t *testing.T) {
	compiler := editorTestCompiler(t)
	installer := editorTestInstaller(t)
	for _, test := range []struct {
		name, paths, selected string
		codes                 []string
	}{
		{
			name: "suffix_first", paths: `"#x/*z":["../app/authored/*"],"#x/*":["../app/other/*"]`,
			selected: "app/authored/foo.ts",
		},
		{
			name: "wildcard_first", paths: `"#x/*":["../app/other/*"],"#x/*z":["../app/authored/*"]`,
			selected: "app/other/fooz.ts", codes: []string{"TS2322"},
		},
		{
			name: "disjoint_suffixes", paths: `"#x/*z":["../app/authored/*"],"#x/*q":["../app/other/*"]`,
			selected: "app/authored/foo.ts",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			editorTestChdir(t)
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "configs/base.json", `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler","paths":{`+test.paths+`,"#generated":["../`+binDir+`/gen/value.d.ts"]}}}`)
			writeFile(t, "app/tsconfig.json", `{"extends":"../configs/base.json","files":["consumer.ts"]}`)
			writeFile(t, "app/consumer.ts", "import { value } from '#x/fooz';\nimport type { Generated } from '#generated';\nexport const result: Generated = value;\n")
			writeFile(t, "app/authored/foo.ts", "export const value: string = 'authored';\n")
			writeFile(t, "app/other/fooz.ts", "export const value: number = 1;\n")
			writeFile(t, binDir+"/gen/value.d.ts", "export type Generated = string;\n")
			want := explainfiles.Edge{Kind: explainfiles.Import, From: "app/consumer.ts", Specifier: "#x/fooz", To: test.selected}
			check := func(project string) {
				t.Helper()
				listing := editorTestListing(t, compiler, project, test.codes...)
				if !slices.Contains(listing.Edges, want) || !slices.Contains(listing.Files, binDir+"/gen/value.d.ts") {
					t.Fatalf("%s lost native alias order or unrelated generated input: %+v", project, listing)
				}
			}
			check("app/tsconfig.json")
			build, editor := binDir+"/app/program.tsconfig.json", "app/.bazel/tsconfig/project.json"
			editorTestMkdir(t, filepath.Dir(build), filepath.Dir(editor))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, "app/tsconfig.json", build, "app/consumer.ts"))
			a := actionConfig{project: "app/tsconfig.json", baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor, generatedFiles: []string{"gen/value.d.ts"}}
			root := binDir + "/editor-program"
			sources := []string{"baseline.json", "configs/base.json", "app/tsconfig.json", "app/consumer.ts", "app/authored/foo.ts", "app/other/fooz.ts"}
			if err := a.layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			check(build)
			t.Chdir(cwd)
			if err := editorRun(root, editorTestCommand(compiler, build), a, editor); err != nil {
				t.Fatal(err)
			}
			check(editor)
			editorTestInstall(t, installer, editor)
			check(editor)
		})
	}
}

func TestEditorWildcardSuffixKeepsGeneratedTreeResolutionAndAliasPrecedence(t *testing.T) {
	compiler := editorTestCompiler(t)
	editorTestChdir(t)
	absolute, err := filepath.Abs("app/absolute")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, "baseline.json", `{}`)
	editorTestWriteJSON(t, "app/tsconfig.json", map[string]any{
		"compilerOptions": map[string]any{
			"strict": true, "module": "preserve", "moduleResolution": "bundler",
			"paths": map[string][]string{
				"#x/*":                    {filepath.ToSlash(absolute) + "/*.d.ts", "./authored/*.d.ts", "./src/*.d.ts"},
				"#x/generated/exact":      {"./overrides/exact.ts"},
				"#x/generated/specific/*": {"./overrides/*.ts"},
			},
		},
		"files": []string{"consumer.ts"},
	})
	writeFile(t, "app/consumer.ts", `
import { value as generated } from '#x/generated/value';
import { value as absolute } from '#x/generated/absolute';
import { value as fallback } from '#x/generated/fallback';
import { value as exact } from '#x/generated/exact';
import { value as specific } from '#x/generated/specific/value';
import { value as plain } from '#x/plain';
export const values: string[] = [generated, absolute, fallback, exact, specific, plain];
`)
	want := map[string]string{
		"#x/generated/value":          "bazel-bin/app/src/generated/value.d.ts",
		"#x/generated/absolute":       "app/absolute/generated/absolute.d.ts",
		"#x/generated/fallback":       "app/authored/generated/fallback.d.ts",
		"#x/generated/exact":          "app/overrides/exact.ts",
		"#x/generated/specific/value": "app/overrides/value.ts",
		"#x/plain":                    "app/src/plain.d.ts",
	}
	for _, file := range want {
		writeFile(t, file, "export declare const value: string;\n")
	}
	writeFile(t, "app/absolute/absolute.d.ts", "export declare const value: number;\n")
	for _, file := range []string{"fallback.d.ts", "exact.d.ts", "specific/value.d.ts"} {
		writeFile(t, "bazel-bin/app/src/generated/"+file, "export declare const value: number;\n")
	}
	build := "bazel-bin/app/program.tsconfig.json"
	editor := ".bazel/tsconfig/project.json"
	editorTestMkdir(t, filepath.Dir(editor))
	args := []string{
		"-tsgo=" + compiler, "-tsconfig=app/tsconfig.json", "-baseline=baseline.json",
		"-out=" + build, "-options=bazel-bin/app/options.json", "-bin_dir=bazel-bin",

		"app/consumer.ts",
	}
	mustWriteTsconfig(t, args)
	selected := func(project string) {
		t.Helper()
		listing := editorTestListing(t, compiler, project)
		actual := map[string]string{}
		for _, edge := range listing.Edges {
			if edge.Kind == explainfiles.Import && edge.From == "app/consumer.ts" {
				actual[edge.Specifier] = edge.To
			}
		}
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("%s selected %v, want %v", project, actual, want)
		}
		t.Logf("project=%s selected=%v", project, actual)
	}
	selected(build)
	command := editorTestCommand(compiler, build)
	if err := editorRun(".", command, actionConfig{project: "app/tsconfig.json", baseline: "baseline.json", out: build, binDir: "bazel-bin", editorPath: editor, generatedDirectories: []string{"app/src/generated"}}, editor); err != nil {
		t.Fatal(err)
	}
	selected(editor)
	writeFile(t, "app/src/generated/value.d.ts", "export declare const value: number;\n")
	selected(editor)
}

func TestEditorExternalGeneratedDependenciesCannotReplaceLocalProjectOrBlockCompilation(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, tree := range []bool{false, true} {
		name := "scalar"
		if tree {
			name = "tree"
		}
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			const declaration = "external/types+/generated/value.d.ts"
			const project = "tsconfig.build.json"
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, project, `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler","paths":{"#external":["./external/types+/generated/value"]}},"files":["app/consumer.ts"]}`)
			writeFile(t, "app/consumer.ts", "import { value } from '#external';\nexport const result: string = value;\n")
			writeFile(t, binDir+"/"+declaration, "export declare const value: string;\n")
			build, editor := binDir+"/program.tsconfig.json", ".bazel/tsconfig/project.json"
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, project, build, "app/consumer.ts"))
			want := explainfiles.Edge{Kind: explainfiles.Import, From: "app/consumer.ts", Specifier: "#external", To: binDir + "/" + declaration}
			if listing := editorTestListing(t, compiler, build); !slices.Contains(listing.Edges, want) {
				t.Fatalf("ordinary compiler lost the external generated dependency: %v", listing.Edges)
			}
			input := "../types+/generated/value.d.ts"
			a := actionConfig{project: project, baseline: "baseline.json", out: build, binDir: binDir, editorPath: editor}
			if tree {
				input = "../types+/generated"
				a.generatedDirectories = []string{input}
			} else {
				a.generatedFiles = []string{input}
			}
			editorTestMkdir(t, filepath.Dir(editor))
			for _, existing := range []bool{false, true} {
				const previous = "{\"files\":[],\"include\":[]}\n"
				if existing {
					writeFile(t, editor, previous)
				}
				err := editorRun(".", editorTestCommand(compiler, build), a, editor)
				if err == nil {
					t.Fatal("local generated editor project accepted an external generated dependency")
				}
				for _, required := range []string{editor, input, "external generated input", "authored editor project", "without generated_sources", "ordinary builds and type checking remain supported"} {
					if !strings.Contains(err.Error(), required) {
						t.Fatalf("external dependency diagnostic omits %q: %v", required, err)
					}
				}
				got, err := os.ReadFile(editor)
				if existing {
					if err != nil || string(got) != previous {
						t.Fatalf("rejection replaced the installed local project: %q, %v", got, err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("rejection created an editor project: %q, %v", got, err)
				}
			}
			if listing := editorTestListing(t, compiler, build); !slices.Contains(listing.Edges, want) {
				t.Fatalf("editor rejection changed ordinary compilation: %v", listing.Edges)
			}
		})
	}
}

func TestEditorExternalAuthoredDependenciesCannotReplaceLocalProjectOrBlockCompilation(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, file, contents string
		generated            bool
	}{
		{"declaration", "external/types+/value.d.ts", "export declare const value: string;\n", false},
		{"source_mode_with_local_generated", "external/types+/value.ts", "export const value: string = 'external';\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			const project = "tsconfig.build.json"
			const editor = ".bazel/tsconfig/project.json"
			checkout := t.TempDir()
			installed := filepath.Join(checkout, filepath.FromSlash(editor))
			paths := map[string][]string{"#external": {"./" + test.file}}
			consumer := "import { value } from '#external';\nexport const result: string = value;\n"
			if test.generated {
				paths["#generated"] = []string{"./gen/tree/value.d.ts"}
				consumer += "import { local } from '#generated';\nexport const current: string = local;\n"
				writeFile(t, binDir+"/gen/tree/value.d.ts", "export declare const local: string;\n")
			}
			writeFile(t, "baseline.json", `{}`)
			writeFile(t, "app/consumer.ts", consumer)
			writeFile(t, test.file, test.contents)
			editorTestWriteJSON(t, project, map[string]any{
				"compilerOptions": map[string]any{"strict": true, "module": "preserve", "moduleResolution": "bundler", "paths": paths},
				"files":           []string{"app/consumer.ts"},
			})
			build := binDir + "/program.tsconfig.json"
			editorTestMkdir(t, filepath.Dir(build))
			mustWriteTsconfig(t, editorTestConfigArgs(compiler, project, build, "app/consumer.ts"))
			want := explainfiles.Edge{Kind: explainfiles.Import, From: "app/consumer.ts", Specifier: "#external", To: test.file}
			if listing := editorTestListing(t, compiler, build); !slices.Contains(listing.Edges, want) {
				t.Fatalf("ordinary compiler lost the external authored dependency: %v", listing.Edges)
			}
			args := []string{"-root=" + binDir + "/editor-program"}
			for _, file := range []string{"baseline.json", project, "app/consumer.ts", test.file} {
				args = append(args, "-source="+file)
			}
			command := []string{"--", compiler, "-p", build, "--noEmit", "--skipDefaultLibCheck", "--pretty", "false"}
			buildArgs := append(slices.Clone(args), command...)
			args = append(args, "-tsconfig="+project, "-editor-config="+build, "-editor-baseline=baseline.json", "-editor-bin-dir="+binDir, "-editor-out="+installed, "-editor-path="+editor)
			if test.generated {
				args = append(args, "-editor-generated-directory=gen/tree")
			}
			args = append(args, append(command, "--explainFiles", "--traceResolution")...)
			editorTestMkdir(t, filepath.Dir(installed))
			writeFile(t, filepath.Join(checkout, "app/previous.ts"), "export const previous: string = 'valid';\n")
			for _, existing := range []bool{false, true} {
				const previous = "{\"files\":[\"../../app/previous.ts\"]}\n"
				if existing {
					writeFile(t, installed, previous)
				}
				err := runTsgo(args)
				if err == nil {
					t.Fatal("local generated editor project accepted an external authored dependency")
				}
				for _, required := range []string{editor, test.file, "external authored input", "authored editor project", "without generated_sources", "ordinary builds and type checking remain supported"} {
					if !strings.Contains(err.Error(), required) {
						t.Fatalf("external dependency diagnostic omits %q: %v", required, err)
					}
				}
				got, err := os.ReadFile(installed)
				if existing {
					if err != nil || string(got) != previous {
						t.Fatalf("rejection replaced the installed local project: %q, %v", got, err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("rejection created an editor project: %q, %v", got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(checkout, test.file)); !os.IsNotExist(err) {
				t.Fatalf("external authored input exists in checkout: %v", err)
			}
			if err := runTsgo(buildArgs); err != nil {
				t.Fatalf("editor rejection blocked ordinary compilation: %v", err)
			}
		})
	}
}

func TestEditorExternalConfigChainCannotReplaceProjectOrBlockCompilation(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, project, external string
		configs                 map[string]string
	}{
		{
			name: "external_leaf_must_not_install", project: "external/settings+/base.json", external: "external/settings+/base.json",
		},
		{
			name: "external_base_must_not_install", project: "tsconfig.build.json", external: "external/settings+/base.json",
			configs: map[string]string{"tsconfig.build.json": `{"extends":"./external/settings+/base.json","files":["app/consumer.ts"]}`},
		},
		{
			name: "external_grandparent_must_not_install", project: "tsconfig.build.json", external: "external/settings+/base.json",
			configs: map[string]string{
				"tsconfig.build.json": `{"extends":"./config/shared.json","files":["app/consumer.ts"]}`,
				"config/shared.json":  `{"extends":"../external/settings+/base.json"}`,
			},
		},
		{
			name: "overridden_external_branch_must_not_install", project: "tsconfig.build.json", external: "external/settings+/base.json",
			configs: map[string]string{
				"tsconfig.build.json": `{"extends":["./external/settings+/base.json","./config/shared.json"],"files":["app/consumer.ts"]}`,
				"config/shared.json":  `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler"}}`,
			},
		},
		{
			name: "authored_chain_must_keep_native_options", project: "tsconfig.build.json",
			configs: map[string]string{
				"tsconfig.build.json": `{"extends":"./config/shared.json","files":["app/consumer.ts"]}`,
				"config/shared.json":  `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler"}}`,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			build := "build/program.tsconfig.json"
			editor := ".bazel/tsconfig/project.json"
			checkout := t.TempDir()
			installed := filepath.Join(checkout, filepath.FromSlash(editor))
			if test.external != "" {
				writeFile(t, test.external, `{"compilerOptions":{"strict":true,"module":"preserve","moduleResolution":"bundler"},"files":["../../app/consumer.ts"]}`)
				if _, err := os.Stat(filepath.Join(checkout, test.external)); !os.IsNotExist(err) {
					t.Fatalf("external config exists in checkout: %v", err)
				}
			}
			files := map[string]string{"baseline.json": `{}`, "app/consumer.ts": "export const value: string = 'valid';\n"}
			for file, contents := range test.configs {
				files[file] = contents
			}
			for file, contents := range files {
				writeFile(t, file, contents)
				writeFile(t, filepath.Join(checkout, file), contents)
			}
			editorTestMkdir(t, filepath.Dir(build), filepath.Dir(installed))
			args := []string{
				"-tsgo=" + compiler, "-tsconfig=" + test.project, "-baseline=baseline.json",
				"-out=" + build, "-options=build/options.json", "-bin_dir=build",
				"app/consumer.ts",
			}
			if err := writeTsconfig(args); err != nil {
				t.Fatalf("ordinary TsConfig rejected the configuration chain: %v", err)
			}
			editorTestListing(t, compiler, build)
			command := []string{compiler, "--skipDefaultLibCheck", "-p", build, "--noEmit", "--explainFiles", "--pretty", "false", "--traceResolution"}
			for _, existing := range []bool{false, true} {
				const previous = "previous editor project\n"
				if existing {
					writeFile(t, installed, previous)
				}
				err := editorRun(".", command, actionConfig{project: test.project, baseline: "baseline.json", out: build, binDir: "build", editorPath: editor}, installed)
				if test.external == "" {
					if err != nil {
						t.Fatalf("authored configuration chain rejected: %v", err)
					}
					editorTestListing(t, compiler, installed)
					writeFile(t, filepath.Join(checkout, "app/consumer.ts"), "export const value: string = null;\n")
					editorTestListing(t, compiler, installed, "TS2322")
					writeFile(t, filepath.Join(checkout, "app/consumer.ts"), files["app/consumer.ts"])
					continue
				}
				if err == nil {
					t.Fatal("generated editor accepted an external config absent from the checkout")
				}
				for _, required := range []string{editor, test.project, test.external, "authored editor project", "without generated_sources", "ordinary builds and type checking remain supported"} {
					if !strings.Contains(err.Error(), required) {
						t.Fatalf("unsupported configuration diagnostic omits %q: %v", required, err)
					}
				}
				contents, err := os.ReadFile(installed)
				if existing {
					if err != nil || string(contents) != previous {
						t.Fatalf("rejected export replaced the existing editor project: %q, %v", contents, err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("rejected export created an editor project: %q, %v", contents, err)
				}
			}
		})
	}
}

func TestEditorConfigPlacementCannotChangePackageOutputMapping(t *testing.T) {
	compiler := editorTestCompiler(t)
	for _, test := range []struct {
		name, field, key, specifier string
		module, resolution          string
		output, equal, unobserved   bool
		packageOptions              map[string]bool
		inactive, unnamed, empty    bool
	}{
		{name: "imports_conflict", field: "imports", key: "#value", specifier: "#value", output: true},
		{name: "self_exports_conflict", field: "exports", key: "./value", specifier: "fixture/value", output: true},
		{name: "unobserved_import_conflict", field: "imports", key: "#value", specifier: "#value", output: true, unobserved: true},
		{name: "empty_imports_cannot_block_changed_context", field: "imports", key: "#value", specifier: "#value", output: true, inactive: true, empty: true},
		{name: "empty_named_exports_cannot_block_changed_context", field: "exports", key: "./value", specifier: "fixture/value", output: true, inactive: true, empty: true},
		{name: "unnamed_exports_allow_changed_context_by_default", field: "exports", key: "./value", output: true, unobserved: true, inactive: true, unnamed: true},
		{name: "disabled_imports_allow_changed_context_with_exports_enabled", field: "imports", key: "#value", output: true, unobserved: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": true}, inactive: true},
		{name: "disabled_imports_allow_explicit_bundler_context", field: "imports", key: "#value", resolution: "bundler", output: true, unobserved: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false}, inactive: true},
		{name: "disabled_imports_cannot_hide_explicit_node16_context", field: "imports", key: "#value", specifier: "#value", module: "node16", resolution: "node16", output: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false}},
		{name: "disabled_imports_cannot_hide_inferred_node16_context", field: "imports", key: "#value", specifier: "#value", module: "node16", output: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false}},
		{name: "disabled_imports_cannot_hide_explicit_nodenext_context", field: "imports", key: "#value", specifier: "#value", module: "nodenext", resolution: "nodenext", output: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false}},
		{name: "disabled_imports_cannot_hide_inferred_nodenext_context", field: "imports", key: "#value", specifier: "#value", module: "nodenext", output: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false}},
		{name: "disabled_exports_allow_changed_context_with_imports_enabled", field: "exports", key: "./value", output: true, unobserved: true, packageOptions: map[string]bool{"resolvePackageJsonImports": true, "resolvePackageJsonExports": false}, inactive: true, unnamed: true},
		{name: "disabled_maps_allow_imports_context_change", field: "imports", key: "#value", output: true, unobserved: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": false}, inactive: true},
		{name: "disabled_maps_allow_exports_context_change", field: "exports", key: "./value", output: true, unobserved: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": false}, inactive: true, unnamed: true},
		{name: "enabled_imports_still_reject_context_with_exports_disabled", field: "imports", key: "#value", output: true, unobserved: true, packageOptions: map[string]bool{"resolvePackageJsonImports": true, "resolvePackageJsonExports": false}},
		{name: "unnamed_exports_allow_changed_context_when_enabled", field: "exports", key: "./value", output: true, unobserved: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": true}, inactive: true, unnamed: true},
		{name: "disabled_exports_cannot_hide_self_reference_context", field: "exports", key: "./value", specifier: "fixture/value", output: true, packageOptions: map[string]bool{"resolvePackageJsonImports": false, "resolvePackageJsonExports": false}},
		{name: "imports_without_output_context", field: "imports", key: "#value", specifier: "#value"},
		{name: "self_exports_without_output_context", field: "exports", key: "./value", specifier: "fixture/value"},
		{name: "imports_equal_context", field: "imports", key: "#value", specifier: "#value", output: true, equal: true},
		{name: "self_exports_equal_context", field: "exports", key: "./value", specifier: "fixture/value", output: true, equal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scratch, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			workspace := filepath.Join(scratch, "workspace")
			output := filepath.Join(scratch, "output")
			editorTestMkdir(t, workspace, output)
			t.Chdir(workspace)
			if err := os.Symlink(output, "bazel-out"); err != nil {
				t.Fatal(err)
			}
			baseline := binDir + "/app/program.tsconfig_baseline.json"
			build := binDir + "/app/program.tsconfig.json"
			editor := "app/.bazel/tsconfig/program.json"
			root := binDir + "/app/program.ide-program"
			writeFile(t, baseline, `{"compilerOptions":{"strict":true,"module":"preserve","target":"es2022","skipLibCheck":true}}`)
			writeFile(t, "value.ts", "export const value: 'source' = 'source';\n")
			writeFile(t, "app/dist/value.d.ts", "export declare const value: 'declaration';\n")
			writeFile(t, binDir+"/gen/tree/index.d.ts", "export declare const generated: 'generated';\n")
			writeFile(t, "gen/package.json", `{"type":"module"}`)
			writeFile(t, binDir+"/gen/package.json", `{"type":"module"}`)
			manifest, target, selected, value := "app/package.json", "./dist/value.js", "app/dist/value.d.ts", "declaration"
			if test.equal {
				manifest, target, selected, value = "package.json", "./app/dist/value.js", "value.ts", "source"
			}
			metadata := map[string]any{"name": "fixture", "type": "module", test.field: map[string]string{test.key: target}}
			if test.empty {
				metadata[test.field] = map[string]string{}
			}
			if test.unnamed {
				delete(metadata, "name")
			}
			editorTestWriteJSON(t, manifest, metadata)
			consumer := "import { generated } from '#generated';\nexport const other: 'generated' = generated;\n"
			if !test.unobserved {
				consumer += "import { value } from '" + test.specifier + "';\nexport const selected: '" + value + "' = value;\n"
			}
			writeFile(t, "app/consumer.ts", consumer)
			options := map[string]any{"paths": map[string][]string{"#generated": {"../gen/tree/index.d.ts"}}}
			if test.module != "" {
				options["module"] = test.module
			}
			if test.resolution != "" {
				options["moduleResolution"] = test.resolution
			}
			for option, enabled := range test.packageOptions {
				options[option] = enabled
			}
			if test.output {
				options["outDir"] = "./dist"
			}
			editorTestWriteJSON(t, "app/tsconfig.json", map[string]any{"compilerOptions": options, "files": []string{"consumer.ts", "dist/value.d.ts", "../value.ts"}})
			mustWriteTsconfig(t, []string{"-tsgo=" + compiler, "-tsconfig=app/tsconfig.json", "-baseline=" + baseline, "-out=" + build, "-options=" + binDir + "/app/program.options.json", "-bin_dir=" + binDir, "-source_only", "app/consumer.ts", "app/dist/value.d.ts", "value.ts"})
			sources := []string{"app/tsconfig.json", manifest, "app/consumer.ts", "app/dist/value.d.ts", "value.ts", "gen/package.json"}
			if err := layOutProgramRoot(root, sources, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			check := func(project string) {
				t.Helper()
				codes := []string{}
				if test.empty {
					codes = append(codes, "TS2307")
				}
				listing := editorTestListing(t, compiler, project, codes...)
				if test.empty {
					if !strings.Contains(listing.Diagnostics[0], "'"+test.specifier+"'") || slices.ContainsFunc(listing.Edges, func(edge explainfiles.Edge) bool { return edge.Specifier == test.specifier }) {
						t.Fatalf("%s changed failed empty-map query %s: %+v", project, test.specifier, listing)
					}
					if !slices.Contains(listing.Edges, explainfiles.Edge{From: "app/consumer.ts", Specifier: "#generated", To: binDir + "/gen/tree/index.d.ts"}) {
						t.Fatalf("%s changed generated definition identity: %v", project, listing.Edges)
					}
				}
				if !test.unobserved && !test.empty && !slices.Contains(listing.Edges, explainfiles.Edge{From: "app/consumer.ts", Specifier: test.specifier, To: selected}) {
					t.Fatalf("%s lost compiler-selected authored identity %s: %v", project, selected, listing.Edges)
				}
				if !slices.Contains(listing.Files, binDir+"/gen/tree/index.d.ts") {
					t.Fatalf("%s lost generated declaration membership: %v", project, listing.Files)
				}
			}
			t.Chdir(root)
			check(build)
			t.Chdir(workspace)
			const previous = "previous installed project\n"
			writeFile(t, editor, previous)
			args := []string{"-root=" + root, "-tsconfig=app/tsconfig.json", "-editor-config=" + build, "-editor-baseline=" + baseline, "-editor-bin-dir=" + binDir, "-editor-out=" + editor, "-editor-path=" + editor, "-editor-generated-directory=gen/tree"}
			for _, source := range sources {
				args = append(args, "-source="+source)
			}
			args = append(args, "--")
			args = append(args, editorTestCommand(compiler, build)...)
			err = runTsgo(args)
			if test.output && !test.equal && !test.inactive {
				if err == nil || !strings.Contains(err.Error(), "config placement changes package scope") {
					t.Fatalf("accepted an incompatible package/config context: %v", err)
				}
				if actual, err := os.ReadFile(editor); err != nil || string(actual) != previous {
					t.Fatalf("context conflict replaced the previous editor project: %q, %v", actual, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			projected := readJSON(t, editor)
			if facts, ok := projected["rootDirSources"].([]any); !ok || len(facts) != 0 {
				t.Fatalf("declaration-only inputs require no rootDir widening: %v", projected["rootDirSources"])
			}
			if got := projected["compilerOptions"].(map[string]any)["rootDir"]; got != "../../.." {
				t.Fatalf("declaration-only inputs changed rootDir: %v", got)
			}
			check(editor)
		})
	}
}

func overlayPairs(dirs ...string) []string {
	pairs := make([]string, len(dirs))
	for i, dir := range dirs {
		pairs[i] = overlayFlag(dir)[len("-overlay="):]
	}
	return pairs
}

func overlayFlag(dir string) string {
	data, err := json.Marshal([]string{dir, binRelative(dir)})
	if err != nil {
		panic(err)
	}
	return "-overlay=" + string(data)
}

func canonicalDir(dir string) string {
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return canonical
}
