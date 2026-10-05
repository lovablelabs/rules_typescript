package typescript

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/rule"
)

func TestEmissionLeavesFilePlacementToCompilerConstruction(t *testing.T) {
	for _, mapping := range []struct{ kind, directives string }{
		{"ts_compile", ""},
		{"custom_compile", "# gazelle:map_kind ts_compile custom_compile //:defs.bzl\n"},
		{"final_compile", "# gazelle:map_kind ts_compile custom_compile //:defs.bzl\n# gazelle:map_kind custom_compile final_compile //:defs.bzl\n"},
	} {
		t.Run(mapping.kind, func(t *testing.T) {
			for _, input := range []struct {
				file, source, importer string
			}{
				{"value.ts", "export const value = 1;\n", "export { value } from '../fixtures/value.js';\n"},
				{"value.json", `{"value":1}`, "import data from '../fixtures/value.json'; export const value = data.value;\n"},
			} {
				t.Run(input.file, func(t *testing.T) {
					root := writeTree(t, map[string]string{
						"MODULE.bazel":           "module(name = \"sibling_emission\")\n",
						"BUILD.bazel":            "",
						"app/BUILD.bazel":        mapping.directives,
						"app/tsconfig.json":      `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true},"include":["*.ts"]}`,
						"app/index.ts":           input.importer,
						"fixtures/" + input.file: input.source,
						"fixtures/BUILD.bazel":   "exports_files([\"" + input.file + "\"], visibility = [\"//app:__pkg__\"])\n",
					})
					output, err := protoGazelle(t, root)
					if err != nil {
						t.Fatalf("source-mode generation: %v\n%s", err, output)
					}
					sourceBuild, err := os.ReadFile(filepath.Join(root, "app/BUILD.bazel"))
					if err != nil {
						t.Fatal(err)
					}
					compile := onDiskRule(t, root, "app", mapping.kind, "app")
					wantLabels(t, "source mode retains sibling input", compile.AttrStrings("srcs"), []string{"index.ts", "//fixtures:" + input.file})
					triggers := []string{"explicit emit", "conditional emit"}
					if mapping.kind == "ts_compile" {
						triggers = append(triggers, "node boundary")
					}
					for _, trigger := range triggers {
						t.Run(trigger, func(t *testing.T) {
							writeFile(t, filepath.Join(root, "app/BUILD.bazel"), string(sourceBuild))
							if trigger == "node boundary" {
								writeFile(t, filepath.Join(root, "BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary")
ts_binary(name = "run", entry_point = "//app")
`)
							} else {
								writeFile(t, filepath.Join(root, "BUILD.bazel"), "")
								emit := "True"
								if trigger == "conditional emit" {
									emit = `select({"//conditions:default": True})`
								}
								writeFile(t, filepath.Join(root, "app/BUILD.bazel"), strings.Replace(string(sourceBuild), mapping.kind+"(", mapping.kind+"(\n    emit = "+emit+",  # keep", 1))
							}
							output, err := protoGazelle(t, root)
							if err != nil {
								t.Fatalf("%s rejected source identity before File construction: %v\n%s", trigger, err, output)
							}
							compile := onDiskRule(t, root, "app", mapping.kind, "app")
							wantLabels(t, "emission retains sibling input identity", compile.AttrStrings("srcs"), []string{"index.ts", "//fixtures:" + input.file})
						})
					}
					for _, keep := range []string{"rule", "srcs", "element", "authored rule"} {
						t.Run("authored "+keep, func(t *testing.T) {
							body := mapping.directives + "load(\"@rules_typescript//ts:defs.bzl\", \"ts_compile\")\n"
							if keep == "rule" {
								body += "# keep\n"
							}
							name := "app"
							if keep == "authored rule" {
								name = "custom"
								writeFile(t, filepath.Join(root, "app/index.ts"), "export const local = 1;\n")
							}
							body += "ts_compile(\n    name = \"" + name + "\",\n    emit = True,  # keep\n    srcs = ["
							if keep == "element" {
								body += "\n        \"//fixtures:" + input.file + "\",  # keep\n    "
							} else {
								body += "\"//fixtures:" + input.file + "\""
							}
							body += "],"
							if keep == "srcs" {
								body += "  # keep"
							}
							writeFile(t, filepath.Join(root, "app/BUILD.bazel"), body+"\n)\n")
							output, err := protoGazelle(t, root)
							if err != nil {
								t.Fatalf("authored source layout was rejected: %v\n%s", err, output)
							}
							compile := onDiskRule(t, root, "app", mapping.kind, name)
							want := []string{"//fixtures:" + input.file}
							if keep == "element" {
								want = append(want, "index.ts")
							}
							wantLabels(t, "authored srcs", compile.AttrStrings("srcs"), want)
							if keep == "element" {
								writeFile(t, filepath.Join(root, "fixtures/other.ts"), "export const other = 2;\n")
								writeFile(t, filepath.Join(root, "fixtures/BUILD.bazel"), "exports_files([\""+input.file+"\", \"other.ts\"])\n")
								writeFile(t, filepath.Join(root, "app/index.ts"), input.importer+"export { other } from '../fixtures/other.js';\n")
								output, err = protoGazelle(t, root)
								if err != nil {
									t.Fatalf("additional source identity was rejected: %v\n%s", err, output)
								}
								compile = onDiskRule(t, root, "app", mapping.kind, name)
								wantLabels(t, "kept and observed source identities", compile.AttrStrings("srcs"), []string{"index.ts", "//fixtures:" + input.file, "//fixtures:other.ts"})
							}
						})
					}
				})
			}
		})
	}
}

func TestEmission_SiblingTestSourcesKeepRuntimePlacement(t *testing.T) {
	for _, layout := range []struct{ name, keep string }{
		{"cross_package_test", "  # keep"},
		{"test_test", "  # keep"},
		{"test_test", ""},
	} {
		t.Run(layout.name+layout.keep, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel":               "module(name = \"cross_package\")\n",
				"test/tsconfig.json":         `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"include":["*.ts"]}`,
				"test/cross_package.test.ts": "import { engine } from '../lib/engine.js'; export const result = engine;\n",
				"lib/engine.ts":              "import { state } from './state.js'; export const engine = state;\n",
				"lib/state.ts":               "export const state = 1;\n",
				"lib/BUILD.bazel":            "exports_files([\"engine.ts\", \"state.ts\"], visibility = [\"//test:__pkg__\"])\n",
				"test/BUILD.bazel": loadDefs + `"ts_test")
ts_test(
    name = "` + layout.name + `",
    srcs = [
        "cross_package.test.ts",
        "//lib:engine.ts",` + layout.keep + `
        "//lib:state.ts",` + layout.keep + `
    ],
    emit = True,  # keep
)
`,
			})
			output, err := protoGazelle(t, root)
			if err != nil {
				t.Fatalf("supported emitted test layout was rejected: %v\n%s", err, output)
			}
			test := onDiskRule(t, root, "test", "ts_test", layout.name)
			wantLabels(t, "test srcs", test.AttrStrings("srcs"), []string{"//lib:engine.ts", "//lib:state.ts", "cross_package.test.ts"})
			if !strings.Contains(buildFileText(t, root, "test"), "emit = True") {
				t.Fatal("test lost its explicit emission")
			}
		})
	}
}

func TestEmission_NodeBoundaryEmitsNewDependenciesInOneRun(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			requireTsgo(t)
			root := t.TempDir()
			writeWorkspace(t, root, map[string]string{
				"package.json": convergePlainPkg,
				"BUILD.bazel": `load("@rules_typescript//ts:defs.bzl", "ts_binary")
exports_files(["package.json"], visibility = ["//app:__pkg__", "//lib:__pkg__", "//native:__pkg__"])
ts_binary(name = "run", entry_point = "//app")
`,
				"app/tsconfig.json":    `{"compilerOptions":{"module":"esnext","moduleResolution":"bundler"},"include":["*.ts"]}`,
				"app/index.ts":         `import {value} from "../lib/index"; export const result = value;`,
				"lib/tsconfig.json":    `{"compilerOptions":{"module":"esnext","moduleResolution":"bundler"},"include":["*.ts"]}`,
				"lib/index.ts":         `export const value = 1;`,
				"native/tsconfig.json": `{"include":["*.ts"]}`,
				"native/index.ts":      `export const untouched = 1;`,
			})
			if existing {
				for _, pkg := range []string{"app", "lib"} {
					writeWorkspace(t, root, map[string]string{pkg + "/BUILD.bazel": "load(\"@rules_typescript//ts:defs.bzl\", \"ts_compile\")\nts_compile(name = \"" + pkg + "\", srcs = [\"index.ts\"])\n"})
				}
			}
			captureLog(t, func() { convergeGazelle(t, root) })
			first := map[string]string{}
			for _, pkg := range []string{"app", "lib", "native"} {
				first[pkg] = buildFileText(t, root, pkg)
				want := pkg != "native"
				if strings.Contains(first[pkg], "emit = True") != want {
					t.Fatalf("%s emission boundary wrong:\n%s", pkg, first[pkg])
				}
			}
			wantLabels(t, "unrooted compiler retains its ancestor scope", onDiskRule(t, root, "native", "ts_compile", "native").AttrStrings("package_scopes"), []string{"//:package.json"})
			logs := captureLog(t, func() { convergeGazelle(t, root) })
			if strings.Contains(logs, "declares emit as an expression") {
				t.Fatalf("generated boolean reported as unmergeable: %s", logs)
			}
			for pkg, body := range first {
				if next := buildFileText(t, root, pkg); next != body {
					t.Fatalf("%s needs second run:\n%s", pkg, lineDiff(body, next))
				}
			}

			writeWorkspace(t, root, map[string]string{"BUILD.bazel": ""})
			captureLog(t, func() { convergeGazelle(t, root) })
			for _, pkg := range []string{"app", "lib"} {
				if text := buildFileText(t, root, pkg); strings.Contains(text, "emit = True") {
					t.Fatalf("%s retained emission after its consumer was removed:\n%s", pkg, text)
				}
			}
		})
	}
}

func emissionPublicationTree(directive, entry string) map[string]string {
	return map[string]string{
		"MODULE.bazel": "module(name = \"emission_publication\")\n",
		"BUILD.bazel": loadDefs + `"ts_binary")
ts_binary(name = "run", entry_point = "//` + entry + `")
`,
		"app/BUILD.bazel": loadDefs + `"ts_compile")
ts_compile(name = "app", srcs = ["index.ts", "//fixtures:value.ts"], deps = ["//lib"], tsconfig = "tsconfig.json")
`,
		"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#value":["../fixtures/value.ts"]}},"files":["index.ts"]}`,
		"app/index.ts":      "export { value } from '../lib/index.js';\n",
		"lib/BUILD.bazel": directive + loadDefs + `"ts_compile")
ts_compile(name = "lib", srcs = ["index.ts", "native.ts"], tsconfig = "tsconfig.json", visibility = ["//visibility:public"])
`,
		"lib/tsconfig.json":    `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#value":["./native.ts"]}},"files":["index.ts","native.ts"]}`,
		"lib/index.ts":         "export { value } from '#value';\n",
		"lib/native.ts":        "export const value = 'owner';\n",
		"fixtures/BUILD.bazel": "exports_files([\"value.ts\"], visibility = [\"//app:__pkg__\"])\n",
		"fixtures/value.ts":    "export const value = 'consumer';\n",
	}
}

func formattedEmissionOwner(t *testing.T, text string) string {
	t.Helper()
	file, err := rule.LoadData("lib/BUILD.bazel", "lib", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return string(file.Format())
}

func TestEmission_PartialUpdateCannotInventSiblingEmission(t *testing.T) {
	for _, update := range []struct {
		name, directive string
		args            []string
	}{
		{"indexed sibling", "", []string{"-r=false", "app"}},
		{"unindexed sibling", "", []string{"-index=false", "-r=false", "app"}},
		{"ignored owner", "# gazelle:ignore\n", []string{"app", "lib"}},
		{"language opt-out", "# gazelle:lang proto\n", []string{"app", "lib"}},
	} {
		t.Run(update.name, func(t *testing.T) {
			files := emissionPublicationTree(update.directive, "lib")
			files["lib/BUILD.bazel"] = formattedEmissionOwner(t, files["lib/BUILD.bazel"])
			root := writeTree(t, files)
			var first map[string]string
			for _, pass := range []string{"partial first", "repeated"} {
				if output, err := protoGazelle(t, root, update.args...); err != nil {
					t.Fatalf("%s update: %v\n%s", pass, err, output)
				}
				if got := buildFileText(t, root, "lib"); got != files["lib/BUILD.bazel"] {
					t.Fatalf("observed owner changed: %s", lineDiff(files["lib/BUILD.bazel"], got))
				}
				consumer := onDiskRule(t, root, "app", "ts_compile", "app")
				wantLabels(t, "untouched source owner retains the consumer's resolution", consumer.AttrStrings("srcs"), []string{"index.ts", "//fixtures:value.ts"})
				wantLabels(t, "source owner supplies its declared inputs", consumer.AttrStrings("deps"), []string{"//lib"})
				if first == nil {
					first = buildFileBytes(t, root)
				} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("partial publication did not converge: %s", diff)
				}
			}
		})
	}
}

func TestEmission_RootedUpdateCannotRequireUnpublishedPromotion(t *testing.T) {
	for _, update := range []struct {
		name, directive string
		args            []string
	}{
		{"unindexed sibling", "", []string{"-index=false", "-r=false", "app"}},
		{"ignored owner", "# gazelle:ignore\n", []string{"app", "lib"}},
		{"language opt-out", "# gazelle:lang proto\n", []string{"app", "lib"}},
	} {
		for _, inputs := range []string{"TypeScript", "JavaScript", "declarations", "JSON"} {
			t.Run(update.name+"/"+inputs, func(t *testing.T) {
				files := emissionPublicationTree(update.directive, "app")
				switch inputs {
				case "JavaScript":
					files["lib/BUILD.bazel"] = strings.Replace(files["lib/BUILD.bazel"], `["index.ts", "native.ts"]`, `["index.mjs", "index.d.mts", "value.json"]`, 1)
					files["lib/index.mjs"] = "export const value = 'runtime';\n"
					files["lib/index.d.mts"] = "export declare const value: string;\n"
					files["lib/value.json"] = `{"value":42}`
					files["app/index.ts"] = "export { value } from '../lib/index.mjs';\n"
				case "declarations":
					files["lib/BUILD.bazel"] = strings.Replace(files["lib/BUILD.bazel"], `["index.ts", "native.ts"]`, `["value.d.ts"]`, 1)
					files["lib/value.d.ts"] = "export interface Value { value: string }\n"
					files["app/index.ts"] = "export type { Value } from '../lib/value.js';\n"
				case "JSON":
					files["lib/BUILD.bazel"] = strings.Replace(files["lib/BUILD.bazel"], `["index.ts", "native.ts"]`, `["value.json"]`, 1)
					files["lib/value.json"] = `{"value":42}`
					files["app/tsconfig.json"] = strings.Replace(files["app/tsconfig.json"], `"module":"preserve"`, `"module":"preserve","resolveJsonModule":true`, 1)
					files["app/index.ts"] = "import value from '../lib/value.json'; export const result = value;\n"
				}
				files["lib/BUILD.bazel"] = formattedEmissionOwner(t, files["lib/BUILD.bazel"])
				root := writeTree(t, files)
				owner := files["lib/BUILD.bazel"]
				if inputs == "TypeScript" {
					before := buildFileBytes(t, root)
					output, err := protoGazelle(t, root, update.args...)
					if err == nil || !strings.Contains(output, "//app") || !strings.Contains(output, "requires emission from //lib for runtime TypeScript input") || !strings.Contains(output, "include that package in this update") {
						t.Fatalf("unpublished owner promotion was accepted: %v\n%s", err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("rejected publication changed BUILD files: %s", diff)
					}
					owner = formattedEmissionOwner(t, strings.Replace(owner, `name = "lib",`, `name = "lib", emit = True,`, 1))
					writeFile(t, filepath.Join(root, "lib/BUILD.bazel"), owner)
				}
				if output, err := protoGazelle(t, root, update.args...); err != nil {
					t.Fatalf("declared runtime owner was rejected: %v\n%s", err, output)
				}
				if got := buildFileText(t, root, "lib"); got != owner {
					t.Fatalf("declared emission changed outside publication: %s", lineDiff(owner, got))
				}
				consumer := onDiskRule(t, root, "app", "ts_compile", "app")
				wantLabels(t, "published owner supplies the consumer boundary", consumer.AttrStrings("srcs"), []string{"index.ts"})
				wantLabels(t, "published compiler owner remains a dependency", consumer.AttrStrings("deps"), []string{"//lib"})
				if !strings.Contains(buildFileText(t, root, "app"), "emit = True") {
					t.Fatal("rooted consumer did not publish its emission")
				}
			})
		}
	}
}

func TestEmission_DirectRuntimeConsumerCannotLoseUnpublishedDemand(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, alias := range []bool{false, true} {
			for _, emission := range []string{"", `emit = select({"//conditions:default": False}),`, `emit = select({"//conditions:default": True}),`} {
				for _, consumer := range []string{"updated", "observed", "kept"} {
					t.Run(fmt.Sprintf("indexed=%t/alias=%t/emit=%s/%s", indexed, alias, emission, consumer), func(t *testing.T) {
						entry := "//lib:lib"
						if alias {
							entry = "//bridge:entry"
						}
						binary := loadDefs + `"ts_binary")` + "\n"
						if consumer == "kept" {
							binary += "# keep\n"
						}
						binary += fmt.Sprintf("ts_binary(name = \"run\", entry_point = %q)\n", entry)
						tree := map[string]string{
							"MODULE.bazel":       "module(name = \"direct_runtime_publication\")\n",
							"BUILD.bazel":        "",
							"app/BUILD.bazel":    binary,
							"app/tsconfig.json":  `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
							"app/index.ts":       "export const unrelated = 1;\n",
							"bridge/BUILD.bazel": `alias(name = "entry", actual = "//lib:lib", visibility = ["//visibility:public"])` + "\n",
							"lib/BUILD.bazel": loadDefs + `"ts_compile")
ts_compile(name = "lib", ` + emission + ` srcs = ["index.ts"], visibility = ["//visibility:public"])
`,
							"lib/index.ts": "export const value = 42;\n",
						}
						if consumer == "observed" {
							tree["BUILD.bazel"], tree["app/BUILD.bazel"] = binary, ""
						}
						root := writeTree(t, tree)
						args := []string{fmt.Sprintf("-index=%t", indexed), "-r=false", "app"}
						before := buildFileBytes(t, root)
						output, err := protoGazelle(t, root, args...)
						if consumer == "updated" {
							if err == nil || !strings.Contains(output, "//app:run requires emission from //lib for runtime TypeScript input lib/index.ts") {
								t.Fatalf("direct runtime demand disappeared: %v\n%s", err, output)
							}
							if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
								t.Fatalf("rejected direct demand changed BUILD files: %s", diff)
							}
							tree["lib/BUILD.bazel"] = strings.Replace(tree["lib/BUILD.bazel"], `name = "lib", `+emission, `name = "lib", emit = True,`, 1)
							writeFile(t, filepath.Join(root, "lib/BUILD.bazel"), tree["lib/BUILD.bazel"])
							output, err = protoGazelle(t, root, args...)
						}
						if err != nil {
							t.Fatalf("satisfied, observed or manual runtime consumer rejected: %v\n%s", err, output)
						}
						if got := buildFileText(t, root, "lib"); got != tree["lib/BUILD.bazel"] {
							t.Fatalf("untouched owner changed: %s", lineDiff(tree["lib/BUILD.bazel"], got))
						}
						if strings.Contains(buildFileText(t, root, "app"), "emit = True") {
							t.Fatal("direct runtime demand promoted the unrelated app program")
						}
					})
				}
			}
		}
	}
}

func TestEmission_KeptOwnerCannotCertifyUnemittedRuntimeInputs(t *testing.T) {
	for _, owner := range []struct {
		name, prefix, emission string
		emitted                bool
	}{
		{"whole source owner", "# keep\n", "", false},
		{"whole unknown owner", "# keep\n", `emit = select({"//conditions:default": True}),`, false},
		{"kept source attribute", "", "emit = False, # keep\n", false},
		{"kept unknown source attribute", "", "emit = select({\"//conditions:default\": False}), # keep\n", false},
		{"kept unknown emitted attribute", "", "emit = select({\"//conditions:default\": True}), # keep\n", false},
		{"kept emitted attribute", "", "emit = True, # keep\n", true},
	} {
		for _, manual := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/manual=%t", owner.name, manual), func(t *testing.T) {
				binary := `ts_binary(name = "run", entry_point = ":app")` + "\n"
				if manual {
					binary = "# keep\n" + binary
				}
				root := writeTree(t, map[string]string{
					"MODULE.bazel": "module(name = \"kept_runtime_owner\")\n",
					"BUILD.bazel":  "",
					"app/BUILD.bazel": loadDefs + `"ts_binary", "ts_compile")` + "\n" + binary + owner.prefix +
						`ts_compile(name = "app", ` + owner.emission + ` srcs = ["index.ts"])` + "\n",
					"app/tsconfig.json": `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
					"app/index.ts":      "export const value = 42;\n",
				})
				before := buildFileBytes(t, root)
				output, err := protoGazelle(t, root, "-r=false", "app")
				if !manual && !owner.emitted {
					if err == nil || !strings.Contains(output, "//app:run requires emission from //app for runtime TypeScript input app/index.ts") {
						t.Fatalf("kept owner in updated package certified runtime publication: %v\n%s", err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("kept owner rejection changed BUILD files: %s", diff)
					}
				} else if err != nil {
					t.Fatalf("manual consumer or known emitted owner rejected: %v\n%s", err, output)
				}
			})
		}
	}
	for _, nativeDir := range []string{"", "messages"} {
		name := "fixed protobuf source owner"
		if nativeDir != "" {
			name += " with nested native"
		}
		for _, kept := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/kept=%t", name, kept), func(t *testing.T) {
				prefix := ""
				if kept {
					prefix = "# keep\n"
				}
				wrapper := path.Join("plain", nativeDir, "example_message_proto")
				native := ":example_message_proto"
				if nativeDir != "" {
					native = "//" + path.Join("schema", nativeDir) + ":example_message_proto"
				}
				generatedDir := path.Join("schema/generated", nativeDir)
				consumer := loadDefs + `"ts_test")
ts_test(name = "app_test", srcs = ["index.test.ts"], runner = %q)
`
				tree := map[string]string{
					"MODULE.bazel":               "module(name = \"fixed_proto_emission\")\n",
					"BUILD.bazel":                "",
					"node_modules/.modules.yaml": "layoutVersion: 5\n",
					"pnpm-lock.yaml": `lockfileVersion: '9.0'
importers:
  .: {}
  app:
    devDependencies:
      vitest:
        specifier: 4.1.11
        version: 4.1.11
packages:
  vitest@4.1.11:
    resolution: {integrity: sha512-aaa}
snapshots:
  vitest@4.1.11: {}
`,
					"schema/BUILD.bazel": `load("@rules_typescript//proto:defs.bzl", "ts_proto_config", "ts_proto_library")
# gazelle:proto_strip_import_prefix /schema
# gazelle:ts_proto plain
ts_proto_config(name = "plain", out_dir = "generated", tsconfig = "//app:tsconfig")
` + prefix + fmt.Sprintf("ts_proto_library(name = %q, proto = %q, out_dir = \"generated\", tsconfig = \"//app:tsconfig\")\n", wrapper, native),
					path.Join("schema", nativeDir, "message.proto"): `syntax = "proto3"; package example.message; message Message {}`,
					path.Join(generatedDir, "neighbor.ts"):          "export const neighbor = 1;\n",
					"app/BUILD.bazel":                               fmt.Sprintf(consumer, "@rules_typescript//ts/runners:vitest"),
					"app/package.json":                              `{"private":true,"devDependencies":{"vitest":"4.1.11"}}`,
					"app/tsconfig.json":                             `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.test.ts"]}`,
					"app/index.test.ts":                             fmt.Sprintf("export { value } from '../%s/message_pb.js';\n", generatedDir),
				}
				if nativeDir != "" {
					tree[path.Join("schema", nativeDir, "BUILD.bazel")] = ""
				}
				root := writeTree(t, tree)
				if output, err := protoGazelle(t, root); err != nil {
					t.Fatalf("source-capable consumer rejected fixed protobuf output: %v\n%s", err, output)
				}
				test := onDiskRule(t, root, "app", "ts_test", "app_test")
				wantLabels(t, "source-capable protobuf dependency", test.AttrStrings("deps"), []string{"//schema:" + wrapper, "@npm//app:vitest"})
				if test.Attr("emit") != nil || onDiskRule(t, root, "schema", "ts_proto_library", wrapper).Attr("emit") != nil {
					t.Fatal("source-capable consumer invented protobuf emission")
				}
				writeFile(t, filepath.Join(root, "app/BUILD.bazel"), fmt.Sprintf(consumer, "@rules_typescript//ts/runners:node_test"))
				before := buildFileBytes(t, root)
				for _, args := range [][]string{nil, {"-r=false", "app"}, {"-index=false", "-r=false", "app"}} {
					output, err := protoGazelle(t, root, args...)
					if err == nil || !strings.Contains(output, "requires emitted JavaScript from //schema:"+wrapper) || !strings.Contains(output, "ts_proto_library publishes TypeScript with fixed emit = False") {
						t.Fatalf("native consumer with %v accepted fixed protobuf source output: %v\n%s", args, err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("rejected protobuf publication changed BUILD files: %s", diff)
					}
				}
			})
		}
	}
}

func TestEmission_UnknownMembershipCannotCertifyRuntimePublication(t *testing.T) {
	for _, membership := range []struct {
		name, srcs, forwarding, diagnostic, emission string
		override                                     bool
	}{
		{"glob override", `glob(["*.ts"])`, "", "//lib has unsupported srcs membership", "", true},
		{"select override", `select({"//conditions:default": ["index.ts"]})`, "", "//lib has unsupported srcs membership", "", true},
		{"forwarded glob dependency", `[":sources"]`, `filegroup(name = "sources", srcs = glob(["*.ts"]))`, "//lib:sources has unsupported srcs membership", "", false},
		{"forwarded select override", `[":forward"]`, "alias(name = \"forward\", actual = \":sources\")\nfilegroup(name = \"sources\", srcs = select({\"//conditions:default\": [\"index.ts\"]}))", "//lib:sources has unsupported srcs membership", "", true},
		{"JavaScript dependency", `["index.mjs"]`, "", "", "", false},
		{"JSON dependency", `["value.json"]`, "", "", "", false},
		{"declaration dependency", `["index.d.ts"]`, "", "", "", false},
		{"empty dependency", `[]`, "", "", "", false},
		{"unknown emission with JavaScript", `["index.mjs"]`, "", "", `emit = select({"//conditions:default": False}),`, false},
		{"unknown emission with JSON", `["value.json"]`, "", "", `emit = select({"//conditions:default": False}),`, false},
		{"unknown emission with declarations", `["index.d.ts"]`, "", "", `emit = select({"//conditions:default": False}),`, false},
		{"unknown emission with empty sources", `[]`, "", "", `emit = select({"//conditions:default": False}),`, false},
	} {
		for _, indexed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/indexed=%t", membership.name, indexed), func(t *testing.T) {
				app := loadDefs + `"ts_binary", "ts_compile")
ts_binary(name = "run", entry_point = ":app")
ts_compile(name = "app", srcs = ["index.ts"], deps = ["//lib"], # keep
)
`
				index := "export const value = 42;\n"
				if membership.override {
					app = "# gazelle:resolve typescript lib/index.ts //lib:lib\n" + app
					index = "export { value } from '../lib/index.js';\n"
				}
				owner := loadDefs + `"ts_compile")` + "\n" + membership.forwarding + "\n# keep\n" +
					fmt.Sprintf("ts_compile(name = \"lib\", %s srcs = %s, visibility = [\"//visibility:public\"])\n", membership.emission, membership.srcs)
				root := writeTree(t, map[string]string{
					"MODULE.bazel":      "module(name = \"unknown_runtime_publication\")\n",
					"BUILD.bazel":       "",
					"app/BUILD.bazel":   app,
					"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
					"app/index.ts":      index,
					"lib/BUILD.bazel":   owner,
					"lib/index.ts":      "export const value = 42;\n",
					"lib/index.mjs":     "export const value = 42;\n",
					"lib/index.d.ts":    "export declare const value: number;\n",
					"lib/value.json":    `{"value":42}`,
				})
				args := []string{fmt.Sprintf("-index=%t", indexed), "-r=false", "app"}
				before := buildFileBytes(t, root)
				output, err := protoGazelle(t, root, args...)
				if membership.diagnostic != "" {
					if err == nil || !strings.Contains(output, "requires a runtime boundary from //lib") || !strings.Contains(output, membership.diagnostic) || !strings.Contains(output, "explicit source-file labels or declare emit = True") {
						t.Fatalf("unknown runtime membership was treated as empty: %v\n%s", err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("unknown membership changed BUILD files before refusal: %s", diff)
					}
					owner = strings.Replace(owner, `name = "lib",`, `name = "lib", emit = True,`, 1)
					writeFile(t, filepath.Join(root, "lib/BUILD.bazel"), owner)
					output, err = protoGazelle(t, root, args...)
				}
				if err != nil {
					t.Fatalf("known emitted boundary or non-TypeScript owner rejected: %v\n%s", err, output)
				}
				if got := buildFileText(t, root, "lib"); got != owner {
					t.Fatalf("manually owned sources changed: %s", lineDiff(owner, got))
				}
				wantLabels(t, "runtime dependency remains supplied by its owner", onDiskRule(t, root, "app", "ts_compile", "app").AttrStrings("deps"), []string{"//lib"})
			})
		}
	}
}

func TestEmission_UnknownRuntimeReferencesCannotPublishPartialUpdates(t *testing.T) {
	for _, reference := range []struct{ name, file, literal, expression, declaration, attribute string }{
		{"selected dependencies", "lib", `["//helper:helper"]`, `select({"//conditions:default": ["//helper:helper"]})`, "", "deps"},
		{"variable dependencies", "lib", `["//helper:helper"]`, `DEPENDENCIES`, `DEPENDENCIES = ["//helper:helper"]` + "\n", "deps"},
		{"variable dependency element", "lib", `["//helper:helper"]`, `[DEPENDENCY]`, `DEPENDENCY = "//helper:helper"` + "\n", "deps"},
		{"selected forwarding", "bridge", `"//lib:lib"`, `select({"//conditions:default": "//lib:lib"})`, "", "actual"},
		{"variable forwarding", "bridge", `"//lib:lib"`, `FORWARD`, `FORWARD = "//lib:lib"` + "\n", "actual"},
		{"selected entry", "app", `"//bridge:entry"`, `select({"//conditions:default": "//bridge:entry"})`, "", "entry_point"},
	} {
		for _, indexed := range []bool{false, true} {
			for _, consumer := range []string{"updated", "observed", "kept"} {
				t.Run(fmt.Sprintf("%s/indexed=%t/%s", reference.name, indexed, consumer), func(t *testing.T) {
					binary := loadDefs + `"ts_binary")` + "\n"
					if consumer == "kept" {
						binary += "# keep\n"
					}
					binary += `ts_binary(name = "run", entry_point = "//bridge:entry")` + "\n"
					files := map[string]string{
						"MODULE.bazel":       "module(name = \"runtime_reference_publication\")\n",
						"BUILD.bazel":        "",
						"app/BUILD.bazel":    binary,
						"app/tsconfig.json":  `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
						"app/index.ts":       "export const unrelated = 1;\n",
						"bridge/BUILD.bazel": `alias(name = "entry", actual = "//lib:lib", visibility = ["//visibility:public"])` + "\n",
						"lib/BUILD.bazel": loadDefs + `"ts_compile")
ts_compile(name = "lib", emit = True, srcs = ["index.mjs", "index.d.mts"], deps = ["//helper:helper"], visibility = ["//visibility:public"])
`,
						"lib/index.mjs":   "export { value } from '../helper/value.js';\n",
						"lib/index.d.mts": "export declare const value: number;\n",
						"helper/BUILD.bazel": loadDefs + `"ts_compile")
ts_compile(name = "helper", srcs = ["value.ts"], emit = True, visibility = ["//visibility:public"])
`,
						"helper/value.ts": "export const value = 42;\n",
					}
					changed := reference.file + "/BUILD.bazel"
					original := files[changed]
					files[changed] = reference.declaration + strings.Replace(original, reference.literal, reference.expression, 1)
					if consumer == "observed" {
						files["BUILD.bazel"], files["app/BUILD.bazel"] = files["app/BUILD.bazel"], ""
						if reference.file == "app" {
							changed = "BUILD.bazel"
						}
					}
					root := writeTree(t, files)
					args := []string{fmt.Sprintf("-index=%t", indexed), "-r=false", "app"}
					before := buildFileBytes(t, root)
					output, err := protoGazelle(t, root, args...)
					if consumer == "updated" {
						if err == nil || !strings.Contains(output, "requires the runtime closure of") || !strings.Contains(output, "its "+reference.attribute+" is an expression") {
							t.Fatalf("unknown runtime reference certified publication: %v\n%s", err, output)
						}
						if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("unknown runtime reference changed BUILD files before refusal: %s", diff)
						}
						writeFile(t, filepath.Join(root, changed), original)
						output, err = protoGazelle(t, root, args...)
					}
					if err != nil {
						t.Fatalf("literal, observed or manually owned closure was rejected: %v\n%s", err, output)
					}
					for _, pkg := range []string{"bridge", "lib", "helper"} {
						want := files[pkg+"/BUILD.bazel"]
						if consumer == "updated" && reference.file == pkg {
							want = original
						}
						if got := buildFileText(t, root, pkg); got != want {
							t.Fatalf("untouched %s changed: %s", pkg, lineDiff(want, got))
						}
					}
					if strings.Contains(buildFileText(t, root, "app"), "emit = True") {
						t.Fatal("direct runtime demand promoted the unrelated app program")
					}
				})
			}
		}
	}
}

func TestEmission_GeneratorToolsCannotBecomeOutputRuntimeDependencies(t *testing.T) {
	for _, identity := range []struct{ name, generator, compiler, generatorDirectives, compilerDirectives, supplier string }{
		{"canonical", "ts_codegen", "ts_compile", "", "", "//lib:runtime_owner"},
		{"aliased", "custom_codegen", "custom_compile", "# gazelle:alias_kind custom_codegen ts_codegen\n", "# gazelle:alias_kind custom_compile ts_compile\n", "//bridge:runtime"},
		{"mapped", "custom_codegen", "custom_compile", "# gazelle:map_kind ts_codegen custom_codegen //:defs.bzl\n", "# gazelle:map_kind ts_compile custom_compile //:defs.bzl\n", "//bridge:runtime"},
	} {
		for _, toolDeps := range []string{`["//tools:node_modules/@acme/tool"]`, `select({"//conditions:default": ["//tools:node_modules/@acme/tool"]})`} {
			for _, update := range []struct {
				name string
				args []string
			}{
				{"full", nil},
				{"indexed partial", []string{"-r=false", "app"}},
				{"unindexed partial", []string{"-index=false", "-r=false", "app"}},
			} {
				t.Run(identity.name+"/"+toolDeps+"/"+update.name, func(t *testing.T) {
					generator := "//generated:generate"
					if identity.name != "canonical" {
						generator = "//bridge:generator"
					}
					files := map[string]string{
						"MODULE.bazel":               "module(name = \"generator_edge_roles\")\n",
						"BUILD.bazel":                "exports_files([\"package.json\", \"defs.bzl\"], visibility = [\"//visibility:public\"])\n",
						"defs.bzl":                   "load(\"@rules_typescript//ts:defs.bzl\", _compile = \"ts_compile\", _codegen = \"ts_codegen\")\nts_compile = _compile\nts_codegen = _codegen\ncustom_compile = _compile\ncustom_codegen = _codegen\n",
						"package.json":               `{"private":true}`,
						"node_modules/.modules.yaml": "layoutVersion: 5\n",
						"pnpm-lock.yaml": `lockfileVersion: '9.0'
importers:
  .: {}
  tools:
    dependencies:
      '@acme/tool':
        specifier: workspace:*
        version: link:../tool
  tool: {}
`,
						"app/BUILD.bazel": loadDefs + `"ts_test")
ts_test(
    name = "app_test",
    srcs = ["index.test.ts"],
    runner = "//bridge:runner",
    deps = ["` + identity.supplier + `"], # keep
)
`,
						"app/tsconfig.json": `{"compilerOptions":{"types":[]},"files":["index.test.ts"]}`,
						"app/index.test.ts": "export {};\n",
						"lib/BUILD.bazel": identity.compilerDirectives + "load(\"//:defs.bzl\", \"" + identity.compiler + "\")\n# keep\n" + identity.compiler + `(
    name = "runtime_owner",
    srcs = ["index.mjs", "index.d.mts"],
    deps = ["` + generator + `", "//runtime:runtime"],
    visibility = ["//visibility:public"],
)
`,
						"lib/index.mjs":   "export const value = 42;\n",
						"lib/index.d.mts": "export declare const value: number;\n",
						"generated/BUILD.bazel": identity.generatorDirectives + "load(\"//:defs.bzl\", \"" + identity.generator + "\")\n" + identity.generator + `(
    name = "generate",
    srcs = ["input.json"],
    outs = ["generated.mjs"],
    generator = "//tools:generate",
    args = ["{out}"],
    node_modules = "//tools:node_modules",
    deps = ` + toolDeps + `,
    visibility = ["//visibility:public"],
)
`,
						"generated/input.json": "{}\n",
						"bridge/BUILD.bazel": `alias(name = "runtime", actual = "//lib:runtime_owner", visibility = ["//visibility:public"])
alias(name = "generator", actual = "//generated:generate", visibility = ["//visibility:public"])
alias(name = "runner", actual = "@rules_typescript//ts/runners:node_test", visibility = ["//visibility:public"])
`,
						"tools/BUILD.bazel": loadDefs + `"node_modules", "node_modules_member")
load("@rules_shell//shell:sh_binary.bzl", "sh_binary")
node_modules(name = "node_modules", parent = "//:node_modules", visibility = ["//visibility:public"])
node_modules_member(name = "node_modules/@acme/tool", member = "@npm//:acme_tool", visibility = ["//visibility:public"])
sh_binary(name = "generate", srcs = ["generate.sh"], visibility = ["//visibility:public"])
`,
						"tools/generate.sh": "#!/bin/sh\nprintf 'export const generated = 1;\\n' > \"$1\"\n",
						"runtime/BUILD.bazel": loadDefs + `"ts_compile")
# keep
ts_compile(name = "runtime", emit = True, srcs = ["value.ts"], visibility = ["//visibility:public"])
`,
						"runtime/value.ts": "export const value = 42;\n",
						"tool/BUILD.bazel": loadDefs + `"ts_compile")
ts_compile(name = "tool", emit = False, # keep
    srcs = ["index.ts", "package.json"], visibility = ["//visibility:public"])
`,
						"tool/package.json":  `{"name":"@acme/tool","exports":"./index.ts"}`,
						"tool/tsconfig.json": `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
						"tool/index.ts":      "export const tool: number = 1;\n",
					}
					for name, contents := range files {
						if filepath.Base(name) != "BUILD.bazel" {
							continue
						}
						file, err := rule.LoadData(name, filepath.ToSlash(filepath.Dir(name)), []byte(contents))
						if err != nil {
							t.Fatal(err)
						}
						files[name] = string(file.Format())
					}
					root := writeTree(t, files)
					before := buildFileBytes(t, root)
					if output, err := protoGazelle(t, root, update.args...); err != nil {
						t.Fatalf("generator tool edges became output runtime demands: %v\n%s", err, output)
					}
					if got := onDiskRule(t, root, "tool", "ts_compile", "tool"); ruleEmission(got) != sourceEmission {
						t.Fatal("output consumer promoted a generator-only tool")
					}
					published := buildFileBytes(t, root)
					if len(update.args) > 0 {
						before["app/BUILD.bazel"] = published["app/BUILD.bazel"]
						if diff := snapshotDiff(before, published); diff != "" {
							t.Fatalf("partial output publication changed unrelated BUILD files: %s", diff)
						}
					}
					if output, err := protoGazelle(t, root, update.args...); err != nil {
						t.Fatalf("generator output rerun: %v\n%s", err, output)
					}
					if diff := snapshotDiff(published, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("generator output publication did not converge: %s", diff)
					}
					owner := buildFileText(t, root, "runtime")
					writeFile(t, filepath.Join(root, "runtime/BUILD.bazel"), strings.Replace(owner, "emit = True", "emit = False", 1))
					before = buildFileBytes(t, root)
					output, err := protoGazelle(t, root, update.args...)
					if err == nil || !strings.Contains(output, "requires emission from //runtime for runtime TypeScript input runtime/value.ts") {
						t.Fatalf("a real compiler dependency lost its runtime demand: %v\n%s", err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("runtime dependency rejection changed BUILD files: %s", diff)
					}
				})
			}
		}
	}
}

