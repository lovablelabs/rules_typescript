package typescript

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/rule"
)

func fixtureManifest(root, dir string) *manifest {
	c := emptyConfig()
	c.RepoRoot = root
	return getConfig(c).programs.nearestManifest(c, dir)
}

func TestManifest_Read(t *testing.T) {
	root, _ := npmRepo(t)
	writeFile(t, filepath.Join(root, "workers/download/package.json"), `{
		"name": "download",
		"dependencies": {"zod": "^3.0.0"},
		"devDependencies": {"wrangler": "4.118.0", "typescript": "5.9.2",
			"zod": "^3.0.0"}
	}`)
	m := readManifest(root, "workers/download")
	if m == nil || m.name != "download" || m.dir != "workers/download" {
		t.Fatalf("readManifest = %+v", m)
	}
	if want := []string{"typescript", "wrangler", "zod"}; !slices.Equal(m.deps,
		want) {
		t.Errorf("deps = %v, want %v", m.deps, want)
	}
	if m := readManifest(root, "workers/download/test"); m != nil {
		t.Errorf("a directory without package.json read %+v", m)
	}
	writeFile(t, filepath.Join(root, "broken/package.json"), `{"name": `)
	if m := readManifest(root, "broken"); m != nil {
		t.Errorf("a malformed package.json read %+v", m)
	}
}

// The manifest a package reads is the nearest one at or above its directory.
func TestManifest_Nearest(t *testing.T) {
	root, _ := npmRepo(t)
	for dir, want := range map[string]string{
		"workers/download/test": "workers/download",
		"workers/download":      "workers/download",
		"packages/lib/src/wire": "packages/lib",
		"packages/lib/example":  "packages/lib/example",
		"scripts":               "",
	} {
		m := fixtureManifest(root, dir)
		if m == nil || m.dir != want {
			t.Errorf("fixtureManifest(%q) = %+v, want dir %q", dir, m, want)
		}
	}
	if m := fixtureManifest(t.TempDir(), "a/b"); m != nil {
		t.Errorf("a tree without any package.json read %+v", m)
	}
}

func TestPackageScopeDoesNotReadGeneratedCheckoutCopies(t *testing.T) {
	c := emptyConfig()
	c.RepoRoot = t.TempDir()
	s := getConfig(c).programs
	ix := buildIndex(t, c, indexedRule{kind: "genrule", name: "manifest", pkg: "foreign", outs: []string{"package.json"}})
	for _, state := range []string{"absent", "excluded stale", "visible stale"} {
		t.Run(state, func(t *testing.T) {
			var files []string
			if state != "absent" {
				writeFile(t, filepath.Join(c.RepoRoot, "foreign/package.json"), "not valid JSON")
			}
			if state == "visible stale" {
				files = []string{"package.json"}
			}
			s.visit("foreign", files)
			if scope := s.packageScope(c, "foreign/nested"); scope != "foreign/package.json" {
				t.Fatalf("generated scope = %q, want foreign/package.json", scope)
			}
			if identity := s.requireInput(c, ix, "foreign/package.json", "consumer"); identity != generatedInput {
				t.Fatalf("generated input identity = %v, want generatedInput", identity)
			}
			producer, pkg := s.outputProducer("foreign/package.json")
			if producer == nil || producer.Name() != "manifest" || pkg != "foreign" {
				t.Fatalf("generated scope lost its producer: %v in %q", producer, pkg)
			}
		})
	}
	if got := s.inputIdentity(c, ix, "foreign/excluded.ts"); got != unavailableInput {
		t.Fatalf("excluded authored input identity = %v, want unavailableInput", got)
	}
}

