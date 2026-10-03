package main

import (
	"github.com/mikn/rules_typescript/tests/integration/harness"
	"path/filepath"
	"strings"
)

func main() {
	harness.Run(harness.Config{Name: "proto_generated", WorkspaceRel: "tests/integration/proto_generated"}, func(it *harness.IT) {
		it.Write(it.Path("testdeps/pnpm-lock.yaml"), it.Read(filepath.Join(it.RulesTSRoot, "tests/npm/pnpm-lock.yaml")))
		it.Write(it.Path("pnpm-lock.yaml"), it.Read(filepath.Join(it.RulesTSRoot, "proto/private/pnpm-lock.yaml")))
		it.Write(it.Path("testdeps/BUILD.bazel"), `load("@npm_test//:defs.bzl", "npm_virtual_store")
npm_virtual_store(name = "node_modules/.pnpm")
exports_files(["pnpm-lock.yaml"])`)
		it.Write(it.Path("schema/BUILD.bazel"), `load("@rules_typescript//proto:defs.bzl", "ts_proto_config")
# gazelle:proto_strip_import_prefix /schema
# gazelle:ts_proto plain json
ts_proto_config(
    name = "plain",
    out_dir = "generated/plain",
    tsconfig = "//:compiler.json",
    node_modules = "//:node_modules",
    deps = ["@npm//:bufbuild_protobuf"],
)
ts_proto_config(
    name = "json",
    out_dir = "generated/json",
    tsconfig = "//:compiler.json",
    node_modules = "//:node_modules",
    deps = ["@npm//:bufbuild_protobuf"],
    options = ["target=ts", "json_types=true"],
)
package(default_visibility = ["//visibility:public"])
`)
		it.Write(it.Path("schema/common/common.proto"), `syntax = "proto3"; package example.common; message Common { string value = 1; }`)
		it.Write(it.Path("schema/messages/message.proto"), `syntax = "proto3"; package example.messages; import "common/common.proto"; import "google/protobuf/timestamp.proto"; message Message { example.common.Common common = 1; google.protobuf.Timestamp at = 2; }`)

		it.Write(it.Path("MODULE.bazel"), it.Read(it.Path("MODULE.bazel"))+`
bazel_dep(name = "external_a", version = "1.0", repo_name = "renamed")
local_path_override(module_name = "external_a", path = "external_a")
local_path_override(module_name = "external_b", path = "external_b")
`)
		it.Write(it.Path("external_a/MODULE.bazel"), `module(name="external_a",version="1.0")
bazel_dep(name="external_b",version="1.0",repo_name="sibling")
bazel_dep(name="protobuf",version="36.1.bcr.1")`)
		it.Write(it.Path("external_b/MODULE.bazel"), `module(name="external_b",version="1.0")
bazel_dep(name="protobuf",version="36.1.bcr.1")`)
		it.Write(it.Path("external_a/api/BUILD.bazel"), `load("@protobuf//bazel:proto_library.bzl","proto_library")
package(default_visibility=["//visibility:public"])
alias(name="a_alias",actual=":a_forward")
proto_library(name="a_forward",deps=[":left",":right"])
proto_library(name="left",deps=[":a_proto"])
proto_library(name="right",deps=[":a_proto"])
config_setting(name="source_info",flag_values={"@protobuf//bazel/flags:experimental_proto_descriptor_sets_include_source_info":"true"})
proto_library(name="a_proto",srcs=["a.proto"],strip_import_prefix="/api",import_prefix="foreign",deps=select({":source_info":["@sibling//schema:b_proto"],"//conditions:default":[":unused_proto"]}))
proto_library(name="unused_proto",srcs=["b.proto"],strip_import_prefix="/api",import_prefix="foreign")`)
		it.Write(it.Path("external_b/schema/BUILD.bazel"), `load("@protobuf//bazel:proto_library.bzl","proto_library")
proto_library(name="b_proto",srcs=["b.proto"],strip_import_prefix="/schema",import_prefix="foreign",visibility=["//visibility:public"])`)
		it.Write(it.Path("external_a/api/a.proto"), `syntax="proto3"; package foreign; import "foreign/b.proto"; message External { Sibling sibling = 1; }`)
		it.Write(it.Path("external_a/api/b.proto"), `syntax="proto3"; package foreign; message Unused { string value = 1; }`)
		it.Write(it.Path("external_b/schema/b.proto"), `syntax="proto3"; package foreign; message Sibling { string value = 1; }`)
		it.Write(it.Path("schema/messages/message.proto"), `syntax = "proto3"; package example.messages; import "common/common.proto"; import "google/protobuf/timestamp.proto"; import "foreign/a.proto"; message Message { example.common.Common common = 1; google.protobuf.Timestamp at = 2; foreign.External external = 3; }`)
		it.Write(it.Path("schema/BUILD.bazel"), it.Read(it.Path("schema/BUILD.bazel"))+"\n# gazelle:resolve proto foreign/a.proto @renamed//api:a_alias\n# gazelle:resolve proto foreign/b.proto @renamed//api:unused_proto\n")
		it.Write(it.Path("package.json"), it.Read(filepath.Join(it.RulesTSRoot, "proto/private/package.json")))
		it.Write(it.Path("BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_pnpm")
`+it.Read(it.Path("BUILD.bazel"))+"\nts_pnpm(name = \"pnpm\")\nexports_files([\"package.json\"])\n")
		it.Install()
		it.MustBazel("run", "//:gazelle", "--", "schema")
		before := it.Read(it.Path("schema/BUILD.bazel"))
		configs := strings.Fields(it.BazelStdout("query", `kind("ts_proto_config rule", //schema:all)`))
		if len(configs) != 2 {
			it.Fail("expected two queryable typed configurations, got %v", configs)
		}
		it.MustBazel("build", "//schema:plain", "//schema:json")
		if files := strings.TrimSpace(it.BazelStdout("cquery", "set(//schema:plain //schema:json)", "--output=files")); files != "" {
			it.Fail("configuration targets declared output files: %s", files)
		}
		if strings.Contains(before, "/unused_proto\"") {
			it.Fail("available external root was emitted without a selected import")
		}
		if !strings.Contains(before, "b_proto") {
			it.Fail("reachable external sibling was omitted")
		}
		it.MustBazel("test", "//consumer:generated_identity_test")
		it.MustBazel("run", "//:gazelle", "--", "schema")
		if after := it.Read(it.Path("schema/BUILD.bazel")); before != after {
			it.Fail("runtime generation changed owner BUILD")
		}
		badOverride := strings.Replace(before, "foreign/a.proto @renamed//api:a_alias", "foreign/a.proto @unknown//api:a_alias", 1)
		it.Write(it.Path("schema/BUILD.bazel"), badOverride)
		log, err := it.BazelLog("unknown-proto-override.log", "run", "//:gazelle", "--", "schema")
		if err == nil || !log.Contains("is not a declared graph root") {
			log.Dump()
			it.Fail("unknown apparent override did not fail at graph boundary")
		}
		if it.Read(it.Path("schema/BUILD.bazel")) != badOverride {
			it.Fail("failed external resolution changed the owner BUILD")
		}
		it.Write(it.Path("schema/BUILD.bazel"), before)
		original := it.Read(it.Path("schema/messages/message.proto"))
		withoutExternal := strings.ReplaceAll(strings.ReplaceAll(original, `import "foreign/a.proto";`, ""), "foreign.External external = 3;", "")
		it.Write(it.Path("schema/messages/message.proto"), withoutExternal)
		it.MustBazel("run", "//:gazelle", "--", "schema")
		if strings.Contains(it.Read(it.Path("schema/BUILD.bazel")), "_external/") {
			it.Fail("removed import left stale external wrappers")
		}
		it.Write(it.Path("schema/messages/message.proto"), original)
		it.MustBazel("run", "//:gazelle", "--", "schema")
		if it.Read(it.Path("schema/BUILD.bazel")) != before {
			it.Fail("restoring external import changed generated owner identity")
		}
		for _, invalid := range []struct{ name, attrs, diagnostic string }{
			{"missing_label", `tsconfig = "//:absent_config"`, "absent_config"},
			{"javascript", `tsconfig = "//:compiler.json", options = ["target=js"]`, "must select target=ts"},
			{"empty_options", `tsconfig = "//:compiler.json", options = []`, "must select target=ts"},
		} {
			it.Write(it.Path("schema/BUILD.bazel"), before+`
ts_proto_config(name = "invalid", out_dir = "invalid", `+invalid.attrs+`)
`)
			log, err := it.BazelLog(invalid.name+".log", "build", "//schema:invalid")
			if err == nil || !log.Contains(invalid.diagnostic) {
				it.Fail("configuration did not reject %s: %v\n%s", invalid.name, err, log.Text)
			}
			it.Write(it.Path("schema/BUILD.bazel"), before)
		}
		it.Write(it.Path("settings/tsconfig.json"), `{"compilerOptions":{"module":"ESNext","moduleResolution":"Bundler","target":"ES2022","strict":true,"types":["./ambient"]},"include":["contract.ts"]}`)
		it.Write(it.Path("settings/ambient.d.ts"), "declare const generatedAmbient: unique symbol;\n")
		it.Write(it.Path("settings/contract.ts"), "export interface GeneratedContext { token: typeof generatedAmbient }\n")
		it.MustBazel("run", "//:gazelle", "--", "settings")
		it.Write(it.Path("schema/BUILD.bazel"), strings.ReplaceAll(before, `tsconfig = "//:compiler.json"`, `tsconfig = "//settings:tsconfig"`))
		it.MustBazel("run", "//:gazelle", "--", "schema")
		ambient := it.Read(it.Path("schema/BUILD.bazel"))
		if !strings.Contains(ambient, `"//settings"`) {
			it.Fail("selected compiler ambient declaration has no generated dependency")
		}
		it.MustBazel("build", "//schema:plain/messages/example_messages_proto", "//schema:json/messages/example_messages_proto", "--output_groups=+_validation")
		it.Write(it.Path("schema/BUILD.bazel"), strings.ReplaceAll(ambient, `"//settings",`, ""))
		missingAmbient, err := it.BazelLog("missing-ambient-dependency.log", "build", "//schema:plain/messages/example_messages_proto", "--output_groups=+_validation")
		if err == nil || !missingAmbient.Contains("compilerOptions.types entry") {
			missingAmbient.Dump()
			it.Fail("removing the ambient owner did not reject the missing declared input")
		}
		it.Write(it.Path("schema/BUILD.bazel"), ambient)
		it.MustBazel("test", "//consumer:generated_identity_test")
		it.MustBazel("run", "//:gazelle", "--", "schema")
		if after := it.Read(it.Path("schema/BUILD.bazel")); after != ambient {
			it.Fail("ambient dependency generation changed on repeat")
		}
		it.Pass("ordinary generated wrappers compile and execute both output identities")
	})
}
