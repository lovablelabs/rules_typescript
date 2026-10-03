package typescript

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"
	bzl "github.com/bazelbuild/buildtools/build"
)

func TestMain(m *testing.M) {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Setenv("TMPDIR", tmp); err != nil {
		log.Fatal(err)
	}
	os.Exit(m.Run())
}

// Under Bazel the toolchain's tsgo is in the runfiles; a plain go test sets
// TSGO or skips what lists programs.
func requireTsgo(t *testing.T) {
	t.Helper()
	if _, err := newProgramStore().binary(); err != nil {
		t.Skipf("no tsgo binary: %v", err)
	}
}

// writeTree writes a repository-relative tree under a fresh repo root.
func writeTree(t *testing.T, tree map[string]string) string {
	t.Helper()
	root := t.TempDir()
	writeWorkspace(t, root, tree)
	return root
}

type generatedTree struct {
	results map[string]language.GenerateResult
	logged  string
}

func generateAll(t *testing.T, root string,
	opts ...func(*config.Config)) generatedTree {
	t.Helper()
	requireTsgo(t)
	out := generatedTree{results: map[string]language.GenerateResult{}}
	c := &config.Config{RepoRoot: root, WorkDir: root, Strict: true, Exts: map[string]any{}}
	lang := &tsLang{}
	cexts := []config.Configurer{&walk.Configurer{}, &resolve.Configurer{}, lang}
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	for _, ext := range cexts {
		ext.RegisterFlags(flags, "update", c)
	}
	for _, opt := range opts {
		opt(c)
	}
	for _, ext := range cexts {
		if err := ext.CheckFlags(flags, c); err != nil {
			t.Fatal(err)
		}
	}
	out.logged = captureLog(t, func() {
		lang.Before(context.Background())
		err := walk.Walk2(c, cexts, []string{root}, walk.VisitAllUpdateSubdirsMode, func(args walk.Walk2FuncArgs) walk.Walk2FuncResult {
			if args.Update {
				out.results[args.Rel] = generateRules(language.GenerateArgs{
					Config: args.Config, Dir: args.Dir, Rel: args.Rel, File: args.File,
					Subdirs: args.Subdirs, RegularFiles: args.RegularFiles, GenFiles: args.GenFiles,
				})
			}
			return walk.Walk2FuncResult{}
		})
		lang.DoneGeneratingRules()
		if err != nil {
			t.Fatal(err)
		}
	})
	return out
}

// verbose is -ts_verbose: the run's store says what it lists and leaves out.
func verbose(c *config.Config) {
	tc := defaultTsConfig()
	tc.programs.verbose = true
	c.Exts[languageName] = tc
}

func generatedRule(res language.GenerateResult, name string) *rule.Rule {
	for _, r := range res.Gen {
		if r.Name() == name {
			return r
		}
	}
	return nil
}

// mustRule is the generated rule of that kind and name.
func mustRule(t *testing.T, res language.GenerateResult, kind, name string,
) *rule.Rule {
	t.Helper()
	r := generatedRule(res, name)
	if r == nil || r.Kind() != kind {
		t.Fatalf("no %s %s; generated %v", kind, name, generatedNames(t, res))
	}
	return r
}

// generatedNames maps rule name to kind for every generated rule, failing on a
// name two rules share.
func generatedNames(t *testing.T, res language.GenerateResult,
) map[string]string {
	t.Helper()
	byName := make(map[string]string, len(res.Gen))
	for _, r := range res.Gen {
		if kind, dup := byName[r.Name()]; dup {
			t.Errorf("duplicate target name %q: %s and %s", r.Name(), r.Kind(), kind)
		}
		byName[r.Name()] = r.Kind()
	}
	return byName
}

func kindsOf(rules []*rule.Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Kind()+"("+r.Name()+")")
	}
	return out
}

func assertRule(t *testing.T, byName map[string]string, name, kind string) {
	t.Helper()
	got, ok := byName[name]
	if !ok {
		t.Errorf("no target named %q; generated %v", name, byName)
		return
	}
	if got != kind {
		t.Errorf("target %q: got kind %s, want %s", name, got, kind)
	}
}

func wantStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// Label lists have exact membership and multiplicity, but no semantic ordering.
func wantLabels(t *testing.T, what string, got, want []string) {
	t.Helper()
	normalize := func(values []string) []string {
		out := slices.Clone(values)
		for i, value := range out {
			lbl, err := label.Parse(value)
			if err != nil {
				t.Fatalf("%s: invalid label %q: %v", what, value, err)
			}
			out[i] = lbl.String()
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(normalize(got), normalize(want)) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func withdraws(res language.GenerateResult, kind, name string) bool {
	for _, r := range res.Empty {
		if r.Kind() == kind && r.Name() == name {
			return true
		}
	}
	return false
}

func hasSrc(srcs []string, want string) bool {
	return slices.Contains(srcs, want)
}

// onDiskRule is the rule of that kind and name in pkg's BUILD file after a run.
func onDiskRule(t *testing.T, root, pkg, kind, name string) *rule.Rule {
	t.Helper()
	r := ruleNamed(loadRules(t, root, pkg), kind, name)
	if r == nil {
		t.Fatalf("no %s(%s) in //%s:\n%s", kind, name, pkg,
			buildFileText(t, root, pkg))
	}
	return r
}

func converge(t *testing.T, tree map[string]string) (root, logged string) {
	t.Helper()
	requireTsgo(t)
	root = writeTree(t, tree)
	logged = captureLog(t, func() { convergeGazelle(t, root) })
	return root, logged
}

const (
	rootManifest = `{"name":"w"}` + "\n"
	includeAll   = `{"include":["**/*"]}` + "\n"
	includeTs    = `{"compilerOptions":{"lib":["es2022"]},"include":["*.ts"]}` +
		"\n"
	includeSrc = `{"compilerOptions":{"lib":["es2022"]},` +
		`"include":["src/**/*"]}` + "\n"
	loadDefs = `load("@rules_typescript//ts:defs.bzl", `
)

// ---- the package ------------------------------------------------------------

// A directory below a package is not one: its files are the package's, and a
// BUILD file an earlier run left there is emptied and named, once.
func TestGenerate_ADirectoryBelowAPackageIsNotAPackage(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":      rootManifest,
		"pkg/tsconfig.json": includeAll,
		"pkg/src/a.ts":      "export const a = 1;\n",
		"pkg/src/a.test.ts": "export const t = 1;\n",
		"pkg/src/BUILD.bazel": loadDefs + `"ts_compile", "ts_test")

ts_compile(
    name = "src",
    srcs = ["a.ts"],
)

ts_test(
    name = "src_test",
    srcs = ["a.test.ts"],
)
`,
	}))

	below := g.results["pkg/src"]
	if len(below.Gen) != 0 {
		t.Errorf("pkg/src generated %v; it holds no tsconfig.json",
			generatedNames(t, below))
	}
	got := kindsOf(below.Empty)
	sort.Strings(got)
	wantStrings(t, "pkg/src withdraws", got, []string{
		"filegroup(vitest_config)", "filegroup(wrangler_config)",
		"node_modules(node_modules)", "ts_compile(src)", "ts_config(tsconfig)",
		"ts_dev_server(dev)", "ts_test(src_test)"})
	if n := strings.Count(g.logged, "pkg/src/BUILD.bazel"); n != 1 {
		t.Errorf("the BUILD file to delete was named %d times, want once:\n%s",
			n, g.logged)
	}

	pkg := g.results["pkg"]
	compile := mustRule(t, pkg, "ts_compile", "pkg")
	wantStrings(t, "ts_compile srcs", compile.AttrStrings("srcs"),
		[]string{"src/a.ts"})
	test := mustRule(t, pkg, "ts_test", "pkg_test")
	wantStrings(t, "ts_test srcs", test.AttrStrings("srcs"),
		[]string{"src/a.test.ts"})
	for _, r := range []*rule.Rule{compile, test} {
		if got := r.AttrString("tsconfig"); got != ":tsconfig" {
			t.Errorf("%s tsconfig = %q, want :tsconfig", r.Kind(), got)
		}
	}
}

// A rule under # keep in a directory that is no package is the merger's to
// leave, so the run does not say it is withdrawn.
func TestGenerate_AKeptRuleInANonPackageIsNotSaidWithdrawn(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json": rootManifest,
		"fixture/a.ts": "export const a = 1;\n",
		"fixture/BUILD.bazel": loadDefs + `"ts_compile")

# keep
ts_compile(
    name = "fixture",
    srcs = ["a.ts"],
)
`,
	}))
	if !withdraws(g.results["fixture"], "ts_compile", "fixture") {
		t.Errorf("fixture does not withdraw its ts_compile; Empty = %v",
			kindsOf(g.results["fixture"].Empty))
	}
	if strings.Contains(g.logged, "fixture/BUILD.bazel") {
		t.Errorf("the kept rule was said to be withdrawn:\n%s", g.logged)
	}
}

// srcs are the program's files wherever they sit plus the tree's other regular
// files; a deeper package's, an out_dir's and the program's exclusions are not.
func TestGenerate_SrcsAreTheProgramAndTheTreesOtherFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json": rootManifest,
		"pkg/BUILD.bazel": loadDefs + `"ts_codegen")

ts_codegen(
    name = "tree",
    srcs = ["names.txt"],
    generator = "//:gen",
    out_dir = "compiled",
)
`,
		"pkg/tsconfig.json": `{"compilerOptions":{"resolveJsonModule":true,` +
			`"module":"esnext","moduleResolution":"bundler","lib":["es2022"]},` +
			`"include":["src/**/*"],"exclude":["src/skip.ts"]}` + "\n",
		"pkg/package.json": `{"name":"pkg"}` + "\n",
		"pkg/README.md":    "# pkg\n",
		"pkg/names.txt":    "a\n",
		"pkg/legacy.js":    "module.exports = 1;\n",
		"pkg/src/a.ts": "import d from \"./data.json\";\n" +
			"export const a = d;\n",
		"pkg/src/skip.ts":           "export const skipped = 1;\n",
		"pkg/src/data.json":         `{"k":1}` + "\n",
		"pkg/src/deep/b.ts":         "export const b = 1;\n",
		"pkg/src/deep/fixture.snap": "snap\n",
		"pkg/assets/logo.svg":       "<svg/>\n",
		"pkg/tools/gen.ts":          "export const g = 1;\n",
		"pkg/compiled/index.ts":     "export const generated = 1;\n",
		"pkg/compiled/README.md":    "# generated\n",
		"pkg/sub/tsconfig.json":     includeTs,
		"pkg/sub/c.ts":              "export const c = 1;\n",
		"pkg/sub/notes.md":          "notes\n",
	})
	g := generateAll(t, root)

	compile := mustRule(t, g.results["pkg"], "ts_compile", "pkg")
	wantStrings(t, "ts_compile pkg srcs", compile.AttrStrings("srcs"), []string{
		"README.md", "assets/logo.svg", "names.txt",
		"src/a.ts", "src/data.json", "src/deep/b.ts", "src/deep/fixture.snap"})
	sub := mustRule(t, g.results["pkg/sub"], "ts_compile", "sub")
	wantStrings(t, "ts_compile sub srcs", sub.AttrStrings("srcs"),
		[]string{"c.ts", "notes.md"})
	for _, rel := range []string{"pkg/compiled", "pkg/tools", "pkg/src"} {
		if res := g.results[rel]; len(res.Gen) != 0 {
			t.Errorf("%s generated %v, want nothing", rel, generatedNames(t, res))
		}
	}

	writeFile(t, filepath.Join(root, "pkg/src/a.ts"), "import d from './data.json'; import scope from '../package.json'; export const a = [d, scope];\n")
	g = generateAll(t, root)
	compile = mustRule(t, g.results["pkg"], "ts_compile", "pkg")
	if !slices.Contains(compile.AttrStrings("srcs"), "package.json") {
		t.Fatal("compiler-observed package.json import was demoted to metadata")
	}
}

// A program with no inputs is no package: its directory's files belong to the
// package above, and a ts_config an earlier run wrote there is withdrawn.
func TestGenerate_AProgramWithNoInputsIsNoPackage(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":              rootManifest,
		"site/tsconfig.json":        `{"include":["script/**/*","src/**/*"]}` + "\n",
		"site/src/a.ts":             "export const a = 1;\n",
		"site/script/b.ts":          "export const b = 1;\n",
		"site/script/tsconfig.json": `{"include":["nothing/**/*"]}` + "\n",
		"site/script/BUILD.bazel": loadDefs + `"ts_compile", "ts_config")

ts_compile(
    name = "script",
    srcs = ["b.ts"],
    tsconfig = ":tsconfig",
)

ts_config(
    name = "tsconfig",
    src = "tsconfig.json",
)
`,
	}))

	script := g.results["site/script"]
	if len(script.Gen) != 0 {
		t.Errorf("site/script generated %v; TS18003 names no file",
			generatedNames(t, script))
	}
	for _, want := range [][2]string{
		{"ts_compile", "script"}, {"ts_config", "tsconfig"},
	} {
		if !withdraws(script, want[0], want[1]) {
			t.Errorf("site/script does not withdraw %s(%s); Empty = %v",
				want[0], want[1], kindsOf(script.Empty))
		}
	}
	site := mustRule(t, g.results["site"], "ts_compile", "site")
	wantStrings(t, "ts_compile site srcs", site.AttrStrings("srcs"),
		[]string{"script/b.ts", "script/tsconfig.json", "src/a.ts"})
}

// Target names are the directory's; the repository root's is root.
func TestGenerate_TargetNamesAreTheDirectorys(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":      rootManifest,
		"tsconfig.json":     `{"include":["src/**/*"]}` + "\n",
		"src/a.ts":          "export const a = 1;\n",
		"src/a.test.ts":     "export const t = 1;\n",
		"a/b/tsconfig.json": includeTs,
		"a/b/x.ts":          "export const x = 1;\n",
		"a/b/x.test.ts":     "export const t = 1;\n",
	}))
	root := generatedNames(t, g.results[""])
	assertRule(t, root, "root", "ts_compile")
	assertRule(t, root, "root_test", "ts_test")
	assertRule(t, root, tsConfigTargetName, "ts_config")
	deep := generatedNames(t, g.results["a/b"])
	assertRule(t, deep, "b", "ts_compile")
	assertRule(t, deep, "b_test", "ts_test")
}

// A package whose every owned file is a declaration compiles nothing: no
// target, and -ts_verbose says so.
func TestGenerate_ADeclarationOnlyPackageWritesNoTarget(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":        rootManifest,
		"types/tsconfig.json": `{"include":["*.d.ts"]}` + "\n",
		"types/globals.d.ts":  "declare const BUILD_ID: string;\n",
	}), verbose)
	got := generatedNames(t, g.results["types"])
	if len(got) != 1 || got[tsConfigTargetName] != "ts_config" {
		t.Errorf("types generated %v, want its ts_config alone", got)
	}
	if !strings.Contains(g.logged, "types/tsconfig.json") {
		t.Errorf("the target-less program was not named:\n%s", g.logged)
	}
}

// tsc drops x.mjs from a program that holds x.d.mts and reads the declaration
// in its place, so the JavaScript a declaration stands for is a src beside it.
func TestGenerate_ADeclarationsJavaScriptTwinIsASrc(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":      rootManifest,
		"lib/tsconfig.json": includeAll,
		"lib/compile.d.mts": "export declare function compile(v: number): string;\n",
		"lib/compile.mjs":   "export function compile(v) {\n  return `${v}`;\n}\n",
		"lib/compile.test.ts": "import { compile } from \"./compile.mjs\";\n" +
			"export const s = compile(1);\n",
		"lib/index.ts":  "export { compile } from \"./compile.mjs\";\n",
		"lib/legacy.js": "module.exports = 1;\n",
	}))

	compile := mustRule(t, g.results["lib"], "ts_compile", "lib")
	wantStrings(t, "ts_compile srcs", compile.AttrStrings("srcs"),
		[]string{"compile.d.mts", "compile.mjs", "index.ts"})
	test := mustRule(t, g.results["lib"], "ts_test", "lib_test")
	wantStrings(t, "ts_test srcs", test.AttrStrings("srcs"),
		[]string{"compile.d.mts", "compile.test.ts"})
}

// ---- the worker shape -------------------------------------------------------

var workerTree = map[string]string{
	"package.json": rootManifest,
	"worker/tsconfig.json": `{"compilerOptions":{"lib":["es2022"],` +
		`"types":["./worker-configuration.d.ts"]},` +
		`"include":["src/**/*","worker-configuration.d.ts"]}` + "\n",
	"worker/worker-configuration.d.ts": "interface Env {\n\tKV: string;\n}\n",
	"worker/wrangler.jsonc":            `{"name":"w"}` + "\n",
	"worker/src/index.ts": "export const handler = (env: Env) => " +
		"env.KV;\n",
	"worker/test/tsconfig.json": `{"extends":"../tsconfig.json",` +
		`"compilerOptions":{"types":["../worker-configuration.d.ts"]},` +
		`"include":["**/*.ts"]}` + "\n",
	"worker/test/env.d.ts": "declare const TEST_ENV: string;\n",
	"worker/test/index.spec.ts": "import { handler } from \"../src/index\";\n" +
		"export const t = handler;\nexport const e = TEST_ENV;\n",
}

// The test program owns env.d.ts and index.spec.ts: one ts_test, no ts_compile
// over the declaration alone, deps from the listing and no types attribute.
func TestGenerate_WorkerTestPackageWritesTsTestOnly(t *testing.T) {
	root, _ := converge(t, workerTree)

	rules := loadRules(t, root, "worker/test")
	if r := ruleNamed(rules, "ts_compile", "test"); r != nil {
		t.Errorf("worker/test writes a ts_compile over env.d.ts alone:\n%s",
			buildFileText(t, root, "worker/test"))
	}
	test := onDiskRule(t, root, "worker/test", "ts_test", "test_test")
	wantStrings(t, "ts_test srcs", test.AttrStrings("srcs"),
		[]string{"env.d.ts", "index.spec.ts"})
	wantStrings(t, "ts_test deps", test.AttrStrings("deps"),
		[]string{"//worker"})
	if test.Attr("types") != nil {
		t.Errorf("ts_test carries types = %v; the tsconfig owns it",
			test.Attr("types"))
	}
	wantStrings(t, "worker/test ts_config deps",
		tsConfigDeps(t, root, "worker/test"), []string{"//worker:tsconfig"})

	compile := onDiskRule(t, root, "worker", "ts_compile", "worker")
	wantStrings(t, "ts_compile worker srcs", compile.AttrStrings("srcs"),
		[]string{"src/index.ts", "worker-configuration.d.ts", "wrangler.jsonc"})
	assertNoDanglingLabels(t, root)
	if crossing := crossesPackageBoundary(t, root); len(crossing) > 0 {
		t.Errorf("srcs cross a package boundary:\n%s",
			strings.Join(crossing, "\n"))
	}
}

func TestGenerate_DeclarationCodegenIsEveryTargetsDep(t *testing.T) {
	root, _ := converge(t, map[string]string{
		"package.json": rootManifest,
		"BUILD.bazel": "filegroup(\n    name = \"gen\",\n" +
			"    srcs = [\"gen.sh\"],\n)\n",
		"gen.sh": "#!/bin/sh\n",
		"worker/BUILD.bazel": loadDefs + `"ts_codegen")

ts_codegen(
    name = "worker_types",
    srcs = ["wrangler.jsonc"],
    outs = ["worker-configuration.d.ts"],
    generator = "//:gen",
)
`,
		"worker/wrangler.jsonc": `{"name":"w"}` + "\n",
		"worker/tsconfig.json": `{"compilerOptions":{"lib":["es2022"],` +
			`"types":["./worker-configuration.d.ts"]},` +
			`"include":["src/**/*"]}` + "\n",
		"worker/src/index.ts": "export const handler = (env: Env) => " +
			"env.KV;\n",
		"worker/src/index.test.ts": "export const t = 1;\n",
	})
	compile := onDiskRule(t, root, "worker", "ts_compile", "worker")
	wantStrings(t, "ts_compile worker deps", compile.AttrStrings("deps"),
		[]string{":worker_types"})
	test := onDiskRule(t, root, "worker", "ts_test", "worker_test")
	wantStrings(t, "ts_test worker_test deps", test.AttrStrings("deps"),
		[]string{":worker", ":worker_types"})
	assertNoDanglingLabels(t, root)
}

// A declaration left by a local generator run still comes from its provider.
func TestGenerate_DeclarationCodegenOutOnDiskIsNoSrc(t *testing.T) {
	root, _ := converge(t, map[string]string{
		"package.json": rootManifest,
		"BUILD.bazel": "filegroup(\n    name = \"gen\",\n" +
			"    srcs = [\"gen.sh\"],\n)\n",
		"gen.sh": "#!/bin/sh\n",
		"worker/BUILD.bazel": loadDefs + `"ts_codegen")

ts_codegen(
    name = "worker_types",
    srcs = ["wrangler.jsonc"],
    outs = ["worker-configuration.d.ts"],
    generator = "//:gen",
)
`,
		"worker/wrangler.jsonc": `{"name":"w"}` + "\n",
		"worker/tsconfig.json": `{"compilerOptions":{"lib":["es2022"],` +
			`"types":["./worker-configuration.d.ts"]},` +
			`"include":["src/**/*","worker-configuration.d.ts"]}` + "\n",
		"worker/worker-configuration.d.ts": "interface Env {\n\tKV: string;\n" +
			"}\n",
		"worker/src/index.ts": "export const handler = (env: Env) => " +
			"env.KV;\n",
		"worker/src/index.test.ts": "export const t = 1;\n",
		"worker/test/tsconfig.json": `{"extends":"../tsconfig.json",` +
			`"compilerOptions":{"types":["../worker-configuration.d.ts"]},` +
			`"include":["**/*.ts"]}` + "\n",
		"worker/test/index.spec.ts": "import { handler } from " +
			"\"../src/index\";\nexport const t = handler;\n",
	})
	compile := onDiskRule(t, root, "worker", "ts_compile", "worker")
	wantStrings(t, "ts_compile worker srcs", compile.AttrStrings("srcs"),
		[]string{"src/index.ts", "wrangler.jsonc"})
	test := onDiskRule(t, root, "worker", "ts_test", "worker_test")
	wantStrings(t, "ts_test worker_test srcs", test.AttrStrings("srcs"),
		[]string{"src/index.test.ts"})
	below := onDiskRule(t, root, "worker/test", "ts_test", "test_test")
	wantStrings(t, "ts_test test_test deps", below.AttrStrings("deps"),
		[]string{"//worker", "//worker:worker_types"})
	assertNoDanglingLabels(t, root)
}

// The ts_compile takes the directory's name; a hand-written rule of another
// kind holding it keeps the merger from writing the compile; the run says so.
func TestGenerate_AGeneratedNameAHandWrittenRuleHoldsIsSaid(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json": rootManifest,
		"worker/BUILD.bazel": loadDefs + `"ts_codegen")

ts_codegen(
    name = "worker",
    srcs = ["wrangler.jsonc"],
    outs = ["worker-configuration.d.ts"],
    generator = "//:gen",
)
`,
		"worker/wrangler.jsonc": `{"name":"w"}` + "\n",
		"worker/tsconfig.json": `{"compilerOptions":{"lib":["es2022"],` +
			`"types":["./worker-configuration.d.ts"]},` +
			`"include":["src/**/*"]}` + "\n",
		"worker/src/index.ts": "export const handler = (env: Env) => " +
			"env.KV;\n",
	}))
	mustRule(t, g.results["worker"], "ts_compile", "worker")
	for _, want := range []string{"ts_codegen(worker)", "ts_compile"} {
		if !strings.Contains(g.logged, want) {
			t.Errorf("the taken name was not said with %q:\n%s", want, g.logged)
		}
	}
}

// A BUILD file an earlier run left inside an out_dir is emptied and named.
func TestGenerate_APackageLeftUnderAnOutDirIsEmptied(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json": rootManifest,
		"web/BUILD.bazel": loadDefs + `"ts_codegen")

ts_codegen(
    name = "messages",
    srcs = ["names.txt"],
    generator = "//:gen",
    out_dir = "compiled",
)
`,
		"web/names.txt":     "hello\n",
		"web/tsconfig.json": includeTs,
		"web/app.ts":        "export const x = 1;\n",
		"web/compiled/BUILD.bazel": loadDefs + `"ts_compile")

ts_compile(
    name = "compiled",
    srcs = ["index.ts"],
)
`,
		"web/compiled/index.ts":        "export const generated = 1;\n",
		"web/compiled/tsconfig.json":   `{"files":["stale-secret.ts"]}`,
		"web/compiled/stale-secret.ts": "export const stale = 1;\n",
	}))
	res := g.results["web/compiled"]
	if len(res.Gen) != 0 || !withdraws(res, "ts_compile", "compiled") {
		t.Errorf("web/compiled: generated %v, Empty %v; want the withdrawal",
			generatedNames(t, res), kindsOf(res.Empty))
	}
	if !strings.Contains(g.logged, "web/compiled") {
		t.Errorf("web/compiled was not named as a package in an out_dir:\n%s",
			g.logged)
	}
	web := mustRule(t, g.results["web"], "ts_compile", "web")
	if srcs := web.AttrStrings("srcs"); hasSrc(srcs, "compiled/index.ts") {
		t.Errorf("web's srcs %v hold a file under the out_dir", srcs)
	}
}

// ---- ts_config --------------------------------------------------------------

// A ts_config is written for every package and for a tsconfig.json a program's
// chain extends; one nothing extends and no program lists gets nothing.
func TestGenerate_TsConfigForTheRootAProgramExtends(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json":  rootManifest,
		"tsconfig.json": `{"compilerOptions":{"strict":true}}` + "\n",
		"scripts/tsconfig.json": `{"extends":"../tsconfig.json",` +
			`"include":["*.ts"]}` + "\n",
		"scripts/run.ts":       "export const run = 1;\n",
		"unused/tsconfig.json": `{"compilerOptions":{"strict":true}}` + "\n",
		"unused/README.md":     "nothing here\n",
		"unused/BUILD.bazel": loadDefs + `"ts_config")

ts_config(
    name = "tsconfig",
    src = "tsconfig.json",
)
`,
	})
	g := generateAll(t, root)

	rootRes := g.results[""]
	cfg := mustRule(t, rootRes, "ts_config", tsConfigTargetName)
	if got := cfg.AttrString("src"); got != "tsconfig.json" {
		t.Errorf("root ts_config src = %q, want tsconfig.json", got)
	}
	if cfg.Attr("deps") != nil {
		t.Errorf("root ts_config deps = %v, want none", cfg.Attr("deps"))
	}

	scripts := mustRule(t, g.results["scripts"], "ts_config", tsConfigTargetName)
	wantStrings(t, "scripts ts_config deps", scripts.AttrStrings("deps"),
		[]string{"//:tsconfig"})

	unused := g.results["unused"]
	if len(unused.Gen) != 0 {
		t.Errorf("unused generated %v; nothing extends its tsconfig.json",
			generatedNames(t, unused))
	}
	if !withdraws(unused, "ts_config", tsConfigTargetName) {
		t.Errorf("unused keeps its stale ts_config; Empty = %v",
			kindsOf(unused.Empty))
	}
	if output, err := protoGazelle(t, root); err != nil {
		t.Fatalf("resolve extended root configuration: %v\n%s", err, output)
	}
	for _, r := range loadRules(t, root, "") {
		if r.Kind() == "ts_compile" {
			t.Errorf("the root publishes %s: its tsconfig.json is no program", r.Name())
		}
	}
}

// Every base of an extends array is a dep; a base that is no tsconfig.json has
// no ts_config to name, so the run says so and writes nothing for it.
func TestGenerate_TsConfigDepsAreTheChainsBases(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":           rootManifest,
		"base/tsconfig.json":     `{"compilerOptions":{"lib":["es2022"]}}` + "\n",
		"base/README.md":         "a base\n",
		"strict/tsconfig.json":   `{"compilerOptions":{"strict":true}}` + "\n",
		"strict/README.md":       "a base\n",
		"app/tsconfig.base.json": `{"compilerOptions":{"noEmit":true}}` + "\n",
		"app/tsconfig.json": `{"extends":["../base/tsconfig.json",` +
			`"../strict/tsconfig.json","./tsconfig.base.json"],` +
			`"include":["*.ts"]}` + "\n",
		"app/a.ts": "export const a = 1;\n",
	}))
	cfg := mustRule(t, g.results["app"], "ts_config", tsConfigTargetName)
	wantStrings(t, "app ts_config deps", cfg.AttrStrings("deps"),
		[]string{"//base:tsconfig", "//strict:tsconfig"})
	for _, base := range []string{"base", "strict"} {
		if generatedRule(g.results[base], tsConfigTargetName) == nil {
			t.Errorf("%s writes no ts_config; app's chain extends it", base)
		}
	}
	if !strings.Contains(g.logged, "tsconfig.base.json") {
		t.Errorf("the base with no ts_config was not named:\n%s", g.logged)
	}
}

// ---- deps -------------------------------------------------------------------

const zodLock = `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      zod:
        specifier: ^3.24.2
        version: 3.24.2

packages:

  zod@3.24.2:
    resolution: {integrity: sha512-aaa}

snapshots:

  zod@3.24.2: {}
`

// The listing is the one source of deps: a specifier the lexer would read but
// tsgo did not resolve -- the package is not installed -- yields no label.
func TestGenerate_DepsComeFromTheListingAlone(t *testing.T) {
	root, _ := converge(t, map[string]string{
		"package.json": `{"name":"w","dependencies":{"zod":"3.24.2"}}` +
			"\n",
		pnpmLockfileName:             zodLock,
		"node_modules/.modules.yaml": "layoutVersion: 5\n",
		"pkg/tsconfig.json":          includeTs,
		"pkg/a.ts": "import { z } from \"zod\";\n" +
			"export const s = z;\n",
	})
	r := onDiskRule(t, root, "pkg", "ts_compile", "pkg")
	if deps := r.AttrStrings("deps"); len(deps) != 0 {
		t.Errorf("deps = %v, want none: no listing names zod", deps)
	}
}

// ---- the vitest config ------------------------------------------------------

// The config beside the tests is the test's, and what it imports is a dep.
func TestGenerate_VitestConfigBesideTheTestsIsTheTests(t *testing.T) {
	root, _ := converge(t, map[string]string{
		"package.json":            rootManifest,
		"shared/tsconfig.json":    includeTs,
		"shared/vitest.shared.ts": "export const shared = true;\n",
		"pkg/tsconfig.json":       includeSrc,
		"pkg/src/a.ts":            "export const a = 1;\n",
		"pkg/src/a.test.ts": "import { a } from \"./a\";\n" +
			"export const t = a;\n",
		"pkg/vitest.config.mts": "import { shared } from " +
			"\"../shared/vitest.shared\";\n" +
			"export default { test: { globals: shared } };\n",
	})
	test := onDiskRule(t, root, "pkg", "ts_test", "pkg_test")
	if got := test.AttrString("config"); got != "vitest.config.mts" {
		t.Errorf("config = %q, want vitest.config.mts", got)
	}
	wantStrings(t, "ts_test deps", test.AttrStrings("deps"),
		[]string{":pkg", "//shared"})
	assertNoDanglingLabels(t, root)
}

// With no vitest.config.*, plain vitest reads vite.config.*: the test's config.
func TestGenerate_ViteConfigIsTheTestsWhenNoVitestConfig(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":       rootManifest,
		"pkg/tsconfig.json":  includeSrc,
		"pkg/src/a.test.ts":  "export const t = 1;\n",
		"pkg/vite.config.ts": "export default { define: { __A__: \"1\" } };\n",
	}))
	test := mustRule(t, g.results["pkg"], "ts_test", "pkg_test")
	if got := test.AttrString("config"); got != "vite.config.ts" {
		t.Errorf("config = %q, want vite.config.ts", got)
	}
}

// Beside a vite.config.*, the vitest.config.* is the one vitest reads.
func TestGenerate_VitestConfigWinsOverViteConfig(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":         rootManifest,
		"pkg/tsconfig.json":    includeSrc,
		"pkg/src/a.test.ts":    "export const t = 1;\n",
		"pkg/vite.config.ts":   "export default {};\n",
		"pkg/vitest.config.ts": "export default { test: { globals: true } };\n",
	}))
	test := mustRule(t, g.results["pkg"], "ts_test", "pkg_test")
	if got := test.AttrString("config"); got != "vitest.config.ts" {
		t.Errorf("config = %q, want vitest.config.ts", got)
	}
}

// Plain vitest reads the config beside the nearest package.json; a test a
// package down names it by label, and that package writes the filegroup.
func TestGenerate_VitestConfigAtThePackageRootReachesATestBelow(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":              rootManifest,
		"worker/package.json":       `{"name":"worker"}` + "\n",
		"worker/vitest.config.mts":  "export default { test: { globals: true } };\n",
		"worker/tsconfig.json":      `{"include":["src/**/*"]}` + "\n",
		"worker/src/index.ts":       "export const w = 1;\n",
		"worker/test/tsconfig.json": includeTs,
		"worker/test/index.test.ts": "export const t = 1;\n",
	}))
	test := mustRule(t, g.results["worker/test"], "ts_test", "test_test")
	if got := test.AttrString("config"); got != "//worker:vitest_config" {
		t.Errorf("config = %q, want //worker:vitest_config", got)
	}
	fg := mustRule(t, g.results["worker"], "filegroup", vitestConfigTargetName)
	wantStrings(t, "filegroup srcs", fg.AttrStrings("srcs"),
		[]string{"vitest.config.mts"})
	wantStrings(t, "filegroup visibility", fg.AttrStrings("visibility"),
		[]string{"//visibility:public"})
}

// A config beside a package.json in a directory that is no package is a label
// nothing writes: the test gets no config and the run says which file.
func TestGenerate_VitestConfigInANonPackageIsSaid(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":           rootManifest,
		"app/package.json":       `{"name":"app"}` + "\n",
		"app/vitest.config.mts":  "export default { test: { globals: true } };\n",
		"app/test/tsconfig.json": includeTs,
		"app/test/a.test.ts":     "export const t = 1;\n",
	}))
	test := mustRule(t, g.results["app/test"], "ts_test", "test_test")
	if test.Attr("config") != nil {
		t.Errorf("config = %q, want unset: app is no package",
			test.AttrString("config"))
	}
	if !strings.Contains(g.logged, "app/vitest.config.mts") {
		t.Errorf("the unreachable config was not named:\n%s", g.logged)
	}
	if generatedRule(g.results["app"], vitestConfigTargetName) != nil {
		t.Errorf("app writes a filegroup in a directory that is no package")
	}
}

// The repository root always has a BUILD file, so its config is its label.
func TestGenerate_VitestConfigAtTheRepoRootIsTheRootLabel(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":           rootManifest,
		"vitest.config.ts":       "export default { test: { globals: true } };\n",
		"pkg/test/tsconfig.json": includeTs,
		"pkg/test/index.test.ts": "export const t = 1;\n",
	}))
	test := mustRule(t, g.results["pkg/test"], "ts_test", "test_test")
	if got := test.AttrString("config"); got != "//:vitest_config" {
		t.Errorf("config = %q, want //:vitest_config", got)
	}
	mustRule(t, g.results[""], "filegroup", vitestConfigTargetName)
}

// The config went; the filegroup goes with it.
func TestGenerate_WithdrawsTheVitestConfigFilegroupWithTheFile(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":      rootManifest,
		"pkg/package.json":  `{"name":"pkg"}` + "\n",
		"pkg/tsconfig.json": includeTs,
		"pkg/a.ts":          "export const a = 1;\n",
		"pkg/BUILD.bazel": `filegroup(
    name = "vitest_config",
    srcs = ["vitest.config.ts"],
    visibility = ["//visibility:public"],
)
`,
	}))
	if !withdraws(g.results["pkg"], "filegroup", vitestConfigTargetName) {
		t.Errorf("the filegroup outlived its file; Empty = %v",
			kindsOf(g.results["pkg"].Empty))
	}
}

// ---- the Workers pool -------------------------------------------------------

// A pool config as a worker writes it, naming its wrangler config through
// `wrangler.configPath`.
func poolConfig(configPath string) string {
	return "import { cloudflareTest } from " +
		"\"@cloudflare/vitest-pool-workers\";\n" +
		"export default { plugins: [cloudflareTest({ wrangler: { configPath: " +
		"\"" + configPath + "\" } })] };\n"
}

// The wrangler config a package-root vitest config names in a string literal
// is a label beside vitest_config, the file's name as configPath spells it.
func TestGenerate_WranglerConfigTheConfigNamesIsAFilegroup(t *testing.T) {
	root, _ := converge(t, map[string]string{
		"MODULE.bazel":               "module(name = \"wrangler_after_walk\")\n",
		"package.json":               `{"name":"w","devDependencies":{"@vitest/coverage-istanbul":"4.1.11"}}` + "\n",
		pnpmLockfileName:             poolLock,
		"node_modules/.modules.yaml": "layoutVersion: 5\n",
		"node_modules/@cloudflare/vitest-pool-workers/package.json": `{"name":"@cloudflare/vitest-pool-workers","version":"0.18.4","types":"index.d.ts"}`,
		"node_modules/@cloudflare/vitest-pool-workers/index.d.ts":   "export declare function cloudflareTest(o: unknown): unknown;\n",
		"worker/package.json":        `{"name":"worker","devDependencies":{"@cloudflare/vitest-pool-workers":"0.18.4"}}` + "\n",
		"worker/vitest.config.mts":   poolConfig("./wrangler.test.jsonc"),
		"worker/wrangler.test.jsonc": `{"main":"src/index.ts"}` + "\n",
		"worker/tsconfig.json":       includeSrc,
		"worker/src/index.ts":        "export const w = 1;\n",
		"worker/test/tsconfig.json":  includeTs,
		"worker/test/index.test.ts":  "export const t = 1;\n",
	})
	fg := onDiskRule(t, root, "worker", "filegroup", "wrangler_config")
	wantStrings(t, "filegroup srcs", fg.AttrStrings("srcs"),
		[]string{"wrangler.test.jsonc"})
	wantStrings(t, "filegroup visibility", fg.AttrStrings("visibility"),
		[]string{"//visibility:public"})
	onDiskRule(t, root, "worker", "filegroup", vitestConfigTargetName)
	first := buildFileBytes(t, root)
	for _, args := range [][]string{nil, {"-index=false", "-r=false", "worker/test"}} {
		output, err := protoGazelle(t, root, args...)
		if err != nil {
			t.Fatalf("Wrangler publication after the walk with %v: %v\n%s", args, err, output)
		}
		if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
			t.Fatalf("Wrangler replay changed observed BUILD files: %s", diff)
		}
	}
}

// The pool reads a wrangler config through configPath alone, so a config naming
// none gets no filegroup, whatever sits beside it; a stale one is withdrawn.
func TestGenerate_AConfigNamingNoWranglerConfigWritesNoFilegroup(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":        rootManifest,
		"worker/package.json": `{"name":"worker"}` + "\n",
		"worker/vitest.config.mts": "import { cloudflareTest } from " +
			"\"@cloudflare/vitest-pool-workers\";\n" +
			"export default { plugins: [cloudflareTest({ miniflare: {} })] };\n",
		"worker/wrangler.jsonc":     `{"main":"src/index.ts"}` + "\n",
		"worker/tsconfig.json":      includeSrc,
		"worker/src/index.ts":       "export const w = 1;\n",
		"worker/test/tsconfig.json": includeTs,
		"worker/test/index.test.ts": "export const t = 1;\n",
		"worker/BUILD.bazel": `filegroup(
    name = "wrangler_config",
    srcs = ["wrangler.jsonc"],
    visibility = ["//visibility:public"],
)
`,
	}))
	if !withdraws(g.results["worker"], "filegroup", "wrangler_config") {
		t.Errorf("a filegroup for a file no configPath names; Gen = %v, Empty = %v",
			generatedNames(t, g.results["worker"]),
			kindsOf(g.results["worker"].Empty))
	}
}

// The file the literal names went: the filegroup goes with it, and the run
// says which file the config still names.
func TestGenerate_WithdrawsTheWranglerConfigFilegroupWithTheFile(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":             rootManifest,
		"worker/package.json":      `{"name":"worker"}` + "\n",
		"worker/vitest.config.mts": poolConfig("./wrangler.jsonc"),
		"worker/tsconfig.json":     includeSrc,
		"worker/src/index.ts":      "export const w = 1;\n",
		"worker/BUILD.bazel": `filegroup(
    name = "wrangler_config",
    srcs = ["wrangler.jsonc"],
    visibility = ["//visibility:public"],
)
`,
	}))
	if !withdraws(g.results["worker"], "filegroup", "wrangler_config") {
		t.Errorf("the filegroup outlived its file; Empty = %v",
			kindsOf(g.results["worker"].Empty))
	}
	if !strings.Contains(g.logged, "worker/wrangler.jsonc") {
		t.Errorf("the missing file was not named:\n%s", g.logged)
	}
}

// The pooled shape end to end: the config's edge names the pool, so the test
// names the filegroup, runs istanbul coverage and carries its package.
func TestGenerate_APooledTestNamesTheWranglerConfigAndIstanbul(t *testing.T) {
	root, _ := converge(t, map[string]string{
		"package.json":               `{"name":"w","devDependencies":{"@vitest/coverage-istanbul":"4.1.11"}}` + "\n",
		pnpmLockfileName:             poolLock,
		"node_modules/.modules.yaml": "layoutVersion: 5\n",
		"node_modules/@cloudflare/vitest-pool-workers/package.json": `{"name":` +
			`"@cloudflare/vitest-pool-workers","version":"0.18.4",` +
			`"types":"index.d.ts"}` + "\n",
		"node_modules/@cloudflare/vitest-pool-workers/index.d.ts": "export " +
			"declare function cloudflareTest(o: unknown): unknown;\n",
		"worker/package.json": `{"name":"worker","devDependencies":` +
			`{"@cloudflare/vitest-pool-workers":"0.18.4"}}` + "\n",
		"worker/vitest.config.mts":  poolConfig("./wrangler.jsonc"),
		"worker/wrangler.jsonc":     `{"main":"src/index.ts"}` + "\n",
		"worker/tsconfig.json":      includeSrc,
		"worker/src/index.ts":       "export const w = 1;\n",
		"worker/test/tsconfig.json": includeTs,
		"worker/test/index.test.ts": "import { w } from \"../src/index\";\n" +
			"export const t = w;\n",
	})
	test := onDiskRule(t, root, "worker/test", "ts_test", "test_test")
	if got := test.AttrString("wrangler_config"); got != "//worker:"+
		wranglerConfigTargetName {
		t.Errorf("wrangler_config = %q, want //worker:wrangler_config", got)
	}
	if got := test.AttrString("coverage_provider"); got != "istanbul" {
		t.Errorf("coverage_provider = %q, want istanbul", got)
	}
	wantStrings(t, "ts_test deps", test.AttrStrings("deps"), []string{
		"//worker", "@npm//:vitest_coverage-istanbul",
		"@npm//worker:cloudflare_vitest-pool-workers"})
	fg := onDiskRule(t, root, "worker", "filegroup", "wrangler_config")
	wantStrings(t, "filegroup srcs", fg.AttrStrings("srcs"),
		[]string{"wrangler.jsonc"})
	assertNoDanglingLabels(t, root)
}

func TestGeneratedWranglerConfigCannotDisappearWithoutCheckoutCopy(t *testing.T) {
	for _, producerDir := range []string{"worker", "worker/config"} {
		t.Run(producerDir, func(t *testing.T) {
			file := path.Join(producerDir, "wrangler.jsonc")
			configPath := "./" + strings.TrimPrefix(file, "worker/")
			wantFile := "wrangler.jsonc"
			if producerDir != "worker" {
				wantFile = "//" + producerDir + ":wrangler.jsonc"
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel":               "module(name = \"generated_wrangler\")\n",
				"package.json":               `{"name":"w","devDependencies":{"@vitest/coverage-istanbul":"4.1.11"}}` + "\n",
				pnpmLockfileName:             poolLock,
				"node_modules/.modules.yaml": "layoutVersion: 5\n",
				"node_modules/@cloudflare/vitest-pool-workers/package.json": `{"name":"@cloudflare/vitest-pool-workers","version":"0.18.4","types":"index.d.ts"}`,
				"node_modules/@cloudflare/vitest-pool-workers/index.d.ts":   "export declare function cloudflareTest(o: unknown): unknown;\n",
				"worker/package.json":                 `{"name":"worker","devDependencies":{"@cloudflare/vitest-pool-workers":"0.18.4"}}`,
				path.Join(producerDir, "BUILD.bazel"): "# gazelle:exclude wrangler.jsonc\ngenrule(name = \"wrangler\", outs = [\"wrangler.jsonc\"], cmd = \"echo config > $@\", visibility = [\"//visibility:public\"])\n",
				"worker/vitest.config.mts":            poolConfig(configPath),
				"worker/tsconfig.json":                includeSrc,
				"worker/src/index.ts":                 "export const w = 1;\n",
				"worker/src/index.test.ts":            "import { w } from './index'; export const t = w;\n",
				"worker/test/tsconfig.json":           includeTs,
				"worker/test/index.test.ts":           "import { w } from '../src/index'; export const t = w;\n",
			})
			var first map[string]string
			for _, state := range []string{"cold", "materialized", "removed"} {
				file := filepath.Join(root, file)
				if state == "materialized" {
					writeFile(t, file, "stale contents are not read by Gazelle\n")
				} else if state == "removed" {
					if err := os.Remove(file); err != nil {
						t.Fatal(err)
					}
				}
				for _, args := range [][]string{nil, {"-r=false", "worker", "worker/test"}, {"-index=false", "-r=false", "worker", "worker/test"}, {"-index=false", "-r=false", "worker/test"}} {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("%s generated Wrangler config with %v: %v\n%s", state, args, err, output)
					}
					test := onDiskRule(t, root, "worker/test", "ts_test", "test_test")
					if got := test.AttrString("wrangler_config"); got != "//worker:wrangler_config" {
						t.Errorf("%s generated Wrangler config reaches the test = %q, want //worker:wrangler_config", state, got)
					}
					wantStrings(t, "generated Wrangler File", onDiskRule(t, root, "worker", "filegroup", "wrangler_config").AttrStrings("srcs"), []string{wantFile})
					if got := onDiskRule(t, root, "worker", "ts_test", "worker_test").AttrString("wrangler_config"); got != wantFile {
						t.Errorf("%s local test Wrangler label = %q, want %q", state, got, wantFile)
					}
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("%s update %v changed generated Wrangler inputs: %s", state, args, diff)
					}
				}
			}
		})
	}
}

// worker declares the pool; istanbul is in the lockfile and in no manifest,
// so its label is the root's.
const poolLock = `lockfileVersion: '9.0'

importers:

  .:
    devDependencies:
      '@vitest/coverage-istanbul':
        specifier: 4.1.11
        version: 4.1.11

  worker:
    devDependencies:
      '@cloudflare/vitest-pool-workers':
        specifier: 0.18.4
        version: 0.18.4

packages:

  '@cloudflare/vitest-pool-workers@0.18.4':
    resolution: {integrity: sha512-aaa}

  '@vitest/coverage-istanbul@4.1.11':
    resolution: {integrity: sha512-bbb}

snapshots:

  '@cloudflare/vitest-pool-workers@0.18.4': {}

  '@vitest/coverage-istanbul@4.1.11': {}
`

// ---- the .d.mts / .d.cts flavours ------------------------------------------

// A declaration is a declaration whatever its extension: it rides in every
// target, and the JavaScript it stands for is a src beside it, listed or not.
func TestGenerate_DeclarationFlavoursRideInEveryTarget(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json": rootManifest,
		"pkg/tsconfig.json": `{"compilerOptions":{"lib":["es2022"]},` +
			`"include":["*.ts","*.mts"]}` + "\n",
		"pkg/entry.ts":    "export const e = 1;\n",
		"pkg/compile.mjs": "export function compile(v) { return `${v}`; }\n",
		"pkg/compile.d.mts": "export declare function compile(v: number): " +
			"string;\n",
		"pkg/globals.d.mts": "declare const BUILD_ID: string;\n",
		"pkg/compile.test.ts": "import { compile } from \"./compile.mjs\";\n" +
			"export const t = compile(1);\n",
	}))
	res := g.results["pkg"]
	compile := mustRule(t, res, "ts_compile", "pkg")
	wantStrings(t, "ts_compile srcs", compile.AttrStrings("srcs"),
		[]string{"compile.d.mts", "compile.mjs", "entry.ts", "globals.d.mts"})
	test := mustRule(t, res, "ts_test", "pkg_test")
	wantStrings(t, "ts_test srcs", test.AttrStrings("srcs"),
		[]string{"compile.d.mts", "compile.test.ts", "globals.d.mts"})
}

// ---- the lockfile's importers -----------------------------------------------

// Every importer gets its node_modules and a link target per member, the
// lockfile's package the store call and `hoist`; a non-importer withdraws all.
func TestGenerate_ImporterNodeModules(t *testing.T) {
	g := generateAll(t, writeTree(t, map[string]string{
		"package.json":              rootManifest,
		pnpmLockfileName:            lockText,
		"packages/lib/package.json": `{"name":"@acme/lib"}` + "\n",
		"packages/ui/package.json":  `{"name":"@acme/ui-src"}` + "\n",
		"web/package.json":          `{"name":"web-app"}` + "\n",
		"web/tsconfig.json":         includeSrc,
		"web/src/app.ts":            "export const app = 1;\n",
		"web/BUILD.bazel": `load(
    "@rules_typescript//npm:defs.bzl",
    "node_modules_member",
)

node_modules_member(
    name = "node_modules/@acme/gone",
    member = "@npm//:acme_gone",
)
`,
	}))

	root := g.results[""]
	mustRule(t, root, "npm_virtual_store", "node_modules/.pnpm")
	nm := mustRule(t, root, "node_modules", "node_modules")
	wantStrings(t, "root node_modules deps", nm.AttrStrings("deps"), []string{
		"@npm//:types_node", "@npm//:typescript", "@npm//:vite", "@npm//:zod",
	})
	if got := nm.AttrString("parent"); got != "" {
		t.Errorf("the root's node_modules names a parent: %q", got)
	}
	if got := nm.AttrString("hoist"); got != ":node_modules/.pnpm/node_modules" {
		t.Errorf("the root's hoist = %q, want :node_modules/.pnpm/node_modules",
			got)
	}
	wantStrings(t, "root node_modules visibility", nm.AttrStrings("visibility"),
		[]string{"//visibility:public"})
	lib := mustRule(t, root, "node_modules_member", "node_modules/@acme/lib")
	if got := lib.AttrString("member"); got != "@npm//:acme_lib" {
		t.Errorf("node_modules/@acme/lib links %q, want @npm//:acme_lib", got)
	}

	web := g.results["web"]
	wnm := mustRule(t, web, "node_modules", "node_modules")
	wantStrings(t, "web node_modules deps", wnm.AttrStrings("deps"), []string{
		"@npm//web:acme_lib", "@npm//web:marked", "@npm//web:react",
		"@npm//web:tailwindcss-v3", "@npm//web:types_mdast",
		"@npm//web:types_react",
	})
	if got := wnm.AttrString("parent"); got != "//:node_modules" {
		t.Errorf("web's parent = %q, want //:node_modules", got)
	}
	if got := wnm.AttrString("hoist"); got != "" {
		t.Errorf("web's node_modules names a hoist: %q", got)
	}
	ui := mustRule(t, web, "node_modules_member", "node_modules/@acme/ui")
	if got := ui.AttrString("member"); got != "@npm//:acme_ui" {
		t.Errorf("node_modules/@acme/ui links %q, want @npm//:acme_ui", got)
	}
	if !withdraws(web, "node_modules_member", "node_modules/@acme/gone") {
		t.Errorf("web keeps a link no importer declares; Empty = %v",
			kindsOf(web.Empty))
	}

	ui2 := mustRule(t, g.results["packages/ui"], "node_modules", "node_modules")
	if deps := ui2.AttrStrings("deps"); len(deps) != 0 {
		t.Errorf("packages/ui declares nothing but links %v", deps)
	}
	if got := ui2.AttrString("parent"); got != "//:node_modules" {
		t.Errorf("packages/ui's parent = %q, want //:node_modules", got)
	}

	for _, rel := range []string{"web/src", "packages"} {
		res := g.results[rel]
		if r := generatedRule(res, "node_modules"); r != nil {
			t.Errorf("%s is no importer but generates %s", rel, r.Kind())
		}
		if !withdraws(res, "node_modules", "node_modules") {
			t.Errorf("%s does not withdraw node_modules; Empty = %v", rel,
				kindsOf(res.Empty))
		}
	}
}

const foreignLock = `lockfileVersion: '9.0'

importers:

  .: {}

  app/example/inner: {}
`

func TestGenerate_AForeignProjectGetsNothing(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json":              rootManifest,
		pnpmLockfileName:            foreignLock,
		"app/tsconfig.json":         includeSrc,
		"app/src/a.ts":              "export const a = 1;\n",
		"app/README.md":             "the app\n",
		"app/example/package.json":  `{"name":"example"}` + "\n",
		"app/example/tsconfig.json": includeTs,
		"app/example/index.ts":      "export const example = 1;\n",
		"app/example/notes.md":      "a foreign project's file\n",
		"app/example/BUILD.bazel": loadDefs + `"ts_compile")` + `

ts_compile(
    name = "example",
    srcs = ["index.ts"],
    tsconfig = ":tsconfig",
)
`,
		"app/example/deep/tsconfig.json":  includeTs,
		"app/example/deep/c.ts":           "export const c = 1;\n",
		"app/example/inner/package.json":  `{"name":"inner"}` + "\n",
		"app/example/inner/tsconfig.json": includeTs,
		"app/example/inner/b.ts":          "export const b = 1;\n",
	})
	g := generateAll(t, root)

	example := g.results["app/example"]
	if r := generatedRule(example, "example"); r != nil {
		t.Errorf("app/example generates %s(example); its package.json is no "+
			"importer, so pnpm installs nothing for it", r.Kind())
	}
	if r := generatedRule(g.results["app/example/deep"], "deep"); r != nil {
		t.Errorf("app/example/deep generates %s(deep) under the foreign "+
			"manifest", r.Kind())
	}
	for _, r := range ownedRuleNames("app/example") {
		if !withdraws(example, r.Kind(), r.Name()) {
			t.Errorf("app/example does not withdraw %s(%s); Empty = %v",
				r.Kind(), r.Name(), kindsOf(example.Empty))
		}
	}
	app := mustRule(t, g.results["app"], "ts_compile", "app")
	wantStrings(t, "app srcs", app.AttrStrings("srcs"),
		[]string{"README.md", "src/a.ts"})
	mustRule(t, g.results["app/example/inner"], "ts_compile", "inner")

	said := "typescript: app/example/package.json is no importer in " +
		"pnpm-lock.yaml, so pnpm installs nothing for it; nothing is " +
		"written under app/example\n"
	if n := strings.Count(g.logged, said); n != 1 {
		t.Errorf("the manifest was named %d times, want once:\n%s", n, g.logged)
	}
	withdrawn := "app/example is not a package -- app/example/package.json " +
		"is no importer in pnpm-lock.yaml -- so ts_compile(example) is " +
		"withdrawn"
	if !strings.Contains(g.logged, withdrawn) {
		t.Errorf("the stale BUILD file was not named:\n%s", g.logged)
	}
}

func TestGazelleExclusionsPreventSourceUpload(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"excluded_inputs\")\n",
		"BUILD.bazel": "# gazelle:exclude app/local.scratch.ts\n" +
			"# gazelle:exclude app/keyfile-local.json\n" +
			"# gazelle:exclude app/generated\n" +
			"# gazelle:exclude sibling/value.d.ts\n" +
			"# gazelle:resolve typescript sibling/value.d.ts //sibling:generated\n",
		"settings/tsconfig.json": `{"compilerOptions":{"strict":true}}`,
		"vitest.config.mts":      "export default {};\n",
		".gitignore":             "app/generated/\n",
		"app/BUILD.bazel": "load(\"@rules_typescript//ts:defs.bzl\", \"ts_codegen\", \"ts_compile\")\n" +
			"ts_codegen(name = \"generated\", out_dir = \"generated\", generator = \":generator\")\n" +
			"ts_compile(name = \"app\", srcs = [\n    \"//:fixtures/helper.ts\", # keep\n])\n",
		"app/tsconfig.json": `{"extends":"../settings/tsconfig.json","compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true},"include":["**/*.ts"]}`,
		"app/index.ts": "import { generated } from './generated/value';\n" +
			"import { external } from '../sibling/value';\n" +
			"import { helper } from '../fixtures/helper';\n" +
			"import { other } from '../other/input';\n" +
			"import data from './value.json';\n" +
			"export const value = generated + external + helper + other + data.value;\n",
		"app/local.scratch.ts":   "export const excluded = 'inert excluded source';\n",
		"app/keyfile-local.json": `{"value":"inert excluded data"}`,
		"app/ordinary.ts":        "export const ordinary = 1;\n",
		"app/value.json":         `{"value":1}`,
		"app/generated/value.ts": "export const generated = 1;\n",
		"fixtures/b.ts":          "export const helper = 1;\n",
		"fixtures/helper.ts":     "export { helper } from './b';\n",
		"sibling/BUILD.bazel": "load(\"@rules_typescript//ts:defs.bzl\", \"ts_codegen\")\n" +
			"ts_codegen(name = \"generated\", outs = [\"value.d.ts\"], generator = \":generator\")\n",
		"sibling/value.d.ts":    "export declare const external: number;\n",
		"app/index.test.ts":     "export {};\n",
		"other/BUILD.bazel":     "# gazelle:lang proto\n# gazelle:exclude ignored.ts\n",
		"other/tsconfig.json":   `{"include":["*.ts"]}`,
		"other/input.ts":        "export const other = 1;\n",
		"other/ignored.ts":      "export const ignored = 'inert excluded source';\n",
		"app/vitest.config.mts": "export default {};\n",
	})
	for pass := range 2 {
		output, err := protoGazelle(t, root)
		if err != nil {
			t.Fatalf("generation %d: %v\n%s", pass, err, output)
		}
		file, err := rule.LoadFile(filepath.Join(root, "app/BUILD.bazel"), "app")
		if err != nil {
			t.Fatal(err)
		}
		compile := ruleNamed(file.Rules, "ts_compile", "app")
		if compile == nil {
			t.Fatalf("no compile rule in %s", file.Format())
		}
		for _, want := range []string{"index.ts", "ordinary.ts", "value.json", "//:fixtures/helper.ts", "//:fixtures/b.ts", "//other:input.ts"} {
			if !slices.Contains(compile.AttrStrings("srcs"), want) {
				t.Errorf("eligible input %s missing from %s", want, file.Format())
			}
		}
		for _, producer := range []string{":generated", "//sibling:generated"} {
			if !slices.Contains(compile.AttrStrings("deps"), producer) {
				t.Fatalf("generated input lost producer %s: %s", producer, file.Format())
			}
		}
		for _, excluded := range []string{"local.scratch.ts", "keyfile-local.json", "generated/value.ts", "value.d.ts"} {
			if strings.Contains(string(file.Format()), excluded) {
				t.Errorf("excluded physical source became an input: %s", file.Format())
			}
		}
	}
	before := convergeSnapshot(t, root)
	for _, target := range []string{"local.scratch.ts", "keyfile-local.json", "../other/ignored.ts"} {
		for _, importer := range []string{"index.ts", "vitest.config.mts"} {
			t.Run(importer+" imports "+target, func(t *testing.T) {
				file := filepath.Join(root, "app", importer)
				original, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { writeFile(t, file, string(original)) })
				writeFile(t, file, "import './"+target+"';\n"+string(original))
				output, err := protoGazelle(t, root)
				if err == nil || !strings.Contains(output, path.Clean(path.Join("app", target))) || !strings.Contains(output, "excluded or ignored") {
					t.Fatalf("excluded import did not fail visibly: %v\n%s", err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("excluded import changed generated inputs or exports: %s", diff)
				}
			})
		}
	}
	for _, excluded := range []string{"app/tsconfig.json", "settings/tsconfig.json", "vitest.config.mts"} {
		t.Run("excluded config "+excluded, func(t *testing.T) {
			file := filepath.Join(root, "BUILD.bazel")
			original, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { writeFile(t, file, string(original)) })
			writeFile(t, file, "# gazelle:exclude "+excluded+"\n"+string(original))
			before := convergeSnapshot(t, root)
			output, err := protoGazelle(t, root)
			if err == nil || !strings.Contains(output, excluded) || !strings.Contains(output, "excluded or ignored") {
				t.Fatalf("excluded config did not fail visibly: %v\n%s", err, output)
			}
			if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
				t.Fatalf("excluded config changed generated inputs or exports: %s", diff)
			}
		})
	}
}

func TestGeneratedConfigsKeepExplicitFileInputsWithoutDiscovery(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":           "module(name = \"generated_configs\")\n",
		"BUILD.bazel":            "# gazelle:exclude app/wrangler.jsonc\n",
		"app/tsconfig.json":      `{"extends":"../settings/tsconfig.json","files":["index.ts"]}`,
		"app/package.json":       `{"name":"config-consumer"}`,
		"settings/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":[]}`,
		"app/index.ts":           "export const value = 1;\n",
		"app/vitest.config.mts":  "export default { test: { poolOptions: { workers: { wrangler: { configPath: './wrangler.jsonc' } } } } };\n",
		"app/wrangler.jsonc":     `{"name":"example"}`,
		"app/BUILD.bazel": `genrule(name = "wrangler", outs = ["wrangler.jsonc"], cmd = "echo config > $@")
`,
	})
	output, err := protoGazelle(t, root)
	if err != nil {
		t.Fatalf("generated config eligibility: %v\n%s", err, output)
	}
	for _, pkg := range []string{"app", "settings"} {
		cfg := onDiskRule(t, root, pkg, "ts_config", "tsconfig")
		if got := cfg.AttrString("src"); got != "tsconfig.json" {
			t.Errorf("%s config src = %q, want tsconfig.json", pkg, got)
		}
	}
	wantStrings(t, "authored base", onDiskRule(t, root, "app", "ts_config", "tsconfig").AttrStrings("deps"), []string{"//settings:tsconfig"})
	wantStrings(t, "generated filegroup input", onDiskRule(t, root, "app", "filegroup", "wrangler_config").AttrStrings("srcs"), []string{"wrangler.jsonc"})
	wantStrings(t, "library deps", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("deps"), nil)
	for _, selected := range []string{"generated.json", ":generated_config"} {
		t.Run("stale selected "+selected, func(t *testing.T) {
			producer := "genrule(name = \"config_source\", outs = [\"generated.json\"], cmd = \"echo config > $@\")\n"
			build := loadDefs + `"ts_compile", "ts_config")` + "\n" + producer +
				"ts_config(name = \"generated_config\", src = \"generated.json\")\n# keep\n" +
				"ts_compile(name = \"app\", srcs = [\"index.ts\"], tsconfig = \"" + selected + "\")\n"
			root := writeTree(t, map[string]string{
				"MODULE.bazel":      "module(name = \"generated_selected_config\")\n",
				"BUILD.bazel":       "# gazelle:exclude app/secret.d.ts\n",
				"app/BUILD.bazel":   build,
				"app/tsconfig.json": `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
				"app/index.ts":      "export const value = 1;\n",
				"app/secret.d.ts":   "declare const secret: string;\n",
			})
			var first map[string]string
			for _, stale := range []bool{false, true} {
				if stale {
					writeFile(t, filepath.Join(root, "app/generated.json"), `{"compilerOptions":{"types":["./secret.d.ts"]},"files":["index.ts"]}`)
				}
				for _, args := range [][]string{nil, {"-r=false", "app"}, nil} {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("selected generated config stale=%t with %v: %v\n%s", stale, args, err, output)
					}
					r := onDiskRule(t, root, "app", "ts_compile", "app")
					if got := r.AttrString("tsconfig"); got != selected {
						t.Fatalf("kept config = %q, want %q", got, selected)
					}
					wantStrings(t, "generated config retains authored inputs", r.AttrStrings("srcs"), []string{"index.ts"})
					wantStrings(t, "generated config adds no stale type dependency", r.AttrStrings("deps"), nil)
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("generated selected config checkout state changed BUILD files: %s", diff)
					}
				}
			}
			writeFile(t, filepath.Join(root, "app/BUILD.bazel"), strings.Replace(build, producer, "", 1))
			before := convergeSnapshot(t, root)
			for _, args := range [][]string{{"-r=false", "app"}, nil} {
				output, err := protoGazelle(t, root, args...)
				if err == nil || !strings.Contains(output, "app/secret.d.ts") || !strings.Contains(output, "excluded or ignored") {
					t.Fatalf("authored config bypassed excluded type validation with %v: %v\n%s", args, err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("rejected authored config changed BUILD files: %s", diff)
				}
			}
		})
	}
}

func TestGeneratedConfigTreeCannotSupplyAutomaticMembership(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"tree_config\")\n",
		"BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(name = "settings", out_dir = "settings", generator = "//:generator")
`,
		"settings/tsconfig.json": `{"compilerOptions":{"module":"preserve","types":[]},"files":[]}`,
		"app/tsconfig.json":      `{"extends":"../settings/tsconfig.json","files":["index.ts"]}`,
		"app/index.ts":           "export const value = 42;\n",
	})
	before := convergeSnapshot(t, root)
	output, err := protoGazelle(t, root)
	if err == nil || !strings.Contains(output, "cannot discover program membership from generated metadata settings/tsconfig.json") || !strings.Contains(output, "use authored metadata") {
		t.Fatalf("automatic discovery read a generated config tree: %v\n%s", err, output)
	}
	if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
		t.Fatalf("generated config discovery changed BUILD files: %s", diff)
	}
}

func TestGeneratedVitestConfigUsesDirectFile(t *testing.T) {
	for _, producer := range []struct {
		kind, file, rule string
		deps             []string
	}{
		{"genrule", "vitest.config.mjs", `genrule(name = "config_source", outs = ["vitest.config.mjs"], cmd = "echo config > $@")`, nil},
		{"ts_codegen", "vitest.config.mts", loadDefs + `"ts_codegen")
ts_codegen(name = "config_source", outs = ["vitest.config.mts"], generator = ":generator")`, []string{":config_source"}},
	} {
		t.Run(producer.kind, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel":       "module(name = \"generated_vitest_config\")\n",
				"app/BUILD.bazel":    "# gazelle:exclude scratch.ts\n# gazelle:exclude wrangler.jsonc\n" + producer.rule + "\n",
				"app/package.json":   `{"name":"config-consumer"}`,
				"app/tsconfig.json":  `{"files":["index.test.ts"]}`,
				"app/index.test.ts":  "export {};\n",
				"app/scratch.ts":     "export const value = 1;\n",
				"app/wrangler.jsonc": `{"name":"stale"}`,
			})
			var first map[string]string
			for _, stale := range []bool{false, true} {
				if stale {
					writeFile(t, filepath.Join(root, "app", producer.file), "import './scratch'; export default { test: { poolOptions: { workers: { wrangler: { configPath: './wrangler.jsonc' } } } } };\n")
				}
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("generated config stale=%t: %v\n%s", stale, err, output)
				}
				test := onDiskRule(t, root, "app", "ts_test", "app_test")
				if got := test.AttrString("config"); got != producer.file {
					t.Errorf("config = %q, want %q", got, producer.file)
				}
				wantLabels(t, "test srcs", test.AttrStrings("srcs"), []string{"index.test.ts"})
				wantLabels(t, "test package scope", test.AttrStrings("package_scopes"), []string{"package.json"})
				wantStrings(t, "test deps", test.AttrStrings("deps"), producer.deps)
				wantLabels(t, "generated config root retains only its authored scope", test.AttrStrings("config_srcs"), []string{"package.json"})
				for _, r := range loadRules(t, root, "app") {
					if r.Kind() == "ts_compile" || r.Name() == "wrangler_config" {
						t.Fatalf("generated config acquired a compiler or stale config input: %s", r.Name())
					}
				}
				if first == nil {
					first = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("stale config changed generated inputs: %s", diff)
				}
			}
		})
	}
}

func TestGeneratedVitestConfigKeepsRuntimeOwner(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", override), func(t *testing.T) {
			build := loadDefs + `"ts_compile", "ts_test")
# gazelle:exclude vitest.config.mjs
# gazelle:exclude stale-secret.mjs
genrule(
    name = "make_config",
    outs = ["vitest.config.mjs"],
    cmd = "printf '%s\\n' \"import { value } from './helper.mjs'; export default { value };\" > $@",
)
# keep
ts_compile(name = "config_runtime", srcs = [":make_config", "helper.mjs"], emit = False)
ts_test(
    name = "app_test",
    srcs = ["index.test.ts"],
    config = "vitest.config.mjs", # keep
)
`
			wantOwner := ":config_runtime"
			if override {
				build += `# gazelle:resolve typescript app/vitest.config.mjs :override_runtime
# keep
ts_compile(name = "override_runtime", srcs = ["helper.mjs"], emit = False)
`
				wantOwner = ":override_runtime"
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel":         "module(name = \"generated_config_runtime\")\n",
				"app/BUILD.bazel":      build,
				"app/tsconfig.json":    `{"files":["index.test.ts"]}`,
				"app/index.test.ts":    "export {};\n",
				"app/helper.mjs":       "export const value = 'declared runtime';\n",
				"app/stale-secret.mjs": "export const value = 'stale checkout';\n",
			})
			var first map[string]string
			for _, stale := range []bool{false, true} {
				if stale {
					writeFile(t, filepath.Join(root, "app/vitest.config.mjs"), "import { value } from './stale-secret.mjs'; export default { value };\n")
				}
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("generated config stale=%t: %v\n%s", stale, err, output)
				}
				r := onDiskRule(t, root, "app", "ts_test", "app_test")
				if got := r.AttrString("config"); got != "vitest.config.mjs" {
					t.Fatalf("config = %q, want vitest.config.mjs", got)
				}
				wantStrings(t, "generated config runtime owner", r.AttrStrings("deps"), []string{wantOwner})
				wantStrings(t, "generated config is staged by config, not srcs", r.AttrStrings("srcs"), []string{"index.test.ts"})
				wantStrings(t, "stale config imports are not traversed", r.AttrStrings("config_srcs"), nil)
				owner := onDiskRule(t, root, "app", "ts_compile", strings.TrimPrefix(wantOwner, ":"))
				if !slices.Contains(owner.AttrStrings("srcs"), "helper.mjs") {
					t.Fatalf("config runtime owner lost helper.mjs: %v", owner.AttrStrings("srcs"))
				}
				if first == nil {
					first = buildFileBytes(t, root)
				} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("stale config changed generated inputs: %s", diff)
				}
			}
		})
	}
}

func TestExcludedProposedConfigsRespectKeptRules(t *testing.T) {
	for _, attr := range []string{"filegroup.srcs", "ts_config.src", "ts_config.deps", "ts_test.config"} {
		for _, keep := range []string{"rule", "attribute"} {
			t.Run(attr+"/"+keep, func(t *testing.T) {
				prefix, suffix := "# keep\n", ""
				if keep == "attribute" {
					prefix, suffix = "", " # keep"
				}
				tree := map[string]string{"MODULE.bazel": "module(name = \"kept_config\")\n"}
				pkg, kind, name := "", "filegroup", "vitest_config"
				switch attr {
				case "filegroup.srcs":
					tree["BUILD.bazel"] = "# gazelle:exclude vitest.config.mts\n" + prefix + "filegroup(\n    name = \"vitest_config\",\n    srcs = [\"safe.config.mts\"]," + suffix + "\n)\n"
					tree["vitest.config.mts"] = "export default {};\n"
					tree["safe.config.mts"] = "export default {};\n"
				case "ts_config.src":
					pkg, kind, name = "app", "ts_config", "tsconfig"
					tree["BUILD.bazel"] = "# gazelle:exclude app/tsconfig.json\n"
					tree["app/tsconfig.json"] = `{"files":["index.ts"]}`
					tree["app/safe.json"] = `{"files":["index.ts"]}`
					tree["app/index.ts"] = "export const value = 1;\n"
					tree["app/BUILD.bazel"] = loadDefs + `"ts_config")` + "\n" + prefix + "ts_config(\n    name = \"tsconfig\",\n    src = \"safe.json\"," + suffix + "\n)\n"
				case "ts_config.deps":
					pkg, kind, name = "app", "ts_config", "tsconfig"
					tree["BUILD.bazel"] = "# gazelle:exclude base/tsconfig.json\n"
					tree["base/BUILD.bazel"] = "# gazelle:ignore\n"
					tree["base/tsconfig.json"] = `{"compilerOptions":{"strict":true}}`
					tree["app/tsconfig.json"] = `{"extends":"../base/tsconfig.json","files":["index.ts"]}`
					tree["app/safe.json"] = `{"compilerOptions":{"strict":true}}`
					tree["app/index.ts"] = "export const value = 1;\n"
					tree["app/BUILD.bazel"] = loadDefs + `"ts_config")` + "\n" + prefix + "ts_config(\n    name = \"tsconfig\",\n    src = \"tsconfig.json\",\n    deps = [\":safe\"]," + suffix + "\n)\n" + "ts_config(name = \"safe\", src = \"safe.json\")\n"
				case "ts_test.config":
					pkg, kind, name = "app", "ts_test", "app_test"
					tree["BUILD.bazel"] = "# gazelle:exclude app/vitest.config.mts\n"
					tree["app/tsconfig.json"] = `{"files":["index.test.ts"]}`
					tree["app/index.test.ts"] = "export {};\n"
					tree["app/vitest.config.mts"] = "export default {};\n"
					tree["app/safe.config.mts"] = "export default {};\n"
					tree["app/BUILD.bazel"] = loadDefs + `"ts_test")` + "\n" + prefix + "ts_test(\n    name = \"app_test\",\n    srcs = [\"index.test.ts\"],\n    config = \"safe.config.mts\"," + suffix + "\n)\n"
				}
				root := writeTree(t, tree)
				var first map[string]string
				for pass := range 2 {
					output, err := protoGazelle(t, root)
					if err != nil {
						t.Fatalf("kept config pass %d: %v\n%s", pass, err, output)
					}
					r := onDiskRule(t, root, pkg, kind, name)
					switch attr {
					case "filegroup.srcs":
						wantStrings(t, "kept config srcs", r.AttrStrings("srcs"), []string{"safe.config.mts"})
					case "ts_config.src":
						if got := r.AttrString("src"); got != "safe.json" {
							t.Fatalf("kept config src = %q, want safe.json", got)
						}
					case "ts_config.deps":
						wantStrings(t, "kept config deps", r.AttrStrings("deps"), []string{":safe"})
					case "ts_test.config":
						if got := r.AttrString("config"); got != "safe.config.mts" {
							t.Fatalf("kept test config = %q, want safe.config.mts", got)
						}
						wantStrings(t, "kept test srcs", r.AttrStrings("srcs"), []string{"index.test.ts"})
						wantStrings(t, "self-contained config srcs", r.AttrStrings("config_srcs"), nil)
						wantStrings(t, "self-contained config deps", r.AttrStrings("deps"), nil)
					}
					if pass == 0 {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("kept config did not converge: %s", diff)
					}
				}
			})
		}
	}
}

func TestMappedProgramRulesDoNotLoseDependencies(t *testing.T) {
	for _, chained := range []bool{false, true} {
		t.Run(fmt.Sprint(chained), func(t *testing.T) {
			mapping := "# gazelle:map_kind ts_compile custom_compile //:defs.bzl\n# gazelle:map_kind ts_test custom_test //:defs.bzl\n"
			if chained {
				mapping += "# gazelle:map_kind custom_compile final_compile //:defs.bzl\n# gazelle:map_kind custom_test final_test //:defs.bzl\n"
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel":         "module(name = \"mapped_program_rules\")\n",
				"BUILD.bazel":          mapping,
				"app/tsconfig.json":    `{"compilerOptions":{"paths":{"#impl":["../late/index.ts"]}},"files":["index.ts"]}`,
				"app/index.ts":         "export { value } from '../dep/index';\n",
				"checks/tsconfig.json": `{"compilerOptions":{"paths":{"#impl":["../late/index.ts"]}},"files":["index.test.ts"]}`,
				"checks/index.test.ts": "import { value } from '../dep/index'; export { value };\n",
				"dep/tsconfig.json":    `{"compilerOptions":{"paths":{"#impl":["../impl/value.ts"]}},"files":["index.ts"]}`,
				"dep/index.ts":         "export { value } from '#impl';\n",
				"impl/value.ts":        "export const value = 42;\n",
				"late/BUILD.bazel": "load(\"//:defs.bzl\", \"wrapped_compile\")\n" +
					"# gazelle:map_kind ts_compile owner_compile //:defs.bzl\n" +
					"# gazelle:alias_kind wrapped_compile owner_compile\n" +
					"wrapped_compile(name = \"late\")\n",
				"late/tsconfig.json": `{"files":["index.ts"]}`,
				"late/index.ts":      "export { value } from '../leaf/value';\n",
				"leaf/value.ts":      "export const value = 7;\n",
			})
			var first map[string]string
			for pass, args := range [][]string{nil, {"-index=false"}, {"-r=false", "checks"}, {"-r=false", "app"}} {
				output, err := protoGazelle(t, root, args...)
				if err != nil {
					t.Fatalf("mapped rule pass %d: %v\n%s", pass, err, output)
				}
				prefix := "custom_"
				if chained {
					prefix = "final_"
				}
				late := onDiskRule(t, root, "late", "wrapped_compile", "late")
				wantLabels(t, "mapped owner supplies the borrowed leaf", late.AttrStrings("srcs"), []string{"index.ts", "//:leaf/value.ts"})
				for _, consumer := range []struct{ pkg, kind, name string }{{"app", "compile", "app"}, {"checks", "test", "checks_test"}} {
					r := onDiskRule(t, root, consumer.pkg, prefix+consumer.kind, consumer.name)
					deps, srcs := []string{"//dep", "//late"}, []string{"index.ts"}
					if consumer.kind == "test" {
						srcs = []string{"index.test.ts"}
					}
					wantLabels(t, "mapped rule retains its imported owners", r.AttrStrings("deps"), deps)
					wantLabels(t, "mapped dependency supplies its borrowed implementation", r.AttrStrings("srcs"), srcs)
				}
				if pass == 0 {
					first = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("mapped rules did not converge: %s", diff)
				}
			}
		})
	}
}

func TestKeptCompilerConfigDoesNotResolveDiscardedTypes(t *testing.T) {
	for _, kind := range []string{"ts_compile", "ts_test"} {
		for _, configuration := range []struct {
			name, selected string
			wholeRule      bool
		}{
			{"rule", ":config_alias", true},
			{"attribute", ":safe_config", false},
			{"baseline", "", true},
			{"external", "@external//:tsconfig", true},
		} {
			t.Run(kind+"/"+configuration.name, func(t *testing.T) {
				name, source := "app", "safe.ts"
				if kind == "ts_test" {
					name, source = "app_test", "safe.test.ts"
				}
				prefix, suffix := "", " # keep"
				if configuration.wholeRule {
					prefix, suffix = "# keep\n", ""
				}
				selected := configuration.selected
				if configuration.name == "attribute" && kind == "ts_test" {
					selected = "safe.json"
				}
				configAttr := ""
				if selected != "" {
					configAttr = "    tsconfig = \"" + selected + "\"," + suffix + "\n"
				}
				discardedConfig := `{"compilerOptions":{"types":["./secret.d.ts"]},"files":["` + source + `"]}`
				root := writeTree(t, map[string]string{
					"MODULE.bazel": "module(name = \"kept_compiler_types\")\n",
					"app/BUILD.bazel": loadDefs + `"ts_compile", "ts_config", "ts_test")` + "\n# gazelle:exclude secret.d.ts\n" +
						"genrule(name = \"generated_types\", outs = [\"generated.d.ts\"], cmd = \"echo declaration > $@\")\n" +
						"ts_config(name = \"safe_config\", src = \"safe.json\")\nalias(name = \"config_alias\", actual = \":safe_config\")\n" + prefix + kind + "(\n" +
						"    name = \"" + name + "\",\n    srcs = [\"" + source + "\"]," + suffix + "\n" +
						configAttr + ")\n",
					"app/tsconfig.json":       discardedConfig,
					"app/safe.json":           `{"compilerOptions":{"types":[]},"files":["ambient.ts"]}`,
					"app/" + source:           "export const safe = 1;\n",
					"app/ambient.ts":          "export {};\n",
					"app/secret.d.ts":         "declare const secret: string;\n",
					"app/required-types.d.ts": "import './secret'; export {};\n",
				})
				var first map[string]string
				for _, args := range [][]string{nil, {"-index=false"}, nil} {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("discarded types with %v: %v\n%s", args, err, output)
					}
					r := onDiskRule(t, root, "app", kind, name)
					if got := r.AttrString("tsconfig"); got != selected {
						t.Fatalf("kept config = %q, want %q", got, selected)
					}
					wantStrings(t, "kept config retains its safe source", r.AttrStrings("srcs"), []string{source})
					wantStrings(t, "discarded types add no dependency", r.AttrStrings("deps"), nil)
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("kept compiler config did not converge: %s", diff)
					}
				}
				before := convergeSnapshot(t, root)
				writeFile(t, filepath.Join(root, "app/tsconfig.json"), strings.Replace(discardedConfig, "secret.d.ts", "generated.d.ts", 1))
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("discarded generated type acquired a compiler requirement: %v\n%s", err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("discarded generated type changed BUILD files: %s", diff)
				}
				writeFile(t, filepath.Join(root, "app/tsconfig.json"), discardedConfig)
				var requiredTypes []string
				if configuration.name == "rule" || configuration.name == "attribute" {
					requiredTypes = []string{"source import", "./secret.d.ts", "./required-types.d.ts"}
				}
				for _, required := range requiredTypes {
					if required == "source import" {
						writeFile(t, filepath.Join(root, "app", source), "import './secret'; export const safe = 1;\n")
					} else {
						writeFile(t, filepath.Join(root, "app", source), "export const safe = 1;\n")
						writeFile(t, filepath.Join(root, "app/safe.json"), `{"compilerOptions":{"types":["`+required+`"]},"files":["ambient.ts"]}`)
					}
					output, err := protoGazelle(t, root)
					if err == nil || !strings.Contains(output, "app/secret.d.ts") || !strings.Contains(output, "excluded or ignored") {
						t.Fatalf("required %s bypassed validation: %v\n%s", required, err, output)
					}
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("rejected %s changed BUILD files: %s", required, diff)
					}
				}
			})
		}
	}
}

func TestUnobservedCompilerConfigCannotEraseExistingDependency(t *testing.T) {
	for _, kind := range []string{"ts_compile", "ts_test"} {
		name, source := "app", "index.ts"
		if kind == "ts_test" {
			name, source = "app_test", "index.test.ts"
		}
		for _, selection := range []struct{ name, config, sources string }{
			{"@external//:tsconfig", "@external//:tsconfig", ""},
			{"generated.json", "generated.json", ""},
			{":generated_config", ":generated_config", ""},
			{":opaque_config", ":opaque_config", ""},
			{"missing.json", "missing.json", ""},
			{"authored config with glob", "tsconfig.json", `glob(["*.ts"])`},
			{"authored config with select", "tsconfig.json", `select({"//conditions:default": ["` + source + `"]})`},
		} {
			t.Run(kind+"/"+selection.name, func(t *testing.T) {
				selected, sources := selection.config, selection.sources
				sourceKeep := ""
				reason := "selected tsconfig \"" + selected + "\""
				if sources == "" {
					sources = `["` + source + `"]`
				} else {
					sourceKeep = " # keep"
					reason = "unsupported srcs membership"
				}
				build := loadDefs + `"ts_compile", "ts_config", "ts_test")
genrule(name = "config_source", outs = ["generated.json"], cmd = "echo '{}' > $@")
ts_config(name = "generated_config", src = "generated.json")
filegroup(name = "opaque_config", srcs = ["selected.json"])
` + kind + `(
    name = "` + name + `",
    srcs = ` + sources + `,` + sourceKeep + `
    tsconfig = "` + selected + `", # keep
    deps = ["//lib"],
)
`
				root := writeTree(t, map[string]string{
					"MODULE.bazel":      "module(name = \"unobserved_compiler\")\n",
					"BUILD.bazel":       "",
					"app/BUILD.bazel":   build,
					"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["` + source + `"]}`,
					"app/selected.json": `{"compilerOptions":{"types":[]}}`,
					"app/" + source:     "import { value } from '../lib/value.js'; export const answer = value;\n",
					"lib/BUILD.bazel":   loadDefs + "\"ts_compile\")\n# keep\nts_compile(name = \"lib\", srcs = [\"value.ts\"], visibility = [\"//visibility:public\"])\n",
					"lib/value.ts":      "export const value = 42;\n",
				})
				before := buildFileBytes(t, root)
				wantSources := bzl.FormatString(onDiskRule(t, root, "app", kind, name).Attr("srcs"))
				for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "app"}} {
					output, err := protoGazelle(t, root, args...)
					if err == nil || !strings.Contains(output, label.New("unobserved_compiler", "app", name).String()+" cannot discover its compiler closure") || !strings.Contains(output, reason) || !strings.Contains(output, "keep the whole rule") {
						t.Fatalf("unobserved compiler inputs published dependencies with %v: %v\n%s", args, err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("rejected selected config changed BUILD files: %s", diff)
					}
				}
				if output, err := protoGazelle(t, root, "-r=false", "lib"); err != nil {
					t.Fatalf("untouched unobserved compiler blocked another package: %v\n%s", err, output)
				}
				if got := buildFileBytes(t, root)["app/BUILD.bazel"]; got != before["app/BUILD.bazel"] {
					t.Fatalf("unobserved compiler outside the update changed:\n%s", got)
				}
				for _, manual := range []string{"rule", "attributes"} {
					kept := strings.Replace(build, kind+"(\n", "# keep\n"+kind+"(\n", 1)
					if manual == "attributes" {
						kept = strings.Replace(build, "srcs = "+sources+","+sourceKeep, "srcs = "+sources+", # keep", 1)
						kept = strings.Replace(kept, "deps = [\"//lib\"],", "deps = [\"//lib\"], # keep\n    type_inputs = [], # keep\n    package_scopes = [], # keep", 1)
					}
					writeFile(t, filepath.Join(root, "app/BUILD.bazel"), kept)
					for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "app"}} {
						output, err := protoGazelle(t, root, args...)
						if err != nil {
							t.Fatalf("kept %s did not retain manual closure with %v: %v\n%s", manual, args, err, output)
						}
						r := onDiskRule(t, root, "app", kind, name)
						wantLabels(t, "manual dependency survives unobserved compiler inputs", r.AttrStrings("deps"), []string{"//lib"})
						if got := bzl.FormatString(r.Attr("srcs")); got != wantSources {
							t.Fatalf("manual sources changed: got %s, want %s", got, wantSources)
						}
					}
				}
				if kind == "ts_compile" && selection.sources != "" {
					writeFile(t, filepath.Join(root, "app/index.test.ts"), "export {};\n")
					writeFile(t, filepath.Join(root, "app/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts","index.test.ts"]}`)
					joined := strings.Replace(build, "ts_compile(\n", "# keep\nts_compile(\n", 1) + `
ts_test(name = "app_test", srcs = ["index.test.ts"], tsconfig = "tsconfig.json")
`
					writeFile(t, filepath.Join(root, "app/BUILD.bazel"), joined)
					before := buildFileBytes(t, root)
					for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "app"}} {
						output, err := protoGazelle(t, root, args...)
						if err == nil || !strings.Contains(output, label.New("unobserved_compiler", "app", "app_test").String()+" cannot discover its compiler closure") || !strings.Contains(output, "unsupported srcs membership") {
							t.Fatalf("managed test accepted incomplete joined library with %v: %v\n%s", args, err, output)
						}
						if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("rejected joined library changed BUILD files: %s", diff)
						}
					}
				}
			})
		}
	}
}

func TestKeptUnobservedCompilerConfigDoesNotResolveDirectoryImports(t *testing.T) {
	for _, kind := range []string{"ts_compile", "ts_test"} {
		for _, selected := range []string{"", "@external//:tsconfig"} {
			t.Run(kind+"/"+selected, func(t *testing.T) {
				name, source, value := "app", "index.ts", "value.ts"
				if kind == "ts_test" {
					name, source, value = "app_test", "index.test.ts", "value.test.ts"
				}
				native := strings.TrimSuffix(value, ".ts") + ".native.ts"
				build := loadDefs + `"ts_compile", "ts_test")` + "\n# gazelle:exclude " + native + "\n" +
					"# keep\nts_compile(name = \"authored\", srcs = [\"authored.d.ts\"])\n# keep\n" + kind + "(\n" +
					"    name = \"" + name + "\",\n    srcs = [\"" + source + "\", \"" + value + "\"],\n" +
					"    deps = [\":authored\"],\n"
				configAttr := ""
				if selected != "" {
					configAttr = "    tsconfig = \"" + selected + "\",\n"
				}
				root := writeTree(t, map[string]string{
					"MODULE.bazel":      "module(name = \"kept_unobserved_config\")\n",
					"app/BUILD.bazel":   build + configAttr + ")\n",
					"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","moduleSuffixes":[".native",""]},"files":["` + source + `"]}`,
					"app/" + source:     "export { value } from './" + strings.TrimSuffix(value, ".ts") + ".js';\n",
					"app/" + value:      "export const value = 1;\n",
					"app/" + native:     "export const value = 2;\n",
					"app/authored.d.ts": "export interface Authored {}\n",
				})
				var first map[string]string
				for _, args := range [][]string{nil, {"-index=false", "app"}, nil} {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("discarded directory import with %v: %v\n%s", args, err, output)
					}
					r := onDiskRule(t, root, "app", kind, name)
					if got := r.AttrString("tsconfig"); got != selected {
						t.Fatalf("kept config = %q, want %q", got, selected)
					}
					wantStrings(t, "authored sources survive discarded resolution", r.AttrStrings("srcs"), []string{source, value})
					wantStrings(t, "authored dependencies survive discarded resolution", r.AttrStrings("deps"), []string{":authored"})
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("unobserved compiler config did not converge: %s", diff)
					}
				}
				writeFile(t, filepath.Join(root, "app/BUILD.bazel"), build+"    tsconfig = \":tsconfig\",\n)\n")
				before := convergeSnapshot(t, root)
				for _, args := range [][]string{{"-index=false", "app"}, nil} {
					output, err := protoGazelle(t, root, args...)
					if err == nil || !strings.Contains(output, "app/"+source+" imports app/"+native) || !strings.Contains(output, "excluded or ignored") {
						t.Fatalf("selected suffix config bypassed exclusion with %v: %v\n%s", args, err, output)
					}
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("rejected selected suffix config changed BUILD files: %s", diff)
					}
				}
			})
		}
	}
}

func TestTestConfigDoesNotDropSourceModeClosureOrTraverseEmittedLibrary(t *testing.T) {
	for _, boundary := range []struct {
		name, emit, trigger, override, alias           string
		source, external, unknown, manual, declaration bool
	}{
		{name: "default source", source: true},
		{name: "explicit source owner", source: true, override: "//app:app"},
		{name: "qualified source owner", source: true, override: "@//app:app"},
		{name: "canonical source owner", source: true, override: "@@//app:app"},
		{name: "named alias source owner retains closure", source: true, override: "//app:source_alias", alias: "@test_config_boundary//app:app"},
		{name: "canonical alias source owner", source: true, override: "//app:source_alias", alias: "@@//app:app"},
		{name: "external owner stays external", source: true, override: "@external//app:app", external: true},
		{name: "canonical alias matching apparent name stays external", source: true, override: "//app:source_alias", alias: "@@test_config_boundary//app:app", external: true},
		{name: "kept source", emit: "False", source: true},
		{name: "kept emitted", emit: "True"},
		{name: "unknown source boundary cannot drop closure", emit: `select({"//conditions:default": False})`, unknown: true},
		{name: "unknown emitted boundary cannot claim closure", emit: `select({"//conditions:default": True})`, unknown: true},
		{name: "emitted owner preserves declaration closure", emit: "True", declaration: true},
		{name: "unknown owner preserves declaration closure", emit: `select({"//conditions:default": False})`, unknown: true, declaration: true},
		{name: "whole kept consumer owns unknown boundary", emit: `select({"//conditions:default": False})`, unknown: true, manual: true},
		{name: "explicit emitted owner", emit: "True", override: "//app:app"},
		{name: "binary requires emission", trigger: "binary"},
		{name: "manifest requires emission", trigger: "manifest"},
		{name: "node runner promotes newly discovered owner", emit: "False", trigger: "node", source: true},
	} {
		configs := []string{"test.json"}
		if boundary.emit == "True" && !boundary.declaration {
			configs = append(configs, "tsconfig.json", ":library_config_alias")
		}
		for _, selected := range configs {
			t.Run(boundary.name+"/"+selected, func(t *testing.T) {
				t.Parallel()
				latePromotion := boundary.trigger == "node" && selected == "test.json"
				sourceClosure := boundary.declaration || boundary.source && !boundary.external && boundary.trigger != "node" && selected == "test.json"
				exclusions := "# gazelle:exclude fixtures/unrelated.ts\n"
				if !sourceClosure && !boundary.unknown {
					exclusions += "# gazelle:exclude fixtures/value.ts\n"
				}
				rootBuild := exclusions
				if boundary.override != "" {
					rootBuild += "# gazelle:resolve typescript app/index.ts " + boundary.override + "\n"
				}
				if boundary.trigger == "binary" {
					rootBuild += loadDefs + `"ts_binary")` + "\nts_binary(name = \"run\", entry_point = \"//app\")\n"
				}
				libraryEmit := ""
				if boundary.emit != "" {
					libraryEmit = ", emit = " + boundary.emit + ", # keep\n"
				}
				runner := ""
				if boundary.trigger == "node" {
					runner = "runner = \"@rules_typescript//ts/runners:node_test\", "
				}
				tree := map[string]string{
					"MODULE.bazel": "module(name = \"test_config_boundary\")\n",
					"BUILD.bazel":  rootBuild,
					"app/BUILD.bazel": loadDefs + `"ts_compile", "ts_test")` + "\n" +
						"alias(name = \"library_config_alias\", actual = \":tsconfig\")\n" +
						"ts_compile(name = \"app\"" + libraryEmit + ")\n" +
						"ts_test(name = \"app_test\", " + runner + "tsconfig = \"" + selected + "\", # keep\n)\n",
					"app/tsconfig.json":     `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#value":["../dep/index.js"],"#unrelated":["../dep/index.js"]},"types":[]},"files":["index.ts","index.test.ts","unrelated.ts"]}`,
					"app/test.json":         `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#value":["../fixtures/value.js"],"#unrelated":["../fixtures/unrelated.js"]},"types":[]},"files":["index.test.ts"]}`,
					"app/index.ts":          "import { value } from '#value'; export const answer: number = value;\n",
					"app/index.test.ts":     "import { answer } from './index.js'; export const actual: number = answer;\n",
					"app/unrelated.ts":      "export { value } from '#unrelated';\n",
					"dep/tsconfig.json":     `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
					"dep/index.ts":          "export const value: number = 42;\n",
					"fixtures/BUILD.bazel":  `exports_files(["value.ts", "leaf.ts", "unrelated.ts"], visibility = ["//app:__pkg__"])`,
					"fixtures/value.ts":     "export { value } from './leaf.js';\n",
					"fixtures/leaf.ts":      "export const value: number = 0;\n",
					"fixtures/unrelated.ts": "export const value: number = -1;\n",
				}
				if boundary.trigger == "manifest" {
					tree["app/package.json"] = `{"name":"test-config-boundary","type":"module","main":"./index.js"}`
				}
				if boundary.alias != "" {
					tree["app/BUILD.bazel"] += "alias(name = \"source_alias\", actual = \"" + boundary.alias + "\")\n"
				}
				if boundary.trigger == "node" {
					tree["app/test.json"] = strings.Replace(tree["app/test.json"], "../fixtures/value.js", "../extra/index.js", 1)
					tree["extra/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#unrelated":["../dep/index.js"]},"types":[]},"files":["index.ts"]}`
					tree["extra/index.ts"] = "export { value } from '#unrelated';\n"
				}
				if latePromotion {
					tree["checks/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#unrelated":["../fixtures/unrelated.js"]},"types":[]},"files":["index.test.ts"]}`
					tree["checks/index.test.ts"] = "import { value } from '../extra/index.js'; export const actual: number = value;\n"
				}
				if sourceClosure && !boundary.declaration {
					tree["app/index.ts"] += "export { supplied } from '../private/supplied.js';\n"
					tree["private/BUILD.bazel"] = `exports_files(["supplied.ts"], visibility = ["//app:__pkg__"])`
					tree["private/supplied.ts"] = "export const supplied = 1;\n"
				}
				libraryRoot := "index.ts"
				if boundary.trigger == "node" {
					libraryRoot = "index.mjs"
					tree["app/index.mjs"] = strings.Replace(tree["app/index.ts"], "answer: number", "answer", 1)
					tree["app/unrelated.mjs"] = tree["app/unrelated.ts"]
					delete(tree, "app/index.ts")
					delete(tree, "app/unrelated.ts")
					tree["app/index.test.ts"] = strings.Replace(tree["app/index.test.ts"], "./index.js", "./index.mjs", 1)
					for _, cfg := range []string{"app/tsconfig.json", "app/test.json"} {
						tree[cfg] = strings.Replace(tree[cfg], `"compilerOptions":{`, `"compilerOptions":{"allowJs":true,`, 1)
						tree[cfg] = strings.Replace(tree[cfg], `"index.ts"`, `"index.mjs"`, 1)
						tree[cfg] = strings.Replace(tree[cfg], `"unrelated.ts"`, `"unrelated.mjs"`, 1)
					}
				}
				if boundary.declaration {
					libraryRoot = "index.d.ts"
					tree["app/index.d.ts"] = strings.Replace(tree["app/index.ts"], "export const answer: number = value;", "export declare const answer: typeof value;", 1)
					delete(tree, "app/index.ts")
					tree["app/tsconfig.json"] = strings.Replace(tree["app/tsconfig.json"], "index.ts", libraryRoot, 1)
				}
				if boundary.manual {
					tree["app/BUILD.bazel"] = strings.Replace(tree["app/BUILD.bazel"], "ts_test(name = \"app_test\", ", "# keep\nts_test(name = \"app_test\", srcs = [\"index.test.ts\", \"//fixtures:value.ts\", \"//fixtures:leaf.ts\"], deps = [\":app\"], ", 1)
				}
				root := writeTree(t, tree)
				before := convergeSnapshot(t, root)
				var first map[string]string
				for _, args := range [][]string{nil, {"-index=false", "-r=false", "app"}, nil} {
					output, err := protoGazelle(t, root, args...)
					if boundary.unknown && !boundary.manual && !boundary.declaration {
						if err == nil || !strings.Contains(output, "whose emit value is an expression") || !strings.Contains(output, "keep the whole consuming rule") {
							t.Fatalf("unknown emission pruned a required implementation boundary with %v: %v\n%s", args, err, output)
						}
						if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("unknown emission changed BUILD files before refusal: %s", diff)
						}
						continue
					}
					if err != nil {
						t.Fatalf("test config boundary with %v: %v\n%s", args, err, output)
					}
					library := onDiskRule(t, root, "app", "ts_compile", "app")
					wantStrings(t, "library implementation dependency", library.AttrStrings("deps"), []string{"//dep"})
					emit, explicit := library.Attr("emit").(*bzl.Ident)
					if !boundary.source && !boundary.unknown && (!explicit || emit.Name != "True") {
						t.Fatalf("library lost its required emission: %s", buildFileText(t, root, "app"))
					}
					if boundary.source && library.Attr("emit") != nil && (!explicit || emit.Name != "False") {
						t.Fatalf("source-mode library acquired emission: %s", buildFileText(t, root, "app"))
					}
					if sourceClosure {
						want := []string{libraryRoot, "test.json", "unrelated.ts"}
						if !boundary.declaration {
							want = append(want, "//private:supplied.ts")
						}
						wantLabels(t, "library supplies its private inputs", library.AttrStrings("srcs"), want)
					}
					test := onDiskRule(t, root, "app", "ts_test", "app_test")
					sources := []string{"index.test.ts"}
					if boundary.declaration {
						sources = append(sources, libraryRoot)
					}
					if sourceClosure || boundary.manual {
						sources = append(sources, "//fixtures:value.ts", "//fixtures:leaf.ts")
					}
					wantLabels(t, "test retains its compiler inputs", test.AttrStrings("srcs"), sources)
					if got := test.AttrString("tsconfig"); got != selected {
						t.Fatalf("test config = %q, want %q", got, selected)
					}
					deps := []string{":app"}
					if boundary.alias != "" {
						deps = append(deps, ":source_alias")
					}
					if boundary.external && boundary.alias == "" {
						deps = append(deps, boundary.override)
					}
					if selected != "test.json" {
						deps = append(deps, "//dep")
					}
					if latePromotion {
						deps = append(deps, "//extra")
						extra := onDiskRule(t, root, "extra", "ts_compile", "extra")
						if emit, literal := extra.Attr("emit").(*bzl.Ident); !literal || emit.Name != "True" {
							t.Fatalf("new test dependency was not emitted in one run: %s", buildFileText(t, root, "extra"))
						}
						unrooted := onDiskRule(t, root, "checks", "ts_test", "checks_test")
						wantLabels(t, "unrooted test observes the promoted owner", unrooted.AttrStrings("deps"), []string{"//extra"})
						wantStrings(t, "unrooted test does not read emitted implementation", unrooted.AttrStrings("srcs"), []string{"index.test.ts"})
					}
					wantLabels(t, "test program dependency boundary", test.AttrStrings("deps"), deps)
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("test config boundary did not converge: %s", diff)
					}
				}
				if boundary.unknown {
					return
				}
				if sourceClosure {
					writeFile(t, filepath.Join(root, "BUILD.bazel"), rootBuild+"# gazelle:exclude fixtures/leaf.ts\n")
					before := convergeSnapshot(t, root)
					for _, args := range [][]string{{"-r=false", "app"}, nil} {
						output, err := protoGazelle(t, root, args...)
						if err == nil || !strings.Contains(output, "fixtures/value.ts imports fixtures/leaf.ts") || !strings.Contains(output, "excluded or ignored") {
							t.Fatalf("source-mode library's test-config closure bypassed exclusion with %v: %v\n%s", args, err, output)
						}
						if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("rejected library implementation input changed BUILD files: %s", diff)
						}
					}
					writeFile(t, filepath.Join(root, "BUILD.bazel"), rootBuild)
				}
				writeFile(t, filepath.Join(root, "app/index.test.ts"), "import '../fixtures/unrelated.js'; export {};\n")
				before = convergeSnapshot(t, root)
				for _, args := range [][]string{{"-r=false", "app"}, nil} {
					output, err := protoGazelle(t, root, args...)
					if err == nil || !strings.Contains(output, "app/index.test.ts imports fixtures/unrelated.ts") || !strings.Contains(output, "excluded or ignored") {
						t.Fatalf("test's own excluded input bypassed validation with %v: %v\n%s", args, err, output)
					}
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("rejected test input changed BUILD files: %s", diff)
					}
				}
			})
		}
	}
}

func TestSourceModeDependencyDoesNotExposePrivateInputs(t *testing.T) {
	for _, input := range []struct {
		extension, emit string
	}{
		{extension: "json"},
		{extension: "d.ts"},
		{extension: "ts"},
		{extension: "js"},
		{extension: "ts", emit: "False"},
		{extension: "ts", emit: "True"},
	} {
		extension := input.extension
		compiler := input.emit != ""
		for _, consumer := range []string{"borrowed", "direct", "remapped"} {
			t.Run(fmt.Sprintf("%s/emit=%s/%s", extension, input.emit, consumer), func(t *testing.T) {
				name := "value." + extension
				config := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"resolveJsonModule":true,"paths":{"#input":["../private/` + name + `"]},"types":[]},"include":["*.ts"]}`
				imported := "import { value } from '#input'; export const actual: number = value;\n"
				content := "export const value = 42;\n"
				if extension == "d.ts" {
					content = "export declare const value: number;\n"
				}
				if extension == "json" {
					imported = "import value from '#input'; export const actual: number = value.answer;\n"
					content = `{"answer":42}`
				}
				producer := loadDefs + `"ts_codegen")
ts_codegen(name = "input", outs = ["` + name + `"], generator = "//:generator", visibility = ["//lib:__pkg__"])
`
				if compiler {
					producer = loadDefs + `"ts_compile")
# keep
ts_compile(name = "input", srcs = ["` + name + `"], emit = ` + input.emit + `, visibility = ["//lib:__pkg__"])
`
				}
				tree := map[string]string{
					"MODULE.bazel":           "module(name = \"private_input\")\n",
					"BUILD.bazel":            "",
					"lib/tsconfig.json":      config,
					"lib/index.ts":           imported,
					"consumer/tsconfig.json": config,
					"consumer/index.test.ts": "import { actual as libraryActual } from '../lib/index.js'; export const result: number = libraryActual;\n",
					"private/BUILD.bazel":    producer,
				}
				if consumer == "direct" {
					tree["consumer/index.test.ts"] += imported
				}
				if consumer == "remapped" {
					tree["consumer/tsconfig.json"] = strings.Replace(config, "../private/", "../other/", 1)
					tree["other/BUILD.bazel"] = strings.Replace(producer, "//lib:__pkg__", "//consumer:__pkg__", 1)
				}
				root := writeTree(t, tree)
				var cold map[string]string
				for _, present := range []bool{false, true} {
					if present || compiler {
						writeFile(t, filepath.Join(root, "private", name), content)
						if consumer == "remapped" {
							writeFile(t, filepath.Join(root, "other", name), content)
						}
					}
					output, err := protoGazelle(t, root)
					if err != nil {
						t.Fatalf("private input present=%t: %v\n%s", present || compiler, err, output)
					}
					library := onDiskRule(t, root, "lib", "ts_compile", "lib")
					librarySources := []string{"index.ts"}
					if !compiler && (extension == "ts" || extension == "js") {
						librarySources = append(librarySources, "//private:"+name)
					}
					wantLabels(t, "library retains only unowned compiler inputs", library.AttrStrings("srcs"), librarySources)
					wantLabels(t, "library retains its private dependency", library.AttrStrings("deps"), []string{"//private:input"})
					test := onDiskRule(t, root, "consumer", "ts_test", "consumer_test")
					deps := []string{"//lib"}
					sources := []string{"index.test.ts"}
					if consumer == "direct" {
						deps = append(deps, "//private:input")
					}
					if consumer == "remapped" {
						deps = append(deps, "//other:input")
						if !compiler && (extension == "ts" || extension == "js") {
							sources = append(sources, "//other:"+name)
						}
					}
					wantLabels(t, "consumer retains only its required dependencies", test.AttrStrings("deps"), deps)
					wantLabels(t, "consumer retains unsupplied compiler inputs", test.AttrStrings("srcs"), sources)
					if cold == nil {
						cold = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(cold, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("input ownership did not converge: %s", diff)
					}
				}
			})
		}
	}
}

func TestWorkspaceMemberRetainsConsumerSourceResolution(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":               "module(name = \"member_source_boundary\")\n",
		"BUILD.bazel":                "",
		"package.json":               `{"private":true}`,
		"node_modules/.modules.yaml": "layoutVersion: 5\n",
		pnpmLockfileName: `lockfileVersion: '9.0'
importers:
  .: {}
  lib:
    dependencies:
      shared:
        specifier: workspace:*
        version: link:../packages/shared
  packages/shared: {}
`,
		"lib/BUILD.bazel": loadDefs + `"node_modules_member")
node_modules_member(name = "node_modules/shared", member = "@npm//:shared", visibility = ["//lib:__pkg__"]) # keep
`,
		"lib/tsconfig.json":             `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"paths":{"shared":["../packages/shared/index.ts"],"#schema":["../packages/shared/schema.ts"]}},"files":["index.ts"]}`,
		"lib/index.ts":                  "export type { Result } from 'shared';\n",
		"packages/shared/package.json":  `{"name":"shared","exports":"./index.ts"}`,
		"packages/shared/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"paths":{"#schema":["./schema.ts"]}},"include":["*.ts"]}`,
		"packages/shared/index.ts":      "import type { Value } from '#schema'; import type { Local } from './local.js'; export type Result = Value & Local;\n",
		"packages/shared/schema.ts":     "export interface Value { member: string }\n",
		"packages/shared/local.ts":      "export interface Local { local: number }\n",
		"consumer/tsconfig.json":        `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"paths":{"shared":["../packages/shared/index.ts"],"#schema":["../fixtures/consumer-schema.ts"]}},"files":["index.ts"]}`,
		"consumer/index.ts":             "import type { Result } from '../lib/index.js'; export type Actual = Result;\n",
		"fixtures/consumer-schema.ts":   "export interface Value { consumer: boolean }\n",
		"fixtures/BUILD.bazel":          `exports_files(["consumer-schema.ts"], visibility = ["//consumer:__pkg__"])`,
	})
	output, err := protoGazelle(t, root)
	if err != nil {
		t.Fatalf("linked source member: %v\n%s", err, output)
	}
	member := onDiskRule(t, root, "packages/shared", "ts_compile", "shared")
	wantLabels(t, "member retains its own resolution", member.AttrStrings("srcs"), []string{"index.ts", "local.ts", "schema.ts"})
	wantLabels(t, "member retains its package metadata", member.AttrStrings("package_scopes"), []string{"package.json"})
	if emit, literal := member.Attr("emit").(*bzl.Ident); member.Attr("emit") != nil && (!literal || emit.Name != "False") {
		t.Fatal("member must supply original sources and package scope to the consumer")
	}
	library := onDiskRule(t, root, "lib", "ts_compile", "lib")
	wantLabels(t, "library keeps its own member import", library.AttrStrings("deps"), []string{":node_modules/shared"})
	link := onDiskRule(t, root, "lib", "node_modules_member", "node_modules/shared")
	wantStrings(t, "library link remains private", link.AttrStrings("visibility"), []string{"//lib:__pkg__"})
	consumer := onDiskRule(t, root, "consumer", "ts_compile", "consumer")
	wantLabels(t, "linked source retains consumer-specific input", consumer.AttrStrings("srcs"), []string{"index.ts", "//fixtures:consumer-schema.ts"})
	wantLabels(t, "consumer retains original member sources without the private npm link", consumer.AttrStrings("deps"), []string{"//lib", "//packages/shared"})
	if got := consumer.AttrString("tsconfig"); got != ":tsconfig" {
		t.Fatalf("consumer resolution context = %q, want :tsconfig", got)
	}

	writeFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude fixtures/consumer-schema.ts\n")
	memberBuild := loadDefs + `"ts_compile")
ts_compile(name = "shared", emit = True, # keep
)
`
	writeFile(t, filepath.Join(root, "packages/shared/BUILD.bazel"), memberBuild)
	output, err = protoGazelle(t, root)
	if err != nil {
		t.Fatalf("emitted member traversed consumer-only implementation input: %v\n%s", err, output)
	}
	consumer = onDiskRule(t, root, "consumer", "ts_compile", "consumer")
	wantStrings(t, "emitted member stops source traversal", consumer.AttrStrings("srcs"), []string{"index.ts"})
	wantLabels(t, "emitted member keeps the library ownership boundary", consumer.AttrStrings("deps"), []string{"//lib"})
	library = onDiskRule(t, root, "lib", "ts_compile", "lib")
	wantLabels(t, "emitted member retains npm identity", library.AttrStrings("deps"), []string{":node_modules/shared"})

	writeFile(t, filepath.Join(root, "packages/shared/BUILD.bazel"), strings.Replace(memberBuild, "True", "False", 1))
	before := convergeSnapshot(t, root)
	output, err = protoGazelle(t, root, "-r=false", "consumer")
	if err == nil || !strings.Contains(output, "packages/shared/index.ts imports fixtures/consumer-schema.ts") || !strings.Contains(output, "excluded or ignored") {
		t.Fatalf("source member bypassed consumer input exclusion: %v\n%s", err, output)
	}
	if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
		t.Fatalf("rejected member input changed BUILD files: %s", diff)
	}
}

func TestWorkspaceMemberRejectsEscapingSourceModeAndAuthoredDeclarationImports(t *testing.T) {
	for _, placement := range []string{"linked foreign", "unknown member emission cannot authorize declaration relocation", "unlinked foreign", "linked local", "linked test", "standalone", "emitted implementation selects checker input", "linked authored declaration"} {
		t.Run(placement, func(t *testing.T) {
			const imported = "export type { Options } from '../../../types/api.js';\n"
			tree := map[string]string{
				"MODULE.bazel":               "module(name = \"member_declarations\")\n",
				"BUILD.bazel":                "",
				"package.json":               `{"private":true}`,
				"node_modules/.modules.yaml": "layoutVersion: 5\n",
				pnpmLockfileName: `lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      shared:
        specifier: workspace:*
        version: link:packages/shared
  packages/shared: {}
`,
				"packages/shared/package.json":  `{"name":"shared","exports":"./src/index.ts"}`,
				"packages/shared/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["src/**/*.ts"]}`,
				"packages/shared/src/index.ts":  imported,
				"types/api.d.ts":                "export interface Options { retries: number }\n",
				"types/BUILD.bazel":             `exports_files(["api.d.ts"], visibility = ["//packages/shared:__pkg__"])`,
			}
			wantSources := []string{"src/index.ts", "//types:api.d.ts"}
			switch placement {
			case "unknown member emission cannot authorize declaration relocation":
				tree["packages/shared/BUILD.bazel"] = loadDefs + `"ts_compile")
ts_compile(name = "shared", emit = select({"//conditions:default": False}), # keep
)
`
			case "unlinked foreign":
				tree[pnpmLockfileName] = "lockfileVersion: '9.0'\nimporters:\n  .: {}\n  packages/shared: {}\n"
			case "linked local":
				tree["packages/shared/src/index.ts"] = strings.Replace(imported, "../../../types/api.js", "./api.js", 1)
				tree["packages/shared/src/api.d.ts"] = tree["types/api.d.ts"]
				wantSources = []string{"src/index.ts", "src/api.d.ts"}
			case "linked test":
				tree["packages/shared/src/index.ts"] = "export const value = 1;\n"
				tree["packages/shared/src/index.test.ts"] = imported
				wantSources = []string{"src/index.ts"}
			case "linked authored declaration":
				tree["packages/shared/src/index.ts"] = "export type { Options } from './public.js';\n"
				tree["packages/shared/src/public.d.ts"] = imported
			case "emitted implementation selects checker input":
				tree["packages/shared/src/index.ts"] = "import type { Options } from '../../../types/api.js';\nconst options: Options = { retries: 42 };\nexport const answer: number = options.retries;\n"
				tree["packages/shared/package.json"] = `{"name":"shared","exports":"./src/index.js","types":"./src/index.d.ts"}`
				tree["packages/shared/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","rootDir":"src","outDir":"dist","types":[]},"include":["src/**/*.ts"]}`
				tree["consumer/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"paths":{"shared":["../packages/shared/src/index.ts"]}},"files":["index.ts"]}`
				tree["consumer/index.ts"] = "import { answer } from 'shared'; export const value: number = answer;\n"
			case "standalone":
				delete(tree, "packages/shared/src/index.ts")
				delete(tree, "packages/shared/tsconfig.json")
				tree["packages/shared/package.json"] = `{"name":"shared","types":"./api.d.ts"}`
				tree["packages/shared/BUILD.bazel"] = loadDefs + `"ts_compile")
# keep
ts_compile(name = "shared", srcs = ["package.json", "//types:api.d.ts"], visibility = ["//visibility:public"])
`
				wantSources = []string{"package.json", "//types:api.d.ts"}
			}
			root := writeTree(t, tree)
			before := convergeSnapshot(t, root)
			updates := [][]string{nil}
			if placement == "linked foreign" || placement == "unknown member emission cannot authorize declaration relocation" {
				updates = append(updates, []string{"-r=false", "packages/shared"})
			} else if placement == "emitted implementation selects checker input" {
				updates = append(updates, []string{"-r=false", "packages/shared"}, nil)
			}
			for _, args := range updates {
				output, err := protoGazelle(t, root, args...)
				if placement == "linked foreign" || placement == "linked authored declaration" || placement == "unknown member emission cannot authorize declaration relocation" {
					importer := "index.ts"
					if placement == "linked authored declaration" {
						importer = "public.d.ts"
					}
					if err == nil || !strings.Contains(output, "imports foreign declaration types/api.d.ts from packages/shared/src/"+importer) ||
						!strings.Contains(output, "Did you mean to move the declaration inside the member or import it through a separately published package?") {
						t.Fatalf("linked member did not reject declaration relocation with %v: %v\n%s", args, err, output)
					}
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("rejected member layout changed BUILD files: %s", diff)
					}
					continue
				}
				if err != nil {
					t.Fatalf("supported declaration placement with %v: %v\n%s", args, err, output)
				}
				member := onDiskRule(t, root, "packages/shared", "ts_compile", "shared")
				wantLabels(t, "member declaration inputs", member.AttrStrings("srcs"), wantSources)
				if placement == "emitted implementation selects checker input" {
					if !strings.Contains(buildFileText(t, root, "packages/shared"), "emit = True") {
						t.Fatal("published JavaScript entry did not require emission")
					}
					consumer := onDiskRule(t, root, "consumer", "ts_compile", "consumer")
					wantLabels(t, "bare consumer uses the published member", consumer.AttrStrings("deps"), []string{"//:node_modules/shared"})
				}
				if placement == "linked test" {
					test := onDiskRule(t, root, "packages/shared", "ts_test", "shared_test")
					wantLabels(t, "test retains its foreign declaration", test.AttrStrings("srcs"), []string{"src/index.test.ts", "//types:api.d.ts"})
					wantLabels(t, "test retains compiler scope", test.AttrStrings("type_inputs"), []string{"//:package.json"})
					wantLabels(t, "library supplies the test's package manifest", test.AttrStrings("deps"), []string{":shared"})
				}
			}
		})
	}
}

func TestSourceModeDependencyDoesNotExposeItsNpmImporter(t *testing.T) {
	for _, kind := range []string{"ts_compile", "ts_test"} {
		for _, use := range []string{"type", "value"} {
			for _, ownerPaths := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/ownerPaths_%t", kind, use, ownerPaths), func(t *testing.T) {
					config := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["*.ts"]}`
					entry, name := "index.ts", "consumer"
					if kind == "ts_test" {
						entry, name = "index.test.ts", "consumer_test"
					}
					library := "import type { Value } from 'nested-value'; export type Result = Value;\n"
					source := "import type { Result } from '../lib/index.js'; export type Imported = Result;\n"
					if use == "value" {
						library = "export { value } from 'nested-value';\n"
						source = "import { value } from '../lib/index.js'; export const imported = value;\n"
					}
					tree := map[string]string{
						"MODULE.bazel":               "module(name = \"supplied_npm\")\n",
						"package.json":               `{"private":true}`,
						"node_modules/.modules.yaml": "layoutVersion: 5\n",
						pnpmLockfileName: `lockfileVersion: '9.0'
importers:
  .: {}
  consumer: {}
  lib:
    dependencies:
      nested-value:
        specifier: 1.0.0
        version: 1.0.0
packages:
  nested-value@1.0.0:
    resolution: {integrity: sha512-aaa}
snapshots:
  nested-value@1.0.0: {}
`,
						"lib/package.json":  `{"private":true,"dependencies":{"nested-value":"1.0.0"}}`,
						"lib/tsconfig.json": config,
						"lib/index.ts":      library,
						"lib/node_modules/nested-value/package.json": `{"name":"nested-value","version":"1.0.0","types":"index.d.ts"}`,
						"lib/node_modules/nested-value/index.d.ts":   "export interface Value { value: number }\nexport declare const value: Value;\n",
						"consumer/package.json":                      `{"private":true}`,
						"consumer/tsconfig.json":                     config,
						"consumer/" + entry:                          source,
					}
					ownerDeps := []string{"@npm//lib:nested-value"}
					consumerDeps := []string{"//lib"}
					var importers []string
					if ownerPaths {
						tree["lib/tsconfig.json"] = strings.Replace(config, `"types":[]`, `"types":[],"paths":{"nested-value":["./native.ts"]}`, 1)
						tree["lib/native.ts"] = "export interface Value { value: number }\nexport const value: Value = { value: 42 };\n"
						ownerDeps = nil
						consumerDeps = append(consumerDeps, "@npm//lib:nested-value")
						importers = []string{"//lib:node_modules"}
					}
					root := writeTree(t, tree)
					var first map[string]string
					for _, args := range [][]string{nil, {"-r=false", "consumer"}, nil} {
						output, err := protoGazelle(t, root, args...)
						if err != nil {
							t.Fatalf("npm source boundary with %v: %v\n%s", args, err, output)
						}
						owner := onDiskRule(t, root, "lib", "ts_compile", "lib")
						wantLabels(t, "source owner retains its own resolution", owner.AttrStrings("deps"), ownerDeps)
						consumer := onDiskRule(t, root, "consumer", kind, name)
						wantLabels(t, "consumer retains only its required npm identities", consumer.AttrStrings("deps"), consumerDeps)
						wantStrings(t, "dependency sources remain owned by the library", consumer.AttrStrings("srcs"), []string{entry})
						wantLabels(t, "consumer retains its own scope", consumer.AttrStrings("package_scopes"), []string{"package.json"})
						wantLabels(t, "only consumer-specific npm lookups retain an importer", consumer.AttrStrings("source_node_modules"), importers)
						if first == nil {
							first = convergeSnapshot(t, root)
						} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("npm source boundary did not converge: %s", diff)
						}
					}
				})
			}
		}
	}
}

func TestBorrowedSourceKeepsItsNpmImporterUntilACompilerOwnsIt(t *testing.T) {
	for _, lookup := range []struct {
		name, target string
		generated    bool
		inherited    bool
	}{
		{name: "first-party declaration keeps foreign runtime importer", target: "api.d.ts"},
		{name: "generated declaration keeps foreign runtime importer", target: "api.d.ts", generated: true},
		{name: "implementation alias adds no npm requirement", target: "api.ts"},
		{name: "equivalent consumer lookup adds no importer", target: "api.d.ts", inherited: true},
	} {
		t.Run(lookup.name, func(t *testing.T) {
			declaration := "    dependencies:\n      dependency:\n        specifier: 1.0.0\n        version: 1.0.0\n"
			lock := "lockfileVersion: '9.0'\nimporters:\n  .: {}\n  app: {}\n  shared:\n" + declaration
			rootManifest, sharedManifest := `{"private":true}`, `{"private":true,"dependencies":{"dependency":"1.0.0"}}`
			runtime := "@npm//shared:dependency"
			if lookup.inherited {
				lock = "lockfileVersion: '9.0'\nimporters:\n  .:\n" + declaration + "  app: {}\n  shared: {}\n"
				rootManifest, sharedManifest = sharedManifest, rootManifest
				runtime = "@npm//:dependency"
			}
			files := map[string]string{
				"MODULE.bazel":               "module(name = \"borrowed_runtime_importer\")\n",
				"package.json":               rootManifest,
				"node_modules/.modules.yaml": "layoutVersion: 5\n",
				pnpmLockfileName:             lock + "packages:\n  dependency@1.0.0: {}\nsnapshots:\n  dependency@1.0.0: {}\n",
				"app/package.json":           `{"private":true}`,
				"app/tsconfig.json":          `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"paths":{"dependency":["../shared/` + lookup.target + `"]}},"files":["index.ts"]}`,
				"app/index.ts":               "export { value } from '../shared/helper.js';\n",
				"shared/package.json":        sharedManifest,
				"shared/helper.ts":           "export { value } from 'dependency';\n",
				"shared/BUILD.bazel":         "exports_files([\"helper.ts\", \"package.json\"], visibility = [\"//visibility:public\"])\n",
			}
			if lookup.generated {
				files["shared/BUILD.bazel"] += `genrule(name = "declaration", outs = ["api.d.ts"], cmd = "unused", visibility = ["//visibility:public"])` + "\n"
			} else {
				contents := "export declare const value: number;\n"
				if lookup.target == "api.ts" {
					contents = "export const value = 42;\n"
				}
				files["shared/"+lookup.target] = contents
				files["shared/BUILD.bazel"] += "exports_files([\"" + lookup.target + "\"], visibility = [\"//visibility:public\"])\n"
			}
			root := writeTree(t, files)
			output, err := protoGazelle(t, root)
			if err != nil {
				t.Fatalf("borrowed runtime lookup: %v\n%s", err, output)
			}
			consumer := onDiskRule(t, root, "app", "ts_compile", "app")
			wantLabels(t, "compiler retains the first-party inputs", consumer.AttrStrings("srcs"), []string{"index.ts", "//shared:helper.ts", "//shared:" + lookup.target})
			var deps, importers []string
			if isDeclarationFile(lookup.target) {
				deps = []string{runtime}
				if !lookup.inherited {
					importers = []string{"//shared:node_modules"}
				}
			}
			wantLabels(t, "runtime package demand keeps its declared supplier", consumer.AttrStrings("deps"), deps)
			wantLabels(t, "only a differing runtime lookup requires an importer", consumer.AttrStrings("source_node_modules"), importers)
		})
	}
	for _, extension := range []string{"ts", "d.ts"} {
		t.Run(extension, func(t *testing.T) {
			for _, lookup := range []struct {
				typesDir string
				shadow   bool
			}{{"", false}, {"shared", false}, {"", true}} {
				t.Run(fmt.Sprintf("inherited_package_types_in_%s_shadow_%t", orRepoRoot(lookup.typesDir), lookup.shadow), func(t *testing.T) {
					typesDir := lookup.typesDir
					declaration := "    dependencies:\n      '@types/dependency':\n        specifier: 1.0.0\n        version: 1.0.0\n"
					lock := "lockfileVersion: '9.0'\nimporters:\n  .:\n    dependencies:\n      dependency:\n        specifier: 1.0.0\n        version: 1.0.0\n"
					rootDeps := []string{"@npm//:dependency"}
					rootManifest, sharedManifest := `{"private":true,"dependencies":{"dependency":"1.0.0"}}`, `{"private":true}`
					appImporter, appManifest := "  app: {}\n", `{"private":true}`
					if lookup.shadow {
						appImporter = "  app:\n" + strings.ReplaceAll(declaration, "1.0.0", "2.0.0")
						appManifest = `{"private":true,"dependencies":{"@types/dependency":"2.0.0"}}`
					}
					if typesDir == "" {
						lock += strings.TrimPrefix(declaration, "    dependencies:\n") + appImporter + "  shared: {}\n"
						rootDeps = append(rootDeps, "@npm//:types_dependency")
						rootManifest = `{"private":true,"dependencies":{"dependency":"1.0.0","@types/dependency":"1.0.0"}}`
					} else {
						lock += "  app: {}\n  shared:\n" + declaration
						sharedManifest = `{"private":true,"dependencies":{"@types/dependency":"1.0.0"}}`
					}
					resolved := "  dependency@1.0.0: {}\n  '@types/dependency@1.0.0': {}\n"
					if lookup.shadow {
						resolved += "  '@types/dependency@2.0.0': {}\n"
					}
					root := writeTree(t, map[string]string{
						"MODULE.bazel": "module(name = \"inherited_importer\")\n",
						"BUILD.bazel": `load("@rules_typescript//npm:defs.bzl", "node_modules")
node_modules(name = "node_modules", deps = [` + strings.Join(quotedEach(rootDeps), ", ") + `], visibility = ["//visibility:public"])
`,
						"package.json":                         rootManifest,
						pnpmLockfileName:                       lock + "packages:\n" + resolved + "snapshots:\n" + resolved,
						"node_modules/.modules.yaml":           "layoutVersion: 5\n",
						"node_modules/dependency/package.json": `{"name":"dependency","version":"1.0.0","main":"index.js"}`,
						"node_modules/dependency/index.js":     "exports.value = 42;\n",
						path.Join(typesDir, "node_modules/@types/dependency/package.json"): `{"name":"@types/dependency","version":"1.0.0","types":"index.d.ts"}`,
						path.Join(typesDir, "node_modules/@types/dependency/index.d.ts"):   "export interface Value { value: number }\n",
						"app/package.json":           appManifest,
						"app/tsconfig.json":          `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","noImplicitAny":true,"types":[]},"files":["index.ts"]}`,
						"app/index.ts":               "export type { Value } from '../shared/helper.js';\n",
						"shared/package.json":        sharedManifest,
						"shared/BUILD.bazel":         `exports_files(["helper.` + extension + `", "package.json"], visibility = ["//visibility:public"])`,
						"shared/helper." + extension: "export type { Value } from 'dependency';\n",
					})
					if lookup.shadow {
						writeFile(t, filepath.Join(root, "app/node_modules/@types/dependency/package.json"), `{"name":"@types/dependency","version":"2.0.0","types":"index.d.ts"}`)
						writeFile(t, filepath.Join(root, "app/node_modules/@types/dependency/index.d.ts"), "export interface Value { value: string }\n")
					}
					before := buildFileBytes(t, root)
					output, err := protoGazelle(t, root, "-index=false", "-r=false", "app")
					var want []string
					if lookup.shadow {
						want = []string{"//:node_modules"}
					}
					if typesDir != "" {
						if err == nil || !strings.Contains(output, "importer //shared:node_modules") {
							t.Fatalf("split declaration lookup lost its foreign importer: %v\n%s", err, output)
						}
						if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("rejected split lookup changed BUILD files: %s", diff)
						}
						writeFile(t, filepath.Join(root, "shared/BUILD.bazel"), "load(\"@rules_typescript//npm:defs.bzl\", \"node_modules\")\n"+buildFileText(t, root, "shared")+`
node_modules(name = "node_modules", parent = "//:node_modules", deps = ["@npm//shared:types_dependency"], visibility = ["//visibility:public"])
`)
						output, err = protoGazelle(t, root, "-index=false", "-r=false", "app")
						want = []string{"//shared:node_modules"}
					}
					if err != nil {
						t.Fatalf("inherited npm lookup: %v\n%s", err, output)
					}
					consumer := onDiskRule(t, root, "app", "ts_compile", "app")
					wantLabels(t, "only differing source lookups require a context", consumer.AttrStrings("source_node_modules"), want)
					wantLabels(t, "the package keeps its root identity", consumer.AttrStrings("deps"), []string{"@npm//:dependency"})
					if lookup.shadow {
						writeFile(t, filepath.Join(root, "shared/BUILD.bazel"), "load(\"@rules_typescript//npm:defs.bzl\", \"node_modules\")\n"+buildFileText(t, root, "shared")+`
node_modules(name = "node_modules", parent = "//:node_modules", visibility = ["//visibility:public"])
`)
						consumerPath := filepath.Join(root, "app/BUILD.bazel")
						original := buildFileText(t, root, "app")
						for _, context := range []string{":node_modules", "//shared:node_modules", "//:node_modules"} {
							build, err := rule.LoadData(consumerPath, "app", []byte(original))
							if err != nil {
								t.Fatal(err)
							}
							for _, r := range build.Rules {
								if r.Kind() == "ts_compile" {
									r.SetAttr("source_node_modules", []string{context})
									r.AttrComments("source_node_modules").Before = []bzl.Comment{{Token: "# keep"}}
								}
							}
							writeFile(t, consumerPath, string(build.Format()))
							before := buildFileBytes(t, root)
							output, err := protoGazelle(t, root, "-index=false", "-r=false", "app")
							if context == ":node_modules" {
								if err == nil || !strings.Contains(output, "kept source_node_modules") {
									t.Fatalf("shadowing kept context accepted ancestor membership: %v\n%s", err, output)
								}
								if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
									t.Fatalf("rejected shadowing context changed BUILD files: %s", diff)
								}
							} else if err != nil {
								t.Fatalf("equivalent kept lookup %s rejected: %v\n%s", context, err, output)
							} else {
								wantLabels(t, "equivalent kept lookup is retained", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("source_node_modules"), []string{context})
							}
						}
					}
				})
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel":               "module(name = \"borrowed_importer\")\n",
				"package.json":               `{"private":true}`,
				"node_modules/.modules.yaml": "layoutVersion: 5\n",
				pnpmLockfileName: `lockfileVersion: '9.0'
importers:
  .: {}
  app:
    dependencies:
      dependency:
        specifier: 1.0.0
        version: 1.0.0
  shared:
    dependencies:
      dependency:
        specifier: 1.0.0
        version: 1.0.0
packages:
  dependency@1.0.0:
    resolution: {integrity: sha512-aaa}
snapshots:
  dependency@1.0.0: {}
`,
				"app/package.json":                            `{"private":true,"dependencies":{"dependency":"1.0.0"}}`,
				"app/tsconfig.json":                           `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
				"app/index.ts":                                "export type { Value } from '../shared/helper.js';\n",
				"shared/package.json":                         `{"private":true,"dependencies":{"dependency":"1.0.0"}}`,
				"shared/BUILD.bazel":                          `exports_files(["helper.` + extension + `", "package.json"], visibility = ["//visibility:public"])`,
				"shared/helper." + extension:                  "export type { Value } from 'dependency';\n",
				"shared/node_modules/dependency/package.json": `{"name":"dependency","version":"1.0.0","types":"index.d.ts"}`,
				"shared/node_modules/dependency/index.d.ts":   "export interface Value { value: number }\n",
			})
			before := buildFileBytes(t, root)
			output, err := protoGazelle(t, root, "-index=false", "-r=false", "app")
			if err == nil || !strings.Contains(output, "importer //shared:node_modules") || !strings.Contains(output, "include its package") {
				t.Fatalf("cold partial update published an unavailable importer: %v\n%s", err, output)
			}
			if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
				t.Fatalf("rejected cold update changed BUILD files: %s", diff)
			}
			helper := filepath.Join(root, "shared/helper."+extension)
			writeFile(t, helper, "export interface Value { value: number }\n")
			output, err = protoGazelle(t, root, "-index=false", "-r=false", "app")
			if err != nil {
				t.Fatalf("numeric-only helper required an npm importer: %v\n%s", err, output)
			}
			wantLabels(t, "input without npm demands needs no importer", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("source_node_modules"), nil)
			writeFile(t, helper, "export type { Value } from 'dependency';\n")
			for _, args := range [][]string{nil, {"-index=false", "-r=false", "app"}} {
				output, err := protoGazelle(t, root, args...)
				if err != nil {
					t.Fatalf("borrowed importer with %v: %v\n%s", args, err, output)
				}
				consumer := onDiskRule(t, root, "app", "ts_compile", "app")
				wantLabels(t, "borrowed input retains its lookup directory", consumer.AttrStrings("source_node_modules"), []string{"//shared:node_modules"})
				wantLabels(t, "borrowed input retains its declared package", consumer.AttrStrings("deps"), []string{"@npm//shared:dependency"})
			}
			consumerPath := filepath.Join(root, "app/BUILD.bazel")
			original, err := os.ReadFile(consumerPath)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, "contexts/BUILD.bazel"), `alias(name = "source_importer", actual = "//shared:node_modules", visibility = ["//visibility:public"])
`)
			for _, kept := range []string{"empty", "unknown expression", "direct", "alias", "equivalent dependency", "selected config and root"} {
				build, err := rule.LoadData(consumerPath, "app", original)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range build.Rules {
					if r.Kind() != "ts_compile" {
						continue
					}
					switch kept {
					case "empty":
						r.SetAttr("source_node_modules", []string{})
					case "unknown expression":
						r.SetAttr("source_node_modules", &bzl.Ident{Name: "SOURCE_IMPORTERS"})
					case "alias":
						r.SetAttr("source_node_modules", []string{"//contexts:source_importer"})
					case "equivalent dependency":
						r.SetAttr("deps", []string{"@npm//app:dependency"})
						r.AttrComments("deps").Before = []bzl.Comment{{Token: "# keep"}}
					case "selected config and root":
						r.AttrComments("srcs").Before = []bzl.Comment{{Token: "# keep"}}
						r.SetAttr("tsconfig", "selected.tsconfig.json")
						r.AttrComments("tsconfig").Before = []bzl.Comment{{Token: "# keep"}}
						config, err := os.ReadFile(filepath.Join(root, "app/tsconfig.json"))
						if err != nil {
							t.Fatal(err)
						}
						writeFile(t, filepath.Join(root, "app/selected.tsconfig.json"), string(config))
						writeFile(t, filepath.Join(root, "app/tsconfig.json"), strings.Replace(string(config), "index.ts", "unused.ts", 1))
						writeFile(t, filepath.Join(root, "app/unused.ts"), "export {};\n")
					}
					if kept != "selected config and root" {
						r.AttrComments("source_node_modules").Before = []bzl.Comment{{Token: "# keep"}}
					}
				}
				writeFile(t, consumerPath, string(build.Format()))
				before := buildFileBytes(t, root)
				output, err := protoGazelle(t, root, "-index=false", "-r=false", "app")
				if kept == "empty" || kept == "unknown expression" {
					if err == nil || !strings.Contains(output, "kept source_node_modules") || !strings.Contains(output, "//shared:node_modules") {
						t.Fatalf("%s importer did not reject incomplete closure: %v\n%s", kept, err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("rejected importer changed BUILD files: %s", diff)
					}
				} else if err != nil {
					t.Fatalf("%s importer rejected retained scope: %v\n%s", kept, err, output)
				}
				if kept == "alias" {
					wantLabels(t, "partial update retains the unwalked importer alias", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("source_node_modules"), []string{"//contexts:source_importer"})
					if got := buildFileText(t, root, "contexts"); got != before["contexts/BUILD.bazel"] {
						t.Fatalf("partial update changed the unwalked importer package: %s", got)
					}
				}
				if kept == "selected config and root" {
					consumer := onDiskRule(t, root, "app", "ts_compile", "app")
					wantLabels(t, "selected compiler retains its kept source closure", consumer.AttrStrings("srcs"), []string{"index.ts", "//shared:helper." + extension})
					wantLabels(t, "selected compiler program supplies importer demands", consumer.AttrStrings("source_node_modules"), []string{"//shared:node_modules"})
					config, err := os.ReadFile(filepath.Join(root, "app/selected.tsconfig.json"))
					if err != nil {
						t.Fatal(err)
					}
					writeFile(t, filepath.Join(root, "app/tsconfig.json"), string(config))
					for _, file := range []string{"selected.tsconfig.json", "unused.ts"} {
						if err := os.Remove(filepath.Join(root, "app", file)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			writeFile(t, consumerPath, string(original))
			writeFile(t, filepath.Join(root, "shared/BUILD.bazel"), loadDefs+`"ts_compile")
# keep
ts_compile(name = "shared", srcs = ["helper.`+extension+`"], package_scopes = ["package.json"], node_modules = ":node_modules", deps = ["@npm//shared:dependency"], visibility = ["//visibility:public"])
`)
			output, err = protoGazelle(t, root)
			if err != nil {
				t.Fatalf("owned source importer: %v\n%s", err, output)
			}
			consumer := onDiskRule(t, root, "app", "ts_compile", "app")
			wantLabels(t, "compiler owner supplies its own importer", consumer.AttrStrings("source_node_modules"), nil)
			wantLabels(t, "compiler owner supplies the borrowed input", consumer.AttrStrings("deps"), []string{"//shared"})
		})
	}
}

func TestEmittedDependencyDoesNotHideDirectBorrowedInput(t *testing.T) {
	for _, leaf := range []string{"leaf", "a_leaf"} {
		t.Run(leaf, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel":      "module(name = \"emitted_intermediary\")\n",
				"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
				"app/index.ts":      "export { value as wrapped } from '../b/index.js'; export { value } from '../fixtures/value.js';\n",
				"b/BUILD.bazel": loadDefs + `"ts_compile")
# keep
ts_compile(name = "b", srcs = ["index.d.ts"], deps = ["//c"], emit = True, visibility = ["//visibility:public"])
`,
				"b/index.d.ts":          "export { value } from '../c/index.js';\n",
				"c/tsconfig.json":       `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
				"c/index.ts":            "export { value } from '../fixtures/value.js';\n",
				"fixtures/BUILD.bazel":  `exports_files(["value.ts"], visibility = ["//app:__pkg__", "//c:__pkg__"])`,
				"fixtures/value.ts":     "export { value } from '../" + leaf + "/index.js';\n",
				leaf + "/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
				leaf + "/index.ts":      "export const value: number = 42;\n",
			})
			for _, args := range [][]string{nil, {"-r=false", "app"}, nil} {
				var first map[string]string
				for _, pass := range []string{"first", "repeated"} {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("emitted intermediary with %v (%s): %v\n%s", args, pass, err, output)
					}
					consumer := onDiskRule(t, root, "app", "ts_compile", "app")
					sources, deps := []string{"index.ts", "//fixtures:value.ts"}, []string{"//b", "//" + leaf}
					if slices.Contains(consumer.AttrStrings("deps"), "//c") {
						sources, deps = []string{"index.ts"}, []string{"//b", "//c"}
					}
					wantLabels(t, "direct input is explicit or supplied by a direct source owner", consumer.AttrStrings("srcs"), sources)
					wantLabels(t, "emitted intermediary cannot be the only route to borrowed implementation", consumer.AttrStrings("deps"), deps)
					supplier := onDiskRule(t, root, "c", "ts_compile", "c")
					wantLabels(t, "source supplier retains the original input identity", supplier.AttrStrings("srcs"), []string{"index.ts", "//fixtures:value.ts"})
					wantLabels(t, "source supplier preserves the borrowed input dependency", supplier.AttrStrings("deps"), []string{"//" + leaf})
					if supplier.Attr("emit") != nil {
						if emit, ok := supplier.Attr("emit").(*bzl.Ident); !ok || emit.Name != "False" {
							t.Fatal("direct supplier cannot substitute declarations for the borrowed source")
						}
					}
					intermediary := onDiskRule(t, root, "b", "ts_compile", "b")
					wantStrings(t, "intermediary exposes declarations", intermediary.AttrStrings("srcs"), []string{"index.d.ts"})
					if emit, ok := intermediary.Attr("emit").(*bzl.Ident); !ok || emit.Name != "True" {
						t.Fatal("intermediary lost its emitted boundary")
					}
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("emitted intermediary changed for the same update %v: %s", args, diff)
					}
				}
			}
		})
	}
}

// Failed gazelle_roundtrip_borrowed_test: app and app_test both compiled the borrowed helper to one output.
func TestEmittedPackageCompilerAloneRetainsABorrowedSourceItsTestImports(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"borrowed_single_owner\")\n",
		"app/BUILD.bazel": loadDefs + `"ts_compile", "ts_test")
ts_compile(name = "app", emit = True, # keep
)
ts_test(name = "app_test", emit = True, # keep
)
`,
		"app/tsconfig.json":    `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts","index.test.ts"]}`,
		"app/index.ts":         "export { value } from '../fixtures/value.js';\n",
		"app/index.test.ts":    "import { value } from '../fixtures/value.js';\nimport { value as wrapped } from './index.js';\nexport const same: boolean = value === wrapped;\n",
		"fixtures/BUILD.bazel": `exports_files(["value.ts"], visibility = ["//app:__pkg__"])`,
		"fixtures/value.ts":    "export const value: number = 42;\n",
	})
	if output, err := protoGazelle(t, root); err != nil {
		t.Fatalf("gazelle: %v\n%s", err, output)
	}
	compiler := onDiskRule(t, root, "app", "ts_compile", "app")
	test := onDiskRule(t, root, "app", "ts_test", "app_test")
	wantLabels(t, "the package compiler retains the borrowed source", compiler.AttrStrings("srcs"), []string{"index.ts", "//fixtures:value.ts"})
	wantLabels(t, "the test leaves the borrowed source to its compiler", test.AttrStrings("srcs"), []string{"index.test.ts"})
	if !slices.Contains(test.AttrStrings("deps"), ":app") {
		t.Errorf("test deps = %v, want :app", test.AttrStrings("deps"))
	}
}

func TestKeptCompilerConfigDoesNotResolveDiscardedImports(t *testing.T) {
	for _, kind := range []string{"ts_compile", "ts_test"} {
		for _, selected := range []string{"safe.json", ":config_alias"} {
			t.Run(kind+"/"+selected, func(t *testing.T) {
				name, source := "app", "index.ts"
				if kind == "ts_test" {
					name, source = "app_test", "index.test.ts"
				}
				config := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"paths":{"#impl":["../private/value.ts"]}},"files":["` + source + `"]}`
				safeConfig := strings.Replace(config, "../private/value.ts", "../fixtures/value.ts", 1)
				root := writeTree(t, map[string]string{
					"MODULE.bazel": "module(name = \"kept_compiler_imports\")\n",
					"BUILD.bazel":  "# gazelle:exclude private/value.ts\n",
					"app/BUILD.bazel": loadDefs + `"ts_compile", "ts_config", "ts_test")` + "\n" +
						"ts_config(name = \"safe_config\", src = \"safe.json\")\nalias(name = \"config_alias\", actual = \":safe_config\")\n" +
						kind + "(name = \"" + name + "\", tsconfig = \"" + selected + "\", # keep\n)\n",
					"app/tsconfig.json":    config,
					"app/safe.json":        safeConfig,
					"app/" + source:        "export { value } from '#impl';\n",
					"app/ambient.ts":       "export {};\n",
					"private/value.ts":     "export const value = 'discarded';\n",
					"fixtures/BUILD.bazel": "exports_files([\"value.ts\"], visibility = [\"//app:__pkg__\"])\n",
					"fixtures/value.ts":    "export { value } from '../leaf/value';\n",
					"leaf/BUILD.bazel":     "exports_files([\"value.ts\"], visibility = [\"//app:__pkg__\"])\n",
					"leaf/value.ts":        "export const value = 'selected';\n",
				})
				var first map[string]string
				for _, roots := range []string{source, "ambient.ts"} {
					writeFile(t, filepath.Join(root, "app/safe.json"), strings.Replace(safeConfig, `"files":["`+source+`"]`, `"files":["`+roots+`"]`, 1))
					for _, args := range [][]string{nil, {"-index=false", "app"}, nil} {
						output, err := protoGazelle(t, root, args...)
						if err != nil {
							t.Fatalf("selected imports with config roots %s and %v: %v\n%s", roots, args, err, output)
						}
						r := onDiskRule(t, root, "app", kind, name)
						if got := r.AttrString("tsconfig"); got != selected {
							t.Fatalf("kept config = %q, want %q", got, selected)
						}
						wantLabels(t, "selected import closure", r.AttrStrings("srcs"), []string{source, "safe.json", "//fixtures:value.ts", "//leaf:value.ts"})
						wantStrings(t, "unowned imports have no compiler dependency", r.AttrStrings("deps"), nil)
						if first == nil {
							first = convergeSnapshot(t, root)
						} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("selected import closure did not converge: %s", diff)
						}
					}
				}
				for _, excluded := range []string{"fixtures/value.ts", "leaf/value.ts"} {
					writeFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude private/value.ts\n# gazelle:exclude "+excluded+"\n")
					before := convergeSnapshot(t, root)
					for _, args := range [][]string{{"-index=false", "app"}, nil} {
						output, err := protoGazelle(t, root, args...)
						if err == nil || !strings.Contains(output, excluded) || !strings.Contains(output, "excluded or ignored") {
							t.Fatalf("excluded selected import with %v: %v\n%s", args, err, output)
						}
						if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("rejected selected import changed BUILD files: %s", diff)
						}
					}
				}
			})
		}
	}
}

func TestPartialUpdateObservesKeptCompilerTypeInputs(t *testing.T) {
	for _, kind := range []string{"ts_compile", "ts_test"} {
		for _, selected := range []string{"safe.json", ":config_alias"} {
			t.Run(kind+"/"+selected, func(t *testing.T) {
				name, source := "app", "index.ts"
				if kind == "ts_test" {
					name, source = "app_test", "index.test.ts"
				}
				root := writeTree(t, map[string]string{
					"MODULE.bazel": "module(name = \"partial_compiler_types\")\n",
					"BUILD.bazel":  "",
					"app/BUILD.bazel": loadDefs + `"ts_compile", "ts_config", "ts_test")` + "\n" +
						"alias(name = \"config_alias\", actual = \"//settings:safe_config\")\n" +
						kind + "(name = \"" + name + "\", tsconfig = \"" + selected + "\", # keep\n)\n",
					"settings/BUILD.bazel": loadDefs + `"ts_config")` + "\nts_config(name = \"safe_config\", src = \"//app:safe.json\")\n",
					"app/tsconfig.json":    `{"compilerOptions":{"types":[]},"files":["` + source + `"]}`,
					"app/safe.json":        `{"compilerOptions":{"types":["../ambient/global.d.ts"]},"files":["` + source + `"]}`,
					"app/" + source:        "export const value = 1;\n",
					"ambient/BUILD.bazel":  "exports_files([\"global.d.ts\"], visibility = [\"//app:__pkg__\"])\n",
					"ambient/global.d.ts":  "import type { Marker } from '../types/leaf'; declare global { const ambientValue: Marker; } export {};\n",
					"types/BUILD.bazel":    "exports_files([\"leaf.d.ts\"], visibility = [\"//app:__pkg__\"])\n",
					"types/leaf.d.ts":      "export interface Marker { readonly value: string; }\n",
				})
				var first map[string]string
				for _, args := range [][]string{nil, {"-index=false", "app"}, nil} {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("selected types with %v: %v\n%s", args, err, output)
					}
					r := onDiskRule(t, root, "app", kind, name)
					wantLabels(t, "selected type closure", r.AttrStrings("srcs"), []string{source, "safe.json", "//ambient:global.d.ts", "//types:leaf.d.ts"})
					wantStrings(t, "raw type sources have no compiler dependency", r.AttrStrings("deps"), nil)
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("partial selected-config update did not converge: %s", diff)
					}
				}
				for _, excluded := range []string{"ambient/global.d.ts", "types/leaf.d.ts"} {
					writeFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude "+excluded+"\n")
					before := convergeSnapshot(t, root)
					for _, args := range [][]string{{"-index=false", "app"}, nil} {
						output, err := protoGazelle(t, root, args...)
						if err == nil || !strings.Contains(output, excluded) || !strings.Contains(output, "excluded or ignored") {
							t.Fatalf("excluded selected type with %v: %v\n%s", args, err, output)
						}
						if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("rejected selected type changed BUILD files: %s", diff)
						}
					}
				}
			})
		}
	}
}

func TestMappedKeptConfigDoesNotValidateDiscardedSource(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"mapped_kept_config\")\n",
		"BUILD.bazel":  "# gazelle:map_kind ts_config custom_config //:defs.bzl\n# gazelle:exclude app/tsconfig.json\n",
		"app/BUILD.bazel": loadDefs + `"ts_config")` + `
ts_config(
    name = "tsconfig",
    src = "safe.json", # keep
)
`,
		"app/tsconfig.json": `{"files":["index.ts"]}`,
		"app/safe.json":     `{"files":["index.ts"]}`,
		"app/index.ts":      "export const safe = 1;\n",
	})
	var first map[string]string
	for pass := range 2 {
		output, err := protoGazelle(t, root)
		if err != nil {
			t.Fatalf("mapped kept config pass %d: %v\n%s", pass, err, output)
		}
		if got := onDiskRule(t, root, "app", "custom_config", "tsconfig").AttrString("src"); got != "safe.json" {
			t.Fatalf("kept source = %q, want safe.json", got)
		}
		if pass == 0 {
			first = convergeSnapshot(t, root)
		} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
			t.Fatalf("mapped kept config did not converge: %s", diff)
		}
	}
}

func TestKeptProgramSourcesDoNotResolveDiscardedImporters(t *testing.T) {
	for _, spelling := range []struct{ kind, wrapped string }{
		{"ts_compile", ""}, {"ts_test", ""},
		{"ts_compile", "ts_compile"}, {"ts_test", "ts_test"},
		{"ts_compile", "mapped_compile"}, {"ts_test", "mapped_test"},
	} {
		kind, declared, directives := spelling.kind, spelling.kind, ""
		if spelling.wrapped != "" {
			declared = "custom_" + strings.TrimPrefix(kind, "ts_")
			directives = "load(\"//:defs.bzl\", \"" + declared + "\")\n# gazelle:alias_kind " + declared + " " + spelling.wrapped + "\n"
			if spelling.wrapped != kind {
				directives += "# gazelle:map_kind " + kind + " " + spelling.wrapped + " //:defs.bzl\n"
			}
		}
		for _, keep := range []string{"rule", "attribute"} {
			t.Run(kind+"/"+spelling.wrapped+"/"+keep, func(t *testing.T) {
				name, suffix := "app", ".ts"
				if kind == "ts_test" {
					name, suffix = "app_test", ".test.ts"
				}
				safe, unused := "safe"+suffix, "unused"+suffix
				prefix, attrKeep, tsconfig := "# keep\n", "", "    tsconfig = \":safe_config\",\n"
				if keep == "attribute" {
					prefix, attrKeep = "", " # keep"
					tsconfig = "    tsconfig = \":safe_config\", # keep\n"
				}
				build := directives + loadDefs + `"ts_compile", "ts_config", "ts_test")` + "\n# gazelle:exclude secret.ts\n" +
					"ts_config(name = \"safe_config\", src = \"safe.json\")\n" + prefix + declared + "(\n" +
					"    name = \"" + name + "\",\n    srcs = [\"" + safe + "\"]," + attrKeep + "\n" + tsconfig + ")\n"
				root := writeTree(t, map[string]string{
					"MODULE.bazel":        "module(name = \"kept_program_sources\")\n",
					"app/BUILD.bazel":     build,
					"app/tsconfig.json":   `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"include":["*.ts"]}`,
					"app/safe.json":       `{"files":["` + safe + `"]}`,
					"app/" + safe:         "export const safe = 1;\n",
					"app/" + unused:       "import '../other/index'; import './secret'; export {};\n",
					"app/secret.ts":       "export const secret = 1;\n",
					"other/tsconfig.json": `{"files":["index.ts"]}`,
					"other/index.ts":      "export const other = 1;\n",
				})
				var first map[string]string
				for _, args := range [][]string{nil, {"-index=false"}, nil} {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("discarded importer with %v: %v\n%s", args, err, output)
					}
					r := onDiskRule(t, root, "app", declared, name)
					wantStrings(t, "kept source subset", r.AttrStrings("srcs"), []string{safe})
					wantStrings(t, "discarded importer adds no dependency", r.AttrStrings("deps"), nil)
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("kept source subset changed on rerun: %s", diff)
					}
				}
				writeFile(t, filepath.Join(root, "app/BUILD.bazel"), strings.Replace(build, `srcs = ["`+safe+`"]`, `srcs = ["`+safe+`", "`+unused+`"]`, 1))
				writeFile(t, filepath.Join(root, "app/safe.json"), `{"files":["`+safe+`","`+unused+`"]}`)
				before := convergeSnapshot(t, root)
				output, err := protoGazelle(t, root)
				if err == nil || !strings.Contains(output, "app/"+unused+" imports app/secret.ts") || !strings.Contains(output, "excluded or ignored") {
					t.Fatalf("retained importer bypassed exclusion: %v\n%s", err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("rejected retained importer changed BUILD files: %s", diff)
				}
			})
		}
	}
}

func TestKeptForwardedRootsDoNotLoseCompilerEdges(t *testing.T) {
	for _, discovery := range []string{"tsconfig extra absent", "tsconfig extra listed", "manifest"} {
		for _, spelling := range []string{"direct", "filegroup", "nested alias"} {
			t.Run(discovery+"/"+spelling, func(t *testing.T) {
				source, extra, declaration := "extra.ts", "app/extra.ts", ""
				tree := map[string]string{
					"MODULE.bazel":      "module(name = \"kept_forwarded_roots\")\n",
					"app/index.ts":      "export const index = 1;\n",
					"dep/tsconfig.json": `{"files":["index.ts"]}`,
					"dep/index.ts":      "export const value = 2;\n",
				}
				if spelling == "filegroup" {
					source = ":extra_sources"
					declaration = "filegroup(name = \"extra_sources\", srcs = [\"extra.ts\"])\n"
				} else if spelling == "nested alias" {
					source, extra = ":extra_sources", "forward/nested/extra.ts"
					declaration = "filegroup(name = \"extra_sources\", srcs = [\"//forward:sources\"])\n"
					tree["forward/BUILD.bazel"] = `alias(name = "sources", actual = "//forward/nested:sources", visibility = ["//visibility:public"])`
					tree["forward/nested/BUILD.bazel"] = "filegroup(name = \"sources\", srcs = [\":leaf\"], visibility = [\"//visibility:public\"])\nalias(name = \"leaf\", actual = \"extra.ts\")\n"
				}
				imported := "../dep/index"
				if spelling == "nested alias" {
					imported = "../../dep/index"
				}
				tree[extra] = "export { value } from '" + imported + "';\n"
				if discovery == "manifest" {
					tree["app/package.json"] = `{"exports":"./index.ts"}`
				} else {
					files := `"index.ts"`
					if discovery == "tsconfig extra listed" {
						files += `,"../` + extra + `"`
					}
					tree["app/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":[` + files + `]}`
				}
				keep := "\n        \"" + source + "\", # keep\n    ],\n"
				if discovery == "tsconfig extra listed" {
					keep = "\n        \"" + source + "\",\n    ], # keep\n"
				}
				tree["app/BUILD.bazel"] = loadDefs + `"ts_compile")` + "\n" + declaration + "ts_compile(\n    name = \"app\",\n    srcs = [\n        \"index.ts\"," + keep + ")\n"
				root := writeTree(t, tree)
				for _, imported := range []bool{true, false, true} {
					contents := "export const value = 2;\n"
					if imported {
						contents = tree[extra]
					}
					writeFile(t, filepath.Join(root, extra), contents)
					var first map[string]string
					for _, args := range [][]string{nil, {"-r=false", "app"}} {
						output, err := protoGazelle(t, root, args...)
						if err != nil {
							t.Fatalf("kept roots with %v: %v\n%s", args, err, output)
						}
						compile := onDiskRule(t, root, "app", "ts_compile", "app")
						if !slices.Contains(compile.AttrStrings("srcs"), source) {
							t.Fatalf("source spelling %q lost: %v", source, compile.AttrStrings("srcs"))
						}
						var deps []string
						if imported {
							deps = []string{"//dep"}
						}
						wantLabels(t, "only the kept root requires this dependency", compile.AttrStrings("deps"), deps)
						if discovery == "manifest" && compile.Attr("tsconfig") != nil {
							t.Fatalf("manifest became tsconfig: %s", buildFileText(t, root, "app"))
						}
						if first == nil {
							first = convergeSnapshot(t, root)
						} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("kept closure did not converge: %s", diff)
						}
					}
				}
			})
		}
	}
}

func TestUnknownForwardedMembershipDoesNotWriteAnIncompleteClosure(t *testing.T) {
	for _, forwarding := range []struct{ name, rule, attribute string }{
		{"glob", `filegroup(name = "extra_sources", srcs = glob(["extra.ts"]))`, "srcs"},
		{"select", `filegroup(name = "extra_sources", srcs = select({"//conditions:default":["extra.ts"]}))`, "srcs"},
		{"output group", `filegroup(name = "extra_sources", srcs = ["extra.ts"], output_group = "declarations")`, "output_group"},
		{"opaque", `custom_sources(name = "extra_sources")`, "outputs"},
		{"external", `filegroup(name = "extra_sources", srcs = ["@external//:extra.ts"])`, "external source"},
		{"cycle", "filegroup(name = \"extra_sources\", srcs = [\":next\"])\nalias(name = \"next\", actual = \":extra_sources\")", "cycle"},
		{"empty", `filegroup(name = "extra_sources", srcs = [])`, ""},
	} {
		for _, ownership := range []string{"automatic", "manual", "ignored"} {
			t.Run(forwarding.name+"/"+ownership, func(t *testing.T) {
				keep := ""
				if ownership == "manual" {
					keep = "# keep\n"
				} else if ownership == "ignored" {
					keep = "# gazelle:ignore\n"
				}
				root := writeTree(t, map[string]string{
					"MODULE.bazel":      "module(name = \"unknown_forwarding\")\n",
					"app/BUILD.bazel":   loadDefs + `"ts_compile")` + "\n" + forwarding.rule + "\n" + keep + "ts_compile(name = \"app\", srcs = [\"index.ts\", \":extra_sources\"], # keep\n    deps = [])\n",
					"app/tsconfig.json": `{"files":["index.ts"]}`,
					"app/index.ts":      "export const index = 1;\n",
					"app/extra.ts":      "export const extra = 2;\n",
				})
				before := buildFileBytes(t, root)
				output, err := protoGazelle(t, root)
				if ownership != "automatic" || forwarding.attribute == "" {
					if err != nil {
						t.Fatalf("manual or literal empty membership rejected: %v\n%s", err, output)
					}
					return
				}
				if err == nil || !strings.Contains(output, label.New("unknown_forwarding", "app", "app").String()+" cannot discover its compiler closure") || !strings.Contains(output, label.New("unknown_forwarding", "app", "extra_sources").String()) || !strings.Contains(output, forwarding.attribute) || !strings.Contains(output, "explicit source-file labels") {
					t.Fatalf("unknown forwarding accepted without actionable error: %v\n%s", err, output)
				}
				if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("unsupported membership changed BUILD files: %s", diff)
				}
			})
		}
	}
}

func TestKeptConfigSourceLabelsUseFileIdentity(t *testing.T) {
	const external = "@external//:outside.config.mts"
	for _, selected := range []string{"vitest.config.mts", ":vitest.config.mts", "//:vitest.config.mts", "@//:vitest.config.mts"} {
		t.Run(selected, func(t *testing.T) {
			build := fmt.Sprintf(`# gazelle:exclude outside.config.mts
filegroup(
    name = "vitest_config",
    srcs = [
        %q, # keep
        %q, # keep
    ],
)
`, selected, external)
			root := writeTree(t, map[string]string{
				"MODULE.bazel":       "module(name = \"kept_config_labels\")\n",
				"vitest.config.mts":  "export default {};\n",
				"outside.config.mts": "export default {};\n",
			})
			for _, state := range []string{"authored", "excluded", "generated"} {
				t.Run(state, func(t *testing.T) {
					current := build
					if state != "authored" {
						current = "# gazelle:exclude vitest.config.mts\n" + current
					}
					if state == "generated" {
						current += "genrule(name = \"config_source\", outs = [\"vitest.config.mts\"], cmd = \"echo config > $@\")\n"
					}
					writeFile(t, filepath.Join(root, "BUILD.bazel"), current)
					for pass := range 2 {
						before := convergeSnapshot(t, root)
						output, err := protoGazelle(t, root)
						if state == "excluded" {
							if err == nil || !strings.Contains(output, "imports vitest.config.mts:") || !strings.Contains(output, "excluded or ignored") {
								t.Fatalf("excluded config was not rejected: %v\n%s", err, output)
							}
							if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
								t.Fatalf("rejected config changed BUILD files: %s", diff)
							}
							break
						}
						if err != nil {
							t.Fatalf("config label pass %d: %v\n%s", pass, err, output)
						}
						r := onDiskRule(t, root, "", "filegroup", "vitest_config")
						for _, kept := range []string{selected, external} {
							if !slices.Contains(r.AttrStrings("srcs"), kept) {
								t.Errorf("kept label %q missing from %v", kept, r.AttrStrings("srcs"))
							}
						}
						if state == "generated" {
							wantStrings(t, "producer outputs", onDiskRule(t, root, "", "genrule", "config_source").AttrStrings("outs"), []string{"vitest.config.mts"})
						}
						if pass > 0 {
							if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
								t.Fatalf("kept config labels did not converge: %s", diff)
							}
						}
					}
				})
			}
		})
	}
}

func TestKeptVitestConfigUsesSelectedClosure(t *testing.T) {
	for _, selected := range []string{"safe.config.mts", ":safe.config.mts", "//app:safe.config.mts", "@//app:safe.config.mts", ":safe_config", ":config_alias", "//:vitest_config"} {
		t.Run(selected, func(t *testing.T) {
			tree := map[string]string{
				"MODULE.bazel":          "module(name = \"kept_runtime_config\")\n",
				"BUILD.bazel":           "# gazelle:exclude app/vitest.config.mts\n# gazelle:exclude app/unused.mjs\n",
				"app/tsconfig.json":     `{"files":["index.test.ts"]}`,
				"app/index.test.ts":     "export {};\n",
				"app/vitest.config.mts": "import value from './unused.mjs'; export default { value };\n",
				"app/unused.mjs":        "export default 'unused';\n",
				"app/safe.config.mts":   "import value from './safe/helper.mjs'; export default { value };\n",
				"app/safe/helper.mjs":   "export default 'selected';\n",
				"app/BUILD.bazel": loadDefs + `"ts_test")
filegroup(name = "safe_config", srcs = ["safe.config.mts"])
alias(name = "config_alias", actual = ":safe_config")
ts_test(
    name = "app_test",
    srcs = ["index.test.ts"],
    config = "` + selected + `", # keep
)
`,
			}
			configSource, helper := "app/safe.config.mts", "safe/helper.mjs"
			if selected == "//:vitest_config" {
				tree["vitest.config.mts"] = tree["app/safe.config.mts"]
				tree["safe/helper.mjs"] = tree["app/safe/helper.mjs"]
				configSource, helper = "vitest.config.mts", "//:safe/helper.mjs"
			}
			root := writeTree(t, tree)
			var first map[string]string
			for pass := range 2 {
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("selected config pass %d: %v\n%s", pass, err, output)
				}
				r := onDiskRule(t, root, "app", "ts_test", "app_test")
				if got := r.AttrString("config"); got != selected {
					t.Fatalf("config = %q, want %q", got, selected)
				}
				wantStrings(t, "selected config closure", r.AttrStrings("config_srcs"), []string{helper})
				wantStrings(t, "test sources", r.AttrStrings("srcs"), []string{"index.test.ts"})
				wantStrings(t, "config needs no compiler dependency", r.AttrStrings("deps"), nil)
				if pass == 0 {
					first = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("selected config did not converge: %s", diff)
				}
			}
			writeFile(t, filepath.Join(root, "BUILD.bazel"), tree["BUILD.bazel"]+"# gazelle:exclude "+configSource+"\n")
			before := convergeSnapshot(t, root)
			output, err := protoGazelle(t, root)
			if err == nil || !strings.Contains(output, configSource) || !strings.Contains(output, "excluded or ignored") {
				t.Fatalf("the selected excluded config was not rejected: %v\n%s", err, output)
			}
			if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
				t.Fatalf("rejected config changed BUILD files: %s", diff)
			}
		})
	}
}

func TestUnknownSelectedConfigCannotWithdrawRuntimeInputs(t *testing.T) {
	for _, selection := range []string{"select", "unkept select", "replaced select", "sibling alias", "sibling filegroup", "variable filegroup", "external", "opaque output", "output group", "automatic output group", "None", "absent", "default"} {
		for _, update := range []struct {
			name string
			args []string
		}{
			{"full", nil},
			{"indexed partial", []string{"-r=false", "app"}},
			{"unindexed partial", []string{"-index=false", "-r=false", "app"}},
		} {
			t.Run(selection+"/"+update.name, func(t *testing.T) {
				const selected = `"//worker:vitest.config.mts"`
				root := writeTree(t, map[string]string{
					"MODULE.bazel":               "module(name = \"selected_config_identity\")\n",
					"BUILD.bazel":                "",
					pnpmLockfileName:             poolRepoLock,
					"node_modules/.modules.yaml": "layoutVersion: 5\n",
					"app/tsconfig.json":          `{"compilerOptions":{"types":[]},"files":["index.test.ts"]}`,
					"app/index.test.ts":          "export {};\n",
					"app/BUILD.bazel": loadDefs + `"ts_test")
ts_test(
    name = "app_test",
    srcs = ["index.test.ts"],
    config = ` + selected + `, # keep
)
`,
					"routes/BUILD.bazel":    "alias(name = \"config\", actual = " + selected + ")\n",
					"worker/BUILD.bazel":    "filegroup(name = \"selected\", srcs = [\"vitest.config.mts\"])\n",
					"worker/tsconfig.json":  `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
					"worker/index.ts":       "export const value = 1;\n",
					poolCfg:                 "import './helper.mjs';\n" + poolConfig("./wrangler.jsonc"),
					"worker/helper.mjs":     "export const value = 42;\n",
					"worker/wrangler.jsonc": `{"main":"index.ts"}`,
					"worker/node_modules/@cloudflare/vitest-pool-workers/package.json": `{"name":"@cloudflare/vitest-pool-workers","version":"0.18.4","types":"index.d.ts"}`,
					"worker/node_modules/@cloudflare/vitest-pool-workers/index.d.ts":   "export declare function cloudflareTest(options: unknown): unknown;\n",
				})
				if selection == "automatic output group" {
					writeFile(t, filepath.Join(root, "worker/package.json"), "{}\n")
				}
				if output, err := protoGazelle(t, root); err != nil {
					t.Fatalf("known config setup: %v\n%s", err, output)
				}
				initial := onDiskRule(t, root, "app", "ts_test", "app_test")
				configInputs := []string{"//worker:helper.mjs"}
				if selection == "automatic output group" {
					configInputs = append(configInputs, "//worker:package.json")
				}
				wantLabels(t, "known config inputs", initial.AttrStrings("config_srcs"), configInputs)
				for attr, want := range map[string]string{"wrangler_config": "//worker:wrangler_config", "coverage_provider": "istanbul"} {
					if got := initial.AttrString(attr); got != want {
						t.Fatalf("known config %s = %q, want %q", attr, got, want)
					}
				}
				app := buildFileText(t, root, "app")
				unknown := true
				switch selection {
				case "select", "unkept select", "replaced select":
					app = strings.Replace(app, selected, `select({"//conditions:default": `+selected+`})`, 1)
					if selection != "select" {
						app = strings.Replace(app, "# keep", "", 1)
					}
				case "sibling alias":
					app = strings.Replace(app, selected, `"//routes:config"`, 1)
					writeFile(t, filepath.Join(root, "routes/BUILD.bazel"), "alias(name = \"config\", actual = select({\"//conditions:default\": "+selected+"}))\n")
				case "sibling filegroup", "variable filegroup", "output group", "automatic output group":
					app = strings.Replace(app, selected, `"//worker:selected"`, 1)
					worker := buildFileText(t, root, "worker")
					switch selection {
					case "sibling filegroup":
						worker = strings.Replace(worker, `srcs = ["vitest.config.mts"]`, `srcs = select({"//conditions:default": ["vitest.config.mts"]})`, 1)
					case "variable filegroup":
						worker = "CONFIG_FILES = [\"vitest.config.mts\"]\n" + strings.Replace(worker, `srcs = ["vitest.config.mts"]`, "srcs = CONFIG_FILES", 1)
					default:
						name := "selected"
						if selection == "automatic output group" {
							name = "vitest_config"
							app = strings.Replace(app, `"//worker:selected"`, `"//worker:vitest_config"`, 1)
						}
						if !strings.Contains(worker, "name = \""+name+"\",") {
							t.Fatalf("setup omitted selected filegroup %s", name)
						}
						worker = strings.Replace(worker, "name = \""+name+"\",", "name = \""+name+"\",\n    output_group = \"typescript\",", 1)
					}
					writeFile(t, filepath.Join(root, "worker/BUILD.bazel"), worker)
				case "external":
					app = strings.Replace(app, selected, `"@other//worker:config"`, 1)
				case "opaque output":
					app = strings.Replace(app, selected, `"//opaque:config"`, 1)
					writeFile(t, filepath.Join(root, "opaque/BUILD.bazel"), "load(\"//opaque:defs.bzl\", \"config_source\")\nconfig_source(name = \"config\")\n")
					writeFile(t, filepath.Join(root, "opaque/defs.bzl"), "def config_source(name):\n    native.genrule(name = name, outs = [name + \".mts\"], cmd = \"echo 'export default {}' > $@\")\n")
				case "None":
					app = strings.Replace(app, selected, "None", 1)
					fallthrough
				case "default", "absent":
					unknown = false
					if selection != "None" {
						lines := strings.Split(app, "\n")
						lines = slices.DeleteFunc(lines, func(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "config = ") })
						app = strings.Join(lines, "\n")
					}
				}
				if selection == "None" || selection == "default" || selection == "replaced select" {
					writeFile(t, filepath.Join(root, "app/vitest.config.mts"), "import './helper.mjs'; export default {};\n")
					writeFile(t, filepath.Join(root, "app/helper.mjs"), "export const value = 1;\n")
					unknown = false
				}
				absent := selection == "None" || selection == "absent"
				if absent {
					app = strings.Replace(app, "    name = \"app_test\",", `    name = "app_test",
    data = select({"//conditions:default": ["fixture.json"]}), # keep`, 1)
					writeFile(t, filepath.Join(root, "app/fixture.json"), "{}\n")
					writeFile(t, filepath.Join(root, "app/added.test.ts"), "import { value } from './value'; export const answer = value;\n")
					writeFile(t, filepath.Join(root, "app/value.ts"), "export const value = 42;\n")
					writeFile(t, filepath.Join(root, "app/tsconfig.json"), `{"compilerOptions":{"types":[]},"include":["*.ts"]}`)
				}
				writeFile(t, filepath.Join(root, "app/BUILD.bazel"), app)
				var ordinaryData string
				if absent {
					ordinaryData = bzl.FormatString(onDiskRule(t, root, "app", "ts_test", "app_test").Attr("data"))
				}
				before := buildFileBytes(t, root)
				output, err := protoGazelle(t, root, update.args...)
				if unknown {
					if err == nil || !strings.Contains(output, "cannot establish its selected config root") || !strings.Contains(output, "keep the whole ts_test rule") {
						t.Fatalf("unknown config root did not refuse automatic publication: %v\n%s", err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("unknown config withdrew runtime inputs before rejection: %s", diff)
					}
					app = strings.Replace(app, "ts_test(\n", "# keep\nts_test(\n", 1)
					file, err := rule.LoadData("BUILD.bazel", "app", []byte(app))
					if err != nil {
						t.Fatal(err)
					}
					app = string(file.Format())
					writeFile(t, filepath.Join(root, "app/BUILD.bazel"), app)
				} else {
					if err != nil {
						t.Fatalf("proven config identity was refused: %v\n%s", err, output)
					}
					got := onDiskRule(t, root, "app", "ts_test", "app_test")
					var helpers []string
					if selection == "default" || selection == "replaced select" {
						helpers = []string{"helper.mjs"}
						if got.AttrString("config") != "vitest.config.mts" {
							t.Fatal("default discovery lost its known config")
						}
					} else if selection == "None" && bzl.FormatString(got.Attr("config")) != "None" || selection == "absent" && got.Attr("config") != nil {
						t.Fatal("proven absence selected a user config")
					}
					if absent {
						if got.ShouldKeep() || bzl.FormatString(got.Attr("data")) != ordinaryData {
							t.Fatal("absent config changed ordinary data or automatic test ownership")
						}
						wantLabels(t, "absent config retains source maintenance", got.AttrStrings("srcs"), []string{"index.test.ts", "added.test.ts"})
						wantLabels(t, "absent config retains dependency maintenance", got.AttrStrings("deps"), []string{":app"})
						if ruleEmission(got) != sourceEmission {
							t.Fatal("ordinary data with absent config required test emission")
						}
					}
					wantLabels(t, "known config helper transition", got.AttrStrings("config_srcs"), helpers)
					for _, attr := range []string{"wrangler_config", "coverage_provider"} {
						if got.Attr(attr) != nil {
							t.Fatalf("known config retained stale %s", attr)
						}
					}
				}
				for pass := range 2 {
					before = buildFileBytes(t, root)
					if output, err := protoGazelle(t, root, update.args...); err != nil {
						t.Fatalf("manual or known config repeat %d: %v\n%s", pass, err, output)
					}
					if unknown && buildFileText(t, root, "app") != app {
						t.Fatal("whole-rule ownership changed its config or runtime inputs")
					}
					if pass > 0 {
						if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("manual or known config did not converge: %s", diff)
						}
					}
				}
			})
		}
	}
}

func TestPartialUpdateKeptVitestConfigReadsSiblingTargets(t *testing.T) {
	for _, identity := range []struct{ name, selected, kind, root, settings, consumer string }{
		{"filegroup", "//settings:selected", "filegroup", "", "", ""},
		{"alias", "//routes:config_alias", "filegroup", "", "", ""},
		{"generated", "//settings:make_config", "", "", "", ""},
		{"inherited alias", "//settings:selected", "wrapped_filegroup", "# gazelle:alias_kind wrapped_filegroup filegroup\n", "", ""},
		{"sibling alias", "//settings:selected", "wrapped_filegroup", "", "# gazelle:alias_kind wrapped_filegroup filegroup\n", "# gazelle:alias_kind wrapped_filegroup ts_config\n"},
		{"sibling map chain", "//routes:config_alias", "final_filegroup", "", "# gazelle:map_kind filegroup wrapped_filegroup //:defs.bzl\n# gazelle:map_kind wrapped_filegroup final_filegroup //:defs.bzl\n", "# gazelle:map_kind filegroup unrelated_filegroup //:defs.bzl\n"},
	} {
		t.Run(identity.name, func(t *testing.T) {
			selected := identity.selected
			tree := map[string]string{
				"MODULE.bazel":             "module(name = \"sibling_config\")\n",
				"BUILD.bazel":              identity.root,
				"settings/BUILD.bazel":     identity.settings + identity.kind + "(name = \"selected\", srcs = [\"safe.config.mts\"])\n",
				"routes/BUILD.bazel":       "alias(name = \"config_alias\", actual = \"//settings:selected\")\n",
				"settings/safe.config.mts": "import { value } from './helper.mjs'; export default { value };\n",
				"settings/helper.mjs":      "export const value = 'selected';\n",
				"app/tsconfig.json":        `{"files":["index.ts","index.test.ts"]}`,
				"app/index.ts":             "export const value = 1;\n",
				"app/index.test.ts":        "export {};\n",
				"app/BUILD.bazel": identity.consumer + loadDefs + `"ts_test")
ts_test(
    name = "app_test",
    srcs = ["index.test.ts"],
    config = "` + selected + `", # keep
)
`,
			}
			states := []string{"authored"}
			closure := []string{"//settings:helper.mjs"}
			generated := selected == "//settings:make_config"
			if generated {
				tree["BUILD.bazel"] = "# gazelle:exclude settings/helper.mjs\n# gazelle:exclude settings/wrangler.jsonc\n"
				tree["settings/BUILD.bazel"] = `genrule(
    name = "make_config",
    outs = [":safe.config.mts"],
    cmd = "echo config > $@",
    visibility = ["//visibility:public"],
)
`
				delete(tree, "settings/safe.config.mts")
				delete(tree, "routes/BUILD.bazel")
				tree["settings/wrangler.jsonc"] = `{"name":"stale"}`
				states = []string{"absent", "poisoned"}
				closure = nil
			}
			root := writeTree(t, tree)
			var full map[string]string
			for _, state := range states {
				if state == "poisoned" {
					writeFile(t, filepath.Join(root, "settings/safe.config.mts"), "import { value } from './helper.mjs'; export default { value, test: { poolOptions: { workers: { wrangler: { configPath: './wrangler.jsonc' } } } } };\n")
				}
				t.Run(state, func(t *testing.T) {
					updates := [][]string{nil, {"-index=false", "app"}}
					if !generated && identity.kind != "filegroup" {
						updates = append(updates, nil)
					}
					for _, args := range updates {
						output, err := protoGazelle(t, root, args...)
						if err != nil {
							t.Fatalf("selected sibling config %v: %v\n%s", args, err, output)
						}
						r := onDiskRule(t, root, "app", "ts_test", "app_test")
						if got := r.AttrString("config"); got != selected {
							t.Fatalf("config = %q, want %q", got, selected)
						}
						wantStrings(t, "sibling config closure", r.AttrStrings("config_srcs"), closure)
						wantStrings(t, "test sources", r.AttrStrings("srcs"), []string{"index.test.ts"})
						wantStrings(t, "test dependencies", r.AttrStrings("deps"), []string{":app"})
						if got := r.AttrString("wrangler_config"); got != "" {
							t.Fatalf("stale config selected wrangler_config = %q", got)
						}
						builds := buildFileBytes(t, root)
						if generated && builds["settings/BUILD.bazel"] != tree["settings/BUILD.bazel"] {
							t.Fatalf("generated config changed its producer BUILD:\n%s", builds["settings/BUILD.bazel"])
						}
						if full == nil {
							full = builds
						} else if diff := snapshotDiff(full, builds); diff != "" {
							t.Fatalf("checkout state or partial update changed the full result or sibling BUILD files: %s", diff)
						}
					}
				})
			}
			if generated || identity.kind != "filegroup" {
				return
			}
			writeFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude settings/safe.config.mts\n")
			before := buildFileBytes(t, root)
			for _, args := range [][]string{nil, {"-index=false", "app"}} {
				output, err := protoGazelle(t, root, args...)
				if err == nil || !strings.Contains(output, "settings/safe.config.mts") || !strings.Contains(output, "excluded or ignored") {
					t.Fatalf("excluded sibling config %v was not rejected: %v\n%s", args, err, output)
				}
				if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("rejected sibling config changed BUILD files: %s", diff)
				}
			}
		})
	}
}

func TestGeneratedDeclarationSharedWithKeptTest(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":      "module(name = \"shared_declaration\")\n",
		"app/tsconfig.json": `{"files":["index.ts","index.test.ts"]}`,
		"app/index.ts":      "export type { Value } from './value';\n",
		"app/index.test.ts": "import type { Value } from './value'; export type Checked = Value;\n",
		"app/BUILD.bazel": loadDefs + `"ts_codegen", "ts_test")
ts_codegen(name = "types", outs = ["value.d.ts"], generator = ":generator")
# keep
ts_test(name = "app_test", srcs = [":types", "index.test.ts"], deps = [":app"])
`,
	})
	for _, present := range []bool{false, true} {
		if present {
			writeFile(t, filepath.Join(root, "app/value.d.ts"), "export type Value = string;\n")
		}
		output, err := protoGazelle(t, root)
		if err != nil {
			t.Fatalf("shared declaration present=%t: %v\n%s", present, err, output)
		}
		wantStrings(t, "library deps", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("deps"), []string{":types"})
		wantLabels(t, "kept test inputs", onDiskRule(t, root, "app", "ts_test", "app_test").AttrStrings("srcs"), []string{":types", "index.test.ts"})
	}
}

func TestIgnoredDirectoriesDoNotSupplyUnimportedAncestorData(t *testing.T) {
	const ignoredBuild = "# gazelle:ignore\nexports_files([\"helper.ts\"])\n"
	root := writeTree(t, map[string]string{
		"MODULE.bazel":             "module(name = \"ignored_data\")\n",
		"app/tsconfig.json":        `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
		"app/index.ts":             "export const value = 1;\n",
		"app/helper.ts":            "export { helper } from './private/helper';\n",
		"app/private/BUILD.bazel":  ignoredBuild,
		"app/private/keyfile.json": `{"value":"inert unimported data"}`,
		"app/private/helper.ts":    "export const helper = 1;\n",
	})
	for _, imported := range []bool{false, true} {
		if imported {
			writeFile(t, filepath.Join(root, "app/index.ts"), "export { helper } from './helper.js';\n")
		}
		output, err := protoGazelle(t, root)
		if err != nil {
			t.Fatalf("generation with import=%t: %v\n%s", imported, err, output)
		}
		file, err := rule.LoadFile(filepath.Join(root, "app/BUILD.bazel"), "app")
		if err != nil {
			t.Fatal(err)
		}
		compile := ruleNamed(file.Rules, "ts_compile", "app")
		if compile == nil {
			t.Fatalf("no compile rule in %s", file.Format())
		}
		want := []string{"index.ts"}
		if imported {
			want = append([]string{"//app/private:helper.ts", "helper.ts"}, want...)
		}
		wantLabels(t, "srcs", compile.AttrStrings("srcs"), want)
		wantStrings(t, "unowned closure needs no dependency", compile.AttrStrings("deps"), nil)
		if imported {
			before := convergeSnapshot(t, root)
			for _, args := range [][]string{{"-index=false", "app"}, {"-index=false", "app"}, nil} {
				output, err := protoGazelle(t, root, args...)
				if err != nil {
					t.Fatalf("local import update %v: %v\n%s", args, err, output)
				}
				compile := onDiskRule(t, root, "app", "ts_compile", "app")
				wantLabels(t, "one spelling per source identity", compile.AttrStrings("srcs"), want)
				wantStrings(t, "observed closure needs no dependency", compile.AttrStrings("deps"), nil)
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("index mode changed the local import graph: %s", diff)
				}
			}
		}
		contents, err := os.ReadFile(filepath.Join(root, "app/private/BUILD.bazel"))
		if err != nil || string(contents) != ignoredBuild {
			t.Fatalf("ignored BUILD changed: %s, %v", contents, err)
		}
	}
	writeFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude app/private/helper.ts\n")
	before := convergeSnapshot(t, root)
	output, err := protoGazelle(t, root)
	if err == nil || !strings.Contains(output, "app/private/helper.ts") || !strings.Contains(output, "excluded or ignored") {
		t.Fatalf("file exclusion did not reject the import from an ignored package: %v\n%s", err, output)
	}
	if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
		t.Fatalf("excluded import changed generated files: %s", diff)
	}
}

func TestExcludedSourcesStayOutOfRootProgram(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":  "module(name = \"excluded_owner\")\n",
		"BUILD.bazel":   "# gazelle:exclude scratch.ts\n",
		"tsconfig.json": `{"include":["*.ts"]}`,
		"index.ts":      "export const value = 1;\n",
		"scratch.ts":    "export const scratch = 2;\n",
	})
	output, err := protoGazelle(t, root)
	if err != nil {
		t.Fatalf("generation: %v\n%s", err, output)
	}
	compile := onDiskRule(t, root, "", "ts_compile", "root")
	wantLabels(t, "srcs", compile.AttrStrings("srcs"), []string{"MODULE.bazel", "index.ts"})
}

func TestColdUnindexedScopesPublishOnlyRetainedMetadata(t *testing.T) {
	for _, demanded := range []bool{false, true} {
		t.Run(fmt.Sprintf("demanded=%t", demanded), func(t *testing.T) {
			tree := map[string]string{
				"MODULE.bazel":        "module(name = \"cold_scope\")\n",
				"package.json":        `{"type":"module"}`,
				"unused/package.json": `{"type":"module"}`,
				"app/tsconfig.json":   `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
				"app/index.ts":        "export const value = 1;\n",
			}
			if !demanded {
				tree["app/package.json"] = `{"type":"module"}`
			}
			root := writeTree(t, tree)
			var first map[string]string
			for range 2 {
				output, err := protoGazelle(t, root, "-index=false")
				if err != nil {
					t.Fatalf("cold metadata publication: %v\n%s", err, output)
				}
				wantScope := "package.json"
				if demanded {
					wantScope = "//:package.json"
					var exports int
					for _, r := range loadRules(t, root, "") {
						if r.Kind() == "ts_compile" {
							t.Fatal("raw scope publication retained an empty compiler")
						}
						if r.Kind() != "exports_files" {
							continue
						}
						exports++
						list, err := exportSourceMembership(r)
						if err != nil || len(list.List) != 1 || list.List[0].(*bzl.StringExpr).Value != "package.json" {
							t.Fatalf("metadata declaration includes unused sources: %v", err)
						}
						wantStrings(t, "scope visibility", r.AttrStrings("visibility"), []string{"//:__subpackages__"})
					}
					if exports != 1 {
						t.Fatalf("root scope exports = %d, want one", exports)
					}
				} else if _, err := os.Stat(filepath.Join(root, "BUILD.bazel")); !os.IsNotExist(err) {
					t.Fatalf("unused metadata created a root BUILD: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "unused/BUILD.bazel")); !os.IsNotExist(err) {
					t.Fatalf("unused metadata created a package boundary: %v", err)
				}
				wantLabels(t, "retained direct scope", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("package_scopes"), []string{wantScope})
				if first == nil {
					first = buildFileBytes(t, root)
				} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("cold metadata publication did not converge: %s", diff)
				}
			}
		})
	}
}

func TestRunnableAncestorScopeUsesOnePublishedCompilerOwner(t *testing.T) {
	for _, policy := range []struct {
		name, build, want, refused, consumer string
	}{
		{"restricted export", `exports_files(["package.json"], visibility = ["//app:__pkg__"])`, "//app:__pkg__", "", ""},
		{"explicit local repository", `exports_files(["package.json"], visibility = ["@//app:__pkg__"])`, "//app:__pkg__", "", ""},
		{"canonical local repository", `exports_files(["package.json"], visibility = ["@@//app:__pkg__"])`, "//app:__pkg__", "", ""},
		{"apparent local repository", `exports_files(["package.json"], visibility = ["@scope_visibility//app:__pkg__"])`, "//app:__pkg__", "", ""},
		{"canonical external name cannot grant local access", `exports_files(["package.json"], visibility = ["@@scope_visibility//app:__pkg__"])`, "", "denies access", ""},
		{"external repository cannot grant local access", `exports_files(["package.json"], visibility = ["@other//app:__pkg__"])`, "", "denies access", ""},
		{"private export denies another package", `exports_files(["package.json"], visibility = ["//visibility:private"])`, "", "denies access", ""},
		{"empty export denies another package", `exports_files(["package.json"], visibility = [])`, "", "denies access", ""},
		{"package grant denies another package", `exports_files(["package.json"], visibility = ["//other:__pkg__"])`, "", "denies access", ""},
		{"package grant excludes descendants", `exports_files(["package.json"], visibility = ["//app:__pkg__"])`, "", "denies access", "app/child"},
		{"subpackage grant includes its package", `exports_files(["package.json"], visibility = ["//app:__subpackages__"])`, "//app:__subpackages__", "", ""},
		{"subpackage grant includes descendants", `exports_files(["package.json"], visibility = ["//app:__subpackages__"])`, "//app:__subpackages__", "", "app/child"},
		{"subpackage grant excludes sibling prefixes", `exports_files(["package.json"], visibility = ["//app:__subpackages__"])`, "", "denies access", "application"},
		{"default public export", `exports_files(["package.json"])`, "//visibility:public", "", ""},
		{"package group", "package_group(name = \"readers\", packages = [\"//app\"])\nexports_files(srcs = [\"package.json\"], visibility = [\":readers\"])", "//:readers", "", ""},
		{"package default public", `package(default_visibility = ["//visibility:public"])`, "//visibility:public", "", ""},
		{"package default private", `package(default_visibility = ["//visibility:private"])`, "", "denies access", ""},
		{"new source package grants repository consumers", "", "//:__subpackages__", "", ""},
		{"unknown visibility", "ACCESS = [\"//app:__pkg__\"]\nexports_files([\"package.json\"], visibility = ACCESS)", "", "exports_files visibility is an expression", ""},
		{"unknown membership", "SOURCES = [\"package.json\"]\nexports_files(SOURCES)", "", "exports_files source membership is an expression", ""},
		{"manual owner", "exports_files([\"package.json\"], visibility = [\"//app:__pkg__\"])\n" + loadDefs + "\"ts_compile\")\n# keep\nts_compile(name = \"root\", srcs = [\"package.json\"], visibility = [\"//visibility:public\"])\n", "//visibility:public", "", ""},
	} {
		t.Run("source visibility/"+policy.name, func(t *testing.T) {
			consumer := policy.consumer
			if consumer == "" {
				consumer = "app"
			}
			tree := map[string]string{
				"MODULE.bazel":              "module(name = \"scope_visibility\")\n",
				"package.json":              `{"type":"module"}`,
				consumer + "/BUILD.bazel":   "",
				consumer + "/tsconfig.json": `{"compilerOptions":{"module":"preserve","types":[]},"files":["index.ts"]}`,
				consumer + "/index.ts":      "export const answer: number = 42;\n",
			}
			if policy.build != "" {
				tree["BUILD.bazel"] = policy.build + "\n"
			}
			root := writeTree(t, tree)
			if policy.name == "package default public" {
				if output, err := protoGazelle(t, root); err != nil {
					t.Fatalf("public package default rejected source-only metadata: %v\n%s", err, output)
				}
				file, err := rule.LoadFile(filepath.Join(root, "BUILD.bazel"), "")
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, r := range file.Rules {
					if r.Kind() == "exports_files" {
						found = true
						wantLabels(t, "source export retains package default", r.AttrStrings("visibility"), []string{policy.want})
					}
				}
				if !found {
					t.Fatal("public package default did not publish the required scope")
				}
			}
			if policy.refused != "" {
				before := buildFileBytes(t, root)
				if output, err := protoGazelle(t, root); err == nil || !strings.Contains(output, policy.refused) {
					t.Fatalf("source-only metadata access did not refuse the policy: %v\n%s", err, output)
				}
				if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("refused source-only access changed BUILD files: %s", diff)
				}
			}
			writeFile(t, filepath.Join(root, consumer, "BUILD.bazel"), loadDefs+"\"ts_binary\")\n"+fmt.Sprintf("ts_binary(name = \"run\", entry_point = %q)\n", ":"+packageName(consumer)))
			before := buildFileBytes(t, root)
			output, err := protoGazelle(t, root)
			if policy.refused != "" {
				if err == nil || !strings.Contains(output, policy.refused) {
					t.Fatalf("source access policy did not refuse demanded publication: %v\n%s", err, output)
				}
				if policy.refused == "denies access" && (!strings.Contains(output, label.New("scope_visibility", consumer, packageName(consumer)).String()) || !strings.Contains(output, "package.json") || !strings.Contains(output, "Did you mean")) {
					t.Fatalf("denial omitted the consumer, scope or remedy: %s", output)
				}
				if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("refused access policy changed BUILD files: %s", diff)
				}
				return
			}
			if err != nil {
				t.Fatalf("scope publication: %v\n%s", err, output)
			}
			publisher := onDiskRule(t, root, "", "ts_compile", "root")
			wantLabels(t, "publisher retains source access", publisher.AttrStrings("visibility"), []string{policy.want})
			wantLabels(t, "scope dependency", onDiskRule(t, root, consumer, "ts_compile", packageName(consumer)).AttrStrings("deps"), []string{"//:root"})
			if policy.name != "restricted export" && policy.name != "new source package grants repository consumers" {
				return
			}
			first := buildFileBytes(t, root)
			for _, args := range [][]string{{"-index=false", "-r=false", "app"}, nil} {
				if output, err := protoGazelle(t, root, args...); err != nil {
					t.Fatalf("scope visibility rerun %v: %v\n%s", args, err, output)
				}
				if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("scope visibility changed on rerun %v: %s", args, diff)
				}
			}
		})
	}
	t.Run("private scope retains same-package access", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"MODULE.bazel": "module(name = \"same_package_scope\")\n",
			"BUILD.bazel": "exports_files([\"package.json\"], visibility = [\"//visibility:private\"])\n" + loadDefs + `"ts_test")
ts_test(name = "root_test", runner = "@rules_typescript//ts/runners:node_test")
`,
			"package.json":  `{"type":"module"}`,
			"tsconfig.json": `{"compilerOptions":{"module":"preserve","types":[]},"files":["index.test.ts"]}`,
			"index.test.ts": "export const answer: number = 42;\n",
		})
		if output, err := protoGazelle(t, root); err != nil {
			t.Fatalf("private scope rejected its own package: %v\n%s", err, output)
		}
		wantLabels(t, "private publisher", onDiskRule(t, root, "", "ts_compile", "root").AttrStrings("visibility"), []string{"//visibility:private"})
		wantLabels(t, "same-package scope dependency", onDiskRule(t, root, "", "ts_test", "root_test").AttrStrings("deps"), []string{":root"})
	})
	t.Run("kept visibility cannot widen a newly published source", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"MODULE.bazel": "module(name = \"kept_scope_visibility\")\n",
			"BUILD.bazel": "exports_files([\"package.json\"], visibility = [\"//app:__pkg__\"])\n" + loadDefs + `"ts_compile")
ts_compile(
    name = "root",
    srcs = [],
    visibility = ["//visibility:public"], # keep
)
`,
			"package.json":      `{"type":"module"}`,
			"app/tsconfig.json": `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
			"app/index.ts":      "export const answer = 42;\n",
			"app/BUILD.bazel":   loadDefs + "\"ts_binary\")\nts_binary(name = \"run\", entry_point = \":app\")\n",
		})
		before := buildFileBytes(t, root)
		output, err := protoGazelle(t, root)
		if err == nil || !strings.Contains(output, "kept visibility that differs from its source export") {
			t.Fatalf("a kept publisher attribute widened source access: %v\n%s", err, output)
		}
		if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
			t.Fatalf("refused kept visibility changed BUILD files: %s", diff)
		}
	})
	t.Run("one publisher cannot widen distinct source policies", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"MODULE.bazel": "module(name = \"scope_visibility_conflict\")\n",
			"BUILD.bazel": "exports_files([\"package.json\"], visibility = [\"//app:__pkg__\"])\n" +
				"exports_files([\"nested/package.json\"], visibility = [\"//nested/app:__pkg__\"])\n",
			"package.json":             `{"type":"module"}`,
			"nested/package.json":      `{"type":"module"}`,
			"app/tsconfig.json":        `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
			"app/index.ts":             "export const answer = 42;\n",
			"app/BUILD.bazel":          loadDefs + "\"ts_binary\")\nts_binary(name = \"run\", entry_point = \":app\")\n",
			"nested/app/tsconfig.json": `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
			"nested/app/index.ts":      "export const answer = 42;\n",
			"nested/app/BUILD.bazel":   loadDefs + "\"ts_binary\")\nts_binary(name = \"run\", entry_point = \":app\")\n",
		})
		before := buildFileBytes(t, root)
		output, err := protoGazelle(t, root)
		if err == nil || !strings.Contains(output, "different or unknown source visibility") {
			t.Fatalf("one publisher widened distinct source policies: %v\n%s", err, output)
		}
		if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
			t.Fatalf("incompatible access policies changed BUILD files: %s", diff)
		}
	})
	for _, indexed := range []bool{false, true} {
		for _, sources := range []string{`[]`, `["stale.ts"]`} {
			t.Run(fmt.Sprintf("cold/%t/%s", indexed, sources), func(t *testing.T) {
				root := writeTree(t, map[string]string{
					"MODULE.bazel":      "module(name = \"cold_runtime_scope\")\n",
					"BUILD.bazel":       "exports_files([\"package.json\"], visibility = [\"//visibility:public\"])\n",
					"package.json":      `{"type":"module"}`,
					"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
					"app/index.ts":      "export const answer: number = 42;\n",
					"app/stale.ts":      "export const stale = 0;\n",
					"app/BUILD.bazel": loadDefs + `"ts_compile", "ts_binary")` + "\n" +
						"ts_compile(name = \"app\", srcs = " + sources + ")\nts_binary(name = \"run\", entry_point = \":app\")\n",
				})
				args := []string{fmt.Sprintf("-index=%t", indexed)}
				var first map[string]string
				for pass := 0; pass < 2; pass++ {
					if output, err := protoGazelle(t, root, args...); err != nil {
						t.Fatalf("cold scope update: %v\n%s", err, output)
					}
					compile := onDiskRule(t, root, "app", "ts_compile", "app")
					wantLabels(t, "cold selected roots", compile.AttrStrings("srcs"), []string{"index.ts"})
					wantLabels(t, "cold runtime scope dependency", compile.AttrStrings("deps"), []string{"//:root"})
					wantLabels(t, "cold scope writer", onDiskRule(t, root, "", "ts_compile", "root").AttrStrings("package_scopes"), []string{"package.json"})
					if pass == 0 {
						first = buildFileBytes(t, root)
					} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("cold full generation needed a second pass: %s", diff)
					}
				}
			})
		}
	}
	for _, owner := range []string{"generated", "named", "kept", "scope metadata", "foreign", "data only", "cycle"} {
		t.Run(owner, func(t *testing.T) {
			tree := map[string]string{
				"MODULE.bazel":      "module(name = \"runtime_scope\")\n",
				"BUILD.bazel":       "exports_files([\"package.json\"], visibility = [\"//visibility:public\"])\n",
				"package.json":      `{"type":"module"}`,
				"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
				"app/index.ts":      "export const answer: number = 42;\n",
				"app/BUILD.bazel": loadDefs + `"ts_compile", "ts_binary")
ts_compile(name = "app", srcs = ["index.ts"])
ts_binary(name = "run", entry_point = ":app")
`,
			}
			wantDep := "//:root"
			ownerPackage, ownerName := "", "scope"
			attrs := `srcs = ["package.json"]`
			switch owner {
			case "kept":
				ownerName = "root"
			case "foreign":
				ownerPackage = "holder"
				attrs = `srcs = ["//:package.json"]`
			case "scope metadata":
				attrs = `srcs = [], package_scopes = ["package.json"]`
			case "data only":
				attrs = `srcs = [], data = ["package.json"]`
			case "cycle":
				attrs += `, deps = ["//app:app"]`
			}
			if owner != "generated" {
				filename := path.Join(ownerPackage, "BUILD.bazel")
				tree[filename] += loadDefs + "\"ts_compile\")\n# keep\n" +
					fmt.Sprintf("ts_compile(name = %q, %s, visibility = [\"//visibility:public\"])\n", ownerName, attrs)
				wantDep = label.New("", ownerPackage, ownerName).String()
			}
			root := writeTree(t, tree)
			for pass, args := range [][]string{{"-index=false", "-r=false", "app"}, nil, {"-index=false", "-r=false", "app"}, nil} {
				before := buildFileBytes(t, root)
				if output, err := protoGazelle(t, root, args...); err != nil {
					t.Fatalf("%s update %v: %v\n%s", owner, args, err, output)
				}
				if args != nil {
					after := buildFileBytes(t, root)
					for file, content := range before {
						if !strings.HasPrefix(file, "app/") && after[file] != content {
							t.Fatalf("partial scope generation changed untouched %s", file)
						}
					}
				}
				compile := onDiskRule(t, root, "app", "ts_compile", "app")
				missing := owner == "generated" && pass == 0 || owner == "foreign" || owner == "data only" || owner == "cycle"
				if missing {
					wantLabels(t, "unsupported scope remains compiler metadata", compile.AttrStrings("package_scopes"), []string{"//:package.json"})
					wantLabels(t, "no fabricated or cyclic owner", compile.AttrStrings("deps"), nil)
				} else {
					wantLabels(t, "runtime scope owner", compile.AttrStrings("deps"), []string{wantDep})
					wantLabels(t, "owner supplies original compiler metadata", compile.AttrStrings("package_scopes"), nil)
				}
				writers := 0
				for _, declared := range loadRules(t, root, "") {
					if declared.Kind() == "ts_compile" {
						writers++
					}
				}
				wantWriters := 1
				if owner == "foreign" || owner == "generated" && pass == 0 {
					wantWriters = 0
				}
				if writers != wantWriters {
					t.Fatalf("root scope writers = %d, want %d", writers, wantWriters)
				}
			}
		})
	}
}

func TestAncestorProgramShapeCannotOmitOrDuplicateRuntimeScopePublisher(t *testing.T) {
	for _, source := range []string{"root.test.ts", "scope.d.ts", ""} {
		for _, indexed := range []bool{false, true} {
			for _, ancestor := range []string{"", "parent"} {
				name := fmt.Sprintf("%s/indexed=%t", source, indexed)
				if source == "" {
					name = fmt.Sprintf("importer-only/indexed=%t", indexed)
				}
				if ancestor != "" {
					name += "/new-package=" + ancestor
				}
				t.Run(name, func(t *testing.T) {
					appPackage := path.Join(ancestor, "app")
					publisherName := packageName(ancestor)
					publisherLabel := label.New("", ancestor, publisherName).String()
					tree := map[string]string{
						"MODULE.bazel":                         "module(name = \"ancestor_program_scope\")\n",
						"BUILD.bazel":                          fmt.Sprintf("exports_files([%q], visibility = [\"//visibility:public\"])\n", path.Join(ancestor, "package.json")),
						"package.json":                         `{"type":"module"}`,
						path.Join(appPackage, "tsconfig.json"): `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
						path.Join(appPackage, "index.ts"):      "export const answer: number = 42;\n",
						path.Join(appPackage, "BUILD.bazel"): loadDefs + `"ts_binary")
ts_binary(name = "run", entry_point = ":app")
`,
					}
					tree[path.Join(ancestor, "package.json")] = `{"type":"module"}`
					if source == "" {
						tree[pnpmLockfileName] = "lockfileVersion: '9.0'\nimporters:\n  .: {}\n"
						if ancestor != "" {
							tree[pnpmLockfileName] += "  " + ancestor + ": {}\n"
						}
						tree["node_modules/.modules.yaml"] = "layoutVersion: 5\n"
					} else {
						tree[path.Join(ancestor, "tsconfig.json")] = fmt.Sprintf(`{"compilerOptions":{"types":[]},"files":[%q]}`, source)
						tree[path.Join(ancestor, source)] = "export {};\n"
					}
					root := writeTree(t, tree)
					if output, err := protoGazelle(t, root, "-index=false", "-r=false", appPackage); err != nil {
						t.Fatalf("partial generation: %v\n%s", err, output)
					}
					if got := buildFileText(t, root, ""); got != tree["BUILD.bazel"] {
						t.Fatalf("partial generation changed the untouched ancestor: %s", got)
					}
					if ancestor != "" {
						if _, err := os.Stat(filepath.Join(root, ancestor, "BUILD.bazel")); !os.IsNotExist(err) {
							t.Fatalf("partial generation created the untouched ancestor BUILD: %v", err)
						}
					}
					args := []string{fmt.Sprintf("-index=%t", indexed)}
					var first map[string]string
					for pass := 0; pass < 2; pass++ {
						if output, err := protoGazelle(t, root, args...); err != nil {
							t.Fatalf("full generation: %v\n%s", err, output)
						}
						app := onDiskRule(t, root, appPackage, "ts_compile", "app")
						wantLabels(t, "child selects the scope publisher", app.AttrStrings("deps"), []string{publisherLabel})
						if emit, literal := app.Attr("emit").(*bzl.Ident); !literal || emit.Name != "True" {
							t.Fatal("child lost its emitted runtime demand")
						}
						publisher := onDiskRule(t, root, ancestor, "ts_compile", publisherName)
						wantLabels(t, "publisher has no modules", publisher.AttrStrings("srcs"), nil)
						if publisher.Attr("tsconfig") != nil {
							t.Fatal("scope-only publisher acquired a compiler program")
						}
						wantLabels(t, "publisher retains only its manifest", publisher.AttrStrings("package_scopes"), []string{"package.json"})
						wantLabels(t, "publisher retains public source visibility", publisher.AttrStrings("visibility"), []string{"//visibility:public"})
						for _, target := range loadRules(t, root, ancestor) {
							if target.Kind() == "ts_compile" && target.Name() != publisherName {
								t.Fatal("ancestor acquired a second manifest publisher")
							}
						}
						if ancestor != "" {
							if strings.Contains(buildFileText(t, root, ""), ancestor+"/package.json") {
								t.Fatal("source export remained above the new package boundary")
							}
							exports := 0
							for _, target := range loadRules(t, root, ancestor) {
								if target.Kind() != "exports_files" {
									continue
								}
								members, err := exportSourceMembership(target)
								if err != nil {
									t.Fatal(err)
								}
								for _, member := range members.List {
									if member.(*bzl.StringExpr).Value == "package.json" {
										exports++
										wantLabels(t, "relocated manifest keeps source visibility", target.AttrStrings("visibility"), []string{"//visibility:public"})
									}
								}
							}
							if exports != 1 {
								t.Fatalf("new package has %d manifest exports, want one", exports)
							}
						}
						if isTestFile(source) {
							test := onDiskRule(t, root, ancestor, "ts_test", testTargetName(publisherName))
							wantLabels(t, "source test does not consume the emitted publisher", test.AttrStrings("deps"), nil)
							wantLabels(t, "source test retains its original nearest scope", test.AttrStrings("package_scopes"), []string{"package.json"})
							if emit, literal := test.Attr("emit").(*bzl.Ident); test.Attr("emit") != nil && (!literal || emit.Name != "False") {
								t.Fatal("ancestor test acquired an emitted runtime boundary")
							}
							if slices.Contains(test.AttrStrings("srcs"), "package.json") {
								t.Fatal("ancestor test retained a second manifest source")
							}
						}
						if first == nil {
							first = buildFileBytes(t, root)
						} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("scope publication required another update: %s", diff)
						}
					}
					writeFile(t, filepath.Join(root, appPackage, "BUILD.bazel"), "")
					for pass := 0; pass < 2; pass++ {
						if output, err := protoGazelle(t, root, args...); err != nil {
							t.Fatalf("runtime consumer removal: %v\n%s", err, output)
						}
						for _, target := range loadRules(t, root, ancestor) {
							if target.Kind() == "ts_compile" {
								t.Fatal("source-only consumers retained a dormant manifest publisher")
							}
						}
						if isTestFile(source) {
							test := onDiskRule(t, root, ancestor, "ts_test", testTargetName(publisherName))
							wantLabels(t, "source test retains original scope", test.AttrStrings("package_scopes"), []string{"package.json"})
							wantLabels(t, "source test drops unused publisher", test.AttrStrings("deps"), nil)
						}
					}
				})
			}
		}
	}
}

func TestScopePublisherCannotTakeKeptTestManifestOwnership(t *testing.T) {
	for _, keep := range []string{"rule", "srcs", "element", "manual"} {
		t.Run(keep, func(t *testing.T) {
			prefix, suffix, element, name := "", "", "", "root_test"
			switch keep {
			case "rule":
				prefix = "# keep\n"
			case "srcs":
				suffix = "  # keep"
			case "element":
				element = "  # keep"
			case "manual":
				name = "manual_test"
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel":  "module(name = \"kept_scope_writer\")\n",
				"package.json":  `{"type":"module"}`,
				"tsconfig.json": `{"files":["root.test.ts"]}`,
				"root.test.ts":  "export {};\n",
				"BUILD.bazel": loadDefs + `"ts_test")` + "\n" + prefix + fmt.Sprintf(`ts_test(
    name = %q,
    srcs = [
        "root.test.ts",
        "package.json",%s
    ],%s
)
`, name, element, suffix),
				"app/tsconfig.json": `{"files":["index.ts"]}`,
				"app/index.ts":      "export const answer = 42;\n",
				"app/BUILD.bazel": loadDefs + `"ts_binary")
ts_binary(name = "run", entry_point = ":app")
`,
			})
			if output, err := protoGazelle(t, root); err != nil {
				t.Fatalf("kept manifest generation: %v\n%s", err, output)
			}
			for _, target := range loadRules(t, root, "") {
				if target.Kind() == "ts_compile" {
					t.Fatal("automatic publication introduced a second writer beside the kept test")
				}
			}
			if !slices.Contains(onDiskRule(t, root, "", "ts_test", name).AttrStrings("srcs"), "package.json") {
				t.Fatal("automatic publication removed the kept manifest source")
			}
			wantLabels(t, "manual runtime scope remains explicit", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("package_scopes"), []string{"//:package.json"})
		})
	}
}

func TestTransitiveEmissionPublishesEachNearestScopeInOneUpdate(t *testing.T) {
	for _, test := range []struct {
		name            string
		indexed, mapped bool
	}{
		{"unindexed", false, false},
		{"mapped indexed", true, true},
		{"mapped unindexed", false, true},
		{"indexed", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree := map[string]string{
				"MODULE.bazel":      "module(name = \"transitive_scopes\")\n",
				"package.json":      `{"type":"module"}`,
				"app/package.json":  `{"type":"module"}`,
				"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
				"app/index.ts":      "export { value } from '../shared/lib/index.js';\n",
				"app/BUILD.bazel": loadDefs + `"ts_binary")
ts_binary(name = "run", entry_point = ":app")
`,
				"shared/BUILD.bazel":       "exports_files([\"package.json\"], visibility = [\"//shared/lib:__pkg__\"])\n",
				"shared/package.json":      `{"type":"module","imports":{"#value":"./lib/value.js"}}`,
				"shared/lib/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts","value.ts"]}`,
				"shared/lib/index.ts":      "export { value } from '#value';\n",
				"shared/lib/value.ts":      "export const value: number = 42;\n",
			}
			kind := "ts_compile"
			if test.mapped {
				kind = "scope_compile"
				tree["BUILD.bazel"] = "# gazelle:map_kind ts_compile scope_compile //:scope.bzl\n"
			}
			root := writeTree(t, tree)
			var first map[string]string
			for pass := 0; pass < 2; pass++ {
				if output, err := protoGazelle(t, root, fmt.Sprintf("-index=%t", test.indexed)); err != nil {
					t.Fatalf("transitive emission update: %v\n%s", err, output)
				}
				wantLabels(t, "inferred library dependency", onDiskRule(t, root, "app", kind, "app").AttrStrings("deps"), []string{"//shared/lib:lib"})
				lib := onDiskRule(t, root, "shared/lib", kind, "lib")
				wantLabels(t, "transitive runtime scope", lib.AttrStrings("deps"), []string{"//shared:shared"})
				wantLabels(t, "scope supplied by its owner", lib.AttrStrings("package_scopes"), nil)
				wantLabels(t, "nearest scope writer", onDiskRule(t, root, "shared", kind, "shared").AttrStrings("package_scopes"), []string{"package.json"})
				if test.mapped && !strings.Contains(buildFileText(t, root, "shared"), `load("//:scope.bzl", "scope_compile")`) {
					t.Fatal("late scope owner lost its mapped load")
				}
				if pass == 0 {
					first = buildFileBytes(t, root)
				} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("scope publication needed a second update: %s", diff)
				}
			}
			writeFile(t, filepath.Join(root, "app/BUILD.bazel"), "")
			if output, err := protoGazelle(t, root, fmt.Sprintf("-index=%t", test.indexed)); err != nil {
				t.Fatalf("compiler-only update: %v\n%s", err, output)
			}
			for _, target := range loadRules(t, root, "shared") {
				if target.Kind() == kind {
					t.Fatal("compiler-only scope retained a dormant compiler owner")
				}
			}
		})
	}
}

func TestExportRelocationRefusesBeforePublishingEitherPackage(t *testing.T) {
	const sources = `["child/fixture.json", "retained.json", "sibling/fixture.json"]`
	const policies = `visibility = ["//visibility:public"], licenses = ["notice"]`
	for _, keyword := range []bool{false, true} {
		prefix := ""
		if keyword {
			prefix = "srcs = "
		}
		literal := "exports_files(" + prefix + sources + ", " + policies + ")\n"
		for _, exports := range []struct {
			name, build, diagnostic string
		}{
			{"literal", literal, ""},
			{"kept rule", "# keep\n" + literal, "kept exports_files prevents creating package parent/child"},
			{"kept membership", "exports_files(\n    " + prefix + sources + ", # keep\n    " + policies + ",\n)\n", "kept exports_files prevents creating package parent/child"},
			{"kept moved entry", "exports_files(\n    " + prefix + "[\n        \"child/fixture.json\", # keep\n        \"retained.json\",\n        \"sibling/fixture.json\",\n    ],\n    " + policies + ",\n)\n", "kept exports_files prevents creating package parent/child"},
			{"kept retained entry", "exports_files(\n    " + prefix + "[\n        \"child/fixture.json\",\n        \"retained.json\", # keep\n        \"sibling/fixture.json\",\n    ],\n    " + policies + ",\n)\n", ""},
			{"identifier membership", "SOURCES = " + sources + "\nexports_files(" + prefix + "SOURCES, " + policies + ")\n", "exports_files source membership is an expression"},
			{"compound membership", "exports_files(" + prefix + sources + " + [], " + policies + ")\n", "exports_files source membership is an expression"},
			{"expression entry", "CHILD = \"child/fixture.json\"\nexports_files(" + prefix + "[CHILD, \"retained.json\", \"sibling/fixture.json\"], " + policies + ")\n", "exports_files source membership is an expression"},
			{"visibility expression", "VISIBILITY = [\"//visibility:public\"]\nexports_files(" + prefix + sources + ", visibility = VISIBILITY)\n", "exports_files visibility is an expression"},
			{"licenses expression", "LICENSES = [\"notice\"]\nexports_files(" + prefix + sources + ", licenses = LICENSES)\n", "exports_files licenses is an expression"},
		} {
			for _, indexed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/keyword=%t/indexed=%t", exports.name, keyword, indexed), func(t *testing.T) {
					root := writeTree(t, map[string]string{
						"MODULE.bazel":                "module(name = \"partial_exports\")\n",
						"BUILD.bazel":                 "exports_files([\"package.json\"], visibility = [\"//parent/child:__pkg__\"])\n",
						"package.json":                convergePlainPkg,
						"parent/BUILD.bazel":          exports.build + "# keep\nexports_files([\"other.json\"], visibility = [\"//visibility:private\"])\n",
						"parent/retained.json":        `{}`,
						"parent/sibling/fixture.json": `{}`,
						"parent/other.json":           `{}`,
						"parent/child/tsconfig.json":  `{"files":["index.ts"]}`,
						"parent/child/index.ts":       "export const value = 42;\n",
						"parent/child/fixture.json":   `{}`,
					})
					before := buildFileBytes(t, root)
					for _, partial := range []bool{true, false} {
						args := []string{fmt.Sprintf("-index=%t", indexed)}
						diagnostic := exports.diagnostic
						if partial {
							args = append(args, "-r=false", "parent/child")
							if diagnostic == "" {
								diagnostic = "moving source exports from parent to parent/child requires both BUILD files"
							}
						} else {
							args = append(args, "parent")
						}
						output, err := protoGazelle(t, root, args...)
						if diagnostic != "" {
							if err == nil || !strings.Contains(output, diagnostic) {
								t.Fatalf("partial=%t export transition was not refused with %q: %v\n%s", partial, diagnostic, err, output)
							}
							if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
								t.Fatalf("partial=%t refused export transition wrote BUILD files: %s", partial, diff)
							}
							continue
						}
						if err != nil {
							t.Fatalf("complete export transition: %v\n%s", err, output)
						}
						parent := buildFileText(t, root, "parent")
						if strings.Contains(parent, "child/fixture.json") {
							t.Fatal("complete update left the source export in its parent package")
						}
						retained, unrelated, moved := false, false, false
						for _, r := range loadRules(t, root, "parent") {
							if r.Kind() != "exports_files" {
								continue
							}
							expr := r.Attr("srcs")
							if len(r.Args()) == 1 {
								expr = r.Args()[0]
							}
							list := expr.(*bzl.ListExpr)
							var names []string
							for _, item := range list.List {
								names = append(names, item.(*bzl.StringExpr).Value)
							}
							if slices.Contains(names, "retained.json") {
								retained = true
								wantStrings(t, "unmoved source exports survive", names, []string{"retained.json", "sibling/fixture.json"})
								if (r.Attr("srcs") != nil) != keyword {
									t.Fatal("retained export changed its original call form")
								}
								wantStrings(t, "retained visibility", r.AttrStrings("visibility"), []string{"//visibility:public"})
								wantStrings(t, "retained licenses", r.AttrStrings("licenses"), []string{"notice"})
								if exports.name == "kept retained entry" && !rule.ShouldKeep(list.List[0]) {
									t.Fatal("unmoved source lost its keep marker")
								}
							}
							if slices.Equal(names, []string{"other.json"}) {
								unrelated = r.ShouldKeep() && slices.Equal(r.AttrStrings("visibility"), []string{"//visibility:private"})
							}
						}
						for _, r := range loadRules(t, root, "parent/child") {
							if r.Kind() != "exports_files" {
								continue
							}
							list, err := exportSourceMembership(r)
							if err != nil {
								t.Fatal(err)
							}
							moved = len(list.List) == 1 && list.List[0].(*bzl.StringExpr).Value == "fixture.json"
							wantStrings(t, "relocated visibility", r.AttrStrings("visibility"), []string{"//visibility:public"})
							wantStrings(t, "relocated licenses", r.AttrStrings("licenses"), []string{"notice"})
						}
						if !retained || !unrelated || !moved {
							t.Fatalf("export transition lost declarations: retained=%t unrelated=%t moved=%t", retained, unrelated, moved)
						}
						complete := buildFileBytes(t, root)
						if output, err := protoGazelle(t, root, args...); err != nil {
							t.Fatalf("repeated export transition: %v\n%s", err, output)
						}
						if diff := snapshotDiff(complete, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("export transition did not converge: %s", diff)
						}
					}
				})
			}
		}
	}
	for _, existing := range []bool{false, true} {
		for _, indexed := range []bool{false, true} {
			t.Run(fmt.Sprintf("unrelated unknown exports/existing=%t/indexed=%t", existing, indexed), func(t *testing.T) {
				tree := map[string]string{
					"MODULE.bazel":               "module(name = \"unrelated_exports\")\n",
					"parent/child/package.json":  convergePlainPkg,
					"parent/BUILD.bazel":         "",
					"parent/child/tsconfig.json": `{"files":["index.ts"]}`,
					"parent/child/index.ts":      "export const value = 42;\n",
				}
				exporter := ""
				if existing {
					exporter = "parent/"
					tree["parent/child/BUILD.bazel"] = ""
				}
				exportFile, err := rule.LoadData(exporter+"BUILD.bazel", strings.TrimSuffix(exporter, "/"), []byte("SOURCES = [\"retained.json\"]\nexports_files(srcs = SOURCES)\n"))
				if err != nil {
					t.Fatal(err)
				}
				tree[exporter+"BUILD.bazel"] = string(exportFile.Format())
				tree[exporter+"retained.json"] = `{}`
				root := writeTree(t, tree)
				for _, partial := range []bool{true, false} {
					args := []string{fmt.Sprintf("-index=%t", indexed)}
					if partial {
						args = append(args, "-r=false", "parent/child")
					} else {
						args = append(args, "parent")
					}
					if output, err := protoGazelle(t, root, args...); err != nil {
						t.Fatalf("unrelated exports acquired a membership requirement: %v\n%s", err, output)
					}
					wantStrings(t, "existing boundary keeps the compiler inputs", onDiskRule(t, root, "parent/child", "ts_compile", "child").AttrStrings("srcs"), []string{"index.ts"})
					if got := buildFileBytes(t, root)[exporter+"BUILD.bazel"]; got != tree[exporter+"BUILD.bazel"] {
						t.Fatalf("unrelated export declaration changed:\n%s", got)
					}
				}
			})
		}
	}
}

func TestAncestorPackageScopeSurvivesEffectiveCompilerInputs(t *testing.T) {
	for _, use := range []string{"runtime helper", "declaration helper", "root without edges", "kept discarded root", "kept retained root", "kept missing scope", "kept retained scope", "kept source scope", "kept supplied scope", "kept transitive scope", "kept emitted scope", "kept type-only scope", "kept module through scope"} {
		t.Run(use, func(t *testing.T) {
			suppliedScope := slices.Contains([]string{"kept source scope", "kept supplied scope", "kept transitive scope", "kept emitted scope", "kept type-only scope", "kept module through scope"}, use)
			helper, value := "helper.ts", "value.ts"
			source := "export { value } from '../shared/helper.js';\n"
			helperSource, valueSource := "export { value } from '#value';\n", "export const value = 42;\n"
			if use == "declaration helper" {
				helper, value = "helper.d.ts", "value.d.ts"
				source = "import type { Value } from '../shared/helper.js'; export const value: Value = 42;\n"
				helperSource, valueSource = "export type { Value } from '#value';\n", "export type Value = number;\n"
			}
			if use == "root without edges" || strings.HasPrefix(use, "kept ") && !suppliedScope {
				source = "export const value = 42;\n"
			}
			if use == "kept module through scope" {
				source += "import scope from '../package.json'; export const format = scope.type;\n"
			}
			config := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`
			tree := map[string]string{
				"MODULE.bazel":       "module(name = \"ancestor_scope\")\n",
				"BUILD.bazel":        "exports_files([\"package.json\"], visibility = [\"//visibility:public\"])\n",
				"package.json":       fmt.Sprintf(`{"type":"module","imports":{"#value":"./shared/%s"}}`, value),
				"app/tsconfig.json":  config,
				"app/index.ts":       source,
				"shared/BUILD.bazel": fmt.Sprintf("exports_files([%q, %q], visibility = [\"//app:__pkg__\"])\n", helper, value),
				"shared/" + helper:   helperSource,
				"shared/" + value:    valueSource,
			}
			if use == "declaration helper" {
				tree["app/package.json"] = `{"type":"module"}`
			}
			if strings.HasPrefix(use, "kept ") {
				tree["BUILD.bazel"] += "# gazelle:exclude app/discarded/package.json\n"
				tree["app/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts","discarded/unused.ts"]}`
				tree["app/discarded/unused.ts"] = "export const unused = 1;\n"
				tree["app/discarded/package.json"] = `{"type":"module"}`
				roots := `["index.ts", "//:package.json"]`
				if use == "kept missing scope" || use == "kept retained scope" || suppliedScope {
					roots = `["index.ts"]`
				}
				if suppliedScope {
					attrs := `srcs = ["helper.ts", "value.ts"], package_scopes = ["//:package.json"], emit = False`
					if use == "kept source scope" {
						attrs = `srcs = ["helper.ts", "value.ts", "//:package.json"], emit = False`
					}
					if use == "kept emitted scope" {
						attrs = strings.Replace(attrs, "False", "True", 1)
					}
					if use == "kept type-only scope" {
						attrs = `srcs = ["helper.ts", "value.ts"], type_inputs = ["//:package.json"], emit = False`
					}
					if use == "kept transitive scope" {
						attrs = `srcs = ["helper.ts", "value.ts"], deps = ["//metadata:scope"], emit = False`
						tree["metadata/BUILD.bazel"] = loadDefs + `"ts_compile")` + "\n" +
							`ts_compile(name = "scope", srcs = [], package_scopes = ["//:package.json"], emit = False, visibility = ["//visibility:public"])` + "\n"
					}
					tree["shared/BUILD.bazel"] = loadDefs + `"ts_compile")` + "\n" +
						`ts_compile(name = "scope_owner", ` + attrs + `, visibility = ["//visibility:public"])` + "\n"
				}
				if use == "kept retained root" {
					roots = `["index.ts", "discarded/unused.ts", "//:package.json"]`
				}
				scopes := `[]`
				if use == "kept retained scope" {
					scopes = `["//:package.json"]`
				}
				tree["app/BUILD.bazel"] = loadDefs + `"ts_compile")` + "\n" +
					"ts_compile(\n    name = \"app\",\n    srcs = " + roots + ", # keep\n    package_scopes = " + scopes + ", # keep\n)\n"
			}
			root := writeTree(t, tree)
			var first map[string]string
			for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "-r=false", "app"}, nil} {
				before := buildFileBytes(t, root)
				output, err := protoGazelle(t, root, args...)
				if use == "kept missing scope" || use == "kept type-only scope" || use == "kept module through scope" {
					attr := "package_scopes"
					if use == "kept module through scope" {
						attr = "srcs"
					}
					if err == nil || !strings.Contains(output, "package.json") || !strings.Contains(output, "kept "+attr) {
						t.Fatalf("kept roots lost their required scope with %v: %v\n%s", args, err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("rejected kept scope changed BUILD files: %s", diff)
					}
					continue
				}
				if use == "kept retained root" {
					if err == nil || !strings.Contains(output, "app/discarded/package.json") || !strings.Contains(output, "excluded or ignored") {
						t.Fatalf("retained root omitted its excluded scope with %v: %v\n%s", args, err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("rejected root scope changed BUILD files: %s", diff)
					}
					continue
				}
				if err != nil {
					t.Fatalf("required scope update %v: %v\n%s", args, err, output)
				}
				compile := onDiskRule(t, root, "app", "ts_compile", "app")
				wantSrcs := []string{"index.ts"}
				wantScopes := []string{"//:package.json"}
				var wantTypes []string
				if use == "kept discarded root" {
					wantSrcs = append(wantSrcs, "//:package.json")
					wantScopes = nil
				}
				if suppliedScope {
					wantSrcs = []string{"index.ts"}
					wantScopes = nil
					wantLabels(t, "scope supplier", compile.AttrStrings("deps"), []string{"//shared:scope_owner"})
				}
				if use == "runtime helper" || use == "declaration helper" {
					wantSrcs = append(wantSrcs, "//shared:"+helper, "//shared:"+value)
				}
				if use == "declaration helper" {
					wantScopes = []string{"package.json"}
					wantTypes = []string{"//:package.json"}
				}
				wantLabels(t, "runtime scope and sources", compile.AttrStrings("srcs"), wantSrcs)
				wantLabels(t, "runtime scope metadata", compile.AttrStrings("package_scopes"), wantScopes)
				wantLabels(t, "compiler-only scope", compile.AttrStrings("type_inputs"), wantTypes)
				for _, declared := range loadRules(t, root, "") {
					if declared.Kind() == "ts_compile" {
						t.Fatal("compiler-only ancestor manifest unexpectedly has a runtime owner")
					}
				}
				if use == "runtime helper" || use == "declaration helper" {
					isolated := t.TempDir()
					writeFile(t, filepath.Join(isolated, "app/tsconfig.json"), config)
					for _, input := range slices.Concat(compile.AttrStrings("srcs"), compile.AttrStrings("package_scopes"), compile.AttrStrings("type_inputs")) {
						parsed, err := label.Parse(input)
						if err != nil {
							t.Fatal(err)
						}
						parsed = parsed.Abs("", "app")
						file := path.Join(parsed.Pkg, parsed.Name)
						contents, err := os.ReadFile(filepath.Join(root, file))
						if err != nil {
							t.Fatal(err)
						}
						writeFile(t, filepath.Join(isolated, file), string(contents))
					}
					tsgo, err := newProgramStore().binary()
					if err != nil {
						t.Fatal(err)
					}
					listed, err := listProgram(isolated, "app", tsgo)
					if err != nil {
						t.Fatal(err)
					}
					if !slices.Contains(listed.Edges, importEdge("shared/"+helper, "#value", "shared/"+value)) {
						t.Fatalf("declared inputs cannot reproduce package-private resolution: edges=%+v diagnostics=%v", listed.Edges, listed.Diagnostics)
					}
				}
				if first == nil {
					first = buildFileBytes(t, root)
				} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("scope inputs changed across update modes: %s", diff)
				}
			}
			if use == "kept retained root" || use == "kept missing scope" || use == "kept type-only scope" || use == "kept module through scope" {
				return
			}
			writeFile(t, filepath.Join(root, "BUILD.bazel"), tree["BUILD.bazel"]+"# gazelle:exclude package.json\n")
			before := buildFileBytes(t, root)
			for _, args := range [][]string{nil, {"-index=false", "-r=false", "app"}} {
				output, err := protoGazelle(t, root, args...)
				if err == nil || !strings.Contains(output, "package.json") || !strings.Contains(output, "excluded or ignored") {
					t.Fatalf("excluded required ancestor scope with %v: %v\n%s", args, err, output)
				}
				if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("rejected ancestor scope changed BUILD files: %s", diff)
				}
			}
		})
	}
}

func TestKeptDeclarationScopeRequiresRetainedCompilerInput(t *testing.T) {
	for _, supply := range []string{"absent", "compiler", "scope", "runtime"} {
		t.Run(supply, func(t *testing.T) {
			build := loadDefs + `"ts_compile")` + "\n" + `ts_compile(
    name = "app",
    type_inputs = [], # keep
`
			if supply != "absent" {
				build += `    deps = ["//metadata:scope"], # keep` + "\n"
			}
			build += ")\n"
			tree := map[string]string{
				"MODULE.bazel":       "module(name = \"kept_scope\")\n",
				"BUILD.bazel":        `exports_files(["package.json"], visibility = ["//visibility:public"])`,
				"package.json":       `{"type":"module","imports":{"#value":"./shared/value.d.ts"}}`,
				"app/BUILD.bazel":    build,
				"app/package.json":   `{"type":"module"}`,
				"app/tsconfig.json":  `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
				"app/index.ts":       "import type { Value } from '../shared/helper.js'; export const value: Value = 42;\n",
				"shared/BUILD.bazel": `exports_files(["helper.d.ts", "value.d.ts"], visibility = ["//app:__pkg__"])`,
				"shared/helper.d.ts": "export type { Value } from '#value';\n",
				"shared/value.d.ts":  "export type Value = number;\n",
			}
			if supply != "absent" {
				attrs := `srcs = ["anchor.ts", "package.json"], type_inputs = ["//:package.json"]`
				if supply == "scope" {
					attrs = `srcs = ["anchor.ts", "package.json"], package_scopes = ["//:package.json"]`
				}
				if supply == "runtime" {
					attrs = `srcs = ["anchor.ts", "package.json", "//:package.json"]`
				}
				tree["metadata/BUILD.bazel"] = loadDefs + `"ts_compile")` + "\n" +
					`ts_compile(name = "scope", ` + attrs + `, emit = False, visibility = ["//visibility:public"])` + "\n"
				tree["metadata/anchor.ts"] = "export const anchor = 1;\n"
				tree["metadata/package.json"] = `{"type":"module"}`
			}
			root := writeTree(t, tree)
			var first map[string]string
			for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "-r=false", "app"}} {
				before := buildFileBytes(t, root)
				output, err := protoGazelle(t, root, args...)
				if supply == "absent" {
					if err == nil || !strings.Contains(output, "package.json") || !strings.Contains(output, "kept type_inputs") {
						t.Fatalf("kept declaration scope was discarded with %v: %v\n%s", args, err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("rejected compiler scope changed BUILD files: %s", diff)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s dependency did not supply the compiler scope with %v: %v\n%s", supply, args, err, output)
				}
				compile := onDiskRule(t, root, "app", "ts_compile", "app")
				wantLabels(t, "kept compiler inputs remain empty", compile.AttrStrings("type_inputs"), nil)
				wantLabels(t, "scope stays compiler-only", compile.AttrStrings("srcs"), []string{"index.ts", "//shared:helper.d.ts", "//shared:value.d.ts"})
				wantLabels(t, "local runtime scope remains metadata", compile.AttrStrings("package_scopes"), []string{"package.json"})
				wantLabels(t, "kept dependency supplies scope", compile.AttrStrings("deps"), []string{"//metadata:scope"})
				if first == nil {
					first = buildFileBytes(t, root)
				} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("supplied compiler scope changed across updates: %s", diff)
				}
			}
		})
	}
}

func TestDeclarationOnlyPackageSuppliesImportedDeclarations(t *testing.T) {
	for _, extension := range []string{"ts", "mts", "cts"} {
		t.Run(extension, func(t *testing.T) {
			declaration := "value.d." + extension
			internal := "internal.d." + extension
			module := strings.Replace(extension, "ts", "js", 1)
			root := writeTree(t, map[string]string{
				"MODULE.bazel":         "module(name = \"declaration_input\")\n",
				"BUILD.bazel":          "",
				"types/BUILD.bazel":    fmt.Sprintf("exports_files([%q, %q, \"package.json\"], visibility = [\"//app:__pkg__\"])\n", declaration, internal),
				"types/package.json":   fmt.Sprintf(`{"type":"module","imports":{"#internal":"./%s"}}`, internal),
				"types/tsconfig.json":  fmt.Sprintf(`{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":[%q,%q]}`, declaration, internal),
				"types/" + declaration: "export type { Value } from '#internal';\n",
				"types/" + internal:    "export interface Value { answer: number }\n",
				"app/tsconfig.json":    `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"resolveJsonModule":true},"files":["index.ts"]}`,
				"app/index.ts":         fmt.Sprintf("import type { Value } from '../types/value.%s'; export const value: Value = { answer: 42 };\n", module),
			})
			var first map[string]string
			for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "-r=false", "app"}, nil} {
				output, err := protoGazelle(t, root, args...)
				if err != nil {
					t.Fatalf("declaration scope update %v: %v\n%s", args, err, output)
				}
				for _, r := range loadRules(t, root, "types") {
					if r.Kind() == "ts_compile" || r.Kind() == "ts_test" {
						t.Fatalf("declarations-only package unexpectedly owns a compiler target: %s", r.Name())
					}
				}
				compile := onDiskRule(t, root, "app", "ts_compile", "app")
				wantLabels(t, "declaration closure", compile.AttrStrings("srcs"), []string{"//types:" + declaration, "//types:" + internal, "index.ts"})
				wantLabels(t, "declaration scope is a compiler input", compile.AttrStrings("type_inputs"), []string{"//types:package.json"})
				if first == nil {
					first = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("declaration scope changed on update %v: %s", args, diff)
				}
			}
			original := fmt.Sprintf("import type { Value } from '../types/value.%s'; export const value: Value = { answer: 42 };\n", module)
			writeFile(t, filepath.Join(root, "app/index.ts"), original+"import scope from '../types/package.json'; export const format = scope.type;\n")
			if output, err := protoGazelle(t, root); err != nil {
				t.Fatalf("runtime JSON demand: %v\n%s", err, output)
			}
			compile := onDiskRule(t, root, "app", "ts_compile", "app")
			wantLabels(t, "runtime demand retains the scope File", compile.AttrStrings("srcs"), []string{"//types:" + declaration, "//types:" + internal, "//types:package.json", "index.ts"})
			if compile.Attr("type_inputs") != nil {
				t.Fatal("runtime demand retained a redundant compiler-only input")
			}
			writeFile(t, filepath.Join(root, "app/index.ts"), "export const value = 42;\n")
			if output, err := protoGazelle(t, root, "-r=false", "app"); err != nil {
				t.Fatalf("removed type import: %v\n%s", err, output)
			}
			compile = onDiskRule(t, root, "app", "ts_compile", "app")
			wantLabels(t, "removed declaration closure", compile.AttrStrings("srcs"), []string{"index.ts"})
			if compile.Attr("type_inputs") != nil {
				t.Fatal("removed type import retained compiler metadata")
			}
			writeFile(t, filepath.Join(root, "app/index.ts"), original)
			writeFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude types/package.json\n")
			before := convergeSnapshot(t, root)
			for _, args := range [][]string{nil, {"-r=false", "app"}} {
				output, err := protoGazelle(t, root, args...)
				if err == nil || !strings.Contains(output, "types/"+declaration+" imports types/package.json") || !strings.Contains(output, "excluded or ignored") {
					t.Fatalf("declaration scope bypassed manifest exclusion with %v: %v\n%s", args, err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("rejected declaration scope changed BUILD files: %s", diff)
				}
			}
		})
	}
}

func TestBorrowedGeneratedPackageScopeRejectsMixedLayoutRegardlessOfCheckout(t *testing.T) {
	for _, test := range []struct{ kind, scope string }{{"genrule", "foreign"}, {"ts_codegen", "foreign"}, {"genrule", ""}} {
		t.Run(test.kind+"/"+test.scope, func(t *testing.T) {
			const exports = `exports_files(["value.ts"], visibility = ["//app:__pkg__"])
`
			producer := `genrule(name = "manifest", outs = ["package.json"], cmd = "echo '{}' > $@", visibility = ["//app:__pkg__"])
`
			if test.kind == "ts_codegen" {
				producer = loadDefs + `"ts_codegen")
ts_codegen(name = "manifest", outs = ["package.json"], generator = "//:generator", visibility = ["//app:__pkg__"])
`
			}
			metadata := path.Join(test.scope, "package.json")
			files := map[string]string{
				"MODULE.bazel":        "module(name = \"generated_scope\")\n",
				"BUILD.bazel":         "# gazelle:exclude " + metadata + "\n",
				"app/tsconfig.json":   `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
				"app/index.ts":        "export { value } from '../foreign/value.js';\n",
				"foreign/BUILD.bazel": exports,
				"foreign/value.ts":    "export const value = 42;\n",
			}
			files[path.Join(test.scope, "BUILD.bazel")] = producer + files[path.Join(test.scope, "BUILD.bazel")]
			root := writeTree(t, files)
			before := convergeSnapshot(t, root)
			for _, state := range []string{"absent", "excluded stale"} {
				if state != "absent" {
					exported := "./value.ts"
					if test.scope == "" {
						exported = "./foreign/value.ts"
					}
					writeFile(t, filepath.Join(root, filepath.FromSlash(metadata)), fmt.Sprintf(`{"type":"module","exports":%q,"main":%q,"types":%q}`, exported, exported, exported))
				}
				for _, args := range [][]string{nil, {"-r=false", "app"}} {
					output, err := protoGazelle(t, root, args...)
					if err == nil || !strings.Contains(output, "with generated package scope "+metadata) ||
						!strings.Contains(output, "authored module is checked at its source path") || strings.Contains(output, "excluded or ignored") {
						t.Fatalf("%s with %v lost the generated scope layout conflict: %v\n%s", state, args, err, output)
					}
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("%s layout conflict changed BUILD ownership: %s", state, diff)
					}
				}
			}
			if test.scope == "" {
				return
			}
			writeFile(t, filepath.Join(root, "foreign/BUILD.bazel"), exports+`exports_files(["package.json"], visibility = ["//app:__pkg__"])
`)
			writeFile(t, filepath.Join(root, "foreign/package.json"), `{"type":"module"}`)
			before = convergeSnapshot(t, root)
			for _, args := range [][]string{nil, {"-r=false", "app"}} {
				output, err := protoGazelle(t, root, args...)
				if err == nil || !strings.Contains(output, "foreign/value.ts imports foreign/package.json") || !strings.Contains(output, "excluded or ignored") {
					t.Fatalf("authored scope exclusion with %v was not preserved: %v\n%s", args, err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("authored exclusion changed BUILD ownership: %s", diff)
				}
			}
			writeFile(t, filepath.Join(root, "BUILD.bazel"), "")
			output, err := protoGazelle(t, root)
			if err != nil {
				t.Fatalf("authored package scope failed: %v\n%s", err, output)
			}
			compile := onDiskRule(t, root, "app", "ts_compile", "app")
			wantLabels(t, "authored module inputs", compile.AttrStrings("srcs"), []string{"index.ts", "//foreign:value.ts"})
			wantLabels(t, "authored package scope", compile.AttrStrings("package_scopes"), []string{"//foreign:package.json"})
		})
	}
}

func TestUpdateOnlyPreservesFilteredSubdirectoryInputs(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"contained_inputs\")\n",
		"BUILD.bazel": "# gazelle:generation_mode update_only\n" +
			"# gazelle:exclude nested/secret.json\n# gazelle:exclude nested/scratch.ts\n",
		"tsconfig.json": `{"compilerOptions":{"resolveJsonModule":true,"module":"preserve","moduleResolution":"bundler"},"include":["**/*.ts"]}`,
		"index.ts": "import data from './nested/value.json';\n" +
			"import { helper } from './nested/helper';\nexport const value = data.value + helper;\n",
		"nested/value.json":  `{"value":1}`,
		"nested/secret.json": `{"value":"inert excluded data"}`,
		"nested/helper.ts":   "export const helper = 1;\n",
		"nested/scratch.ts":  "export const scratch = 2;\n",
	})
	output, err := protoGazelle(t, root)
	if err != nil {
		t.Fatalf("generation: %v\n%s", err, output)
	}
	compile := onDiskRule(t, root, "", "ts_compile", "root")
	wantLabels(t, "srcs", compile.AttrStrings("srcs"), []string{"MODULE.bazel", "index.ts", "nested/helper.ts", "nested/value.json"})
	if _, err := os.Stat(filepath.Join(root, "nested/BUILD.bazel")); !os.IsNotExist(err) {
		t.Fatalf("contained subdirectory became a package: %v", err)
	}
	writeFile(t, filepath.Join(root, "index.ts"), "import './nested/secret.json';\nexport {};\n")
	before := convergeSnapshot(t, root)
	output, err = protoGazelle(t, root)
	if err == nil || !strings.Contains(output, "nested/secret.json") || !strings.Contains(output, "excluded or ignored") {
		t.Fatalf("aggregated excluded import did not fail: %v\n%s", err, output)
	}
	if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
		t.Fatalf("excluded import changed generated files: %s", diff)
	}
}

func TestPartialUpdateObservesSharedTsconfigBase(t *testing.T) {
	const baseBuild = loadDefs + `"ts_config")

ts_config(
    name = "tsconfig",
    src = "tsconfig.json",
    visibility = ["//visibility:public"],
)
`
	for _, tc := range []struct {
		name    string
		exclude string
	}{
		{name: "eligible"},
		{name: "excluded file", exclude: "settings/tsconfig.json"},
		{name: "excluded directory", exclude: "settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := ""
			if tc.exclude != "" {
				build = "# gazelle:exclude " + tc.exclude + "\n"
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel":           "module(name = \"shared_config\")\n",
				"BUILD.bazel":            build,
				"settings/BUILD.bazel":   baseBuild,
				"settings/tsconfig.json": `{"compilerOptions":{"strict":true}}`,
				"app/tsconfig.json":      `{"extends":"../settings/tsconfig.json","files":["index.ts"]}`,
				"app/index.ts":           "export const value = 1;\n",
			})
			before := convergeSnapshot(t, root)
			output, err := protoGazelle(t, root, "-index=false", "app")
			if tc.exclude != "" {
				if err == nil || !strings.Contains(output, "settings/tsconfig.json") || !strings.Contains(output, "excluded or ignored") {
					t.Fatalf("excluded shared config was not rejected: %v\n%s", err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("rejected config changed BUILD files: %s", diff)
				}
				return
			}
			if err != nil {
				t.Fatalf("eligible shared config outside the update was rejected: %v\n%s", err, output)
			}
			cfg := onDiskRule(t, root, "app", "ts_config", "tsconfig")
			wantStrings(t, "shared config deps", cfg.AttrStrings("deps"), []string{"//settings:tsconfig"})
			compile := onDiskRule(t, root, "app", "ts_compile", "app")
			wantStrings(t, "app srcs", compile.AttrStrings("srcs"), []string{"index.ts"})
			if got := compile.AttrString("tsconfig"); got != ":tsconfig" {
				t.Errorf("app tsconfig = %q, want :tsconfig", got)
			}
			if got := buildFileText(t, root, "settings"); got != baseBuild {
				t.Errorf("limited update changed the shared config BUILD:\n%s", got)
			}
			before = convergeSnapshot(t, root)
			output, err = protoGazelle(t, root, "-index=false", "app")
			if err != nil {
				t.Fatalf("repeat limited update: %v\n%s", err, output)
			}
			if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
				t.Fatalf("repeat limited update changed BUILD files: %s", diff)
			}
		})
	}
}

func TestUntouchedCompilerOwnersDoNotLeakPrivateSources(t *testing.T) {
	for _, spelling := range []struct{ name, kind, directives string }{
		{"canonical", "ts_compile", ""},
		{"aliased", "custom_compile", "# gazelle:alias_kind custom_compile ts_compile\n"},
		{"mapped", "custom_compile", "# gazelle:map_kind ts_compile custom_compile //:defs.bzl\n"},
		{"mapped alias", "custom_compile", "# gazelle:map_kind ts_compile mapped_compile //:defs.bzl\n# gazelle:alias_kind custom_compile mapped_compile\n"},
	} {
		for _, input := range []struct {
			name, target, source string
			generated            bool
		}{
			{name: "local conventional", target: "lib", source: "lib/value.ts"},
			{name: "local explicit", target: "library", source: "lib/value.ts"},
			{name: "foreign declaration", target: "lib", source: "fixtures/value.d.ts"},
			{name: "foreign generated declaration", target: "lib", source: "fixtures/value.d.ts", generated: true},
		} {
			for _, update := range []struct {
				name, directive string
				args            []string
			}{
				{"language opt-out", "# gazelle:lang proto\n", nil},
				{"ignored owner", "# gazelle:ignore\n", nil},
				{"indexed sibling", "", []string{"-r=false", "app"}},
				{"unindexed sibling", "", []string{"-index=false", "-r=false", "app"}},
			} {
				t.Run(spelling.name+"/"+input.name+"/"+update.name, func(t *testing.T) {
					load := loadDefs + `"ts_compile")` + "\n"
					if spelling.kind != "ts_compile" {
						load = "load(\"//:defs.bzl\", \"" + spelling.kind + "\")\n"
					}
					source, configSource := "value.ts", "value.ts"
					contents := "export const value = 42;\n"
					importer := "export { value } from '../lib/value';\n"
					exports := "exports_files([\"value.ts\"], visibility = [\"//visibility:private\"])\n"
					if isDeclarationFile(input.source) {
						source, configSource = "//fixtures:value.d.ts", "../fixtures/value.d.ts"
						contents = "export interface Value { value: string }\n"
						importer = "export type { Value } from '../fixtures/value';\n"
						exports = ""
					}
					files := map[string]string{
						"MODULE.bazel": "module(name = \"untouched_owner\")\n",
						"BUILD.bazel":  "",
						"lib/BUILD.bazel": load + spelling.directives + update.directive + exports +
							fmt.Sprintf("%s(name = %q, srcs = [%q], tsconfig = \"tsconfig.json\", visibility = [\"//visibility:public\"])\n", spelling.kind, input.target, source),
						"lib/tsconfig.json": fmt.Sprintf(`{"files":[%q]}`, configSource),
						"app/BUILD.bazel": loadDefs + `"ts_compile")` + "\n" +
							fmt.Sprintf("ts_compile(name = \"app\", srcs = [\"index.ts\"], deps = [%q])\n", "//lib:"+input.target),
						"app/tsconfig.json": `{"files":["index.ts"]}`,
						"app/index.ts":      importer,
					}
					if input.generated {
						files["fixtures/BUILD.bazel"] = `genrule(name = "generated", outs = ["value.d.ts"], cmd = "unused", visibility = ["//lib:__pkg__"])` + "\n"
					} else {
						files[input.source] = contents
						if isDeclarationFile(input.source) {
							files["fixtures/BUILD.bazel"] = "exports_files([\"value.d.ts\"], visibility = [\"//lib:__pkg__\"])\n"
						}
					}
					ownerFile, err := rule.LoadData("BUILD.bazel", "lib", []byte(files["lib/BUILD.bazel"]))
					if err != nil {
						t.Fatal(err)
					}
					files["lib/BUILD.bazel"] = string(ownerFile.Format())
					root := writeTree(t, files)
					states := []string{"initial", "repeated"}
					if input.generated {
						states = []string{"cold", "materialized", "removed"}
					}
					var first map[string]string
					for _, state := range states {
						if state == "materialized" {
							writeFile(t, filepath.Join(root, input.source), contents)
						} else if state == "removed" {
							if err := os.Remove(filepath.Join(root, input.source)); err != nil {
								t.Fatal(err)
							}
						}
						if output, err := protoGazelle(t, root, update.args...); err != nil {
							t.Fatalf("%s update: %v\n%s", state, err, output)
						}
						consumer := onDiskRule(t, root, "app", "ts_compile", "app")
						wantLabels(t, "consumer retains the compiler dependency", consumer.AttrStrings("deps"), []string{"//lib:" + input.target})
						wantStrings(t, "consumer cannot borrow the private source", consumer.AttrStrings("srcs"), []string{"index.ts"})
						owner := onDiskRule(t, root, "lib", spelling.kind, input.target)
						wantStrings(t, "untouched owner retains its declaration", owner.AttrStrings("srcs"), []string{source})
						if got := buildFileText(t, root, "lib"); got != files["lib/BUILD.bazel"] {
							t.Fatalf("%s changed the untouched owner: %s", state, lineDiff(files["lib/BUILD.bazel"], got))
						}
						if first == nil {
							first = buildFileBytes(t, root)
						} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("%s changed ownership or package boundaries: %s", state, diff)
						}
					}
				})
			}
		}
	}
}

func TestPartialIndexTestClaimCannotExposePrivateCompilerSource(t *testing.T) {
	for _, spelling := range []struct{ name, kind, directives string }{
		{"canonical", "ts_compile", ""},
		{"aliased", "custom_compile", "# gazelle:alias_kind custom_compile ts_compile\n"},
		{"mapped", "custom_compile", "# gazelle:map_kind ts_compile custom_compile //:defs.bzl\n"},
		{"mapped alias", "custom_compile", "# gazelle:map_kind ts_compile mapped_compile //:defs.bzl\n# gazelle:alias_kind custom_compile mapped_compile\n"},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			load := loadDefs + `"ts_compile")` + "\n"
			if spelling.kind != "ts_compile" {
				load = "load(\"//:defs.bzl\", \"" + spelling.kind + "\")\n"
			}
			ownerBuild := load + spelling.directives +
				"exports_files([\"value.d.ts\"], visibility = [\"//tests:__pkg__\"])\n" +
				spelling.kind + "(name = \"owner\", srcs = [\"value.d.ts\"], visibility = [\"//visibility:public\"])\n"
			ownerFile, err := rule.LoadData("BUILD.bazel", "lib", []byte(ownerBuild))
			if err != nil {
				t.Fatal(err)
			}
			ownerBuild = string(ownerFile.Format())
			root := writeTree(t, map[string]string{
				"MODULE.bazel":        "module(name = \"partial_owner_index\")\n",
				"BUILD.bazel":         "",
				"lib/BUILD.bazel":     ownerBuild,
				"lib/value.d.ts":      "export type Value = string;\n",
				"app/tsconfig.json":   `{"files":["index.ts"]}`,
				"app/index.ts":        "export type { Value } from '../lib/value';\n",
				"tests/tsconfig.json": `{"files":["index.test.ts"]}`,
				"tests/index.test.ts": "export const checked = true;\n",
				"tests/BUILD.bazel": loadDefs + `"ts_test")` + "\n" +
					"# keep\nts_test(name = \"checks\", srcs = [\"index.test.ts\", \"//lib:value.d.ts\"])\n",
			})
			var first map[string]string
			for _, args := range [][]string{{"-index=false", "-r=false", "app", "tests"}, nil, {"-index=false", "-r=false", "app", "tests"}} {
				if output, err := protoGazelle(t, root, args...); err != nil {
					t.Fatalf("update %v: %v\n%s", args, err, output)
				}
				consumer := onDiskRule(t, root, "app", "ts_compile", "app")
				wantStrings(t, "public compiler remains the dependency", consumer.AttrStrings("deps"), []string{"//lib:owner"})
				wantStrings(t, "private raw declaration is not borrowed", consumer.AttrStrings("srcs"), []string{"index.ts"})
				wantLabels(t, "test retains its independent claim", onDiskRule(t, root, "tests", "ts_test", "checks").AttrStrings("srcs"), []string{"index.test.ts", "//lib:value.d.ts"})
				if got := buildFileText(t, root, "lib"); got != ownerBuild {
					t.Fatalf("update changed the declared compiler or raw-file visibility:\n%s", got)
				}
				if first == nil {
					first = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("index population changed the generated graph: %s", diff)
				}
			}
		})
	}
}

func TestPartialUpdatePreservesOwnersWithoutPromotingBorrowedSources(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"partial_inputs\")\n",
		"BUILD.bazel": "# gazelle:generation_mode update_only\n" +
			"# gazelle:exclude lib/nested/excluded.ts\n",
		"package.json":               `{"name":"partial_inputs","type":"module"}`,
		pnpmLockfileName:             "lockfileVersion: '9.0'\nimporters:\n  .: {}\n",
		"node_modules/.modules.yaml": "",
		"vitest.config.mts":          "import { helper } from './config/helper.mjs';\nexport default { test: { name: helper } };\n",
		"config/helper.mjs":          "export const helper = 'contained';\n",
		"app/BUILD.bazel":            "",
		"app/tsconfig.json":          `{"include":["*.ts"]}`,
		"app/index.ts":               "export { helper } from '../lib/nested/helper'; export { authored } from '../fixtures/authored'; export { kept } from '../fixtures/kept'; export { b } from '../b'; export { privateHelper } from './private/helper'; export { foreignHelper } from './foreign/helper'; export { owned } from './private/owned'; export { value } from '../dep/value'; export { h } from './helpers/h'; export { nested } from './helpers/nested/h'; export { inherited } from './helpers/inherited/h'; export { enabled } from './helpers/enabled/h';\n",
		"app/index.test.ts":          "export const test = 1;\n",
		"app/private/BUILD.bazel": loadDefs + `"ts_compile")
# gazelle:ignore
exports_files(["helper.ts"], visibility = ["//visibility:public"])
ts_compile(name = "private", srcs = ["owned.ts"], visibility = ["//visibility:public"])
`,
		"app/private/helper.ts":    "export const privateHelper = 4;\n",
		"app/private/owned.ts":     "export const owned = 6;\n",
		"app/foreign/BUILD.bazel":  `exports_files(["helper.ts", "package.json"], visibility = ["//visibility:public"])`,
		"app/foreign/package.json": `{"name":"foreign"}`,
		"app/foreign/helper.ts":    "export const foreignHelper = 5;\n",
		"app/helpers/BUILD.bazel": "# gazelle:lang proto\n" +
			"exports_files([\"h.ts\", \"nested/h.ts\"], visibility = [\"//visibility:public\"])\n",
		"app/helpers/h.ts":                  "export const h = 8;\n",
		"app/helpers/nested/h.ts":           "export const nested = 9;\n",
		"app/helpers/inherited/BUILD.bazel": "exports_files([\"h.ts\"], visibility = [\"//visibility:public\"])\n",
		"app/helpers/inherited/h.ts":        "export const inherited = 10;\n",
		"app/helpers/enabled/BUILD.bazel":   "# gazelle:lang\nexports_files([\"h.ts\"], visibility = [\"//visibility:public\"])\n",
		"app/helpers/enabled/h.ts":          "export const enabled = 11;\n",
		"lib/BUILD.bazel":                   "",
		"lib/tsconfig.json":                 `{"include":["**/*.ts"]}`,
		"lib/nested/helper.ts":              "export const helper = 1;\n",
		"lib/nested/excluded.ts":            "export const helper = 2;\n",
		"a/BUILD.bazel":                     "",
		"a/tsconfig.json":                   `{"files":["index.ts"]}`,
		"a/index.ts":                        "export { b } from '../b'; export { shared } from '../fixtures/shared'; export { enabled } from '../app/helpers/enabled/h';\n",
		"b/BUILD.bazel":                     "",
		"b/tsconfig.json":                   `{"files":["index.ts"]}`,
		"b/index.ts":                        "import { shared } from '../fixtures/shared'; import { privateHelper } from '../app/private/helper'; import { foreignHelper } from '../app/foreign/helper'; import { h } from '../app/helpers/h'; import { nested } from '../app/helpers/nested/h'; import { inherited } from '../app/helpers/inherited/h'; export const b = shared + privateHelper + foreignHelper + h + nested + inherited;\n",
		"fixtures/BUILD.bazel":              `exports_files(["shared.ts", "authored.ts", "kept.ts"], visibility = ["//visibility:public"])`,
		"fixtures/shared.ts":                "export const shared = 1;\n",
		"fixtures/authored.ts":              "export const authored = 2;\n",
		"fixtures/kept.ts":                  "export const kept = 3;\n",
		"generated/BUILD.bazel":             `genrule(name = "source", outs = ["shared.ts", "explicit.ts", "kept.ts"], cmd = "touch $(OUTS)", visibility = ["//visibility:public"])`,
		"generated_a/BUILD.bazel":           "",
		"generated_a/tsconfig.json":         `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
		"generated_a/index.ts":              "export const a = 1; export { shared } from '../generated/shared.js'; export { explicit } from '../generated/explicit.js'; export { kept } from '../generated/kept.js';\n",
		"generated_b/BUILD.bazel":           "",
		"generated_b/tsconfig.json":         `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
		"generated_b/index.ts":              "export { a } from '../generated_a/index.js'; export { shared } from '../generated/shared.js';\n",
		"owners/BUILD.bazel": loadDefs + `"ts_compile")
ts_compile(name = "explicit", srcs = ["//fixtures:authored.ts", "//generated:explicit.ts"], visibility = ["//app:__pkg__", "//generated_a:__pkg__"])
`,
		"kept/BUILD.bazel": `load("//:defs.bzl", "kept_compile")
# gazelle:map_kind ts_compile kept_compile //:defs.bzl
# keep
kept_compile(name = "kept", srcs = ["//fixtures:kept.ts", "//generated:kept.ts"], visibility = ["//app:__pkg__", "//generated_a:__pkg__"])
`,
		"dep/BUILD.bazel": `load("//:defs.bzl", "custom_compile")
# gazelle:map_kind ts_compile custom_compile //:defs.bzl
# keep
custom_compile(name = "dep", srcs = ["value.ts"], visibility = ["//visibility:public"])
`,
		"dep/package.json": `{"name":"foreign-owner"}`,
		"dep/value.ts":     "export const value = 7;\n",
	})
	output, err := protoGazelle(t, root)
	if err != nil {
		t.Fatalf("initial generation: %v\n%s", err, output)
	}
	borrowedSources := []string{"//fixtures:shared.ts", "//app/private:helper.ts", "//app/foreign:helper.ts", "//app/helpers:h.ts", "//app/helpers:nested/h.ts", "//app/helpers/inherited:h.ts"}
	borrowedScopes := []string{"//:package.json", "//app/foreign:package.json"}
	before := convergeSnapshot(t, root)
	for i, dirs := range [][]string{nil, {"-r=false", "generated_a"}, nil, {"b"}, {"a"}} {
		if i > 0 {
			output, err = protoGazelle(t, root, dirs...)
			if err != nil {
				t.Fatalf("update %v: %v\n%s", dirs, err, output)
			}
		}
		for _, pkg := range []string{"a", "b"} {
			r := onDiskRule(t, root, pkg, "ts_compile", pkg)
			srcs := append(slices.Clone(borrowedSources), "index.ts")
			scopes := borrowedScopes
			var deps []string
			if pkg == "a" {
				srcs = []string{"index.ts"}
				scopes = nil
				deps = []string{"//app", "//b"}
			}
			wantLabels(t, pkg+" retains only inputs absent from dependencies", r.AttrStrings("srcs"), srcs)
			wantLabels(t, pkg+" retains only scopes absent from dependencies", r.AttrStrings("package_scopes"), scopes)
			wantStrings(t, pkg+" has no reverse borrowing dependency", r.AttrStrings("deps"), deps)
		}
		for _, pkg := range []string{"generated_a", "generated_b"} {
			r := onDiskRule(t, root, pkg, "ts_compile", pkg)
			srcs := []string{"//generated:shared.ts", "index.ts"}
			scopes := []string{"//:package.json"}
			deps := []string{"//kept", "//owners:explicit"}
			if pkg == "generated_b" {
				srcs = []string{"index.ts"}
				scopes = nil
				deps = []string{"//generated_a"}
			}
			wantLabels(t, pkg+" retains only generated inputs absent from dependencies", r.AttrStrings("srcs"), srcs)
			wantLabels(t, pkg+" retains only scopes absent from generated dependencies", r.AttrStrings("package_scopes"), scopes)
			wantStrings(t, pkg+" has no generated borrowing cycle", r.AttrStrings("deps"), deps)
		}
		for _, file := range []string{"shared.ts", "explicit.ts", "kept.ts"} {
			if _, err := os.Stat(filepath.Join(root, "generated", file)); !os.IsNotExist(err) {
				t.Fatalf("generation materialized output %s: %v", file, err)
			}
		}
		app := onDiskRule(t, root, "app", "ts_compile", "app")
		wantLabels(t, "app keeps its own sources while b supplies borrowed inputs", app.AttrStrings("srcs"), []string{"helpers/enabled/h.ts", "index.ts"})
		wantLabels(t, "app retains its unsupplied package scope", app.AttrStrings("package_scopes"), []string{"//dep:package.json"})
		wantLabels(t, "app keeps its forward dependency and authored owners", app.AttrStrings("deps"), []string{"//app/private", "//b", "//dep", "//kept", "//lib", "//owners:explicit"})
		wantStrings(t, "foreign mapped owner keeps its sources", onDiskRule(t, root, "dep", "custom_compile", "dep").AttrStrings("srcs"), []string{"value.ts"})
		wantStrings(t, "explicit owner keeps its authored and generated sources", onDiskRule(t, root, "owners", "ts_compile", "explicit").AttrStrings("srcs"), []string{"//fixtures:authored.ts", "//generated:explicit.ts"})
		wantStrings(t, "mapped owner keeps its cross-package sources", onDiskRule(t, root, "kept", "kept_compile", "kept").AttrStrings("srcs"), []string{"//fixtures:kept.ts", "//generated:kept.ts"})
		if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
			t.Fatalf("update %v changed the shared-source graph: %s", dirs, diff)
		}
	}
	fullTest := onDiskRule(t, root, "app", "ts_test", "app_test")
	wantLabels(t, "full update retains explicit borrowed inputs", fullTest.AttrStrings("srcs"), append(slices.Clone(borrowedSources), "index.test.ts"))
	wantLabels(t, "full update selects only the executable test", fullTest.AttrStrings("test_srcs"), []string{"index.test.ts"})
	wantLabels(t, "full update retains the application dependency", fullTest.AttrStrings("deps"), []string{":app"})
	if emit, literal := fullTest.Attr("emit").(*bzl.Ident); fullTest.Attr("emit") != nil && (!literal || emit.Name != "False") {
		t.Fatal("full update acquired an emitted test boundary")
	}
	beforePartial := buildFileBytes(t, root)
	output, err = protoGazelle(t, root, "app")
	if err != nil {
		t.Fatalf("partial update rejected an indexed contained source: %v\n%s", err, output)
	}
	compile := onDiskRule(t, root, "app", "ts_compile", "app")
	wantStrings(t, "contained, authored and kept owners", compile.AttrStrings("deps"), []string{"//app/private", "//b", "//dep", "//kept", "//lib", "//owners:explicit"})
	wantLabels(t, "partial update preserves application sources", compile.AttrStrings("srcs"), []string{"helpers/enabled/h.ts", "index.ts"})
	wantLabels(t, "partial update preserves the unsupplied application scope", compile.AttrStrings("package_scopes"), []string{"//dep:package.json"})
	test := onDiskRule(t, root, "app", "ts_test", "app_test")
	wantStrings(t, "config_srcs", test.AttrStrings("config_srcs"), []string{"//:config/helper.mjs", "//:package.json"})
	wantLabels(t, "partial update retains only its executable test source", test.AttrStrings("srcs"), []string{"index.test.ts"})
	wantLabels(t, "partial update uses the untouched source owner", test.AttrStrings("deps"), []string{":app", "//b"})
	if selected := test.AttrStrings("test_srcs"); len(selected) > 0 {
		wantLabels(t, "partial update selects only the executable test", selected, []string{"index.test.ts"})
	}
	for _, generated := range []*rule.Rule{fullTest, test} {
		if got := generated.AttrString("tsconfig"); got != ":tsconfig" {
			t.Fatalf("test compiler context = %q, want :tsconfig", got)
		}
	}
	if emit, literal := test.Attr("emit").(*bzl.Ident); test.Attr("emit") != nil && (!literal || emit.Name != "False") {
		t.Fatal("partial update acquired an emitted test boundary")
	}
	supplier := onDiskRule(t, root, "b", "ts_compile", "b")
	wantLabels(t, "untouched owner supplies the exact original borrowed sources", supplier.AttrStrings("srcs"), append(slices.Clone(borrowedSources), "index.ts"))
	wantLabels(t, "untouched owner supplies original nearest scopes", supplier.AttrStrings("package_scopes"), borrowedScopes)
	wantLabels(t, "untouched owner has no reverse borrowing dependency", supplier.AttrStrings("deps"), nil)
	if emit, literal := supplier.Attr("emit").(*bzl.Ident); supplier.Attr("emit") != nil && (!literal || emit.Name != "False") {
		t.Fatal("untouched owner cannot replace borrowed implementations with declarations")
	}
	afterPartial := buildFileBytes(t, root)
	untouched := map[string]string{}
	for file, content := range afterPartial {
		if file != "app/BUILD.bazel" {
			untouched[file] = content
		}
	}
	delete(beforePartial, "app/BUILD.bazel")
	if diff := snapshotDiff(beforePartial, untouched); diff != "" {
		t.Fatalf("partial update changed an untouched BUILD file: %s", diff)
	}
	if output, err := protoGazelle(t, root, "app"); err != nil {
		t.Fatalf("repeated partial update: %v\n%s", err, output)
	}
	if diff := snapshotDiff(afterPartial, buildFileBytes(t, root)); diff != "" {
		t.Fatalf("repeated partial update changed the existing graph: %s", diff)
	}
	rootBuild := filepath.Join(root, "BUILD.bazel")
	originalBuild, err := os.ReadFile(rootBuild)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, rootBuild, string(originalBuild)+"# gazelle:exclude app/foreign/package.json\n")
	beforeExcludedManifest := convergeSnapshot(t, root)
	for _, args := range [][]string{nil, {"-r=false", "b"}} {
		output, err := protoGazelle(t, root, args...)
		if err == nil || !strings.Contains(output, "app/foreign/helper.ts imports app/foreign/package.json") || !strings.Contains(output, "excluded or ignored") {
			t.Fatalf("borrowed source lost its excluded package metadata with %v: %v\n%s", args, err, output)
		}
		if diff := snapshotDiff(beforeExcludedManifest, convergeSnapshot(t, root)); diff != "" {
			t.Fatalf("rejected package metadata changed BUILD files: %s", diff)
		}
	}
	writeFile(t, rootBuild, string(originalBuild))
	writeFile(t, filepath.Join(root, "app/index.ts"), "export { helper } from '../lib/nested/excluded';\n")
	output, err = protoGazelle(t, root, "app")
	if err == nil || !strings.Contains(output, "lib/nested/excluded.ts") || !strings.Contains(output, "excluded or ignored") {
		t.Fatalf("partial update admitted an excluded indexed input: %v\n%s", err, output)
	}
	writeFile(t, filepath.Join(root, "app/index.ts"), "export { helper } from '../lib/nested/helper';\n")
	contents, err := os.ReadFile(rootBuild)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, rootBuild, "# gazelle:exclude config/helper.mjs\n"+string(contents))
	before = convergeSnapshot(t, root)
	for _, dirs := range [][]string{nil, {"app"}} {
		output, err = protoGazelle(t, root, dirs...)
		if err == nil || !strings.Contains(output, "config/helper.mjs") || !strings.Contains(output, "excluded or ignored") {
			t.Fatalf("update %v admitted an excluded config helper: %v\n%s", dirs, err, output)
		}
		if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
			t.Fatalf("excluded config import changed BUILD files: %s", diff)
		}
	}
}

func TestUnimportedScalarOutputStaysOutOfEmittedProgram(t *testing.T) {
	for _, test := range []struct{ name, output, authored, kind, discovery string }{
		{"independent stem", "locales.ts", "", "ts_codegen", "include"},
		{"TypeScript shadows JavaScript", "value.ts", "value.js", "genrule", "include"},
		{"TSX shadows JavaScript", "value.tsx", "value.js", "ts_codegen", "inherited"},
		{"declaration shadows JavaScript", "value.d.ts", "value.js", "genrule", "default excludes"},
		{"ESM TypeScript shadows JavaScript", "value.mts", "value.mjs", "genrule", "include"},
		{"ESM declaration and literal JavaScript", "value.d.mts", "value.mjs", "ts_codegen", "files"},
		{"CommonJS TypeScript shadows JavaScript", "value.cts", "value.cjs", "genrule", "include"},
		{"CommonJS declaration shadows JavaScript", "value.d.cts", "value.cjs", "ts_codegen", "inherited"},
		{"lower priority JavaScript", "value.js", "value.ts", "genrule", "include"},
		{"kept JavaScript root", "value.ts", "value.js", "ts_codegen", "kept"},
		{"generated directory", "generated/value.ts", "value.js", "ts_codegen", "tree"},
		{"scalar output beside authored directory", "value.ts", "value.js", "genrule", "scalar directory"},
		{"wildcard output without projection", "value.js", "value.ts", "genrule", "unused wildcard"},
		{"projected wildcard output rejects before writing", "value.ts", "value.js", "genrule", "projected wildcard"},
		{"literal output cannot become a pattern", "value?.ts", "valueX.ts", "genrule", "literal pattern"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree := map[string]string{
				"MODULE.bazel": "module(name = \"unimported_output\")\n",
				"BUILD.bazel": loadDefs + `"ts_binary")
ts_binary(name = "run", entry_point = "//app")
`,
				"app/index.ts": "export const value = 1;\n",
				"fixtures/BUILD.bazel": loadDefs + `"ts_compile")
ts_compile(name = "fixtures", srcs = ["helper.js"])
`,
				"fixtures/tsconfig.json": `{"compilerOptions":{"allowJs":true},"files":["helper.js"]}`,
				"fixtures/helper.js":     "export const helper = 1;\n",
			}
			build := fmt.Sprintf("genrule(name = \"generated\", outs = [%q], cmd = \"unused\")\n", test.output)
			if test.kind == "ts_codegen" {
				build = loadDefs + `"ts_codegen", "ts_compile")` + "\n" + fmt.Sprintf("ts_codegen(name = \"generated\", outs = [%q], generator = \":generator\")\n", test.output)
			}
			options := map[string]any{"allowJs": true, "module": "preserve", "moduleResolution": "bundler", "jsx": "preserve"}
			config := map[string]any{"compilerOptions": options, "include": []string{"**/*"}, "exclude": []string{"ignored.js"}}
			tree["app/ignored.js"] = "export const ignored = true;\n"
			switch test.discovery {
			case "inherited", "default excludes":
				config["include"] = []string{"../app/**/*"}
				config["exclude"] = []string{"../app/ignored.js"}
				if test.discovery == "default excludes" {
					delete(config, "exclude")
					delete(tree, "app/ignored.js")
					options["outDir"], options["declarationDir"] = "../app/dist", "../app/types"
					tree["app/dist/ignored.js"] = "export const ignored = true;\n"
					tree["app/types/ignored.d.ts"] = "export declare const ignored: true;\n"
				}
				data, err := json.Marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				tree["settings/base.json"] = string(data)
				config = map[string]any{"extends": "../settings/base.json"}
			case "files":
				config["files"] = []string{"index.ts", test.authored}
			case "kept":
				config["include"] = []string{"index.ts"}
				build += "ts_compile(\n    name = \"app\",\n    srcs = [\"index.ts\", \"value.js\"],  # keep\n)\n"
			case "tree":
				build = loadDefs + `"ts_codegen")
ts_codegen(name = "generated", out_dir = "generated", generator = ":generator")
`
			case "scalar directory":
				build = "genrule(name = \"generated\", outs = [\"value.ts\", \"assets.ts\"], cmd = \"unused\")\n"
				tree["app/assets.ts/helper.ts"] = "export { helper } from '../../fixtures/helper.js';\n"
			case "unused wildcard", "projected wildcard":
				build = fmt.Sprintf("genrule(name = \"generated\", outs = [%q, \"ignored?.ts\"], cmd = \"unused\")\n", test.output)
				config["exclude"] = []string{"ignored*"}
				tree["app/ignored?.ts"] = "export const ignored = true;\n"
			}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			tree["app/tsconfig.json"] = string(data)
			tree["app/BUILD.bazel"] = build
			wantSrcs, wantDeps := []string{"index.ts"}, []string{}
			if test.authored != "" {
				tree["app/"+test.authored] = "export { helper } from '../fixtures/helper.js';\n"
				wantSrcs = append(wantSrcs, test.authored)
				wantDeps = append(wantDeps, "//fixtures")
			}
			if test.kind == "ts_codegen" {
				wantDeps = append(wantDeps, ":generated")
			}
			if test.discovery == "scalar directory" {
				wantSrcs = append(wantSrcs, "assets.ts/helper.ts")
				slices.Sort(wantSrcs)
			}
			root := writeTree(t, tree)
			var first map[string]string
			for _, state := range []string{"cold", "materialized", "removed"} {
				outputFile := filepath.Join(root, "app", test.output)
				if state == "materialized" {
					content := "export const generated = true;\n"
					if isDeclarationFile(test.output) {
						content = "export declare const generated: boolean;\n"
					}
					writeFile(t, outputFile, content)
				} else if state == "removed" {
					if err := os.Remove(outputFile); err != nil {
						t.Fatal(err)
					}
				}
				output, err := protoGazelle(t, root)
				unsupported := ""
				if test.discovery == "literal pattern" {
					unsupported = "value?.ts"
				} else if test.discovery == "projected wildcard" {
					unsupported = "ignored?.ts"
				}
				if unsupported != "" && state == "materialized" {
					if err == nil || !strings.Contains(output, unsupported+" cannot be excluded literally") {
						t.Fatalf("literal output became an exclusion pattern: %v\n%s", err, output)
					}
				} else {
					if err != nil {
						t.Fatalf("%s output: %v\n%s", state, err, output)
					}
					compile := onDiskRule(t, root, "app", "ts_compile", "app")
					wantStrings(t, state+" authored roots", compile.AttrStrings("srcs"), wantSrcs)
					wantLabels(t, state+" dependency closure", compile.AttrStrings("deps"), wantDeps)
					if body := buildFileText(t, root, "app"); !strings.Contains(body, "emit = True") {
						t.Fatalf("binary consumer did not require emission:\n%s", body)
					}
				}
				if first == nil {
					first = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("%s changed authored discovery: %s", state, diff)
				}
				for _, file := range []string{"app/tsconfig.json", "settings/base.json", "app/index.ts", "app/" + test.authored, "app/assets.ts/helper.ts", "fixtures/helper.js"} {
					if before, exists := tree[file]; exists {
						after, err := os.ReadFile(filepath.Join(root, file))
						if err != nil || string(after) != before {
							t.Fatalf("discovery changed authored file %s: %v", file, err)
						}
					}
				}
				projections, err := filepath.Glob(filepath.Join(root, "app/.gazelle-tsconfig-*.json"))
				if err != nil || len(projections) != 0 {
					t.Fatalf("discovery left temporary configurations: %v, %v", projections, err)
				}
			}
		})
	}
	for _, discovery := range []string{"literal config root", "manifest root"} {
		for _, update := range []struct {
			name string
			args []string
		}{{"recursive", nil}, {"parent only", []string{"-r=false", "."}}} {
			t.Run(discovery+"/"+update.name+" cannot create a package from a generated copy", func(t *testing.T) {
				files := map[string]string{
					"MODULE.bazel":     "module(name = \"generated_root_membership\")\n",
					"BUILD.bazel":      `genrule(name = "generated", outs = ["nested/gen.ts"], cmd = "unused")` + "\n",
					"tsconfig.json":    `{"compilerOptions":{"module":"preserve"},"files":["nested/helper.ts"]}`,
					"nested/helper.ts": "export const helper = 1;\n",
				}
				if discovery == "literal config root" {
					files["nested/tsconfig.json"] = `{"compilerOptions":{"module":"preserve"},"files":["gen.ts"]}`
				} else {
					files["nested/package.json"] = `{"name":"generated-root","exports":"./gen.ts"}`
				}
				root := writeTree(t, files)
				var first map[string]string
				for _, state := range []string{"cold", "materialized", "removed"} {
					generated := filepath.Join(root, "nested/gen.ts")
					if state == "materialized" {
						writeFile(t, generated, "export const generated = 1;\n")
					} else if state == "removed" {
						if err := os.Remove(generated); err != nil {
							t.Fatal(err)
						}
					}
					if output, err := protoGazelle(t, root, update.args...); err != nil {
						t.Fatalf("%s generated root: %v\n%s", state, err, output)
					}
					compile := onDiskRule(t, root, "", "ts_compile", "root")
					if !slices.Contains(compile.AttrStrings("srcs"), "nested/helper.ts") || slices.Contains(compile.AttrStrings("srcs"), "nested/gen.ts") {
						t.Fatalf("%s changed the parent's authored membership: %v", state, compile.AttrStrings("srcs"))
					}
					if _, err := os.Stat(filepath.Join(root, "nested/BUILD.bazel")); !os.IsNotExist(err) {
						t.Fatalf("%s generated root created a package boundary: %v", state, err)
					}
					wantStrings(t, "parent keeps the declared generated output", onDiskRule(t, root, "", "genrule", "generated").AttrStrings("outs"), []string{"nested/gen.ts"})
					if first == nil {
						first = buildFileBytes(t, root)
					} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("%s changed package boundaries or ownership: %s", state, diff)
					}
				}
			})
		}
	}
}

func TestScalarFilesRejectUnsupportedCompilerSources(t *testing.T) {
	cases := []struct{ name, file, producer, source, stale string }{
		{"json", "value.json", "genrule", "import data from './value.json'; export const answer: number = data.answer;\n", `{"answer":"stale"}`},
		{"typescript", "value.ts", "genrule", "export { value } from './value.js';\n", "export { secret as value } from './stale-secret';\n"},
		{"javascript", "value.js", "genrule", "export { value } from './value.js';\n", "export { secret as value } from './stale-secret';\n"},
		{"codegen typescript", "value.ts", "ts_codegen", "export { value } from './value.js';\n", "export { secret as value } from './stale-secret';\n"},
		{"codegen JavaScript", "value.js", "ts_codegen", "export { value } from './value.js';\n", "export { secret as value } from './stale-secret';\n"},
		{"codegen ESM JavaScript", "value.mjs", "ts_codegen", "export { value } from './value.mjs';\n", "export { secret as value } from './stale-secret';\n"},
		{"codegen CommonJS JavaScript", "value.cjs", "ts_codegen", "export { value } from './value.cjs';\n", "exports.value = require('./stale-secret').secret;\n"},
		{"authored rejects JSX", "value.jsx", "", "export { value } from './value.jsx';\n", "export const value = 1;\n"},
		{"authored rejects ESM TypeScript", "value.mts", "", "export { value } from './value.mjs';\n", "export const value = 1;\n"},
		{"authored rejects CommonJS TypeScript", "value.cts", "", "export { value } from './value.cjs';\n", "export const value = 1;\n"},
		{"genrule rejects JSX", "value.jsx", "genrule", "export { value } from './value.jsx';\n", "export { secret as value } from './stale-secret';\n"},
		{"genrule rejects ESM TypeScript", "value.mts", "genrule", "export { value } from './value.mjs';\n", "export { secret as value } from './stale-secret';\n"},
		{"genrule rejects CommonJS TypeScript", "value.cts", "genrule", "export { value } from './value.cjs';\n", "export { secret as value } from './stale-secret';\n"},
		{"codegen rejects ESM TypeScript", "value.mts", "ts_codegen", "export { value } from './value.mjs';\n", "export { secret as value } from './stale-secret';\n"},
		{"codegen rejects CommonJS TypeScript", "value.cts", "ts_codegen", "export { value } from './value.cjs';\n", "export { secret as value } from './stale-secret';\n"},
		{"declaration", "value.d.ts", "genrule", "export type { Value } from './value.js';\n", "export type { Secret as Value } from './stale-secret';\n"},
		{"ESM declaration", "value.d.mts", "genrule", "export type { Value } from './value.mjs';\n", "export type { Secret as Value } from './stale-secret';\n"},
		{"CommonJS declaration", "value.d.cts", "genrule", "export type { Value } from './value.cjs';\n", "export type { Secret as Value } from './stale-secret';\n"},
		{"configured declaration", "value.d.ts", "genrule", "export const value: Value = 1;\n", "import './stale-secret'; declare global { type Value = string; }\n"},
	}
	t.Run("accepted outputs ignore stale contents", func(t *testing.T) {
		tree := map[string]string{
			"MODULE.bazel": "module(name = \"generated_scalar\")\n",
		}
		build := loadDefs + `"ts_codegen", "ts_compile")` + "\n# gazelle:exclude stale-secret.ts\n"
		var roots, sources, deps, declarations []string
		stale := map[string]string{"stale-secret.ts": "export const secret = 1; export interface Secret {}\n"}
		for _, test := range cases {
			if test.file == "value.mts" || test.file == "value.cts" || test.file == "value.jsx" {
				continue
			}
			name := strings.ToLower(strings.ReplaceAll(test.name, " ", "_"))
			file := strings.Replace(test.file, "value", name, 1)
			root := "input_" + name + ".ts"
			roots = append(roots, root)
			sources = append(sources, root, file)
			tree["app/"+root] = strings.ReplaceAll(test.source, "./value", "./"+name)
			stale[file] = test.stale
			build += "# gazelle:exclude " + file + "\n"
			if test.producer == "ts_codegen" {
				build += fmt.Sprintf("ts_codegen(name = %q, outs = [%q, %q], generator = \":generator\")\n", name, file, name+"_helper.mjs")
				deps = append(deps, ":"+name)
			} else {
				build += fmt.Sprintf("genrule(name = %q, outs = [%q], cmd = \"echo generated > $@\")\n", name, file)
			}
			if isDeclarationFile(file) {
				declarations = append(declarations, file)
			}
		}
		tree["app/BUILD.bazel"] = build
		files, err := json.Marshal(roots)
		if err != nil {
			t.Fatal(err)
		}
		tree["app/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"resolveJsonModule":true,"types":["./configured_declaration.d.ts"]},"files":` + string(files) + `}`
		root := writeTree(t, tree)
		var cold map[string]string
		for _, state := range []string{"absent", "stale", "removed"} {
			for name, contents := range stale {
				file := filepath.Join(root, "app", name)
				if state == "stale" {
					writeFile(t, file, contents)
				} else if state == "removed" {
					if err := os.Remove(file); err != nil {
						t.Fatal(err)
					}
				}
			}
			var args []string
			if state == "stale" {
				args = []string{"-r=false", "app"}
			}
			output, err := protoGazelle(t, root, args...)
			if err != nil {
				t.Fatalf("%s update rejected generated Files: %v\n%s", state, err, output)
			}
			consumer := onDiskRule(t, root, "app", "ts_compile", "app")
			wantLabels(t, "consumer srcs", consumer.AttrStrings("srcs"), sources)
			wantLabels(t, "consumer deps", consumer.AttrStrings("deps"), deps)
			for _, test := range cases {
				if test.file == "value.mts" || test.file == "value.cts" || test.file == "value.jsx" {
					continue
				}
				name := strings.ToLower(strings.ReplaceAll(test.name, " ", "_"))
				producer := onDiskRule(t, root, "app", test.producer, name)
				outputs := []string{strings.Replace(test.file, "value", name, 1)}
				if test.producer == "ts_codegen" {
					outputs = append(outputs, name+"_helper.mjs")
				}
				wantLabels(t, "declared outputs", producer.AttrStrings("outs"), outputs)
			}

			for _, r := range loadRules(t, root, "app") {
				if r.Kind() == "ts_compile" && r.Name() != "app" {
					t.Fatalf("generated File acquired an unnecessary compiler: %s", r.Name())
				}
			}
			snapshot := convergeSnapshot(t, root)
			if cold == nil {
				cold = snapshot
			} else if diff := snapshotDiff(cold, snapshot); diff != "" {
				t.Fatalf("%s update changed the generated graph: %s", state, diff)
			}
		}
		for _, file := range declarations {
			name := strings.TrimSuffix(file, path.Ext(file)) + "_owner"
			build += fmt.Sprintf("# keep\nts_compile(name = %q, srcs = [%q])\n", name, file)
			deps = append(deps, ":"+name)
			sources = slices.DeleteFunc(sources, func(src string) bool { return src == file })
		}
		writeFile(t, filepath.Join(root, "app/BUILD.bazel"), build)
		for _, present := range []bool{false, true} {
			var args []string
			if present {
				for _, file := range declarations {
					writeFile(t, filepath.Join(root, "app", file), stale[file])
				}
				writeFile(t, filepath.Join(root, "app/stale-secret.ts"), stale["stale-secret.ts"])
				args = []string{"-r=false", "app"}
			}
			output, err := protoGazelle(t, root, args...)
			if err != nil {
				t.Fatalf("explicit declaration owners present=%t: %v\n%s", present, err, output)
			}
			consumer := onDiskRule(t, root, "app", "ts_compile", "app")
			wantLabels(t, "explicit declaration owners remain dependencies", consumer.AttrStrings("deps"), deps)
			wantLabels(t, "owned declarations are not borrowed", consumer.AttrStrings("srcs"), sources)
			for _, file := range declarations {
				name := strings.TrimSuffix(file, path.Ext(file)) + "_owner"
				wantStrings(t, "explicit owner retains its File", onDiskRule(t, root, "app", "ts_compile", name).AttrStrings("srcs"), []string{file})
			}
		}
	})
	for _, test := range cases {
		if test.file != "value.mts" && test.file != "value.cts" && test.file != "value.jsx" {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			producer := fmt.Sprintf("genrule(name = \"values\", outs = [%q], cmd = \"echo generated > $@\")\n", test.file)
			if test.producer == "ts_codegen" {
				producer = loadDefs + `"ts_codegen")` + "\n" + fmt.Sprintf("ts_codegen(name = \"values\", outs = [%q], generator = \":generator\")\n", test.file)
			}
			tree := map[string]string{
				"MODULE.bazel":      "module(name = \"generated_scalar\")\n",
				"app/BUILD.bazel":   "# gazelle:exclude " + test.file + "\n# gazelle:exclude stale-secret.ts\n" + producer,
				"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"jsx":"preserve"},"files":["index.ts"]}`,
				"app/index.ts":      test.source,
			}
			input := "generated app/" + test.file + " from @generated_scalar//app:values"
			if test.producer == "" {
				tree["app/BUILD.bazel"] = ""
				tree["app/index.ts"] = strings.ReplaceAll(test.source, "./value", "../support/value")
				tree["support/BUILD.bazel"] = fmt.Sprintf("exports_files([%q], visibility = [\"//app:__pkg__\"])\n", test.file)
				tree["support/"+test.file] = test.stale
				input = "support/" + test.file
			}
			root := writeTree(t, tree)
			before := convergeSnapshot(t, root)
			updates := [][]string{nil}
			if test.producer == "" || test.file == "value.jsx" {
				updates = append(updates, []string{"-r=false", "app"})
			}
			for _, args := range updates {
				if len(args) != 0 && test.producer != "" {
					writeFile(t, filepath.Join(root, "app", test.file), test.stale)
					writeFile(t, filepath.Join(root, "app/stale-secret.ts"), "export const secret = 1;\n")
				}
				output, err := protoGazelle(t, root, args...)
				if err == nil || !strings.Contains(output, "imports "+input+", which ts_compile cannot accept as a source") {
					t.Fatalf("update %v accepted an unsupported compiler source: %v\n%s", args, err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("rejected compiler source changed BUILD files: %s", diff)
				}
			}
		})
	}
}

func TestSiblingAliasOverrideCannotChangeInheritedProducerOrParentIndex(t *testing.T) {
	for _, override := range []string{"a_override", "z_override"} {
		for _, parentIndex := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/parent_index=%t", override, parentIndex), func(t *testing.T) {
				consumerSource := "export { value } from '../m_generated/entry.mjs';\n"
				deps := []string{"//m_generated:runtime"}
				updates := [][]string{nil, {"-r=false", "consumer"}, {"-index=false", "-r=false", "consumer"}}
				states := []string{"cold", "warm"}
				if parentIndex {
					consumerSource += "export { parent } from '../parent_tree/runtime';\n"
					deps = append(deps, "//:parent_runtime")
					updates = append(updates, []string{"-index=false"})
					states = append(states, "stale")
				}
				updates = append(updates, nil)
				root := writeTree(t, map[string]string{
					"MODULE.bazel": "module(name = \"scoped_producer_identity\")\n",
					"BUILD.bazel": `# gazelle:alias_kind wrapped_codegen ts_codegen
load("//:defs.bzl", "wrapped_codegen")
wrapped_codegen(name = "parent_runtime", out_dir = "parent_tree", generator = ":generator", visibility = ["//visibility:public"])
`,
					"tsconfig.json":                      `{"files":["root.ts"]}`,
					"root.ts":                            "export const root = 1;\n",
					"parent_tree/.keep":                  "",
					override + "/BUILD.bazel":            "# gazelle:alias_kind wrapped_codegen filegroup\nload(\"//:defs.bzl\", \"wrapped_codegen\")\nwrapped_codegen(name = \"local\", out_dir = \"authored\")\n",
					override + "/authored/tsconfig.json": `{"files":["index.ts"]}`,
					override + "/authored/index.ts":      "export const local = 1;\n",
					"m_generated/BUILD.bazel":            "load(\"//:defs.bzl\", \"wrapped_codegen\")\nwrapped_codegen(name = \"runtime\", outs = [\"entry.mjs\", \"helper.mjs\"], generator = \":generator\", visibility = [\"//visibility:public\"])\n",
					"m_generated/tsconfig.json":          `{"files":["stub.ts"]}`,
					"m_generated/stub.ts":                "export const unrelated = 1;\n",
					"consumer/tsconfig.json":             `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"types":[]},"files":["index.ts"]}`,
					"consumer/index.ts":                  consumerSource,
				})
				if parentIndex {
					writeFile(t, filepath.Join(root, "consumer/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"types":[]},"files":["index.ts","example.test.ts"]}`)
					writeFile(t, filepath.Join(root, "consumer/example.test.ts"), "export {};\n")
					writeFile(t, filepath.Join(root, "consumer/vitest.config.mts"), "import { parent } from '../parent_tree/runtime'; export default { parent };\n")
				}
				var first map[string]string
				for _, state := range states {
					if state != "cold" {
						writeFile(t, filepath.Join(root, "m_generated/entry.mjs"), "export const value = 1;\n")
						writeFile(t, filepath.Join(root, "m_generated/helper.mjs"), "export const helper = 2;\n")
						writeFile(t, filepath.Join(root, "parent_tree/runtime.d.ts"), "export declare const parent: number;\n")
					}
					if state == "stale" {
						writeFile(t, filepath.Join(root, "parent_tree/runtime.d.ts"), "export { parent } from '../hidden/secret';\n")
						writeFile(t, filepath.Join(root, "hidden/secret.d.ts"), "export declare const parent: string;\n")
					}
					for _, args := range updates {
						output, err := protoGazelle(t, root, args...)
						if err != nil {
							t.Fatalf("state=%s update %v: %v\n%s", state, args, err, output)
						}
						consumer := onDiskRule(t, root, "consumer", "ts_compile", "consumer")
						wantLabels(t, "inherited scalar input", consumer.AttrStrings("srcs"), []string{"index.ts", "//m_generated:entry.mjs"})
						wantLabels(t, "sibling and parent retain their alias meaning", consumer.AttrStrings("deps"), deps)
						if parentIndex {
							test := onDiskRule(t, root, "consumer", "ts_test", "consumer_test")
							if !slices.Contains(test.AttrStrings("deps"), "//:parent_runtime") {
								t.Fatalf("state=%s update %v lost the config's declared tree dependency: %v", state, args, test.AttrStrings("deps"))
							}
							for _, attr := range []string{"srcs", "config_srcs"} {
								for _, input := range test.AttrStrings(attr) {
									if strings.Contains(input, "parent_tree/") || strings.Contains(input, "hidden/") {
										t.Fatalf("state=%s update %v borrowed generated tree or stale closure into %s: %s", state, args, attr, input)
									}
								}
							}
						}
						onDiskRule(t, root, override+"/authored", "ts_compile", "authored")
						onDiskRule(t, root, "m_generated", "ts_config", "tsconfig")
						producer := onDiskRule(t, root, "m_generated", "wrapped_codegen", "runtime")
						wantStrings(t, "inherited producer retains runtime siblings", producer.AttrStrings("outs"), []string{"entry.mjs", "helper.mjs"})
						if first == nil {
							first = convergeSnapshot(t, root)
						} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("state=%s update %v changed scoped producer identity: %s", state, args, diff)
						}
					}
				}
			})
		}
	}
}

func TestMappedGeneratorCannotLoseRuntimeSiblingsAfterLiveRuleRefresh(t *testing.T) {
	for _, spelling := range []struct{ kind, directives string }{
		{"ts_codegen", ""},
		{"wrapped_codegen", "# gazelle:alias_kind wrapped_codegen ts_codegen\n"},
		{"mapped_codegen", "# gazelle:map_kind ts_codegen mapped_codegen //:defs.bzl\n"},
		{"final_codegen", "# gazelle:map_kind ts_codegen mapped_codegen //:defs.bzl\n# gazelle:map_kind mapped_codegen final_codegen //:defs.bzl\n"},
	} {
		t.Run(spelling.kind, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel": "module(name = \"live_producer_identity\")\n",
				"BUILD.bazel":  "",
				"generated/BUILD.bazel": spelling.directives + fmt.Sprintf(`load("//:defs.bzl", %q)
%s(name = "runtime", outs = ["entry.mjs", "helper.mjs"], generator = ":generator", visibility = ["//visibility:public"])
`, spelling.kind, spelling.kind),
				"generated/tsconfig.json": `{"files":["stub.ts"]}`,
				"generated/stub.ts":       "export const unrelated = 1;\n",
				"app/BUILD.bazel":         "# gazelle:alias_kind wrapped_codegen filegroup\n# gazelle:map_kind ts_codegen other_codegen //:defs.bzl\n",
				"app/tsconfig.json":       `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"types":[]},"files":["index.ts"]}`,
				"app/index.ts":            "export { value } from '../generated/entry.mjs';\n",
			})
			var first map[string]string
			for _, materialized := range []bool{false, true} {
				if materialized {
					writeFile(t, filepath.Join(root, "generated/entry.mjs"), "export { value } from './helper.mjs';\n")
					writeFile(t, filepath.Join(root, "generated/helper.mjs"), "export const value = 42;\n")
				}
				for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "-r=false", "app"}, nil} {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("materialized=%t update %v: %v\n%s", materialized, args, err, output)
					}
					consumer := onDiskRule(t, root, "app", "ts_compile", "app")
					wantLabels(t, "only the selected scalar is compiled", consumer.AttrStrings("srcs"), []string{"index.ts", "//generated:entry.mjs"})
					wantLabels(t, "producer supplies the runtime sibling", consumer.AttrStrings("deps"), []string{"//generated:runtime"})
					producer := onDiskRule(t, root, "generated", spelling.kind, "runtime")
					wantStrings(t, "producer retains both runtime outputs", producer.AttrStrings("outs"), []string{"entry.mjs", "helper.mjs"})
					onDiskRule(t, root, "generated", "ts_config", "tsconfig")
					own := onDiskRule(t, root, "generated", "ts_compile", "generated")
					wantLabels(t, "same-package codegen dependency uses semantic kind", own.AttrStrings("deps"), []string{":runtime"})
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("materialized=%t update %v changed producer closure: %s", materialized, args, diff)
					}
				}
			}
		})
	}
}

func TestScalarGeneratedTypeScriptRetainsDeclaredOwnerBeforeOutputExists(t *testing.T) {
	for _, identity := range []struct{ name, kind, directives string }{
		{"canonical", "ts_codegen", ""},
		{"aliased", "wrapped_codegen", "load(\"//:codegen.bzl\", \"wrapped_codegen\")\n# gazelle:alias_kind wrapped_codegen ts_codegen\n\n"},
		{"mapped", "wrapped_codegen", "# gazelle:map_kind ts_codegen wrapped_codegen //:codegen.bzl\n\n"},
		{"mapped with alias", "ts_codegen", "# gazelle:map_kind ts_codegen wrapped_codegen //:codegen.bzl\n# gazelle:alias_kind wrapped_codegen ts_codegen\n\n"},
	} {
		consumers := []string{"source", "config with excluded descendant"}
		states := []string{"cold", "stale"}
		if identity.name == "canonical" {
			consumers = append(consumers, "config with eligible descendant", "config with .ts", "config with .mts", "config with .cts")
			states = append(states, "removed")
		}
		for _, consumerKind := range consumers {
			t.Run(identity.name+"/"+consumerKind, func(t *testing.T) {
				configFile := "value.mjs"
				if strings.HasPrefix(consumerKind, "config with .") {
					configFile = "value" + strings.TrimPrefix(consumerKind, "config with ")
				}
				producerBuild := identity.directives + loadDefs + `"ts_codegen", "ts_compile")
` + fmt.Sprintf(`%s(name = "source", outs = ["value.ts"], generator = ":generator")
# keep
ts_compile(name = "compiled", srcs = ["value.ts"])
`, identity.kind)
				if consumerKind == "source" {
					producerBuild += fmt.Sprintf(`%[1]s(name = "types", outs = ["ambient.d.ts"], generator = ":generator")
%[1]s(name = "runtime", outs = ["runtime.mjs"], generator = ":generator")
%[1]s(name = "json", outs = ["value.json"], generator = ":generator")
`, identity.kind)
				}
				root := writeTree(t, map[string]string{
					"MODULE.bazel":          "module(name = \"scalar_inputs\")\n",
					"BUILD.bazel":           "# gazelle:exclude generated/value.ts\n# gazelle:exclude generated/stale-secret.ts\n",
					"generated/BUILD.bazel": producerBuild,
					"app/tsconfig.json":     `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"resolveJsonModule":true,"paths":{"#runtime":["../generated/runtime.mjs","./fallback.mjs"]}},"include":["*.ts"]}`,
					"app/index.ts":          "export { value } from '../generated/value';\nexport type { Ambient } from '../generated/ambient';\nexport { runtime } from '#runtime';\nimport data from '../generated/value.json'; export { data };\n",
					"app/fallback.mjs":      "export const runtime = -1;\n",
				})
				if identity.name == "aliased" {
					writeFile(t, filepath.Join(root, "app/BUILD.bazel"), "# gazelle:alias_kind wrapped_codegen filegroup\n")
				} else if strings.HasPrefix(identity.name, "mapped") {
					writeFile(t, filepath.Join(root, "app/BUILD.bazel"), "# gazelle:map_kind ts_codegen unrelated_codegen //:codegen.bzl\n")
				}
				staleFiles := map[string]string{
					"generated/value.ts":        "import { secret } from './stale-secret.ts'; export const value = secret;\n",
					"generated/ambient.d.ts":    "import type { Secret } from './stale-secret.ts'; export type Ambient = Secret;\n",
					"generated/runtime.mjs":     "export { secret as runtime } from './stale-secret.ts';\n",
					"generated/value.json":      `{"value":"stale"}`,
					"generated/stale-secret.ts": "export const secret = 1; export type Secret = number;\n",
				}
				if consumerKind != "source" {
					writeFile(t, filepath.Join(root, "app/index.ts"), "export const value = 1;\n")
					writeFile(t, filepath.Join(root, "app/example.test.ts"), "export {};\n")
					writeFile(t, filepath.Join(root, "app/vitest.config.mts"), "import { value } from './generated/"+configFile+"'; export default { value };\n")
					build := identity.directives + loadDefs + `"ts_codegen")
` + fmt.Sprintf(`%s(name = "config_source", outs = [%q, "helper.mjs"], generator = ":generator")
`, identity.kind, configFile)
					if consumerKind == "config with excluded descendant" {
						build += "# gazelle:exclude stale-secret.ts\n"
					}
					writeFile(t, filepath.Join(root, "app/generated/BUILD.bazel"), build)
					staleFiles = map[string]string{
						"app/generated/" + configFile:   "import { secret } from './stale-secret.ts'; export const value = secret;\n",
						"app/generated/helper.mjs":      "export const helper = 'stale';\n",
						"app/generated/stale-secret.ts": "export const secret = 1;\n",
					}
				}
				var cold map[string]string
				for _, state := range states {
					for file, contents := range staleFiles {
						if state == "stale" {
							writeFile(t, filepath.Join(root, file), contents)
						} else if state == "removed" {
							if err := os.Remove(filepath.Join(root, file)); err != nil {
								t.Fatal(err)
							}
						}
					}
					updates := [][]string{nil}
					partial := identity.name == "aliased" && consumerKind == "source" ||
						strings.HasPrefix(identity.name, "mapped") && consumerKind == "config with excluded descendant"
					if state == "cold" && partial {
						updates = append(updates, []string{"-index=false", "-r=false", "app"}, nil)
					}
					for _, args := range updates {
						output, err := protoGazelle(t, root, args...)
						if err != nil {
							t.Fatalf("%s generation with %v: %v\n%s", state, args, err, output)
						}
						compile := onDiskRule(t, root, "generated", "ts_compile", "compiled")
						wantStrings(t, state+" generated srcs", compile.AttrStrings("srcs"), []string{"value.ts"})
						consumer := onDiskRule(t, root, "app", "ts_compile", "app")
						if consumerKind == "source" {
							wantLabels(t, state+" consumer stages selected JavaScript instead of the fallback", consumer.AttrStrings("srcs"), []string{"index.ts", "//generated:runtime.mjs"})
							wantStrings(t, state+" consumer deps", consumer.AttrStrings("deps"), []string{"//generated:compiled", "//generated:json", "//generated:runtime", "//generated:types"})
						} else {
							wantStrings(t, state+" consumer srcs", consumer.AttrStrings("srcs"), []string{"index.ts"})
							test := onDiskRule(t, root, "app", "ts_test", "app_test")
							wantStrings(t, state+" test srcs", test.AttrStrings("srcs"), []string{"example.test.ts"})
							wantStrings(t, state+" config srcs", test.AttrStrings("config_srcs"), []string{"//app/generated:" + configFile})
							wantLabels(t, state+" config deps retain sibling runtime outputs", test.AttrStrings("deps"), []string{":app", "//app/generated:config_source"})
							kind := identity.kind
							if identity.name == "mapped with alias" {
								kind = "wrapped_codegen"
							}
							producer := onDiskRule(t, root, "app/generated", kind, "config_source")
							wantLabels(t, state+" producer runtime files", producer.AttrStrings("outs"), []string{configFile, "helper.mjs"})
						}
						snapshot := convergeSnapshot(t, root)
						if cold == nil {
							cold = snapshot
						} else if diff := snapshotDiff(cold, snapshot); diff != "" {
							t.Fatalf("%s output with %v differs from cold generation: %s", state, args, diff)
						}
					}
				}
				if consumerKind != "source" || identity.name != "canonical" {
					return
				}
				writeFile(t, filepath.Join(root, "generated/BUILD.bazel"), identity.directives+loadDefs+`"ts_codegen")
`+fmt.Sprintf(`%s(name = "source", outs = ["value.ts"], generator = ":generator")
`, identity.kind))
				writeFile(t, filepath.Join(root, "BUILD.bazel"), loadDefs+`"ts_binary")
ts_binary(name = "run", entry_point = "//app")
`)
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("generated File layout belongs to compiler construction: %v\n%s", err, output)
				}
				consumer := onDiskRule(t, root, "app", "ts_compile", "app")
				if !slices.Contains(consumer.AttrStrings("srcs"), "//generated:value.ts") {
					t.Fatal("emission inference discarded the producer's source File")
				}
			})
		}
	}
}

func TestColdScalarImportChoosesOnlyItsCompilerOwner(t *testing.T) {
	for _, test := range []struct {
		name, producer, chosen, other, module      string
		suffixes, compilerPackage, fallback, modes string
	}{
		{"typescript", "ts_codegen", "value.ts", "other.ts", "value.js", "", "", "", ""},
		{"declaration", "ts_codegen", "value.d.ts", "other.d.ts", "value.js", "", "", "", ""},
		{"genrule", "genrule", "value.ts", "other.ts", "value.js", "", "", "", ""},
		{"ts before tsx", "ts_codegen", "value.ts", "value.tsx", "value.js", "", "", "", ""},
		{"tsx before declaration", "ts_codegen", "value.tsx", "value.d.ts", "value.js", "", "", "", ""},
		{"declaration before js", "ts_codegen", "value.d.ts", "value.js", "value", "", "", "", ""},
		{"jsx prefers tsx", "ts_codegen", "value.tsx", "value.ts", "value.jsx", "", "", "", ""},
		{"mts declaration", "ts_codegen", "value.d.mts", "other.d.mts", "value.mjs", "", "", "", ""},
		{"cts declaration", "ts_codegen", "value.d.cts", "other.d.cts", "value.cjs", "", "", "", ""},
		{"native suffix", "ts_codegen", "value.native.ts", "value.ts", "value.js", `,"moduleSuffixes":[".native"]`, "", "", ""},
		{"three packages", "genrule", "value.ts", "other.ts", "value.js", "", "compiled", "", ""},
		{"stale fallback", "ts_codegen", "value.ts", "other.ts", "value.js", "", "", "value.d.ts", ""},
		{"two modes", "ts_codegen", "value.d.mts", "value.d.cts", "value.mjs", "", "", "", "distinct"},
		{"two modes same fallback", "ts_codegen", "value.mts", "other.d.ts", "value.mjs", "", "", "value.d.mts", "shared"},
	} {
		t.Run(test.name, func(t *testing.T) {
			producer := test.producer + "(name = \"source\", outs = [\"missing/" + test.chosen + "\", \"missing/" + test.other + "\"], "
			if test.producer == "genrule" {
				producer += "cmd = \"echo generated > $@\")\n"
			} else {
				producer += "generator = \":generator\")\n"
			}
			owners := "# keep\nts_compile(name = \"chosen\", srcs = [\"missing/" + test.chosen + "\"])\n# keep\nts_compile(name = \"other\", srcs = [\"missing/" + test.other + "\"])\n"
			deps := []string{"//generated:chosen"}
			if isDeclarationFile(test.chosen) && test.modes != "distinct" {
				owners = ""
				deps = []string{"//generated:source"}
			}
			if test.modes == "distinct" {
				deps = append(deps, "//generated:other")
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel": "module(name = \"cold_directory\")\n",
				"BUILD.bazel":  "# gazelle:exclude generated/missing/" + test.chosen + "\n# gazelle:exclude generated/missing/" + test.other + "\n",
				"generated/BUILD.bazel": loadDefs + `"ts_codegen", "ts_compile")
` + producer + owners,
				"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","jsx":"preserve","paths":{"#value":["../generated/missing/` + test.module + `"]}` + test.suffixes + `},"include":["*.ts"]}`,
				"app/index.ts":      "export type { Value } from '#value';\n",
			})
			if test.compilerPackage != "" {
				writeFile(t, filepath.Join(root, "generated/BUILD.bazel"), producer)
				writeFile(t, filepath.Join(root, test.compilerPackage, "BUILD.bazel"), loadDefs+`"ts_compile")
# keep
ts_compile(name = "chosen", srcs = ["//generated:missing/value.ts"])
`)
				deps = []string{"//compiled:chosen"}
			}
			if test.modes != "" {
				writeFile(t, filepath.Join(root, "app/tsconfig.json"), `{"compilerOptions":{"module":"nodenext","moduleResolution":"nodenext"},"files":["index.ts"]}`)
				required := "./generated/missing/value.cjs"
				if test.modes == "shared" {
					required = "./generated/missing/value.d.mts"
					deps = append(deps, "//:fallback")
				}
				writeFile(t, filepath.Join(root, "package.json"), `{"name":"modes","imports":{"#value":{"import":"./generated/missing/value.mjs","require":"`+required+`"}}}`)
				writeFile(t, filepath.Join(root, "app/index.ts"), "import type { Value as ESM } from '#value' with { \"resolution-mode\": \"import\" };\nimport type { Value as CJS } from '#value' with { \"resolution-mode\": \"require\" };\nexport type Both = [ESM, CJS];\n")
				if err := os.MkdirAll(filepath.Join(root, "generated/missing"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if test.fallback != "" {
				writeFile(t, filepath.Join(root, "generated/missing", test.fallback), "import type { Secret } from './stale-secret'; export type Value = Secret;\n")
				writeFile(t, filepath.Join(root, "generated/missing/stale-secret.ts"), "export type Secret = string;\n")
				build := "# gazelle:exclude generated/missing/stale-secret.ts\n"
				if test.modes == "shared" {
					build = loadDefs + `"ts_compile")
# keep
ts_compile(name = "fallback", srcs = ["//generated:missing/value.d.mts"])
`
				}
				writeFile(t, filepath.Join(root, "BUILD.bazel"), build)
			}
			var cold map[string]string
			for _, state := range []string{"empty directory", "outputs present"} {
				switch state {
				case "empty directory":
					if err := os.MkdirAll(filepath.Join(root, "generated/missing"), 0o755); err != nil {
						t.Fatal(err)
					}
				case "outputs present":
					for _, file := range []string{test.chosen, test.other} {
						content := "export type Value = string;\n"
						if path.Ext(file) == ".js" {
							content = "export const value = 1;\n"
						}
						writeFile(t, filepath.Join(root, "generated/missing", file), content)
					}
				}
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("%s generation: %v\n%s", state, err, output)
				}
				consumer := onDiskRule(t, root, "app", "ts_compile", "app")
				wantStrings(t, state+" srcs", consumer.AttrStrings("srcs"), []string{"index.ts"})
				wantLabels(t, state+" deps", consumer.AttrStrings("deps"), deps)
				snapshot := convergeSnapshot(t, root)
				if cold == nil {
					cold = snapshot
				} else if diff := snapshotDiff(cold, snapshot); diff != "" {
					t.Fatalf("%s changed the selected owner: %s", state, diff)
				}
			}
			if test.name != "typescript" {
				return
			}
			for _, file := range []string{test.chosen, test.other} {
				if err := os.Remove(filepath.Join(root, "generated/missing", file)); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, filepath.Join(root, "generated/BUILD.bazel"), loadDefs+`"ts_codegen", "ts_compile")
ts_codegen(name = "source", outs = ["missing/value.ts", "missing/other.ts"], generator = ":generator")
# keep
ts_compile(name = "other", srcs = ["missing/other.ts"])
`)
			writeFile(t, filepath.Join(root, "app/BUILD.bazel"), loadDefs+`"ts_compile")
ts_compile(
    name = "app",
    deps = ["//generated:other"], # keep
)
`)
			output, err := protoGazelle(t, root)
			if err != nil {
				t.Fatalf("consumer-owned generated source: %v\n%s", err, output)
			}
			consumer := onDiskRule(t, root, "app", "ts_compile", "app")
			wantLabels(t, "consumer owns the unwrapped output", consumer.AttrStrings("srcs"), []string{"//generated:missing/value.ts", "index.ts"})
			wantStrings(t, "kept dependency remains declared", consumer.AttrStrings("deps"), []string{"//generated:other"})
		})
	}
}

func TestScalarGeneratedOverrideCanNameCompilerBehindFilegroup(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"generated_override\")\n",
		"BUILD.bazel": "# gazelle:exclude generated/value.ts\n" +
			"# gazelle:exclude generated/stale-secret.ts\n" +
			"# gazelle:resolve typescript generated/value.ts //generated:compiled\n",
		"generated/BUILD.bazel": loadDefs + `"ts_codegen", "ts_compile")
ts_codegen(name = "source", outs = ["value.ts"], generator = ":generator")
filegroup(name = "sources", srcs = ["value.ts"])
# keep
ts_compile(name = "compiled", srcs = [":sources"], visibility = ["//app:__pkg__"])
`,
		"app/tsconfig.json": `{"include":["*.ts"]}`,
		"app/index.ts":      "export { value } from '../generated/value.js';\n",
	})
	var cold map[string]string
	for _, present := range []bool{false, true} {
		if present {
			writeFile(t, filepath.Join(root, "generated/value.ts"), "import { secret } from './stale-secret'; export const value = secret;\n")
			writeFile(t, filepath.Join(root, "generated/stale-secret.ts"), "export const secret = 1;\n")
		}
		output, err := protoGazelle(t, root)
		if err != nil {
			t.Fatalf("override with output present=%t: %v\n%s", present, err, output)
		}
		consumer := onDiskRule(t, root, "app", "ts_compile", "app")
		wantStrings(t, "consumer srcs", consumer.AttrStrings("srcs"), []string{"index.ts"})
		wantStrings(t, "explicit owner", consumer.AttrStrings("deps"), []string{"//generated:compiled"})
		snapshot := convergeSnapshot(t, root)
		if cold == nil {
			cold = snapshot
		} else if diff := snapshotDiff(cold, snapshot); diff != "" {
			t.Fatalf("stale output changed the graph: %s", diff)
		}
	}
}

func TestScalarGeneratedTypeScriptPreservesSeparateCompiler(t *testing.T) {
	for _, owner := range []struct {
		pkg, source, output string
		generatedScope      bool
	}{
		{"app", "value.ts", "value.ts", false},
		{"app", "missing/value.ts", "missing/value.ts", false},
		{"app", ":source", "value.ts", false},
		{"generated", ":source", "value.ts", false},
		{"generated", "value.ts", "value.ts", false},
		{"generated", "value.ts", "value.ts", true},
	} {
		name := owner.pkg + "/" + owner.source
		if owner.generatedScope {
			name += "/generated scope"
		}
		t.Run(name, func(t *testing.T) {
			module := "./" + strings.TrimSuffix(owner.output, ".ts") + ".js"
			deps := []string{":compiled", ":source"}
			if owner.pkg != "app" {
				module = "../" + owner.pkg + "/" + strings.TrimPrefix(module, "./")
				deps = []string{"//" + owner.pkg + ":compiled"}
			}
			files := map[string]string{
				"MODULE.bazel": "module(name = \"separate_codegen_owner\")\n",
				"BUILD.bazel": loadDefs + `"ts_binary")
ts_binary(name = "run", entry_point = "//app")
`,
				"app/tsconfig.json":          `{"include":["**/*.ts"]}`,
				"app/index.ts":               "export { value } from '" + module + "';\n",
				owner.pkg + "/tsconfig.json": `{"include":["**/*.ts"]}`,
				owner.pkg + "/BUILD.bazel": loadDefs + `"ts_codegen", "ts_compile")
ts_codegen(name = "source", outs = ["` + owner.output + `"], generator = ":generator")
# keep
ts_compile(name = "compiled", srcs = ["` + owner.source + `"], tsconfig = "tsconfig.json", emit = True)
`,
			}
			if owner.generatedScope {
				files[owner.pkg+"/BUILD.bazel"] += `ts_codegen(name = "manifest", outs = ["package.json"], generator = ":generator")` + "\n"
			}
			root := writeTree(t, files)
			if parentDir(owner.output) != "" {
				if err := os.MkdirAll(filepath.Join(root, owner.pkg, parentDir(owner.output)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			output, err := protoGazelle(t, root)
			if err != nil {
				t.Fatalf("separate compiler generation: %v\n%s", err, output)
			}
			consumer := onDiskRule(t, root, "app", "ts_compile", "app")
			wantStrings(t, "checked-in consumer srcs", consumer.AttrStrings("srcs"), []string{"index.ts"})
			wantStrings(t, "generated compiler dep", consumer.AttrStrings("deps"), deps)
			compiled := onDiskRule(t, root, owner.pkg, "ts_compile", "compiled")
			wantStrings(t, "authored generated srcs", compiled.AttrStrings("srcs"), []string{owner.source})
			if owner.pkg != "app" && ruleNamed(loadRules(t, root, owner.pkg), "ts_compile", owner.pkg) != nil {
				t.Fatal("generated outputs acquired a second compiler owner")
			}
			if _, err := os.Stat(filepath.Join(root, owner.pkg, owner.output)); !os.IsNotExist(err) {
				t.Fatalf("generation materialized the absent output: %v", err)
			}
			before := convergeSnapshot(t, root)
			output, err = protoGazelle(t, root)
			if err != nil {
				t.Fatalf("separate compiler regeneration: %v\n%s", err, output)
			}
			if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
				t.Fatalf("separate compiler ownership changed on rerun: %s", diff)
			}
		})
	}
}

func TestCrossPackageProducerLabelRetainsSourceCompilerOwner(t *testing.T) {
	for _, packages := range []struct{ compiler, producer, consumer string }{
		{"compiled", "generated", "app"},
		{"a_owner", "b_generator", "c_app"},
	} {
		src := "//" + packages.producer + ":source"
		t.Run(packages.compiler, func(t *testing.T) {
			producer := `genrule(name = "source", outs = ["value.ts"], cmd = "echo generated > $@")`
			root := writeTree(t, map[string]string{
				"MODULE.bazel":                     "module(name = \"producer_owner\")\n",
				"BUILD.bazel":                      "# gazelle:exclude " + packages.producer + "/value.ts\n",
				packages.producer + "/BUILD.bazel": producer + "\n",
				packages.compiler + "/BUILD.bazel": loadDefs + `"ts_compile")
# keep
ts_compile(name = "chosen", srcs = ["` + src + `"], emit = False, visibility = ["//visibility:public"])
`,
				packages.consumer + "/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
				packages.consumer + "/index.ts":      "export { value } from '../" + packages.producer + "/value.js';\n",
			})
			var cold map[string]string
			for _, present := range []bool{false, true} {
				if present {
					writeFile(t, filepath.Join(root, packages.producer, "value.ts"), "export const value = 1;\n")
				}
				updates := [][]string{nil}
				if present {
					updates = [][]string{{"-r=false", packages.consumer}, nil}
				}
				for _, args := range updates {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("update %v present=%t: %v\n%s", args, present, err, output)
					}
					consumer := onDiskRule(t, root, packages.consumer, "ts_compile", packages.consumer)
					wantStrings(t, "consumer inputs", consumer.AttrStrings("srcs"), []string{"index.ts"})
					wantStrings(t, "compiler owner", consumer.AttrStrings("deps"), []string{"//" + packages.compiler + ":chosen"})
					compiler := onDiskRule(t, root, packages.compiler, "ts_compile", "chosen")
					wantStrings(t, "authored compiler inputs", compiler.AttrStrings("srcs"), []string{src})
					if text := buildFileText(t, root, packages.compiler); !strings.Contains(text, "emit = False") {
						t.Fatalf("source compiler changed mode: %s", text)
					}
					if got := ruleNamed(loadRules(t, root, packages.producer), "ts_compile", packages.producer); got != nil {
						t.Fatalf("producer acquired a second compiler: %s", got.Name())
					}
					if !present {
						if _, err := os.Stat(filepath.Join(root, packages.producer, "value.ts")); !os.IsNotExist(err) {
							t.Fatalf("generation materialized the output: %v", err)
						}
					}
					if cold == nil {
						cold = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(cold, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("update %v present=%t changed the graph: %s", args, present, diff)
					}
				}
			}
		})
	}
}

func TestExcludedGenruleOutputRetainsItsCompilerOwner(t *testing.T) {
	for _, out := range []string{"value.ts", ":value.ts", "//generated:value.ts", "@//generated:value.ts", "@@//generated:value.ts"} {
		for _, src := range []string{"value.ts", ":producer"} {
			t.Run(out+"/"+src, func(t *testing.T) {
				root := writeTree(t, map[string]string{
					"MODULE.bazel":       "module(name = \"generated_owner\")\n",
					"BUILD.bazel":        "# gazelle:exclude generated/value.ts\n",
					"app/tsconfig.json":  `{"include":["*.ts"]}`,
					"app/index.ts":       "export { value } from '../generated/value';\n",
					"generated/value.ts": "export const value = 1;\n",
					"generated/BUILD.bazel": "load(\"@rules_typescript//ts:defs.bzl\", \"ts_compile\")\n" +
						"genrule(name = \"producer\", outs = [\"" + out + "\"], cmd = \"echo generated > $@\")\n" +
						"# keep\nts_compile(name = \"compiled\", srcs = [\"" + src + "\"], visibility = [\"//app:__pkg__\"])\n",
				})
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("generation: %v\n%s", err, output)
				}
				compile := onDiskRule(t, root, "app", "ts_compile", "app")
				wantStrings(t, "srcs", compile.AttrStrings("srcs"), []string{"index.ts"})
				wantStrings(t, "deps", compile.AttrStrings("deps"), []string{"//generated:compiled"})
			})
		}
	}
}

func TestGeneratedManifestProbePreservesDirectoryImport(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":      "module(name = \"manifest_probe\")\n",
		"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true},"files":["index.ts"]}`,
		"app/index.ts":      "export { value } from '../lib';\n",
		"lib/index.ts":      "export { value } from './helper';\n",
		"lib/helper.ts":     "export const value = 1;\n",
		"lib/BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(name = "manifest", outs = ["package.json"], generator = ":generator")
`,
	})
	before := convergeSnapshot(t, root)
	output, err := protoGazelle(t, root)
	if err == nil || !strings.Contains(output, "cannot discover program membership from generated metadata lib/package.json") || !strings.Contains(output, "use authored metadata for automatic discovery") {
		t.Fatalf("directory import trusted generated resolver metadata: %v\n%s", err, output)
	}
	if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
		t.Fatalf("generated scope conflict changed BUILD ownership: %s", diff)
	}
}

func TestGeneratedPackageJSONRemainsAnExplicitModule(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":      "module(name = \"json_module\")\n",
		"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true},"files":["index.ts"]}`,
		"app/index.ts":      "import data from '../lib/package.json'; export const value = data.value;\n",
		"lib/BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(name = "manifest", outs = ["package.json"], generator = ":generator")
`,
	})
	output, err := protoGazelle(t, root)
	if err != nil {
		t.Fatalf("generation: %v\n%s", err, output)
	}
	consumer := onDiskRule(t, root, "app", "ts_compile", "app")
	wantStrings(t, "explicit JSON producer", consumer.AttrStrings("deps"), []string{"//lib:manifest"})
}

func TestMissingScalarDirectoryPreservesCompilerFallback(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"scalar_fallback\")\n",
		"BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(name = "unrelated", outs = ["generated/other.ts", "generated/helper.mjs"], generator = ":generator")
`,
		"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#value":["../generated/value.js","../lib/value.js"]}},"files":["index.ts"]}`,
		"app/index.ts":      "export { value } from '#value';\n",
		"lib/value.ts":      "export const value = 1;\n",
		"lib/BUILD.bazel":   "",
	})
	output, err := protoGazelle(t, root)
	if err != nil {
		t.Fatalf("unrelated output blocked fallback: %v\n%s", err, output)
	}
	consumer := onDiskRule(t, root, "app", "ts_compile", "app")
	wantLabels(t, "fallback input", consumer.AttrStrings("srcs"), []string{"//lib:value.ts", "index.ts"})
	wantStrings(t, "unrelated producer", consumer.AttrStrings("deps"), nil)
	writeFile(t, filepath.Join(root, "app/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#value":["../generated/other.js"]}},"files":["index.ts"]}`)
	output, err = protoGazelle(t, root)
	if err != nil || !strings.Contains(output, "compiler skipped file lookups") || !strings.Contains(output, "#value") || !strings.Contains(output, "no scalar output selected") {
		t.Fatalf("unsupported scalar lookup needs an ordinary diagnostic: %v\n%s", err, output)
	}
	writeFile(t, filepath.Join(root, "app/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#value":["../generated/other.ts"]}},"files":["index.ts"]}`)
	output, err = protoGazelle(t, root)
	if err != nil {
		t.Fatalf("explicit scalar probe rejected its declared File: %v\n%s", err, output)
	}
	consumer = onDiskRule(t, root, "app", "ts_compile", "app")
	wantLabels(t, "explicit scalar input", consumer.AttrStrings("srcs"), []string{"//:generated/other.ts", "index.ts"})
	wantStrings(t, "declared producer remains available", consumer.AttrStrings("deps"), []string{"//:unrelated"})
	cold := convergeSnapshot(t, root)
	writeFile(t, filepath.Join(root, "generated/other.ts"), "export { value } from './stale-secret';\n")
	writeFile(t, filepath.Join(root, "generated/stale-secret.ts"), "export const value = 1;\n")
	output, err = protoGazelle(t, root)
	if err != nil {
		t.Fatalf("stale cross-package scalar changed its declared closure: %v\n%s", err, output)
	}
	if diff := snapshotDiff(cold, convergeSnapshot(t, root)); diff != "" {
		t.Fatalf("stale cross-package scalar changed its graph: %s", diff)
	}
}

func buildFileBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(root, func(file string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != "BUILD" && entry.Name() != "BUILD.bazel" {
			return err
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, file)
		files[filepath.ToSlash(rel)] = string(contents)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestGeneratedSelectionOwnsProgramMembership(t *testing.T) {
	for _, retained := range []string{"", "configured root", "other edge", "kept source", "aliased kept source", "mapped aliased kept source"} {
		for _, excluded := range []bool{false, true} {
			name := retained + "/eligible descendant"
			if excluded {
				name = retained + "/excluded descendant"
			}
			t.Run(name, func(t *testing.T) {
				files := `["index.ts"]`
				index := "import type { Value } from '#value'; export type Result = Value;\n"
				if retained == "configured root" {
					files = `["index.ts","fallback.ts"]`
				}
				if retained == "other edge" {
					index += "export type { Value as Fallback } from './fallback.js';\n"
				}
				kind, appBuild := "ts_compile", ""
				if strings.HasSuffix(retained, "kept source") {
					appBuild = loadDefs + `"ts_compile")` + "\n"
					if retained != "kept source" {
						kind = "custom_compile"
						wrapped := "ts_compile"
						if retained == "mapped aliased kept source" {
							wrapped = "mapped_compile"
							appBuild += "# gazelle:map_kind ts_compile mapped_compile //:defs.bzl\n"
						}
						appBuild += "load(\"//:defs.bzl\", \"custom_compile\")\n# gazelle:alias_kind custom_compile " + wrapped + "\n"
					}
					appBuild += kind + "(name = \"app\", srcs = [\n    \"fallback.ts\", # keep\n    \"index.ts\",\n])\n"
				}
				rootBuild := ""
				if excluded {
					rootBuild = "# gazelle:exclude app/private/secret.ts\n"
				}
				root := writeTree(t, map[string]string{
					"MODULE.bazel": "module(name = \"effective_membership\")\n",
					"BUILD.bazel":  rootBuild,
					"generated/BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(name = "types", outs = ["value.d.ts"], generator = ":generator")
`,
					"app/BUILD.bazel":   appBuild,
					"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#value":["../generated/value.js","./fallback.js"]}},"files":` + files + `}`,
					"app/index.ts":      index,
					"app/fallback.ts": "import type { Secret } from './private/secret';\n" +
						"import type { Extra } from './helper'; export type Value = [Secret, Extra];\n",
					"app/helper.ts":         "export type { Extra } from '../support/value';\n",
					"app/private/secret.ts": "export type Secret = string;\n",
					"support/tsconfig.json": `{"files":["value.ts"]}`,
					"support/value.ts":      "export type Extra = number;\n",
				})
				var cold map[string]string
				for _, state := range []string{"absent", "materialized", "rerun"} {
					if state == "materialized" {
						writeFile(t, filepath.Join(root, "generated/value.d.ts"), "export type Value = boolean;\n")
					}
					before := buildFileBytes(t, root)
					output, err := protoGazelle(t, root)
					if retained != "" && excluded {
						if err == nil || !strings.Contains(output, "app/fallback.ts imports app/private/secret.ts") || !strings.Contains(output, "excluded or ignored") {
							t.Fatalf("%s retained fallback lost its exclusion diagnostic: %v\n%s", state, err, output)
						}
						if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("%s rejected import wrote BUILD files: %s", state, diff)
						}
						continue
					}
					if err != nil {
						t.Fatalf("%s generation: %v\n%s", state, err, output)
					}
					consumer := onDiskRule(t, root, "app", kind, "app")
					wantSrcs, wantDeps := []string{"index.ts"}, []string{"//generated:types"}
					if retained != "" {
						wantSrcs = []string{"fallback.ts", "helper.ts", "index.ts", "private/secret.ts"}
						wantDeps = append(wantDeps, "//support:support")
					}
					wantStrings(t, state+" membership", consumer.AttrStrings("srcs"), wantSrcs)
					wantLabels(t, state+" dependencies", consumer.AttrStrings("deps"), wantDeps)
					if cold == nil {
						cold = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(cold, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("%s changed BUILD files: %s", state, diff)
					}
				}
			})
		}
	}
}

func TestNativeGazelleTestOnlyPackageKeepsSameNamedCodegen(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"test_codegen\")\n",
		"checks/BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(
    name = "checks",
    srcs = ["//tools:input.txt"],
    outs = ["value.d.ts"],
    generator = "//tools:generator",
    args = ["{out}", "{srcs}"],
)
`,
		"checks/tsconfig.json": `{"files":["index.test.ts"]}`,
		"checks/index.test.ts": "export {};\n",
		"tools/BUILD.bazel": loadDefs + `"ts_binary")
ts_binary(name = "generator", entry_point = "generator.mjs", visibility = ["//checks:__pkg__"])
exports_files(["input.txt"])
`,
		"tools/input.txt":     "export declare const value: number;\n",
		"tools/generator.mjs": "import { copyFileSync } from 'node:fs'; copyFileSync(process.argv[3], process.argv[2]);\n",
	})
	var first map[string]string
	for range 2 {
		output, err := protoGazelle(t, root)
		if err != nil {
			t.Fatalf("test-only generation: %v\n%s", err, output)
		}
		test := onDiskRule(t, root, "checks", "ts_test", "checks_test")
		wantStrings(t, "test sources", test.AttrStrings("srcs"), []string{"index.test.ts"})
		wantStrings(t, "generator dependency", test.AttrStrings("deps"), []string{":checks"})
		onDiskRule(t, root, "checks", "ts_codegen", "checks")
		if first == nil {
			first = buildFileBytes(t, root)
		} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
			t.Fatalf("rerun changed generator dependency: %s", diff)
		}
	}
}

func TestProtoConfigurationDoesNotOwnAuthoredSourceClosure(t *testing.T) {
	for _, state := range []string{"unused", "enabled_without_native", "neighboring_native"} {
		t.Run(state, func(t *testing.T) {
			build := `load("@rules_typescript//proto:defs.bzl", "ts_proto_config")
ts_proto_config(name = "plain", out_dir = "generated", tsconfig = ":tsconfig")
`
			if state != "unused" {
				build += "# gazelle:ts_proto plain\n"
			}
			tree := map[string]string{
				"MODULE.bazel":              "module(name = \"authored_proto_neighbor\")\n",
				"app/BUILD.bazel":           build,
				"app/tsconfig.json":         `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
				"app/index.ts":              "export { value } from './generated/value_pb.js';\n",
				"app/generated/value_pb.ts": "import { helper } from './helper.js'; export const value = helper;\n",
				"app/generated/helper.ts":   "export const helper = 42;\n",
			}
			if state == "neighboring_native" {
				tree["app/BUILD.bazel"] += "# gazelle:proto_strip_import_prefix /app\n"
				tree["app/actual.proto"] = `syntax = "proto3"; package example.actual; message Actual {}`
			}
			root := writeTree(t, tree)
			wantSrcs := []string{"generated/helper.ts", "generated/value_pb.ts", "index.ts"}
			if state == "neighboring_native" {
				wantSrcs = append([]string{"actual.proto"}, wantSrcs...)
			}
			var first map[string]string
			for range 2 {
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("authored closure: %v\n%s", err, output)
				}
				consumer := onDiskRule(t, root, "app", "ts_compile", "app")
				wantLabels(t, "authored membership", consumer.AttrStrings("srcs"), wantSrcs)
				wantStrings(t, "no generated dependency", consumer.AttrStrings("deps"), nil)
				if first == nil {
					first = buildFileBytes(t, root)
				} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("rerun changed authored membership: %s", diff)
				}
			}
		})
	}
}

func TestNativeGazelleErasedImportKeepsGeneratedDeclaration(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"erased_import\")\n",
		"app/BUILD.bazel": loadDefs + `"ts_codegen")
# gazelle:exclude companion.mjs
ts_codegen(name = "types", outs = ["companion.d.mts"], generator = ":generator")
`,
		"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
		"app/index.ts":      "import type { Value } from './companion.mjs'; export type Result = Value;\n",
		"app/companion.mjs": "throw new Error('excluded runtime must not be read');\n",
	})
	var cold map[string]string
	for _, present := range []bool{false, true} {
		if present {
			writeFile(t, filepath.Join(root, "app/companion.d.mts"), "export type Value = number;\n")
		}
		output, err := protoGazelle(t, root)
		if err != nil {
			t.Fatalf("declaration present=%t: erased import rejected: %v\n%s", present, err, output)
		}
		consumer := onDiskRule(t, root, "app", "ts_compile", "app")
		wantStrings(t, "declaration owner", consumer.AttrStrings("deps"), []string{":types"})
		wantStrings(t, "excluded JavaScript stays out", consumer.AttrStrings("srcs"), []string{"index.ts"})
		if cold == nil {
			cold = convergeSnapshot(t, root)
		} else if diff := snapshotDiff(cold, convergeSnapshot(t, root)); diff != "" {
			t.Fatal(diff)
		}
	}
}

func TestGeneratedDeclarationKnownTypeReasonsDoNotReachRuntime(t *testing.T) {
	for _, use := range []string{"reference", "type reference", "configured types", "declaration importer", "augmentation"} {
		for _, valueImport := range []bool{false, true} {
			name := use
			if valueImport {
				name += "/with value import"
			}
			t.Run(name, func(t *testing.T) {
				index := "export const result = 1;\n"
				wantSrcs := []string{"index.ts"}
				tree := map[string]string{
					"MODULE.bazel":      "module(name = \"declaration_reasons\")\n",
					"BUILD.bazel":       "# gazelle:exclude foreign/private.js\n# gazelle:exclude foreign/stale.ts\n",
					"app/tsconfig.json": `{"compilerOptions":{"allowJs":true,"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
					"foreign/BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(name = "types", outs = ["companion.d.ts"], generator = ":generator")
`,
					"foreign/companion.d.ts": "export type Value = number; export declare const value: number;\n",
					"foreign/companion.js":   "export { value } from './private.js';\n",
					"foreign/private.js":     "export const value = 1;\n",
					"foreign/stale.ts":       "export interface Hidden {}\n",
				}
				switch use {
				case "reference":
					index = "/// <reference path=\"../foreign/companion.d.ts\" />\n" + index
				case "type reference":
					index = "/// <reference types=\"../foreign/companion\" />\n" + index
				case "configured types":
					tree["app/tsconfig.json"] = `{"compilerOptions":{"allowJs":true,"module":"preserve","moduleResolution":"bundler","types":["../foreign/companion"]},"files":["index.ts","unused.ts"]}`
					tree["app/unused.ts"] = "export const unused = true;\n"
					tree["app/BUILD.bazel"] = loadDefs + `"ts_compile")
ts_compile(
    name = "app",
    srcs = ["index.ts"],  # keep
)
`
				case "declaration importer":
					index += "export type { Value } from './types';\n"
					tree["app/types.d.ts"] = "export type { Value } from '../foreign/companion.js';\n"
					wantSrcs = append(wantSrcs, "types.d.ts")
				case "augmentation":
					index += "declare module '../foreign/companion.js' { interface Extra { value: number } }\n"
				}
				if valueImport {
					index += "export { value } from '../foreign/companion.js';\n"
				}
				tree["app/index.ts"] = index
				root := writeTree(t, tree)
				states := []string{"present"}
				if use == "type reference" || use == "configured types" {
					states = []string{"absent", "present", "stale", "removed"}
				}
				var first map[string]string
				for _, state := range states {
					declaration := filepath.Join(root, "foreign/companion.d.ts")
					switch state {
					case "absent", "removed":
						if err := os.Remove(declaration); err != nil {
							t.Fatal(err)
						}
					case "present":
						writeFile(t, declaration, tree["foreign/companion.d.ts"])
					case "stale":
						writeFile(t, declaration, tree["foreign/companion.d.ts"]+"export type { Hidden } from './stale';\n")
					}
					output, err := protoGazelle(t, root)
					if err != nil {
						t.Fatalf("%s declaration import reached unobserved runtime: %v\n%s", state, err, output)
					}
					consumer := onDiskRule(t, root, "app", "ts_compile", "app")
					wantStrings(t, state+" declaration producer", consumer.AttrStrings("deps"), []string{"//foreign:types"})
					wantStrings(t, state+" no runtime sources", consumer.AttrStrings("srcs"), wantSrcs)
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("%s changed declaration closure: %s", state, diff)
					}
				}
			})
		}
	}
}

func TestNativeGazelleFileProbePrecedesSameNamedGeneratedTree(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"file_before_tree\")\n",
		"app/BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(name = "tree", out_dir = "generated", srcs = ["//tools:input.txt"], generator = "//tools:generator", args = ["{out}", "{srcs}"])
`,
		"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
		"app/index.ts":      "export { value } from './generated';\n",
		"app/generated.ts":  "export { value } from './helper';\n",
		"app/helper.ts":     "export const value = 'authored';\n",
		"tools/BUILD.bazel": loadDefs + `"ts_binary")
ts_binary(name = "generator", entry_point = "generator.mjs", visibility = ["//app:__pkg__"])
exports_files(["input.txt"])
`,
		"tools/input.txt": "export declare const value: string;\n",
		"tools/generator.mjs": "import { mkdirSync, copyFileSync, writeFileSync } from 'node:fs';\n" +
			"const out = process.argv[2]; mkdirSync(out, { recursive: true }); copyFileSync(process.argv[3], out + '/index.d.ts'); writeFileSync(out + '/index.js', \"export const value = 'generated';\\n\");\n",
	})
	var first map[string]string
	for _, state := range []string{"absent", "empty", "materialized"} {
		if state == "empty" {
			if err := os.MkdirAll(filepath.Join(root, "app/generated"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if state == "materialized" {
			writeFile(t, filepath.Join(root, "app/generated/index.d.ts"), "export declare const value: string;\n")
			writeFile(t, filepath.Join(root, "app/generated/index.js"), "export const value = 'generated';\n")
		}
		for range 2 {
			output, err := protoGazelle(t, root)
			if err != nil {
				t.Fatalf("%s generation: %v\n%s", state, err, output)
			}
			consumer := onDiskRule(t, root, "app", "ts_compile", "app")
			wantStrings(t, state+" authored closure", consumer.AttrStrings("srcs"), []string{"generated.ts", "helper.ts", "index.ts"})
			wantStrings(t, state+" declared codegen", consumer.AttrStrings("deps"), []string{":tree"})
			if first == nil {
				first = buildFileBytes(t, root)
			} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
				t.Fatalf("%s changed file precedence: %s", state, diff)
			}
		}
	}
}

func TestCompilerTreeCandidatesHonorResolvedFileOverride(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":            "module(name = \"tree_override\")\n",
		"BUILD.bazel":             "# gazelle:resolve typescript generated/tree/value.ts //owners:chosen\n",
		"app/tsconfig.json":       `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
		"app/index.ts":            "export { value } from '../generated/tree/value.js';\n",
		"generated/tree/value.ts": "export { value } from './stale';\n",
		"generated/tree/stale.ts": "export const value = 1;\n",
		"generated/BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(name = "tree", out_dir = "tree", generator = ":generator")
`,
		"owners/BUILD.bazel": loadDefs + `"ts_compile")
# keep
ts_compile(name = "chosen", srcs = ["//generated:tree"], emit = True)
`,
	})
	output, err := protoGazelle(t, root)
	if err != nil {
		t.Fatalf("generation: %v\n%s", err, output)
	}
	consumer := onDiskRule(t, root, "app", "ts_compile", "app")
	wantStrings(t, "override stops traversal", consumer.AttrStrings("srcs"), []string{"index.ts"})
	wantStrings(t, "override precedes tree", consumer.AttrStrings("deps"), []string{"//owners:chosen"})
}

func TestGenruleMembershipUsesDeclaredOutputBeforeAndAfterMaterialization(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"genrule_membership\")\n",
		"BUILD.bazel": loadDefs + `"ts_binary")
ts_binary(name = "run", entry_point = "//app")
`,
		"app/BUILD.bazel": loadDefs + `"ts_compile")
genrule(name = "producer", outs = ["value.ts"], cmd = "echo generated > $@")
# keep
ts_compile(name = "compiled", srcs = [":producer"], emit = True)
`,
		"app/tsconfig.json": `{"include":["*.ts"]}`,
		"app/index.ts":      "export { value } from './value.js';\n",
		"lib/secret.ts":     "export const secret = 1;\n",
	})
	var cold map[string]string
	for _, present := range []bool{false, true} {
		if present {
			writeFile(t, filepath.Join(root, "app/value.ts"), "import { secret } from '../lib/secret'; export const value = secret;\n")
		}
		output, err := protoGazelle(t, root)
		if err != nil {
			t.Fatalf("present=%t: %v\n%s", present, err, output)
		}
		consumer := onDiskRule(t, root, "app", "ts_compile", "app")
		wantStrings(t, "one compiler owner", consumer.AttrStrings("srcs"), []string{"index.ts"})
		wantStrings(t, "declared compiler dep", consumer.AttrStrings("deps"), []string{":compiled"})
		if !present {
			cold = convergeSnapshot(t, root)
		} else if diff := snapshotDiff(cold, convergeSnapshot(t, root)); diff != "" {
			t.Fatal(diff)
		}
		before := convergeSnapshot(t, root)
		output, err = protoGazelle(t, root, "-r=false", "app")
		if err != nil {
			t.Fatalf("partial update: %v\n%s", err, output)
		}
		if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
			t.Fatalf("partial update changed ownership: %s", diff)
		}
	}
}

func TestNativeGazelleErasedImportDoesNotDiscoverRuntime(t *testing.T) {
	for _, generated := range []bool{false, true} {
		t.Run(fmt.Sprintf("generated=%t", generated), func(t *testing.T) {
			const declaration = "export type Value = number; export declare const value: number;\n"
			build := "# gazelle:exclude private.mjs\n"
			if generated {
				build += loadDefs + `"ts_codegen")
ts_codegen(name = "types", outs = ["value.d.mts"], generator = ":generator")
`
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel":      `module(name = "erasure_boundary")` + "\n",
				"app/tsconfig.json": `{"compilerOptions":{"allowJs":true,"checkJs":true,"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
				"app/index.ts":      "import type { Value } from '../lib/value.mjs'; export const answer: Value = 42;\n",
				"lib/BUILD.bazel":   build,
				"lib/value.mjs":     "export { value } from './private.mjs';\n",
				"lib/private.mjs":   "export const value = 42;\n",
			})
			var cold map[string]string
			states := []string{"authored"}
			if generated {
				states = []string{"absent", "materialized"}
			}
			for _, state := range states {
				if state != "absent" {
					writeFile(t, filepath.Join(root, "lib/value.d.mts"), declaration)
				}
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("%s: erased import discovered runtime: %v\n%s", state, err, output)
				}
				consumer := onDiskRule(t, root, "app", "ts_compile", "app")
				wantSrcs, wantDeps := []string{"//lib:value.d.mts", "index.ts"}, []string(nil)
				if generated {
					wantSrcs, wantDeps = []string{"index.ts"}, []string{"//lib:types"}
				}
				wantLabels(t, "typing inputs only", consumer.AttrStrings("srcs"), wantSrcs)
				wantStrings(t, "typing owners only", consumer.AttrStrings("deps"), wantDeps)
				if cold == nil {
					cold = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(cold, convergeSnapshot(t, root)); diff != "" {
					t.Fatal(diff)
				}
			}
		})
	}
}

func TestGeneratedDiscoveryConfigCannotCreateOrWithdrawMembership(t *testing.T) {
	for _, position := range []string{"primary", "extends"} {
		t.Run(position, func(t *testing.T) {
			metadata := "app/tsconfig.json"
			config := `{"files":["index.ts"]}`
			build := loadDefs + `"ts_compile")
ts_compile(name = "app", srcs = ["index.ts"])
`
			files := map[string]string{
				"MODULE.bazel": "module(name = \"generated_discovery\")\n",
				"app/index.ts": "export const value = 1;\n",
			}
			if position == "extends" {
				metadata = "settings/base.json"
				files["app/tsconfig.json"] = `{"extends":"../settings/base.json","files":["index.ts"]}`
				files["settings/BUILD.bazel"] = `genrule(name = "config", outs = ["base.json"], cmd = "echo '{}' > $@")`
				config = `{"compilerOptions":{"types":["./stale.d.ts"]}}`
			} else {
				build += `genrule(name = "config", outs = ["tsconfig.json"], cmd = "echo '{}' > $@")`
			}
			files["BUILD.bazel"] = "# gazelle:exclude " + metadata + "\n"
			files["app/BUILD.bazel"] = build
			root := writeTree(t, files)
			before := convergeSnapshot(t, root)
			for _, state := range []string{"cold", "excluded stale"} {
				if state != "cold" {
					writeFile(t, filepath.Join(root, filepath.FromSlash(metadata)), config)
				}
				for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "app"}} {
					output, err := protoGazelle(t, root, args...)
					if err == nil || !strings.Contains(output, "cannot discover program membership from generated metadata "+metadata) {
						t.Fatalf("%s config %s with %v: %v\n%s", position, state, args, err, output)
					}
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("unreadable generated discovery changed BUILD files: %s", diff)
					}
				}
			}
		})
	}
}

func TestGeneratedResolverMetadataCannotSelectDeclaredCompilerOwners(t *testing.T) {
	for _, kind := range []string{"ts_compile", "custom_compile"} {
		for _, redirect := range []string{"./alternate.ts", "../other/alternate.ts"} {
			t.Run(kind+"/"+redirect, func(t *testing.T) {
				load, directives := loadDefs+`"ts_compile")`+"\n", ""
				if kind != "ts_compile" {
					load = "load(\"//:defs.bzl\", \"custom_compile\")\n"
					directives = "# gazelle:map_kind ts_compile mapped_compile //:defs.bzl\n# gazelle:alias_kind custom_compile mapped_compile\n"
				}
				root := writeTree(t, map[string]string{
					"MODULE.bazel":      "module(name = \"resolver_metadata\")\n",
					"BUILD.bazel":       "",
					"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
					"app/index.ts":      "export { value } from '../lib';\n",
					"lib/BUILD.bazel": load + directives +
						"genrule(name = \"manifest\", outs = [\"package.json\"], cmd = \"echo '{}' > $@\")\n" +
						kind + "(name = \"original\", srcs = [\"index.ts\"], visibility = [\"//visibility:public\"])\n" +
						kind + "(name = \"redirected\", srcs = [\"alternate.ts\"], visibility = [\"//visibility:public\"])\n",
					"lib/index.ts":       "export const value = 1;\n",
					"lib/alternate.ts":   "export const value = 2;\n",
					"other/BUILD.bazel":  load + directives + kind + "(name = \"redirected\", srcs = [\"alternate.ts\"], visibility = [\"//visibility:public\"])\n",
					"other/alternate.ts": "export const value = 3;\n",
				})
				for _, state := range []string{"cold", "stale"} {
					if state == "stale" {
						writeFile(t, filepath.Join(root, "lib/package.json"), fmt.Sprintf(`{"types":%q}`, redirect))
					}
					for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "-r=false", "app"}} {
						before := buildFileBytes(t, root)
						output, err := protoGazelle(t, root, args...)
						if err == nil || !strings.Contains(output, "generated metadata lib/package.json") {
							t.Fatalf("%s update %v trusted generated resolver metadata: %v\n%s", state, args, err, output)
						}
						if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("rejected resolver metadata changed BUILD files: %s", diff)
						}
					}
				}
			})
		}
	}
}

func TestConfigAndTypeConsumersCannotTrustGeneratedResolverMetadata(t *testing.T) {
	for _, consumer := range []string{"test config", "test config continuation", "configured types", "configured types continuation"} {
		t.Run(consumer, func(t *testing.T) {
			config := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.test.ts"]}`
			if consumer == "configured types" {
				config = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":["../lib"]},"files":["index.test.ts"]}`
			} else if consumer == "configured types continuation" {
				config = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":["../types"]},"files":["index.test.ts"]}`
			}
			tree := map[string]string{
				"MODULE.bazel":      "module(name = \"config_metadata\")\n",
				"app/tsconfig.json": config,
				"app/index.test.ts": "export const value = 1;\n",
				"lib/BUILD.bazel": loadDefs + `"ts_compile")` + "\n" +
					"genrule(name = \"manifest\", outs = [\"package.json\"], cmd = \"echo '{}' > $@\")\n" +
					"ts_compile(name = \"original\", srcs = [\"index.d.ts\"], visibility = [\"//visibility:public\"])\n",
				"lib/index.d.ts": "export declare const value: number;\n",
				"other/BUILD.bazel": loadDefs + `"ts_compile")` + "\n" +
					"ts_compile(name = \"redirected\", srcs = [\"value.d.ts\"], visibility = [\"//visibility:public\"])\n",
				"other/value.d.ts": "export declare const value: string;\n",
			}
			if consumer == "test config" {
				tree["app/vitest.config.mts"] = "import { value } from '../lib'; export default { name: value };\n"
			} else if consumer == "test config continuation" {
				tree["app/vitest.config.mts"] = "import { value } from './helper.ts'; export default { name: value };\n"
				tree["app/helper.ts"] = "export { value } from '../lib';\n"
			} else if consumer == "configured types continuation" {
				tree["types/BUILD.bazel"] = ""
				tree["types/index.d.ts"] = "export { value } from '../lib';\n"
			}
			root := writeTree(t, tree)
			for _, state := range []string{"cold", "stale"} {
				if state == "stale" {
					writeFile(t, filepath.Join(root, "lib/package.json"), `{"types":"../other/value.d.ts"}`)
				}
				for _, args := range [][]string{nil, {"-index=false", "-r=false", "app"}} {
					before := buildFileBytes(t, root)
					output, err := protoGazelle(t, root, args...)
					if err == nil || !strings.Contains(output, "generated metadata lib/package.json") {
						t.Fatalf("%s update %v trusted generated resolver metadata: %v\n%s", state, args, err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("rejected metadata changed BUILD files: %s", diff)
					}
				}
			}
		})
	}
}

func TestKeptProgramRootsAdmitOnlyRequiredPackageMetadata(t *testing.T) {
	for _, keep := range []string{"attribute", "rule"} {
		for _, retained := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retained=%t", keep, retained), func(t *testing.T) {
				sources := `["safe.ts"]`
				if retained {
					sources = `["safe.ts", "nested/unused.ts"]`
				}
				build := loadDefs + `"ts_compile")` + "\n"
				attrKeep := " # keep"
				if keep == "rule" {
					build += "# keep\n"
					attrKeep = ""
				}
				build += "ts_compile(\n    name = \"app\",\n    srcs = " + sources + "," + attrKeep + "\n)\n"
				root := writeTree(t, map[string]string{
					"MODULE.bazel":           "module(name = \"selected_scope\")\n",
					"app/BUILD.bazel":        build,
					"app/tsconfig.json":      `{"files":["safe.ts","nested/unused.ts"]}`,
					"app/safe.ts":            "export const safe = 1;\n",
					"app/nested/unused.ts":   "export const unused = 2;\n",
					"app/nested/BUILD.bazel": "genrule(name = \"manifest\", outs = [\"package.json\"], cmd = \"echo '{}' > $@\")\n",
				})
				for _, state := range []string{"cold", "stale"} {
					if state == "stale" {
						writeFile(t, filepath.Join(root, "app/nested/package.json"), `{"type":"module"}`)
					}
					for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "-r=false", "app"}} {
						before := buildFileBytes(t, root)
						output, err := protoGazelle(t, root, args...)
						if retained {
							if err == nil || !strings.Contains(output, "generated package scope app/nested/package.json") {
								t.Fatalf("%s update %v ignored required source scope: %v\n%s", state, args, err, output)
							}
							if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
								t.Fatalf("rejected source scope changed BUILD files: %s", diff)
							}
						} else {
							if err != nil {
								t.Fatalf("%s update %v required discarded source scope: %v\n%s", state, args, err, output)
							}
							wantStrings(t, "effective roots exclude the unused source", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("srcs"), []string{"safe.ts"})
						}
					}
				}
			})
		}
	}
}

func TestGeneratedProgramScopeCannotChangeEmissionOrForeignMembership(t *testing.T) {
	for _, lock := range []bool{false, true} {
		t.Run(fmt.Sprint(lock), func(t *testing.T) {
			files := map[string]string{
				"MODULE.bazel": "module(name = \"generated_program_scope\")\n",
				"BUILD.bazel":  "# gazelle:exclude app/package.json\n",
				"app/BUILD.bazel": loadDefs + `"ts_compile")
genrule(name = "manifest", outs = ["package.json"], cmd = "echo '{}' > $@")
ts_compile(name = "app", srcs = ["index.ts"])
`,
				"app/tsconfig.json": `{"files":["index.ts"]}`,
				"app/index.ts":      "export const value = 1;\n",
			}
			if lock {
				files[pnpmLockfileName] = "lockfileVersion: '9.0'\nimporters:\n  .: {}\n"
				files["node_modules/.modules.yaml"] = "layoutVersion: 5\n"
			}
			root := writeTree(t, files)
			before := convergeSnapshot(t, root)
			for _, state := range []string{"cold", "excluded stale"} {
				if state != "cold" {
					writeFile(t, filepath.Join(root, "app/package.json"), `{"name":"app","exports":"./index.js"}`)
				}
				for _, args := range [][]string{nil, {"-r=false", "app"}} {
					output, err := protoGazelle(t, root, args...)
					if err == nil || !strings.Contains(output, "imports app/index.ts with generated package scope app/package.json") || strings.Contains(output, "is no importer") {
						t.Fatalf("%s scope with %v changed discovery policy: %v\n%s", state, args, err, output)
					}
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("generated scope changed emission or withdrew an owner: %s", diff)
					}
				}
			}
		})
	}
}

func TestUntouchedCompilerMetadataCannotAbortUnrelatedUpdates(t *testing.T) {
	for _, owner := range []struct{ name, kind, target, directives string }{
		{"conventional", "ts_compile", "lib", ""},
		{"arbitrary", "ts_compile", "library", ""},
		{"mapped", "custom_compile", "lib", "# gazelle:map_kind ts_compile custom_compile //:defs.bzl\n"},
		{"aliased", "custom_compile", "lib", "# gazelle:alias_kind custom_compile ts_compile\n"},
	} {
		for _, metadata := range []struct{ name, declaration, discovery, file, contents, diagnostic string }{
			{"generated primary", `genrule(name = "metadata", outs = ["tsconfig.json"], cmd = "echo '{}' > $@")`, "", "tsconfig.json", `{"files":["value.ts"]}`, ""},
			{"unknown primary", "OUTPUTS = [\"tsconfig.json\"]\ngenrule(name = \"metadata\", outs = OUTPUTS, cmd = \"echo '{}' > $@\")", "", "tsconfig.json", `{"files":["value.ts"]}`, ""},
			{"generated extends", `genrule(name = "metadata", outs = ["base.json"], cmd = "echo '{}' > $@")`, `{"extends":"./base.json","files":["value.ts"]}`, "base.json", `{"compilerOptions":{"module":"preserve"}}`, "generated metadata lib/base.json"},
			{"generated scope", `genrule(name = "metadata", outs = ["package.json"], cmd = "echo '{}' > $@")`, `{"files":["value.ts"]}`, "package.json", `{"type":"module"}`, ""},
		} {
			for _, update := range []struct {
				name, directive string
				args            []string
			}{
				{"language opt-out", "# gazelle:lang proto\n", nil},
				{"ignored", "# gazelle:ignore\n", nil},
				{"indexed sibling", "", []string{"-r=false", "app"}},
				{"unindexed sibling", "", []string{"-index=false", "-r=false", "app"}},
			} {
				t.Run(owner.name+"/"+metadata.name+"/"+update.name, func(t *testing.T) {
					load := loadDefs + `"ts_compile")` + "\n"
					if owner.kind != "ts_compile" {
						load = "load(\"//:defs.bzl\", \"" + owner.kind + "\")\n"
					}
					libBuild := load + owner.directives + update.directive + metadata.declaration + "\n" +
						fmt.Sprintf("%s(name = %q, srcs = [\"value.ts\"], visibility = [\"//visibility:public\"])\n", owner.kind, owner.target)
					formatted, err := rule.LoadData("lib/BUILD.bazel", "lib", []byte(libBuild))
					if err != nil {
						t.Fatal(err)
					}
					libBuild = string(formatted.Format())
					tree := map[string]string{
						"MODULE.bazel":      "module(name = \"untouched_metadata\")\n",
						"BUILD.bazel":       "",
						"lib/BUILD.bazel":   libBuild,
						"lib/value.ts":      "export const value = 42;\n",
						"app/tsconfig.json": `{"files":["index.ts"]}`,
						"app/index.ts":      "export const unrelated = 1;\n",
					}
					if metadata.discovery != "" {
						tree["lib/tsconfig.json"] = metadata.discovery
					}
					root := writeTree(t, tree)
					var first map[string]string
					for _, state := range []string{"cold", "stale", "repeated"} {
						if state == "stale" {
							writeFile(t, filepath.Join(root, "lib", metadata.file), metadata.contents)
						}
						if output, err := protoGazelle(t, root, update.args...); err != nil {
							t.Fatalf("%s unrelated update: %v\n%s", state, err, output)
						}
						wantStrings(t, "untouched compiler retains its sources", onDiskRule(t, root, "lib", owner.kind, owner.target).AttrStrings("srcs"), []string{"value.ts"})
						if got := buildFileText(t, root, "lib"); got != libBuild {
							t.Fatalf("%s observation changed untouched BUILD bytes: %s", state, lineDiff(libBuild, got))
						}
						wantStrings(t, "unrelated program generates normally", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("srcs"), []string{"index.ts"})
						if first == nil {
							first = convergeSnapshot(t, root)
						} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("%s metadata changed untouched membership: %s", state, diff)
						}
						if metadata.discovery != "" {
							appBuild := buildFileText(t, root, "app")
							writeFile(t, filepath.Join(root, "app/BUILD.bazel"), loadDefs+`"ts_compile")
ts_compile(
    name = "app",
    srcs = ["index.ts"],
    tsconfig = "//lib:tsconfig.json", # keep
)
`)
							before := buildFileBytes(t, root)
							output, err := protoGazelle(t, root, update.args...)
							if metadata.diagnostic != "" {
								if err == nil || !strings.Contains(output, metadata.diagnostic) {
									t.Fatalf("%s selected config accepted refused discovery: %v\n%s", state, err, output)
								}
								if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
									t.Fatalf("%s selected config refusal changed BUILD files: %s", state, diff)
								}
							} else {
								if err != nil {
									t.Fatalf("%s selected config required discarded source scope: %v\n%s", state, err, output)
								}
								app := onDiskRule(t, root, "app", "ts_compile", "app")
								wantStrings(t, "selected config retains only effective sources", app.AttrStrings("srcs"), []string{"index.ts"})
								if got := app.AttrString("tsconfig"); got != "//lib:tsconfig.json" {
									t.Fatalf("selected config = %q, want //lib:tsconfig.json", got)
								}
								if got := buildFileText(t, root, "lib"); got != libBuild {
									t.Fatalf("%s selected config changed untouched BUILD bytes: %s", state, lineDiff(libBuild, got))
								}
							}
							writeFile(t, filepath.Join(root, "app/BUILD.bazel"), appBuild)
						}
					}
				})
			}
		}
	}
}

func TestIncompleteDiscoveryCannotCreateOrWithdrawPrograms(t *testing.T) {
	for _, declaration := range []struct{ name, rule string }{
		{"identifier", `OUTPUTS = ["tsconfig.json"]
genrule(name = "metadata", outs = OUTPUTS, cmd = "echo '{}' > $@")`},
		{"expression", `genrule(name = "metadata", outs = ["tsconfig.json"] + [], cmd = "echo '{}' > $@")`},
		{"tree", loadDefs + `"ts_codegen")
DIRECTORY = "generated"
ts_codegen(name = "metadata", out_dir = DIRECTORY, generator = ":generator")`},
	} {
		for _, owner := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/owner=%t", declaration.name, owner), func(t *testing.T) {
				build := declaration.rule
				if owner {
					build = loadDefs + `"ts_compile")` + "\n" + build + "\n" + `ts_compile(name = "app", srcs = ["index.ts"])`
				}
				root := writeTree(t, map[string]string{
					"MODULE.bazel":    `module(name = "unknown_discovery")`,
					"app/BUILD.bazel": build,
					"app/index.ts":    "export const value = 1;\n",
				})
				before := convergeSnapshot(t, root)
				if !owner {
					before = nil
				}
				for _, stale := range []bool{false, true} {
					if stale {
						dir := "app"
						config := `{"files":["index.ts"]}`
						if declaration.name == "tree" {
							dir = "app/generated"
							config = `{"files":["../index.ts"]}`
						}
						writeFile(t, filepath.Join(root, dir, "tsconfig.json"), config)
						writeFile(t, filepath.Join(root, dir, "package.json"), `{"name":"stale","exports":"./index.js"}`)
					}
					for _, args := range [][]string{nil, {"-index=false", "app"}} {
						output, err := protoGazelle(t, root, args...)
						if (err != nil) != owner || !strings.Contains(output, "cannot determine output provenance: //app:metadata") {
							t.Fatalf("stale=%t %v: error=%v, owner=%t\n%s", stale, args, err, owner, output)
						}
						if !owner {
							for _, r := range loadRules(t, root, "app") {
								if r.Kind() == "ts_compile" || r.Kind() == "ts_test" || r.Kind() == "ts_config" {
									t.Fatal("unknown primary discovery synthesized a program")
								}
							}
						}
						if before == nil {
							before = convergeSnapshot(t, root)
						}
						if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
							t.Fatalf("unknown discovery changed the BUILD graph: %s", diff)
						}
					}
				}
			})
		}
	}
}

func TestIncompleteSiblingConfigRejectsRequiredExtendsBeforeVisit(t *testing.T) {
	for _, declaration := range []string{
		`OUTPUT = "base.json"
genrule(name = "metadata", outs = [OUTPUT], cmd = "echo '{}' > $@")`,
		`load("@bazel_skylib//rules:write_file.bzl", "write_file")
OUTPUT = "base.json"
write_file(name = "metadata", out = OUTPUT, content = ["{}"])`,
	} {
		root := writeTree(t, map[string]string{
			"MODULE.bazel":         `module(name = "unknown_extends")`,
			"app/tsconfig.json":    `{"extends":"../settings/base.json","files":["index.ts"]}`,
			"app/index.ts":         "export const value = 1;\n",
			"settings/BUILD.bazel": declaration,
		})
		before := convergeSnapshot(t, root)
		for _, stale := range []bool{false, true} {
			if stale {
				writeFile(t, filepath.Join(root, "settings/base.json"), `{"compilerOptions":{"module":"preserve"}}`)
			}
			for _, args := range [][]string{nil, {"-index=false", "app"}} {
				output, err := protoGazelle(t, root, args...)
				if err == nil || !strings.Contains(output, "cannot determine output provenance: //settings:metadata") {
					t.Fatalf("stale=%t %v did not reject required unknown config: %v\n%s", stale, args, err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("unknown extends changed BUILD files: %s", diff)
				}
			}
		}
	}
}

func TestUnknownDiscoveryPreservesExplicitProgramsAndUnrelatedRules(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": `module(name = "explicit_opaque_outputs")`,
		"explicit/BUILD.bazel": loadDefs + `"ts_test")
genrule(name = "tests", outs = ["t{}.test.ts".format(i) for i in range(2)], cmd = "touch $(OUTS)")
# keep
ts_test(name = "explicit_test", srcs = [":tests"], config = "vitest.config.mjs")
ts_test(name = "one_file_test", srcs = ["one.test.ts"], config = "vitest.config.mjs", data = [":tests"])
`,
		"explicit/one.test.ts":       "export {};\n",
		"explicit/vitest.config.mjs": "export default {};\n",
		"unrelated/BUILD.bazel": `OUTPUTS = ["version.go"]
genrule(name = "generated", outs = OUTPUTS, cmd = "echo 'package unrelated' > $@")
filegroup(name = "files", srcs = [":generated"])
`,
		"unrelated/authored.go":  "package unrelated\n",
		"ordinary/tsconfig.json": `{"files":["index.ts"]}`,
		"ordinary/index.ts":      "export const value = 1;\n",
	})
	var explicit, unrelated string
	for _, stale := range []bool{false, true} {
		if stale {
			writeFile(t, filepath.Join(root, "explicit/tsconfig.json"), `{"files":["stale.ts"]}`)
			writeFile(t, filepath.Join(root, "explicit/package.json"), `{"name":"stale"}`)
		}
		output, err := protoGazelle(t, root)
		if err != nil {
			t.Fatalf("explicit programs stale=%t: %v\n%s", stale, err, output)
		}
		if !stale {
			explicit = buildFileText(t, root, "explicit")
			unrelated = buildFileText(t, root, "unrelated")
		}
		wantStrings(t, "kept producer input", onDiskRule(t, root, "explicit", "ts_test", "explicit_test").AttrStrings("srcs"), []string{":tests"})
		wantStrings(t, "independent test input", onDiskRule(t, root, "explicit", "ts_test", "one_file_test").AttrStrings("srcs"), []string{"one.test.ts"})
		wantStrings(t, "independent test data", onDiskRule(t, root, "explicit", "ts_test", "one_file_test").AttrStrings("data"), []string{":tests"})
		if got := buildFileText(t, root, "explicit"); got != explicit {
			t.Fatalf("opaque producer changed explicit programs: %s", lineDiff(explicit, got))
		}
		if got := buildFileText(t, root, "unrelated"); got != unrelated {
			t.Fatalf("unrelated computed outputs changed: %s", lineDiff(unrelated, got))
		}
		wantStrings(t, "ordinary authored membership", onDiskRule(t, root, "ordinary", "ts_compile", "ordinary").AttrStrings("srcs"), []string{"index.ts"})
	}
}

func TestUnknownPackageScopeCannotSupplyBorrowedInputs(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":      `module(name = "unknown_scope")`,
		"app/tsconfig.json": `{"files":["index.ts"]}`,
		"app/index.ts":      "export { value } from '../foreign/value';\n",
		"foreign/value.ts":  "export const value = 1;\n",
		"foreign/BUILD.bazel": `OUTPUTS = ["package.json"]
genrule(name = "metadata", outs = OUTPUTS, cmd = "echo '{}' > $@")
exports_files(["value.ts"])
`,
	})
	before := convergeSnapshot(t, root)
	for _, stale := range []bool{false, true} {
		if stale {
			writeFile(t, filepath.Join(root, "foreign/package.json"), `{"name":"stale","type":"module"}`)
		}
		output, err := protoGazelle(t, root)
		if err == nil || !strings.Contains(output, "cannot determine output provenance: //foreign:metadata") {
			t.Fatalf("unknown scope stale=%t: %v\n%s", stale, err, output)
		}
		if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
			t.Fatalf("unknown scope changed BUILD files: %s", diff)
		}
	}
}

func TestUnknownInheritedVitestConfigCannotWithdrawWorkerSettings(t *testing.T) {
	for _, keep := range []string{"", "# keep\n"} {
		t.Run(fmt.Sprintf("kept=%t", keep != ""), func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel": `module(name = "unknown_inherited_config")`,
				"BUILD.bazel": `OUTPUTS = ["vitest.config.mjs"]
genrule(name = "metadata", outs = OUTPUTS, cmd = "echo 'export default {}' > $@")
filegroup(name = "vitest_config", srcs = ["vitest.config.mjs"])
filegroup(name = "wrangler_config", srcs = ["wrangler.jsonc"])
`,
				"app/tsconfig.json": `{"files":["index.test.ts"]}`,
				"app/package.json":  `{}`,
				"app/index.test.ts": "export {};\n",
				"app/BUILD.bazel": loadDefs + `"ts_test")` + "\n" + keep + `ts_test(
    name = "app_test",
    srcs = ["index.test.ts"],
    config = "//:vitest_config",  # keep
    wrangler_config = "//:wrangler_config",
    coverage_provider = "istanbul",
)
`,
			})
			before := convergeSnapshot(t, root)
			for _, stale := range []bool{false, true} {
				if stale {
					writeFile(t, filepath.Join(root, "vitest.config.mjs"), "export default {};\n")
				}
				output, err := protoGazelle(t, root)
				if err == nil || !strings.Contains(output, "cannot determine output provenance: //:metadata") {
					t.Fatalf("unknown inherited config stale=%t: %v\n%s", stale, err, output)
				}
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("unknown config withdrew worker settings: %s", diff)
				}
			}
		})
	}
	for _, consumer := range []struct {
		name, groupKeep, srcsKeep, testKeep, wranglerKeep string
		wantError                                         bool
	}{
		{name: "automatic filegroup", testKeep: "# keep\n", wantError: true},
		{name: "automatic test", srcsKeep: " # keep", wantError: true},
		{name: "manual attributes", srcsKeep: " # keep", wranglerKeep: " # keep"},
		{name: "manual rules", groupKeep: "# keep\n", testKeep: "# keep\n"},
	} {
		t.Run("named Wrangler/"+consumer.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel":               `module(name = "unknown_named_wrangler")`,
				"package.json":               `{"name":"w","devDependencies":{"@vitest/coverage-istanbul":"4.1.11"}}`,
				pnpmLockfileName:             poolLock,
				"node_modules/.modules.yaml": "layoutVersion: 5\n",
				"node_modules/@cloudflare/vitest-pool-workers/package.json": `{"name":"@cloudflare/vitest-pool-workers","version":"0.18.4","types":"index.d.ts"}`,
				"node_modules/@cloudflare/vitest-pool-workers/index.d.ts":   "export declare function cloudflareTest(o: unknown): unknown;\n",
				"worker/package.json":      `{"name":"worker","devDependencies":{"@cloudflare/vitest-pool-workers":"0.18.4"}}`,
				"worker/vitest.config.mts": poolConfig("./config/wrangler.jsonc"),
				"worker/tsconfig.json":     includeSrc,
				"worker/src/index.ts":      "export const w = 1;\n",
				"worker/BUILD.bazel": consumer.groupKeep + `filegroup(
    name = "wrangler_config",
    srcs = ["//worker/config:wrangler.jsonc"],` + consumer.srcsKeep + `
    visibility = ["//visibility:public"],
)
`,
				"worker/config/BUILD.bazel": `# gazelle:exclude wrangler.jsonc
OUTPUTS = ["wrangler.jsonc"]
genrule(name = "metadata", outs = OUTPUTS, cmd = "echo '{}' > $@", visibility = ["//visibility:public"])
`,
				"worker/test/tsconfig.json": includeTs,
				"worker/test/index.test.ts": "import { w } from '../src/index'; export const t = w;\n",
				"worker/test/BUILD.bazel": loadDefs + `"ts_test")` + "\n" + consumer.testKeep + `ts_test(
    name = "test_test",
    srcs = ["index.test.ts"],
    config = "//worker:vitest_config",
    wrangler_config = "//worker:wrangler_config",` + consumer.wranglerKeep + `
    coverage_provider = "istanbul",
    deps = ["//worker", "@npm//:vitest_coverage-istanbul", "@npm//worker:cloudflare_vitest-pool-workers"],
)
`,
			})
			before := buildFileBytes(t, root)
			for _, stale := range []bool{false, true} {
				if stale {
					writeFile(t, filepath.Join(root, "worker/config/wrangler.jsonc"), `{"main":"stale.ts"}`)
				}
				for _, args := range [][]string{nil, {"-index=false", "-r=false", "worker", "worker/test"}} {
					output, err := protoGazelle(t, root, args...)
					if consumer.wantError {
						if err == nil || !strings.Contains(output, "cannot determine output provenance: //worker/config:metadata") {
							t.Fatalf("unknown named Wrangler stale=%t: %v\n%s", stale, err, output)
						}
					} else {
						if err != nil {
							t.Fatalf("manual Wrangler stale=%t: %v\n%s", stale, err, output)
						}
						wrangler := onDiskRule(t, root, "worker", "filegroup", "wrangler_config")
						wantStrings(t, "manual Wrangler inputs", wrangler.AttrStrings("srcs"), []string{"//worker/config:wrangler.jsonc"})
						test := onDiskRule(t, root, "worker/test", "ts_test", "test_test")
						if test.AttrString("wrangler_config") != "//worker:wrangler_config" || test.AttrString("coverage_provider") != "istanbul" {
							t.Fatalf("manual Wrangler settings changed: %s", buildFileText(t, root, "worker/test"))
						}
						if !stale && len(args) == 0 {
							before = buildFileBytes(t, root)
						}
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("unknown named Wrangler changed BUILD files: %s", diff)
					}
				}
			}
		})
	}
}

func TestKeptTestRootsCannotUseUnmergedGeneratedMembership(t *testing.T) {
	for _, keep := range []string{"element", "attribute"} {
		t.Run(keep, func(t *testing.T) {
			config := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true},"files":["a.test.ts"]}`
			srcs := "[\n        \"a.test.ts\",\n        \"//other:b.test.ts\",  # keep\n    ],"
			roots := []string{"a.test.ts", "//other:b.test.ts"}
			imports := "export { value } from '../support/helper';\n"
			if keep == "attribute" {
				config = strings.Replace(config, `"a.test.ts"]`, `"a.test.ts","b.test.ts"]`, 1)
				srcs = "[\"a.test.ts\", \"//support:fixtures\"],  # keep"
				roots = []string{"a.test.ts", "//support:fixtures"}
				imports = "import data from '../support/fixture.json'; export const value = data.value;\n"
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel":         `module(name = "kept_test_roots")`,
				"app/tsconfig.json":    config,
				"app/BUILD.bazel":      loadDefs + "\"ts_test\")\nts_test(name = \"app_test\", srcs = " + srcs + "\n)\n",
				"app/a.test.ts":        imports,
				"app/b.test.ts":        "export const discarded = 1;\n",
				"other/BUILD.bazel":    `exports_files(["b.test.ts"])`,
				"other/b.test.ts":      "export const kept = 1;\n",
				"support/BUILD.bazel":  "exports_files([\"helper.ts\", \"other.ts\", \"fixture.json\"])\nfilegroup(name = \"fixtures\", srcs = [\"fixture.json\"], visibility = [\"//visibility:public\"])\n",
				"support/helper.ts":    "export const value = 42;\n",
				"support/other.ts":     "export const value = 42;\n",
				"support/fixture.json": `{"value":42}`,
			})
			states := []string{"helper", "removed"}
			if keep == "element" {
				states = []string{"helper", "nested helper", "removed"}
			}
			for _, state := range states {
				if state == "nested helper" {
					writeFile(t, filepath.Join(root, "support/helper.ts"), "export { value } from './other';\n")
				} else if state == "removed" {
					writeFile(t, filepath.Join(root, "app/a.test.ts"), "export const value = 42;\n")
				}
				var first map[string]string
				for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "-r=false", "app"}, nil} {
					if output, err := protoGazelle(t, root, args...); err != nil {
						t.Fatalf("%s update %v: %v\n%s", state, args, err, output)
					}
					r := onDiskRule(t, root, "app", "ts_test", "app_test")
					if state == "removed" && keep == "element" {
						wantLabels(t, "kept roots after helper removal", r.AttrStrings("srcs"), roots)
						if r.Attr("test_srcs") != nil {
							t.Fatal("removed helper left a redundant execution-root selection")
						}
					} else if keep == "element" {
						wantLabels(t, "execution roots use effective kept membership", r.AttrStrings("test_srcs"), roots)
					}
					if keep == "attribute" {
						wantLabels(t, "kept subset remains compiler membership", r.AttrStrings("srcs"), roots)
						if r.Attr("test_srcs") != nil {
							t.Fatal("kept compiler membership left a redundant execution-root selection")
						}
					} else if state != "removed" {
						want := append(slices.Clone(roots), "//support:helper.ts")
						if state == "nested helper" {
							want = append(want, "//support:other.ts")
						}
						wantLabels(t, "kept root and borrowed closure remain available", r.AttrStrings("srcs"), want)
					}
					if first == nil {
						first = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(first, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("%s update %v changed kept execution roots: %s", state, args, diff)
					}
				}
			}
		})
	}
}

func TestBorrowedHelpersCannotBecomeTestRoots(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":        `module(name = "test_roots")`,
		"app/tsconfig.json":   `{"files":["index.test.ts"]}`,
		"app/index.test.ts":   "export { value } from '../support/helper';\n",
		"support/BUILD.bazel": `exports_files(["helper.ts", "other.ts"])`,
		"support/helper.ts":   "export const value = 42;\n",
		"support/other.ts":    "export const value = 42;\n",
	})
	for _, helper := range []string{"export const value = 42;\n", "export { value } from './other';\n"} {
		writeFile(t, filepath.Join(root, "support/helper.ts"), helper)
		for _, args := range [][]string{nil, {"-r=false", "app"}} {
			if output, err := protoGazelle(t, root, args...); err != nil {
				t.Fatalf("%v: %v\n%s", args, err, output)
			}
			r := onDiskRule(t, root, "app", "ts_test", "app_test")
			wantStrings(t, "execution roots", r.AttrStrings("test_srcs"), []string{"index.test.ts"})
			if !slices.Contains(r.AttrStrings("srcs"), "//support:helper.ts") || strings.Contains(helper, "./other") && !slices.Contains(r.AttrStrings("srcs"), "//support:other.ts") {
				t.Fatalf("helper closure was not retained: %v", r.AttrStrings("srcs"))
			}
		}
	}
	writeFile(t, filepath.Join(root, "app/index.test.ts"), "export const value = 42;\n")
	if output, err := protoGazelle(t, root); err != nil {
		t.Fatalf("removed helper: %v\n%s", err, output)
	}
	r := onDiskRule(t, root, "app", "ts_test", "app_test")
	wantStrings(t, "remaining roots", r.AttrStrings("srcs"), []string{"index.test.ts"})
	if r.Attr("test_srcs") != nil {
		t.Fatal("redundant execution-root subset survived helper removal")
	}
}