func TestNewPackageCannotInvalidateAncestorDeclaredTargets(t *testing.T) {
	for _, test := range []struct {
		name, build, target, producer, output string
	}{
		{"scalar output", `genrule(name = "generated", outs = ["nested/gen.ts"], cmd = "unused")`, "//:nested/gen.ts", "//:generated", "nested/gen.ts"},
		{"declared rule", `filegroup(name = "nested/forward", srcs = [])`, "//:nested/forward", "//:nested/forward", ""},
	} {
		for _, update := range []struct {
			name string
			args []string
		}{{"recursive", nil}, {"child only", []string{"-r=false", "nested"}}} {
			t.Run(test.name+"/"+update.name, func(t *testing.T) {
				root := writeTree(t, map[string]string{
					"MODULE.bazel":         "module(name = \"ancestor_declared_targets\")\n",
					"BUILD.bazel":          test.build + "\n",
					"nested/tsconfig.json": `{"compilerOptions":{"module":"preserve"},"files":["index.ts"]}`,
					"nested/index.ts":      "export const authored = true;\n",
				})
				before := buildFileBytes(t, root)
				states := []string{"cold"}
				if test.output != "" {
					states = append(states, "materialized")
				}
				for _, state := range states {
					if state == "materialized" {
						writeFile(t, filepath.Join(root, test.output), "export const generated = true;\n")
					}
					output, err := protoGazelle(t, root, update.args...)
					if err == nil {
						t.Fatalf("%s created a package across an ancestor target:\n%s", state, output)
					}
					for _, diagnostic := range []string{"cannot create package nested", test.target, test.producer, "Did you mean", "move the declaring rule explicitly"} {
						if !strings.Contains(output, diagnostic) {
							t.Fatalf("%s refusal did not identify %q: %v\n%s", state, diagnostic, err, output)
						}
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("%s refusal changed BUILD files: %s", state, diff)
					}
				}
			})
		}
	}
}

func TestImporterCannotCreatePackageAcrossOpaqueAncestorOutputs(t *testing.T) {
	for _, pkg := range []string{"nested", "parent/child"} {
		t.Run(pkg, func(t *testing.T) {
			files := map[string]string{
				"MODULE.bazel": "module(name = \"opaque_ancestor_outputs\")\n",
				"BUILD.bazel": `OUTPUTS = ["nested/gen.ts"]
genrule(name = "generated", outs = OUTPUTS, cmd = "unused")
`,
				pnpmLockfileName:             "lockfileVersion: '9.0'\nimporters:\n  .: {}\n  " + pkg + ": {}\n",
				"node_modules/.modules.yaml": "layoutVersion: 5\n",
				pkg + "/package.json":        `{}`,
			}
			if pkg == "parent/child" {
				files["parent/BUILD.bazel"] = ""
			}
			root := writeTree(t, files)
			before := buildFileBytes(t, root)
			output, err := protoGazelle(t, root)
			if pkg == "parent/child" {
				if err != nil {
					t.Fatalf("outputs beyond an existing package blocked a new child: %v\n%s", err, output)
				}
				onDiskRule(t, root, pkg, "node_modules", "node_modules")
				return
			}
			if err == nil || !strings.Contains(output, "//:generated has nonliteral outs") || !strings.Contains(output, "Did you mean to use literal output declarations") {
				t.Fatalf("opaque ancestor outputs did not refuse the importer boundary: %v\n%s", err, output)
			}
			if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
				t.Fatalf("refused importer boundary changed BUILD files: %s", diff)
			}
		})
	}
}

// A ts_test's runtime deps carry the manifest's dependencies and
// devDependencies, lockfile-gated, in D7 spelling; a linked member is its view.
func TestManifestLabels_Union(t *testing.T) {
	root, l := npmRepo(t)
	writeFile(t, filepath.Join(root, "workers/download/package.json"), `{
		"name": "download",
		"dependencies": {"@acme/lib": "workspace:*", "left-pad": "1.3.0"},
		"devDependencies": {"wrangler": "4.118.0", "typescript": "5.9.2"}
	}`)
	var got []string
	out := captureLog(t, func() {
		got = l.manifestLabels(fixtureManifest(root, "workers/download/test"),
			"workers/download/test")
	})
	want := []string{
		"//:node_modules/@acme/lib",
		"@npm//workers/download:typescript",
		"@npm//workers/download:wrangler",
	}
	if !slices.Equal(got, want) {
		t.Errorf("manifestLabels = %v, want %v", got, want)
	}
	if !strings.Contains(out, "workers/download/package.json") ||
		!strings.Contains(out, "left-pad") {
		t.Errorf("log %q does not name the manifest and left-pad", out)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("%d log lines, want 1: %q", n, out)
	}
}