func TestEmission_DeclarationBridgeCannotHideUnpublishedTypeScript(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, remedy := range []string{"include owner", "declared emission"} {
			t.Run(fmt.Sprintf("indexed=%t/%s", indexed, remedy), func(t *testing.T) {
				files := map[string]string{
					"MODULE.bazel": "module(name = \"transitive_publication\")\n",
					"app/BUILD.bazel": loadDefs + `"ts_binary")
ts_binary(name = "run", entry_point = ":app")
`,
					"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
					"app/index.ts":      "export { value } from '../lib/index.mjs';\n",
					"lib/BUILD.bazel": loadDefs + `"ts_compile")
ts_compile(name = "lib", srcs = ["index.mjs", "index.d.mts"], deps = ["//helper:forward"], visibility = ["//visibility:public"])
`,
					"lib/index.mjs":   "export { value } from '../helper/value.js';\n",
					"lib/index.d.mts": "export declare const value: number;\n",
					"helper/BUILD.bazel": loadDefs + `"ts_compile")
alias(name = "forward", actual = ":helper", visibility = ["//visibility:public"])
filegroup(name = "sources", srcs = ["value.ts"])
ts_compile(name = "helper", srcs = [":sources"], visibility = ["//visibility:public"])
`,
					"helper/tsconfig.json": `{"files":["value.ts"]}`,
					"helper/value.ts":      "export const value = 42;\n",
				}
				root := writeTree(t, files)
				args := []string{fmt.Sprintf("-index=%t", indexed), "-r=false", "app"}
				before := buildFileBytes(t, root)
				for range 2 {
					output, err := protoGazelle(t, root, args...)
					if err == nil || !strings.Contains(output, "//app:run requires emission from //helper for runtime TypeScript input helper/value.ts") {
						t.Fatalf("declaration bridge concealed unpublished source owner: %v\n%s", err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("refused transitive publication changed BUILD files: %s", diff)
					}
				}
				if remedy == "include owner" {
					args = append(args, "helper")
				} else {
					files["helper/BUILD.bazel"] = strings.Replace(files["helper/BUILD.bazel"], `name = "helper",`, `name = "helper", emit = True,`, 1)
					writeFile(t, filepath.Join(root, "helper/BUILD.bazel"), files["helper/BUILD.bazel"])
				}
				var first map[string]string
				for range 2 {
					if output, err := protoGazelle(t, root, args...); err != nil {
						t.Fatalf("published transitive owner rejected: %v\n%s", err, output)
					}
					for _, pkg := range []string{"app", "helper"} {
						if !strings.Contains(buildFileText(t, root, pkg), "emit = True") {
							t.Fatalf("%s did not publish required emission", pkg)
						}
					}
					if got := buildFileText(t, root, "lib"); got != files["lib/BUILD.bazel"] {
						t.Fatalf("untouched JavaScript owner changed: %s", lineDiff(files["lib/BUILD.bazel"], got))
					}
					if remedy == "declared emission" && buildFileText(t, root, "helper") != files["helper/BUILD.bazel"] {
						t.Fatal("untouched emitted owner changed")
					}
					if first == nil {
						first = buildFileBytes(t, root)
					} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("transitive publication did not converge: %s", diff)
					}
				}
			})
		}
	}
}

