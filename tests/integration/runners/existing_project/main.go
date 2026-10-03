package main

import (
	"fmt"
	"strings"

	"github.com/mikn/rules_typescript/tests/integration/harness"
)

func main() {
	harness.Run(harness.Config{
		Name:         "existing_project",
		WorkspaceRel: "tests/integration/existing_project",
	}, func(it *harness.IT) {
		it.Write(it.Path("src/lib/package.json"), `{"main":"./math.js"}`+"\n")
		it.Write(it.Path("src/broken/package.json"), `{"main":"./type_error.js"}`+"\n")
		it.MustBazel("run", "//:gazelle")
		it.Pass("bazel run //:gazelle")

		build := it.Path("src/lib/BUILD.bazel")
		it.RequireFile(build, "Gazelle did not generate src/lib/BUILD.bazel")
		it.Pass("src/lib/BUILD.bazel generated")

		if strings.Contains(it.Read(build), "declarations") {
			it.Dump(build)
			it.Fail("Gazelle emitted a declarations attribute; the tsgo default needs none")
		}
		it.Pass("src/lib/BUILD.bazel has no declarations attribute (tsgo default)")

		it.MustBazel("build", "//src/lib:all", "--output_groups=+declarations")
		it.Pass("bazel build //src/lib:all --output_groups=+declarations")

		for _, rel := range []string{"src/lib/math.js", "src/lib/math.d.ts"} {
			it.RequireFile(it.Bin(rel), "expected output file not found: %s", rel)
			it.Pass("output file exists: %s", rel)
		}

		// This is the reason tsgo owns declaration emit: a syntactic emitter
		// cannot infer these and would widen them silently.
		dts := it.Bin("src/lib/math.d.ts")
		it.Dump(dts)
		for _, fn := range []string{"add", "multiply", "subtract", "divide"} {
			it.RequireMatches(dts, fmt.Sprintf(`declare function %s\(a: number, b: number\): number`, fn),
				"%s() lost its inferred 'number' return type in math.d.ts", fn)
			it.Pass("%s(): number inferred in math.d.ts", fn)
		}
		it.RequireNotMatches(dts, `:[[:blank:]]*(unknown|\{\})[[:blank:]]*;`,
			"math.d.ts widened an export to 'unknown' or '{}'")
		it.Pass("math.d.ts contains no widened exports")

		// TsgoCheck is a validation: Bazel runs it with the build it belongs to.
		broken, err := it.BazelLog("broken.log", "build", "//src/broken:all")
		if err == nil {
			broken.Dump()
			it.Fail("//src/broken:all built successfully; a type error must fail the build")
		}
		it.Pass("//src/broken:all failed the build without --output_groups=+_validation")

		if !broken.Matches(`TS[0-9]{4}|not assignable`) {
			broken.Dump()
			it.Fail("build failed, but not with a type diagnostic")
		}
		it.Pass("failure names the type error")

		it.RequireNoFile(it.Bin("src/broken/type_error.d.ts"),
			"a failing target still produced src/broken/type_error.d.ts")
		it.Pass("no .d.ts was produced for the failing target")

		sharedSrc(it)
		srcsShapesStillBuild(it)
	})
}

func sharedSrc(it *harness.IT) {
	it.Write(it.Path("shared/BUILD.bazel"), "exports_files([\"util.ts\"])\n")
	it.Write(it.Path("shared/util.ts"), "export const util = 1;\n")
	it.Write(it.Path("consumer/main.ts"), "import { util } from '../shared/util.js';\nexport function main() { return util; }\n")
	it.Write(it.Path("consumer/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "consumer",
    emit = True,
    srcs = [
        "main.ts",
        "//shared:util.ts",
    ],
)
`)

	it.MustBazel("build", "//consumer:consumer", "--output_groups=+declarations")
	for _, rel := range []string{
		"consumer/consumer/main.js",
		"consumer/consumer/main.d.ts",
		"consumer/shared/util.js",
		"consumer/shared/util.d.ts",
	} {
		it.RequireFile(it.Bin(rel), "the common source layout lost its output: %s", rel)
	}
	it.RequireMatches(it.Bin("consumer/consumer/main.js"), `['"]\.\./shared/util\.js['"]`,
		"the emitted consumer changed its relative import to the borrowed source")
	it.RequireMatches(it.Bin("consumer/consumer/main.d.ts"), `declare function main\(\): number`,
		"the borrowed source lost its inferred return type in the consumer declaration")
	it.Pass("borrowed sources retain relative imports and declarations in the consumer's common layout")
}

func srcsShapesStillBuild(it *harness.IT) {
	it.Write(it.Path("holder/a.ts"), "export const a = 1;\n")
	it.Write(it.Path("holder/sub/x.ts"), "export const x = 1;\n")
	it.Write(it.Path("holder/sub/BUILD.bazel"), "exports_files([\"x.ts\"])\n")
	it.Write(it.Path("holder/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "holder",
    emit = True,
    srcs = [
        "a.ts",
        "//holder/sub:x.ts",
    ],
)
`)

	it.Write(it.Path("chooses/a.ts"), "export const a = 1;\n")
	it.Write(it.Path("chooses/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "chooses",
    emit = True,
    srcs = select({"//conditions:default": ["a.ts"]}),
)
`)

	it.Write(it.Path("canonical/a.ts"), "export const a = 1;\n")
	it.Write(it.Path("canonical/b.ts"), "export const b = 1;\n")
	it.Write(it.Path("canonical/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "canonical",
    emit = True,
    srcs = [
        "a.ts",
        "@@//canonical:b.ts",
    ],
)
`)

	it.Write(it.Path("toplevel.ts"), "export const toplevel = 1;\n")
	it.Write(it.Path("BUILD.bazel"), it.Read(it.Path("BUILD.bazel"))+`
load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "toplevel",
    emit = True,
    srcs = [
        "toplevel.ts",
        "//shared:util.ts",
    ],
)
`)

	it.MustBazel("build", "//holder:holder", "//chooses:chooses",
		"//canonical:canonical", "//:toplevel", "--output_groups=+declarations")
	it.Pass("bazel build //holder //chooses //canonical //:toplevel, declarations")

	for _, rel := range []string{
		"holder/a.d.ts",
		"holder/sub/x.d.ts",
		"chooses/a.js",
		"canonical/a.d.ts",
		"canonical/b.d.ts",
		"toplevel.d.ts",
		"shared/util.d.ts",
	} {
		it.RequireFile(it.Bin(rel), "a supported srcs shape lost its output: %s", rel)
	}
	it.Pass("a descendant src, a select, a canonical self-label and a top-level foreign src all still emit")
}