// The root's manifest names take the flat label; a package under no manifest
// has no union.
func TestManifestLabels_RootAndNone(t *testing.T) {
	root, l := npmRepo(t)
	writeFile(t, filepath.Join(root, "package.json"), `{
		"name": "monorepo",
		"devDependencies": {"typescript": "5.9.2", "vite": "8.2.2"}
	}`)
	got := l.manifestLabels(fixtureManifest(root, "scripts"), "scripts")
	if want := []string{"@npm//:typescript", "@npm//:vite"}; !slices.Equal(got,
		want) {
		t.Errorf("manifestLabels = %v, want %v", got, want)
	}
	if got := l.manifestLabels(nil, ""); got != nil {
		t.Errorf("manifestLabels(nil) = %v, want nil", got)
	}
}

// The root manifest read from a package below it: the member's link target is
// spelled from the test's package, not the manifest's, so it stays the root's.
func TestManifestLabels_RootManifestFromBelow(t *testing.T) {
	root, l := npmRepo(t)
	writeFile(t, filepath.Join(root, "package.json"),
		`{"name": "monorepo", "dependencies": {"@acme/lib": "workspace:*"}}`)
	m := fixtureManifest(root, "worker/test/deep")
	if got := l.manifestLabels(m, "worker/test/deep"); !slices.Equal(got,
		[]string{"//:node_modules/@acme/lib"}) {
		t.Errorf("from worker/test/deep: %v, want the root's link target", got)
	}
	if got := l.manifestLabels(m, ""); !slices.Equal(got,
		[]string{":node_modules/@acme/lib"}) {
		t.Errorf("from the root: %v, want the link target in the package", got)
	}
}

func TestManifestProgramDoesNotDropExportedSourcesOrImplicitTypes(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"manifest_implicit_types\")\n",
		"package.json": `{"name":"workspace","devDependencies":{"@types/ambient":"1.0.0"}}`,
		pnpmLockfileName: `lockfileVersion: '9.0'
importers:
  .:
    devDependencies:
      '@types/ambient':
        specifier: 1.0.0
        version: 1.0.0
  lib: {}
packages:
  '@types/ambient@1.0.0': {}
snapshots:
  '@types/ambient@1.0.0': {}
`,
		"node_modules/.modules.yaml":               "layoutVersion: 5\n",
		"node_modules/@types/ambient/package.json": `{"name":"@types/ambient","version":"1.0.0","types":"index.d.ts"}`,
		"node_modules/@types/ambient/index.d.ts":   "declare const MANIFEST_VALUE: number;\n",
		"lib/package.json":                         `{"name":"@test/lib","types":"./types/index.d.ts","exports":{".":{"types":"./types/index.d.ts","import":"./src/index.mjs"},"./test":"./src/value.test.ts","./styles.css":"./generated/styles.css"}}`,
		"lib/types/index.d.ts":                     `export declare const value: typeof MANIFEST_VALUE;`,
		"lib/src/index.mjs":                        `export { value } from "./value.mjs";`,
		"lib/src/value.mjs":                        `export const value = 42;`,
		"lib/src/value.test.ts":                    `export const actual: typeof MANIFEST_VALUE = MANIFEST_VALUE;`,
		"lib/generated/styles.css":                 `body { color: red; }`,
	})
	var first map[string]string
	for _, args := range [][]string{nil, {"-index=false"}, nil} {
		output, err := protoGazelle(t, root, args...)
		if err != nil {
			t.Fatalf("manifest program with %v: %v\n%s", args, err, output)
		}
		compile := onDiskRule(t, root, "lib", "ts_compile", "lib")
		wantLabels(t, "manifest compile retains exported sources and resources", compile.AttrStrings("srcs"), []string{"types/index.d.ts", "src/index.mjs", "src/value.mjs", "generated/styles.css"})
		wantLabels(t, "manifest compile retains its package scope", compile.AttrStrings("package_scopes"), []string{"package.json"})
		test := onDiskRule(t, root, "lib", "ts_test", "lib_test")
		wantStrings(t, "manifest compile retains implicit types", compile.AttrStrings("deps"), []string{"@npm//:types_ambient"})
		wantLabels(t, "manifest test retains implicit types", test.AttrStrings("deps"), []string{":lib", "@npm//:types_ambient"})
		if compile.AttrString("tsconfig") != "" || test.AttrString("tsconfig") != "" || ruleNamed(loadRules(t, root, "lib"), "ts_config", "tsconfig") != nil {
			t.Fatal("manifest source package invented a tsconfig")
		}
		if first == nil {
			first = convergeSnapshot(t, root)
		} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
			t.Fatalf("manifest program did not converge: %s", diff)
		}
	}
}

