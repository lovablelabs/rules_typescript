package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
	"github.com/mikn/rules_typescript/tests/integration/harness"
)

// A deleted test target satisfies both "the output builds" and "two runs are
// byte-identical", which is how seven hand-written go_tests once disappeared.
func testTargets(it *harness.IT) []string {
	out := strings.Fields(it.BazelStdout("query", "tests(//...)"))
	sort.Strings(out)
	return out
}

// labels is what Bazel loaded for one attribute of one target, sorted.
func labels(it *harness.IT, attr, target string) []string {
	query := fmt.Sprintf("labels(%s, %s)", attr, target)
	out := strings.Fields(it.BazelStdout("query", query))
	sort.Strings(out)
	return out
}

func testValidationStamp(it *harness.IT, target string, arguments []string) string {
	var stamps []string
	for _, argument := range arguments {
		if stamp, ok := strings.CutPrefix(argument, "-stamp="); ok {
			stamps = append(stamps, stamp)
		}
	}
	if len(stamps) != 1 || stamps[0] == "" {
		it.Fail("%s consumer action names %v check stamps, want exactly one", target, stamps)
	}
	expression := fmt.Sprintf(`"\n".join([file.path for file in providers(target)["OutputGroupInfo"]._validation.to_list() if file.path == %q])`, stamps[0])
	files := strings.Fields(it.BazelStdout("cquery", target, "--output=starlark", "--starlark:expr="+expression))
	if len(files) != 1 {
		it.Fail("%s publishes %v for consumer check stamp %s, want exactly one", target, files, stamps[0])
	}
	return it.Path(files[0])
}

func requireLabels(it *harness.IT, attr, target string, want []string) {
	want = slices.Sorted(slices.Values(want))
	if got := labels(it, attr, target); !slices.Equal(got, want) {
		it.Fail("%s of %s = %v, want %v", attr, target, got, want)
	}
}

// The ts_test Gazelle writes for a package is named after its directory.
func testTarget(dir string) string {
	return fmt.Sprintf("//%s:%s_test", dir, filepath.Base(dir))
}

// A ts_test's deps carry its manifest's dependencies and devDependencies
// beside its edges; under the root package.json that is these six.
var rootManifestDeps = []string{"//:node_modules/shared", "@npm//:culori",
	"@npm//:types_culori", "@npm//:types_node", "@npm//:vite", "@npm//:vitest"}

func withRootManifest(own ...string) []string {
	return slices.Sorted(slices.Values(slices.Concat(own, rootManifestDeps)))
}

// The packages Gazelle writes whole: deleted between the two passes.
var generated = []string{
	"src/lib", "src/app", "src/icons", "src/typed", "src/env", "foreign_json", "foreign_consumer",
	"worker", "worker/test", "worker/test/deep",
	"aliased", "aliased/src", "jsx", "jsx/runtime",
	"configured", "configured/test", "packages/shared",
	"member", "dotdot", "dotdot/inner", "pooled/test", "augmented", "devserver",
}

// BUILD files a run merges into rather than writes, the root's among them.
var handWritten = []string{
	"", "src/i18n", "src/locales", "generated_worker",
	"shared_program", "wrangler_lock", "pooled",
}

// Each test target names one check group here, so every group fits its own
// timeout; the empty name is the round trip itself.
var checkGroups = map[string]func(it *harness.IT, foreignExports string){
	"scope":      scopeChecks,
	"transitive": transitiveScopeChecks,
	"config":     configChecks,
	"packages":   packageScopeChecks,
	"foreign":    foreignChecks,
	"runtime":    runtimeChecks,
	"borrowed":   borrowedChecks,
	"roots":      rootsChecks,
}

func main() {
	started := time.Now()
	group := os.Getenv("GAZELLE_ROUNDTRIP_GROUP")
	name := "gazelle_roundtrip"
	if group != "" {
		name += "_" + group
	}
	harness.Run(harness.Config{
		Name:         name,
		WorkspaceRel: "tests/integration/gazelle_roundtrip",
	}, func(it *harness.IT) {
		switch group {
		case "":
			roundtrip(it)
			return
		case "conformance":
			deadline := conformanceDeadline(it, started)
			generate(it)
			formalConformance(it, deadline)
			return
		}
		checks, ok := checkGroups[group]
		if !ok {
			it.Fail("unknown GAZELLE_ROUNDTRIP_GROUP %q", group)
		}
		foreignExports, _ := generate(it)
		// The round trip checks Gazelle emptied this file; a consumer deletes it before building.
		if err := os.Remove(it.Path("worker/src/BUILD.bazel")); err != nil {
			it.Fail("cannot delete the emptied BUILD file: %v", err)
		}
		checks(it, foreignExports)
	})
}

// generate stages the hand-written packages, installs, and runs Gazelle once.
func generate(it *harness.IT) (foreignExports string, gazelleLog *harness.Log) {
	// Written here, not shipped: a BUILD file under a --deleted_packages
	// entry is the OUTER workspace's, and glob_workspace_files stops there.
	it.Write(it.Path("src/i18n/BUILD.bazel"), cataloguePackage)
	it.Write(it.Path("src/locales/BUILD.bazel"), outDirCodegenPackage)
	it.Write(it.Path("generated_worker/BUILD.bazel"), generatedWorkerPackage)
	it.Write(it.Path("shared_program/BUILD.bazel"), sharedProgramPackage)
	it.Write(it.Path("worker/src/BUILD.bazel"), staleWorkerSrcPackage)
	it.Write(it.Path("wrangler_lock/BUILD.bazel"), wranglerLock)
	it.Write(it.Path("pooled/wrangler.source.jsonc"), it.Read(it.Path("pooled/wrangler.jsonc")))
	if err := os.Remove(it.Path("pooled/wrangler.jsonc")); err != nil {
		it.Fail("cannot remove the generated Wrangler checkout copy: %v", err)
	}
	it.Write(it.Path("pooled/BUILD.bazel"), `genrule(
    name = "wrangler",
    srcs = ["wrangler.source.jsonc"],
    outs = ["wrangler.jsonc"],
    cmd = "cp $(location wrangler.source.jsonc) $@",
)
`)
	it.Write(it.Path("BUILD.bazel"), it.Read(it.Path("BUILD.bazel"))+`
sh_binary(
    name = "json_gen",
    srcs = ["tools/json_gen.sh"],
    visibility = ["//visibility:public"],
)
`)
	// wrangler, for generated_worker's ts_codegen, is in tests/workers' lockfile.
	it.Write(it.Path("wrangler_lock/pnpm-lock.yaml"),
		it.Read(filepath.Join(it.RulesTSRoot, "tests/workers/pnpm-lock.yaml")))

	foreignFile, err := rule.LoadData("BUILD.bazel", "foreign_fixtures", []byte(`exports_files(["value.json", "a.ts", "nested/b.ts", "companion.mjs"], visibility = ["//foreign_json:__pkg__"])
`))
	if err != nil {
		it.Fail("cannot load foreign source exports: %v", err)
	}
	foreignExports = string(foreignFile.Format())
	it.Write(it.Path("foreign_fixtures/BUILD.bazel"), foreignExports)

	it.Install()
	it.Pass("pnpm install: the listing runs over the tree the build will check")

	gazelleLog, err = it.BazelLog("gazelle_pass_1", "run", "//:gazelle", "--", "-ts_verbose")
	if err != nil {
		gazelleLog.Dump()
		it.Fail("bazel run //:gazelle exited non-zero: %v", err)
	}
	it.Pass("gazelle pass 1 complete")
	return foreignExports, gazelleLog
}

func roundtrip(it *harness.IT) {
	_, gazelleLog := generate(it)

	for _, dir := range generated {
		it.RequireFile(it.Path(dir, "BUILD.bazel"), "Gazelle did not generate %s/BUILD.bazel", dir)
		it.Pass("%s/BUILD.bazel generated (pass 1)", dir)
	}
	afterFirstRun := map[string]string{}
	for _, dir := range handWritten {
		afterFirstRun[dir] = it.Read(it.Path(dir, "BUILD.bazel"))
	}
	staleBuildFileIsEmptiedAndNamed(it, gazelleLog)

	before := testTargets(it)
	if len(before) == 0 {
		it.Fail("no test targets in this workspace: the set comparison below would be vacuous")
	}
	it.Pass("test targets after pass 1: %d", len(before))

	firstPass := map[string]string{}
	for _, dir := range generated {
		firstPass[dir] = it.Read(it.Path(dir, "BUILD.bazel"))
		if err := os.Remove(it.Path(dir, "BUILD.bazel")); err != nil {
			it.Fail("cannot delete %s/BUILD.bazel: %v", dir, err)
		}
	}
	it.Pass("BUILD files snapshotted and deleted")

	it.MustBazel("run", "//:gazelle")
	it.Pass("gazelle pass 2 complete")

	differs := false
	for _, dir := range generated {
		second := it.Read(it.Path(dir, "BUILD.bazel"))
		if second != firstPass[dir] {
			fmt.Fprintf(os.Stderr, "--- %s/BUILD.bazel pass 1 ---\n%s--- pass 2 ---\n%s", dir, firstPass[dir], second)
			differs = true
			continue
		}
		it.Pass("%s/BUILD.bazel is identical across both Gazelle runs", dir)
	}
	for _, dir := range handWritten {
		second := it.Read(it.Path(dir, "BUILD.bazel"))
		if second != afterFirstRun[dir] {
			fmt.Fprintf(os.Stderr, "--- %s/BUILD.bazel pass 1 ---\n%s"+
				"--- pass 2 ---\n%s", dir, afterFirstRun[dir], second)
			differs = true
			continue
		}
		it.Pass("%s/BUILD.bazel, merged into, is identical across both Gazelle runs",
			filepath.Join(".", dir))
	}
	if differs {
		it.Fail("Gazelle output is not idempotent — see the dumps above")
	}

	it.MustBazel("test", "//pooled/test:test_test")
	it.Pass("ordinary generated Workers source closure runs in workerd")

	// Compare untouched generation first; the output assertions below require emission.
	emitFixturePrograms(it)
	// //pooled now publishes a rewritten scope, so Gazelle restages its test's config without the authored one.
	it.MustBazel("run", "//:gazelle", "--", "-r=false", "pooled/test")

	it.MustBazel("build", "//...")
	it.Pass("bazel build //...")

	after := testTargets(it)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		fmt.Fprintf(os.Stderr, "--- before ---\n%s\n--- after ---\n%s\n",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
		it.Fail("Gazelle changed the test target set: %d before, %d after", len(before), len(after))
	}
	it.Pass("test target set unchanged across a delete-and-regenerate: %d", len(after))

	it.MustBazel("test", "//...", "--output_groups=+declarations")
	it.Pass("bazel test //... with declarations after explicit fixture emission opt-ins")

	for _, rel := range []string{"src/lib/math.js", "src/lib/math.d.ts", "src/app/index.js", "src/app/index.d.ts"} {
		it.RequireFile(it.Bin(rel), "expected output file not found: %s", rel)
		it.Pass("output file exists: %s", rel)
	}

	theRootBaseIsATsConfig(it)
	declarationEntryResolvesThroughItsOwner(it)
	referenceTypesBecomeADep(it)
	jsxRuntimeBecomesADep(it)
	generatedDeclarationNamesItsGenerator(it)
	aliasedImportsAreDeps(it)
	theTreesFilesAreSrcs(it)
	outDirContentsAreNotSources(it)
	assetImportTypesThroughItsDeclaration(it)
	theTsconfigsExcludeIsTheExclusion(it)
	allowJsCompilesAndDeclares(it)
	checkedInDeclarationTypesTheJavaScript(it)
	programsAreListedWithTheToolchainsTsgo(it, gazelleLog)
	generatedDevServerBuilds(it)
	handWrittenRuleSharesTheProgram(it)
	memberSelfImportIsTheCompile(it)
	memberByNameIsTheHubView(it)
	parentDirectoryImportIsADep(it)
	packageRootVitestConfigReachesTheTestBelow(it)
	configBesideTheTestsIsNamed(it)
	workersPoolConfigReachesTheTest(it)
	importerScopedLabelsResolve(it)
	pairedTypesReachTheProgramThroughTheChain(it)
	declarationRetainsNpmRuntime(it)
	augmentationIsADep(it)
	foreignProjectGetsNothing(it, gazelleLog)
	declarationMovesToACodegen(it)
}

func borrowedChecks(it *harness.IT, _ string) {
	borrowedSourceKeepsItsNpmLookupDirectory(it)
}

func scopeChecks(it *harness.IT, _ string) {
	emittedLibraryRetainsAncestorScope(it)
}

func transitiveScopeChecks(it *harness.IT, _ string) {
	transitiveEmissionPublishesScopeAtomically(it)
}

func packageScopeChecks(it *harness.IT, _ string) {
	emittedPackageScopesFollowModules(it)
}

func foreignChecks(it *harness.IT, foreignExports string) {
	foreignSourcesKeepTheirOwners(it, foreignExports)
	samePackageOwnerSuppliesForeignJSON(it)
	generatedCompilerInputsUseDeclaredFiles(it)
	emittedConsumerKeepsSourceModeRuntimeFiles(it)
	sourceModeDirectRuntimeCannotLoseForeignModules(it)
	siblingOwnerRetainsSourcePackageScope(it)
}

func runtimeChecks(it *harness.IT, _ string) {
	companionErasureBoundary(it)
}

func configChecks(it *harness.IT, _ string) {
	generatedConfigStagesScalarRuntime(it)
	generatedRootConfigRetainsRuntimeOwner(it)
}

// Later checks run against the root publisher and binaries mixedSourceRootsDeclareAndRun leaves.
func rootsChecks(it *harness.IT, _ string) {
	mixedSourceRootsDeclareAndRun(it)
	borrowedHelpersDoNotBecomeTestEntries(it)
	importedHelpersCannotBeReplacedByData(it)
	runnerModuleAliasesPreserveIdentity(it)
	vitestConfigReferencesRetainRuntimeIdentity(it)
	compilerInputsCannotBecomeRuntimeLayout(it)
	nativeDescendantRetainsRuntimeInputs(it)
	workspaceMemberRejectsBorrowedSibling(it)
}

func foreignSourcesKeepTheirOwners(it *harness.IT, foreignExports string) {
	requireLabels(it, "srcs", "//foreign_json:foreign_json", []string{
		"//foreign_fixtures:a.ts", "//foreign_fixtures:companion.mjs", "//foreign_fixtures:nested/b.ts",
		"//foreign_fixtures:value.json", "//foreign_json:index.ts",
	})
	requireLabels(it, "package_scopes", "//foreign_json:foreign_json", []string{"//:package.json"})
	if it.Read(it.Path("foreign_fixtures/BUILD.bazel")) != foreignExports {
		it.Fail("Gazelle changed the source exports without a TypeScript target")
	}
	requireLabels(it, "srcs", "//foreign_consumer:foreign_consumer_test", []string{"//foreign_consumer:foreign.test.ts"})
	requireLabels(it, "deps", "//foreign_consumer:foreign_consumer_test", withRootManifest("//foreign_json:foreign_json"))
	requireLabels(it, "deps", "//foreign_json:foreign_json", []string{"//src/i18n:manifest"})
	it.MustBazel("test", "//foreign_consumer:foreign_consumer_test")
	func() {
		manifest := it.Path("foreign_fixtures/package.json")
		helper := it.Path("foreign_fixtures/helper.mjs")
		companion := it.Path("foreign_fixtures/companion.mjs")
		it.RequireNoFile(manifest, "the borrowed module's package scope must be staged by this check")
		it.RequireNoFile(helper, "the package-private module must be staged by this check")
		defer os.Remove(manifest)
		defer os.Remove(helper)
		defer it.Write(companion, it.Read(companion))
		defer it.Write(it.Path("foreign_fixtures/BUILD.bazel"), foreignExports)
		it.Write(manifest, `{"type":"module","imports":{"#helper":"./helper.mjs"}}`)
		it.Write(helper, "export const offset = 0;\n")
		it.Write(companion, "export { offset } from '#helper';\n")
		it.Write(it.Path("foreign_fixtures/BUILD.bazel"), foreignExports+"\nexports_files([\"package.json\", \"helper.mjs\"], visibility = [\"//foreign_json:__pkg__\"])\n")
		it.MustBazel("run", "//:gazelle", "--", "-r=false", "foreign_json")
		requireLabels(it, "srcs", "//foreign_json:foreign_json", []string{
			"//foreign_fixtures:a.ts", "//foreign_fixtures:companion.mjs", "//foreign_fixtures:helper.mjs",
			"//foreign_fixtures:nested/b.ts", "//foreign_fixtures:value.json", "//foreign_json:index.ts",
		})
		requireLabels(it, "package_scopes", "//foreign_json:foreign_json", []string{"//:package.json", "//foreign_fixtures:package.json"})
		it.MustBazel("test", "//foreign_consumer:foreign_consumer_test", "--output_groups=+_validation")
		it.Write(helper, "export const offset = 'wrong';\n")
		log, err := it.BazelLog("borrowed_package_import_type", "build", "//foreign_json:foreign_json", "--output_groups=+_validation")
		if err == nil || !log.Contains("TS2322") || !log.Contains("nested/b.ts") {
			log.Dump()
			it.Fail("the borrowed package's private import did not retain its compiler type")
		}
		it.Pass("borrowed runtime modules retain package-private imports for checking and execution")
	}()
	func() {
		const pkg = "borrowed_types"
		it.RequireNoDir(it.Path(pkg), "the declaration-only package must be staged by this check")
		defer os.RemoveAll(it.Path(pkg))
		consumer := it.Path("foreign_json/index.ts")
		defer it.Write(consumer, it.Read(consumer))
		it.Write(it.Path(pkg, "BUILD.bazel"), `exports_files(["api.d.ts", "internal.d.ts", "package.json"], visibility = ["//foreign_json:__pkg__"])
`)
		it.Write(it.Path(pkg, "package.json"), `{"type":"module","imports":{"#internal":"./internal.d.ts"}}`)
		it.Write(it.Path(pkg, "tsconfig.json"), `{"extends":"../tsconfig.json","files":["api.d.ts","internal.d.ts"]}`)
		it.Write(it.Path(pkg, "api.d.ts"), "export type { Value } from '#internal';\n")
		it.Write(it.Path(pkg, "internal.d.ts"), "export interface Value { answer: number }\n")
		it.Write(consumer, it.Read(consumer)+"\nimport type { Value } from '../borrowed_types/api.js';\nexport const typed: Value = { answer: 42 };\n")
		it.MustBazel("run", "//:gazelle", "--", "-r=false", "foreign_json")
		requireLabels(it, "srcs", "//foreign_json:foreign_json", []string{
			"//borrowed_types:api.d.ts", "//borrowed_types:internal.d.ts",
			"//foreign_fixtures:a.ts", "//foreign_fixtures:companion.mjs", "//foreign_fixtures:nested/b.ts",
			"//foreign_fixtures:value.json", "//foreign_json:index.ts",
		})
		requireLabels(it, "type_inputs", "//foreign_json:foreign_json", []string{"//borrowed_types:package.json"})
		it.MustBazel("test", "//foreign_consumer:foreign_consumer_test", "--@rules_typescript//ts:lib_check", "--output_groups=+_validation")
		it.Write(it.Path(pkg, "internal.d.ts"), "export interface Value { answer: string }\n")
		log, err := it.BazelLog("borrowed_declaration_package_type", "build", "//foreign_json:foreign_json", "--@rules_typescript//ts:lib_check", "--output_groups=+_validation")
		if err == nil || !log.Contains("TS2322") || !log.Contains("index.ts") {
			log.Dump()
			it.Fail("the borrowed declaration's package-private type did not constrain its consumer")
		}
		it.Pass("borrowed declarations retain package-private type imports with library checking enabled")
	}()
	it.MustBazel("run", "//:gazelle", "--", "-r=false", "foreign_json")
	func() {
		defer it.Write(it.Path("foreign_fixtures/BUILD.bazel"), foreignExports)
		it.Write(it.Path("foreign_fixtures/BUILD.bazel"), strings.Replace(foreignExports, "//foreign_json:__pkg__", "//visibility:private", 1))
		log, err := it.BazelLog("foreign_json_visibility", "build", "//foreign_json:foreign_json")
		if err == nil || !log.Contains("not visible") {
			log.Dump()
			it.Fail("foreign source import bypassed its owner's visibility")
		}
	}()
}

func borrowedSourceKeepsItsNpmLookupDirectory(it *harness.IT) {
	const base = "borrowed_npm_importer"
	it.RequireNoDir(it.Path(base), "the borrowed npm importer fixture must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	for _, file := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "BUILD.bazel", "tsconfig.json"} {
		defer it.Write(it.Path(file), it.Read(it.Path(file)))
	}
	it.Write(it.Path("package.json"), `{"private":true}`)
	it.Write(it.Path(base, "root.ts"), "export const root = 1;\n")
	it.Write(it.Path("tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["`+base+`/root.ts"]}`)
	it.Write(it.Path("pnpm-workspace.yaml"), it.Read(it.Path("pnpm-workspace.yaml"))+"  - "+base+"/*\n")
	manifest := `{"private":true,"dependencies":{"culori":"4.0.2","@types/culori":"2.1.1","source-identity":"workspace:*"}}`
	it.Write(it.Path(base, "app/package.json"), `{"private":true}`)
	it.Write(it.Path(base, "shared/package.json"), manifest)
	it.Write(it.Path(base, "member/package.json"), `{"name":"source-identity","version":"0.0.0","type":"module","exports":"./index.ts"}`)
	it.Write(it.Path(base, "member/index.ts"), "export const value: number = 42;\n")
	config := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","noImplicitAny":true,"types":[]},"files":["index.ts"]}`
	it.Write(it.Path(base, "member/tsconfig.json"), config)
	it.Write(it.Path(base, "app/tsconfig.json"), config)
	it.Write(it.Path(base, "app/index.ts"), "import { hex, marker } from '../shared/helper';\nexport const colour: string | undefined = hex;\nexport const identity: number = marker;\n")
	it.Write(it.Path(base, "shared/helper.ts"), "import { formatHex } from 'culori';\nimport { value } from 'source-identity';\nexport const hex = formatHex('red');\nexport const marker = value;\n")
	it.Write(it.Path(base, "shared/BUILD.bazel"), `exports_files(["helper.ts", "package.json"], visibility = ["//visibility:public"])
`)
	install := func() {
		it.MustBazel("run", "//:pnpm", "--", "install", "--lockfile-only", "--no-frozen-lockfile")
		it.Install()
		it.RequireNoFile(it.Path("node_modules/culori"), "a root package link would conceal the missing borrowed importer")
		it.RequireNoFile(it.Path("node_modules/@types/culori"), "a root types link would conceal the missing borrowed importer")
	}
	install()
	it.RequireNoFile(it.Path(base, "app/node_modules/culori"), "the package must resolve only through the borrowed importer")
	it.RequireNoFile(it.Path(base, "app/node_modules/@types/culori"), "the declaration companion must resolve only through the borrowed importer")
	target := "//" + base + "/app:app"
	for _, dirs := range [][]string{{".", base + "/app", base + "/shared", base + "/member"}, {base + "/app"}} {
		it.MustBazel(append([]string{"run", "//:gazelle", "--", "-index=false", "-r=false"}, dirs...)...)
		requireLabels(it, "source_node_modules", target, []string{"//" + base + "/shared:node_modules"})
		it.MustBazel("build", target, "--output_groups=+_validation")
	}
	if owners := strings.TrimSpace(it.BazelStdout("query", `kind("ts_compile rule", //`+base+`/shared:*)`)); owners != "" {
		it.Fail("a compiler owner concealed the borrowed importer regression: %s", owners)
	}
	input := it.Path(base, "app/index.ts")
	correct := it.Read(input)
	for _, probe := range []struct{ name, declaration, diagnostic string }{
		{"member", "export const wrong: boolean = marker;\n", "not assignable to type 'boolean'"},
		{"companion", "export const wrongColour: number = hex;\n", "not assignable to type 'number'"},
	} {
		it.Write(input, correct+probe.declaration)
		log, err := it.BazelLog("borrowed_npm_"+probe.name+"_type", "build", target, "--output_groups=+_validation")
		if err == nil || !log.Contains(probe.diagnostic) {
			log.Dump()
			it.Fail("the borrowed %s did not retain its compiler type", probe.name)
		}
	}
	it.Write(input, correct)
	func() {
		build := it.Path(base, "app/BUILD.bazel")
		exports := it.Path(base, "shared/BUILD.bazel")
		defer it.Write(build, it.Read(build))
		defer it.Write(exports, it.Read(exports))
		entry := it.Path(base, "shared/entry.mjs")
		declaration := it.Path(base, "shared/entry.d.mts")
		defer os.Remove(entry)
		defer os.Remove(declaration)
		it.Write(entry, `import * as colour from 'culori';
import { realpathSync } from 'node:fs';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
const url = import.meta.resolve('culori');
if (fileURLToPath(url) !== realpathSync(fileURLToPath(url)) || colour !== await import(url)) {
  throw new Error('ESM package lost its store identity');
}
const require = createRequire(import.meta.url);
const resolved = require.resolve('culori');
const commonJS = require('culori');
if (resolved !== realpathSync(resolved) || require(resolved) !== commonJS || require.cache[resolved]?.exports !== commonJS) {
  throw new Error('CommonJS package lost its store or cache identity');
}
export const hex = colour.formatHex('red');
if (hex !== '#ff0000') throw new Error('checkout package replaced declared dependency: ' + hex);
console.log('borrowed native entry: ' + hex);
`)
		it.Write(declaration, "export declare const hex: string | undefined;\n")
		it.Write(exports, it.Read(exports)+`
exports_files(["entry.mjs", "entry.d.mts"], visibility = ["//visibility:public"])
`)
		nativeBuild, err := rule.LoadFile(build, base+"/app")
		if err != nil {
			it.Fail("cannot load the native binary's package: %v", err)
		}
		for _, load := range nativeBuild.Loads {
			if load.Name() == "@rules_typescript//ts:defs.bzl" {
				load.Add("ts_binary")
			}
		}
		chain := labels(it, "node_modules", target)
		if len(chain) != 1 {
			it.Fail("the borrowed source consumer has no single npm importer: %v", chain)
		}
		it.Write(build, string(nativeBuild.Format())+fmt.Sprintf(`
ts_compile(
    name = "borrowed_native",
    srcs = [%q, %q],
    package_scopes = [%q],
    emit = False,
    node_modules = %q,
    source_node_modules = [%q],
    deps = [%q],
)
ts_binary(name = "native_run", entry_point = ":borrowed_native")
`, "//"+base+"/shared:entry.mjs", "//"+base+"/shared:entry.d.mts", "//"+base+"/shared:package.json", chain[0], "//"+base+"/shared:node_modules", "@npm//"+base+"/shared:culori"))
		native := "//" + base + "/app:native_run"
		it.MustBazel("build", native, "--output_groups=+_validation")
		var config struct {
			Node struct {
				Entry string `json:"entry"`
			} `json:"node"`
		}
		if err := json.Unmarshal([]byte(it.Read(it.Bin(base, "app/native_run_launcher.json"))), &config); err != nil {
			it.Fail("cannot read the native binary's launcher config: %v", err)
		}
		wantEntry := "_main/" + base + "/shared/entry.mjs"
		if config.Node.Entry != wantEntry {
			it.Fail("borrowed native entry lost its authored runfiles path: %+v", config)
		}
		checkout := it.Path(base, "shared/node_modules")
		saved := it.Path(base, "shared/node_modules.saved")
		it.RequireNoFile(saved, "the installed importer backup must be absent")
		if err := os.Rename(checkout, saved); err != nil {
			it.Fail("cannot isolate the checkout importer: %v", err)
		}
		defer func() {
			if err := os.RemoveAll(checkout); err != nil {
				it.Fail("cannot remove the poisoned checkout importer: %v", err)
			}
			if err := os.Rename(saved, checkout); err != nil {
				it.Fail("cannot restore the checkout importer: %v", err)
			}
		}()
		for _, state := range []string{"absent", "poisoned"} {
			if state == "poisoned" {
				it.Write(filepath.Join(checkout, "culori/package.json"), `{"name":"culori","type":"module","exports":"./index.mjs"}`)
				it.Write(filepath.Join(checkout, "culori/index.mjs"), "throw new Error('poisoned checkout culori executed');\n")
			}
			it.RequireNoFile(it.Path("node_modules/culori"), "a root public link would conceal the borrowed importer")
			it.RequireNoFile(it.Path(base, "app/node_modules/culori"), "a consumer link would conceal the borrowed importer")
			for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
				log, err := it.BazelLog("borrowed_native_"+state+runfiles, "run", native, runfiles)
				if err != nil || !log.Contains("borrowed native entry: #ff0000") || log.Contains("poisoned checkout culori executed") {
					log.Dump()
					it.Fail("%s checkout under %s changed the declared borrowed npm dependency", state, runfiles)
				}
			}
		}
	}()
	func() {
		appBuild := it.Path(base, "app/BUILD.bazel")
		sharedBuild := it.Path(base, "shared/BUILD.bazel")
		helper := it.Path(base, "shared/helper.ts")
		for _, file := range []string{input, helper, appBuild, sharedBuild} {
			defer it.Write(file, it.Read(file))
		}
		ownerConfig := it.Path(base, "shared/tsconfig.json")
		native := it.Path(base, "shared/native.ts")
		it.RequireNoFile(ownerConfig, "the source owner config must be introduced by this check")
		it.RequireNoFile(native, "the owner-only implementation must be introduced by this check")
		defer os.Remove(ownerConfig)
		defer os.Remove(native)
		it.Write(ownerConfig, `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","noImplicitAny":true,"types":[],"paths":{"culori":["./native.ts"]}},"files":["helper.ts","native.ts"]}`)
		it.Write(native, "export const formatHex = (_colour: string): number => 42;\n")
		it.Write(helper, "export { formatHex } from 'culori';\n")
		consumer := "import { formatHex } from '../shared/helper';\nexport const colour: string | undefined = formatHex('red');\n"
		it.Write(input, consumer)
		owner := "//" + base + "/shared:shared"
		var first string
		for _, dirs := range [][]string{{base + "/app", base + "/shared"}, {base + "/app"}, {base + "/app", base + "/shared"}} {
			it.MustBazel(append([]string{"run", "//:gazelle", "--", "-index=false", "-r=false"}, dirs...)...)
			requireLabels(it, "deps", owner, nil)
			requireLabels(it, "deps", target, []string{owner, "@npm//" + base + "/shared:culori"})
			requireLabels(it, "srcs", target, []string{"//" + base + "/app:index.ts"})
			requireLabels(it, "source_node_modules", target, []string{"//" + base + "/shared:node_modules"})
			generated := it.Read(appBuild) + it.Read(sharedBuild)
			if first != "" && generated != first {
				it.Fail("the consumer-only npm importer did not converge across full and partial updates")
			}
			first = generated
			it.MustBazel("build", target, "--output_groups=+_validation")
		}
		it.Write(input, consumer+"export const wrongColour: number = formatHex('red');\n")
		log, err := it.BazelLog("source_owner_consumer_npm_type", "build", target, "--output_groups=+_validation")
		if err == nil || !log.Contains("not assignable to type 'number'") {
			log.Dump()
			it.Fail("the source owner's implementation mapping replaced the consumer's npm declaration")
		}
		it.Pass("source ownership preserves the consumer's distinct npm lookup and declaration companion")
	}()
	nativeNpmContext := func(version, supplyingPackage string, additionalCases bool) {
		context := base + "/native_context"
		it.RequireNoDir(it.Path(context), "the native importer context must be staged by this check")
		defer os.RemoveAll(it.Path(context))
		sharedBuild := it.Path(base, "shared/BUILD.bazel")
		defer it.Write(sharedBuild, it.Read(sharedBuild))
		sharedManifest := it.Path(base, "shared/package.json")
		defer it.Write(sharedManifest, it.Read(sharedManifest))
		it.Write(sharedManifest, strings.Replace(it.Read(sharedManifest), `"private":true`, `"private":true,"type":"module"`, 1))
		for _, name := range []string{"native-entry.ts", "native-entry.js.imports.json", "native.tsconfig.json"} {
			it.RequireNoFile(it.Path(base, "shared", name), "the emitted native entry must be staged by this check")
			defer os.Remove(it.Path(base, "shared", name))
		}
		const assetContents = "{\"marker\":\"authored sibling data\"}\n"
		it.Write(it.Path(base, "shared/native-entry.js.imports.json"), assetContents)
		assetCheck := `declare const process: { argv: string[]; getBuiltinModule(name: 'fs'): { readFileSync(path: string, encoding: 'utf8'): string } };
if (process.getBuiltinModule('fs').readFileSync(process.argv[1] + '.imports.json', 'utf8') !== ` + strconv.Quote(assetContents) + `) throw new Error('authored sibling JSON was replaced');
console.log('authored sibling JSON unchanged');
`
		valueEntry := assetCheck + `import { formatHex } from 'culori';
declare global { interface ImportMeta { resolve(specifier: string): string; } }
const resolved = import.meta.resolve('culori');
if (formatHex('red') !== '#ff0000' || !resolved.includes('/culori@` + version + `/')) throw new Error('emitted entry selected another npm store: ' + resolved);
console.log('emitted native store: ` + version + `');
`
		it.Write(it.Path(base, "shared/native-entry.ts"), valueEntry)
		it.Write(it.Path(base, "shared/native.tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","target":"es2022","types":[]},"files":["native-entry.ts"]}`)
		shared, err := rule.LoadFile(sharedBuild, base+"/shared")
		if err != nil {
			it.Fail("cannot load the emitted entry's source package: %v", err)
		}
		var definitions *rule.Load
		for _, load := range shared.Loads {
			if load.Name() == "@rules_typescript//ts:defs.bzl" {
				definitions = load
			}
		}
		if definitions == nil {
			definitions = rule.NewLoad("@rules_typescript//ts:defs.bzl")
			definitions.Insert(shared, 0)
		}
		definitions.Add("ts_binary")
		definitions.Add("ts_compile")
		it.Write(sharedBuild, string(shared.Format())+`
exports_files(["native-entry.ts", "native-entry.js.imports.json"], visibility = ["//visibility:public"])
ts_compile(name = "native_assets", data = ["native-entry.js.imports.json"], emit = False, visibility = ["//visibility:public"])
ts_compile(name = "native_entry", srcs = ["native-entry.ts"], package_scopes = ["package.json"], emit = True, tsconfig = "native.tsconfig.json", node_modules = ":node_modules", deps = [":native_assets", "@npm//`+base+`/shared:culori"])
ts_binary(name = "native_run", entry_point = ":native_entry")
`)
		it.Write(it.Path(context, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","target":"es2022","types":[]},"files":["../shared/native-entry.ts"]}`)
		contextBuild := `load("@rules_typescript//npm:defs.bzl", "node_modules")
load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
node_modules(name = "node_modules", deps = ["@npm//` + base + `/` + supplyingPackage + `:culori", "@npm//` + base + `/shared:types_culori"], parent = "//:node_modules")
ts_compile(name = "borrowed", srcs = ["//` + base + `/shared:native-entry.ts"], package_scopes = ["//` + base + `/shared:package.json"], emit = True, tsconfig = "tsconfig.json", node_modules = ":node_modules", source_node_modules = ["//` + base + `/shared:node_modules"], deps = ["//` + base + `/shared:native_assets", "@npm//` + base + `/shared:culori"])
ts_binary(name = "run", entry_point = ":borrowed")
`
		it.Write(it.Path(context, "BUILD.bazel"), contextBuild)
		log, err := it.BazelLog("emitted_native_npm_"+supplyingPackage, "run", "//"+context+":run")
		if err != nil || !log.Contains("authored sibling JSON unchanged") || !log.Contains("emitted native store: "+version) {
			log.Dump()
			it.Fail("the emitted native entry did not execute its source npm store")
		}
		if additionalCases {
			func() {
				entry := it.Path(base, "shared/native-entry.ts")
				build := it.Path(context, "BUILD.bazel")
				config := it.Path(context, "tsconfig.json")
				for _, file := range []string{entry, build, config, sharedBuild, sharedManifest} {
					defer it.Write(file, it.Read(file))
				}
				scope := it.Read(sharedManifest)
				exports := it.Read(sharedBuild)
				compilerConfig := it.Read(config)
				nested := it.Path(base, "shared/nested")
				it.RequireNoDir(nested, "the nested native importer must be staged by this check")
				defer os.RemoveAll(nested)
				lookupSource := `declare const process: {
  argv: string[];
  getBuiltinModule(name: 'fs'): { readFileSync(path: string, encoding: 'utf8'): string; realpathSync(path: string): string };
  getBuiltinModule(name: 'url'): { fileURLToPath(url: string): string };
};
declare global { interface ImportMeta { resolve(specifier: string): string; } }
`
				lookupCheck := `const resolvedPath = process.getBuiltinModule('url').fileURLToPath(resolved);
if (!resolved.includes('/culori@` + version + `/') || process.getBuiltinModule('fs').realpathSync(resolvedPath) !== resolvedPath) {
  throw new Error('native lookup lost its canonical source store: ' + resolved);
}
`
				it.Write(sharedManifest, strings.Replace(scope, `"type":"module"`, `"type":"module","imports":{"#dep":"culori"}`, 1))
				it.Write(sharedBuild, exports+"\nexports_files([\"nested/alias.ts\"], visibility = [\"//visibility:public\"])\n")
				it.Write(filepath.Join(nested, "alias.ts"), lookupSource+`import * as colour from '#dep';
const resolved = import.meta.resolve('#dep');
`+lookupCheck+`if (colour.formatHex('red') !== '#ff0000' || colour !== await import(resolved)) throw new Error('package import executed different source bytes');
console.log('native package import store: `+version+`');
`)
				it.Write(config, `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","target":"es2022","types":[]},"files":["../shared/nested/alias.ts"]}`)
				it.Write(build, strings.Replace(contextBuild, `srcs = ["//`+base+`/shared:native-entry.ts"]`, `srcs = ["//`+base+`/shared:nested/alias.ts"]`, 1))
				log, err := it.BazelLog("native_package_import_scope", "run", "//"+context+":run")
				if err != nil || !log.Contains("native package import store: "+version) {
					log.Dump()
					it.Fail("a nested package import did not execute the source scope's npm store")
				}
				it.Write(sharedManifest, scope)
				it.Write(sharedBuild, exports)
				it.Write(build, contextBuild)
				it.Write(config, compilerConfig)
				it.Write(entry, lookupSource+`import type { formatHex } from 'culori';
const colour: ReturnType<typeof formatHex> = '#ff0000';
const resolved = import.meta.resolve('culori');
`+lookupCheck+`if (process.getBuiltinModule('fs').readFileSync(process.argv[1] + '.imports.json', 'utf8') !== `+strconv.Quote(assetContents)+`) throw new Error('authored sibling JSON was replaced');
console.log('authored sibling JSON unchanged');
console.log('native resolve-only store: `+version+` ' + colour);
`)
				log, err = it.BazelLog("native_resolve_only_npm", "run", "//"+context+":run")
				if err != nil || !log.Contains("authored sibling JSON unchanged") || !log.Contains("native resolve-only store: "+version+" #ff0000") {
					log.Dump()
					it.Fail("a resolve-only emitted entry did not retain its canonical source npm store")
				}
			}()
			log, err = it.BazelLog("source_owned_native_npm", "run", "//"+base+"/shared:native_run")
			if err != nil || !log.Contains("authored sibling JSON unchanged") || !log.Contains("emitted native store: "+version) {
				log.Dump()
				it.Fail("source-owned emission did not execute its npm store")
			}
			func() {
				entry := it.Path(base, "shared/native-entry.ts")
				build := it.Path(context, "BUILD.bazel")
				for _, file := range []string{entry, build, sharedBuild} {
					defer it.Write(file, it.Read(file))
				}
				factory := it.Path(base, "shared/factory")
				it.RequireNoDir(factory, "the re-exported require factory must be staged by this check")
				defer os.RemoveAll(factory)
				it.Write(filepath.Join(factory, "package.json"), `{"type":"module"}`)
				it.Write(filepath.Join(factory, "require.ts"), `declare const process: {
  getBuiltinModule(name: 'module'): {
    createRequire(url: string): {
      (specifier: string): { formatHex(value: string): string | undefined };
      resolve(specifier: string): string;
    };
  };
};
const createRequire = process.getBuiltinModule('module').createRequire;
export { createRequire as makeRequire };
`)
				it.Write(sharedBuild, it.Read(sharedBuild)+"\nexports_files([\"factory/require.ts\", \"factory/package.json\"], visibility = [\"//visibility:public\"])\n")
				callbackBuild := strings.Replace(contextBuild, `srcs = ["//`+base+`/shared:native-entry.ts"]`, `srcs = ["//`+base+`/shared:native-entry.ts", "//`+base+`/shared:factory/require.ts"]`, 1)
				callbackBuild = strings.Replace(callbackBuild, `package_scopes = ["//`+base+`/shared:package.json"]`, `package_scopes = ["//`+base+`/shared:package.json", "//`+base+`/shared:factory/package.json"]`, 1)
				callbackBuild = strings.Replace(callbackBuild, `entry_point = ":borrowed")`, `entry_point = ":borrowed", entry_file = "native-entry.ts")`, 1)
				it.Write(build, callbackBuild)
				it.Write(entry, assetCheck+`import type { formatHex } from 'culori';
import { makeRequire } from './factory/require.js';
declare global { interface ImportMeta { url: string; } }
const load = makeRequire(import.meta.url);
const resolved = load.resolve('culori');
const colourModule = load('culori');
const colour: ReturnType<typeof formatHex> = colourModule.formatHex('red');
if (colour !== '#ff0000' || !resolved.includes('/culori@`+version+`/') || colourModule !== load(resolved)) {
  throw new Error('re-exported require factory selected another npm store: ' + resolved);
}
console.log('native callback store: `+version+` ' + colour);
`)
				log, err := it.BazelLog("native_callback_npm", "run", "//"+context+":run")
				if err != nil || !log.Contains("authored sibling JSON unchanged") || !log.Contains("native callback store: "+version+" #ff0000") {
					log.Dump()
					it.Fail("a re-exported require factory lost its caller's source npm store")
				}
			}()
			occupied := "shared/node_modules/culori"
			it.Write(it.Path(context, occupied), "occupied npm destination\n")
			it.Write(it.Path(context, "BUILD.bazel"), contextBuild+`
ts_binary(name = "conflicting_run", entry_point = ":borrowed", data = ["`+occupied+`"])
`)
			for _, format := range []string{"esm", "cjs"} {
				if format == "cjs" {
					it.Write(sharedManifest, strings.Replace(it.Read(sharedManifest), `"type":"module"`, `"type":"commonjs"`, 1))
					it.Write(it.Path(context, "tsconfig.json"), `{"compilerOptions":{"module":"node16","moduleResolution":"node16","target":"es2022","types":[]},"files":["../shared/native-entry.ts"]}`)
					// A node16 program declares its module on a ts_config, as Gazelle writes one.
					contextFile := it.Path(context, "BUILD.bazel")
					declared := strings.Replace(it.Read(contextFile), `"ts_binary", "ts_compile")`, `"ts_binary", "ts_compile", "ts_config")`, 1)
					it.Write(contextFile, strings.ReplaceAll(declared, `tsconfig = "tsconfig.json"`, `tsconfig = ":tsconfig"`)+`
ts_config(name = "tsconfig", src = "tsconfig.json", module = "node16")
`)
					valueEntry = assetCheck + `import { formatHex } from 'culori';
declare const require: { resolve(specifier: string): string };
const resolved = require.resolve('culori');
if (formatHex('red') !== '#ff0000' || !resolved.includes('/culori@` + version + `/')) throw new Error('emitted entry selected another npm store: ' + resolved);
console.log('emitted native store: ` + version + `');
`
				}
				it.Write(it.Path(base, "shared/native-entry.ts"), valueEntry)
				if format == "cjs" {
					log, err = it.BazelLog("emitted_native_npm_cjs", "run", "//"+context+":run")
					if err != nil || !log.Contains("authored sibling JSON unchanged") || !log.Contains("emitted native store: "+version) {
						log.Dump()
						it.Fail("tsgo's emitted native entry did not execute its source npm store")
					}
					// TypeScript 7 rejects sloppy-mode wrapper arguments; the arrow reaches the same wrapper through module.
					it.Write(it.Path(base, "shared/native-entry.ts"), `declare const module: {
  require(specifier: 'culori'): { formatHex(value: string): string | undefined };
  filename: string;
  constructor: { createRequire(path: string): { resolve(specifier: 'culori'): string } };
};
const wrapper = (() => module)();
const resolved = wrapper.constructor.createRequire(wrapper.filename).resolve('culori');
if (wrapper.require('culori').formatHex('red') !== '#ff0000' || !resolved.includes('/culori@`+version+`/')) throw new Error('wrapper module selected another npm store: ' + resolved);
console.log('native wrapper module store: `+version+`');
`)
					log, err = it.BazelLog("native_wrapper_module_npm", "run", "//"+context+":run")
					if err != nil || !log.Contains("native wrapper module store: "+version) {
						log.Dump()
						it.Fail("an emitted CommonJS arrow did not retain its wrapper's source npm store")
					}
					it.Write(it.Path(base, "shared/native-entry.ts"), valueEntry)
				}
				log, err = it.BazelLog("native_npm_projection_conflict_"+format, "run", "//"+context+":conflicting_run")
				if err == nil || !log.Contains("projection destination ") || !log.Contains(context+"/"+occupied) || !log.Contains(`source "`+base+`/shared/native-entry.ts"`) || !log.Contains("runtime entry ") || log.Contains("authored sibling JSON unchanged") || log.Contains("emitted native store:") {
					log.Dump()
					it.Fail("native %s npm context placement replaced an occupied destination", format)
				}
				if got := it.Read(it.Path(base, "shared/native-entry.js.imports.json")); got != assetContents {
					it.Fail("native %s npm context placement changed authored sibling JSON: %q", format, got)
				}
				if got := it.Read(it.Path(context, occupied)); got != "occupied npm destination\n" {
					it.Fail("native %s npm context placement changed ordinary source data: %q", format, got)
				}
			}
		}
	}
	nativeNpmContext("4.0.2", "shared", false)
	appManifest := strings.Replace(manifest, `"dependencies":{`, `"dependencies":{"vitest":"4.1.11","@types/node":"22.20.1",`, 1)
	it.Write(it.Path(base, "app/package.json"), appManifest)
	it.Write(it.Path(base, "shared/package.json"), strings.Replace(manifest, "4.0.2", "3.3.0", 1))
	install()
	it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", ".", base+"/app", base+"/shared")
	it.MustBazel("build", target, "--output_groups=+_validation")
	nativeNpmContext("3.3.0", "app", true)
	requireLabels(it, "source_node_modules", target, []string{"//" + base + "/shared:node_modules"})
	if slices.Contains(labels(it, "deps", target), "@npm//"+base+"/app:culori") {
		it.Fail("an unused consumer package became a direct compiler dependency")
	}
	// aquery's inputs() must match the whole exec path.
	query := fmt.Sprintf(`inputs(".*/%s/app/node_modules/culori", mnemonic("TsgoCheck", %s))`, base, target)
	var shadowInputs struct {
		Actions []json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal([]byte(it.BazelStdout("aquery", query, "--output=jsonproto")), &shadowInputs); err != nil {
		it.Fail("cannot inspect the consumer's shadowing npm input: %v", err)
	}
	if len(shadowInputs.Actions) != 1 {
		it.Fail("the consumer's nearer package link was omitted from its compiler inputs")
	}
	it.Write(it.Path(base, "app/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","noImplicitAny":true,"types":["node"]},"include":["*.ts"]}`)
	it.Write(it.Path(base, "shared/helper.ts"), `import * as colour from 'culori';
import { createRequire } from 'node:module';
import { value } from 'source-identity';
export const hex = colour.formatHex('red');
export const marker = value;
export const borrowedColour = colour;
export const borrowedResolution = createRequire(import.meta.url).resolve('culori');
`)
	it.Write(input, `import * as localColour from 'culori';
import { createRequire } from 'node:module';
import { hex, marker, borrowedColour, borrowedResolution } from '../shared/helper';
export const colour: string | undefined = hex;
export const identity: number = marker;
export { borrowedColour, borrowedResolution, localColour };
export const localResolution = createRequire(import.meta.url).resolve('culori');
`)
	it.Write(it.Path(base, "app/importers.test.ts"), `import { expect, test } from 'vitest';
import { colour, identity, borrowedColour, borrowedResolution, localColour, localResolution } from './index';
test("borrowed and local imports retain different npm stores", () => {
  expect(colour).toBe('#ff0000');
  expect(identity).toBe(42);
  expect(localColour.formatHex('blue')).toBe('#0000ff');
  expect(borrowedColour).not.toBe(localColour);
  expect(borrowedResolution).toContain('/culori@3.3.0/');
  expect(localResolution).toContain('/culori@4.0.2/');
});
`)
	it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", ".", base+"/app", base+"/shared")
	it.MustBazel("test", testTarget(base+"/app"), "--output_groups=+_validation")
	func() {
		for _, file := range []string{base + "/app/BUILD.bazel", base + "/app/index.ts", base + "/app/importers.test.ts", base + "/shared/helper.ts", base + "/shared/BUILD.bazel"} {
			defer it.Write(it.Path(file), it.Read(it.Path(file)))
		}
		owner := base + "/vitest_owner"
		it.RequireNoDir(it.Path(owner), "the moved Vitest producer must be staged by this check")
		defer os.RemoveAll(it.Path(owner))
		it.Write(it.Path(owner, "anchor.ts"), "export { anchor } from '../shared/placement';\n")
		placement := it.Path(base, "shared/placement.ts")
		it.RequireNoFile(placement, "the mixed emitted placement must be staged by this check")
		defer os.Remove(placement)
		it.Write(placement, "export const anchor: number = 42;\n")
		sharedBuild, err := rule.LoadFile(it.Path(base, "shared/BUILD.bazel"), base+"/shared")
		if err != nil {
			it.Fail("cannot load the source npm importer: %v", err)
		}
		sourceImporterConfigured := false
		for _, target := range sharedBuild.Rules {
			if target.Kind() == "node_modules" && target.Name() == "node_modules" {
				target.SetAttr("deps", append(target.AttrStrings("deps"), "@npm//"+base+"/app:vitest"))
				sourceImporterConfigured = true
			}
		}
		if !sourceImporterConfigured {
			it.Fail("the borrowed helper's generated npm importer is missing")
		}
		it.Write(it.Path(base, "shared/BUILD.bazel"), string(sharedBuild.Format())+"\nexports_files([\"placement.ts\"], visibility = [\"//visibility:public\"])\n")
		// The library's types entry resolves on its own importer, the nearest at or above its package.
		it.Write(it.Path(owner, "package.json"), `{"private":true,"devDependencies":{"@types/node":"22.20.1"}}`)
		install()
		it.Write(it.Path(owner, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":["node"]}}`)
		it.Write(it.Path(owner, "tsconfig.commonjs.json"), `{"compilerOptions":{"module":"commonjs","types":["node"]}}`)
		buildPath := it.Path(base, "app/BUILD.bazel")
		build, err := rule.LoadFile(buildPath, base+"/app")
		if err != nil {
			it.Fail("cannot load the moved Vitest importer: %v", err)
		}
		var compiler, test *rule.Rule
		for _, target := range build.Rules {
			if target.Kind() == "ts_compile" && target.Name() == "app" {
				compiler = target
			}
			if target.Kind() == "ts_test" {
				test = target
			}
		}
		if compiler == nil || test == nil || !slices.Contains(compiler.AttrStrings("srcs"), "//"+base+"/shared:helper.ts") {
			it.Fail("the generated app does not own its borrowed helper and test")
		}
		// The helper's culori comes from the shared importer, as Gazelle spells it for the library.
		ownerDeps := []string{"@npm//" + base + "/app:vitest", ":placement"}
		for _, dep := range compiler.AttrStrings("deps") {
			if dep == "@npm//"+base+"/app:culori" {
				dep = "@npm//" + base + "/shared:culori"
			}
			if !slices.Contains(ownerDeps, dep) {
				ownerDeps = append(ownerDeps, dep)
			}
		}
		it.Write(it.Path(owner, "BUILD.bazel"), fmt.Sprintf(`load("@rules_typescript//npm:defs.bzl", "node_modules")
load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_config")
node_modules(name = "node_modules", parent = "//:node_modules", deps = ["@npm//%s:types_node"])
ts_config(name = "commonjs_config", src = "tsconfig.commonjs.json", module = "commonjs")
ts_compile(name = "placement", srcs = ["anchor.ts", "//%s/shared:placement.ts"], emit = True)
ts_compile(
    name = "library",
    srcs = ["//%s/shared:helper.ts"],
    package_scopes = ["//%s/shared:package.json"],
    emit = True,
    tsconfig = "tsconfig.json",
    node_modules = ":node_modules",
    source_node_modules = ["//%s/shared:node_modules"],
    deps = [],
    visibility = ["//visibility:public"],
)
`, owner, base, base, base, base))
		ownerBuild, err := rule.LoadFile(it.Path(owner, "BUILD.bazel"), owner)
		if err != nil {
			it.Fail("cannot load the moved Vitest producer: %v", err)
		}
		var library *rule.Rule
		for _, target := range ownerBuild.Rules {
			if target.Kind() == "ts_compile" && target.Name() == "library" {
				library = target
				library.SetAttr("deps", ownerDeps)
			}
		}
		compiler.SetAttr("srcs", []string{"index.ts"})
		compiler.SetAttr("deps", append(compiler.AttrStrings("deps"), "//"+owner+":library"))
		test.SetAttr("deps", append(test.AttrStrings("deps"), "@npm//"+base+"/app:culori", "//"+owner+":library"))
		it.Write(it.Path(base, "shared/helper.ts"), `import * as colour from 'culori';
import { expect } from 'vitest';
import { value } from 'source-identity';
import { anchor } from '../vitest_owner/anchor';
expect(anchor).toBe(42);
expect(colour.formatHex('red')).toBe('#ff0000');
export const hex = colour.formatHex('red');
export const marker = value;
export const borrowedColour = colour;
export const helperIdentity: object = {};
`)
		it.Write(it.Path(base, "app/index.ts"), `import * as localColour from 'culori';
import { createRequire } from 'node:module';
import { hex, marker, borrowedColour, helperIdentity } from '../shared/helper';
export const colour = hex;
export const identity = marker;
export { borrowedColour, helperIdentity, localColour };
export const localResolution = createRequire(import.meta.url).resolve('culori');
export const localURL = import.meta.url;
`)
		it.Write(it.Path(base, "app/importers.test.ts"), `import { it, expect } from 'vitest';
import * as testColour from 'culori';
import { createRequire } from 'node:module';
import { realpathSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { colour, identity, borrowedColour, helperIdentity, localColour, localResolution, localURL } from './index';
import { borrowedColour as aliasColour, helperIdentity as aliasIdentity } from '../shared/helper';
it('moved imports retain source and test npm stores at one helper identity', async () => {
  expect(colour).toBe('#ff0000');
  expect(identity).toBe(42);
  expect(localColour.formatHex('blue')).toBe('#0000ff');
  expect(borrowedColour).not.toBe(localColour);
  expect(testColour).toBe(localColour);
  expect(aliasColour).toBe(borrowedColour);
  expect(aliasIdentity).toBe(helperIdentity);
  const extension = process.env.NPM_CONTEXT_EXTENSION;
  const aliasPath = fileURLToPath(new URL('../shared/helper.' + extension, import.meta.url));
  const helperPath = realpathSync(aliasPath);
  expect(aliasPath).toContain('/app/shared/helper.' + extension);
  expect(helperPath).toContain('/vitest_owner/shared/helper.' + extension);
  expect(helperPath).not.toBe(aliasPath);
  const canonical = await import(helperPath);
  expect(canonical.borrowedColour).toBe(borrowedColour);
  expect(canonical.helperIdentity).toBe(helperIdentity);
  const sourceRequire = createRequire(helperPath);
  const sourceResolution = sourceRequire.resolve('culori');
  expect(sourceResolution).toContain('/culori@3.3.0/');
  expect(sourceResolution).toBe(realpathSync(sourceResolution));
  expect(sourceRequire('culori')).toBe(sourceRequire(sourceResolution));
  expect(localResolution).toContain('/culori@4.0.2/');
  expect(createRequire(import.meta.url).resolve('culori')).toBe(localResolution);
  expect(fileURLToPath(import.meta.url)).toContain('/app/app/importers.test.' + extension);
  expect(fileURLToPath(localURL)).toContain('/app/app/index.' + extension);
  console.log('moved Vitest ' + process.env.NPM_CONTEXT_PHASE + ' retained distinct npm stores');
});
`)
		for _, phase := range []string{"emitted", "source", "commonjs"} {
			emit := phase != "source"
			extension := "js"
			if !emit {
				extension = "ts"
			}
			library.SetAttr("emit", emit)
			compiler.SetAttr("emit", emit)
			test.SetAttr("emit", emit)
			test.SetAttr("env", map[string]string{"NPM_CONTEXT_EXTENSION": extension, "NPM_CONTEXT_PHASE": phase})
			if phase == "commonjs" {
				library.SetAttr("tsconfig", ":commonjs_config")
			}
			it.Write(buildPath, string(build.Format()))
			it.Write(it.Path(owner, "BUILD.bazel"), string(ownerBuild.Format()))
			log, err := it.BazelLog("borrowed_npm_vitest_"+phase, "test", testTarget(base+"/app"), "--output_groups=+_validation", "--test_output=all")
			if err != nil || !log.Contains("moved Vitest "+phase+" retained distinct npm stores") {
				log.Dump()
				it.Fail("the moved %s Vitest caller lost a source npm store or selected module identity", phase)
			}
			if phase == "commonjs" {
				it.RequireFile(it.Bin(owner, "library.es/shared/helper.js"), "the moved producer's selected ES twin was not emitted")
			}
		}
	}()
	func() {
		for _, file := range []string{"BUILD.bazel", base + "/app/BUILD.bazel", base + "/shared/BUILD.bazel", base + "/member/BUILD.bazel", base + "/app/importers.test.ts", base + "/shared/helper.ts"} {
			defer it.Write(it.Path(file), it.Read(it.Path(file)))
		}
		it.Write(it.Path(base, "shared/helper.ts"), it.Read(it.Path(base, "shared/helper.ts"))+"export const borrowedRequiredColour = createRequire(import.meta.url)('culori');\n")
		it.Write(it.Path(base, "app/importers.test.ts"), `import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createRequire } from 'node:module';
import { realpathSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { colour, identity, borrowedColour, borrowedResolution, localColour, localResolution } from './index';
import { borrowedRequiredColour } from '../shared/helper';
test('emitted borrowed helper keeps its own npm store', async () => {
  assert.equal(colour, '#ff0000');
  assert.equal(identity, 42);
  assert.equal(localColour.formatHex('blue'), '#0000ff');
  assert.notStrictEqual(borrowedColour, localColour);
  assert.match(borrowedResolution, /\/culori@3\.3\.0\//);
  assert.match(localResolution, /\/culori@4\.0\.2\//);
  const require = createRequire(import.meta.url);
  assert.equal(borrowedResolution, realpathSync(borrowedResolution));
  assert.equal(localResolution, realpathSync(localResolution));
  assert.strictEqual(borrowedRequiredColour, require(borrowedResolution));
  assert.notStrictEqual(borrowedRequiredColour, require(localResolution));
  const borrowedURL = import.meta.resolve('../shared/helper.js');
  const borrowed = await import(borrowedURL);
  assert.strictEqual(borrowed.borrowedColour, borrowedColour);
  assert.equal(fileURLToPath(borrowedURL), realpathSync(fileURLToPath(borrowedURL)));
  console.log('emitted Node helper retained distinct import and require stores');
});
`)
		build := it.Path(base, "app/BUILD.bazel")
		file, err := rule.LoadFile(build, base+"/app")
		if err != nil {
			it.Fail("cannot load the borrowed importer Node test: %v", err)
		}
		for _, target := range file.Rules {
			if target.Kind() == "ts_test" {
				target.SetAttr("runner", "@rules_typescript//ts/runners:node_test")
			}
		}
		it.Write(build, string(file.Format()))
		it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", ".", base+"/app", base+"/shared", base+"/member")
		file, err = rule.LoadFile(build, base+"/app")
		if err != nil {
			it.Fail("cannot load the generated borrowed importer Node test: %v", err)
		}
		emitted := false
		for _, target := range file.Rules {
			if target.Kind() == "ts_test" {
				emit, literal := target.Attr("emit").(*bzl.Ident)
				emitted = literal && emit.Name == "True"
			}
		}
		if !emitted {
			it.Fail("Gazelle did not promote the native Node test to emission")
		}
		for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
			log, err := it.BazelLog("borrowed_npm_node"+runfiles, "test", testTarget(base+"/app"), "--output_groups=+_validation", "--test_output=all", runfiles)
			if err != nil || !log.Contains("emitted Node helper retained distinct import and require stores") {
				log.Dump()
				it.Fail("%s Node execution lost the borrowed source's npm store", runfiles)
			}
		}
	}()
	correct = it.Read(input)
	for _, source := range []string{"localColour", "borrowedColour"} {
		it.Write(input, correct+"export const wrong: number = "+source+".formatHex('red');\n")
		log, err := it.BazelLog("borrowed_npm_types_"+source, "build", target, "--output_groups=+_validation")
		if err == nil || !log.Contains("not assignable to type 'number'") {
			log.Dump()
			it.Fail("the %s import lost its declaration companion", source)
		}
	}
	it.Write(input, correct)
	appBuild := it.Path(base, "app/BUILD.bazel")
	defer it.Write(appBuild, it.Read(appBuild))
	wrongContext, err := rule.LoadFile(appBuild, base+"/app")
	if err != nil {
		it.Fail("cannot load the importer identity fixture: %v", err)
	}
	for _, target := range wrongContext.Rules {
		if target.Kind() == "ts_compile" && target.Name() == "app" {
			target.SetAttr("source_node_modules", []string{":node_modules"})
		}
	}
	it.Write(appBuild, string(wrongContext.Format()))
	log, err := it.BazelLog("borrowed_npm_wrong_location", "build", target, "--output_groups=+_validation")
	if err == nil || !log.Contains("links culori@4.0.2") || !log.Contains("culori@3.3.0") {
		log.Dump()
		it.Fail("one importer location supplied a different declared npm store")
	}
	it.Pass("borrowed and local imports retain their own npm stores and types; a mismatched lookup location fails")
}

func emittedLibraryRetainsAncestorScope(it *harness.IT) {
	const pkg = "emitted_ancestor_scope"
	it.RequireNoDir(it.Path(pkg), "the emitted ancestor-scope regression must be staged by this check")
	defer os.RemoveAll(it.Path(pkg))
	manifestPath := it.Path("package.json")
	defer it.Write(manifestPath, it.Read(manifestPath))
	rootBuildPath := it.Path("BUILD.bazel")
	defer it.Write(rootBuildPath, it.Read(rootBuildPath))
	removeScopeOwner := func() {
		build, err := rule.LoadFile(rootBuildPath, "")
		if err != nil {
			it.Fail("cannot load the ancestor scope owner: %v", err)
		}
		for _, target := range build.Rules {
			if target.Kind() == "ts_compile" && target.Name() == "root" {
				target.Delete()
			}
		}
		it.Write(rootBuildPath, string(build.Format()))
	}
	removeScopeOwner()
	rootWithoutOwner := it.Read(rootBuildPath)
	setScopeExport := func(visibility []string) {
		build, err := rule.LoadFile(rootBuildPath, "")
		if err != nil {
			it.Fail("cannot load the ancestor source export: %v", err)
		}
		for _, exported := range build.Rules {
			if exported.Kind() != "exports_files" || len(exported.Args()) != 1 {
				continue
			}
			if files, ok := exported.Args()[0].(*bzl.ListExpr); ok {
				for _, file := range files.List {
					if name, ok := file.(*bzl.StringExpr); ok && name.Value == "package.json" {
						if visibility == nil {
							exported.Delete()
							var packageRule *rule.Rule
							for _, candidate := range build.Rules {
								if candidate.Kind() == "package" {
									packageRule = candidate
									break
								}
							}
							if packageRule == nil {
								packageRule = rule.NewRule("package", "")
								packageRule.Insert(build)
							}
							packageRule.SetAttr("default_visibility", []string{"//visibility:private"})
						} else {
							exported.SetAttr("visibility", visibility)
						}
						it.Write(rootBuildPath, string(build.Format()))
						return
					}
				}
			}
		}
		it.Fail("the ancestor source export is missing")
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(it.Read(manifestPath)), &manifest); err != nil {
		it.Fail("cannot read the root manifest for the emitted scope regression: %v", err)
	}
	manifest["type"] = "module"
	manifest["imports"] = map[string]string{"#scope-type": "./" + pkg + "/value.d.ts"}
	contents, err := json.Marshal(manifest)
	if err != nil {
		it.Fail("cannot write the root manifest for the emitted scope regression: %v", err)
	}
	it.Write(manifestPath, string(contents)+"\n")
	it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`)
	it.Write(it.Path(pkg, "index.ts"), `import type { Value } from '#scope-type';
const value: Value = { answer: 42 };
export const answer: number = value.answer;
console.log('emitted ancestor scope: ' + answer);
`)
	it.Write(it.Path(pkg, "value.d.ts"), "export interface Value { answer: number }\n")
	it.Write(it.Path(pkg, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(
    name = "emitted_ancestor_scope",
    emit = True,  # keep
)
ts_binary(name = "run", entry_point = ":emitted_ancestor_scope")
`)
	it.RequireNoFile(it.Path(pkg, "package.json"), "the emitted program must inherit the exported root manifest")
	it.MustBazel("run", "//:gazelle", "--", "-r=false", pkg)
	target := "//" + pkg
	generatedBuild := it.Read(it.Path(pkg, "BUILD.bazel"))
	it.Write(it.Path(pkg, "bundler.bzl"), `load("@rules_typescript//ts:defs.bzl", "BundlerInfo")
load("@rules_typescript//ts/private:providers.bzl", "NpmPackageInfo")
load("@rules_typescript//ts/private:runtime.bzl", "JS_TOOL_TOOLCHAIN_TYPE", "get_js_tool")

def _shell_escape(value):
    return "'" + value.replace("'", "'\\''") + "'"

def _impl(ctx):
    runtime = get_js_tool(ctx)
    vite = ctx.attr.vite[NpmPackageInfo].store
    executable = ctx.actions.declare_file(ctx.label.name + ".sh")
    command = [runtime.runtime_binary.path] + runtime.args_prefix + [ctx.file.adapter.path, vite.tree.path + "/dist/node/index.js"]
    ctx.actions.write(executable, "#!/bin/sh\nexec " + " ".join([_shell_escape(arg) for arg in command]) + " \"$@\"\n", is_executable = True)
    return [BundlerInfo(
        bundler_binary = executable,
        config_file = None,
        runtime_deps = depset([runtime.runtime_binary, ctx.file.adapter], transitive = [vite.transitive]),
    )]

scope_bundler = rule(
    implementation = _impl,
    attrs = {
        "adapter": attr.label(allow_single_file = True),
        "vite": attr.label(providers = [NpmPackageInfo], cfg = "exec"),
    },
    toolchains = [JS_TOOL_TOOLCHAIN_TYPE],
)
`)
	it.Write(it.Path(pkg, "bundler.mjs"), `import path from 'node:path';
import { pathToFileURL } from 'node:url';
const [vite, ...args] = process.argv.slice(2);
const options = {};
const external = [];
for (let i = 0; i < args.length; i++) {
  if (args[i] === '--external') external.push(args[++i]);
  else if (args[i] === '--sourcemap') options.sourcemap = true;
  else options[args[i]] = args[++i];
}
const { build } = await import(pathToFileURL(path.resolve(vite)).href);
await build({
  configFile: false,
  logLevel: 'silent',
  build: {
    lib: {
      entry: path.resolve(options['--entry']),
      formats: [options['--format']],
      fileName: () => path.basename(options['--out-dir']).replace(/_bundle$/, '') + '.js',
    },
    outDir: path.resolve(options['--out-dir']),
    emptyOutDir: false,
    minify: false,
    sourcemap: options.sourcemap ?? false,
    rollupOptions: { external },
  },
});
`)
	it.Write(it.Path(pkg, "BUILD.bazel"), `load(":bundler.bzl", "scope_bundler")
`+generatedBuild+`
scope_bundler(name = "bundler", adapter = "bundler.mjs", vite = "@npm//:vite")
genrule(
    name = "bundle_format",
    outs = ["bundled_run_bundle/package.json"],
    cmd = "echo '{\"type\":\"commonjs\"}' > $@",
)
ts_binary(
    name = "bundled_run",
    entry_point = ":emitted_ancestor_scope",
    bundler = ":bundler",
    format = "cjs",
    data = [":bundle_format"],
)
`)
	for _, backend := range []string{"tsgo", "oxc"} {
		flag := "--@rules_typescript//ts:declarations=" + backend
		it.MustBazel("build", target, flag, "--output_groups=+declarations,+_validation")
		it.RequireNoFile(it.Bin(pkg, "package.json"), "an ancestor scope must not become a closer manifest with different relative targets")
		if strings.Contains(it.Read(it.Bin(pkg, "index.js")), "#scope-type") {
			it.Fail("%s retained the compiler-only package import at runtime", backend)
		}
		for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
			log, err := it.BazelLog("emitted_ancestor_scope_"+backend+runfiles, "run", "//"+pkg+":run", flag, runfiles)
			if err != nil || !log.Contains("emitted ancestor scope: 42") {
				log.Dump()
				it.Fail("%s under %s lost the original scope at its logical ancestor path", backend, runfiles)
			}
		}
		if got := strings.TrimSpace(it.BazelStdout("run", "//"+pkg+":bundled_run", flag)); got != "emitted ancestor scope: 42" {
			it.Fail("%s applied the original module's runtime scope to its self-contained CJS bundle: %q", backend, got)
		}
	}
	it.Write(it.Path(pkg, "BUILD.bazel"), generatedBuild)
	for _, source := range []string{"bundler.bzl", "bundler.mjs"} {
		if err := os.Remove(it.Path(pkg, source)); err != nil {
			it.Fail("cannot remove the bundle fixture source %s: %v", source, err)
		}
	}
	requireLabels(it, "srcs", target, []string{"//" + pkg + ":index.ts", "//" + pkg + ":value.d.ts"})
	requireLabels(it, "package_scopes", target, []string{"//:package.json"})
	if it.Read(rootBuildPath) != rootWithoutOwner {
		it.Fail("partial generation changed the untouched ancestor scope package")
	}
	shadowed := pkg + "/shadowed"
	it.Write(it.Path(shadowed, "package.json"), `{"type":"module"}`)
	it.Write(it.Path(shadowed, "index.ts"), "console.log('nearest scope retained');\n")
	it.Write(it.Path(shadowed, "plugin/value.ts"), "export const answer: number = 42;\n")
	it.Write(it.Path(shadowed, "main.mjs"), "import { answer } from './plugin/value.js'; if (answer !== 42) throw new Error('data plugin changed'); console.log('data plugin: ' + answer);\n")
	it.Write(it.Path(shadowed, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(name = "metadata", srcs = [], package_scopes = ["//:package.json"])
ts_compile(name = "entry", srcs = ["index.ts", "plugin/value.ts"], package_scopes = ["package.json"], deps = [":metadata"], emit = True, visibility = [":__subpackages__"])
ts_binary(name = "run", entry_point = ":entry")
ts_binary(name = "raw", entry_point = "main.mjs", data = [":entry"])
genrule(name = "closer_scope", outs = ["plugin/package.json"], cmd = "echo '{\"type\":\"commonjs\"}' > $@", visibility = [":__subpackages__"])
ts_binary(name = "shadowed_raw", entry_point = "main.mjs", data = [":entry", ":closer_scope"])
`)
	if got := strings.TrimSpace(it.BazelStdout("run", "//"+shadowed+":run")); got != "nearest scope retained" {
		it.Fail("a shadowed ancestor scope rejected a runnable module with its own package scope: %q", got)
	}
	if got := strings.TrimSpace(it.BazelStdout("run", "//"+shadowed+":raw")); got != "data plugin: 42" {
		it.Fail("a raw JavaScript entry lost its data-only compiled plugin's package scope: %q", got)
	}
	log, err := it.BazelLog("data_plugin_rejects_closer_scope", "build", "//"+shadowed+":shadowed_raw")
	if err == nil || !log.Contains("is shadowed by runtime manifest") || !log.Contains("plugin/value.js") {
		log.Dump()
		it.Fail("a raw JavaScript entry accepted a closer incompatible package scope for its data-only compiled plugin")
	}
	it.Write(it.Path(shadowed, "suite/package.json"), `{"type":"module"}`)
	it.Write(it.Path(shadowed, "suite/data.test.mjs"), "const plugin = '../plugin/value.js'; const { answer } = await import(plugin); if (answer !== 42) throw new Error('data plugin changed'); console.log('test data plugin: ' + answer);\n")
	it.Write(it.Path(shadowed, "suite/BUILD.bazel"), fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_test")
ts_test(name = "data_test", srcs = ["data.test.mjs"], package_scopes = ["package.json"], data = ["//%s:entry"], runner = "@rules_typescript//ts/runners:node_test")
ts_test(name = "shadowed_data_test", srcs = ["data.test.mjs"], package_scopes = ["package.json"], data = ["//%s:entry", "//%s:closer_scope"], runner = "@rules_typescript//ts/runners:node_test")
`, shadowed, shadowed, shadowed))
	log, err = it.BazelLog("test_data_plugin_retains_scope", "test", "//"+shadowed+"/suite:data_test", "--test_output=all")
	if err != nil || !log.Contains("test data plugin: 42") {
		log.Dump()
		it.Fail("a Node test lost its data-only compiled plugin's package scope")
	}
	log, err = it.BazelLog("test_data_plugin_rejects_closer_scope", "build", "//"+shadowed+"/suite:shadowed_data_test")
	if err == nil || !log.Contains("is shadowed by runtime manifest") || !log.Contains("plugin/value.js") {
		log.Dump()
		it.Fail("a Node test accepted a closer incompatible package scope for its data-only compiled plugin")
	}
	if err := os.RemoveAll(it.Path(shadowed)); err != nil {
		it.Fail("cannot remove the shadowed scope fixture: %v", err)
	}
	manifest["imports"] = map[string]string{"#value": "./" + pkg + "/value.js"}
	contents, err = json.Marshal(manifest)
	if err != nil {
		it.Fail("cannot write the runtime ancestor mapping: %v", err)
	}
	it.Write(manifestPath, string(contents)+"\n")
	it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts","value.ts"]}`)
	it.Write(it.Path(pkg, "index.ts"), `import { value, moduleURL } from '#value';
export const answer: number = value;
const name: string = '#value';
const dynamic = await import(name);
if (answer !== 42 || dynamic.value !== 42 || moduleURL !== new URL('./value.js', import.meta.url).href || dynamic.moduleURL !== moduleURL) {
  throw new Error('ancestor scope escaped the emitted module closure');
}
console.log('emitted ancestor scope: ' + answer);
`)
	it.Write(it.Path(pkg, "value.ts"), "export const value: number = 42;\nexport const moduleURL: string = import.meta.url;\n")
	it.Write(it.Path(pkg, "value.js"), "throw new Error('stale source JavaScript was loaded');\n")
	if err := os.Remove(it.Path(pkg, "value.d.ts")); err != nil {
		it.Fail("cannot remove the compile-only declaration: %v", err)
	}
	for _, policy := range []struct {
		name       string
		visibility []string
	}{
		{"private", []string{"//visibility:private"}},
		{"unexported_private_default", nil},
		{"other_package", []string{"//other:__pkg__"}},
	} {
		setScopeExport(policy.visibility)
		beforeRoot, beforeConsumer := it.Read(rootBuildPath), it.Read(it.Path(pkg, "BUILD.bazel"))
		log, err := it.BazelLog("ancestor_publication_"+policy.name, "run", "//:gazelle", "--", "-index=false", "-r=false", ".", pkg)
		if err == nil || !log.Contains("denies access") || !log.Contains(target+" requires runtime package scope") || !log.Contains("package.json") {
			log.Dump()
			it.Fail("%s scope publication did not reject the denied emitted consumer", policy.name)
		}
		if it.Read(rootBuildPath) != beforeRoot || it.Read(it.Path(pkg, "BUILD.bazel")) != beforeConsumer {
			it.Fail("%s scope denial changed BUILD files", policy.name)
		}
		it.Write(rootBuildPath, rootWithoutOwner)
	}
	for _, update := range []string{"full", "partial"} {
		args := []string{"run", "//:gazelle", "--"}
		if update == "partial" {
			args = append(args, "-index=false", "-r=false", pkg)
		}
		beforeRoot := it.Read(rootBuildPath)
		it.MustBazel(args...)
		if update == "partial" && it.Read(rootBuildPath) != beforeRoot {
			it.Fail("partial generation rewrote the existing ancestor scope owner")
		}
		requireLabels(it, "srcs", "//:root", nil)
		requireLabels(it, "package_scopes", "//:root", []string{"//:package.json"})
		requireLabels(it, "deps", target, []string{"//:root"})
		requireLabels(it, "package_scopes", target, nil)
		for _, backend := range []string{"tsgo", "oxc"} {
			flag := "--@rules_typescript//ts:declarations=" + backend
			it.MustBazel("build", target, flag, "--output_groups=+declarations,+_validation")
			it.RequireNoFile(it.Bin(pkg, "package.json"), "an ancestor mapping must retain its distance from the emitted module")
			log, err := it.BazelLog("ancestor_runtime_"+update+"_"+backend, "run", "//"+pkg+":run", flag)
			if err != nil || !log.Contains("emitted ancestor scope: 42") || log.Contains("stale source JavaScript was loaded") {
				log.Dump()
				it.Fail("%s/%s lost the ancestor mapping or loaded source JavaScript", update, backend)
			}
		}
	}
	publicScopeBuild := it.Read(rootBuildPath)
	removeScopeOwner()
	denied := pkg + "_denied"
	it.RequireNoDir(it.Path(denied), "the denied scope consumer must be staged by this check")
	defer os.RemoveAll(it.Path(denied))
	it.Write(it.Path(denied, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`)
	it.Write(it.Path(denied, "index.ts"), "import scope from '../package.json'; export const format: string = scope.type;\n")
	it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", denied)
	setScopeExport([]string{"//" + pkg + ":__pkg__"})
	it.Write(rootBuildPath, it.Read(rootBuildPath)+"\nfilegroup(name = \"scope_same_package\", srcs = [\":root\"])\n")
	restrictedExport := it.Read(rootBuildPath)
	if log, err := it.BazelLog("ancestor_restricted_full_update", "run", "//:gazelle"); err == nil || !log.Contains("denies access") || it.Read(rootBuildPath) != restrictedExport {
		log.Dump()
		it.Fail("a full update accepted a root scope export that denies other root-scoped packages")
	}
	it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", ".", pkg)
	it.MustBazel("build", "//:scope_same_package")
	if got := strings.TrimSpace(it.BazelStdout("run", "//"+pkg+":run")); got != "emitted ancestor scope: 42" {
		it.Fail("the allowed consumer lost its restricted ancestor scope: %q", got)
	}
	restrictedScopeBuild := it.Read(rootBuildPath)
	it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", pkg)
	if it.Read(rootBuildPath) != restrictedScopeBuild {
		it.Fail("partial generation changed the restricted ancestor publisher")
	}
	for _, role := range []string{"json", "scope"} {
		visibilityInput := "//:package.json"
		if role == "scope" {
			it.Write(it.Path(denied, "index.ts"), "export const answer: number = 42;\n")
			buildPath := it.Path(denied, "BUILD.bazel")
			build, err := rule.LoadFile(buildPath, denied)
			if err != nil {
				it.Fail("cannot load the denied scope consumer: %v", err)
			}
			for _, target := range build.Rules {
				if target.Kind() == "ts_compile" && target.Name() == denied {
					target.SetAttr("emit", true)
					target.AttrComments("emit").Suffix = []bzl.Comment{{Token: "# keep"}}
				}
			}
			it.Write(buildPath, string(build.Format()))
			it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", denied)
			if it.Read(rootBuildPath) != restrictedScopeBuild {
				it.Fail("partial generation for the denied consumer changed the restricted ancestor publisher")
			}
			requireLabels(it, "srcs", "//"+denied+":"+denied, []string{"//" + denied + ":index.ts"})
			requireLabels(it, "deps", "//"+denied+":"+denied, []string{"//:root"})
			visibilityInput = "//:root"
		} else {
			requireLabels(it, "srcs", "//"+denied+":"+denied, []string{"//:package.json", "//" + denied + ":index.ts"})
			requireLabels(it, "deps", "//"+denied+":"+denied, nil)
		}
		requireLabels(it, "package_scopes", "//"+denied+":"+denied, nil)
		log, err := it.BazelLog("ancestor_visibility_"+role, "build", "//"+denied+":"+denied, "--output_groups=+_validation")
		if err == nil || !log.Contains("not visible") || !log.Contains(visibilityInput) {
			log.Dump()
			it.Fail("the %s consumer bypassed visibility of %s", role, visibilityInput)
		}
	}
	it.Write(rootBuildPath, publicScopeBuild)
	it.MustBazel("build", "//"+denied+":"+denied, "--output_groups=+_validation")
	if err := os.RemoveAll(it.Path(denied)); err != nil {
		it.Fail("cannot remove the denied scope consumer: %v", err)
	}
	removeScopeOwner()
	it.Write(it.Path(pkg, "index.ts"), "import scope from '../package.json' with { type: 'json' }; export const format: string = scope.type; console.log('original scope format: ' + format);\n")
	it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`)
	it.MustBazel("run", "//:gazelle", "--", "-r=false", pkg)
	requireLabels(it, "srcs", target, []string{"//:package.json", "//" + pkg + ":index.ts"})
	requireLabels(it, "package_scopes", target, nil)
	build, err := rule.LoadFile(it.Path(pkg, "BUILD.bazel"), pkg)
	if err != nil {
		it.Fail("cannot load the scope overlap fixture: %v", err)
	}
	for _, target := range build.Rules {
		if target.Kind() == "ts_compile" && target.Name() == pkg {
			target.SetAttr("package_scopes", []string{"//:package.json"})
		}
	}
	it.Write(it.Path(pkg, "BUILD.bazel"), string(build.Format()))
	for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
		log, err := it.BazelLog("scope_import_keeps_module_layout"+runfiles, "run", "//"+pkg+":run", "--@rules_typescript//ts:declarations=oxc", runfiles)
		if err != nil || !log.Contains("original scope format: module") {
			log.Dump()
			it.Fail("an explicit JSON import lost its original scope at the logical runtime path under %s", runfiles)
		}
	}
	it.Pass("ancestor scope metadata compiles without an owner; runnable full/partial generation retains scope and emitted module identity")
}

func transitiveEmissionPublishesScopeAtomically(it *harness.IT) {
	const base = "transitive_ancestor_scope"
	it.RequireNoDir(it.Path(base), "the transitive scope fixture must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	defer it.Write(it.Path("pnpm-lock.yaml"), it.Read(it.Path("pnpm-lock.yaml")))
	workspace := it.Path("pnpm-workspace.yaml")
	defer it.Write(workspace, it.Read(workspace))
	it.Write(it.Path(base, "package.json"), `{"type":"module","imports":{"#value":"./lib/value.js","#fixture":"./lib/fixture.json"},"devDependencies":{"vitest":"4.1.11"}}`)
	it.Write(it.Path(base, "BUILD.bazel"), `exports_files(["lib/fixture.json", "package.json"], visibility = ["//visibility:public"])
`)
	it.Write(it.Path(base, "app/package.json"), `{"type":"module"}`)
	it.Write(workspace, it.Read(workspace)+"  - "+base+"\n  - "+base+"/app\n")
	it.MustBazel("run", "//:pnpm", "--", "install", "--lockfile-only", "--no-frozen-lockfile")
	it.Write(it.Path(base, "app/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`)
	it.Write(it.Path(base, "app/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary")
ts_binary(name = "run", entry_point = ":app")
`)
	it.Write(it.Path(base, "app/index.ts"), `import { answer, libraryURL } from '../lib/index.js';
if (await answer() !== 42 || libraryURL !== new URL('../lib/index.js', import.meta.url).href) {
  throw new Error('transitive scope loaded a different module');
}
console.log('transitive ancestor scope: 42');
`)
	it.Write(it.Path(base, "lib/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true,"types":[]},"files":["index.ts","value.ts"]}`)
	it.Write(it.Path(base, "lib/index.ts"), `import { value, moduleURL } from '#value';
import fixture from '#fixture' with { type: 'json' };
export const libraryURL: string = import.meta.url;
export async function answer(): Promise<number> {
  const name: string = '#value';
  const dynamic = await import(name);
  if (dynamic.moduleURL !== moduleURL || moduleURL !== new URL('./value.js', import.meta.url).href || fixture.value !== value) {
    throw new Error('transitive scope escaped the emitted closure');
  }
  return value;
}
`)
	it.Write(it.Path(base, "lib/value.ts"), "export const value: number = 42;\nexport const moduleURL: string = import.meta.url;\n")
	it.Write(it.Path(base, "lib/value.js"), "throw new Error('stale transitive JavaScript was loaded');\n")
	it.Write(it.Path(base, "lib/fixture.json"), `{"value":42}`)
	parentBuild := it.Read(it.Path(base, "BUILD.bazel"))
	appBuild := it.Read(it.Path(base, "app/BUILD.bazel"))
	log, err := it.BazelLog("partial_source_export_transition", "run", "//:gazelle", "--", "-index=false", "-r=false", base+"/lib")
	if err == nil || !log.Contains("moving source exports") || !log.Contains("both BUILD files") {
		log.Dump()
		it.Fail("a child-only update published half of an export ownership move")
	}
	it.RequireNoFile(it.Path(base, "lib/BUILD.bazel"), "a refused update must not publish the new child package")
	if it.Read(it.Path(base, "BUILD.bazel")) != parentBuild || it.Read(it.Path(base, "app/BUILD.bazel")) != appBuild {
		it.Fail("a refused export move changed an existing BUILD file")
	}
	it.MustBazel("run", "//:gazelle", "--", "-index=false", base)
	lib := "//" + base + "/lib:lib"
	scope := "//" + base + ":" + base
	requireLabels(it, "deps", lib, []string{scope})
	requireLabels(it, "package_scopes", lib, nil)
	requireLabels(it, "srcs", scope, nil)
	requireLabels(it, "package_scopes", scope, []string{"//" + base + ":package.json"})
	if strings.Contains(it.Read(it.Path(base, "BUILD.bazel")), "lib/fixture.json") {
		it.Fail("the complete update retained a source export across the new package boundary")
	}
	for _, backend := range []string{"tsgo", "oxc"} {
		flag := "--@rules_typescript//ts:declarations=" + backend
		it.MustBazel("build", lib, flag, "--output_groups=+declarations,+_validation")
		it.RequireNoFile(it.Bin(base, "lib/package.json"), "the library's scope must retain its ancestor position")
		if got := it.Read(it.Bin(base, "package.json")); got != it.Read(it.Path(base, "package.json")) {
			it.Fail("%s changed the metadata-only ancestor publisher's manifest: %s", backend, got)
		}
		log, err := it.BazelLog("transitive_ancestor_scope_"+backend, "run", "//"+base+"/app:run", flag)
		if err != nil || !log.Contains("transitive ancestor scope: 42") || log.Contains("stale transitive JavaScript was loaded") {
			log.Dump()
			it.Fail("%s did not publish the inferred library dependency's runtime scope", backend)
		}
	}
	publisherBuildPath := it.Path(base, "BUILD.bazel")
	metadataPublisherBuild := it.Read(publisherBuildPath)
	publisherBuild, err := rule.LoadFile(publisherBuildPath, base)
	if err != nil {
		it.Fail("cannot load the ancestor scope publisher: %v", err)
	}
	var publisher *rule.Rule
	for _, target := range publisherBuild.Rules {
		if target.Kind() == "ts_compile" && target.Name() == base {
			publisher = target
		}
	}
	if publisher == nil {
		it.Fail("the ancestor scope publisher is missing")
	}
	publisher.SetAttr("srcs", []string{"package.json"})
	publisher.DelAttr("package_scopes")
	publisher.AddComment("# keep")
	it.Write(publisherBuildPath, string(publisherBuild.Format()))
	requireLabels(it, "srcs", scope, []string{"//" + base + ":package.json"})
	requireLabels(it, "package_scopes", scope, nil)
	compatibleManifest := it.Read(it.Path(base, "package.json"))
	it.Write(it.Path(base, "package.json"), strings.Replace(compatibleManifest, "./lib/value.js", "./lib/value.ts", 1))
	for _, backend := range []string{"tsgo", "oxc"} {
		flag := "--@rules_typescript//ts:declarations=" + backend
		log, err := it.BazelLog("ancestor_module_scope_targets_"+backend, "build", lib, flag, "--output_groups=+declarations,+_validation")
		if err == nil || !log.Contains("runtime package scope") || !log.Contains(`needs "./lib/value.js"`) {
			log.Dump()
			it.Fail("%s accepted an ancestor JSON module whose private import still selects TypeScript", backend)
		}
	}
	it.Write(it.Path(base, "package.json"), compatibleManifest)
	for _, backend := range []string{"tsgo", "oxc"} {
		if got := strings.TrimSpace(it.BazelStdout("run", "//"+base+"/app:run", "--@rules_typescript//ts:declarations="+backend)); got != "transitive ancestor scope: 42" {
			it.Fail("%s rejected a compatible ancestor JSON module scope: %q", backend, got)
		}
	}
	it.Write(publisherBuildPath, metadataPublisherBuild)
	local := base + "/module_scope"
	localManifest := `{"name":"scope-module","type":"module","imports":{"#value":"./value.ts"},"exports":"./value.ts"}`
	it.Write(it.Path(local, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["main.ts","value.ts"]}`)
	it.Write(it.Path(local, "main.ts"), `import * as privateValue from '#value';
import * as selfValue from 'scope-module';
import * as relativeValue from './value.js';
if (privateValue !== relativeValue || selfValue !== relativeValue || privateValue.value !== 42 ||
    privateValue.moduleURL !== new URL('./value.js', import.meta.url).href) {
  throw new Error('JSON module scope escaped the emitted module closure');
}
console.log('module scope: 42');
`)
	it.Write(it.Path(local, "value.ts"), "export const value: number = 42;\nexport const moduleURL: string = import.meta.url;\n")
	localBuild := `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(name = "library", srcs = ["main.ts", "value.ts", "package.json"], emit = True, tsconfig = "tsconfig.json")
ts_binary(name = "run", entry_point = ":library", entry_file = "main.ts")
`
	for _, backend := range []string{"tsgo", "oxc"} {
		flag := "--@rules_typescript//ts:declarations=" + backend
		it.Write(it.Path(local, "BUILD.bazel"), localBuild)
		it.Write(it.Path(local, "package.json"), localManifest)
		log, err := it.BazelLog("local_module_scope_targets_"+backend, "build", "//"+local+":library", flag, "--output_groups=+declarations,+_validation")
		if err == nil || !log.Contains("runtime package scope") || !log.Contains(`needs "./value.js"`) {
			log.Dump()
			it.Fail("%s accepted a local JSON module whose private import and export still select TypeScript", backend)
		}
		it.Write(it.Path(local, "package.json"), strings.ReplaceAll(localManifest, "./value.ts", "./value.js"))
		if got := strings.TrimSpace(it.BazelStdout("run", "//"+local+":run", flag)); got != "module scope: 42" {
			it.Fail("%s rejected or changed a compatible local JSON module scope: %q", backend, got)
		}
		it.Write(it.Path(local, "package.json"), localManifest)
		it.Write(it.Path(local, "BUILD.bazel"), strings.Replace(localBuild, "emit = True", "emit = False", 1))
		// Source mode declares no scope copy, so the emitted build's copy would otherwise linger.
		staged := it.Bin(local, "package.json")
		if err := os.Remove(staged); err != nil && !os.IsNotExist(err) {
			it.Fail("cannot remove the emitted build's runtime scope: %v", err)
		}
		it.MustBazel("build", "//"+local+":library", flag, "--output_groups=+_validation")
		it.RequireNoFile(staged, "%s source mode stages no runtime copy of its authored scope", backend)
	}
	if err := os.RemoveAll(it.Path(local)); err != nil {
		it.Fail("cannot remove the local JSON module scope fixture: %v", err)
	}
	for _, source := range []string{"scope.test.ts", "scope.d.ts"} {
		it.Write(it.Path(base, "tsconfig.json"), fmt.Sprintf(`{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true,"types":[]},"files":[%q]}`, source))
		if source == "scope.test.ts" {
			it.Write(it.Path(base, source), "import { expect, test } from 'vitest';\nimport { value } from '#value';\nimport fixture from '#fixture' with { type: 'json' };\ntest('ancestor scope reaches emitted declarations and JSON', () => { expect(value).toBe(42); expect(fixture.value).toBe(value); });\n")
		} else {
			if err := os.Remove(it.Path(base, "scope.test.ts")); err != nil {
				it.Fail("cannot remove the ancestor test input: %v", err)
			}
			it.Write(it.Path(base, source), "export interface Scope { answer: number }\n")
		}
		it.MustBazel("run", "//:gazelle", "--", "-index=false", base)
		requireLabels(it, "srcs", scope, nil)
		requireLabels(it, "package_scopes", scope, []string{"//" + base + ":package.json"})
		requireLabels(it, "deps", lib, []string{scope})
		if source == "scope.test.ts" {
			requireLabels(it, "srcs", testTarget(base), []string{"//" + base + ":scope.test.ts"})
			requireLabels(it, "package_scopes", testTarget(base), nil)
			if !slices.Contains(labels(it, "deps", testTarget(base)), lib) {
				it.Fail("ancestor test does not retain the library supplying its runtime scope")
			}
		}
		before := map[string]string{}
		for _, pkg := range []string{base, base + "/app", base + "/lib"} {
			before[pkg] = it.Read(it.Path(pkg, "BUILD.bazel"))
		}
		it.MustBazel("run", "//:gazelle", "--", base)
		for pkg, content := range before {
			if it.Read(it.Path(pkg, "BUILD.bazel")) != content {
				it.Fail("%s ancestor program needs another generation pass in %s", source, pkg)
			}
		}
		for _, backend := range []string{"tsgo", "oxc"} {
			flag := "--@rules_typescript//ts:declarations=" + backend
			args := []string{"build", "//" + base + "/app:run", flag, "--output_groups=+_validation"}
			if source == "scope.test.ts" {
				args = append(args, testTarget(base))
			}
			it.MustBazel(args...)
			log, err := it.BazelLog("ancestor_program_"+source+"_"+backend, "run", "//"+base+"/app:run", flag)
			if err != nil || !log.Contains("transitive ancestor scope: 42") {
				log.Dump()
				it.Fail("%s/%s ancestor program omitted its child's runtime scope", source, backend)
			}
			if source == "scope.test.ts" {
				it.MustBazel("test", testTarget(base), flag)
			}
		}
	}
	it.Write(it.Path(base, "app/BUILD.bazel"), "")
	it.MustBazel("run", "//:gazelle", "--", base)
	build, err := rule.LoadFile(it.Path(base, "BUILD.bazel"), base)
	if err != nil {
		it.Fail("cannot read the ancestor package after runtime consumer removal: %v", err)
	}
	for _, target := range build.Rules {
		if target.Kind() == "ts_compile" && target.Name() == base {
			it.Fail("declaration-only ancestor retained an unused runtime scope publisher")
		}
	}
	it.Pass("transitive emitted libraries retain their own scope; export ownership moves publish atomically")
}

func emittedPackageScopesFollowModules(it *harness.IT) {
	const base = "runtime_scope_placement"
	it.RequireNoDir(it.Path(base), "runtime scope fixtures must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	manifestFor := func(supply string) string {
		moduleType := `"type":"module",`
		if supply == "commonjs" {
			moduleType = ""
		}
		unpublished := ""
		if supply == "direct" {
			unpublished = `,"#unpublished":"./unpublished.ts"`
		}
		return fmt.Sprintf(`{"name":%q,%s"main":"dir/../value.ts","imports":{"#value":"./value.ts","#query":"./value.ts?instance=one#first","#encoded":"./%%76alue.ts?instance=one#first","#other-query":"./value.ts?instance=two#first","#other-fragment":"./value.ts?instance=one#second","#pattern/*":"./*.ts?instance=one#first"%s},"exports":{".":"./value.ts","./query":"./value.ts?instance=one#first","./other-query":"./value.ts?instance=two#first","./other-fragment":"./value.ts?instance=one#second"}}`, "runtime-scope-"+supply, moduleType, unpublished)
	}
	const urlIdentity = `async function assertURLIdentity(aliases: string[], otherQuery: string, otherFragment: string, pathname: string) {
  const first = await import(aliases[0]);
  for (const alias of aliases) {
    if (await import(alias) !== first) throw new Error('equal package target URLs created different module instances');
  }
  const query = await import(otherQuery);
  const fragment = await import(otherFragment);
  const expected = new URL(first.moduleURL);
  if (!expected.pathname.endsWith(pathname) || expected.search !== '?instance=one' || expected.hash !== '#first' ||
      first === query || first === fragment || query === fragment) {
    throw new Error('package target pathname or URL identity changed');
  }
  expected.search = '?instance=two';
  if (query.moduleURL !== expected.href) throw new Error('package target query identity changed');
  expected.search = '?instance=one';
  expected.hash = '#second';
  if (fragment.moduleURL !== expected.href) throw new Error('package target fragment identity changed');
}
`
	defer it.Write(it.Path("pnpm-lock.yaml"), it.Read(it.Path("pnpm-lock.yaml")))
	workspace := it.Path("pnpm-workspace.yaml")
	defer it.Write(workspace, it.Read(workspace))
	for _, supply := range []string{"direct", "dependency", "transitive", "emitted", "commonjs"} {
		it.Write(it.Path(base, supply, "package.json"), manifestFor(supply)+"\n")
	}
	it.Write(workspace, it.Read(workspace)+"  - "+base+"/*\n")
	it.MustBazel("run", "//:pnpm", "--", "install", "--lockfile-only", "--no-frozen-lockfile")
	for _, supply := range []string{"direct", "dependency", "transitive", "emitted", "foreign", "commonjs"} {
		manifest := manifestFor(supply)
		pkg := base + "/" + supply
		sourcePkg, outputPkg := pkg, pkg
		if supply == "foreign" {
			pkg += "/consumer"
			sourcePkg += "/foreign"
			// Outputs keep paths below the common root of the package and its sources.
			outputPkg = filepath.Join(pkg, "foreign")
			it.Write(it.Path(sourcePkg, "package.json"), manifest+"\n")
		}
		it.Write(it.Path(sourcePkg, "main.ts"), `import * as privateValue from '#value';
import * as relativeValue from './value.js';
import * as selfValue from 'runtime-scope-`+supply+`';
export const answer: number = privateValue.value;
if (answer !== 42 || privateValue !== relativeValue || privateValue !== selfValue ||
    privateValue.moduleURL !== new URL('./value.js', import.meta.url).href || !privateValue.moduleURL.endsWith('/value.js')) {
  throw new Error('package scope escaped the emitted module closure');
}
console.log('runtime scope placement: ' + answer);
`+urlIdentity+`
await assertURLIdentity(['#query', '#encoded', '#pattern/value', 'runtime-scope-`+supply+`/query', './value.js?instance=one#first'], '#other-query', '#other-fragment', '/value.js');
`)
		it.Write(it.Path(sourcePkg, "value.ts"), "export const value: number = 42;\nexport const moduleURL: string = import.meta.url;\n")
		sources := []string{"main.ts", "value.ts"}
		files := `"main.ts", "value.ts"`
		allowJS := ""
		backends := []string{"tsgo", "oxc"}
		if supply == "commonjs" {
			it.Write(it.Path(sourcePkg, "legacy.js"), "module.exports = { answer: 42, mainURL: require('node:url').pathToFileURL(require.resolve('./')).href };\n")
			it.Write(it.Path(sourcePkg, "main.ts"), it.Read(it.Path(sourcePkg, "main.ts"))+"\nimport legacy from './legacy.js'; if (legacy.answer !== answer || legacy.mainURL !== new URL('./value.js', import.meta.url).href) throw new Error('package scope changed CommonJS format or main resolution');\n")
			sources = append(sources, "legacy.js")
			files += `, "legacy.js"`
			allowJS = `,"allowJs":true`
			// Oxc's isolated declarations reject the allowJs this raw JavaScript requires.
			backends = []string{"tsgo"}
		}
		if supply == "foreign" {
			sources = []string{"//" + sourcePkg + ":main.ts", "//" + sourcePkg + ":value.ts"}
			files = `"../foreign/main.ts", "../foreign/value.ts"`
			it.Write(it.Path(sourcePkg, "BUILD.bazel"), `exports_files(["main.ts", "value.ts", "package.json"], visibility = ["//visibility:public"])`)
		}
		if supply == "emitted" {
			files = `"main.ts"`
		}
		config := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","target":"es2022","types":[]` + allowJS + `},"files":[%s]}`
		it.Write(it.Path(pkg, "tsconfig.json"), fmt.Sprintf(config, files))
		deps := ""
		if supply == "dependency" || supply == "transitive" {
			metadata := base + "/" + supply + "_metadata"
			it.Write(it.Path(metadata, "BUILD.bazel"), fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_compile")
# keep
ts_compile(name = "scope", srcs = [], package_scopes = [%q], visibility = ["//visibility:public"])
# keep
ts_compile(name = "forwarder", srcs = [], deps = [":scope"], visibility = ["//visibility:public"])
`, "//"+pkg+":package.json"))
			dep := "scope"
			if supply == "transitive" {
				dep = "forwarder"
			}
			deps = fmt.Sprintf("    deps = [%q], # keep\n", "//"+metadata+":"+dep)
		}
		extraLoad, supplier := "", ""
		if supply == "direct" {
			it.Write(it.Path(pkg, "unpublished.ts"), "export const unpublished = 0;\n")
			it.Write(it.Path(pkg, "stale_owner.bzl"), `load("@rules_typescript//ts/private:providers.bzl", "TsInfo", "ts_info")
def _impl(ctx):
    runtime = ctx.actions.declare_file("relocated/unpublished.js")
    ctx.actions.write(runtime, "export const unpublished = 0;")
    scope = ctx.actions.declare_file("relocated/package.json")
    ctx.actions.write(scope, `+fmt.Sprintf("%q", strings.NewReplacer("./unpublished.ts", "./unpublished.js", "./*.ts", "./*.js").Replace(manifest))+`)
    info = ts_info()
    fields = {field: getattr(info, field) for field in dir(info) if field not in ["to_json", "to_proto"]}
    fields["owners"] = depset([struct(
        label = str(ctx.label),
        files = depset(),
        declarations = depset(),
        type_inputs = depset(),
        runtime_files = ((ctx.file.src, runtime),),
        runtime_scopes = ((ctx.file.scope, scope),),
        importers = (),
    )])
    if ctx.attr.live:
        fields["transitive_js"] = depset([runtime], order = "postorder")
        fields["transitive_data"] = depset([scope], order = "postorder")
    return [TsInfo(**fields), DefaultInfo(files = depset([runtime, scope]))]
stale_runtime_owner = rule(implementation = _impl, attrs = {"src": attr.label(allow_single_file = True), "scope": attr.label(allow_single_file = True), "live": attr.bool()})
`)
			extraLoad = "load(\":stale_owner.bzl\", \"stale_runtime_owner\")\n"
			supplier = `stale_runtime_owner(name = "stale_owner", src = "unpublished.ts", scope = "package.json", live = False)
`
			deps = "    deps = [\":stale_owner\"], # keep\n"
		}
		if supply == "emitted" {
			sources = sources[:1]
			it.Write(it.Path(pkg, "value.tsconfig.json"), fmt.Sprintf(config, `"value.ts"`))
			supplier = `# keep
ts_compile(name = "value_library", srcs = ["value.ts"], package_scopes = ["package.json"], emit = True, tsconfig = "value.tsconfig.json")
`
			deps = "    deps = [\":value_library\"], # keep\n"
		}
		quotedSources := make([]string, len(sources))
		for i, source := range sources {
			quotedSources[i] = fmt.Sprintf("%q", source)
		}
		name := filepath.Base(pkg)
		exports := "exports_files([\"package.json\"], visibility = [\"//visibility:public\"])\n"
		configKeep := ""
		if supply == "foreign" {
			exports = ""
			configKeep = " # keep"
		}
		it.Write(it.Path(pkg, "BUILD.bazel"), fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
%s%s%sts_compile(
    name = %q,
    srcs = [%s], # keep
    emit = True, # keep
%s    tsconfig = "tsconfig.json",%s
)
ts_binary(name = "run", entry_point = %q, entry_file = "main.ts")
`, extraLoad, exports, supplier, name, strings.Join(quotedSources, ", "), deps, configKeep, ":"+name))
		it.MustBazel("run", "//:gazelle", "--", "-r=false", pkg)
		target := "//" + pkg + ":" + name
		for _, backend := range backends {
			flag := "--@rules_typescript//ts:declarations=" + backend
			it.MustBazel("build", target, flag, "--output_groups=+declarations,+_validation")
			if got, want := strings.TrimSpace(it.Read(it.Bin(outputPkg, "package.json"))), strings.NewReplacer("dir/../value.ts", "value.js", "./value.ts", "./value.js", "./%76alue.ts", "./value.js", "./*.ts", "./*.js").Replace(manifest); got != want {
				it.Fail("%s %s did not project the runtime scope targets without changing its format: %s", supply, backend, got)
			}
			log, err := it.BazelLog("runtime_scope_"+supply+"_"+backend, "run", "//"+pkg+":run", flag)
			if err != nil || !log.Contains("runtime scope placement: 42") {
				log.Dump()
				it.Fail("%s %s failed package-private lookup beside emitted modules", supply, backend)
			}
		}
		wantSources := []string{"//" + sourcePkg + ":main.ts", "//" + sourcePkg + ":value.ts"}
		if supply == "commonjs" {
			wantSources = append(wantSources, "//"+sourcePkg+":legacy.js")
		}
		if supply == "emitted" {
			wantSources = wantSources[:1]
		}
		requireLabels(it, "srcs", target, wantSources)
		var wantScopes []string
		if supply == "direct" || supply == "foreign" || supply == "commonjs" {
			wantScopes = []string{"//" + sourcePkg + ":package.json"}
		}
		requireLabels(it, "package_scopes", target, wantScopes)
		if supply == "transitive" {
			func() {
				build := it.Path(pkg, "BUILD.bazel")
				manifestPath := it.Path(pkg, "package.json")
				defer it.Write(build, it.Read(build))
				defer it.Write(manifestPath, it.Read(manifestPath))
				var aggregateManifest map[string]any
				if err := json.Unmarshal([]byte(manifest), &aggregateManifest); err != nil {
					it.Fail("cannot load the aggregate runtime scope: %v", err)
				}
				imports := aggregateManifest["imports"].(map[string]any)
				typesFirstValue := json.RawMessage(`{"types":"./value.d.ts","default":"./value.ts"}`)
				imports["#value"] = typesFirstValue
				aggregateManifest["exports"].(map[string]any)["."] = typesFirstValue
				imports["#entry"] = "./main.ts"
				imports["#unpublished"] = "./unpublished.ts"
				encoded, err := json.Marshal(aggregateManifest)
				if err != nil {
					it.Fail("cannot write the aggregate runtime scope: %v", err)
				}
				it.Write(manifestPath, string(encoded)+"\n")
				it.Write(it.Path(pkg, "value.tsconfig.json"), fmt.Sprintf(config, `"value.ts"`))
				it.Write(it.Path(pkg, "entry.tsconfig.json"), fmt.Sprintf(config, `"main.ts"`))
				it.Write(build, `load("@rules_typescript//ts:defs.bzl", "ts_compile")
ts_compile(name = "value_leaf", srcs = ["value.ts"], type_inputs = ["package.json"], emit = True, tsconfig = "value.tsconfig.json", visibility = ["//visibility:public"])
ts_compile(name = "entry_leaf", srcs = ["main.ts"], type_inputs = ["package.json"], deps = [":value_leaf"], emit = True, tsconfig = "entry.tsconfig.json", visibility = ["//visibility:public"])
ts_compile(name = "transitive", srcs = [], package_scopes = ["package.json"], deps = [":entry_leaf", ":value_leaf"], visibility = ["//visibility:public"])
`)
				consumer := base + "/transitive_consumer"
				it.RequireNoDir(it.Path(consumer), "the scope consumer must be staged by this check")
				defer os.RemoveAll(it.Path(consumer))
				it.Write(it.Path(consumer, "entry.ts"), "import '../transitive/main.js';\n")
				it.Write(it.Path(consumer, "package.json"), `{"type":"module"}`)
				it.Write(it.Path(consumer, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","target":"es2022","types":[]},"files":["entry.ts"]}`)
				consumerBuild := `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(name = "entry", srcs = ["entry.ts"], package_scopes = ["package.json"], deps = [%q, %q], emit = True, tsconfig = "tsconfig.json")
ts_binary(name = "run", entry_point = ":entry", entry_file = "entry.ts")
`
				it.Write(it.Path(consumer, "BUILD.bazel"), fmt.Sprintf(consumerBuild, "//"+pkg+":entry_leaf", target))
				it.MustBazel("build", "//"+pkg+":entry_leaf", "//"+pkg+":value_leaf", target, "--output_groups=+_validation")
				projected := strings.NewReplacer("dir/../value.ts", "value.js", "./main.ts", "./main.js", "./value.ts", "./value.js", "./%76alue.ts", "./value.js", "./*.ts", "./*.js").Replace(string(encoded))
				if got := strings.TrimSpace(it.Read(it.Bin(pkg, "package.json"))); got != projected {
					it.Fail("the shared scope owner did not project both leaves, exact and wildcard targets while retaining unpublished targets: %s", got)
				}
				consumerTarget := "//" + consumer + ":run"
				log, err := it.BazelLog("shared_scope_owner", "run", consumerTarget)
				if err != nil || !log.Contains("runtime scope placement: 42") {
					log.Dump()
					it.Fail("the complete aggregate scope did not run both disjoint leaves from another package")
				}
				joined, err := rule.LoadFile(build, pkg)
				if err != nil {
					it.Fail("cannot load the scope-only join: %v", err)
				}
				var aggregate *rule.Rule
				for _, candidate := range joined.Rules {
					if candidate.Kind() == "ts_compile" && candidate.Name() == "transitive" {
						aggregate = candidate
					}
				}
				if aggregate == nil {
					it.Fail("the scope-only aggregate is missing")
				}
				aggregate.DelAttr("package_scopes")
				aggregate.SetAttr("deps", []string{":scope", ":entry_leaf", ":value_leaf"})
				it.Write(build, string(joined.Format())+`
ts_compile(name = "scope", srcs = [], package_scopes = ["package.json"], visibility = ["//visibility:public"])
`)
				for _, join := range []string{"local", "foreign"} {
					joinTarget := target
					if join == "foreign" {
						joinPkg := base + "/transitive_join"
						it.RequireNoDir(it.Path(joinPkg), "the foreign scope join must be staged by this check")
						defer os.RemoveAll(it.Path(joinPkg))
						it.Write(it.Path(joinPkg, "BUILD.bazel"), fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_compile")
ts_compile(name = "combined", srcs = [], deps = [%q, %q, %q], visibility = ["//visibility:public"])
`, "//"+pkg+":scope", "//"+pkg+":entry_leaf", "//"+pkg+":value_leaf"))
						joinTarget = "//" + joinPkg + ":combined"
					}
					it.Write(it.Path(consumer, "BUILD.bazel"), fmt.Sprintf(consumerBuild, "//"+pkg+":entry_leaf", joinTarget))
					it.Write(manifestPath, string(encoded)+"\n")
					log, err = it.BazelLog("scope_only_"+join+"_join_rejects_stale_targets", "build", joinTarget, "--output_groups=+_validation")
					if err == nil || !log.Contains("runtime package scope") || !log.Contains("needs \"./value.js\"") {
						log.Dump()
						it.Fail("the %s source-less join accepted an authored .ts target for an emitted module", join)
					}
					it.Write(manifestPath, projected+"\n")
					log, err = it.BazelLog("scope_only_"+join+"_join_accepts_runtime_targets", "run", consumerTarget)
					if err != nil || !log.Contains("runtime scope placement: 42") {
						log.Dump()
						it.Fail("the %s source-less join rejected compatible scope reuse or lost the runtime module closure", join)
					}
				}
			}()
		}
		if supply == "direct" {
			build := it.Path(pkg, "BUILD.bazel")
			original := it.Read(build)
			for _, publication := range []string{"dependency", "data"} {
				live := strings.Replace(original, "live = False", "live = True", 1)
				if publication == "data" {
					live = strings.Replace(original, "deps = [\":stale_owner\"]", "data = [\":stale_owner\"],\n    deps = [\":stale_owner\"]", 1)
				}
				it.Write(build, live)
				log, err := it.BazelLog("published_disconnected_owner_"+publication, "run", "//"+pkg+":run")
				if err != nil || !log.Contains("runtime scope placement: 42") {
					log.Dump()
					it.Fail("publishing the disconnected relocated module through %s prevented the entry from running", publication)
				}
				// A live deps owner that produces the module holds its mapping; data publishes no mapping (ts-compile.md, Sources).
				if publication == "data" {
					it.RequireContains(it.Bin(outputPkg, "package.json"), `"#unpublished":"./unpublished.ts"`, "a module published only through data must not rewrite an authored target in the entry's scope")
				} else {
					it.RequireContains(it.Bin(outputPkg, "package.json"), `"#unpublished":"./unpublished.js"`, "the entry's scope must follow the module mapping its dependency's producer holds")
				}
			}
			it.Write(build, strings.Replace(original, "emit = True", "emit = False", 1))
			it.MustBazel("build", target, "--output_groups=+_validation")
			outputs := strings.Fields(it.BazelStdout("cquery", target, "--output=files"))
			if !slices.Contains(outputs, sourcePkg+"/package.json") || strings.TrimSpace(it.Read(it.Path(sourcePkg, "package.json"))) != manifest {
				it.Fail("source-mode scope no longer publishes its authored File and TypeScript targets")
			}
		}
	}
	func() {
		sourcePkg, consumerPkg := base+"/copied_scope", base+"/copied_scope_consumer"
		const manifest = `{"type":"module","imports":{"#value":"./value.js"}}`
		it.Write(it.Path(sourcePkg, "package.json"), manifest+"\n")
		it.Write(it.Path(sourcePkg, "conflict/package.json"), manifest+"\n")
		it.Write(it.Path(sourcePkg, "main.ts"), "import { value } from '#value';\nif (value !== 42) throw new Error('copied scope resolved another module');\nconsole.log('copied runtime scope: ' + value);\n")
		it.Write(it.Path(sourcePkg, "value.ts"), "export const value: number = 42;\n")
		it.Write(it.Path(sourcePkg, "selected_scope.bzl"), `load("@rules_typescript//ts/private:providers.bzl", "TsInfo")
def _impl(ctx):
    info = ctx.attr.dep[TsInfo]
    scopes = [runtime for owner in info.owners.to_list() for source, runtime in owner.runtime_scopes if source == ctx.file.scope]
    if len(scopes) != 1 or scopes[0] == ctx.file.scope:
        fail("the data fixture needs one producer-owned scope projection")
    fields = {field: getattr(info, field) for field in dir(info) if field not in ["to_json", "to_proto"]}
    fields["data"] = depset()
    fields["transitive_data"] = depset(order = "postorder")
    if ctx.file.conflict:
        fields["owners"] = depset([struct(
            label = str(ctx.label),
            files = depset(),
            declarations = depset(),
            type_inputs = depset(),
            runtime_files = (),
            runtime_scopes = ((ctx.file.conflict, scopes[0]),),
            importers = (),
        )], transitive = [info.owners])
    return [TsInfo(**fields), DefaultInfo(files = depset(scopes))]
selected_scope = rule(implementation = _impl, attrs = {
    "dep": attr.label(providers = [TsInfo]),
    "scope": attr.label(allow_single_file = True),
    "conflict": attr.label(allow_single_file = True),
})
`)
		build := it.Path(sourcePkg, "BUILD.bazel")
		const scopeBuild = `load("@rules_typescript//ts:defs.bzl", "ts_compile")
load(":selected_scope.bzl", "selected_scope")
exports_files(["main.ts", "value.ts"], visibility = ["//visibility:public"])
ts_compile(name = "scope", srcs = [], package_scopes = ["package.json"])
selected_scope(name = "selected", dep = ":scope", scope = "package.json", visibility = ["//visibility:public"])
`
		it.Write(build, scopeBuild)
		it.Write(it.Path(consumerPkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","target":"es2022","types":[]},"files":["../copied_scope/main.ts","../copied_scope/value.ts"]}`)
		it.Write(it.Path(consumerPkg, "BUILD.bazel"), fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(name = "compiled", srcs = [%q, %q], data = [%q], deps = [%q], emit = True, tsconfig = "tsconfig.json")
ts_binary(name = "run", entry_point = ":compiled", entry_file = "main.ts")
`, "//"+sourcePkg+":main.ts", "//"+sourcePkg+":value.ts", "//"+sourcePkg+":selected", "//"+sourcePkg+":selected"))
		log, err := it.BazelLog("copied_data_scope_reaches_downstream_binary", "run", "//"+consumerPkg+":run")
		if err != nil || !log.Contains("copied runtime scope: 42") {
			log.Dump()
			it.Fail("a scope copied through data lost its original File identity at the downstream binary")
		}
		if got := strings.TrimSpace(it.Read(it.Bin(consumerPkg, "copied_scope", "package.json"))); got != manifest {
			it.Fail("the foreign data stage changed the selected runtime scope: %s", got)
		}
		it.Write(build, strings.Replace(scopeBuild, `dep = ":scope"`, `dep = ":scope", conflict = "conflict/package.json"`, 1))
		log, err = it.BazelLog("copied_data_scope_rejects_distinct_origin", "build", "//"+consumerPkg+":compiled")
		if err == nil || !log.Contains("already occupied by '"+sourcePkg+"/conflict/package.json'") {
			log.Dump()
			it.Fail("copying the selected data scope discarded a distinct declared origin with equal contents")
		}
		it.Write(build, scopeBuild)
	}()
	func() {
		manifestPath := it.Path("packages/shared/package.json")
		memberBuildPath := it.Path("packages/shared/BUILD.bazel")
		memberSource := it.Path("packages/shared/src/wire/index.ts")
		consumerBuildPath := it.Path("member/BUILD.bazel")
		consumerSource := it.Path("member/url_consumer.ts")
		defer it.Write(manifestPath, it.Read(manifestPath))
		defer it.Write(memberBuildPath, it.Read(memberBuildPath))
		defer it.Write(memberSource, it.Read(memberSource))
		defer it.Write(consumerBuildPath, it.Read(consumerBuildPath))
		it.RequireNoFile(consumerSource, "the URL consumer must be staged by this check")
		defer os.Remove(consumerSource)
		var manifest map[string]any
		if err := json.Unmarshal([]byte(it.Read(manifestPath)), &manifest); err != nil {
			it.Fail("cannot load the workspace member manifest: %v", err)
		}
		exports, ok := manifest["exports"].(map[string]any)
		if !ok {
			it.Fail("workspace member exports are not a subpath map")
		}
		exports["./query"] = "./src/wire/index.ts?instance=one#first"
		exports["./alias"] = "./src/wire/%69ndex.ts?instance=one#first"
		exports["./other-query"] = "./src/wire/index.ts?instance=two#first"
		exports["./other-fragment"] = "./src/wire/index.ts?instance=one#second"
		encoded, err := json.Marshal(manifest)
		if err != nil {
			it.Fail("cannot write the workspace member URL targets: %v", err)
		}
		it.Write(manifestPath, string(encoded)+"\n")
		it.Write(memberSource, it.Read(memberSource)+"\ndeclare global { interface ImportMeta { url: string; } }\nexport const moduleURL: string = import.meta.url;\n")
		memberBuild, err := rule.LoadFile(memberBuildPath, "packages/shared")
		if err != nil {
			it.Fail("cannot load the workspace member compiler: %v", err)
		}
		var compiler *rule.Rule
		for _, target := range memberBuild.Rules {
			if target.Kind() == "ts_compile" && target.Name() == "shared" {
				compiler = target
			}
		}
		if compiler == nil {
			it.Fail("workspace member compiler is missing")
		}
		compiler.SetAttr("emit", true)
		it.Write(memberBuildPath, string(memberBuild.Format()))
		consumerBuild, err := rule.LoadFile(consumerBuildPath, "member")
		if err != nil {
			it.Fail("cannot load the workspace package consumer: %v", err)
		}
		var definitions *rule.Load
		for _, load := range consumerBuild.Loads {
			if load.Name() == "@rules_typescript//ts:defs.bzl" {
				definitions = load
			}
		}
		if definitions == nil {
			it.Fail("workspace package consumer lost its TypeScript definitions load")
		}
		definitions.Add("ts_compile")
		definitions.Add("ts_binary")
		it.Write(consumerBuildPath, string(consumerBuild.Format())+`
ts_compile(name = "url_consumer", srcs = ["url_consumer.ts"], deps = ["//:node_modules/shared"], node_modules = "//:node_modules", emit = True, tsconfig = ":tsconfig")
ts_binary(name = "url_run", entry_point = ":url_consumer", entry_file = "url_consumer.ts")
`)
		it.Write(consumerSource, urlIdentity+`
await assertURLIdentity(['shared/query', 'shared/alias'], 'shared/other-query', 'shared/other-fragment', '/src/wire/index.js');
console.log('workspace package URL identity retained');
export {};
`)
		for _, backend := range []string{"tsgo", "oxc"} {
			if got := strings.TrimSpace(it.BazelStdout("run", "//member:url_run", "--@rules_typescript//ts:declarations="+backend)); got != "workspace package URL identity retained" {
				it.Fail("%s changed npm publication URL identity: %q", backend, got)
			}
		}
	}()
	it.Pass("direct and supplied scopes preserve private imports inside the compiler's emitted namespace; .ts private imports and self exports retain emitted module identity; absent package format preserves CommonJS; source-mode scopes retain authored targets; stale owner pairs leave unpublished targets authored; unpublished relocated pairs preserve metadata roles; disconnected published modules do not prevent the entry from running")
}

func borrowedHelpersDoNotBecomeTestEntries(it *harness.IT) {
	const base = "execution_roots"
	it.RequireNoDir(it.Path(base), "execution-root regression must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	defer it.Write(it.Path("BUILD.bazel"), it.Read(it.Path("BUILD.bazel")))
	defer it.Write(it.Path("packages/shared/BUILD.bazel"), it.Read(it.Path("packages/shared/BUILD.bazel")))
	// The partial update below visits none of the packages extending the root tsconfig, so it would withdraw //:tsconfig.
	root, err := rule.LoadFile(it.Path("BUILD.bazel"), "")
	if err != nil {
		it.Fail("cannot load the root BUILD file: %v", err)
	}
	for _, target := range root.Rules {
		if target.Kind() == "ts_config" && target.Name() == "tsconfig" {
			target.AddComment("# keep")
		}
	}
	it.Write(it.Path("BUILD.bazel"), string(root.Format()))
	it.Write(it.Path(base, "kept/BUILD.bazel"), `exports_files(["vitest.test.ts", "node.test.ts", "__snapshots__/vitest.test.ts.snap"], visibility = ["//visibility:public"])`)
	it.Write(it.Path(base, "kept/__snapshots__/vitest.test.ts.snap"), "// Vitest Snapshot v1, https://vitest.dev/guide/snapshot.html\n\nexports[`executes the explicitly kept foreign test at its imported module path 1`] = `42`;\n")
	it.Write(it.Path(base, "support/BUILD.bazel"), `exports_files(["helper.ts", "other.ts"], visibility = ["//visibility:public"])`)
	it.Write(it.Path(base, "support/other.ts"), "export const answer = 42;\n")
	nodeRunner := strings.TrimSpace(it.BazelStdout("cquery", "@rules_typescript//ts/runners:node_test", "--output=starlark", "--starlark:expr=str(target.label)"))
	if !strings.HasPrefix(nodeRunner, "@@") || strings.HasPrefix(nodeRunner, "@@//") {
		it.Fail("Node runner did not resolve to a canonical external label: %q", nodeRunner)
	}
	it.Write(it.Path(base, "runners/BUILD.bazel"), fmt.Sprintf(`alias(name = "node", actual = %q, visibility = ["//visibility:public"])`, nodeRunner))
	const guard = `if (process.argv[1]?.endsWith('/helper.js') || process.argv[1]?.endsWith('/other.js')) {
  throw new Error('a retained helper was launched as an independent test');
}
`
	for _, runner := range []string{"vitest", "node"} {
		pkg := base + "/" + runner
		attrs := "    shard_count = 2,\n    srcs = [\n        \"index.test.ts\",\n        \"//" + base + "/kept:" + runner + ".test.ts\",  # keep\n    ],\n"
		if runner == "node" {
			attrs += "    runner = \"//" + base + "/runners:node\",\n"
		} else {
			attrs += "    emit = True,  # keep\n    data = [\"//" + base + "/kept:__snapshots__/vitest.test.ts.snap\"],\n"
			it.Write(it.Path(pkg, "vitest.config.mjs"), "export default { root: '..', test: { passWithNoTests: false } };\n")
		}
		it.Write(it.Path(pkg, "BUILD.bazel"), "load(\"@rules_typescript//ts:defs.bzl\", \"ts_test\")\nts_test(\n    name = \""+runner+"_test\",\n"+attrs+")\n")
		it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":["node"]},"files":["index.test.ts"]}`)
		imports := "import { it } from 'node:test';\nimport assert from 'node:assert/strict';\n"
		if runner == "vitest" {
			imports = "import { it, assert, expect } from 'vitest';\n"
		}
		it.Write(it.Path(pkg, "index.test.ts"), imports+`import { answer } from '../support/helper';
it('executes the real test with its borrowed helper available', () => {
  assert.equal(answer, 42);
  assert.equal(process.env.TEST_SHARD_INDEX, '1');
  console.log('local execution root ran');
  assert.equal(process.env.TEST_TOTAL_SHARDS, '2');
});
`)
		pathCheck := "  assert.match(import.meta.url, /\\/execution_roots\\/node\\/kept\\/node\\.test\\.js$/);\n"
		if runner == "vitest" {
			pathCheck = "  assert.match(import.meta.url, /\\/execution_roots\\/vitest\\/kept\\/vitest\\.test\\.js$/);\n  expect(answer).toMatchSnapshot();\n"
		}
		it.Write(it.Path(base, "kept", runner+".test.ts"), imports+`import { answer } from '../support/helper';
it('executes the explicitly kept foreign test at its imported module path', () => {
  assert.equal(answer, 42);
`+pathCheck+`
  assert.equal(process.env.TEST_SHARD_INDEX, '0');
  assert.equal(process.env.TEST_TOTAL_SHARDS, '2');
  console.log('kept foreign execution root ran');
});
`)
	}
	for state, helper := range []string{"export const answer = 42;\n", "export { answer } from './other';\n"} {
		it.Write(it.Path(base, "support/helper.ts"), guard+helper)
		it.Write(it.Path(base, "support/other.ts"), guard+"export const answer = 42;\n")
		// The node test reaches the root manifest's member, which Gazelle promotes only inside the update.
		it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", ".", "packages/shared", base+"/node", base+"/vitest")
		for _, runner := range []string{"vitest", "node"} {
			target := "//" + base + "/" + runner + ":" + runner + "_test"
			requireLabels(it, "test_srcs", target, []string{"//" + base + "/kept:" + runner + ".test.ts", "//" + base + "/" + runner + ":index.test.ts"})
			want := []string{"//" + base + "/kept:" + runner + ".test.ts", "//" + base + "/" + runner + ":index.test.ts", "//" + base + "/support:helper.ts"}
			if strings.Contains(helper, "./other") {
				want = append(want, "//"+base+"/support:other.ts")
			}
			sort.Strings(want)
			requireLabels(it, "srcs", target, want)
			log, err := it.BazelLog(fmt.Sprintf("kept_execution_roots_%s_%d", runner, state), "test", target, "--test_output=all")
			if err != nil || !log.Contains("local execution root ran") || !log.Contains("kept foreign execution root ran") {
				log.Dump()
				it.Fail("%s did not execute both effective roots with borrowed helpers available", runner)
			}
		}
		if owners := strings.TrimSpace(it.BazelStdout("query", `kind("ts_compile rule", //`+base+`/...)`)); owners != "" {
			it.Fail("borrowed test helpers acquired artificial compiler owners: %s", owners)
		}
	}
	it.Write(it.Path(base, "vitest/vitest.config.mjs"), "export default { test: { passWithNoTests: false } };\n")
	for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
		log, err := it.BazelLog("default_foreign_execution_roots"+runfiles, "test", "//"+base+"/vitest:vitest_test", "--test_output=all", runfiles)
		if err != nil || !log.Contains("local execution root ran") || !log.Contains("kept foreign execution root ran") {
			log.Dump()
			it.Fail("Vitest's default root lost a declared foreign test with %s", runfiles)
		}
	}
	func() {
		for _, path := range []string{"kept/BUILD.bazel", "support/helper.ts", "support/BUILD.bazel", "vitest/BUILD.bazel", "vitest/index.test.ts"} {
			defer it.Write(it.Path(base, path), it.Read(it.Path(base, path)))
		}
		defer os.Remove(it.Path(base, "kept/commonjs.ts"))
		defer os.Remove(it.Path(base, "support/tsconfig.commonjs.json"))
		it.Write(it.Path(base, "kept/BUILD.bazel"), it.Read(it.Path(base, "kept/BUILD.bazel"))+"\nexports_files([\"commonjs.ts\"], visibility = [\"//visibility:public\"])\n")
		it.Write(it.Path(base, "kept/commonjs.ts"), "export const identity: object = {};\n")
		it.Write(it.Path(base, "support/helper.ts"), `import { expect } from 'vitest';
import { identity } from '../kept/commonjs';
expect(identity).toBe(identity);
export { identity };
export const answer: number = 42;
`)
		it.Write(it.Path(base, "support/tsconfig.commonjs.json"), `{"compilerOptions":{"module":"commonjs"}}`)
		it.Write(it.Path(base, "support/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_config")
exports_files(["helper.ts", "other.ts"], visibility = ["//visibility:public"])
ts_config(name = "commonjs_config", src = "tsconfig.commonjs.json", module = "commonjs")
ts_compile(name = "commonjs", srcs = ["helper.ts", "//`+base+`/kept:commonjs.ts"], emit = True, tsconfig = ":commonjs_config", node_modules = "//:node_modules", deps = ["@npm//:vitest"], visibility = ["//visibility:public"])
`)
		vitestBuild, err := rule.LoadFile(it.Path(base, "vitest/BUILD.bazel"), base+"/vitest")
		if err != nil {
			it.Fail("cannot load the mixed-root Vitest consumer: %v", err)
		}
		configured := false
		for _, target := range vitestBuild.Rules {
			if target.Kind() == "ts_test" && target.Name() == "vitest_test" {
				configured = true
				target.SetAttr("srcs", []string{"index.test.ts", "//" + base + "/kept:vitest.test.ts"})
				target.SetAttr("deps", append(target.AttrStrings("deps"), "//"+base+"/support:commonjs"))
			}
		}
		if !configured {
			it.Fail("the mixed-root Vitest consumer target is missing")
		}
		it.Write(it.Path(base, "vitest/BUILD.bazel"), string(vitestBuild.Format()))
		it.Write(it.Path(base, "vitest/index.test.ts"), it.Read(it.Path(base, "vitest/index.test.ts"))+`
import { identity } from '../support/helper';
import { identity as directIdentity } from '../kept/commonjs';
it('canonical CommonJS dependency links execute one ES twin identity', () => {
  assert.strictEqual(identity, directIdentity);
  console.log('moved CommonJS dependency retained one ES twin identity');
});
`)
		commonJSLog, commonJSErr := it.BazelLog("mixed_commonjs_vitest_identity", "test", "//"+base+"/vitest:vitest_test", "--test_output=all")
		if commonJSErr != nil || !commonJSLog.Contains("moved CommonJS dependency retained one ES twin identity") || !commonJSLog.Contains("kept foreign execution root ran") {
			commonJSLog.Dump()
			it.Fail("the emitted Vitest consumer lost a moved CommonJS dependency's ES twin or module identity")
		}
		it.RequireFile(it.Bin(base, "support/commonjs.es/support/helper.js"), "the moved dependency's ES twin was not emitted")
	}()
	it.Write(it.Path(base, "node/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":["node"],"allowJs":true},"files":["index.test.ts"]}`)
	it.Write(it.Path(base, "support/BUILD.bazel"), `exports_files(["helper.ts", "next.ts", "leaf.ts", "npm.cjs", "package.json"], visibility = ["//visibility:public"])`)
	it.Write(it.Path(base, "support/package.json"), `{"name":"borrowed-helper","type":"module","imports":{"#next":"./next.js","#leaf":"./leaf.js","#sourceLeaf":"./leaf.ts"},"exports":{"./leaf":"./leaf.js"}}`)
	it.Write(it.Path(base, "support/helper.ts"), `import { identity } from '#next';
export const answer = 42;
export { identity };
export async function dynamicIdentity() {
  const privateName: string = '#leaf';
  const selfName: string = 'borrowed-helper/leaf';
  const sourceName: string = '#sourceLeaf';
  return [(await import(privateName)).identity, (await import(selfName)).identity, (await import(sourceName)).identity];
}
`)
	it.Write(it.Path(base, "support/next.ts"), `import { identity } from '#leaf';
import { identity as selfIdentity } from 'borrowed-helper/leaf';
if (identity !== selfIdentity) throw new Error('private and self imports duplicated a borrowed module');
export { identity };
`)
	it.Write(it.Path(base, "support/leaf.ts"), "export const identity = {};\n")
	it.Write(it.Path(base, "kept/identity-marker.ts"), "export const marker: number = 1;\n")
	it.Write(it.Path(base, "kept/BUILD.bazel"), it.Read(it.Path(base, "kept/BUILD.bazel"))+"\nexports_files([\"identity-marker.ts\"], visibility = [\"//visibility:public\"])\n")
	it.Write(it.Path(base, "support/BUILD.bazel"), loadTsCompile+it.Read(it.Path(base, "support/BUILD.bazel"))+`
# keep
ts_compile(
    name = "canonical",
    srcs = ["leaf.ts", "//`+base+`/kept:identity-marker.ts"],
    package_scopes = ["package.json"],
    emit = True,
    visibility = ["//visibility:public"],
)
`)
	it.Write(it.Path(base, "support/npm.cjs"), "module.exports = { npm: require('culori'), path: require.resolve('culori'), cached: require.cache[require.resolve('culori')] };\n")
	it.Write(it.Path(base, "node/index.test.ts"), it.Read(it.Path(base, "node/index.test.ts"))+`
import { createRequire } from 'node:module';
import { realpathSync } from 'node:fs';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { identity, dynamicIdentity } from '../support/helper.js';
import { identity as relativeIdentity } from '../support/leaf.js';
import commonJS from '../support/npm.cjs';
it('keeps private, self, dynamic and require imports at one borrowed module identity', async () => {
  assert.strictEqual(identity, relativeIdentity);
  const canonicalPath = realpathSync(fileURLToPath(new URL('../support/leaf.js', import.meta.url)));
  assert.ok(canonicalPath.endsWith('/support/support/leaf.js'));
  assert.strictEqual((await import(pathToFileURL(canonicalPath).href)).identity, identity);
  for (const dynamic of await dynamicIdentity()) assert.strictEqual(dynamic, identity);
  const require = createRequire(new URL('../support/helper.js', import.meta.url));
  assert.strictEqual(require('#leaf').identity, identity);
  assert.strictEqual(require('#sourceLeaf').identity, identity);
  assert.strictEqual(require('borrowed-helper/leaf').identity, identity);
  assert.strictEqual(require('./leaf.js').identity, identity);
  const npmPath = require.resolve('culori');
  assert.equal(npmPath, realpathSync(npmPath));
  const npm = require('culori');
  assert.strictEqual(commonJS.npm, npm);
  assert.equal(commonJS.path, npmPath);
  assert.strictEqual(require.cache[npmPath]?.exports, npm);
  assert.strictEqual(commonJS.cached, require.cache[npmPath]);
  const fromNpm = createRequire(npmPath);
  assert.equal(fromNpm.resolve(npmPath), npmPath);
  assert.strictEqual(fromNpm(npmPath), npm);
  assert.strictEqual(fromNpm.cache[npmPath], require.cache[npmPath]);
  assert.strictEqual(require(realpathSync(npmPath)), npm);
  assert.strictEqual((await import(pathToFileURL(npmPath).href)).default, npm);
  const npmURL = import.meta.resolve('culori');
  assert.equal(fileURLToPath(npmURL), realpathSync(fileURLToPath(npmURL)));
  assert.strictEqual(await import('culori'), await import(npmURL));
  console.log('borrowed and npm module identities survived every resolver form');
});
`)
	it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", ".", base+"/node")
	for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
		log, err := it.BazelLog("borrowed_private_module_identity"+runfiles, "test", "//"+base+"/node:node_test", "--test_output=all", runfiles)
		if err != nil || !log.Contains("borrowed and npm module identities survived every resolver form") || !log.Contains("local execution root ran") || !log.Contains("kept foreign execution root ran") {
			log.Dump()
			it.Fail("%s lost a borrowed execution root, package scope or module identity", runfiles)
		}
	}
	it.Write(it.Path(base, "runners/direct.bzl"), `load("@rules_typescript//ts:defs.bzl", "TsTestRunnerInfo")

def _launch(ctx, test):
    entry = [file for file in test.entry_points if file.basename == "node.test.js"][0]
    return struct(
        mode = "node",
        section = {"entry": ctx.workspace_name + "/" + entry.short_path},
        env = dict(ctx.attr.env),
        files = [],
        symlinks = {},
        transitive_files = depset(transitive = [test.transitive_js] + test.runtime_data_sets),
        output_groups = {},
    )

def _impl(ctx):
    return [TsTestRunnerInfo(packages = [], hook = None, supports_source_inputs = False, es_modules = False, launch = _launch)]

direct_runner = rule(implementation = _impl)

def _custom_node_launch(ctx, test):
    section = {"test_files_list": ctx.workspace_name + "/" + test.test_files_list.short_path}
    if test.runner.hook:
        section["resolve_hook"] = ctx.workspace_name + "/" + test.runner.hook.short_path
    return struct(
        mode = "node_test",
        section = section,
        env = dict(ctx.attr.env),
        files = [test.runner.hook] if test.runner.hook else [],
        symlinks = {},
        transitive_files = depset(transitive = [test.transitive_js] + test.runtime_data_sets),
        output_groups = {},
    )

def _custom_node_impl(ctx):
    return [TsTestRunnerInfo(packages = [], hook = ctx.file.hook, supports_source_inputs = False, es_modules = False, launch = _custom_node_launch)]

custom_node_runner = rule(implementation = _custom_node_impl, attrs = {"hook": attr.label(allow_single_file = True)})
`)
	it.Write(it.Path(base, "runners/BUILD.bazel"), "load(\":direct.bzl\", \"direct_runner\", \"custom_node_runner\")\n"+it.Read(it.Path(base, "runners/BUILD.bazel"))+"\ndirect_runner(name = \"direct\", visibility = [\"//visibility:public\"])\ncustom_node_runner(name = \"hookless\", visibility = [\"//visibility:public\"])\ncustom_node_runner(name = \"custom_hook\", hook = \"noop.mjs\", visibility = [\"//visibility:public\"])\n")
	it.Write(it.Path(base, "runners/noop.mjs"), "export {};\n")
	it.Write(it.Path(base, "node/index.test.ts"), `import { it } from 'node:test';
import assert from 'node:assert/strict';
import { answer, identity } from '../support/helper.js';
import { identity as relativeIdentity } from '../support/leaf.js';
it('retains borrowed private and self imports at one module identity', () => {
  assert.equal(answer, 42);
  assert.strictEqual(identity, relativeIdentity);
  assert.equal(process.env.TEST_SHARD_INDEX, '1');
  assert.equal(process.env.TEST_TOTAL_SHARDS, '2');
  console.log('custom Node runner retained borrowed module scope');
});
`)
	it.Write(it.Path(base, "kept/node.test.ts"), strings.ReplaceAll(it.Read(it.Path(base, "kept/node.test.ts")), "'../support/helper'", "'../support/helper.js'"))
	nodeBuild := it.Read(it.Path(base, "node/BUILD.bazel"))
	it.Write(it.Path(base, "node/BUILD.bazel"), strings.ReplaceAll(nodeBuild, "//"+base+"/runners:node", "//"+base+"/runners:direct"))
	directLog, directErr := it.BazelLog("custom_runner_output_scope", "test", "//"+base+"/node:node_test", "--test_output=all")
	// The moved marker's scope sits where ts_test staged it, a coordinate compiler-output entries do not admit.
	if directErr == nil || !directLog.Contains("is shadowed by runtime manifest") {
		directLog.Dump()
		it.Fail("a custom runner using compiler-output entries bypassed runtime scope admission")
	}
	for _, runner := range []string{"hookless", "custom_hook"} {
		it.Write(it.Path(base, "node/BUILD.bazel"), strings.ReplaceAll(nodeBuild, "//"+base+"/runners:node", "//"+base+"/runners:"+runner))
		for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
			log, err := it.BazelLog(runner+"_borrowed_package_scope"+runfiles, "test", "//"+base+"/node:node_test", "--test_output=all", runfiles)
			if err != nil || !log.Contains("custom Node runner retained borrowed module scope") || !log.Contains("kept foreign execution root ran") {
				log.Dump()
				it.Fail("%s with %s lost a borrowed execution root or package scope", runner, runfiles)
			}
		}
	}
	it.Write(it.Path(base, "node/BUILD.bazel"), nodeBuild)
	it.Write(it.Path(base, "explicit/entry.ts"), "import { it } from 'node:test';\nit('arbitrary root executed', () => {});\n")
	it.Write(it.Path(base, "explicit/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_test")
ts_test(
    name = "arbitrary_test",
    srcs = ["entry.ts"],
    test_srcs = [],
    emit = True,
    runner = "@rules_typescript//ts/runners:node_test",
    node_modules = "//:node_modules",
    deps = ["@npm//:types_node"],
)
`)
	log, err := it.BazelLog("arbitrary_node_test_root", "test", "//"+base+"/explicit:arbitrary_test", "--test_output=all")
	if err != nil || !log.Contains("arbitrary root executed") {
		log.Dump()
		it.Fail("the default node:test root contract failed to execute an arbitrary source filename")
	}
	explicitBuild := it.Read(it.Path(base, "explicit/BUILD.bazel"))
	it.Write(it.Path(base, "explicit/BUILD.bazel"), strings.ReplaceAll(explicitBuild, "@rules_typescript//ts/runners:node_test", "//"+base+"/runners:hookless"))
	for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
		log, err := it.BazelLog("hookless_local_root"+runfiles, "test", "//"+base+"/explicit:arbitrary_test", "--test_output=all", runfiles)
		if err != nil || !log.Contains("arbitrary root executed") {
			log.Dump()
			it.Fail("%s prevented a hookless callback from executing its supported local root", runfiles)
		}
	}
	it.Pass("borrowed helper closure cannot change Vitest shards or node:test execution roots")
	it.Pass("kept foreign tests remain execution roots for Vitest and node:test")
}

func runnerModuleAliasesPreserveIdentity(it *harness.IT) {
	const base = "runtime_runner_alias"
	it.RequireNoDir(it.Path(base), "the runner alias regression must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	it.Write(it.Path(base, "BUILD.bazel"), "exports_files([\"runner.bzl\"])\n")
	it.Write(it.Path(base, "runner.bzl"), `load("@rules_typescript//ts:defs.bzl", "TsTestRunnerInfo")

def _runner(ctx):
    original = ctx.attr.original[TsTestRunnerInfo]
    def launch(test_ctx, test):
        launched = original.launch(test_ctx, test)
        module = [runtime for source, runtime in test.runtime_files if source.basename == "singleton.mjs"][0]
        alias = module.short_path.rpartition("/")[0] + "/alias.mjs"
        return struct(
            mode = launched.mode,
            section = launched.section,
            env = launched.env,
            files = launched.files,
            symlinks = launched.symlinks | {alias: module},
            transitive_files = launched.transitive_files,
            output_groups = launched.output_groups,
            replacements = getattr(launched, "replacements", {}),
        )
    return [TsTestRunnerInfo(packages = original.packages, hook = original.hook, supports_source_inputs = original.supports_source_inputs, es_modules = original.es_modules, launch = launch)]

alias_runner = rule(implementation = _runner, attrs = {"original": attr.label(providers = [TsTestRunnerInfo])})
`)
	for _, runner := range []struct{ name, module string }{{"node_test", "node:test"}, {"vitest", "vitest"}} {
		pkg := base + "/" + runner.name
		it.Write(it.Path(pkg, "package.json"), `{"type":"module","imports":{"#scope":"./scope.mjs"}}`)
		it.Write(it.Path(pkg, "scope.mjs"), "export const scope = 'declared package scope';\n")
		it.Write(it.Path(pkg, "fixture.txt"), "declared adjacent asset")
		it.Write(it.Path(pkg, "singleton.mjs"), `import * as npm from 'culori';
import { readFileSync } from 'node:fs';
import { scope } from '#scope';
const key = Symbol.for('rules_typescript.runner_alias_evaluations');
globalThis[key] = (globalThis[key] ?? 0) + 1;
export const identity = {};
export const evaluations = () => globalThis[key];
export { npm, scope };
export const asset = readFileSync(new URL('./fixture.txt', import.meta.url), 'utf8');
`)
		it.Write(it.Path(pkg, "alias.test.mjs"), `import { it } from '`+runner.module+`';
import assert from 'node:assert/strict';
import * as canonical from './singleton.mjs';
it('runner aliases cannot create a second module evaluation', async () => {
  const alias = await import(new URL('./alias.mjs', import.meta.url).href);
  assert.strictEqual(alias.identity, canonical.identity, 'runner alias created a second singleton');
  assert.equal(canonical.evaluations(), 1, 'runner alias evaluated the module twice');
  assert.strictEqual(alias.npm, canonical.npm);
  assert.equal(alias.scope, 'declared package scope');
  assert.equal(alias.asset, 'declared adjacent asset');
  console.log('runner alias retained one evaluation and its owner context');
});
`)
		it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":["node"],"allowJs":true},"files":["alias.test.mjs","singleton.mjs","scope.mjs"]}`)
		it.Write(it.Path(pkg, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_test")
load("//`+base+`:runner.bzl", "alias_runner")
alias_runner(name = "runner", original = "@rules_typescript//ts/runners:`+runner.name+`")
ts_test(
    name = "probe",
    srcs = ["alias.test.mjs", "singleton.mjs", "scope.mjs"],
    test_srcs = ["alias.test.mjs"],
    package_scopes = ["package.json"],
    data = ["fixture.txt"],
    emit = True,
    tsconfig = "tsconfig.json",
    runner = ":runner",
    node_modules = "//:node_modules",
    deps = ["@npm//:culori", "@npm//:types_node", "@npm//:vitest"],
)
`)
		for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
			log, err := it.BazelLog("runner_alias_"+runner.name+runfiles, "test", "//"+pkg+":probe", "--test_output=all", runfiles)
			if err != nil || !log.Contains("runner alias retained one evaluation and its owner context") {
				log.Dump()
				it.Fail("%s with %s lost runner alias module identity or its owner context", runner.name, runfiles)
			}
		}
	}
	it.Pass("custom runner aliases preserve one module evaluation, package scope, npm identity and adjacent assets")
}

func vitestConfigReferencesRetainRuntimeIdentity(it *harness.IT) {
	const base = "runtime_config_projection"
	it.RequireNoDir(it.Path(base), "the config-reference fixture must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	it.Write(it.Path(base, "borrowed/BUILD.bazel"), "exports_files([\"anchor.ts\"], visibility = [\"//visibility:public\"])\n")
	it.Write(it.Path(base, "borrowed/anchor.ts"), "export const anchor: number = 1;\n")
	it.Write(it.Path(base, "producer/helper.ts"), "import { anchor } from '../borrowed/anchor.js';\nexport const marker: number = anchor + 41;\n")
	it.Write(it.Path(base, "producer/setup.ts"), `import { beforeAll } from 'vitest';
beforeAll(() => { (globalThis as { producerSetup?: string }).producerSetup = 'selected ES twin'; });
`)
	it.Write(it.Path(base, "producer/tsconfig.json"), `{"compilerOptions":{"module":"commonjs"}}`)
	it.Write(it.Path(base, "producer/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_config")
ts_config(name = "config", src = "tsconfig.json", module = "commonjs")
ts_compile(
    name = "producer",
    srcs = ["helper.ts", "setup.ts", "//`+base+`/borrowed:anchor.ts"],
    emit = True,
    tsconfig = ":config",
    node_modules = "//:node_modules",
    deps = ["@npm//:vitest"],
    visibility = ["//visibility:public"],
)
`)
	for _, mode := range []struct{ name, emit string }{{"emitted", "True"}, {"source", "False"}} {
		pkg := base + "/" + mode.name
		it.Write(it.Path(pkg, "singleton.ts"), `export const state: { setup: string } = { setup: 'pending' };
export const moduleURL = import.meta.url;
`)
		it.Write(it.Path(pkg, "lib/index.ts"), "export { state, moduleURL } from '../singleton.js';\n")
		it.Write(it.Path(pkg, "setup.ts"), "import { state } from './singleton.js';\nstate.setup = 'selected runtime';\n")
		it.Write(it.Path(pkg, "selected/global.mjs"), `import { moduleURL } from '../singleton.js';
export default ({ provide }) => { provide('moduleURL', moduleURL); };
`)
		it.Write(it.Path(pkg, "raw.d.ts"), "declare module '@raw' { const source: string; export default source; }\n")
		it.Write(it.Path(pkg, "selected/alias.test.ts"), `import { expect, inject, it } from 'vitest';
import { state as exact } from '#exact';
import { state as extensionless } from '@shared/singleton';
import { state as index } from '@index';
import { state, moduleURL } from '../singleton.js';
import { marker } from '../../producer/helper.js';
import raw from '@raw';
declare module 'vitest' { export interface ProvidedContext { moduleURL: string; } }
it('config module references cannot select a second source identity', () => {
  expect(exact).toBe(state);
  expect(extensionless).toBe(state);
  expect(index).toBe(state);
  expect(state.setup).toBe('selected runtime');
  expect(inject('moduleURL')).toBe(moduleURL);
  expect((globalThis as { producerSetup?: string }).producerSetup).toBe('selected ES twin');
  expect(marker).toBe(42);
  expect(raw).toContain('state: { setup: string }');
  expect(import.meta.url).toContain('/runtime_config_projection/`+mode.name+`/`+mode.name+`/selected/alias.test.');
  expect(state.setup).toMatchSnapshot();
  console.log('config references retained canonical runtime identity');
});
`)
		it.Write(it.Path(pkg, "selected/__snapshots__/alias.test.ts.snap"), "// Vitest Snapshot v1, https://vitest.dev/guide/snapshot.html\n\nexports[`config module references cannot select a second source identity 1`] = `\"selected runtime\"`;\n")
		for _, excluded := range []string{"sibling.test.ts", "selected/excluded/never.test.ts", "selected/named.test.ts"} {
			it.Write(it.Path(pkg, excluded), "export {};\nthrow new Error('source root or exclusion lost its coordinates: "+excluded+"');\n")
		}
		it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"paths":{"#exact":["./absent.ts","./singleton.ts"],"@shared/*":["./*"],"@index":["./lib"],"@raw":["./singleton.ts?raw"]}},"files":["selected/alias.test.ts","sibling.test.ts","selected/excluded/never.test.ts","selected/named.test.ts","singleton.ts","lib/index.ts","setup.ts","selected/global.mjs","raw.d.ts"]}`)
		it.Write(it.Path(pkg, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_test")
ts_test(
    name = "probe",
    srcs = ["selected/alias.test.ts", "sibling.test.ts", "selected/excluded/never.test.ts", "selected/named.test.ts", "singleton.ts", "lib/index.ts", "setup.ts", "selected/global.mjs", "raw.d.ts"],
    test_srcs = ["selected/alias.test.ts", "sibling.test.ts", "selected/excluded/never.test.ts", "selected/named.test.ts"],
    data = ["singleton.ts", "selected/__snapshots__/alias.test.ts.snap"],
    config = "vitest.config.mjs",
    tsconfig = "tsconfig.json",
    emit = `+mode.emit+`,
    node_modules = "//:node_modules",
    deps = ["//`+base+`/producer", "@npm//:vitest"],
)
`)
		for _, scope := range []struct{ name, root, setup, producer, global, exclude string }{
			{"nested", "./selected", "../setup", "../../producer/setup.ts", "global.mjs", `["excluded/**", "named.test.ts"]`},
			{"ancestor", "..", "./" + mode.name + "/setup", "./producer/setup.ts", mode.name + "/selected/global.mjs", `["` + mode.name + `/selected/excluded/**", "` + mode.name + `/selected/named.test.ts", "` + mode.name + `/sibling.test.ts"]`},
			{"default", "", "./setup", "../producer/setup.ts", "selected/global.mjs", `["selected/excluded/**", "selected/named.test.ts", "sibling.test.ts"]`},
		} {
			root := ""
			if scope.root != "" {
				root = fmt.Sprintf("root: %q, ", scope.root)
			}
			it.Write(it.Path(pkg, "vitest.config.mjs"), fmt.Sprintf(`export default { %stest: {
  setupFiles: [%q, %q],
  globalSetup: %q,
  exclude: %s,
  passWithNoTests: false,
} };
`, root, scope.setup, scope.producer, scope.global, scope.exclude))
			for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
				log, err := it.BazelLog("config_projection_"+mode.name+"_"+scope.name+runfiles, "test", "//"+pkg+":probe", "--test_output=all", runfiles)
				if err != nil || !log.Contains("config references retained canonical runtime identity") {
					log.Dump()
					it.Fail("%s %s with %s lost source-root selection, exclusions, canonical identity or snapshots", mode.name, scope.name, runfiles)
				}
			}
		}
	}
	it.Pass("Vitest source roots and exclusions retain canonical runtime modules and snapshots after relocation")
}

func importedHelpersCannotBeReplacedByData(it *harness.IT) {
	const base = "runtime_helper_identity"
	it.RequireNoDir(it.Path(base), "the imported-helper regression must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	it.Write(it.Path(base, "BUILD.bazel"), "exports_files([\"runfiles.bzl\"])\n")
	it.Write(it.Path(base, "runfiles.bzl"), `load("@rules_typescript//ts:defs.bzl", "TsTestRunnerInfo")

def _shadow(ctx):
    return [DefaultInfo(files = depset(), runfiles = ctx.runfiles(root_symlinks = {
        ctx.workspace_name + "/" + ctx.label.package + "/" + ctx.attr.path: ctx.file.replacement,
    }))]

shadow = rule(implementation = _shadow, attrs = {"replacement": attr.label(allow_single_file = True), "path": attr.string(default = "helper.js")})

def _launch(ctx, test):
    return struct(
        mode = "node",
        section = {"entry": ctx.workspace_name + "/" + test.entry_points[0].short_path},
        env = {},
        files = depset(transitive = [test.transitive_js] + test.runtime_data_sets).to_list(),
        symlinks = {},
        transitive_files = depset(),
        output_groups = {},
    )

def _runner(ctx):
    return [TsTestRunnerInfo(packages = [], supports_source_inputs = False, es_modules = False, launch = _launch)]

direct_runner = rule(implementation = _runner)
`)
	for _, kind := range []string{"local", "dependency", "direct_files", "binary"} {
		pkg := base + "/" + kind
		it.Write(it.Path(pkg, "helper.ts"), "export const answer: number = 0;\n")
		it.Write(it.Path(pkg, "replacement.mjs"), "export const answer = 42;\n")
		it.Write(it.Path(pkg, "package.json"), `{"type":"module"}`)
		it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["entry.test.ts","helper.ts"]}`)
		it.Write(it.Path(pkg, "entry.test.ts"), `import { answer } from './helper.js';
if (answer !== 42) throw new Error('published helper executed');
console.log('replacement made the test pass');
`)
		build := `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile", "ts_test")
load("//` + base + `:runfiles.bzl", "direct_runner", "shadow")
shadow(name = "replacement", replacement = "replacement.mjs")
`
		inputs := `["entry.test.ts", "helper.ts", "package.json"]`
		deps := `[]`
		runner := `"@rules_typescript//ts/runners:node_test"`
		if kind != "local" {
			// A tsconfig's files are its program's roots, so each target's lists only the files it holds.
			it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["entry.test.ts"]}`)
			it.Write(it.Path(pkg, "helper.tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["helper.ts"]}`)
			build += "ts_compile(name = \"helper\", srcs = [\"helper.ts\"], tsconfig = \"helper.tsconfig.json\", emit = True)\n"
			inputs = `["entry.test.ts", "package.json"]`
			deps = `[":helper"]`
		}
		if kind == "direct_files" {
			build += "direct_runner(name = \"direct\")\n"
			runner = `":direct"`
		}
		if kind == "binary" {
			it.Write(it.Path(pkg, "helper.js"), "export const answer = 42;\n")
			build += `ts_compile(name = "entry", srcs = ` + inputs + `, deps = ` + deps + `, emit = True, tsconfig = "tsconfig.json")` + "\n"
			build += `ts_binary(name = "probe", entry_point = ":entry", data = [])` + "\n"
		} else {
			build += `ts_test(name = "probe", srcs = ` + inputs + `, test_srcs = ["entry.test.ts"], deps = ` + deps + `, runner = ` + runner + `, emit = True, tsconfig = "tsconfig.json", data = [])` + "\n"
		}
		placements := []string{"none", "different"}
		if kind == "binary" {
			placements = append(placements, "same")
		}
		for _, placement := range placements {
			configured := build
			switch placement {
			case "different":
				data := `":replacement"`
				if kind == "binary" {
					data = `"helper.js"`
				}
				configured = strings.Replace(build, "data = []", "data = ["+data+"]", 1)
			case "same":
				configured = strings.Replace(build, "data = []", `data = [":helper"]`, 1)
			}
			it.Write(it.Path(pkg, "BUILD.bazel"), configured)
			for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
				args := []string{"test", "//" + pkg + ":probe", "--test_output=all", runfiles}
				if kind == "binary" {
					args = []string{"run", "//" + pkg + ":probe", runfiles}
				}
				log, err := it.BazelLog(fmt.Sprintf("imported_helper_%s_%s%s", kind, placement, runfiles), args...)
				want := "published helper executed"
				if placement == "different" {
					want = "instead of published runtime File '" + pkg + "/helper.js'"
				}
				if err == nil || !log.Contains(want) || log.Contains("replacement made the test pass") {
					log.Dump()
					it.Fail("%s %s did not preserve its imported helper's File identity (data=%s)", kind, runfiles, placement)
				}
			}
		}
	}
	it.Pass("data cannot replace an imported module in binary or test runfiles; duplicate data selecting the same File is accepted")
	pkg := base + "/config"
	it.Write(it.Path(pkg, "package.json"), `{"type":"module"}`)
	it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":["node"]},"files":["probe.test.ts"]}`)
	it.Write(it.Path(pkg, "settings/vitest.config.mjs"), `import { answer } from './helper.mjs';
export default { test: { env: { DECLARED_CONFIG_VALUE: String(answer) } } };
`)
	it.Write(it.Path(pkg, "settings/helper.mjs"), "export const answer = 42;\n")
	it.Write(it.Path(pkg, "replacement.config.mjs"), "export default { test: { env: { DECLARED_CONFIG_VALUE: '0' } } };\n")
	it.Write(it.Path(pkg, "replacement.helper.mjs"), "export const answer = 0;\n")
	it.Write(it.Path(pkg, "replacement.paths.json"), "{}\n")
	it.Write(it.Path(pkg, "probe.test.ts"), `import { expect, it } from 'vitest';
it('rejects replacement of the config and helper that set the test environment', () => {
  expect(process.env.DECLARED_CONFIG_VALUE).toBe('42');
  console.log('declared config and helper executed');
});
`)
	configSource := "_probe.vitest/" + pkg + "/settings/vitest.config.mjs"
	helperSource := "_probe.vitest/" + pkg + "/settings/helper.mjs"
	build := `load("@rules_typescript//ts:defs.bzl", "ts_test")
load("//` + base + `:runfiles.bzl", "shadow")
shadow(name = "same_config", path = "` + configSource + `", replacement = "settings/vitest.config.mjs")
shadow(name = "same_helper", path = "` + helperSource + `", replacement = "settings/helper.mjs")
shadow(name = "different_config", path = "` + configSource + `", replacement = "replacement.config.mjs")
shadow(name = "different_helper", path = "` + helperSource + `", replacement = "replacement.helper.mjs")
shadow(name = "different_generated", path = "_probe.vitest/config.mjs", replacement = "replacement.config.mjs")
shadow(name = "different_paths", path = "_probe.vitest/tsconfig_paths.json", replacement = "replacement.paths.json")
ts_test(
    name = "probe",
    srcs = ["probe.test.ts", "package.json"],
    tsconfig = "tsconfig.json",
    config = "settings/vitest.config.mjs",
    config_srcs = ["settings/helper.mjs"],
    node_modules = "//:node_modules",
    deps = ["@npm//:vitest", "@npm//:types_node"],
    data = [],
)
`
	for _, placement := range []struct{ name, data, source, expected string }{
		{name: "none"},
		{name: "same", data: `":same_config", ":same_helper"`},
		{name: "config", data: `":different_config"`, source: configSource, expected: "settings/vitest.config.mjs"},
		{name: "helper", data: `":different_helper"`, source: helperSource, expected: "settings/helper.mjs"},
		{name: "generated", data: `":different_generated"`, source: "_probe.vitest/config.mjs", expected: "_probe.vitest/config.mjs"},
		{name: "paths", data: `":different_paths"`, source: "_probe.vitest/tsconfig_paths.json", expected: "_probe.vitest/tsconfig_paths.json"},
	} {
		it.Write(it.Path(pkg, "BUILD.bazel"), strings.Replace(build, "data = []", "data = ["+placement.data+"]", 1))
		runfilesModes := []string{"--enable_runfiles"}
		if placement.source == "" {
			runfilesModes = append(runfilesModes, "--noenable_runfiles")
		}
		for _, runfiles := range runfilesModes {
			log, err := it.BazelLog("config_stage_identity_"+placement.name+runfiles, "test", "//"+pkg+":probe", "--test_output=all", runfiles)
			if placement.source == "" {
				if err != nil || !log.Contains("declared config and helper executed") {
					log.Dump()
					it.Fail("%s rejected the declared config staging Files (data=%s)", runfiles, placement.name)
				}
				continue
			}
			if err == nil || !log.Contains("/"+pkg+"/"+placement.source+"' selects") || !log.Contains("instead of published runtime File '"+pkg+"/"+placement.expected+"'") || log.Contains("declared config and helper executed") {
				log.Dump()
				it.Fail("%s did not reject replacement of the %s staging source before executing the test", runfiles, placement.name)
			}
		}
	}
	it.Pass("config staging rejects replaced authored and generated sources; unchanged and same-File config inputs execute")
	entryPkg := base + "/entry"
	it.Write(it.Path(base, "entry_foreign/BUILD.bazel"), `exports_files(["index.ts", "package.json"], visibility = ["//visibility:public"])`)
	it.Write(it.Path(base, "entry_foreign/package.json"), `{"type":"module"}`)
	it.Write(it.Path(base, "entry_foreign/index.ts"), "export interface EntryShape { value: number }\nconsole.log('borrowed index executed');\n")
	it.Write(it.Path(entryPkg, "package.json"), `{"type":"module"}`)
	entryConfig := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["main.ts"]}`
	it.Write(it.Path(entryPkg, "tsconfig.json"), entryConfig)
	it.Write(it.Path(entryPkg, "main.ts"), `import type { EntryShape } from '../entry_foreign/index';
const value: EntryShape = { value: 42 };
console.log('application entry executed: ' + value.value);
`)
	it.Write(it.Path(entryPkg, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(
    name = "entry",
    emit = True, # keep
)
ts_binary(name = "run", entry_point = ":entry")
`)
	// A package.json pnpm installs nothing for marks a foreign project, which Gazelle leaves alone.
	for _, file := range []string{"pnpm-workspace.yaml", "pnpm-lock.yaml"} {
		defer it.Write(it.Path(file), it.Read(it.Path(file)))
	}
	it.Write(it.Path("pnpm-workspace.yaml"), it.Read(it.Path("pnpm-workspace.yaml"))+"  - "+entryPkg+"\n  - "+base+"/entry_foreign\n")
	it.MustBazel("run", "//:pnpm", "--", "install", "--lockfile-only", "--no-frozen-lockfile")
	it.Install()
	it.MustBazel("run", "//:gazelle", "--", "-r=false", entryPkg)
	requireLabels(it, "srcs", "//"+entryPkg+":entry", []string{"//" + entryPkg + ":main.ts", "//" + base + "/entry_foreign:index.ts"})
	for _, selection := range []string{"local", "ambiguous", "explicit"} {
		if selection == "ambiguous" {
			it.Write(it.Path(entryPkg, "other.ts"), "console.log('other local entry executed');\nexport {};\n")
			it.Write(it.Path(entryPkg, "tsconfig.json"), strings.Replace(entryConfig, `"main.ts"`, `"main.ts","other.ts"`, 1))
			it.MustBazel("run", "//:gazelle", "--", "-r=false", entryPkg)
		}
		if selection == "explicit" {
			build, err := rule.LoadFile(it.Path(entryPkg, "BUILD.bazel"), entryPkg)
			if err != nil {
				it.Fail("cannot load the ambiguous binary entry: %v", err)
			}
			for _, target := range build.Rules {
				if target.Kind() == "ts_binary" && target.Name() == "run" {
					target.SetAttr("entry_file", "main.ts")
				}
			}
			it.Write(it.Path(entryPkg, "BUILD.bazel"), string(build.Format()))
		}
		log, err := it.BazelLog("borrowed_index_entry_"+selection, "run", "//"+entryPkg+":run", "--@rules_typescript//ts:declarations=oxc")
		if selection == "ambiguous" {
			if err == nil || !log.Contains("Set entry_file to a unique source filename") || log.Contains("entry executed") || log.Contains("borrowed index executed") {
				log.Dump()
				it.Fail("a borrowed index selected an entry among ambiguous local sources")
			}
		} else if err != nil || !log.Contains("application entry executed: 42") || log.Contains("borrowed index executed") || log.Contains("other local entry executed") {
			log.Dump()
			it.Fail("%s entry selection lost the application's source identity to a retained helper", selection)
		}
	}
	it.Pass("a retained foreign index cannot replace the local binary entry; ambiguous local entries require explicit selection")
}

func compilerInputsCannotBecomeRuntimeLayout(it *harness.IT) {
	const base = "input_roles"
	it.RequireNoDir(it.Path(base), "the compiler input regression must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	defer it.Write(it.Path("BUILD.bazel"), it.Read(it.Path("BUILD.bazel")))
	it.Write(it.Path(base, "types/BUILD.bazel"), `exports_files(["api.d.ts", "internal.d.ts", "package.json"], visibility = ["//visibility:public"])`)
	it.Write(it.Path(base, "types/package.json"), `{"type":"module","imports":{"#internal":"./internal.d.ts"}}`)
	it.Write(it.Path(base, "types/api.d.ts"), "export type { Value } from '#internal';\n")
	it.Write(it.Path(base, "types/internal.d.ts"), "export interface Value { answer: number }\n")
	it.Write(it.Path(base, "app/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`)
	it.Write(it.Path(base, "app/index.ts"), `import type { Value } from '../types/api.js';
const value: Value = { answer: 42 };
export const answer: number = value.answer;
console.log('compiler metadata kept out of runtime: ' + answer);
`)
	it.Write(it.Path(base, "app/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(
    name = "app",
    srcs = ["index.ts"],
    emit = True,  # keep
)
ts_binary(name = "run", entry_point = ":app")
`)
	it.MustBazel("run", "//:gazelle", "--", "-r=false", ".", base+"/app", base+"/types")
	requireLabels(it, "type_inputs", "//"+base+"/app:app", []string{"//" + base + "/types:package.json"})
	for _, backend := range []string{"tsgo", "oxc"} {
		flag := "--@rules_typescript//ts:declarations=" + backend
		it.MustBazel("build", "//"+base+"/app:app", flag, "--output_groups=+declarations,+_validation", "--@rules_typescript//ts:lib_check")
		declaration := it.Read(it.Bin(base, "app/index.d.ts"))
		if !strings.Contains(declaration, "answer") || strings.Contains(declaration, "Value") || strings.Contains(declaration, "../types") {
			it.Fail("%s declaration retained an implementation-local foreign type: %s", backend, declaration)
		}
		log, err := it.BazelLog("compiler_input_runtime_"+backend, "run", "//"+base+"/app:run", flag)
		if err != nil || !log.Contains("compiler metadata kept out of runtime: 42") {
			log.Dump()
			it.Fail("%s lost compiler-only package scope or published it as runtime data", backend)
		}
	}
	it.Write(it.Path(base, "app/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"allowJs":true},"files":["index.ts"]}`)
	it.Write(it.Path(base, "app/index.ts"), "import { answer } from './bridge.js'; export const value: number = answer; console.log('staged JavaScript reaches emitted TypeScript: ' + value);\n")
	it.Write(it.Path(base, "app/bridge.js"), "export { answer } from './value.js';\n")
	it.Write(it.Path(base, "app/value.ts"), "export const answer: number = 42;\n")
	it.MustBazel("run", "//:gazelle", "--", "-r=false", ".", base+"/app", base+"/types")
	requireLabels(it, "type_inputs", "//"+base+"/app:app", nil)
	appBuild, err := rule.LoadFile(it.Path(base, "app/BUILD.bazel"), base+"/app")
	if err != nil {
		it.Fail("cannot load the JavaScript staging fixture: %v", err)
	}
	var definitions *rule.Load
	for _, load := range appBuild.Loads {
		if load.Name() == "@rules_typescript//ts:defs.bzl" {
			definitions = load
			break
		}
	}
	if definitions == nil {
		it.Fail("JavaScript staging fixture lost its TypeScript definitions load")
	}
	definitions.Add("ts_test")
	it.Write(it.Path(base, "app/BUILD.bazel"), string(appBuild.Format())+`
ts_compile(name = "original_js", srcs = ["bridge.js", "value.ts"])
ts_compile(name = "staged_asset", srcs = [], data = ["bridge.js"])
ts_test(name = "staged_test", srcs = ["bridge.js"], emit = True, deps = [":app"], runner = "@rules_typescript//ts/runners:node_test")
`)
	it.MustBazel("build", "//"+base+"/app:app", "//"+base+"/app:original_js", "//"+base+"/app:staged_asset", "--@rules_typescript//ts:declarations=tsgo")
	it.MustBazel("test", "//"+base+"/app:staged_test", "--@rules_typescript//ts:declarations=tsgo")
	log, err := it.BazelLog("staged_javascript_realpath", "run", "//"+base+"/app:run", "--@rules_typescript//ts:declarations=tsgo")
	if err != nil || !log.Contains("staged JavaScript reaches emitted TypeScript: 42") {
		log.Dump()
		it.Fail("a staged JavaScript realpath escaped the emitted module layout")
	}
	func() {
		for _, file := range []string{"app/BUILD.bazel", "app/index.ts", "app/value.ts"} {
			defer it.Write(it.Path(base, file), it.Read(it.Path(base, file)))
		}
		defer it.Write(it.Path("BUILD.bazel"), it.Read(it.Path("BUILD.bazel")))
		it.RequireNoFile(it.Path("javascript_asset_anchor.ts"), "the moved asset anchor must be staged by this check")
		defer os.Remove(it.Path("javascript_asset_anchor.ts"))
		it.RequireNoFile(it.Path(base, "app/bridge.mjs"), "the JavaScript data bridge must be staged by this check")
		defer os.Remove(it.Path(base, "app/bridge.mjs"))
		for _, owner := range []string{"forwarded", "caller"} {
			it.RequireNoDir(it.Path(base, owner), "the moved asset consumer must be staged by this check")
			defer os.RemoveAll(it.Path(base, owner))
		}
		entry := func(valuePath, bridgePath string) string {
			return `import { singleton, initializations } from ` + strconv.Quote(valuePath) + `;
const bridgePath: string = ` + strconv.Quote(bridgePath) + `;
const bridge = await import(bridgePath);
if (bridge.singleton !== singleton || initializations() !== 1 || bridge.initializations() !== 1) {
  throw new Error('JavaScript data escaped the canonical singleton');
}
console.log('JavaScript data retained one singleton and initialization');
`
		}
		it.Write(it.Path(base, "app/index.ts"), entry("./value.js", "./bridge.mjs"))
		it.Write(it.Path(base, "app/value.ts"), `const state = globalThis as typeof globalThis & { assetInitializations?: number };
state.assetInitializations = (state.assetInitializations ?? 0) + 1;
export const singleton: object = {};
export function initializations(): number { return state.assetInitializations!; }
`)
		it.Write(it.Path(base, "app/bridge.mjs"), "export { singleton, initializations } from './value.js';\n")
		it.Write(it.Path(base, "app/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(name = "app", srcs = ["index.ts", "value.ts"], data = ["bridge.mjs"], package_scopes = ["//:package.json"], tsconfig = "tsconfig.json", emit = True, visibility = ["//visibility:public"])
ts_binary(name = "run", entry_point = ":app")
`)
		requireLabels(it, "srcs", "//"+base+"/app:app", []string{"//" + base + "/app:index.ts", "//" + base + "/app:value.ts"})
		requireLabels(it, "data", "//"+base+"/app:app", []string{"//" + base + "/app:bridge.mjs"})
		it.Write(it.Path("javascript_asset_anchor.ts"), "export const anchor: number = 1;\n")
		it.Write(it.Path("BUILD.bazel"), it.Read(it.Path("BUILD.bazel"))+"\nexports_files([\"javascript_asset_anchor.ts\"], visibility = [\"//visibility:public\"])\n")
		it.Write(it.Path(base, "forwarded/index.ts"), "export { singleton, initializations } from '../app/value.js';\n")
		it.Write(it.Path(base, "forwarded/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile")
ts_compile(name = "forwarded", srcs = ["index.ts", "//:javascript_asset_anchor.ts"], deps = ["//`+base+`/app:app"], package_scopes = ["//:package.json"], emit = True, visibility = ["//visibility:public"])
`)
		it.Write(it.Path(base, "caller/index.ts"), entry("../forwarded/index.js", "../app/bridge.mjs"))
		it.Write(it.Path(base, "caller/raw.mjs"), strings.Replace(entry("../app/value.js", "./"+base+"/app/bridge.mjs"), "const bridgePath: string", "const bridgePath", 1))
		it.Write(it.Path(base, "caller/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(name = "caller", srcs = ["index.ts"], deps = ["//`+base+`/forwarded:forwarded"], package_scopes = ["//:package.json"], emit = True)
ts_binary(name = "run", entry_point = ":caller", data = [":caller"])
ts_binary(name = "raw", entry_point = "raw.mjs", data = [":caller"])
`)
		for _, target := range []string{"app:run", "caller:run", "caller:raw"} {
			log, err := it.BazelLog("javascript_data_singleton_"+strings.ReplaceAll(target, ":", "_"), "run", "//"+base+"/"+target, "--@rules_typescript//ts:declarations=tsgo", "--output_groups=+_validation")
			if err != nil || !log.Contains("JavaScript data retained one singleton and initialization") {
				log.Dump()
				it.Fail("%s JavaScript data lost its canonical module instance", target)
			}
		}
		it.RequireNoFile(it.Path(base, "app/value.js"), "a checkout module must not satisfy the forwarded data bridge")
		canonical, err := os.Stat(it.Bin(base, "app/bridge.mjs"))
		if err != nil {
			it.Fail("cannot inspect the canonical data bridge: %v", err)
		}
		for _, owner := range []string{"forwarded", "caller"} {
			alias, err := os.Stat(it.Bin(base, owner, base, "app/bridge.mjs"))
			if err != nil || !os.SameFile(canonical, alias) {
				it.Fail("%s data bridge alias does not retain the exact producer File: %v", owner, err)
			}
		}
	}()
	it.Write(it.Path(base, "app/value.ts"), "export const answer: number = 43;\n")
	it.Write(it.Path(base, "app/source.test.ts"), `import { it, expect } from 'vitest';
import { answer } from './bridge.js';
it('resolves source-mode JavaScript beside the current TypeScript input', () => {
  expect(answer).toBe(43);
  console.log('source JavaScript reached current TypeScript: 43');
});
`)
	it.Write(it.Path(base, "app/BUILD.bazel"), it.Read(it.Path(base, "app/BUILD.bazel"))+`
ts_test(name = "source_test", srcs = ["source.test.ts"], deps = [":original_js", "@npm//:vitest"], node_modules = "//:node_modules")
`)
	log, err = it.BazelLog("source_javascript_placement", "test", "//"+base+"/app:source_test", "--test_output=all")
	if err != nil || !log.Contains("source JavaScript reached current TypeScript: 43") {
		log.Dump()
		it.Fail("source-mode JavaScript lost its TypeScript input or read the previous emitted copy")
	}
	it.Write(it.Path(base, "foreign/value.ts"), "import fixture from '#fixture' with { type: 'json' };\nexport const value: number = fixture.value;\n")
	it.Write(it.Path(base, "foreign/value.json"), `{"value":42}`)
	it.Write(it.Path(base, "foreign/package.json"), `{"type":"module","imports":{"#fixture":"./value.json"}}`)
	it.Write(it.Path(base, "foreign/BUILD.bazel"), `exports_files(["value.ts", "value.json", "package.json"], visibility = ["//visibility:public"])
filegroup(name = "opaque", srcs = ["value.ts", "value.json"], visibility = ["//visibility:public"])
`)
	it.Write(it.Path(base, "mixed/index.ts"), "export const independent: number = 7;\nconsole.log('independent entry: ' + independent);\n")
	it.Write(it.Path(base, "mixed/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true,"types":[]},"files":["index.ts"]}`)
	it.Write(it.Path(base, "mixed/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
# keep
ts_compile(name = "custom", srcs = ["index.ts", "//`+base+`/foreign:opaque"], package_scopes = ["//`+base+`/foreign:package.json"], emit = True)
ts_binary(name = "run", entry_point = ":custom", entry_file = "index.ts")
`)
	it.MustBazel("run", "//:gazelle", "--", base+"/mixed")
	it.MustBazel("build", "//"+base+"/mixed:custom", "--output_groups=+declarations,+_validation")
	for _, artifact := range []struct{ path, symbol string }{
		{"mixed/index.js", "independent"},
		{"mixed/index.d.ts", "independent"},
		{"foreign/value.js", "value"},
		{"foreign/value.d.ts", "value"},
	} {
		if contents := it.Read(it.Bin(base, "mixed", artifact.path)); !strings.Contains(contents, artifact.symbol) {
			it.Fail("independent local/foreign library lost %s: %s", artifact.path, contents)
		}
	}
	for _, imports := range []string{"disconnected", "type_only"} {
		if imports == "type_only" {
			it.Write(it.Path(base, "mixed/index.ts"), `import type { value } from '../foreign/value.js';
export const independent: number = 7;
console.log('independent entry: ' + (independent as typeof value));
`)
		}
		log, err = it.BazelLog("kept_library_"+imports, "run", "//"+base+"/mixed:run")
		if err != nil || !log.Contains("independent entry: 7") {
			log.Dump()
			it.Fail("the kept library's %s modules prevented its independent entry from running", imports)
		}
	}
	it.Write(it.Path(base, "mixed/index.ts"), "import { value } from '../foreign/value.js';\nexport { value };\nconsole.log('mixed emitted modules: ' + value);\n")
	it.MustBazel("build", "//"+base+"/mixed:custom", "--output_groups=+declarations,+_validation")
	it.RequireNoFile(it.Bin(base, "foreign/value.js"), "a separate owner output must not satisfy the mixed entry's relative import")
	it.RequireNoFile(it.Path(base, "foreign/value.js"), "a checkout JavaScript file must not satisfy the mixed entry's relative import")
	it.RequireContains(it.Bin(base, "mixed/mixed/index.d.ts"), "../foreign/value.js", "the mixed entry declaration changed its relative dependency")
	it.RequireContains(it.Bin(base, "mixed/foreign/package.json"), `"#fixture"`, "the emitted foreign module lost its own package scope")
	if got := it.Read(it.Bin(base, "mixed/foreign/value.json")); got != it.Read(it.Path(base, "foreign/value.json")) {
		it.Fail("the mixed compiler changed its foreign JSON module: %s", got)
	}
	log, err = it.BazelLog("kept_library_runtime_import", "run", "//"+base+"/mixed:run")
	if err != nil || !log.Contains("mixed emitted modules: 42") {
		log.Dump()
		it.Fail("the mixed compiler lost a relative runtime module or its package-private JSON import")
	}
	it.Pass("compiler-only declaration scope stays out of runtime data; disconnected, type-only and executed mixed source roots retain their modules, JSON and package scope")
}

func generatedCompilerInputsUseDeclaredFiles(it *harness.IT) {
	const pkg = "generated_json_module"
	dir := it.Path(pkg)
	it.RequireNoDir(dir, "the generated JSON module fixture must be staged by this check")
	defer os.RemoveAll(dir)
	it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true,"types":[]},"files":["value.test.ts"]}`)
	it.Write(it.Path(pkg, "value.test.ts"), `import { expect, it } from 'vitest';
import data from './value.json';

const answer: number = data.answer;
it('reads a generated JSON module through its declared File', () => {
  expect(answer).toBe(42);
});
`)
	target := "//" + pkg + ":" + pkg + "_test"
	for _, emit := range []string{"False", "True"} {
		it.Write(it.Path(pkg, "BUILD.bazel"), fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_test")

genrule(
    name = "values",
    outs = ["value.json"],
    cmd = "echo '{\"answer\":42}' > $@",
)

ts_test(
    name = "%s_test",
    emit = %s,  # keep
)
`, pkg, emit))
		it.MustBazel("run", "//:gazelle", "--", "-r=false", pkg)
		requireLabels(it, "srcs", target, []string{"//" + pkg + ":value.json", "//" + pkg + ":value.test.ts"})
		requireLabels(it, "outs", "//"+pkg+":values", []string{"//" + pkg + ":value.json"})
		if owners := strings.TrimSpace(it.BazelStdout("query", `kind("ts_compile rule", //`+pkg+`:*)`)); owners != "" {
			it.Fail("generated JSON acquired an unnecessary compiler: %s", owners)
		}
		before := it.Read(it.Path(pkg, "BUILD.bazel"))
		it.MustBazel("run", "//:gazelle", "--", "-r=false", pkg)
		if got := it.Read(it.Path(pkg, "BUILD.bazel")); got != before {
			it.Fail("emit=%s generated JSON graph changed on rerun:\n%s\n%s", emit, before, got)
		}
		it.MustBazel("test", target, "--output_groups=+_validation")
	}
	it.Replace(it.Path(pkg, "value.test.ts"), "const answer: number", "const answer: string")
	log, err := it.BazelLog("generated_json_module_type", "build", target, "--output_groups=+_validation")
	if err == nil || !log.Contains("TS2322") || !log.Contains("value.test.ts") {
		log.Dump()
		it.Fail("the generated JSON File did not enforce its type in the consumer")
	}
	it.Pass("generated JSON resolves, type-checks and runs through its declared File in source and emitted tests")

	it.Write(it.Path(pkg, "package.json"), `{"type":"module","imports":{"#schema":"./schema.mjs"}}`)
	it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"checkJs":true,"types":[]},"files":["type-input.ts"]}`)
	const source = "import type { schema } from '#schema'; export const answer: typeof schema.answer = 42;\n"
	for _, route := range []string{"direct", "forwarded"} {
		inputs, deps := `[":schema", "package.json"]`, `[]`
		if route == "forwarded" {
			inputs, deps = `[]`, `[":metadata"]`
		}
		it.Write(it.Path(pkg, "BUILD.bazel"), fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_compile")

genrule(
    name = "schema",
    outs = ["schema.mjs"],
    cmd = "echo 'export const schema = { answer: 42 };' > $@",
)

ts_compile(name = "metadata", type_inputs = [":schema", "package.json"])

ts_compile(
    name = "typed",
    srcs = ["type-input.ts"],
    type_inputs = %s,
    deps = %s,
    tsconfig = "tsconfig.json",
    emit = True,
)
`, inputs, deps))
		it.Write(it.Path(pkg, "type-input.ts"), source)
		it.MustBazel("build", "//"+pkg+":typed", "--output_groups=+declarations,+_validation")
		it.Write(it.Path(pkg, "type-input.ts"), strings.Replace(source, "= 42", "= 'wrong'", 1))
		log, err := it.BazelLog("generated_js_type_input_"+route, "build", "//"+pkg+":typed", "--output_groups=+declarations,+_validation")
		if err == nil || !log.Contains("TS2322") || !log.Contains("type-input.ts") {
			log.Dump()
			it.Fail("%s generated JavaScript type input did not enforce its package-import type", route)
		}
	}
	it.Pass("direct and forwarded generated JavaScript type inputs resolve package imports and reject incompatible assignments")
}

func emittedConsumerKeepsSourceModeRuntimeFiles(it *harness.IT) {
	const pkg = "source_runtime_dependency"
	dir := it.Path(pkg)
	it.RequireNoDir(dir, "the runtime dependency fixture must be staged by this check")
	defer os.RemoveAll(dir)
	defer it.Write(it.Path("BUILD.bazel"), it.Read(it.Path("BUILD.bazel")))
	defer it.Write(it.Path("pnpm-lock.yaml"), it.Read(it.Path("pnpm-lock.yaml")))
	workspace := it.Path("pnpm-workspace.yaml")
	defer it.Write(workspace, it.Read(workspace))
	it.Write(it.Path(pkg, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")

ts_compile(
    name = "runtime",
    srcs = ["helper.d.mts", "helper.mjs", "value.json"],
    emit = False,
)

ts_compile(
    name = "entry",
    srcs = ["index.ts"],
    package_scopes = ["package.json"],
    tsconfig = "tsconfig.json",
    deps = [":runtime"],
    emit = True,
)

ts_binary(
    name = "run",
    entry_point = ":entry",
)
`)
	it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`)
	it.Write(it.Path(pkg, "package.json"), `{"type":"module"}`)
	it.Write(workspace, it.Read(workspace)+"  - "+pkg+"\n")
	it.MustBazel("run", "//:pnpm", "--", "install", "--lockfile-only", "--no-frozen-lockfile")
	it.Write(it.Path(pkg, "helper.mjs"), "export const offset = 2;\n")
	it.Write(it.Path(pkg, "helper.d.mts"), "export declare const offset: number;\n")
	it.Write(it.Path(pkg, "value.json"), `{"value":40}`)
	it.Write(it.Path(pkg, "index.ts"), `import { offset } from './helper.mjs';
import data from './value.json' with { type: 'json' };
console.log(data.value + offset);
`)
	for _, name := range []string{"helper.mjs", "value.json"} {
		it.RequireNoFile(it.Bin(pkg, name), "a previous output must not conceal the missing runtime file")
	}
	if got := strings.TrimSpace(it.BazelStdout("run", "//"+pkg+":run", "--output_groups=+_validation")); got != "42" {
		it.Fail("emitted consumer lost its source-mode JavaScript/JSON runtime dependency: got %q, want 42", got)
	}
	it.Pass("the emitted binary reads package-local JavaScript and JSON from a source-mode dependency")

	const sibling = "sibling_runtime_files"
	it.RequireNoDir(it.Path(sibling), "the sibling runtime inputs must be staged by this check")
	defer os.RemoveAll(it.Path(sibling))
	it.Write(it.Path(sibling, "BUILD.bazel"), `exports_files(["helper.mjs", "helper.d.mts", "value.json"], visibility = ["//source_runtime_dependency:__pkg__"])
`)
	for _, name := range []string{"helper.mjs", "helper.d.mts", "value.json"} {
		it.Write(it.Path(sibling, name), it.Read(it.Path(pkg, name)))
	}
	it.Write(it.Path(pkg, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary")
ts_binary(name = "run", entry_point = ":source_runtime_dependency")
`)
	it.Write(it.Path(pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"types":[]},"files":["index.ts"]}`)
	it.Write(it.Path(pkg, "index.ts"), strings.ReplaceAll(it.Read(it.Path(pkg, "index.ts")), "'./", "'../"+sibling+"/"))
	it.MustBazel("run", "//:gazelle", "--", "-r=false", ".", pkg, sibling)
	for _, name := range []string{"helper.mjs", "value.json"} {
		it.RequireNoFile(it.Bin(sibling, name), "a prior sibling output must not satisfy the emitted entry's relative import")
	}
	for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
		if got := strings.TrimSpace(it.BazelStdout("run", "//"+pkg+":run", "--output_groups=+_validation", runfiles)); got != "42" {
			it.Fail("the emitted entry lost declared sibling JavaScript/JSON under %s: %q", runfiles, got)
		}
	}
	it.Write(it.Path(sibling, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"resolveJsonModule":true,"types":[]},"files":["helper.mjs","helper.d.mts","value.json"]}`)
	it.MustBazel("run", "//:gazelle", "--", "-r=false", ".", pkg, sibling)
	requireLabels(it, "deps", "//"+pkg+":"+pkg, []string{"//" + sibling + ":" + sibling})
	if got := strings.TrimSpace(it.BazelStdout("run", "//"+pkg+":run", "--output_groups=+_validation")); got != "42" {
		it.Fail("emitted consumer lost its sibling compiler's JavaScript/JSON runtime files: got %q, want 42", got)
	}
	it.Pass("the emitted binary reads sibling JavaScript and JSON through their compiler owner")

	const hidden = "transitive_runtime_value"
	it.RequireNoDir(it.Path(hidden), "the transitive source owner must be staged by this check")
	defer os.RemoveAll(it.Path(hidden))
	hiddenBuild := `load("@rules_typescript//ts:defs.bzl", "ts_compile")
ts_compile(name = "transitive_runtime_value", srcs = ["value.ts"], visibility = ["//visibility:public"])
`
	it.Write(it.Path(hidden, "BUILD.bazel"), hiddenBuild)
	it.Write(it.Path(hidden, "value.ts"), "export const offset: number = 2;\n")
	it.Write(it.Path(hidden, "value.js"), "throw new Error('stale hidden runtime source loaded');\n")
	it.Write(it.Path(sibling, "helper.mjs"), "export { offset } from '../"+hidden+"/value.js';\n")
	bridge, err := rule.LoadFile(it.Path(sibling, "BUILD.bazel"), sibling)
	if err != nil {
		it.Fail("cannot load the JavaScript bridge: %v", err)
	}
	for _, target := range bridge.Rules {
		if target.Kind() == "ts_compile" && target.Name() == sibling {
			target.SetAttr("emit", false)
			target.SetAttr("deps", append(target.AttrStrings("deps"), "//"+hidden))
		}
	}
	it.Write(it.Path(sibling, "BUILD.bazel"), string(bridge.Format()))
	before := map[string]string{}
	for _, directory := range []string{"", pkg, sibling, hidden} {
		file := it.Path(directory, "BUILD.bazel")
		before[file] = it.Read(file)
	}
	log, err := it.BazelLog("unpublished_transitive_runtime_source", "run", "//:gazelle", "--", "-index=false", "-r=false", pkg)
	if err == nil || !log.Contains("requires emission from //"+hidden+" for runtime TypeScript input "+hidden+"/value.ts") {
		log.Dump()
		it.Fail("a declaration bridge hid its declared transitive TypeScript dependency from publication")
	}
	for file, original := range before {
		if it.Read(file) != original {
			it.Fail("refused transitive publication changed %s", file)
		}
	}
	it.Write(it.Path(hidden, "BUILD.bazel"), strings.Replace(hiddenBuild, "srcs =", "emit = True, srcs =", 1))
	it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", pkg)
	if got := strings.TrimSpace(it.BazelStdout("run", "//"+pkg+":run", "--output_groups=+_validation")); got != "42" {
		it.Fail("emitted consumer lost the JavaScript bridge's published TypeScript dependency: got %q, want 42", got)
	}
	it.Pass("a JavaScript declaration bridge cannot hide an unpublished TypeScript dependency")
}

func sourceModeDirectRuntimeCannotLoseForeignModules(it *harness.IT) {
	const base = "source_mode_direct_runtime"
	it.RequireNoDir(it.Path(base), "the source-mode runtime fixture must be staged by this check")
	it.RequireNoDir(it.Bin(base), "old outputs must not conceal a missing source-mode runtime module")
	defer os.RemoveAll(it.Path(base))
	it.Write(it.Path(base, "foreign/BUILD.bazel"), `exports_files(["helper.cjs", "value.json"], visibility = ["//visibility:public"])
`)
	it.Write(it.Path(base, "foreign/helper.cjs"), "module.exports = require('./value.json').value;\n")
	it.Write(it.Path(base, "foreign/value.json"), `{"value":42}`)
	for _, kind := range []string{"javascript", "json"} {
		pkg := base + "/" + kind
		expression := "require('../foreign/value.json').value"
		inputs := `["index.cjs", "//` + base + `/foreign:value.json"]`
		if kind == "javascript" {
			expression = "require('../foreign/helper.cjs')"
			inputs = `["index.cjs", "//` + base + `/foreign:helper.cjs", "//` + base + `/foreign:value.json"]`
		}
		it.Write(it.Path(pkg, "index.cjs"), "const value = "+expression+";\nif (value !== 42) throw new Error('foreign runtime value: ' + value);\nconsole.log('source-mode runtime layout: 42');\n")
		it.Write(it.Path(pkg, "direct.bzl"), `load("@rules_typescript//ts:defs.bzl", "TsTestRunnerInfo")

def _launch(ctx, test):
    return struct(
        mode = "node",
        section = {"entry": ctx.workspace_name + "/" + test.entry_points[0].short_path},
        env = {},
        files = [],
        symlinks = {},
        transitive_files = depset(transitive = [test.transitive_js] + test.runtime_data_sets),
        output_groups = {},
    )

def _impl(ctx):
    return [TsTestRunnerInfo(packages = [], supports_source_inputs = False, es_modules = False, launch = _launch)]

direct_runner = rule(implementation = _impl)
`)
		for _, consumer := range []string{"binary", "custom"} {
			build := `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(name = "library", srcs = ` + inputs + `, emit = False)
ts_binary(name = "run", entry_point = ":library", entry_file = "index.cjs")
`
			if consumer == "custom" {
				build = `load("@rules_typescript//ts:defs.bzl", "ts_test")
load(":direct.bzl", "direct_runner")
direct_runner(name = "direct")
ts_test(name = "run", srcs = ` + inputs + `, test_srcs = ["index.cjs"], emit = False, runner = ":direct")
`
			}
			it.Write(it.Path(pkg, "BUILD.bazel"), build)
			missing := "value.json"
			if kind == "javascript" {
				missing = "helper.cjs"
			}
			it.RequireNoFile(it.Bin(base, "foreign", missing), "a prior foreign output must not satisfy the direct entry's relative import")
			for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
				log, err := it.BazelLog("source_mode_"+kind+"_"+consumer+runfiles, "run", "//"+pkg+":run", runfiles)
				if err != nil || !log.Contains("source-mode runtime layout: 42") {
					log.Dump()
					it.Fail("the %s loader lost declared source-mode %s under %s", consumer, kind, runfiles)
				}
			}
		}
		it.Write(it.Path(pkg, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_test")
ts_test(name = "logical", srcs = `+inputs+`, test_srcs = ["index.cjs"], deps = ["@rules_typescript//tests/multi/lib:lib"], emit = False, runner = "@rules_typescript//ts/runners:node_test")
`)
		for _, runfiles := range []string{"--enable_runfiles", "--noenable_runfiles"} {
			log, err := it.BazelLog("source_mode_logical_"+kind+runfiles, "test", "//"+pkg+":logical", "--test_output=all", runfiles)
			if err != nil || !log.Contains("source-mode runtime layout: 42") {
				log.Dump()
				it.Fail("the Node test runner lost the coherent logical %s layout", kind)
			}
		}
	}
	pkg := base + "/local"
	it.Write(it.Path(pkg, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_compile")
ts_compile(name = "library", srcs = ["index.cjs", "helper.cjs", "value.json"], deps = ["@rules_typescript//tests/multi/lib:lib"], emit = False)
ts_binary(name = "run", entry_point = ":library", entry_file = "index.cjs")
`)
	it.Write(it.Path(pkg, "index.cjs"), "console.log(require('./helper.cjs'));\n")
	for _, name := range []string{"helper.cjs", "value.json"} {
		it.Write(it.Path(pkg, name), it.Read(it.Path(base, "foreign", name)))
	}
	if got := strings.TrimSpace(it.BazelStdout("run", "//"+pkg+":run", "--output_groups=+_validation")); got != "42" {
		it.Fail("the coherent source-mode JavaScript/JSON binary produced %q, want 42", got)
	}
	it.Pass("binary and test consumers retain declared foreign JS/JSON in one logical runtime layout")
}

func siblingOwnerRetainsSourcePackageScope(it *harness.IT) {
	const base = "owned_package_scope"
	it.RequireNoDir(it.Path(base), "the sibling-owner package scope fixture must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	it.Write(it.Path(base, "lib/package.json"), `{"type":"module","imports":{"#internal":"./internal.ts","#contract":"./contract.d.ts"}}`)
	it.Write(it.Path(base, "lib/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts","api.d.ts"]}`)
	it.Write(it.Path(base, "lib/index.ts"), "import { value } from '#internal';\nexport const answer: number = value;\n")
	it.Write(it.Path(base, "lib/internal.ts"), "export const value: number = 42;\n")
	it.Write(it.Path(base, "lib/api.d.ts"), "export type { Value } from '#contract';\n")
	it.Write(it.Path(base, "lib/contract.d.ts"), "export interface Value { answer: number }\n")
	it.Write(it.Path(base, "app/tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`)
	it.Write(it.Path(base, "app/index.ts"), `import { answer } from '../lib/index.js';
import type { Value } from '../lib/api.js';
export const value: Value = { answer };
`)
	it.Write(it.Path(base, "app/BUILD.bazel"), `filegroup(
    name = "check",
    srcs = [":app"],
    output_group = "_validation",
)
`)
	for _, emit := range []string{"False", "True"} {
		it.Write(it.Path(base, "lib/BUILD.bazel"), fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_compile")
# keep
ts_compile(
    name = "lib",
    srcs = ["index.ts", "internal.ts", "api.d.ts", "contract.d.ts"],
    package_scopes = ["package.json"],
    emit = %s,
    tsconfig = "tsconfig.json",
    visibility = ["//visibility:public"],
)
`, emit))
		it.MustBazel("run", "//:gazelle", "--", base)
		target := "//" + base + "/app:app"
		requireLabels(it, "deps", target, []string{"//" + base + "/lib:lib"})
		scope := base + "/lib/package.json"
		query := fmt.Sprintf(`inputs("^%s/lib/package[.]json$", mnemonic("TsgoCheck", %s))`, base, target)
		var graph struct {
			Actions []struct {
				Arguments []string `json:"arguments"`
			} `json:"actions"`
		}
		if err := json.Unmarshal([]byte(it.BazelStdout("aquery", query, "--output=jsonproto")), &graph); err != nil {
			it.Fail("cannot read the sibling consumer's declared check inputs: %v", err)
		}
		if len(graph.Actions) != 1 || !slices.Contains(graph.Actions[0].Arguments, "-source="+scope) {
			it.Fail("emit=%s sibling consumer lacks the owner's original package scope as a declared input and source path: %+v", emit, graph.Actions)
		}
		it.MustBazel("build", "//"+base+"/app:check", "--run_validations=false", "--@rules_typescript//ts:lib_check")
		it.RequireFile(it.Bin(base, "app/app.tscheck"), "the sibling consumer check did not run")
	}
	joined := base + "/joined"
	it.Write(it.Path(joined, "package.json"), `{}`)
	it.Write(it.Path(joined, "tsconfig.json"), `{"compilerOptions":{"module":"nodenext","moduleResolution":"nodenext","types":[]},"files":["lib.ts"]}`)
	it.Write(it.Path(joined, "lib.ts"), "const value: 42 = 42;\nexport = value;\n")
	it.Write(it.Path(joined, "scope.test.ts"), "import value = require('./lib.js');\nexport const actual: 42 = value;\n")
	it.Write(it.Path(joined, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_config", "ts_test")
ts_config(name = "config", src = "tsconfig.json", module = "nodenext")
ts_compile(
    name = "lib",
    srcs = ["lib.ts"],
    package_scopes = ["package.json"],
    tsconfig = ":config",
    emit = True,
)
ts_test(
    name = "joined",
    srcs = ["scope.test.ts"],
    test_srcs = ["scope.test.ts"],
    tsconfig = ":config",
    deps = [":lib"],
    emit = True,
    runner = "@rules_typescript//ts/runners:node_test",
)
filegroup(name = "check", srcs = [":joined"], output_group = "_validation", testonly = True)
`)
	var graph struct {
		Actions []struct {
			Arguments []string `json:"arguments"`
		} `json:"actions"`
	}
	programs := labels(it, "native_program", "//"+joined+":joined")
	if len(programs) != 1 {
		it.Fail("joined public test has %v action owners, want one", programs)
	}
	query := fmt.Sprintf(`inputs("^%s/package[.]json$", mnemonic("TsgoCheck", %s))`, joined, programs[0])
	if err := json.Unmarshal([]byte(it.BazelStdout("aquery", query, "--output=jsonproto")), &graph); err != nil {
		it.Fail("cannot read the joined consumer's declared check inputs: %v", err)
	}
	if len(graph.Actions) != 1 || !slices.Contains(graph.Actions[0].Arguments, "-source="+joined+"/package.json") || !slices.Contains(graph.Actions[0].Arguments, "-source="+joined+"/lib.ts") {
		it.Fail("joined consumer lost the owner's original source or package scope: %+v", graph.Actions)
	}
	it.MustBazel("build", "//"+joined+":check", "--run_validations=false", "--@rules_typescript//ts:lib_check")
	it.RequireFile(testValidationStamp(it, "//"+joined+":joined", graph.Actions[0].Arguments), "the joined NodeNext consumer check did not run")
	it.Pass("sibling and joined consumers retain original package scope for source inputs and emitted passthrough declarations")
}

func workspaceMemberRejectsBorrowedSibling(it *harness.IT) {
	source := it.Path("packages/shared/src/index.ts")
	build := it.Path("packages/shared/BUILD.bazel")
	restoreSource, restoreBuild := it.Read(source), it.Read(build)
	defer it.Write(source, restoreSource)
	defer it.Write(build, restoreBuild)
	func() {
		rootBuild := it.Path("BUILD.bazel")
		consumer := it.Path("member/consumer.test.ts")
		defer it.Write(rootBuild, it.Read(rootBuild))
		defer it.Write(consumer, it.Read(consumer))
		defer it.Write(build, restoreBuild)
		it.RequireNoFile(it.Path("LICENSE"), "the standalone license must be staged by this check")
		it.RequireNoFile(it.Path("metadata.json"), "the standalone JSON asset must be staged by this check")
		it.RequireNoDir(it.Path("package-assets"), "the nested package asset must be staged by this check")
		defer os.Remove(it.Path("LICENSE"))
		defer os.Remove(it.Path("metadata.json"))
		defer os.RemoveAll(it.Path("package-assets"))
		it.Write(it.Path("LICENSE"), "workspace member license\n")
		it.Write(it.Path("metadata.json"), "{\"name\":\"shared\"}\n")
		it.Write(it.Path("package-assets/NOTICE"), "workspace member notice\n")
		it.Write(rootBuild, it.Read(rootBuild)+`
exports_files(["LICENSE", "metadata.json", "package-assets/NOTICE"], visibility = ["//packages/shared:__pkg__"])
`)
		file, err := rule.LoadFile(build, "packages/shared")
		if err != nil {
			it.Fail("cannot load the workspace member's asset owner: %v", err)
		}
		var member *rule.Rule
		for _, target := range file.Rules {
			if target.Kind() == "ts_compile" && target.Name() == "shared" {
				member = target
			}
		}
		if member == nil {
			it.Fail("the workspace member has no compile target for the asset check")
		}
		member.AddComment("# keep")
		member.SetAttr("srcs", append(member.AttrStrings("srcs"), "//:LICENSE", "//:package-assets/NOTICE"))
		member.SetAttr("data", []string{"//:LICENSE", "//:metadata.json", ":generated_metadata", "src/index.ts"})
		generated := rule.NewRule("genrule", "generated_metadata")
		generated.SetAttr("srcs", []string{"//:metadata.json"})
		generated.SetAttr("outs", []string{"generated-metadata.json"})
		generated.SetAttr("cmd", `cp "$(location //:metadata.json)" "$@"`)
		generated.Insert(file)
		it.Write(consumer, "import { readFileSync } from 'node:fs';\n"+it.Read(consumer)+`
it("retains standalone member assets at their package paths", () => {
  expect(readFileSync(new URL("../node_modules/shared/LICENSE", import.meta.url), "utf8")).toBe("workspace member license\n");
  expect(readFileSync(new URL("../node_modules/shared/metadata.json", import.meta.url), "utf8")).toBe('{"name":"shared"}\n');
  expect(readFileSync(new URL("../node_modules/shared/generated-metadata.json", import.meta.url), "utf8")).toBe('{"name":"shared"}\n');
  expect(readFileSync(new URL("../node_modules/shared/package-assets/NOTICE", import.meta.url), "utf8")).toBe("workspace member notice\n");
});
`)
		for _, emit := range []bool{false, true} {
			member.SetAttr("emit", emit)
			it.Write(build, string(file.Format()))
			it.MustBazel("run", "//:gazelle")
			requireLabels(it, "data", "//packages/shared:shared", []string{"//:LICENSE", "//:metadata.json", "//packages/shared:generated_metadata", "//packages/shared:src/index.ts"})
			it.MustBazel("test", "//member:member_test")
			it.RequireNoFile(it.Bin("packages/shared/src/index.ts"), "a source also listed in data was copied as an extra asset")
			it.Pass("the kept member with emit=%t retains src assets and explicit JSON asset bytes through its workspace npm link", emit)
		}
		member.SetAttr("emit", false)
		member.SetAttr("srcs", append(member.AttrStrings("srcs"), "//:metadata.json"))
		it.Write(build, string(file.Format()))
		log, err := it.BazelLog("workspace_member_json_source_and_data", "test", "//member:member_test")
		if err == nil || !log.Contains("npm_store_member cannot preserve the relative layout of 'metadata.json' outside member 'packages/shared'.") {
			log.Dump()
			it.Fail("an explicit data entry overrode the foreign JSON source's module path")
		}
	}()
	func() {
		manifest := it.Path("packages/shared/package.json")
		consumer := it.Path("member/consumer.test.ts")
		restoreManifest, restoreConsumer := it.Read(manifest), it.Read(consumer)
		defer it.Write(manifest, restoreManifest)
		defer it.Write(consumer, restoreConsumer)
		defer it.Write(source, restoreSource)
		defer it.Write(build, restoreBuild)
		it.RequireNoDir(it.Path("types"), "the foreign declaration package must be staged by this check")
		defer os.RemoveAll(it.Path("types"))
		it.Write(manifest, `{"name":"shared","version":"0.0.0","types":"./api.d.ts"}`)
		it.Write(consumer, `import type { MemberOptions } from "shared";
export const options: MemberOptions = { retries: 42 };
`)
		for _, nested := range []bool{false, true} {
			sources := `["package.json", "//types:api.d.ts"]`
			it.Write(it.Path("types/api.d.ts"), "export interface MemberOptions { retries: number; }\n")
			it.Write(it.Path("types/BUILD.bazel"), `exports_files(["api.d.ts"], visibility = ["//packages/shared:__pkg__"])
`)
			if nested {
				sources = `["package.json", "//types:api.d.ts", "//types:nested/options.d.ts"]`
				it.Write(it.Path("types/api.d.ts"), "export type { MemberOptions } from './nested/options.js';\n")
				it.Write(it.Path("types/nested/options.d.ts"), "export interface MemberOptions { retries: number; }\n")
				it.Write(it.Path("types/BUILD.bazel"), `exports_files(["api.d.ts", "nested/options.d.ts"], visibility = ["//packages/shared:__pkg__"])
`)
			}
			it.Write(build, loadTsCompile+`
ts_compile(
    name = "shared",
    srcs = `+sources+`,
    visibility = ["//visibility:public"],
)
`)
			log, err := it.BazelLog(fmt.Sprintf("workspace_member_foreign_declaration_nested_%t", nested), "build", "//member:member_test", "--output_groups=+_validation")
			if err == nil || !log.Contains("npm_store_member cannot preserve the relative layout of 'types/api.d.ts' outside member 'packages/shared'. Did you mean to move the file inside the member or import it through a separately published package?") {
				log.Dump()
				it.Fail("the declaration-only member accepted a foreign declaration layout (nested=%t)", nested)
			}
		}
		it.Pass("the declaration-only member rejects foreign declaration relocation, including nested relative imports")
		it.Write(it.Path("types/package.json"), `{"type":"module"}`)
		it.Write(it.Path("types/BUILD.bazel"), it.Read(it.Path("types/BUILD.bazel"))+`exports_files(["package.json"], visibility = ["//packages/shared:__pkg__"])
`)
		it.Write(manifest, strings.ReplaceAll(restoreManifest, ".ts\"", ".js\""))
		it.Write(consumer, restoreConsumer)
		it.Write(source, "import type { MemberOptions } from '../../../types/api.js';\n"+
			"const options: MemberOptions = { retries: 42 };\nexport const answer: number = options.retries;\n"+restoreSource)
		it.Write(build, restoreBuild)
		it.MustBazel("run", "//:gazelle")
		inputs := labels(it, "srcs", "//packages/shared:shared")
		requireLabels(it, "type_inputs", "//packages/shared:shared", []string{"//types:package.json"})
		for _, input := range []string{"//types:api.d.ts"} {
			if !slices.Contains(inputs, input) {
				it.Fail("the emitted member lost its foreign declaration scope input %s", input)
			}
		}
		log, err := it.BazelLog("emitted_member_foreign_checker_declaration", "build", "//member:member_test", "--output_groups=+_validation")
		if err == nil || !log.Contains("npm_store_member cannot preserve the relative layout") || !log.Contains("types/api.d.ts") {
			log.Dump()
			it.Fail("the emitted member published a foreign declaration outside its package")
		}
		it.Pass("the emitted member rejects foreign declaration publication independently of compiler-only scope")
	}()
	config := it.Path("packages/shared/tsconfig.json")
	restoreConfig := it.Read(config)
	defer it.Write(config, restoreConfig)
	it.Write(config, strings.Replace(restoreConfig, `"lib": ["es2022"]`, `"lib": ["es2022"], "allowJs": true, "resolveJsonModule": true`, 1))
	defer os.RemoveAll(it.Path("packages/fixtures"))
	for _, input := range []struct{ file, contents, imported string }{
		{"value.ts", "export const name: string = 'shared';\n", "value.js"},
		{"value.mjs", "export const name = 'shared';\n", "value.mjs"},
		{"value.json", "{\"name\":\"shared\"}\n", "value.json"},
	} {
		it.Write(it.Path("packages/fixtures", input.file), input.contents)
		it.Write(it.Path("packages/fixtures/BUILD.bazel"), fmt.Sprintf("exports_files([%q], visibility = [\"//packages/shared:__pkg__\"])\n", input.file))
		it.Write(source, "import { name as borrowed } from '../../fixtures/"+input.imported+"';\n"+
			strings.Replace(restoreSource, `"shared"`, "borrowed", 1))
		it.MustBazel("run", "//:gazelle")
		if !slices.Contains(labels(it, "srcs", "//packages/shared:shared"), "//packages/fixtures:"+input.file) {
			it.Fail("the workspace member did not acquire the borrowed sibling %s", input.file)
		}
		log, err := it.BazelLog("workspace_member_borrowed_"+input.file, "test", "//member:member_test")
		diagnostic := fmt.Sprintf("npm_store_member cannot preserve the relative layout of 'packages/fixtures/%s' outside member 'packages/shared'. Did you mean to move the file inside the member or import it through a separately published package?", input.file)
		if err == nil || !log.Contains(diagnostic) {
			log.Dump()
			it.Fail("the workspace member store did not reject its unsupported sibling %s layout", input.file)
		}
	}
	func() {
		for _, directory := range []string{"packages/borrowed", "packages/contracts"} {
			it.RequireNoDir(it.Path(directory), "the canonical dependency fixture must be staged by this check")
			defer os.RemoveAll(it.Path(directory))
		}
		writeMember := func(borrowed bool, deps ...string) {
			file, err := rule.LoadData(build, "packages/shared", []byte(restoreBuild))
			if err != nil {
				it.Fail("cannot load the member's original compiler: %v", err)
			}
			configured := false
			for _, target := range file.Rules {
				if target.Kind() == "ts_compile" && target.Name() == "shared" {
					configured = true
					target.AddComment("# keep")
					target.SetAttr("emit", true)
					if borrowed {
						target.SetAttr("srcs", append(target.AttrStrings("srcs"), "//packages/fixtures:value.ts"))
					}
					target.SetAttr("deps", append(target.AttrStrings("deps"), deps...))
				}
			}
			if !configured {
				it.Fail("the workspace member's compiler target is missing")
			}
			it.Write(build, string(file.Format()))
		}
		borrowedSource := "import { name as borrowed } from '../../fixtures/value.js';\n" + strings.Replace(restoreSource, `"shared"`, "borrowed", 1)
		it.Write(source, borrowedSource)
		it.Write(it.Path("packages/fixtures/value.ts"), "export const name: string = 'shared';\n")
		it.Write(it.Path("packages/fixtures/BUILD.bazel"), `exports_files(["value.ts"], visibility = ["//visibility:public"])`)
		writeMember(true)
		it.MustBazel("test", "//member:member_test", "--output_groups=+declarations,+_validation")
		it.Write(it.Path("packages/borrowed/offset.ts"), "export const offset: number = 0;\n")
		it.Write(it.Path("packages/borrowed/BUILD.bazel"), `exports_files(["offset.ts"], visibility = ["//visibility:public"])`)
		it.Write(it.Path("packages/fixtures/value.ts"), "import { offset } from '../borrowed/offset.js';\nexport const name: string = 'shared'.slice(offset);\n")
		it.Write(it.Path("packages/fixtures/BUILD.bazel"), loadTsCompile+`
ts_compile(name = "runtime", srcs = ["value.ts", "//packages/borrowed:offset.ts"], emit = True, visibility = ["//visibility:public"])
`)
		writeMember(false, "//packages/fixtures:runtime")
		const remedy = "Use a separately published npm dependency or include its sources in this member."
		log, err := it.BazelLog("workspace_member_canonical_runtime", "build", "//member:member_test", "--nobuild")
		if err == nil || !log.Contains("requires canonical dependency 'packages/fixtures/fixtures/value.js' through 'packages/shared/fixtures/value.js'") || !log.Contains(remedy) {
			log.Dump()
			it.Fail("the member store did not reject copying a canonical runtime dependency")
		}
		it.Write(it.Path("packages/fixtures/value.ts"), "export const name: string = 'shared';\n")
		it.Write(it.Path("packages/fixtures/BUILD.bazel"), `exports_files(["value.ts"], visibility = ["//visibility:public"])`)
		it.Write(it.Path("packages/contracts/api.d.ts"), "export interface Options { retries: number }\n")
		it.Write(it.Path("packages/contracts/BUILD.bazel"), loadTsCompile+`
ts_compile(name = "api", srcs = ["api.d.ts"], visibility = ["//visibility:public"])
`)
		it.Write(source, "import type { Options } from '../../contracts/api.js';\nexport const options: Options = { retries: 42 };\n"+borrowedSource)
		writeMember(true, "//packages/contracts:api")
		log, err = it.BazelLog("workspace_member_canonical_declaration", "build", "//member:member_test", "--nobuild")
		if err == nil || !log.Contains("requires canonical dependency 'packages/contracts/api.d.ts' through 'packages/shared/contracts/api.d.ts'") || !log.Contains(remedy) {
			log.Dump()
			it.Fail("the member store did not reject copying a declaration-only canonical dependency")
		}
	}()
	it.Write(source, restoreSource)
	it.Write(build, restoreBuild)
	it.Write(config, restoreConfig)
	it.MustBazel("run", "//:gazelle")
	requireLabels(it, "deps", "//member:member_test", withRootManifest())
	it.MustBazel("test", "//member:member_test")
	it.Pass("the restored member supplies the asserted shared value through the workspace npm link")
}

func samePackageOwnerSuppliesForeignJSON(it *harness.IT) {
	build := it.Path("foreign_json/BUILD.bazel")
	restore := it.Read(build)
	defer it.Write(build, restore)
	it.Write(build, restore+`
# keep
ts_compile(
    name = "other",
    srcs = ["//foreign_fixtures:value.json"],
    visibility = ["//foreign_json:__pkg__"],
)
`)
	it.MustBazel("run", "//:gazelle")
	requireLabels(it, "srcs", "//foreign_json:foreign_json", []string{
		"//foreign_fixtures:a.ts", "//foreign_fixtures:companion.mjs", "//foreign_fixtures:nested/b.ts",
		"//foreign_json:index.ts",
	})
	requireLabels(it, "deps", "//foreign_json:foreign_json", []string{"//foreign_json:other", "//src/i18n:manifest"})
	requireLabels(it, "deps", "//foreign_consumer:foreign_consumer_test", withRootManifest("//foreign_json:foreign_json"))
	it.MustBazel("test", "//foreign_consumer:foreign_consumer_test")
	it.Pass("a private same-package owner supplies the imported foreign JSON through its public dependency")
}

func generatedConfigStagesScalarRuntime(it *harness.IT) {
	for _, generated := range []struct {
		producer, extension string
		compiled            bool
	}{
		{"ts_codegen", ".ts", false},
		{"ts_codegen", ".mts", false},
		{"ts_codegen", ".ts", true},
		{"genrule", ".ts", true},
	} {
		producerKind := generated.producer
		typeScriptFile := "generated/typescript" + generated.extension
		ordinaryInput := producerKind == "ts_codegen" && generated.extension == ".ts" && !generated.compiled
		func() {
			for _, rel := range []string{"configured/BUILD.bazel", "configured/test/BUILD.bazel", "configured/vitest.config.mts", "configured/test/answer.test.ts", "configured/tsconfig.json", "configured/test/tsconfig.json", "configured/src/index.ts", "tools/json_gen.sh"} {
				file := it.Path(rel)
				defer it.Write(file, it.Read(file))
			}
			it.Write(it.Path("tools/json_gen.sh"), "#!/usr/bin/env bash\nset -euo pipefail\nwhile (( $# )); do cp -- \"$1\" \"$2\"; shift 2; done\n")
			generatedDir := it.Path("configured/generated")
			it.RequireNoDir(generatedDir, "the config output directory must be staged by this check")
			defer os.RemoveAll(generatedDir)
			if err := os.MkdirAll(generatedDir, 0o755); err != nil {
				it.Fail("cannot create the config output directory: %v", err)
			}
			input := it.Path("configured/config-runtime.txt")
			it.RequireNoFile(input, "the config generator input must be staged by this check")
			defer os.Remove(input)
			helperInput := it.Path("configured/config-helper.txt")
			it.RequireNoFile(helperInput, "the config helper input must be staged by this check")
			defer os.Remove(helperInput)
			declarationInput := it.Path("configured/config-declaration.txt")
			if producerKind == "ts_codegen" {
				it.RequireNoFile(declarationInput, "the generated config declaration input must be staged by this check")
				defer os.Remove(declarationInput)
				it.Write(declarationInput, "export declare const value: number;\n")
			}
			standaloneInput := it.Path("configured/config-standalone.txt")
			it.RequireNoFile(standaloneInput, "the standalone config input must be staged by this check")
			defer os.Remove(standaloneInput)
			jsonInput := it.Path("configured/config-data.txt")
			it.RequireNoFile(jsonInput, "the JSON config generator input must be staged by this check")
			defer os.Remove(jsonInput)
			typeScriptInput := it.Path("configured/config-typescript.txt")
			it.RequireNoFile(typeScriptInput, "the TypeScript config generator input must be staged by this check")
			defer os.Remove(typeScriptInput)
			typeScriptHelperInput := it.Path("configured/config-typescript-helper.txt")
			it.RequireNoFile(typeScriptHelperInput, "the TypeScript helper input must be staged by this check")
			defer os.Remove(typeScriptHelperInput)
			build := it.Path("configured/BUILD.bazel")
			file, err := rule.LoadData(build, "configured", []byte(loadTsCodegen+it.Read(build)+`
# gazelle:exclude generated/value.mjs
# gazelle:exclude generated/value.d.mts
# gazelle:exclude generated/helper.mjs
# gazelle:exclude generated/standalone.mjs
# gazelle:exclude generated/value.json
# gazelle:exclude generated/typescript.ts
# gazelle:exclude generated/typescript.mts
# gazelle:exclude generated/typescript-helper.mjs
# gazelle:exclude generated/stale-secret.mjs
# gazelle:exclude generated/stale-secret.ts
`))
			if err != nil {
				it.Fail("cannot load generated config producers: %v", err)
			}
			for _, output := range []struct{ name, src, out string }{
				{"config_runtime", "config-runtime.txt", "generated/value.mjs"},
				{"config_helper", "config-helper.txt", "generated/helper.mjs"},
				{"config_standalone", "config-standalone.txt", "generated/standalone.mjs"},
				{"config_data", "config-data.txt", "generated/value.json"},
				{"config_typescript", "config-typescript.txt", typeScriptFile},
			} {
				if producerKind == "ts_codegen" && output.name == "config_helper" {
					continue
				}
				name := "make_" + output.name
				producer := rule.NewRule(producerKind, name)
				producer.SetAttr("srcs", []string{output.src})
				producer.SetAttr("outs", []string{output.out})
				producer.SetAttr("visibility", []string{"//configured/test:__pkg__"})
				if producerKind == "ts_codegen" {
					producer.SetAttr("args", []string{"{srcs}", "{out}"})
					producer.SetAttr("generator", "//:json_gen")
					if output.name == "config_runtime" {
						producer.SetAttr("srcs", []string{output.src, "config-helper.txt", "config-declaration.txt"})
						producer.SetAttr("outs", []string{output.out, "generated/helper.mjs", "generated/value.d.mts"})
						producer.SetAttr("args", []string{"{srcs_dir}/config-runtime.txt", "{outs_dir}/value.mjs", "{srcs_dir}/config-helper.txt", "{outs_dir}/helper.mjs", "{srcs_dir}/config-declaration.txt", "{outs_dir}/value.d.mts"})
					} else if output.name == "config_typescript" {
						producer.SetAttr("srcs", []string{output.src, "config-typescript-helper.txt"})
						producer.SetAttr("outs", []string{output.out, "generated/typescript-helper.mjs"})
						producer.SetAttr("args", []string{"{srcs_dir}/config-typescript.txt", "{outs_dir}/typescript" + generated.extension, "{srcs_dir}/config-typescript-helper.txt", "{outs_dir}/typescript-helper.mjs"})
					}
				} else {
					producer.SetAttr("cmd", fmt.Sprintf(`"$(location //:json_gen)" "$(location %s)" "$@"`, output.src))
					producer.SetAttr("tools", []string{"//:json_gen"})
				}
				if generated.compiled && output.name == "config_typescript" || producerKind == "genrule" && (output.name == "config_runtime" || output.name == "config_helper") {
					compiler := rule.NewRule("ts_compile", output.name)
					compiler.AddComment("# keep")
					compiler.SetAttr("srcs", []string{":" + name})
					compiler.SetAttr("emit", false)
					if output.name == "config_runtime" {
						compiler.SetAttr("deps", []string{":config_helper"})
					}
					compiler.SetAttr("visibility", []string{"//configured/test:__pkg__"})
					compiler.Insert(file)
				}
				producer.Insert(file)
			}
			if ordinaryInput {
				for _, rel := range []string{"configured/scalar-bridge.d.ts", "configured/test/fallback.mjs"} {
					path := it.Path(rel)
					it.RequireNoFile(path, "the scalar source fixture must be staged by this check")
					defer os.Remove(path)
				}
				it.Write(it.Path("configured/tsconfig.json"), `{"extends":"../tsconfig.json","compilerOptions":{"allowJs":true,"checkJs":true},"include":["src"]}`)
				it.Write(it.Path("configured/test/tsconfig.json"), `{"extends":"../tsconfig.json","compilerOptions":{"paths":{"#runtime":["../generated/standalone.mjs","./fallback.mjs"]}},"include":["*.ts"]}`)
				it.Write(it.Path("configured/src/index.ts"), "export { standaloneValue } from '../generated/standalone.mjs';\n")
				it.Write(it.Path("configured/scalar-bridge.d.ts"), "export { standaloneValue } from './src/index.js';\n")
				it.Write(it.Path("configured/test/fallback.mjs"), "export const standaloneValue = -1;\n")
				bridge := rule.NewRule("ts_compile", "scalar_bridge")
				bridge.AddComment("# keep")
				bridge.SetAttr("srcs", []string{"scalar-bridge.d.ts"})
				bridge.SetAttr("deps", []string{":configured"})
				bridge.SetAttr("emit", true)
				bridge.SetAttr("visibility", []string{"//configured/test:__pkg__"})
				bridge.Insert(file)
				test := it.Path("configured/test/answer.test.ts")
				it.Write(test, it.Read(test)+`
import { standaloneValue } from '#runtime';
import type { standaloneValue as WrappedValue } from '../scalar-bridge.js';
const generatedNumber: typeof WrappedValue = standaloneValue;
it('reads the generated scalar instead of its fallback', () => {
  expect(generatedNumber).toBe(1);
});
`)
			}
			it.Write(build, string(file.Format()))
			config := it.Path("configured/vitest.config.mts")
			it.Write(config, `import { value } from './generated/value.mjs';
import { standaloneValue } from './generated/standalone.mjs';
import data from './generated/value.json' with { type: 'json' };
import { typeScriptValue } from './`+typeScriptFile+`';
`+strings.Replace(it.Read(config), `"export default 42;"`, `"export default " + (value + standaloneValue + data.offset + typeScriptValue) + ";"`, 1))
			for _, present := range []bool{false, true} {
				value := 42
				runtimeValue := 38
				typeScriptValue := 2
				if present {
					value = 43
					runtimeValue = 39
					typeScriptValue = 3
					it.Write(it.Path("configured/generated/value.mjs"), "export { value } from './stale-secret.mjs';\n")
					if producerKind == "ts_codegen" {
						it.Write(it.Path("configured/generated/value.d.mts"), "export { value } from './stale-secret.mjs';\n")
					}
					it.Write(it.Path("configured/generated/helper.mjs"), "export const helper = -1;\n")
					it.Write(it.Path("configured/generated/standalone.mjs"), "export { standaloneValue } from './stale-secret.mjs';\n")
					it.Write(it.Path("configured/generated/value.json"), `{"offset":-1}`)
					it.Write(it.Path("configured/"+typeScriptFile), "export { typeScriptValue } from './stale-secret.ts';\n")
					if producerKind == "ts_codegen" {
						it.Write(it.Path("configured/generated/typescript-helper.mjs"), "export const typeScriptHelper = -1;\n")
					}
					it.Write(it.Path("configured/generated/stale-secret.mjs"), "export const value = -1; export const standaloneValue = -1;\n")
					it.Write(it.Path("configured/generated/stale-secret.ts"), "export const typeScriptValue: number = -1;\n")
					it.Replace(it.Path("configured/test/answer.test.ts"), "toBe(42)", "toBe(43)")
				}
				it.Write(input, "import { helper } from './helper.mjs'; export const value = helper + 1;\n")
				it.Write(helperInput, fmt.Sprintf("export const helper = %d;\n", runtimeValue-1))
				it.Write(standaloneInput, "export const standaloneValue = 1;\n")
				it.Write(jsonInput, fmt.Sprintf("{\"offset\":%d}\n", value-runtimeValue-typeScriptValue-1))
				if producerKind == "ts_codegen" {
					it.Write(typeScriptInput, "import { typeScriptHelper } from './typescript-helper.mjs'; export const typeScriptValue: number = typeScriptHelper;\n")
					it.Write(typeScriptHelperInput, fmt.Sprintf("export const typeScriptHelper = %d;\n", typeScriptValue))
				} else {
					it.Write(typeScriptInput, fmt.Sprintf("export const typeScriptValue: number = %d;\n", typeScriptValue))
				}
				it.MustBazel("run", "//:gazelle")
				wantSources := []string{"//configured/test:answer.test.ts", "//configured/test:virtual.d.ts"}
				if ordinaryInput {
					wantSources = append(wantSources, "//configured:generated/standalone.mjs")
					requireLabels(it, "srcs", "//configured:configured", []string{
						"//configured:config-data.txt", "//configured:config-declaration.txt", "//configured:config-helper.txt", "//configured:config-runtime.txt",
						"//configured:config-standalone.txt", "//configured:config-typescript-helper.txt", "//configured:config-typescript.txt",
						"//configured:generated/standalone.mjs", "//configured:src/index.ts",
					})
					requireLabels(it, "package_scopes", "//configured:configured", []string{"//configured:package.json"})
					requireLabels(it, "deps", "//configured:scalar_bridge", []string{"//configured:configured"})
					library, err := rule.LoadFile(build, "configured")
					if err != nil {
						it.Fail("cannot read the generated scalar supplier: %v", err)
					}
					for _, target := range library.Rules {
						if target.Name() == "configured" && target.Attr("emit") != nil {
							emit, literal := target.Attr("emit").(*bzl.Ident)
							if !literal || emit.Name != "False" {
								it.Fail("the scalar supplier must retain source-mode inputs")
							}
						}
					}
				}
				requireLabels(it, "srcs", "//configured/test:test_test", wantSources)
				configSrcs := []string{"//configured:generated/standalone.mjs", "//configured:" + typeScriptFile, "//configured:generated/value.json", "//configured:package.json"}
				if producerKind == "genrule" {
					configSrcs = append(configSrcs, "//configured:generated/value.mjs")
					slices.Sort(configSrcs)
				}
				requireLabels(it, "config_srcs", "//configured/test:test_test", configSrcs)
				if producerKind == "ts_codegen" {
					typeScriptOwner := "//configured:make_config_typescript"
					if generated.compiled {
						typeScriptOwner = "//configured:config_typescript"
					}
					wantDeps := []string{typeScriptOwner, "//configured:make_config_data", "//configured:make_config_runtime", "//configured:make_config_standalone", "@npm//configured:vitest"}
					if ordinaryInput {
						wantDeps = append(wantDeps, "//configured:scalar_bridge")
					}
					sort.Strings(wantDeps)
					requireLabels(it, "deps", "//configured/test:test_test", wantDeps)
				} else {
					requireLabels(it, "deps", "//configured/test:test_test", []string{"//configured:config_runtime", "//configured:config_typescript", "@npm//configured:vitest"})
					requireLabels(it, "deps", "//configured:config_runtime", []string{"//configured:config_helper"})
				}
				it.MustBazel("test", "//configured/test:test_test", "--output_groups=+_validation", "--noenable_runfiles")
				if ordinaryInput && !present {
					test := it.Path("configured/test/answer.test.ts")
					restore := it.Read(test)
					it.Replace(test, "generatedNumber: typeof WrappedValue", "generatedNumber: string")
					log, err := it.BazelLog("generated_scalar_js_types", "build", "//configured/test:test_test", "--output_groups=+_validation")
					it.Write(test, restore)
					if err == nil || !log.Contains("TS2322") {
						log.Dump()
						it.Fail("the generated JavaScript input did not reject its number value assigned to string")
					}
				}
			}
			it.Pass("config imports use scalar producers and the runtime helper closure with absent and stale checkout copies (%s, %s, compiled=%t)", producerKind, generated.extension, generated.compiled)
		}()
	}
}

func generatedRootConfigRetainsRuntimeOwner(it *harness.IT) {
	for _, rel := range []string{"configured/BUILD.bazel", "configured/test/BUILD.bazel", "configured/vitest.config.mts", "configured/test/answer.test.ts"} {
		file := it.Path(rel)
		defer it.Write(file, it.Read(file))
	}
	for _, rel := range []string{"configured/config-root.txt", "configured/helper.mjs", "configured/vitest.config.ts"} {
		file := it.Path(rel)
		it.RequireNoFile(file, "the generated root config fixture must be staged by this check")
		defer os.Remove(file)
	}
	authoredConfig := it.Path("configured/vitest.config.mts")
	configSource := it.Read(authoredConfig)
	if err := os.Remove(authoredConfig); err != nil {
		it.Fail("cannot remove the authored config before the generated case: %v", err)
	}
	config := it.Path("configured/vitest.config.ts")
	it.Write(it.Path("configured/helper.mjs"), "export const helper = 40;\n")
	build := it.Path("configured/BUILD.bazel")
	file, err := rule.LoadFile(build, "configured")
	if err != nil {
		it.Fail("cannot load the generated root config producer: %v", err)
	}
	producer := rule.NewRule("genrule", "make_config")
	producer.SetAttr("srcs", []string{"config-root.txt"})
	producer.SetAttr("outs", []string{"vitest.config.ts"})
	producer.SetAttr("cmd", `"$(location //:json_gen)" "$(location config-root.txt)" "$@"`)
	producer.SetAttr("tools", []string{"//:json_gen"})
	producer.SetAttr("visibility", []string{"//configured/test:__pkg__"})
	producer.Insert(file)
	compiler := rule.NewRule("ts_compile", "config_runtime")
	compiler.AddComment("# keep")
	compiler.SetAttr("srcs", []string{":make_config", "helper.mjs"})
	compiler.SetAttr("emit", false)
	compiler.SetAttr("visibility", []string{"//configured/test:__pkg__"})
	compiler.Insert(file)
	it.Write(build, string(file.Format()))

	// A generated config can be selected before its output exists in the checkout.
	build = it.Path("configured/test/BUILD.bazel")
	file, err = rule.LoadFile(build, "configured/test")
	if err != nil {
		it.Fail("cannot load the generated root config consumer: %v", err)
	}
	for _, target := range file.Rules {
		if target.Kind() == "ts_test" && target.Name() == "test_test" {
			target.SetAttr("config", "//configured:vitest.config.ts")
			target.AttrComments("config").Suffix = []bzl.Comment{{Token: "# keep"}}
		}
	}
	it.Write(build, string(file.Format()))
	for _, present := range []bool{false, true} {
		value := 42
		if present {
			value = 43
			it.Write(config, "import './stale-secret.mjs'; throw new Error('stale generated root config'); export default {};\n")
			it.Replace(it.Path("configured/test/answer.test.ts"), "toBe(42)", "toBe(43)")
		}
		it.Write(it.Path("configured/config-root.txt"), "import { helper } from './helper.mjs';\n"+
			strings.Replace(configSource, `"export default 42;"`, fmt.Sprintf(`"export default " + (helper + %d) + ";"`, value-40), 1))
		it.MustBazel("run", "//:gazelle")
		requireLabels(it, "config", "//configured/test:test_test", []string{"//configured:vitest.config.ts"})
		requireLabels(it, "srcs", "//configured/test:test_test", []string{"//configured/test:answer.test.ts", "//configured/test:virtual.d.ts"})
		requireLabels(it, "config_srcs", "//configured/test:test_test", []string{"//configured:package.json"})
		requireLabels(it, "deps", "//configured/test:test_test", []string{"//configured:config_runtime", "@npm//configured:vitest"})
		it.MustBazel("test", "//configured/test:test_test", "--noenable_runfiles")
	}
	it.Pass("a generated root config retains its declared runtime owner with absent and poisoned stale checkout copies")
}

func mixedSourceRootsDeclareAndRun(it *harness.IT) {
	rootBuild := it.Path("BUILD.bazel")
	file, err := rule.LoadFile(rootBuild, "")
	if err != nil {
		it.Fail("cannot load emission consumer: %v", err)
	}
	var defs *rule.Load
	for _, load := range file.Loads {
		if load.Name() == "@rules_typescript//ts:defs.bzl" {
			defs = load
			break
		}
	}
	if defs == nil {
		defs = rule.NewLoad("@rules_typescript//ts:defs.bzl")
		defs.Insert(file, 0)
	}
	defs.Add("ts_binary")
	binary := rule.NewRule("ts_binary", "foreign_emitted")
	binary.SetAttr("entry_point", "//foreign_json")
	binary.Insert(file)
	caller := rule.NewRule("ts_binary", "foreign_consumer_emitted")
	caller.SetAttr("entry_point", "//foreign_consumer")
	caller.Insert(file)
	it.Write(rootBuild, string(file.Format()))
	it.Write(it.Path("foreign_consumer/index.ts"), `import { answer, generatedLocales } from '../foreign_json/index.js';
export const value: number = answer;
if (value !== 42 || generatedLocales.join(',') !== 'en,sv') throw new Error('native caller lost its mixed-root dependency');
console.log('native mixed dependency: ' + value + ' ' + generatedLocales.join(','));
`)
	it.Replace(it.Path("foreign_json/index.ts"), `import manifest from "../src/i18n/generated-manifest.json";`, `import manifest from "../src/i18n/generated-manifest.json" with { type: "json" };`)
	it.Replace(it.Path("foreign_fixtures/nested/b.ts"), `import fixture from "../value.json";`, `import fixture from "../value.json" with { type: "json" };`)
	it.Replace(it.Path("foreign_json/index.ts"), `export { answer } from "../foreign_fixtures/a.js";`, `import { answer } from "../foreign_fixtures/a.js";
export { answer };`)
	it.Write(it.Path("foreign_json/index.ts"), it.Read(it.Path("foreign_json/index.ts"))+`
if (answer !== 42 || generatedLocales.join(',') !== 'en,sv') throw new Error('mixed source roots lost their runtime modules or generated JSON');
console.log('mixed source roots: ' + answer + ' ' + generatedLocales.join(','));
`)
	cycleFiles := []string{"foreign_fixtures/BUILD.bazel", "foreign_json/index.ts", "foreign_consumer/index.ts", "tools/json_gen.sh"}
	cycleOriginals := map[string]string{}
	for _, file := range cycleFiles {
		cycleOriginals[file] = it.Read(it.Path(file))
	}
	it.Write(it.Path("tools/json_gen.sh"), "#!/usr/bin/env bash\nset -euo pipefail\nwhile (( $# )); do cp -- \"$1\" \"$2\"; shift 2; done\n")
	cycleInputs := []string{"cycle-entry.txt", "cycle-back.txt"}
	for _, file := range cycleInputs {
		input := it.Path("foreign_fixtures", file)
		it.RequireNoFile(input, "the generated cycle input must be staged by this check")
		defer os.Remove(input)
	}
	for _, file := range []string{"cycle.mjs", "cycle-back.mjs"} {
		it.RequireNoFile(it.Path("foreign_fixtures", file), "a checkout module must not supply the generated cycle")
		it.RequireNoFile(it.Bin("foreign_fixtures", file), "a prior output must not supply the generated cycle")
	}
	it.Write(it.Path("foreign_fixtures/cycle-entry.txt"), `const key = Symbol.for('generated-cycle.initializations');
globalThis[key] = (globalThis[key] ?? 0) + 1;
export const identity = {};
export const initializations = () => globalThis[key];
export async function identityThroughCycle() {
  const sibling = './cycle-back.mjs';
  return (await import(sibling)).identityFromEntry();
}
`)
	it.Write(it.Path("foreign_fixtures/cycle-back.txt"), `import { identity } from './cycle.mjs';
export const identityFromEntry = () => identity;
`)
	it.Write(it.Path("foreign_fixtures/BUILD.bazel"), loadTsCodegen+cycleOriginals["foreign_fixtures/BUILD.bazel"]+`
ts_codegen(
    name = "cycle",
    srcs = ["cycle-entry.txt", "cycle-back.txt"],
    outs = ["cycle.mjs", "cycle-back.mjs"],
    args = ["{srcs_dir}/cycle-entry.txt", "{outs_dir}/cycle.mjs", "{srcs_dir}/cycle-back.txt", "{outs_dir}/cycle-back.mjs"],
    generator = "//:json_gen",
    visibility = ["//foreign_json:__pkg__"],
)
`)
	it.Write(it.Path("foreign_json/index.ts"), cycleOriginals["foreign_json/index.ts"]+`
import { identity as generatedCycleIdentity, identityThroughCycle as generatedCycleRoundTrip, initializations as generatedCycleInitializations } from '../foreign_fixtures/cycle.mjs';
export { generatedCycleIdentity, generatedCycleRoundTrip, generatedCycleInitializations };
if (await generatedCycleRoundTrip() !== generatedCycleIdentity || generatedCycleInitializations() !== 1) throw new Error('generated cycle duplicated its canonical module');
console.log('generated cycle retained one module initialization');
`)
	it.Write(it.Path("foreign_consumer/index.ts"), cycleOriginals["foreign_consumer/index.ts"]+`
import { generatedCycleIdentity, generatedCycleRoundTrip, generatedCycleInitializations } from '../foreign_json/index.js';
if (await generatedCycleRoundTrip() !== generatedCycleIdentity || generatedCycleInitializations() !== 1) throw new Error('native caller duplicated the generated module');
console.log('native caller retained the generated cycle identity');
`)
	it.MustBazel("run", "//:gazelle")
	compiler, err := rule.LoadFile(it.Path("foreign_json/BUILD.bazel"), "foreign_json")
	if err != nil {
		it.Fail("cannot load the generated mixed-root compiler: %v", err)
	}
	promoted := false
	for _, target := range compiler.Rules {
		if target.Kind() == "ts_compile" && target.Name() == "foreign_json" {
			emit, literal := target.Attr("emit").(*bzl.Ident)
			promoted = literal && emit.Name == "True"
			if !slices.Equal(target.AttrStrings("deps"), []string{"//:root", "//foreign_fixtures:cycle", "//src/i18n:manifest"}) {
				it.Fail("Gazelle did not retain the mixed-root runtime producers and package scope: %v", target.AttrStrings("deps"))
			}
		}
	}
	if !promoted {
		it.Fail("Gazelle did not promote the native binary's mixed-root compiler to emission")
	}
	requireRootScopePublisher(it)
	requireLabels(it, "srcs", "//foreign_json:foreign_json", []string{
		"//foreign_fixtures:a.ts", "//foreign_fixtures:companion.mjs", "//foreign_fixtures:cycle.mjs", "//foreign_fixtures:nested/b.ts",
		"//foreign_fixtures:value.json", "//foreign_json:index.ts",
	})
	it.MustBazel("build", "//:foreign_emitted", "//:foreign_consumer_emitted", "//foreign_consumer:foreign_consumer_test", "--output_groups=+declarations,+_validation")
	for _, artifact := range []struct{ path, symbol string }{
		{"foreign_json/index.js", "generatedLocales"},
		{"foreign_json/index.d.ts", "generatedLocales"},
		{"foreign_fixtures/a.js", "answer"},
		{"foreign_fixtures/a.d.ts", "answer"},
		{"foreign_fixtures/nested/b.js", "answer"},
		{"foreign_fixtures/nested/b.d.ts", "answer: number"},
		{"foreign_fixtures/companion.mjs", "offset"},
		{"foreign_fixtures/companion.d.mts", "offset"},
	} {
		it.RequireContains(it.Bin("foreign_json", artifact.path), artifact.symbol, "mixed-root output %s lost its source association", artifact.path)
	}
	consumer := "//foreign_consumer:foreign_consumer_test"
	programs := labels(it, "native_program", consumer)
	if len(programs) != 1 {
		it.Fail("%s has %v action owners, want one", consumer, programs)
	}
	var graph struct {
		Actions []struct {
			Arguments []string `json:"arguments"`
		} `json:"actions"`
	}
	query := fmt.Sprintf(`mnemonic("TsgoCheck", %s)`, programs[0])
	if err := json.Unmarshal([]byte(it.BazelStdout("aquery", query, "--output=jsonproto")), &graph); err != nil {
		it.Fail("cannot read %s consumer check: %v", consumer, err)
	}
	if len(graph.Actions) != 1 {
		it.Fail("%s has %v consumer checks, want one", consumer, graph.Actions)
	}
	it.RequireFile(testValidationStamp(it, consumer, graph.Actions[0].Arguments), "the downstream declaration consumer did not type-check")
	it.MustBazel("test", "//foreign_consumer:foreign_consumer_test")
	it.RequireNoFile(it.Bin("foreign_fixtures/a.js"), "a prior separate owner must not satisfy the mixed-root import")
	it.RequireNoFile(it.Path("foreign_fixtures/a.js"), "a checkout JavaScript file must not satisfy the mixed-root import")
	log, err := it.BazelLog("mixed_root_native_emission", "run", "//:foreign_emitted")
	if err != nil || !log.Contains("mixed source roots: 42 en,sv") || !log.Contains("generated cycle retained one module initialization") {
		log.Dump()
		it.Fail("the generated mixed-root compiler did not run its borrowed modules and generated JSON")
	}
	log, err = it.BazelLog("mixed_root_native_caller", "run", "//:foreign_consumer_emitted")
	if err != nil || !log.Contains("native mixed dependency: 42 en,sv") || !log.Contains("native caller retained the generated cycle identity") {
		log.Dump()
		it.Fail("a single-root native caller lost its mixed-root dependency's canonical emitted files")
	}
	for _, file := range cycleFiles {
		it.Write(it.Path(file), cycleOriginals[file])
	}
	for _, file := range cycleInputs {
		if err := os.Remove(it.Path("foreign_fixtures", file)); err != nil {
			it.Fail("cannot remove the generated cycle input %s: %v", file, err)
		}
	}
	// OXC's isolatedDeclarations cannot coexist with allowJs.
	if err := os.Rename(it.Path("foreign_fixtures/companion.mjs"), it.Path("foreign_fixtures/companion.ts")); err != nil {
		it.Fail("cannot convert the emission fixture's companion to TypeScript: %v", err)
	}
	it.Replace(it.Path("foreign_fixtures/companion.ts"), "offset =", "offset: number =")
	it.Replace(it.Path("foreign_fixtures/nested/b.ts"), "../companion.mjs", "../companion.js")
	it.Replace(it.Path("foreign_json/tsconfig.json"), "  \"compilerOptions\": { \"allowJs\": true },\n", "")
	it.Replace(it.Path("foreign_fixtures/BUILD.bazel"), "companion.mjs", "companion.ts")
	it.Write(it.Path("foreign_fixtures/tsconfig.json"), `{
  "extends": "../tsconfig.json",
  "include": ["a.ts", "nested/b.ts", "companion.ts"]
}
`)
	ownerBefore := it.Read(it.Path("foreign_fixtures/BUILD.bazel"))
	it.Write(it.Path("foreign_fixtures/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile")
`+ownerBefore+`
ts_compile(
    name = "foreign_fixtures",
    visibility = ["//foreign_json:__pkg__"],  # keep
)
`)
	it.MustBazel("run", "//:gazelle")
	requireRootScopePublisher(it)
	requireLabels(it, "srcs", "//foreign_json:foreign_json", []string{"//foreign_json:index.ts"})
	requireLabels(it, "deps", "//foreign_json:foreign_json", []string{"//:root", "//foreign_fixtures:foreign_fixtures", "//src/i18n:manifest"})
	requireLabels(it, "srcs", "//foreign_fixtures:foreign_fixtures", []string{
		"//foreign_fixtures:a.ts", "//foreign_fixtures:companion.ts", "//foreign_fixtures:nested/b.ts", "//foreign_fixtures:value.json",
	})
	for _, backend := range []string{"tsgo", "oxc"} {
		it.MustBazel("test", "//foreign_consumer:foreign_consumer_test", "--@rules_typescript//ts:declarations="+backend, "--output_groups=+declarations")
		it.RequireFile(it.Bin("foreign_json/index.d.ts"), "emitted consumer has no declaration")
		it.RequireFile(it.Bin("foreign_fixtures/a.js"), "emitted dependency is missing at its own package path")
		it.RequireContains(it.Bin("foreign_fixtures/companion.d.ts"), "offset: number", "emitted companion lost its declared type")
		it.RequireContains(it.Bin("foreign_fixtures/nested/b.d.ts"), "answer: number", "emitted dependency lost its declared type")
		it.RequireContains(it.Bin("foreign_json/index.js"), "../foreign_fixtures/a.js", "emission changed the sibling import path")
	}
	it.Pass("normal Gazelle generation declares and runs mixed source roots through downstream declaration and native consumers; a separate owner retains both emission backends")
}

const loadTsCompile = `load("@rules_typescript//ts:defs.bzl", "ts_compile")
`

const loadTsCodegen = `load("@rules_typescript//ts:defs.bzl", "ts_codegen")
`

// What an every-directory run left below worker/: worker/src holds no
// tsconfig.json, so the package model empties the file and names it.
const staleWorkerSrcPackage = loadTsCompile + `
ts_compile(
    name = "src",
    srcs = ["handler.ts"],
    tsconfig = "//worker:tsconfig",
)
`

func staleBuildFileIsEmptiedAndNamed(it *harness.IT, gazelleLog *harness.Log) {
	stale := it.Path("worker/src/BUILD.bazel")
	it.RequireNotContains(stale, "ts_compile(",
		"the stale ts_compile below //worker survived the run")
	it.Pass("worker/src/BUILD.bazel holds no rule after the run: " +
		"worker/src is no package")
	for _, want := range []string{
		"worker/src is not a package", "ts_compile(src) is withdrawn",
	} {
		if !gazelleLog.Contains(want) {
			gazelleLog.Dump()
			it.Fail("Gazelle did not say %q of the emptied BUILD file", want)
		}
	}
	it.Pass("Gazelle named worker/src/BUILD.bazel as the file to delete")

	// Empty, the file still makes worker/src a package holding //worker's
	// src/handler.ts, so the consumer deletes it before the build.
	if err := os.Remove(stale); err != nil {
		it.Fail("cannot delete the emptied BUILD file: %v", err)
	}
	it.Pass("worker/src/BUILD.bazel deleted, as a consumer deletes what " +
		"Gazelle empties")
}

func requireRootScopePublisher(it *harness.IT) {
	file, err := rule.LoadFile(it.Path("BUILD.bazel"), "")
	if err != nil {
		it.Fail("cannot load the root scope publisher: %v", err)
	}
	for _, target := range file.Rules {
		if target.Name() != "root" {
			continue
		}
		if target.Kind() != "ts_compile" {
			it.Fail("the root scope publisher is %s, want ts_compile", target.Kind())
		}
		if sources := target.Attr("srcs"); sources != nil {
			list, literal := sources.(*bzl.ListExpr)
			if !literal || len(list.List) != 0 {
				it.Fail("the root scope publisher acquired a source program")
			}
		}
		if !slices.Equal(target.AttrStrings("package_scopes"), []string{"package.json"}) ||
			!slices.Equal(target.AttrStrings("visibility"), []string{"//visibility:public"}) {
			it.Fail("the root scope publisher lost its authored package.json or public source visibility")
		}
		return
	}
	it.Fail("the emitted consumers have no root package-scope publisher")
}

func theRootBaseIsATsConfig(it *harness.IT) {
	root := it.Path("BUILD.bazel")
	it.RequireContains(root, `name = "tsconfig"`,
		"no ts_config was written for the root tsconfig.json the packages extend")
	it.Pass("the root retains its ts_config")
	requireLabels(it, "deps", "//src/lib:tsconfig", []string{"//:tsconfig"})
	it.Pass("//src/lib:tsconfig depends on //:tsconfig, the base its " +
		"extends names")
}

// The member's own tests import it by name: a self-reference through the
// manifest as built at the member's path, the compile alone their dep.
func memberSelfImportIsTheCompile(it *harness.IT) {
	build := it.Path("packages/shared/BUILD.bazel")
	it.RequireContains(build, `name = "shared_test"`,
		"Gazelle wrote no ts_test for the member's test files")
	requireLabels(it, "deps", "//packages/shared:shared_test",
		[]string{"//packages/shared:shared", "@npm//:vitest"})
	it.Pass("//packages/shared:shared_test depends on :shared and on no " +
		"link target: the member's own name is a self-reference")

	for _, rel := range []string{"packages/shared/src/entry.test.js", "packages/shared/src/wire.test.js"} {
		it.RequireFile(it.Bin(rel),
			"%s was not written; the `bazel build //...` above did not compile the test program", rel)
	}
	it.Pass("the test program resolved `shared` and `shared/wire` through " +
		"the manifest as built laid at packages/shared/package.json")

	log, err := it.BazelLog("self_import_at_run_time", "test",
		"//packages/shared:shared_test")
	if err != nil {
		log.Dump()
		it.Fail("//packages/shared:shared_test failed: %v", err)
	}
	for _, stale := range []string{
		`Failed to resolve entry for package "shared"`,
		`Cannot find package 'shared/wire'`,
	} {
		if log.Contains(stale) {
			log.Dump()
			it.Fail("the runtime still fails to resolve: %q", stale)
		}
	}
	it.Pass("//packages/shared:shared_test type-checks and runs: node resolves " +
		"`shared` and `shared/wire` through the member's exports map at its " +
		"own path")
}

// member/'s test imports the member by name and by its exports subpath from
// another package: the dep is the root's link target, never the ts_compile.
func memberByNameIsTheHubView(it *harness.IT) {
	requireLabels(it, "deps", "//member:member_test", withRootManifest())
	it.Pass("//member:member_test depends on //:node_modules/shared, the root's " +
		"link target, for `shared` and `shared/wire`")
	it.RequireFile(it.Bin("member/consumer.test.js"),
		"the member's consumer did not compile against the view")
	it.Pass("the test compiled and ran under `bazel test //...` above, " +
		"through the importer's link")
}

// dotdot/inner imports "..", the package above: the listing resolves the
// specifier to dotdot/index.ts and ownership names //dotdot.
func parentDirectoryImportIsADep(it *harness.IT) {
	requireLabels(it, "deps", "//dotdot/inner:inner", []string{"//dotdot:dotdot"})
	it.RequireFile(it.Bin("dotdot/inner/leaf.js"),
		"//dotdot/inner did not compile")
	it.Pass("//dotdot/inner depends on //dotdot through an import of `..`")
}

// dropConfigSrcs removes the config scope a probe's removed attribute leaves orphaned.
func dropConfigSrcs(it *harness.IT, dir string) {
	build := it.Path(dir, "BUILD.bazel")
	file, err := rule.LoadFile(build, dir)
	if err != nil {
		it.Fail("cannot load //%s: %v", dir, err)
	}
	for _, target := range file.Rules {
		if target.Kind() == "ts_test" {
			target.DelAttr("config_srcs")
		}
	}
	it.Write(build, string(file.Format()))
}

// configured/ keeps its vitest.config.mts beside package.json and its test one
// package down; the config answers the test's `virtual:answer` import.
func packageRootVitestConfigReachesTheTestBelow(it *harness.IT) {
	requireLabels(it, "srcs", "//configured:vitest_config",
		[]string{"//configured:vitest.config.mts"})
	it.Pass("//configured:vitest_config holds the package root's vitest config")

	below := it.Path("configured/test/BUILD.bazel")
	const attr = "    config = \"//configured:vitest_config\",\n"
	requireLabels(it, "config", "//configured/test:test_test",
		[]string{"//configured:vitest_config"})
	it.Pass("//configured/test:test_test names the package root's config by label")

	// `bazel test //...` above ran it under the config; without the attr nothing
	// resolves the import the plugin answers.
	restore := it.Read(below)
	it.Replace(below, attr, "")
	dropConfigSrcs(it, "configured/test")
	log, err := it.BazelLog("test_without_the_package_root_config", "test", "//configured/test:test_test")
	it.Write(below, restore)
	if err == nil {
		log.Dump()
		it.Fail("//configured/test:test_test passed without the config; Gazelle need not write it")
	}
	if !log.Contains("virtual:answer") {
		log.Dump()
		it.Fail("without the config the test did not fail on the import the plugin answers")
	}
	it.Pass("without the config //configured/test:test_test fails on `virtual:answer`: the setting reached the test through the label")
}

// worker/test keeps a vitest.config.mts beside its tests; its tsconfig lists
// *.ts alone, so the config is the ts_test's config and no target's src.
func configBesideTheTestsIsNamed(it *harness.IT) {
	requireLabels(it, "config", "//worker/test:test_test",
		[]string{"//worker/test:vitest.config.mts"})
	for _, src := range labels(it, "srcs", "//worker/test:test_test") {
		if strings.HasSuffix(src, "vitest.config.mts") {
			it.Fail("the vitest config is a src of the test that runs under it: %s", src)
		}
	}
	it.Pass("//worker/test:test_test names vitest.config.mts as its config " +
		"and compiles it in no target")
}

// pooled/vitest.config.mts installs the Workers pool and names its wrangler
// config: the worker makes the file a label, the test names it, workerd runs.
func workersPoolConfigReachesTheTest(it *harness.IT) {
	it.RequireNoFile(it.Path("pooled/wrangler.jsonc"), "the generated Wrangler config must have no checkout copy")
	requireLabels(it, "srcs", "//pooled:wrangler_config",
		[]string{"//pooled:wrangler.jsonc"})
	requireLabels(it, "wrangler_config", "//pooled/test:test_test",
		[]string{"//pooled:wrangler_config"})
	it.Pass("//pooled/test:test_test names //pooled:wrangler_config, the " +
		"filegroup over the file the config's configPath names")
	it.Pass("the generated Wrangler config reaches the Workers pool without a checkout copy")

	below := it.Path("pooled/test/BUILD.bazel")
	it.RequireContains(below, `coverage_provider = "istanbul"`,
		"the pooled test got no coverage_provider; the pool refuses v8")
	requireLabels(it, "deps", "//pooled/test:test_test", []string{
		"//pooled:pooled", "@npm//pooled:cloudflare_vitest-pool-workers",
		"@npm//pooled:cloudflare_workers-types", "@npm//pooled:vitest",
		"@npm//pooled:vitest_coverage-istanbul"})
	it.Pass("coverage_provider is istanbul and @vitest/coverage-istanbul is a " +
		"dep in the importer's spelling")

	// `bazel test //...` above ran it in workerd; the measurement behind the
	// attribute: without it the pool boots the source `main`.
	const attr = "    wrangler_config = \"//pooled:wrangler_config\",\n"
	restore := it.Read(below)
	// The generated wrangler.jsonc has no checkout copy, so the probe declares it unprepared.
	it.Replace(below, attr, "    data = [\"//pooled:wrangler_config\"],\n")
	log, err := it.BazelLog("pooled_without_wrangler_config", "test",
		"//pooled/test:test_test")
	it.Write(below, restore)
	if err == nil {
		log.Dump()
		it.Fail("//pooled/test:test_test passed without wrangler_config; " +
			"Gazelle need not write it")
	}
	if !log.Contains("src/index.ts") {
		log.Dump()
		it.Fail("without wrangler_config the test did not fail on the source main")
	}
	it.Pass("without wrangler_config the pool boots src/index.ts, which the " +
		"runfiles do not hold: the patched copy reached the test through the label")
}

// configured/package.json declares vitest and is a lockfile importer, so its
// test's label names that importer's resolution (D7).
func importerScopedLabelsResolve(it *harness.IT) {
	requireLabels(it, "deps", "//configured/test:test_test",
		[]string{"@npm//configured:vitest"})
	it.Pass("//configured/test:test_test depends on @npm//configured:vitest, " +
		"the importer's own resolution, and it ran above")
}

// src/app imports culori, which ships no declarations: the edge's label is the
// package the specifier names, and @types/culori arrives paired on the chain.
func pairedTypesReachTheProgramThroughTheChain(it *harness.IT) {
	requireLabels(it, "deps", "//src/app:app",
		[]string{"//src/lib:lib", "@npm//:culori", "@npm//:vite"})
	it.Pass("//src/app depends on @npm//:culori alone and type-checked " +
		"formatHex through the paired @types/culori")
}

// Authored and generated declarations need the same runtime package for a bare import.
func declarationRetainsNpmRuntime(it *harness.IT) {
	for _, rel := range []string{"src/app/index.ts", "src/app/tsconfig.json", "src/app/BUILD.bazel", "BUILD.bazel"} {
		file := it.Path(rel)
		defer it.Write(file, it.Read(file))
	}
	input := it.Path("culori-declaration.txt")
	generator := it.Path("culori-declaration.mjs")
	outputDir := it.Path("culori-types")
	it.RequireNoFile(generator, "the declaration generator must be staged by this check")
	it.RequireNoFile(outputDir, "the declaration output directory must be staged by this check")
	defer os.Remove(generator)
	defer os.Remove(outputDir)
	it.RequireNoFile(input, "the declaration generator input must be staged by this check")
	defer os.Remove(input)
	const declaration = "export declare function formatHex(color: \"red\"): \"#ff0000\";\n"
	it.Write(input, declaration)
	it.Write(generator, `import { copyFileSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
const [input, output] = process.argv.slice(2);
mkdirSync(dirname(output), { recursive: true });
copyFileSync(input, output);
`)
	it.Write(it.Path("src/app/index.ts"), `import { formatHex } from "culori";

// The installed @types/culori returns string | undefined, not this literal.
export const brand: "#ff0000" = formatHex("red");
if (brand !== "#ff0000") throw new Error("culori returned " + brand);
console.log(brand);
`)
	it.Write(it.Path("src/app/tsconfig.json"), `{
  "extends": "../../tsconfig.json",
  "compilerOptions": {
    "types": [],
    "paths": { "culori": ["../../culori-types/culori"] }
  },
  "include": ["index.ts"]
}
`)
	// A producer outside src/app cannot arrive as an automatic same-package dep.
	rootBuild := it.Path("BUILD.bazel")
	root, err := rule.LoadFile(rootBuild, "")
	if err != nil {
		it.Fail("cannot load the declaration producer's package: %v", err)
	}
	for _, load := range root.Loads {
		if load.Name() == "@rules_typescript//ts:defs.bzl" {
			load.Add("ts_codegen")
			load.Add("ts_binary")
		}
	}
	producerBuild := string(root.Format()) + `
ts_binary(
    name = "culori_declaration_gen",
    entry_point = "culori-declaration.mjs",
)

ts_codegen(
    name = "culori_types",
    srcs = ["culori-declaration.txt"],
    %s,
    args = ["{srcs}", "%s"],
    generator = ":culori_declaration_gen",
    visibility = ["//src/app:__pkg__"],
)
`
	consumerBuild := `load("@rules_typescript//ts:defs.bzl", "ts_binary")

%sts_binary(
    name = "culori_runtime",
    entry_point = ":app",
)
`
	copy := it.Path("culori-types/culori.d.ts")
	defer os.Remove(copy)
	stagedSource := it.Read(it.Path("src/app/index.ts"))
	stagedConfig := it.Read(it.Path("src/app/tsconfig.json"))
	runtimeTest := it.Path("src/app/culori_runtime.test.ts")
	it.RequireNoFile(runtimeTest, "the source-mode fallback test must be staged by this check")
	defer os.Remove(runtimeTest)
	fixtures := it.Path("src/fixtures")
	it.RequireNoFile(fixtures, "the exported fallback package must be staged by this check")
	defer os.Remove(fixtures)
	defer os.Remove(it.Path("src/fixtures/BUILD.bazel"))
	for _, pkg := range []string{"app", "fixtures"} {
		for _, name := range []string{"fallback.ts", "fallback_helper.ts"} {
			file := it.Path("src", pkg, name)
			it.RequireNoFile(file, "the fallback closure must be staged by this check")
			defer os.Remove(file)
		}
	}
	for _, producer := range []struct {
		name, output, argument, override string
		fallback, keep                   string
	}{
		{"authored", "", "", "", "", ""},
		{"scalar", `outs = ["culori-types/culori.d.ts"]`, "{out}", "# gazelle:resolve typescript culori-types/culori.d.ts //:culori_types\n\n", "", ""},
		{"tree", `out_dir = "culori-types"`, "{out}/culori.d.ts", "", "", ""},
		{"unkept fallback", `outs = ["culori-types/culori.d.ts"]`, "{out}", "", "app", ""},
		{"kept fallback", `outs = ["culori-types/culori.d.ts"]`, "{out}", "", "app", " # keep"},
		{"unkept sibling fallback", `outs = ["culori-types/culori.d.ts"]`, "{out}", "", "fixtures", ""},
		{"kept sibling fallback", `outs = ["culori-types/culori.d.ts"]`, "{out}", "", "fixtures", " # keep"},
	} {
		generated := producer.name != "authored"
		sourceRuntime := producer.fallback == "fixtures" && producer.keep != ""
		states := []bool{false, true}
		wantDeps := []string{"//:culori_types", "@npm//:culori"}
		suppliers := []string{"//:culori_types", "//src/app:culori_runtime"}
		if generated {
			it.Write(rootBuild, fmt.Sprintf(producerBuild, producer.output, producer.argument))
		} else {
			it.Write(rootBuild, string(root.Format())+`exports_files(["culori-types/culori.d.ts"], visibility = ["//src/app:__pkg__"])
`)
			it.Write(copy, declaration)
			states = []bool{true}
			wantDeps = []string{"@npm//:culori"}
			suppliers = []string{"//src/app:culori_runtime"}
		}
		consumer := fmt.Sprintf(consumerBuild, producer.override)
		config := stagedConfig
		if producer.fallback != "" {
			states = []bool{false}
			config = strings.Replace(config, `["../../culori-types/culori"]`, `["../../culori-types/culori", "../`+producer.fallback+`/fallback"]`, 1)
			fallbackLabel := "fallback.ts"
			if producer.fallback == "fixtures" {
				fallbackLabel = "//src/fixtures:fallback.ts"
				it.Write(it.Path("src/fixtures/BUILD.bazel"), `exports_files(["fallback.ts", "fallback_helper.ts"], visibility = ["//src/app:__pkg__"])
`)
			}
			consumer = strings.Replace(consumer, `"ts_binary")`, `"ts_binary", "ts_compile")`, 1) + `
ts_compile(
    name = "app",
    srcs = [
        "index.ts",
        "` + fallbackLabel + `",` + producer.keep + `
    ],
)
`
			it.Write(it.Path("src", producer.fallback, "fallback.ts"), `import { red } from "./fallback_helper";
import { add } from "../lib";
export function formatHex(color: "red"): "#ff0000" {
  if (add(1, 1) !== 2) throw new Error(color);
  return red;
}
`)
			it.Write(it.Path("src", producer.fallback, "fallback_helper.ts"), `export const red: "#ff0000" = "#ff0000";
`)
			if producer.keep != "" {
				wantDeps = []string{"//:culori_types", "//src/lib:lib", "@npm//:culori"}
			}
		}
		if sourceRuntime {
			// The unowned sibling closure needs a runner that accepts source inputs.
			consumer = strings.Replace(consumer, `"ts_binary",`, `"ts_test",`, 1)
			consumer = strings.Replace(consumer, `ts_binary(
    name = "culori_runtime",
    entry_point = ":app",
)`, `ts_test(
    name = "culori_runtime",
    srcs = ["culori_runtime.test.ts"],
    tsconfig = ":tsconfig",
    node_modules = "//:node_modules",
    deps = [":app", "@npm//:vitest"],
)`, 1)
			it.Write(runtimeTest, `import { expect, it } from "vitest";
import { formatHex } from "../fixtures/fallback";

it("runs the retained sibling and its helper and library imports", () => {
  expect(formatHex("red")).toBe("#ff0000");
});
`)
			suppliers = []string{"//:culori_types"}
		}
		it.Write(it.Path("src/app/tsconfig.json"), config)
		it.Write(it.Path("src/app/BUILD.bazel"), consumer)
		if producer.argument == "{out}" {
			// Scalar selection needs a file probe; a tree can own an absent directory.
			if err := os.Mkdir(outputDir, 0o755); err != nil {
				it.Fail("cannot create the scalar declaration directory: %v", err)
			}
		} else if generated {
			it.RequireNoFile(outputDir, "the generated tree must be absent for the first state")
		}
		if generated {
			it.RequireNoFile(copy, "the generated declaration must be absent for the first state")
		}
		var absentBuild, absentLink string
		for _, present := range states {
			if generated && present {
				it.Write(copy, it.Read(it.Bin("culori-types/culori.d.ts")))
			}
			it.MustBazel("run", "//:gazelle", "--", "src/app")
			requireLabels(it, "deps", "//src/app:app", wantDeps)
			if !generated {
				requireLabels(it, "srcs", "//src/app:app", []string{"//:culori-types/culori.d.ts", "//src/app:index.ts"})
			} else if producer.fallback != "" {
				wantSrcs := []string{"//src/app:index.ts"}
				if producer.keep != "" {
					wantSrcs = []string{"//src/" + producer.fallback + ":fallback.ts", "//src/" + producer.fallback + ":fallback_helper.ts", "//src/app:index.ts"}
				}
				sort.Strings(wantSrcs)
				requireLabels(it, "srcs", "//src/app:app", wantSrcs)
			}
			if sourceRuntime {
				it.MustBazel("test", "//src/app:culori_runtime", "--output_groups=+_validation")
			} else if out := strings.TrimSpace(it.BazelStdout("run", "//src/app:culori_runtime")); out != "#ff0000" {
				it.Fail("%s present=%t: the emitted culori value import returned %q, want #ff0000", producer.name, present, out)
			}
			for _, target := range suppliers {
				if got := labels(it, "node_modules", target); len(got) != 0 {
					it.Fail("%s supplies npm packages outside the compile's deps: %v", target, got)
				}
			}
			if generated {
				if got := it.Read(it.Bin("culori-types/culori.d.ts")); got != declaration {
					it.Fail("the declaration producer wrote %q, want %q", got, declaration)
				}
			}
			launcher := "culori_runtime_launcher"
			if sourceRuntime {
				launcher = "culori_runtime_test_launcher"
			}
			link := it.Bin("src/app/" + launcher + ".runfiles/_main/node_modules/culori")
			destination, err := os.Readlink(link)
			if err != nil {
				it.Fail("the consumer's runtime closure has no culori package link: %v", err)
			}
			it.RequireMatches(filepath.Join(link, "package.json"), `"name"\s*:\s*"culori"`, "the runtime link must reach culori itself, not its types package")
			if !sourceRuntime {
				it.RequireContains(it.Bin("src/app/index.js"), "culori", "emission lost the bare npm import")
			}
			build := it.Read(it.Path("src/app/BUILD.bazel"))
			if present {
				if generated && (build != absentBuild || destination != absentLink) {
					it.Fail("%s: materializing the declaration changed the compiler BUILD or runtime symlink", producer.name)
				}
				if it.Read(copy) != declaration {
					it.Fail("Gazelle or the build changed the staged declaration copy")
				}
			} else {
				absentBuild, absentLink = build, destination
				it.RequireNoFile(copy, "the build wrote the declaration into the checkout")
			}
			if it.Read(it.Path("src/app/index.ts")) != stagedSource || it.Read(it.Path("src/app/tsconfig.json")) != config {
				it.Fail("Gazelle or the build changed the staged source or tsconfig")
			}
		}
		if producer.fallback == "" {
			if err := os.Remove(copy); err != nil {
				it.Fail("cannot restore the absent declaration state: %v", err)
			}
		}
		it.RequireNoFile(copy, "the staged declaration copy was not removed")
		if err := os.Remove(outputDir); err != nil {
			it.Fail("cannot restore the absent declaration directory: %v", err)
		}
	}
	it.Pass("declarations retain the culori runtime; kept fallbacks retain their source and dependency closure while unkept fallbacks are pruned")
}

// probe.ts augments vite's UserConfig and never imports vite: the edge is the
// compiler's `Augmented via` reason, resolved through the importer's link.
func augmentationIsADep(it *harness.IT) {
	build := it.Path("augmented/BUILD.bazel")
	requireLabels(it, "deps", "//augmented:augmented",
		[]string{"@npm//:types_node", "@npm//:vite", "@npm//:vitest"})
	it.Pass("//augmented depends on @npm//:vite from the augmentation alone, " +
		"and on @npm//:types_node from vite's own `/// <reference types>`")

	restore := it.Read(build)
	it.Replace(build, "        \"@npm//:vite\",\n", "")
	log, err := it.BazelLog("augmentation_without_the_dep", "build", "//augmented")
	it.Write(build, restore)
	if err == nil {
		log.Dump()
		it.Fail("//augmented compiled without @npm//:vite; the augmentation " +
			"resolves without the link and Gazelle need not write the dep")
	}
	if !log.Contains("TS2664") {
		log.Dump()
		it.Fail("//augmented failed for some other reason than the " +
			"augmentation's module not being found")
	}
	it.Pass("without the dep the augmentation is TS2664: vite resolves " +
		"through the importer's link, which the dep alone stages")
}

// packages/shared/example's package.json is no lockfile importer: nothing is
// written under it, and the member above holds none of its files.
func foreignProjectGetsNothing(it *harness.IT, gazelleLog *harness.Log) {
	it.RequireNoFile(it.Path("packages/shared/example/BUILD.bazel"),
		"Gazelle wrote a BUILD file under packages/shared/example, whose "+
			"package.json is no importer in pnpm-lock.yaml")
	it.Pass("packages/shared/example, a foreign project, has no BUILD file")

	for _, want := range []string{
		"typescript: packages/shared/example/package.json is no importer in " +
			"pnpm-lock.yaml, so pnpm installs nothing for it; nothing is " +
			"written under packages/shared/example",
		"packages/shared/example/tsconfig.json: not listed: " +
			"packages/shared/example/package.json is no importer in " +
			"pnpm-lock.yaml",
		// packages/shared/tsconfig.json has no include, so its program
		// reaches the file; nothing owns it.
		"typescript: packages/shared/example/index.ts: under " +
			"packages/shared/example, a project pnpm installs nothing for: " +
			"packages/shared/example/package.json is no importer in " +
			"pnpm-lock.yaml; listed by packages/shared/tsconfig.json",
	} {
		if !gazelleLog.Contains(want) {
			gazelleLog.Dump()
			it.Fail("Gazelle did not say %q", want)
		}
	}
	it.Pass("Gazelle named the manifest, the tsconfig.json it did not list " +
		"and the file the member's program reaches under it")

	requireLabels(it, "srcs", "//packages/shared:shared", []string{
		"//packages/shared:src/index.ts", "//packages/shared:src/wire/index.ts"})
	requireLabels(it, "package_scopes", "//packages/shared:shared", []string{"//packages/shared:package.json"})
	it.Pass("//packages/shared holds its own files and nothing under example/")
}

func generatedDevServerBuilds(it *harness.IT) {
	build := it.Path("devserver/BUILD.bazel")
	it.RequireContains(build, `ts_dev_server(`, "Gazelle wrote no dev server")
	it.RequireContains(build, `entry_point = ":devserver"`, "the dev server has the wrong entry point")
	it.RequireContains(build, `node_modules = "//:node_modules"`, "the dev server has no importer tree")
	if strings.Contains(it.Read(build), "server =") {
		it.Fail("Gazelle overrides the default dev server in %s", build)
	}
	it.RequireFile(it.Bin("devserver/dev_launcher"), "the generated dev server did not build")
	it.Pass("Gazelle's default dev server builds and keeps the importer's npm tree")
}

// The node_tooling shape: a kept hand-written rule globs tools/, while the
// program lists tools/helper.ts by import, so the glob leaves that file out.
const sharedProgramPackage = loadTsCompile + `
# keep
ts_compile(
    name = "tooling",
    emit = True,
    srcs = glob(
        ["tools/**/*.ts"],
        exclude = ["tools/helper.ts"],
    ),
    tsconfig = ":tsconfig",
    deps = [":shared_program"],
)
`

func handWrittenRuleSharesTheProgram(it *harness.IT) {
	build := it.Path("shared_program/BUILD.bazel")
	it.RequireContains(build, `name = "tooling"`,
		"the kept rule did not survive the Gazelle runs")
	it.RequireContains(build, `name = "shared_program"`,
		"Gazelle wrote no ts_compile for the program beside the kept rule")
	it.Pass("shared_program/BUILD.bazel holds the kept rule and the generated one")

	requireLabels(it, "srcs", "//shared_program:shared_program",
		[]string{"//shared_program:src/main.ts", "//shared_program:tools/helper.ts"})
	it.Pass("//shared_program compiles tools/helper.ts, which its program " +
		"lists by import")
	requireLabels(it, "srcs", "//shared_program:tooling",
		[]string{"//shared_program:tools/cli.ts"})
	it.Pass("//shared_program:tooling globs the rest of tools/ and excludes " +
		"the program's file")
	for _, rel := range []string{
		"shared_program/tools/helper.js", "shared_program/tools/cli.js",
	} {
		it.RequireFile(it.Bin(rel), "expected output file not found: %s", rel)
	}
	it.Pass("both targets built: cli.ts resolves ./helper through the dep " +
		"on the program's target")
}

// The hand-written target the migration below appends to worker/BUILD.bazel,
// without its load line: the Gazelle run adds the symbol to the file's load.
const workerTypesCodegen = `
ts_codegen(
    name = "worker_types",
    srcs = ["bindings.txt"],
    outs = ["worker-configuration.d.ts"],
    args = [
        "--bindings",
        "{srcs}",
        "--out",
        "{out}",
    ],
    generator = "//:env_types_gen",
    visibility = ["//worker:__subpackages__"],
)
`

// The copy on disk stays, in a shape no program here compiles against: the
// declared out is the codegen's, and a green build says which copy was staged.
func declarationMovesToACodegen(it *harness.IT) {
	it.Write(it.Path("worker/worker-configuration.d.ts"),
		"declare const WORKER_BUILD_ID: string;\n\n"+
			"interface WorkerEnv {\n\treadonly stale: string;\n}\n")
	it.Write(it.Path("worker/bindings.txt"), "bucket\n")
	owner := it.Path("worker/BUILD.bazel")
	it.Write(owner, it.Read(owner)+workerTypesCodegen)
	file, err := rule.LoadFile(owner, "worker")
	if err != nil {
		it.Fail("cannot load the declaration-sharing test: %v", err)
	}
	for _, r := range file.Rules {
		if r.Kind() == "ts_test" && r.Name() == "worker_test" {
			r.AddComment("# keep")
			r.SetAttr("srcs", []string{":worker_types", "src/handler.test.ts"})
		}
	}
	it.Write(owner, string(file.Format()))
	it.MustBazel("run", "//:gazelle")
	it.Pass("gazelle run over the migration: ts_codegen appended, a stale " +
		"copy left on disk")

	it.RequireContains(owner, `name = "worker_types"`,
		"the hand-written ts_codegen did not survive the Gazelle run")
	requireLabels(it, "srcs", "//worker:worker",
		[]string{"//worker:bindings.txt", "//worker:src/handler.ts"})
	requireLabels(it, "srcs", "//worker:worker_test",
		[]string{"//worker:src/handler.test.ts", "//worker:worker_types"})
	requireLabels(it, "deps", "//worker:worker", []string{"//:root", "//worker:worker_types"})
	it.Pass("the library depends on the declaration generator while the kept test shares its output as a source")

	for _, dir := range []string{"worker/test", "worker/test/deep"} {
		requireLabels(it, "deps", testTarget(dir),
			withRootManifest("//:root", "//worker:worker", "//worker:worker_types"))
		it.Pass("//%s depends on the ts_codegen for the entry and on //worker "+
			"for the import", dir)
	}

	dirs := []string{"worker", "worker/test", "worker/test/deep"}
	written := map[string]string{}
	for _, dir := range dirs {
		written[dir] = it.Read(it.Path(dir, "BUILD.bazel"))
	}
	it.MustBazel("run", "//:gazelle")
	for _, dir := range dirs {
		again := it.Read(it.Path(dir, "BUILD.bazel"))
		if again != written[dir] {
			fmt.Fprintf(os.Stderr, "--- %s/BUILD.bazel run 1 ---\n%s"+
				"--- run 2 ---\n%s", dir, written[dir], again)
			it.Fail("a second Gazelle run rewrote %s/BUILD.bazel", dir)
		}
	}
	it.Pass("a second Gazelle run writes nothing under worker/")

	it.MustBazel("test", "//worker/...")
	it.Pass("bazel test //worker/...: every program under worker/ resolves " +
		"the generated declaration")
	declaration := it.Bin("worker/worker-configuration.d.ts")
	it.RequireContains(declaration, "readonly bucket: string;",
		"the generated declaration names no binding from bindings.txt")
	it.Pass("the programs compiled against the generated declaration: each " +
		"reads env.bucket, which the copy on disk does not declare")
}

// Every tsconfig.json here is below the root, so tsgo lists each; that the tsgo
// is the toolchain's, out of the binary's runfiles, only a consumer's run can say.
func programsAreListedWithTheToolchainsTsgo(it *harness.IT, gazelleLog *harness.Log) {
	binary := regexp.MustCompile(`listing programs with \S+/ts/toolchain/tsgo_resolved/(?:tsc|tsgo) \(runfiles\)`).FindString(gazelleLog.Text)
	if binary == "" {
		gazelleLog.Dump()
		it.Fail("Gazelle did not find tsgo through the runfiles")
	}
	it.Pass("Gazelle lists programs with the toolchain's tsgo from its runfiles: %s", binary)

	for _, dir := range []string{
		"src/app", "src/locales", "aliased", "worker", "worker/test",
		"worker/test/deep", "generated_worker", "packages/shared", "member",
		"dotdot/inner",
	} {
		if !gazelleLog.Matches(dir + `/tsconfig.json: \d+ files listed, [1-9]\d* roots, \d+ edges, \d+ type entries`) {
			gazelleLog.Dump()
			it.Fail("Gazelle did not list %s/tsconfig.json", dir)
		}
		it.Pass("%s/tsconfig.json listed", dir)
	}
	if !gazelleLog.Contains("tsconfig.json: not listed: neither include nor " +
		"files in its extends chain") {
		gazelleLog.Dump()
		it.Fail("Gazelle listed the root tsconfig.json, which would enumerate " +
			"the whole workspace")
	}
	it.Pass("the root tsconfig.json is refused before tsgo runs")
	summary := regexp.MustCompile(`\d+ \.ts/\.tsx/\.mts/\.cts files in no program across \d+ director(y|ies)`).FindString(gazelleLog.Text)
	if summary == "" {
		gazelleLog.Dump()
		it.Fail("Gazelle did not report the files in no program once the walk was done")
	}
	it.Pass("Gazelle reports the files in no program: %s", summary)
}

// A ts_codegen is hand-written; its srcs reach into messages/, which no
// BUILD file makes a package.
const cataloguePackage = loadTsCodegen + `
ts_codegen(
    name = "locales",
    srcs = ["settings.json"] + glob(["messages/*.json"]),
    outs = ["locales.ts"],
    args = [
        "{srcs}",
        "{out}",
    ],
    generator = "//:catalogue_gen",
)

ts_codegen(
    name = "manifest",
    srcs = ["settings.json"],
    outs = ["generated-manifest.json"],
    args = ["{srcs}", "{out}"],
    generator = "//:json_gen",
    visibility = ["//foreign_json:__pkg__"],
)
`

// src/i18n's program lists settings.json by import; the tree's other JSON is
// the ts_compile's by the walk, and the codegen beside it is its dep.
func theTreesFilesAreSrcs(it *harness.IT) {
	it.RequireNoFile(it.Path("src/i18n/messages/BUILD.bazel"),
		"Gazelle made src/i18n/messages a package; no tsconfig.json is there")
	it.Pass("src/i18n/messages has no BUILD file of its own")

	requireLabels(it, "srcs", "//src/i18n:i18n", []string{
		"//src/i18n:index.ts", "//src/i18n:messages/en.json",
		"//src/i18n:messages/sv.json", "//src/i18n:settings.json",
	})
	it.Pass("//src/i18n holds the imported settings.json and the messages " +
		"the walk found")
	requireLabels(it, "deps", "//src/i18n:i18n", []string{"//src/i18n:locales", "//src/i18n:manifest"})
	it.RequireFile(it.Bin("src/i18n/index.js"),
		"//src/i18n did not compile; the JSON import did not resolve in the sandbox")
	it.Pass("//src/i18n depends on :locales and type-checks the import of " +
		"settings.json")

	generated := it.Bin("src/i18n/locales.ts")
	it.RequireFile(generated, "the codegen action did not run")
	for _, name := range []string{"settings.json", "en.json", "sv.json"} {
		it.RequireContains(generated, name, "the codegen action never saw %s", name)
		it.Pass("the codegen action saw %s", name)
	}
}

// A hand-written out_dir ts_codegen, with a tree checked in under compiled/
// standing in for what a local run leaves behind.
const outDirCodegenPackage = loadTsCodegen + `
ts_codegen(
    name = "tree",
    srcs = ["names.txt"],
    args = [
        "--names",
        "{srcs}",
        "--outdir",
        "{out}",
    ],
    generator = "//:tree_gen",
    out_dir = "compiled",
)
`

// The workers lockfile's package holds its store and nothing else.
const wranglerLock = `load("@npm_workers//:defs.bzl", "npm_virtual_store")

npm_virtual_store(name = "node_modules/.pnpm")
`

// A worker with nothing checked in: the ts_codegen beside the config writes the
// declaration its tsconfig names; its node_modules links another lockfile.
const generatedWorkerPackage = `load(
    "@rules_typescript//npm:defs.bzl",
    "node_modules",
)
load("@rules_typescript//ts:defs.bzl", "ts_codegen")

# keep
node_modules(
    name = "node_modules",
    hoist = "//wrangler_lock:node_modules/.pnpm/node_modules",
    deps = ["@npm_workers//:wrangler"],
)

ts_codegen(
    name = "worker_types",
    srcs = ["wrangler.jsonc"],
    outs = ["worker-configuration.d.ts"],
    args = [
        "--config",
        "wrangler.jsonc",
        "--out",
        "{out}",
        "--srcs",
        "{srcs}",
        "--strict-vars=false",
    ],
    generator = "@rules_typescript//tools/codegen:wrangler_types",
    node_modules = ":node_modules",
    visibility = ["//generated_worker:__subpackages__"],
)
`

// The converge test says Gazelle names the ts_codegen for a declaration not on
// disk; only a build says the program then type-checks against what it wrote.
func generatedDeclarationNamesItsGenerator(it *harness.IT) {
	build := it.Path("generated_worker/BUILD.bazel")
	it.RequireContains(build, `name = "worker_types"`,
		"the hand-written ts_codegen did not survive the Gazelle run")
	requireLabels(it, "deps", "//generated_worker:generated_worker",
		[]string{"//generated_worker:worker_types"})
	for _, attr := range []string{"types =", "types_srcs"} {
		it.RequireNotContains(build, attr,
			"//generated_worker restates the tsconfig's types entry as %q", attr)
	}
	it.Pass("//generated_worker depends on the ts_codegen and restates nothing")

	declaration := it.Bin("generated_worker/worker-configuration.d.ts")
	it.RequireFile(declaration, "the wrangler types action did not run")
	it.RequireContains(declaration, "CACHE: KVNamespace;",
		"the generated declaration names no binding from wrangler.jsonc")
	it.Pass("wrangler types wrote the declaration from wrangler.jsonc, with no checked-in copy")
	it.RequireFile(it.Bin("generated_worker/src/index.js"),
		"//generated_worker did not compile against the generated declaration")
	it.Pass("//generated_worker type-checks against the Env and runtime " +
		"globals the generated file declares")

	// A binding the config does not declare; only the generated Env can refuse it.
	consumer := it.Path("generated_worker/src/index.ts")
	restore := it.Read(consumer)
	it.Write(consumer, strings.Replace(restore, "env.GREETING", "env.MISSING", 1))
	log, err := it.BazelLog("generated_env_is_in_force", "build",
		"//generated_worker")
	it.Write(consumer, restore)
	if err == nil {
		log.Dump()
		it.Fail("//generated_worker compiled a binding wrangler.jsonc does " +
			"not declare; the generated Env is not the type in force")
	}
	if !log.Contains("TS2339") {
		log.Dump()
		it.Fail("//generated_worker failed for some other reason than the " +
			"undeclared binding")
	}
	it.Pass("env.MISSING is TS2339, so the generated Env types the worker")
}

// A file under the out_dir read into srcs is one output declared twice, the tree
// and a file inside it; the `bazel build //...` above is where Bazel says so.
func outDirContentsAreNotSources(it *harness.IT) {
	requireLabels(it, "srcs", "//src/locales:locales",
		[]string{"//src/locales:app.ts", "//src/locales:names.txt"})
	it.Pass("//src/locales holds the tree's files and nothing under compiled/")

	requireLabels(it, "deps", "//src/locales:locales",
		[]string{"//src/locales:tree"})
	it.Pass("the import of @roundtrip/locales resolves through the tsconfig's paths to :tree")

	for _, dir := range []string{"src/locales/compiled", "src/locales/compiled/messages"} {
		it.RequireNoFile(it.Path(dir, "BUILD.bazel"), "Gazelle made %s a package inside :tree's out_dir", dir)
	}
	it.Pass("no package under src/locales/compiled")

	it.RequireFile(it.Bin("src/locales/app.js"), "//src/locales did not compile against the tree")
	it.Pass("//src/locales compiles against the generated tree")
}

// src/lib/tsconfig.json excludes lib.config.ts; src/app's namesake is in its
// own program, so the exclusion is the tsconfig's and reaches one file.
func theTsconfigsExcludeIsTheExclusion(it *harness.IT) {
	requireLabels(it, "srcs", "//src/lib:lib", []string{
		"//src/lib:format.mjs", "//src/lib:index.ts", "//src/lib:math.ts"})
	it.Pass("//src/lib holds format.mjs under allowJs and no lib.config.ts")

	requireLabels(it, "srcs", "//src/app:app",
		[]string{"//src/app:index.ts", "//src/app:lib.config.ts"})
	it.Pass("//src/app still holds its own lib.config.ts")

	it.RequireFile(it.Bin("src/app/lib.config.js"),
		"src/app/lib.config.ts is in the srcs but nothing compiled it")
	it.Pass("src/app/lib.config.ts compiles; src/lib's namesake is out of the build")
}

// logo.svg is a src by the walk and assets.d.ts's module declaration types the
// import; only a compile that has to fail says the declaration is in force.
func assetImportTypesThroughItsDeclaration(it *harness.IT) {
	requireLabels(it, "srcs", "//src/icons:icons",
		[]string{"//src/icons:assets.d.ts", "//src/icons:index.ts",
			"//src/icons:logo.svg"})
	it.Pass("//src/icons holds logo.svg beside the declaration that types it")

	// //src/icons compiled above, reading logo.viewBox: TS2339 on a string.
	consumer := it.Path("src/icons/index.ts")
	restore := it.Read(consumer)
	it.Write(consumer, "import logo from \"./logo.svg\";\n\nexport const url: string = logo;\n")
	log, err := it.BazelLog("asset_declaration_is_not_a_string",
		"build", "//src/icons")
	it.Write(consumer, restore)
	if err == nil {
		log.Dump()
		it.Fail("//src/icons compiled with the .svg import assigned to a string; the declared type is not being applied")
	}
	if !log.Contains("TS2322") {
		log.Dump()
		it.Fail("//src/icons failed to build for some other reason than the assignment")
	}
	it.Pass("assigning the .svg import to a string is TS2322, so the declared type is the one in force")
}

// src/lib/tsconfig.json sets allowJs, so tsgo lists format.mjs and the rule
// emits its .d.mts; a consumer's failing compile says which type is in force.
func allowJsCompilesAndDeclares(it *harness.IT) {
	for _, rel := range []string{"src/lib/format.mjs", "src/lib/format.d.mts"} {
		it.RequireFile(it.Bin(rel), "expected output file not found: %s", rel)
		it.Pass("output file exists: %s", rel)
	}

	// //src/app compiled above against ../lib, which re-exports format; without
	// the .d.mts the import widens to `any` and any use of it compiles.
	consumer := it.Path("src/app/index.ts")
	restore := it.Read(consumer)
	it.Write(consumer, "import { format } from \"../lib\";\n\nexport const n: number = format(1);\n")
	log, err := it.BazelLog("js_srcs_declaration_is_in_force", "build", "//src/app")
	it.Write(consumer, restore)
	if err == nil {
		log.Dump()
		it.Fail("//src/app compiled with format()'s string result assigned to a number; the .d.mts is not being applied")
	}
	if !log.Contains("TS2322") {
		log.Dump()
		it.Fail("//src/app failed to build for some other reason than the assignment")
	}
	it.Pass("assigning format()'s result to a number is TS2322, so the JSDoc type crossed the package boundary")
}

// The monorepo shape: an untyped compile.mjs, the compile.d.mts tsc reads in
// its place, and a test importing "./compile.mjs"; a failing compile tells.
func checkedInDeclarationTypesTheJavaScript(it *harness.IT) {
	requireLabels(it, "srcs", "//src/typed:typed",
		[]string{"//src/typed:compile.d.mts", "//src/typed:compile.mjs"})
	it.Pass("//src/typed holds the declaration and the JavaScript it stands " +
		"for, which tsgo does not list")
	requireLabels(it, "srcs", "//src/typed:typed_test",
		[]string{"//src/typed:compile.d.mts", "//src/typed:compile.test.ts"})
	requireLabels(it, "deps", "//src/typed:typed_test",
		withRootManifest("//src/typed:typed"))
	it.Pass("//src/typed:typed_test carries the declaration and depends on " +
		"//src/typed:typed for the module")

	// compile.d.mts takes a number; the JavaScript takes anything.
	test := it.Path("src/typed/compile.test.ts")
	restore := it.Read(test)
	it.Write(test, "import { compile } from \"./compile.mjs\";\n\nexport const s: string = compile(\"x\");\n")
	log, err := it.BazelLog("checked_in_declaration_is_in_force", "build",
		"//src/typed:typed_test")
	it.Write(test, restore)
	if err == nil {
		log.Dump()
		it.Fail("//src/typed:typed_test compiled compile(\"x\"); the " +
			"checked-in .d.mts is not the type in force")
	}
	if !log.Contains("TS2345") {
		log.Dump()
		it.Fail("//src/typed:typed_test failed for some other reason " +
			"than the argument")
	}
	it.Pass("compile(\"x\") is TS2345, so compile.d.mts types the import, not tsgo's inference from the .mjs")

	const base = "declaration_owners"
	it.RequireNoDir(it.Path(base), "the declaration ownership regression must be staged by this check")
	defer os.RemoveAll(it.Path(base))
	for _, pkg := range []string{"a", "b", "z"} {
		it.Write(it.Path(base, pkg, "tsconfig.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["*.ts"]}`)
	}
	it.Write(it.Path(base, "a/BUILD.bazel"), `exports_files(["shape.d.ts"], visibility = ["//visibility:public"])`)
	it.Write(it.Path(base, "a/index.ts"), "export { value } from '../z/index.js';\n")
	it.Write(it.Path(base, "a/shape.d.ts"), "export interface Shape { answer: number }\n")
	it.Write(it.Path(base, "b/index.ts"), "import type { Shape } from '../a/shape.js'; export const value: Shape = { answer: 42 };\n")
	it.Write(it.Path(base, "z/index.ts"), "export const value = 42;\n")
	it.Write(it.Path(base, "z/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile")
# keep
ts_compile(name = "z", srcs = ["index.ts", "//declaration_owners/a:shape.d.ts"], tsconfig = "tsconfig.json", visibility = ["//visibility:public"])
`)
	it.MustBazel("run", "//:gazelle", "--", base)
	requireLabels(it, "srcs", "//"+base+"/a:a", []string{"//" + base + "/a:index.ts", "//" + base + "/a:shape.d.ts"})
	requireLabels(it, "deps", "//"+base+"/b:b", []string{"//" + base + "/a:a"})
	it.MustBazel("build", "//"+base+"/...", "--output_groups=+_validation")
	written := map[string]string{}
	for _, pkg := range []string{"a", "b", "z"} {
		written[pkg] = it.Read(it.Path(base, pkg, "BUILD.bazel"))
	}
	it.MustBazel("run", "//:gazelle", "--", "-index=false", "-r=false", base+"/a", base+"/b")
	for pkg, before := range written {
		if it.Read(it.Path(base, pkg, "BUILD.bazel")) != before {
			it.Fail("unindexed update changed declaration ownership in %s", pkg)
		}
	}
	it.Pass("original declaration owners survive a kept supplier, pass strict-deps, and converge in an unindexed update")
}

// aliased/tsconfig.json maps #shared/* to ./shared/*; aliased/src extends it
// and is a package of its own, so its alias imports are deps on //aliased.
func aliasedImportsAreDeps(it *harness.IT) {
	for _, dir := range []string{"aliased", "aliased/src"} {
		it.RequireNotContains(it.Path(dir, "BUILD.bazel"), "path_alias",
			"//%s restates the tsconfig's alias through an attribute the rule does not have", dir)
	}
	it.Pass("no rule under //aliased carries an alias attribute")

	requireLabels(it, "srcs", "//aliased:aliased",
		[]string{"//aliased:shared/util.ts"})
	requireLabels(it, "srcs", "//aliased:aliased_test",
		[]string{"//aliased:shared/util.test.ts"})
	it.Pass("//aliased owns shared/, the files its own program lists and " +
		"aliased/src's does not")

	below := it.Path("aliased/src/BUILD.bazel")
	requireLabels(it, "deps", "//aliased/src:src", []string{"//aliased:aliased"})
	requireLabels(it, "deps", "//aliased/src:src_test",
		withRootManifest("//aliased/src:src", "//aliased:aliased"))
	it.Pass("both rules in //aliased/src depend on //aliased through the alias")

	tests := strings.Fields(it.BazelStdout("query", "tests(//aliased/...)"))
	if len(tests) != 2 {
		it.Fail("expected the two generated ts_tests under //aliased, got %v", tests)
	}
	it.Pass("%s ran under `bazel test //...` above, so both test programs resolved #shared/util", strings.Join(tests, " and "))

	// The measurement behind writing the dep at all: the alias maps to a file
	// only the dep edge stages.
	restore := it.Read(below)
	it.Replace(below, "    deps = [\"//aliased\"],\n", "")
	log, err := it.BazelLog("alias_without_the_dep", "build", "//aliased/src:src")
	it.Write(below, restore)
	if err == nil {
		log.Dump()
		it.Fail("//aliased/src:src compiled without the dep; the alias alone reaches the file and Gazelle need not write one")
	}
	if !log.Contains("TS2307") || !log.Contains("'#shared/util'") {
		log.Dump()
		it.Fail("//aliased/src:src failed for some other reason than the unstaged alias target")
	}
	it.Pass("without the dep `#shared/util` is TS2307: the alias maps to a file nothing stages")
}

// tsc finds env.d.ts's `/// <reference types="node" />` in node_modules/@types; the
// sandbox has none, so index.ts's `process` is TS2591 until a dep carries the declarations.
func referenceTypesBecomeADep(it *harness.IT) {
	build := it.Path("src/env/BUILD.bazel")
	requireLabels(it, "deps", "//src/env:env", []string{"@npm//:types_node"})
	it.RequireNotContains(build, "types =",
		"the directive was written as a types attribute rather than a dep")
	it.Pass("//src/env carries @npm//:types_node from the directive and no types attribute")

	it.RequireFile(it.Bin("src/env/index.js"),
		"//src/env did not compile, so @types/node's declarations never reached its program")
	it.Pass("//src/env type-checks a use of `process` against declarations nothing imports")

	// The measurement behind writing the dep at all.
	restore := it.Read(build)
	it.Replace(build, "    deps = [\"@npm//:types_node\"],\n", "")
	log, err := it.BazelLog("reference_types_without_the_dep", "build", "//src/env")
	it.Write(build, restore)
	if err == nil {
		log.Dump()
		it.Fail("//src/env compiled without the dep; the directive alone reaches the declarations and Gazelle need not write one")
	}
	if !log.Contains("TS2591") {
		log.Dump()
		it.Fail("//src/env failed for some other reason than the missing global")
	}
	it.Pass("without the dep `process` is TS2591: the directive resolves to nothing in the sandbox")
}

// jsx/tsconfig.json names @acme/jsx as the JSX runtime and `paths` sends it to
// runtime/, a package of its own; Icon.tsx imports nothing, so that is the dep.
func jsxRuntimeBecomesADep(it *harness.IT) {
	build := it.Path("jsx/BUILD.bazel")
	requireLabels(it, "deps", "//jsx:jsx", []string{"//jsx/runtime:runtime"})
	it.Pass("//jsx carries //jsx/runtime from jsxImportSource alone: nothing " +
		"in it imports")

	it.RequireFile(it.Bin("jsx/view/Icon.js"),
		"//jsx did not compile, so @acme/jsx/jsx-runtime never reached its program")
	it.Pass("//jsx type-checks a JSX tag against a runtime nothing imports")

	// The measurement behind writing the dep at all.
	restore := it.Read(build)
	it.Replace(build, "    deps = [\"//jsx/runtime\"],\n", "")
	log, err := it.BazelLog("jsx_runtime_without_the_dep", "build", "//jsx")
	it.Write(build, restore)
	if err == nil {
		log.Dump()
		it.Fail("//jsx compiled without the dep; the tsconfig alone reaches " +
			"the runtime and Gazelle need not write one")
	}
	if !log.Contains("TS2875") || !log.Contains("'@acme/jsx/jsx-runtime'") {
		log.Dump()
		it.Fail("//jsx failed for some other reason than the missing runtime")
	}
	it.Pass("without the dep the tag is TS2875 on '@acme/jsx/jsx-runtime': the implicit import resolves to nothing in the sandbox")
}

// worker/tsconfig.json states `types: ["./worker-configuration.d.ts"]` and
// handler.ts uses its declarations: the package holds both, restating nothing.
func declarationEntryResolvesThroughItsOwner(it *harness.IT) {
	owner := it.Path("worker/BUILD.bazel")
	requireLabels(it, "srcs", "//worker:worker",
		[]string{"//worker:src/handler.ts", "//worker:worker-configuration.d.ts"})
	requireLabels(it, "srcs", "//worker:worker_test",
		[]string{"//worker:src/handler.test.ts",
			"//worker:worker-configuration.d.ts"})
	for _, attr := range []string{"types =", "types_srcs", "tsconfig_types"} {
		it.RequireNotContains(owner, attr,
			"//worker restates the tsconfig's entry as %q", attr)
	}
	it.Pass("both rules in //worker hold the declaration the tsconfig names " +
		"and restate nothing")

	it.RequireFile(it.Bin("worker/src/handler.js"),
		"//worker did not compile, so the declarations never reached its program")
	it.Pass("//worker type-checks against declarations nothing there imports")

	theDeclarationStopsAtTheSubtree(it)
	parentEntryResolvesThroughTheOwner(it)
	packageEntryIsADep(it)
}

// A dep edge stages a file and puts nothing in scope: a consumer outside the
// tsconfig's subtree that deps a target inside it gets no declaration.
func theDeclarationStopsAtTheSubtree(it *harness.IT) {
	it.Write(it.Path("outside/ok.ts"), "export const ran = 1;\n")
	it.Write(it.Path("outside/leaked.ts"), "export const seen = WORKER_BUILD_ID;\n")
	it.Write(it.Path("outside/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "ok",
    emit = True,  # keep
    srcs = ["ok.ts"],
    deps = ["//worker"],
)

ts_compile(
    name = "leaked",
    emit = True,  # keep
    srcs = ["leaked.ts"],
    deps = ["//worker"],
)
`)
	it.MustBazel("build", "//outside:ok")
	it.Pass("the dep edge into the subtree analyses and builds")

	log, err := it.BazelLog("declaration_stops_at_the_subtree", "build", "//outside:leaked")
	if err == nil {
		log.Dump()
		it.Fail("the declaration reached a consumer outside the tsconfig's subtree")
	}
	if !log.Matches(`(?i)TS2304`) {
		log.Dump()
		it.Fail("//outside:leaked failed for some other reason than the undefined identifier")
	}
	it.Pass("the same dep edge carries no declaration out of the subtree: TS2304")
}

// worker/test names the declaration as `../` and worker/test/deep as `../../`;
// each program's own tsconfig sets the entry, and the dep is the same owner.
func parentEntryResolvesThroughTheOwner(it *harness.IT) {
	for _, dir := range []string{"worker/test", "worker/test/deep"} {
		build := it.Path(dir, "BUILD.bazel")
		for _, attr := range []string{"types =", "types_srcs", "tsconfig_types"} {
			it.RequireNotContains(build, attr, "//%s restates the tsconfig's entry as %q", dir, attr)
		}
		requireLabels(it, "deps", testTarget(dir),
			withRootManifest("//worker:worker"))
		it.Pass("//%s depends on //worker and restates nothing", dir)
	}

	targets := strings.Fields(it.BazelStdout("query", "tests(//worker/test/...)"))
	if len(targets) != 2 {
		it.Fail("expected one generated ts_test in each of //worker/test and //worker/test/deep, got %v", targets)
	}
	it.Pass("%s ran under `bazel test //...` above, so each test program resolved its entry through //worker", strings.Join(targets, " and "))

	// The measurement behind writing the dep at all: the entry names a file only
	// the dep edge stages, and the rule refuses one nothing stages before tsgo.
	below := it.Path("worker/test/BUILD.bazel")
	restore := it.Read(below)
	it.Replace(below, "        \"//worker\",\n", "")
	// Without //worker nothing publishes the root scope, and ts_test rejects staging the config's copy of it in analysis.
	dropConfigSrcs(it, "worker/test")
	log, err := it.BazelLog("types_entry_without_the_owner", "build",
		"//worker/test:test_test")
	it.Write(below, restore)
	if err == nil {
		log.Dump()
		it.Fail("//worker/test compiled without the dep; the entry alone " +
			"reaches the declaration and Gazelle need not write one")
	}
	if !log.Contains("worker/worker-configuration.d.ts, which no input of " +
		"this action sits at") {
		log.Dump()
		it.Fail("//worker/test failed for some other reason than the entry " +
			"naming a file nothing stages")
	}
	it.Pass("without the dep the entry names a file no input sits at: the " +
		"dep edge is what stages the declaration")
}

// src/app/tsconfig.json names vite/client and no file; the entry resolves through
// the chain, so what the BUILD file carries is the dep that puts vite on it.
func packageEntryIsADep(it *harness.IT) {
	build := it.Path("src/app/BUILD.bazel")
	it.RequireNotContains(build, "types =",
		"//src/app restates the tsconfig's package-only types list")
	if !slices.Contains(labels(it, "deps", "//src/app:app"), "@npm//:vite") {
		it.Fail("//src/app does not depend on the package its types entry names")
	}
	it.Pass("//src/app depends on @npm//:vite for vite/client and restates nothing")

	restore := it.Read(build)
	it.Replace(build, "        \"@npm//:vite\",\n", "")
	log, err := it.BazelLog("package_entry_without_the_dep", "build", "//src/app")
	it.Write(build, restore)
	if err == nil {
		log.Dump()
		it.Fail("//src/app compiled without the dep; the entry alone reaches vite/client and Gazelle need not write one")
	}
	if !log.Matches(`(?i)TS2688.*vite/client|TS2339`) {
		log.Dump()
		it.Fail("//src/app failed for some other reason than vite/client resolving to nothing")
	}
	it.Pass("without the dep vite/client resolves to nothing on the chain: " +
		"the dep is what puts it in the program")
}

func emitFixturePrograms(it *harness.IT) {
	for _, dir := range slices.Concat(generated, handWritten) {
		path := it.Path(dir, "BUILD.bazel")
		file, err := rule.LoadFile(path, dir)
		if err != nil {
			it.Fail("cannot load %s: %v", path, err)
		}
		for _, target := range file.Rules {
			if target.Kind() != "ts_compile" && target.Kind() != "ts_test" {
				continue
			}
			target.SetAttr("emit", true)
			target.AttrComments("emit").Suffix = []bzl.Comment{{Token: "# keep"}}
		}
		it.Write(path, string(file.Format()))
	}
}

func companionErasureBoundary(it *harness.IT) {
	dir := it.Path("erasure")
	it.RequireNoDir(dir, "the erasure fixture must be staged by this check")
	defer os.RemoveAll(dir)
	const declaration = "export type Value = number; export declare const value: number;\n"
	const appConfig = `{"extends":"../../tsconfig.json","compilerOptions":{"allowJs":true,"checkJs":true},"files":["index.ts"]}`
	const typeSource = "import type { Value } from '../lib/value.mjs'; export const answer: Value = 42;\n"
	const valueSource = "import { value } from '../lib/value.mjs'; export const answer: number = value;\n"
	const assertions = `import { expect, it } from 'vitest';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
it('runs only the declared implementation', async () => {
  const lib = resolve(dirname(fileURLToPath(import.meta.url)), '../lib');
  expect(existsSync(resolve(lib, 'value.mjs'))).toBe(RUNTIME);
  expect(existsSync(resolve(lib, 'private.mjs'))).toBe(RUNTIME);
  console.log('erasure boundary staging checked');
  const { answer } = await import('../app/index.js');
  expect(answer).toBe(42);
  console.log('erasure boundary value', answer);
});
`
	it.Write(it.Path("erasure/app/tsconfig.json"), appConfig)
	it.Write(it.Path("erasure/test/tsconfig.json"), `{"extends":"../app/tsconfig.json","files":["value.test.ts"]}`)
	it.Write(it.Path("erasure/lib/value.mjs"), "export { value } from './private.mjs';\n")
	it.Write(it.Path("erasure/lib/private.mjs"), "export const value = 42;\n")
	for _, generated := range []bool{false, true} {
		build := "# gazelle:exclude private.mjs\n"
		if generated {
			it.Write(it.Path("erasure/lib/declaration.txt"), declaration)
			build += loadTsCodegen + `
ts_codegen(
    name = "types",
    srcs = ["declaration.txt"],
    outs = ["value.d.mts"],
    args = ["{srcs}", "{out}"],
    generator = "//:json_gen",
    visibility = ["//visibility:public"],
)
`
		} else {
			build += `exports_files(["value.d.mts"], visibility = ["//visibility:public"])
`
		}
		states := []string{"authored"}
		if generated {
			states = []string{"absent", "materialized"}
		}
		for _, state := range states {
			file := it.Path("erasure/lib/value.d.mts")
			if state == "absent" {
				if err := os.Remove(file); err != nil {
					it.Fail("cannot remove authored declaration before the generated case: %v", err)
				}
			} else {
				it.Write(file, declaration)
			}
			it.Write(it.Path("erasure/lib/BUILD.bazel"), build)
			wantSrcs, wantDeps := []string{"//erasure/app:index.ts", "//erasure/lib:value.d.mts"}, []string{}
			if generated {
				wantSrcs, wantDeps = []string{"//erasure/app:index.ts"}, []string{"//erasure/lib:types"}
			}
			for _, use := range []string{"type", "value", "owned value", "value after owner removal"} {
				source := valueSource
				switch use {
				case "type":
					source = typeSource
				case "owned value":
					source = "import { value } from '../lib/entry.js'; export const answer: number = value;\n"
					it.Write(it.Path("erasure/lib/BUILD.bazel"), strings.Replace(build, "# gazelle:exclude private.mjs\n", "", 1))
					it.Write(it.Path("erasure/lib/tsconfig.json"), `{"extends":"../../tsconfig.json","compilerOptions":{"allowJs":true,"checkJs":true},"files":["entry.ts","value.mjs"]}`)
					it.Write(it.Path("erasure/lib/entry.ts"), "export { value } from './value.mjs';\n")
				case "value after owner removal":
					for _, name := range []string{"tsconfig.json", "entry.ts"} {
						if err := os.Remove(it.Path("erasure/lib", name)); err != nil {
							it.Fail("cannot remove the explicit runtime owner input %s: %v", name, err)
						}
					}
					it.Write(it.Path("erasure/lib/BUILD.bazel"), build)
				}
				it.Write(it.Path("erasure/app/index.ts"), source)
				it.Write(it.Path("erasure/test/value.test.ts"), strings.ReplaceAll(assertions, "RUNTIME", fmt.Sprint(use == "owned value")))
				it.MustBazel("run", "//:gazelle")
				if use == "owned value" {
					requireLabels(it, "srcs", "//erasure/app:app", []string{"//erasure/app:index.ts"})
					requireLabels(it, "deps", "//erasure/app:app", []string{"//erasure/lib:lib"})
					ownerDeps := []string{}
					if generated {
						ownerDeps = []string{"//erasure/lib:types"}
					}
					requireLabels(it, "deps", "//erasure/lib:lib", ownerDeps)
					for _, runtime := range []string{"//erasure/lib:entry.ts", "//erasure/lib:value.mjs", "//erasure/lib:private.mjs"} {
						if !slices.Contains(labels(it, "srcs", "//erasure/lib:lib"), runtime) {
							it.Fail("the compiler-rooted runtime owner omitted %s", runtime)
						}
					}
				} else {
					requireLabels(it, "srcs", "//erasure/app:app", wantSrcs)
					requireLabels(it, "deps", "//erasure/app:app", wantDeps)
				}
				it.MustBazel("build", "//erasure/app:app", "//erasure/test:test_test", "--output_groups=+_validation", "--noenable_runfiles")
				log, err := it.BazelLog(fmt.Sprintf("erasure_%t_%s_%s", generated, state, strings.ReplaceAll(use, " ", "_")), "test", "//erasure/test:test_test", "--noenable_runfiles", "--test_output=all")
				if use == "value" || use == "value after owner removal" {
					if err == nil || !log.Contains("erasure boundary staging checked") || !log.Contains("value.mjs") || !log.Matches(`runfiles do not hold|Failed to resolve import|Cannot find module|ERR_MODULE_NOT_FOUND`) || log.Contains("erasure boundary value 42") {
						log.Dump()
						it.Fail("%s/%s: undeclared implementation did not fail at the runtime staging boundary: %v", state, use, err)
					}
				} else if err != nil || !log.Contains("erasure boundary value 42") {
					log.Dump()
					it.Fail("%s/%s: expected runtime value missing: %v", state, use, err)
				}
			}
		}
	}
	it.Pass("erased authored and generated declaration imports validate and run without runtime inputs under checkJs")
	it.Pass("value consumers reject undeclared implementations before and after a compiler-rooted owner runs the expected value")
}

func nativeDescendantRetainsRuntimeInputs(it *harness.IT) {
	const pkg = "native_descendant"
	it.RequireNoDir(it.Path(pkg), "the descendant fixture must start absent")
	defer os.RemoveAll(it.Path(pkg))
	it.Write(it.Path(pkg, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary")
ts_binary(name = "run", entry_point = "entry.mjs", data = ["late.mjs", "asset.txt"])
`)
	it.Write(it.Path(pkg, "late.mjs"), "export const answer = 42; export const loadedFrom = import.meta.url;\n")
	it.Write(it.Path(pkg, "asset.txt"), "surviving child asset")
	it.Write(it.Path(pkg, "entry.mjs"), `import { spawn } from 'node:child_process';
import { readFileSync, realpathSync } from 'node:fs';
import { sep } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
if (process.argv[2] === 'child') {
  process.send('ready');
  const input = createInterface({ input: process.stdin });
  for await (const line of input) {
    if (line !== 'read') throw new Error('unexpected child command');
    const { answer, loadedFrom } = await import('./late.mjs');
    const group = process.env.TS_NATIVE_TEST_GROUP + sep;
    if (![fileURLToPath(import.meta.url), fileURLToPath(loadedFrom), realpathSync(new URL('./asset.txt', import.meta.url))].every(path => path.startsWith(group))) throw new Error('runtime escaped relocated output group');
    if (answer !== 42 || readFileSync(new URL('./asset.txt', import.meta.url), 'utf8') !== 'surviving child asset') throw new Error('runtime inputs changed');
    console.log('surviving child read runtime inputs');
    process.exit(0);
  }
} else {
  const child = spawn(process.execPath, [fileURLToPath(import.meta.url), 'child'], { stdio: ['inherit', 'inherit', 'inherit', 'ipc'] });
  child.once('message', () => {
    console.log('child ready ' + child.pid);
    if (process.argv[2] === 'normal') process.exit(0);
  });
}
`)
	it.MustBazel("build", "//"+pkg+":run")
	originalLauncher := it.Bin(pkg, "run_launcher")
	configFile := originalLauncher + ".json"
	var config struct {
		Runtime          string `json:"runtime"`
		NativeViewAnchor string `json:"native_view_anchor"`
	}
	contents := it.Read(configFile)
	if err := json.Unmarshal([]byte(contents), &config); err != nil {
		it.Fail("native config: %v", err)
	}
	if config.NativeViewAnchor == "" {
		it.Fail("native config has no canonical view anchor")
	}
	tool, err := filepath.EvalSymlinks(filepath.Join(originalLauncher+".runfiles", filepath.FromSlash(config.Runtime)))
	if err != nil {
		it.Fail("resolve original runtime tool: %v", err)
	}
	relocation, err := os.MkdirTemp(it.Scratch(), "native-relocation-")
	if err != nil {
		it.Fail("relocation directory: %v", err)
	}
	defer os.RemoveAll(relocation)
	canonicalConfig, err := filepath.EvalSymlinks(configFile)
	if err != nil {
		it.Fail("resolve action-owned native config: %v", err)
	}
	oldPrefix := strings.TrimSuffix(canonicalConfig, ".json") + ".runtime"
	movedPrefix := filepath.Join(relocation, filepath.Base(oldPrefix))
	if err := filepath.WalkDir(oldPrefix, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(oldPrefix, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(movedPrefix, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(target, destination)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, info.Mode().Perm())
	}); err != nil {
		it.Fail("copy complete declared output group: %v", err)
	}
	relocatedConfig := filepath.Join(relocation, filepath.Base(canonicalConfig))
	it.Write(relocatedConfig, contents)

	for _, tc := range []struct{ ending, layout string }{{"normal", "directory"}, {"SIGTERM", "manifest"}} {
		ending := tc.ending
		func() {
			input, command, err := os.Pipe()
			if err != nil {
				it.Fail("child input pipe: %v", err)
			}
			defer input.Close()
			defer command.Close()
			output, write, err := os.Pipe()
			if err != nil {
				it.Fail("child output pipe: %v", err)
			}
			defer output.Close()
			defer write.Close()
			launcher := filepath.Join(relocation, tc.layout, "run_launcher")
			if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
				it.Fail("resolver fixture: %v", err)
			}
			if err := os.Symlink(originalLauncher, launcher); err != nil {
				it.Fail("relocated launcher: %v", err)
			}
			manifest := launcher + ".runfiles_manifest"
			tree := launcher + ".runfiles"
			if tc.layout == "manifest" {
				it.Write(manifest, "run_launcher.json "+relocatedConfig+"\n"+config.NativeViewAnchor+" "+relocatedConfig+"\n"+config.Runtime+" "+tool+"\n")
				it.RequireNoDir(tree, "manifest resolver must have no directory tree")
			} else {
				it.RequireNoFile(manifest, "directory resolver must have no manifest")
				for path, target := range map[string]string{"run_launcher.json": relocatedConfig, config.NativeViewAnchor: relocatedConfig, config.Runtime: tool} {
					link := filepath.Join(tree, filepath.FromSlash(path))
					if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
						it.Fail("directory runfiles: %v", err)
					}
					if err := os.Symlink(target, link); err != nil {
						it.Fail("directory runfile: %v", err)
					}
				}
			}
			cmd := exec.Command(launcher, ending)
			environment := []string{"TS_LAUNCHER_CONFIG=", "TS_NATIVE_TEST_GROUP=" + movedPrefix, "RUNFILES_DIR=", "RUNFILES_MANIFEST_FILE=", "RUNFILES_MANIFEST_ONLY="}
			if tc.layout == "manifest" {
				environment = append(environment, "RUNFILES_MANIFEST_FILE="+manifest)
			} else {
				environment = append(environment, "RUNFILES_DIR="+tree)
			}
			cmd.Env = append(os.Environ(), environment...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = input, write, os.Stderr
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				it.Fail("start native parent: %v", err)
			}
			defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			write.Close()
			lines := bufio.NewScanner(output)
			if !lines.Scan() || !strings.HasPrefix(lines.Text(), "child ready ") {
				it.Fail("child did not acknowledge readiness: %q, %v", lines.Text(), lines.Err())
			}
			if ending == "SIGTERM" {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					it.Fail("terminate native parent: %v", err)
				}
			}
			err = cmd.Wait()
			if ending == "normal" && err != nil {
				it.Fail("native parent exit: %v", err)
			}
			if ending == "SIGTERM" && cmd.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
				it.Fail("native parent lost direct signal status: %v", err)
			}
			if _, err := fmt.Fprintln(command, "read"); err != nil {
				it.Fail("release surviving child: %v", err)
			}
			if !lines.Scan() || lines.Text() != "surviving child read runtime inputs" {
				it.Fail("child lost runtime inputs after parent %s: %q, %v", ending, lines.Text(), lines.Err())
			}
		}()
	}
	it.Pass("directory and manifest relocated views retain child imports and assets after normal and direct-SIGTERM parent exit")
}