func TestEmission_GeneratedWorkspaceLinkReachesSourceMember(t *testing.T) {
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		"package.json":               convergePlainPkg,
		"pnpm-lock.yaml":             convergeMemberLock,
		"pnpm-workspace.yaml":        "packages:\n  - packages/*\n",
		"node_modules/.modules.yaml": "hoistPattern:\n  - '*'\n",
		"BUILD.bazel": `load("@rules_typescript//ts:defs.bzl", "ts_binary")
exports_files(["package.json"], visibility = ["//packages/app:__pkg__"])
ts_binary(name="run",entry_point="//packages/app")`,
		"packages/core/package.json":  `{"name":"@w/core","version":"1.0.0","exports":"./index.ts"}`,
		"packages/core/tsconfig.json": `{"include":["*.ts"]}`,
		"packages/core/index.ts":      `export const value = 1;`,
		"packages/app/tsconfig.json":  `{"compilerOptions":{"module":"esnext","moduleResolution":"bundler"},"include":["*.ts"]}`,
		"packages/app/index.ts":       `import { value } from "@w/core"; export const result = value;`,
	})
	if err := os.MkdirAll(filepath.Join(root, "node_modules/@w"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "packages/core"), filepath.Join(root, "node_modules/@w/core")); err != nil {
		t.Fatal(err)
	}
	captureLog(t, func() { convergeGazelle(t, root) })
	for _, pkg := range []string{"packages/app", "packages/core"} {
		text := buildFileText(t, root, pkg)
		if !strings.Contains(text, "emit = True") {
			t.Fatalf("workspace boundary %s not emitted:\n%s", pkg, text)
		}
	}
}