func TestNpmMembersRejectGeneratedManifestNames(t *testing.T) {
	for name, build := range map[string]string{
		"generated": `genrule(name = "manifest", outs = ["package.json"], cmd = "echo '{}' > $@")`,
		"unknown":   "OUTPUTS = [\"package.json\"]\ngenrule(name = \"manifest\", outs = OUTPUTS, cmd = \"echo '{}' > $@\")",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"package.json":          `{"name":"workspace"}`,
				"authored/package.json": `{"name":"authored"}`,
				"linked/package.json":   `{"name":"stale-linked-name"}`,
				pnpmLockfileName: `lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      linked:
        specifier: workspace:*
        version: link:linked
  authored: {}
  inherited: {}
  linked: {}
  member: {}
`,
			})
			for _, state := range []string{"absent", "materialized", "removed"} {
				if state == "materialized" {
					writeFile(t, filepath.Join(root, "member/package.json"), `{"name":"ghost"}`)
				} else if state == "removed" {
					if err := os.Remove(filepath.Join(root, "member/package.json")); err != nil {
						t.Fatal(err)
					}
				}
				c := emptyConfig()
				c.RepoRoot = root
				for _, dir := range []string{"member", "linked"} {
					f, err := rule.LoadData("BUILD.bazel", dir, []byte(build))
					if err != nil {
						t.Fatal(err)
					}
					getConfig(c).programs.recordBuild(c, dir, f)
				}
				configureTsConfig(c, "", nil, nil)
				if got, want := getConfig(c).lock.members, map[string]string{"authored": "authored", "linked": "linked"}; !maps.Equal(got, want) {
					t.Errorf("%s members = %v, want %v", state, got, want)
				}
			}
		})
	}
}

func TestNpmMemberNamesCannotDropCompilerResolvedSources(t *testing.T) {
	for name, build := range map[string]string{
		"generated": `genrule(name = "manifest", outs = ["package.json"], cmd = "echo '{}' > $@")`,
		"unknown":   "OUTPUTS = [\"package.json\"]\ngenrule(name = \"manifest\", outs = OUTPUTS, cmd = \"echo '{}' > $@\")",
		"authored":  "",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel":               "module(name = \"member_source_alias\")\n",
				"package.json":               `{"name":"workspace","type":"module"}`,
				"BUILD.bazel":                `exports_files(["package.json"], visibility = ["//visibility:public"])`,
				pnpmLockfileName:             "lockfileVersion: '9.0'\nimporters:\n  .: {}\n  app: {}\n  member: {}\n",
				"node_modules/.modules.yaml": "layoutVersion: 5\n",
				"member/BUILD.bazel":         build,
				"app/tsconfig.json":          `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"ghost":["../shared/value.ts"]}},"files":["index.ts"]}`,
				"app/index.ts":               `export { value } from "ghost";`,
				"shared/value.ts":            `export { value } from "./helper";`,
				"shared/helper.ts":           `export const value = 1;`,
				"shared/BUILD.bazel":         `exports_files(["value.ts", "helper.ts"], visibility = ["//visibility:public"])`,
			})
			var first map[string]string
			for _, state := range []string{"absent", "materialized", "removed"} {
				var args []string
				if state == "materialized" {
					writeFile(t, filepath.Join(root, "member/package.json"), `{"name":"ghost"}`)
					args = []string{"-index=false"}
				} else if state == "removed" {
					if err := os.Remove(filepath.Join(root, "member/package.json")); err != nil {
						t.Fatal(err)
					}
				}
				output, err := protoGazelle(t, root, args...)
				if err != nil {
					t.Fatalf("%s generation: %v\n%s", state, err, output)
				}
				if name != "authored" && strings.Contains(output, `workspace member "ghost"`) {
					t.Fatalf("%s admitted generated metadata as a member: %s", state, output)
				}
				compile := onDiskRule(t, root, "app", "ts_compile", "app")
				wantLabels(t, state+" source closure", compile.AttrStrings("srcs"), []string{"index.ts", "//shared:value.ts", "//shared:helper.ts"})
				wantLabels(t, state+" dependency closure", compile.AttrStrings("deps"), nil)
				wantLabels(t, state+" package scope", compile.AttrStrings("package_scopes"), []string{"//:package.json"})
				if first == nil {
					first = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("%s changed retained ownership: %s", state, diff)
				}
			}
		})
	}
}

func TestGeneratedPackageMovesExportToCanonicalChildWithoutAlias(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, map[string]string{
		"package.json":         convergePlainPkg,
		"parent/tsconfig.json": `{"include":["*.ts"]}`,
		"parent/index.ts":      `export const value=1;`,
		"parent/BUILD.bazel": `exports_files(["kept.json", "test/fixtures/value.json"], visibility=["//consumer:__pkg__"])
exports_files(["test/fixtures/public.json"], licenses=["notice"])`,
		"parent/kept.json":                 `{}`,
		"parent/test/tsconfig.json":        `{"include":["*.ts"]}`,
		"parent/test/value.ts":             `export const value=2;`,
		"parent/test/fixtures/value.json":  `{}`,
		"parent/test/fixtures/public.json": `{}`,
	})
	captureLog(t, func() { convergeGazelle(t, root) })
	parent := buildFileText(t, root, "parent")
	child := buildFileText(t, root, "parent/test")
	if strings.Contains(parent, "test/fixtures/value.json") || !strings.Contains(parent, "kept.json") {
		t.Fatalf("parent export ownership stale:\n%s", parent)
	}
	if !strings.Contains(child, `"fixtures/value.json"`) || !strings.Contains(child, `visibility = ["//consumer:__pkg__"]`) {
		t.Fatalf("missing canonical child export/visibility:\n%s", child)
	}
	file, err := rule.LoadFile(filepath.Join(root, "parent/test/BUILD.bazel"), "parent/test")
	if err != nil {
		t.Fatal(err)
	}
	public := false
	for _, r := range file.Rules {
		if r.Kind() == "exports_files" && slices.Contains(r.AttrStrings("licenses"), "notice") {
			public = true
			if r.Attr("visibility") != nil {
				t.Fatal("default-public export visibility narrowed")
			}
		}
	}
	if !public {
		t.Fatal("export licenses lost")
	}
	captureLog(t, func() { convergeGazelle(t, root) })
	if next := buildFileText(t, root, "parent/test"); next != child {
		t.Fatalf("export relocation unstable:\n%s", lineDiff(child, next))
	}
}