func TestEmission_UnindexedMemberForwardingCannotHideUnpublishedTypeScript(t *testing.T) {
	for _, boundary := range []string{"retained dependency", "declaration bridge"} {
		for _, remedy := range []string{"declared emission", "full update"} {
			t.Run(boundary+"/"+remedy, func(t *testing.T) {
				files := map[string]string{
					"MODULE.bazel": "module(name = \"member_publication\")\n",
					"BUILD.bazel": loadDefs + `"node_modules", "node_modules_member")
exports_files(["package.json"], visibility = ["//visibility:public"])
node_modules(name = "node_modules", visibility = ["//visibility:public"])
node_modules_member(name = "node_modules/member", member = "@npm//:member", visibility = ["//visibility:public"])
`,
					"package.json":               `{"private":true}`,
					"node_modules/.modules.yaml": "layoutVersion: 5\n",
					"pnpm-lock.yaml": `lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      member:
        specifier: workspace:*
        version: link:packages/member
  app: {}
  lib: {}
  packages/member: {}
`,
					"app/BUILD.bazel": loadDefs + `"ts_binary", "ts_compile")
ts_binary(name = "run", entry_point = ":app")
ts_compile(name = "app", srcs = ["index.ts", "package.json"], deps = ["//:node_modules/member"], # keep
)
`,
					"app/package.json":  `{"type":"module"}`,
					"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
					"app/index.ts":      "export const value = 42;\n",
					"packages/member/BUILD.bazel": loadDefs + `"ts_compile")
ts_compile(name = "member", srcs = ["index.ts", "package.json"], emit = False, visibility = ["//visibility:public"])
`,
					"packages/member/package.json":  `{"name":"member","type":"module","exports":"./index.ts"}`,
					"packages/member/tsconfig.json": `{"compilerOptions":{"types":[]},"files":["index.ts"]}`,
					"packages/member/index.ts":      "export const value = 42;\n",
				}
				if boundary == "declaration bridge" {
					files["app/BUILD.bazel"] = loadDefs + `"ts_binary")
ts_binary(name = "run", entry_point = ":app")
`
					files["app/index.ts"] = "export { value } from '../lib/index.mjs';\n"
					files["lib/BUILD.bazel"] = loadDefs + `"ts_compile")
ts_compile(name = "lib", srcs = ["index.mjs", "index.d.mts"], deps = ["//:node_modules/member"], # keep
    visibility = ["//visibility:public"],
)
`
					files["lib/tsconfig.json"] = `{"compilerOptions":{"allowJs":true,"types":[]},"files":["index.mjs","index.d.mts"]}`
					files["lib/index.mjs"] = "export { value } from 'member';\n"
					files["lib/index.d.mts"] = "export declare const value: number;\n"
				}
				root := writeTree(t, files)
				args := []string{"-index=false", "-r=false", "app"}
				before := buildFileBytes(t, root)
				output, err := protoGazelle(t, root, args...)
				if err == nil || !strings.Contains(output, "//app:run requires emission from //packages/member for runtime TypeScript input packages/member/index.ts") {
					t.Fatalf("unobserved member certified partial runtime publication: %v\n%s", err, output)
				}
				if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("refused member publication changed BUILD files: %s", diff)
				}
				if remedy == "declared emission" {
					files["packages/member/BUILD.bazel"] = strings.Replace(files["packages/member/BUILD.bazel"], "emit = False", "emit = True", 1)
					writeFile(t, filepath.Join(root, "packages/member/BUILD.bazel"), files["packages/member/BUILD.bazel"])
				} else {
					args = []string{"-index=false"}
				}
				if output, err := protoGazelle(t, root, args...); err != nil {
					t.Fatalf("published member rejected: %v\n%s", err, output)
				}
				for _, pkg := range []string{"app", "packages/member"} {
					if !strings.Contains(buildFileText(t, root, pkg), "emit = True") {
						t.Fatalf("%s did not publish required emission", pkg)
					}
				}
				if remedy == "declared emission" {
					for _, pkg := range []string{"", "lib", "packages/member"} {
						file := filepath.ToSlash(filepath.Join(pkg, "BUILD.bazel"))
						if original, exists := files[file]; exists && buildFileText(t, root, pkg) != original {
							t.Fatalf("partial member publication changed untouched %s", file)
						}
					}
				}
			})
		}
	}
}

func TestEmission_NodeRunnerAliasOptsInWithoutASecondRun(t *testing.T) {
	for _, mapping := range []struct {
		name, kind, load, directive, module, runner string
		emit                                        bool
	}{
		{name: "ts_test", kind: "ts_test", load: "@rules_typescript//ts:defs.bzl", emit: true},
		{name: "custom_test", kind: "custom_test", load: "//:defs.bzl", directive: "# gazelle:map_kind ts_test custom_test //:defs.bzl\n", emit: true},
		{name: "local same path", kind: "ts_test", load: "@rules_typescript//ts:defs.bzl", runner: "//ts/runners:node_test"},
		{name: "other repo same path", kind: "ts_test", load: "@rules_typescript//ts:defs.bzl", runner: "@other//ts/runners:node_test"},
		{name: "canonical other repo same path", kind: "ts_test", load: "@rules_typescript//ts:defs.bzl", runner: "@@other//ts/runners:node_test"},
		{name: "renamed dependency", kind: "ts_test", load: "@typescript//ts:defs.bzl", module: "module(name = \"runner_alias\")\nbazel_dep(name = \"rules_typescript\", version = \"0.0.0\", repo_name = \"typescript\")\n", runner: "@typescript//ts/runners:node_test", emit: true},
		{name: "self module", kind: "ts_test", load: "//ts:defs.bzl", module: "module(name = \"rules_typescript\", repo_name = \"typescript\")\n", runner: "//ts/runners:node_test", emit: true},
		{name: "renamed self module", kind: "ts_test", load: "@typescript//ts:defs.bzl", module: "module(name = \"rules_typescript\", repo_name = \"typescript\")\n", runner: "@typescript//ts/runners:node_test", emit: true},
	} {
		module := mapping.module
		if module == "" {
			module = "module(name = \"runner_alias\")\n"
		}
		runner := mapping.runner
		if runner == "" {
			runner = "@rules_typescript//ts/runners:node_test"
		}
		for _, update := range []struct {
			name string
			args []string
		}{
			{"full", nil},
			{"indexed partial", []string{"-r=false", "app"}},
			{"unindexed partial", []string{"-index=false", "-r=false", "app"}},
		} {
			t.Run(mapping.name+"/"+update.name, func(t *testing.T) {
				root := writeTree(t, map[string]string{
					"MODULE.bazel":      module,
					"BUILD.bazel":       mapping.directive,
					"defs.bzl":          "load(\"@rules_typescript//ts:defs.bzl\", \"ts_test\")\ncustom_test = ts_test\n",
					"app/tsconfig.json": `{"files":["check.test.ts"]}`,
					"app/check.test.ts": "export const value = 1;\n",
					"app/BUILD.bazel": "load(\"" + mapping.load + "\", \"" + mapping.kind + "\")\n" + mapping.kind + `(name="app_test",srcs=["check.test.ts"],runner=":node_runner")
alias(name="node_runner",actual="//tools:node_runner")
`,
					"tools/BUILD.bazel": fmt.Sprintf("alias(name=\"node_runner\",actual=%q,visibility=[\"//visibility:public\"])\n", runner),
				})
				if mapping.name == "local same path" {
					writeFile(t, filepath.Join(root, "ts/runners/BUILD.bazel"), "load(\"@rules_typescript//ts/private/rules:runners.bzl\", \"vitest_runner\")\nvitest_runner(name = \"node_test\")\n")
				}
				var first map[string]string
				for range 2 {
					if output, err := protoGazelle(t, root, update.args...); err != nil {
						t.Fatalf("runner alias update: %v\n%s", err, output)
					}
					if emitted := strings.Contains(buildFileText(t, root, "app"), "emit = True"); emitted != mapping.emit {
						t.Fatalf("runner %s: emitted=%t, want %t", runner, emitted, mapping.emit)
					}
					if first == nil {
						first = buildFileBytes(t, root)
					} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("runner alias needed another update: %s", diff)
					}
				}
			})
		}
	}
}