func TestMetadataAdmissionPrecedesFilteredFilesAndIgnoresGeneratedContents(t *testing.T) {
	c := emptyConfig()
	c.RepoRoot = t.TempDir()
	s := getConfig(c).programs
	f, err := rule.LoadData("BUILD.bazel", "app", []byte(`genrule(name = "manifest", outs = ["package.json"], cmd = "echo '{}' > $@")`))
	if err != nil {
		t.Fatal(err)
	}
	s.recordBuild(c, "app", f)
	writeFile(t, filepath.Join(c.RepoRoot, "package.json"), `{"name":"ancestor","dependencies":{"ancestor":"1"}}`)
	writeFile(t, filepath.Join(c.RepoRoot, "app/tsconfig.json"), `{"files":["index.ts"]}`)
	if got := s.metadataIdentity(c, "app/tsconfig.json"); got != authoredInput {
		t.Fatalf("authored discovery before the filtered walk = %v", got)
	}
	if got := s.inputIdentity(c, nil, "app/tsconfig.json"); got != unavailableInput {
		t.Fatalf("metadata discovery granted source eligibility: %v", got)
	}
	for _, state := range []string{"cold", "excluded stale"} {
		if state != "cold" {
			writeFile(t, filepath.Join(c.RepoRoot, "app/package.json"), `{"name":"app","exports":"./index.js","dependencies":{"stale":"1"}}`)
		}
		s.recordEmissionConsumers(c, "app")
		if len(s.emission.outputs) != 0 {
			t.Fatalf("%s generated manifest selected emission: %v", state, s.emission.outputs)
		}
		if got := s.nearestManifest(c, "app/nested"); got != nil {
			t.Fatalf("%s generated manifest selected npm metadata: %+v", state, got)
		}
		if got := s.packageScope(c, "app/nested"); got != "app/package.json" {
			t.Fatalf("%s generated scope lost declared identity: %q", state, got)
		}
	}
}

func TestIncompleteOutputObservationCannotBecomeAuthoredMetadata(t *testing.T) {
	for _, declaration := range []string{
		`genrule(name = "computed", outs = OUTPUTS)`,
		`genrule(name = "computed", outs = ["other.ts"] + OUTPUTS)`,
		`genrule(name = "computed", outs = [OUTPUT])`,
		`write_file(name = "computed", out = OUTPUT)`,
		`ts_codegen(name = "computed", out_dir = DIRECTORY)`,
	} {
		t.Run(declaration, func(t *testing.T) {
			c := emptyConfig()
			c.RepoRoot = t.TempDir()
			s := getConfig(c).programs
			f, err := rule.LoadData("app/BUILD.bazel", "app", []byte(declaration+`
genrule(name = "known", outs = ["known.json"])`))
			if err != nil {
				t.Fatal(err)
			}
			s.recordBuild(c, "app", f)
			writeFile(t, filepath.Join(c.RepoRoot, "package.json"), `{"name":"ancestor"}`)
			for _, stale := range []bool{false, true} {
				if stale {
					writeFile(t, filepath.Join(c.RepoRoot, "app/package.json"), `{"name":"stale","exports":"./index.js"}`)
				}
				if got := s.metadataIdentity(c, "app/package.json"); got != unknownInput {
					t.Fatalf("stale=%t metadata identity = %v, want unknown", stale, got)
				}
				if got := s.inputIdentity(c, nil, "app/package.json"); got != unknownInput {
					t.Fatalf("stale=%t input identity = %v, want unknown", stale, got)
				}
				if got := s.metadataIdentity(c, "app/known.json"); got != generatedInput {
					t.Fatalf("known producer identity = %v, want generated", got)
				}
				if s.nearestManifest(c, "app") != nil || s.packageScope(c, "app") != "app/package.json" {
					t.Fatal("unknown scope read checkout bytes or fell through to ancestor metadata")
				}
				s.recordEmissionConsumers(c, "app")
				if len(s.emission.outputs) != 0 {
					t.Fatal("unknown metadata selected emission")
				}
			}
			s.recordBuild(c, "app/independent", rule.EmptyFile("app/independent/BUILD.bazel", "app/independent"))
			writeFile(t, filepath.Join(c.RepoRoot, "app/independent/package.json"), `{"name":"independent"}`)
			if got := s.metadataIdentity(c, "app/independent/package.json"); got != authoredInput {
				t.Fatalf("unrelated Bazel subpackage inherited unknown provenance: %v", got)
			}
		})
	}
}