func TestEmission_ManifestOutputsFollowNestedProgramOwnership(t *testing.T) {
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		"BUILD.bazel":                   `exports_files(["package.json"], visibility = ["//src:__pkg__", "//types:__pkg__", "//features:__subpackages__", "//js:__pkg__", "//esm:__pkg__", "//native:__pkg__"])`,
		"package.json":                  `{"main":"./src/index.js","exports":{"./types":"./types/index.d.ts","./features/*":"./features/*.js","./js":{"types":"./js/index.d.ts"},"./esm":{"types":"./esm/index.d.mts"}}}`,
		"src/tsconfig.json":             `{"include":["*.ts"]}`,
		"src/index.ts":                  `export const value = 1;`,
		"types/tsconfig.json":           `{"include":["*.ts"]}`,
		"types/index.ts":                `export type Value = number;`,
		"features/tsconfig.json":        `{"include":["*.ts"]}`,
		"js/tsconfig.json":              `{"compilerOptions":{"allowJs":true},"include":["*.js"]}`,
		"js/index.js":                   `export const value = 1;`,
		"esm/tsconfig.json":             `{"compilerOptions":{"allowJs":true},"include":["*.mjs"]}`,
		"esm/index.mjs":                 `export const value = 1;`,
		"features/nested/tsconfig.json": `{"include":["*.ts"]}`,
		"features/nested/index.ts":      `export const nested = 1;`,
		"features/feature.ts":           `export const feature = 1;`,
		"native/tsconfig.json":          `{"include":["*.ts"]}`,
		"native/index.ts":               `export const untouched = 1;`,
	})
	captureLog(t, func() { convergeGazelle(t, root) })
	for _, pkg := range []string{"src", "types", "features", "features/nested", "js", "esm", "native"} {
		body := buildFileText(t, root, pkg)
		if strings.Contains(body, "emit = True") != (pkg != "native") {
			t.Fatalf("%s manifest output ownership wrong:\n%s", pkg, body)
		}
	}
	wantLabels(t, "unrooted compiler retains its ancestor scope", onDiskRule(t, root, "native", "ts_compile", "native").AttrStrings("package_scopes"), []string{"//:package.json"})
}

func TestEmission_ExportPatternsPreserveNestedAndRepeatedSubpaths(t *testing.T) {
	for _, tc := range []struct {
		pattern, file string
		want          bool
	}{
		{"features/*.js", "features/nested/index.js", true},
		{"features/*/copy/*.js", "features/nested/index/copy/nested/index.js", true},
		{"features/*/copy/*.js", "features/a/copy/b.js", false},
		{"features/*.js", "x.js", false},
	} {
		if got := matchesExportOutput(tc.pattern, tc.file); got != tc.want {
			t.Fatalf("%s matches %s = %v, want %v", tc.pattern, tc.file, got, tc.want)
		}
	}
}

func TestEmission_WorkersPoolDoesNotForceSourceDependenciesToEmit(t *testing.T) {
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		"package.json":         convergePlainPkg,
		"worker/tsconfig.json": `{"compilerOptions":{"module":"esnext","moduleResolution":"bundler"},"include":["*.ts"]}`,
		"worker/check.test.ts": `import {value} from "../lib/index"; export const result = value;`,
		"worker/wrangler.json": `{"main":"../lib/index.ts"}`,
		"worker/BUILD.bazel": `load("@rules_typescript//ts:defs.bzl", "ts_test")
ts_test(
    name = "worker_test",
    srcs = ["check.test.ts"],
    wrangler_config = "wrangler.json", # keep
)
`,
		"lib/tsconfig.json": `{"include":["*.ts"]}`,
		"lib/index.ts":      `export const value = 1;`,
	})
	captureLog(t, func() { convergeGazelle(t, root) })
	for _, pkg := range []string{"worker", "lib"} {
		body := buildFileText(t, root, pkg)
		if strings.Contains(body, "emit = True") {
			t.Fatalf("%s Workers pool forced source emission:\n%s", pkg, body)
		}
	}
}
