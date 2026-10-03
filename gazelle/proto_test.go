package typescript

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/label"
	gazelleproto "github.com/bazelbuild/bazel-gazelle/language/proto"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

func protoGazelle(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	binary, err := runfiles.Rlocation(os.Getenv("PROTO_GAZELLE"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, append([]string{"-repo_root=" + root}, args...)...)
	command.Dir = root
	command.Env = append(os.Environ(), "BUILD_WORKSPACE_DIRECTORY="+root)
	b, err := command.CombinedOutput()
	return string(b), err
}

func protoTree() map[string]string {
	return map[string]string{
		"MODULE.bazel": "module(name = \"proto_fixture\")\n",
		"BUILD.bazel":  "# gazelle:exclude ignored\n",
		"settings/BUILD.bazel": `load("@rules_typescript//ts:defs.bzl", "ts_config")
# keep
ts_config(name = "tsconfig", src = "tsconfig.json", visibility = ["//visibility:public"])
`,
		"settings/tsconfig.json": `{"compilerOptions":{"types":[]},"include":["*.ts"]}`,
		"schema/BUILD.bazel": `load("@rules_typescript//proto:defs.bzl", "ts_proto_config")
# gazelle:proto_strip_import_prefix /schema
# gazelle:ts_proto plain json
ts_proto_config(
    name = "plain",
    out_dir = "generated/plain",
    tsconfig = "//settings:tsconfig",
    deps = ["@npm//:bufbuild_protobuf"],
)
ts_proto_config(
    name = "json",
    out_dir = "generated/json",
    tsconfig = "//settings:tsconfig",
    deps = ["@npm//:bufbuild_protobuf"],
    options = ["target=ts", "json_types=true"],
)
`,
		"schema/common/common.proto":     `syntax = "proto3"; package example.common; message Common { string value = 1; }`,
		"schema/messages/message.proto":  `syntax = "proto3"; package example.messages; import "common/common.proto"; import "google/protobuf/timestamp.proto"; message Message { example.common.Common common = 1; google.protobuf.Timestamp at = 2; }`,
		"schema/disabled/BUILD.bazel":    "# gazelle:ts_proto none\n",
		"schema/disabled/disabled.proto": `syntax = "proto3"; package example.disabled; message Disabled {}`,
		"ignored/BUILD.bazel":            "# gazelle:ts_proto plain\n",
	}
}

func TestProtoGenerationKeepsSiblingImportsWithinEachOutputIdentity(t *testing.T) {
	root := writeTree(t, protoTree())
	output, err := protoGazelle(t, root, "schema")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	file := filepath.Join(root, "schema/BUILD.bazel")
	first, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(first)
	for _, want := range []string{`name = "plain/common/example_common_proto"`, `name = "json/messages/example_messages_proto"`, `:plain/common/example_common_proto`, `:json/common/example_common_proto`, `proto = "//schema/messages:example_messages_proto"`, `json_types=true`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in\n%s", want, text)
		}
	}
	if strings.Contains(text, "timestamp_proto") || strings.Contains(text, "example_disabled_proto") {
		t.Fatalf("runtime/excluded input became generated sibling:\n%s", text)
	}
	output, err = protoGazelle(t, root, "schema")
	if err != nil {
		t.Fatalf("repeat: %v\n%s", err, output)
	}
	second, _ := os.ReadFile(file)
	if string(first) != string(second) {
		t.Fatalf("repeat generation changed owner:\n%s", second)
	}
	if err := os.Remove(filepath.Join(root, "schema/messages/message.proto")); err != nil {
		t.Fatal(err)
	}
	output, err = protoGazelle(t, root, "schema")
	if err != nil {
		t.Fatalf("removal: %v\n%s", err, output)
	}
	after, _ := os.ReadFile(file)
	if strings.Contains(string(after), "example_messages_proto") {
		t.Fatalf("deleted schema retains wrappers:\n%s", after)
	}
}

func TestProtoPartialUpdatesCannotOverwriteAnIncompleteOwner(t *testing.T) {
	for _, args := range [][]string{{"schema/common"}, {"-r=false", "schema"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			root := writeTree(t, protoTree())
			before := map[string]string{}
			filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					b, _ := os.ReadFile(p)
					before[p] = string(b)
				}
				return nil
			})
			output, err := protoGazelle(t, root, args...)
			if err == nil || !strings.Contains(output, "requires a complete recursive update") {
				t.Fatalf("partial update must fail: %v\n%s", err, output)
			}
			filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					b, _ := os.ReadFile(p)
					if before[p] != string(b) {
						t.Errorf("partial invocation changed %s", p)
					}
				}
				return nil
			})
		})
	}
}

func TestProtoConfigurationCannotSilentlyAcceptExpressionsOrNonTypeScriptOutputs(t *testing.T) {
	for _, input := range []string{
		`ts_proto_config(name="x", out_dir="../escape", tsconfig=":config")`,
		`ts_proto_config(name="x", out_dir="generated", tsconfig=":config", options=["target=js"])`,
		`ts_proto_config(name="x", out_dir="generated", tsconfig=":config", options=[])`,
		`ts_proto_config(name="x", out_dir="generated", tsconfig=select({"//conditions:default": ":config"}))`,
		`ts_proto_config(name="x", out_dir="generated", tsconfig=":config", deps=DEPS)`,
		`ts_proto_config(name="x", out_dir="generated", tsconfig=":config", options=[OPTION])`,
	} {
		f, err := rule.LoadData("BUILD.bazel", "schema", []byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseProtoIdentity(f.Rules[0], "schema"); err == nil {
			t.Errorf("accepted unsupported configuration %s", input)
		}
	}
}

func TestProtoExistingWrappersResolveWithoutRegeneratingTheirOwner(t *testing.T) {
	for _, indexed := range []bool{true, false} {
		for _, prefix := range []string{"", "api"} {
			t.Run(fmt.Sprintf("indexed=%t/prefix=%s", indexed, prefix), func(t *testing.T) {
				c := emptyConfig()
				c.RepoName = "fixture"
				ts := &tsLang{}
				native := gazelleproto.NewLanguage()
				ix := resolve.NewRuleIndex(func(r *rule.Rule, _ string) resolve.Resolver {
					if r.Kind() == "proto_library" {
						return native
					}
					return ts
				})
				proto := rule.NewRule("proto_library", "message_proto")
				proto.SetAttr("srcs", []string{"message.proto"})
				proto.SetAttr("strip_import_prefix", "/schema")
				proto.SetAttr("import_prefix", prefix)
				nativeFile := rule.EmptyFile("BUILD.bazel", "schema/messages")
				nativeFile.Rules = append(nativeFile.Rules, proto)
				wrappers := rule.EmptyFile("BUILD.bazel", "schema")
				for _, id := range []string{"plain", "json"} {
					wrapper := rule.NewRule("ts_proto_library", id+"/messages/message_proto")
					wrapper.SetAttr("proto", "//schema/messages:message_proto")
					wrapper.SetAttr("out_dir", "generated/"+id)
					wrappers.Rules = append(wrappers.Rules, wrapper)
				}
				for _, file := range []*rule.File{wrappers, nativeFile} {
					if indexed {
						for _, r := range file.Rules {
							ix.AddRule(c, r, file)
						}
					} else {
						getConfig(c).programs.recordBuild(c, file.Pkg, file)
					}
				}
				ix.Finish()
				for _, id := range []string{"plain", "json"} {
					output := path.Join("schema/generated", id, prefix, "messages/message_pb.ts")
					got := resolveProtoOutput(c, ix, output, label.New(c.RepoName, "consumer", "consumer"))
					want := "//schema:" + id + "/messages/message_proto"
					if got != want {
						t.Errorf("%s: got %q, want %q", id, got, want)
					}
					for _, unrelated := range []string{
						path.Join("schema/generated", id, prefix, "messages/other_pb.ts"),
						path.Join("schema/generated", id, prefix, "schema/messages/message_pb.ts"),
					} {
						if got := protoOutputProviders(c, ix, unrelated); len(got) != 0 {
							t.Errorf("%s: selected providers for unrelated output %s: %v", id, unrelated, got)
						}
					}
				}
			})
		}
	}
}

func TestExcludedProtoOutputOverridePrecedesNativeProviderSelection(t *testing.T) {
	for _, override := range []string{"//schema:custom", "@fixture//schema:custom", "@@fixture//schema:custom", "@@other//schema:custom"} {
		for _, providers := range []int{0, 1, 2} {
			t.Run(override+fmt.Sprint(providers), func(t *testing.T) {
				c := emptyConfig()
				c.RepoName = "fixture"
				const file = "schema/generated/messages/message_pb.ts"
				f, err := rule.LoadData("BUILD.bazel", "", []byte(
					"# gazelle:resolve typescript "+file+" "+override+"\n"))
				if err != nil {
					t.Fatal(err)
				}
				(&resolve.Configurer{}).Configure(c, "", f)
				ts, native := &tsLang{}, gazelleproto.NewLanguage()
				ix := resolve.NewRuleIndex(func(r *rule.Rule, _ string) resolve.Resolver {
					if r.Kind() == "proto_library" {
						return native
					}
					return ts
				})
				wrappers := rule.EmptyFile("BUILD.bazel", "schema")
				if providers == 0 {
					producer := rule.NewRule("genrule", "custom_source")
					producer.SetAttr("outs", []string{"generated/messages/message_pb.ts"})
					producer.SetAttr("cmd", "echo 'export const value = 1;' > $@")
					compiler := rule.NewRule("ts_compile", "custom")
					compiler.SetAttr("srcs", []string{":custom_source"})
					wrappers.Rules = append(wrappers.Rules, producer, compiler)
				}
				for i := range providers {
					name := fmt.Sprintf("message%d_proto", i)
					proto := rule.NewRule("proto_library", name)
					proto.SetAttr("srcs", []string{"message.proto"})
					proto.SetAttr("strip_import_prefix", "/schema")
					ix.AddRule(c, proto, rule.EmptyFile("BUILD.bazel", "schema/messages"))
					wrapper := rule.NewRule("ts_proto_library", "generated/"+name)
					wrapper.SetAttr("proto", "//schema/messages:"+name)
					wrapper.SetAttr("out_dir", "generated")
					wrappers.Rules = append(wrappers.Rules, wrapper)
				}
				getConfig(c).programs.recordBuild(c, wrappers.Pkg, wrappers)
				for _, wrapper := range wrappers.Rules {
					ix.AddRule(c, wrapper, wrappers)
				}
				ix.Finish()
				if getConfig(c).programs.sourceFile(file) {
					t.Fatal("excluded output is eligible as a source")
				}
				dep, generated := generatedDep(c, ix, getConfig(c), file, label.New(c.RepoName, "consumer", "consumer"))
				want := "//schema:custom"
				if strings.HasPrefix(override, "@@") {
					want = override
				}
				if dep != want || !generated {
					t.Errorf("override = %q, generated = %t; want %s, true", dep, generated, want)
				}
			})
		}
	}
}

func TestKeptProtoContractCannotClaimProposedOutputs(t *testing.T) {
	for _, contract := range []string{"out_dir", "proto"} {
		for _, ownership := range []string{"attribute", "rule"} {
			t.Run(contract+"/"+ownership, func(t *testing.T) {
				selectedProto, outDir, actual := ":example_message_proto", "generated", "schema/custom/message_pb.ts"
				if contract == "out_dir" {
					outDir = "custom"
				} else {
					selectedProto, actual = "//schema/other:example_other_proto", "schema/generated/other/other_pb.ts"
				}
				keepRule, keepProto, keepOutDir := "", "", ""
				if ownership == "rule" {
					keepRule = "# keep\n"
				} else if contract == "proto" {
					keepProto = " # keep"
				} else {
					keepOutDir = " # keep"
				}
				root := writeTree(t, map[string]string{
					"MODULE.bazel": "module(name = \"kept_proto_contract\")\n",
					"schema/BUILD.bazel": `load("@rules_typescript//proto:defs.bzl", "ts_proto_config", "ts_proto_library")
# gazelle:proto_strip_import_prefix /schema
# gazelle:ts_proto plain
ts_proto_config(name = "plain", out_dir = "generated", tsconfig = "//app:tsconfig")
ts_proto_config(name = "other", out_dir = "alternate", tsconfig = "//app:tsconfig")
` + keepRule + `ts_proto_library(
    name = "plain/example_message_proto",
    proto = "` + selectedProto + `",` + keepProto + `
    out_dir = "` + outDir + `",` + keepOutDir + `
    tsconfig = "//app:tsconfig",
)
`,
					"schema/message.proto":            `syntax = "proto3"; package example.message; message Message {}`,
					"schema/other/BUILD.bazel":        "# gazelle:ts_proto other\n",
					"schema/other/other.proto":        `syntax = "proto3"; package example.other; message Other {}`,
					"schema/generated/neighbor.ts":    "export const neighbor = 1;\n",
					path.Dir(actual) + "/neighbor.ts": "export const neighbor = 1;\n",
					"app/tsconfig.json":               `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#old":["../schema/generated/message_pb.js","./fallback.js"]}},"files":["index.ts"]}`,
					"app/index.ts":                    "export { value } from '#old';\n",
					"app/fallback.ts":                 "export const value = 1;\n",
					"client/tsconfig.json":            `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#actual":["../` + strings.TrimSuffix(actual, ".ts") + `.js","./fallback.js"]}},"files":["index.ts"]}`,
					"client/index.ts":                 "export { value } from '#actual';\n",
					"client/fallback.ts":              "export const value = 'not generated';\n",
				})
				for _, state := range []string{"cold", "materialized"} {
					if state == "materialized" {
						writeFile(t, filepath.Join(root, "schema/generated/message_pb.ts"), "export const value = 'authored old path';\n")
					}
					var first map[string]string
					for _, args := range [][]string{nil, {"-index=false"}, nil} {
						output, err := protoGazelle(t, root, args...)
						if err != nil {
							t.Fatalf("%s %v: %v\n%s", state, args, err, output)
						}
						app := onDiskRule(t, root, "app", "ts_compile", "app")
						sources := []string{"index.ts", "fallback.ts"}
						if state == "materialized" {
							sources = []string{"index.ts", "//schema:generated/message_pb.ts"}
						}
						wantLabels(t, "proposed output does not replace authored compiler membership", app.AttrStrings("srcs"), sources)
						wantLabels(t, "proposed output has no generated supplier", app.AttrStrings("deps"), nil)
						client := onDiskRule(t, root, "client", "ts_compile", "client")
						wantLabels(t, "effective output replaces the compiler fallback", client.AttrStrings("srcs"), []string{"index.ts"})
						wantLabels(t, "effective wrapper supplies its actual output", client.AttrStrings("deps"), []string{"//schema:plain/example_message_proto"})
						wrapper := onDiskRule(t, root, "schema", "ts_proto_library", "plain/example_message_proto")
						if wrapper.AttrString("proto") != selectedProto || wrapper.AttrString("out_dir") != outDir {
							t.Fatalf("kept output contract changed: proto=%s out_dir=%s", wrapper.AttrString("proto"), wrapper.AttrString("out_dir"))
						}
						if first == nil {
							first = buildFileBytes(t, root)
						} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
							t.Fatalf("%s index mode changed the effective contract: %s", state, diff)
						}
					}
				}
			})
		}
	}
}

func TestObservedProtoWrapperRequiresAnEffectiveOutputContract(t *testing.T) {
	for _, contract := range []string{"literal", "duplicate output", "occupied kind", "unknown out_dir", "unknown proto"} {
		t.Run(contract, func(t *testing.T) {
			c := emptyConfig()
			c.RepoName = "fixture"
			file := rule.EmptyFile("BUILD.bazel", "schema")
			wrapper := rule.NewRule("ts_proto_library", "plain/message_proto")
			wrapper.SetAttr("proto", ":message_proto")
			wrapper.SetAttr("out_dir", "generated")
			switch contract {
			case "duplicate output":
				duplicate := rule.NewRule("ts_proto_library", "duplicate")
				duplicate.SetAttr("proto", ":message_proto")
				duplicate.SetAttr("out_dir", "generated")
				file.Rules = append(file.Rules, duplicate)
			case "occupied kind":
				wrapper = rule.NewRule("filegroup", wrapper.Name())
				wrapper.SetAttr("srcs", []string{"authored.ts"})
			case "unknown out_dir":
				wrapper.SetAttr("out_dir", &bzl.Ident{Name: "OUTPUT_ROOT"})
			case "unknown proto":
				wrapper.SetAttr("proto", &bzl.Ident{Name: "NATIVE_PROTO"})
			}
			file.Rules = append(file.Rules, wrapper)
			getConfig(c).programs.recordBuild(c, file.Pkg, file)
			getConfig(c).protos.observations["schema"] = []protoObservation{{
				config: c, native: label.New(c.RepoName, "schema", "message_proto"),
				identity: &protoIdentity{Name: "plain", owner: "schema", OutDir: "generated"}, paths: []string{"message.proto"},
			}}
			ix := resolve.NewRuleIndex(func(*rule.Rule, string) resolve.Resolver { return &tsLang{} })
			ix.Finish()
			var got, want []string
			for _, provider := range protoOutputProviders(c, ix, "schema/generated/message_pb.ts") {
				got = append(got, provider.Label.String())
			}
			if contract == "literal" {
				want = []string{"@fixture//schema:plain/message_proto"}
			} else if contract == "duplicate output" {
				want = []string{"@fixture//schema:duplicate", "@fixture//schema:plain/message_proto"}
			}
			wantLabels(t, "only an effective wrapper contract supplies the output", got, want)
		})
	}
}

func TestProtoSelectionPreservesCompilerObservationUntilNativeIndexCompletes(t *testing.T) {
	c := emptyConfig()
	s := getConfig(c).programs
	const output = "schema/generated/value_pb.ts"
	p := &program{dir: "app", Listing: explainfiles.Listing{
		Files: []string{"app/index.ts", "app/fallback.ts", "app/helper.ts"},
		Roots: []string{"app/index.ts"},
		Edges: []explainfiles.Edge{
			importEdge("app/index.ts", "#value", "app/fallback.ts"),
			importEdge("app/fallback.ts", "./helper", "app/helper.ts"),
		},
	}, candidates: []resolutionCandidate{{from: "app/index.ts", specifier: "#value", path: output, resolved: "app/fallback.ts", file: true}}}
	observation := *p
	observation.Files = slices.Clone(p.Files)
	observation.Edges = slices.Clone(p.Edges)
	observation.candidates = slices.Clone(p.candidates)
	s.inputs["app"] = programInput{config: c, program: p}
	s.selectInput("app")
	wantStrings(t, "provisional membership", s.programs["app"].Files, observation.Files)

	native, ts := gazelleproto.NewLanguage(), &tsLang{}
	ix := resolve.NewRuleIndex(func(r *rule.Rule, _ string) resolve.Resolver {
		if r.Kind() == "proto_library" {
			return native
		}
		return ts
	})
	provider := rule.NewRule("proto_library", "value_proto")
	provider.SetAttr("srcs", []string{"value.proto"})
	provider.SetAttr("strip_import_prefix", "/schema")
	ix.AddRule(c, provider, rule.EmptyFile("BUILD.bazel", "schema"))
	wrapper := rule.NewRule("ts_proto_library", "plain/value_proto")
	wrapper.SetAttr("proto", ":value_proto")
	wrapper.SetAttr("out_dir", "generated")
	ix.AddRule(c, wrapper, rule.EmptyFile("BUILD.bazel", "schema"))
	ix.Finish()
	s.selectInputs(ix)
	wantStrings(t, "final membership", s.programs["app"].Files, []string{"app/index.ts"})
	if edges := s.programs["app"].Edges; len(edges) != 1 || edges[0].To != output {
		t.Fatalf("completed provider did not replace fallback: %v", edges)
	}
	if !reflect.DeepEqual(*p, observation) {
		t.Fatal("selection mutated the compiler observation")
	}
}

func TestNativeProtoSelectionIsIndependentOfConsumerWalkOrder(t *testing.T) {
	for _, test := range []struct{ consumer, source, kind, name string }{
		{"app", "index.ts", "ts_compile", "app"},
		{"web", "index.ts", "ts_compile", "web"},
		{"app", "index.test.ts", "ts_test", "app_test"},
	} {
		consumer := test.consumer
		for _, override := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/override=%t", consumer, test.source, override), func(t *testing.T) {
				build := "# gazelle:exclude " + consumer + "/ignored.ts\n# gazelle:exclude schema/generated/value_pb.ts\n"
				if override {
					build += "# gazelle:resolve typescript schema/generated/value_pb.ts //schema:chosen\n"
				}
				tree := map[string]string{
					"MODULE.bazel": "module(name = \"native_proto_selection\")\n",
					"BUILD.bazel":  build,
					"schema/BUILD.bazel": `load("@rules_typescript//proto:defs.bzl", "ts_proto_config")
# gazelle:proto_strip_import_prefix /schema
# gazelle:ts_proto plain
ts_proto_config(name = "plain", out_dir = "generated", tsconfig = "//` + consumer + `:tsconfig")
alias(name = "chosen", actual = ":plain/example_value_proto")
`,
					"schema/value.proto":              `syntax = "proto3"; package example.value; message Value {}`,
					"schema/generated/neighbor_pb.ts": "import { helper } from './helper.js'; export const neighbor = helper;\n",
					"schema/generated/helper.ts":      "export const helper = 42;\n",
					consumer + "/tsconfig.json":       `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#value":["../schema/generated/value_pb.js","./fallback.js"]}},"files":["` + test.source + `"]}`,
					consumer + "/" + test.source:      "export { value } from '#value'; export { neighbor } from '../schema/generated/neighbor_pb.js';\n",
					consumer + "/fallback.ts":         "export { value } from './fallback_helper.js';\n",
					consumer + "/fallback_helper.ts":  "export const value = 1;\n",
					consumer + "/ignored.ts":          "export const value = 'stale';\n",
					"client/tsconfig.json":            `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
					"client/index.ts":                 "export { value } from '../" + consumer + "/fallback.js';\n",
				}
				root := writeTree(t, tree)
				var cold map[string]string
				for _, state := range []string{"cold", "cold rerun", "stale", "stale rerun"} {
					if state == "stale" {
						writeFile(t, filepath.Join(root, "schema/generated/value_pb.ts"), "export { value } from '../../"+consumer+"/ignored.js';\n")
					}
					output, err := protoGazelle(t, root)
					if err != nil {
						t.Fatalf("%s: %v\n%s", state, err, output)
					}
					r := onDiskRule(t, root, consumer, test.kind, test.name)
					wantLabels(t, state+" authored membership", r.AttrStrings("srcs"), []string{"//schema:generated/helper.ts", "//schema:generated/neighbor_pb.ts", test.source})
					dep := "//schema:plain/example_value_proto"
					if override {
						dep = "//schema:chosen"
					}
					wantLabels(t, state+" generated dependency", r.AttrStrings("deps"), []string{dep})
					client := onDiskRule(t, root, "client", "ts_compile", "client")
					wantLabels(t, state+" borrowed fallback", client.AttrStrings("srcs"), []string{"//" + consumer + ":fallback.ts", "//" + consumer + ":fallback_helper.ts", "index.ts"})
					wantStrings(t, state+" no discarded compiler owner", client.AttrStrings("deps"), nil)
					if test.kind == "ts_test" {
						f, err := rule.LoadFile(filepath.Join(root, consumer, "BUILD.bazel"), consumer)
						if err != nil {
							t.Fatal(err)
						}
						for _, r := range f.Rules {
							if r.Kind() == "ts_compile" && r.Name() == consumer {
								t.Fatal("discarded fallback retained a compiler rule")
							}
						}
					}
					if cold == nil {
						cold = buildFileBytes(t, root)
					} else if diff := snapshotDiff(cold, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("%s changed ownership: %s", state, diff)
					}
				}
			})
		}
	}
}

func TestProtoConfigDoesNotMakeExcludedAuthoredOverrideEligible(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"excluded_proto_neighbor\")\n",
		"app/BUILD.bazel": `load("@rules_typescript//proto:defs.bzl", "ts_proto_config")
# gazelle:exclude generated/value_pb.ts
# gazelle:resolve typescript app/generated/value_pb.ts //app:custom
ts_proto_config(name = "unused", out_dir = "generated", tsconfig = ":tsconfig")
filegroup(name = "custom", srcs = ["generated/value_pb.ts"])
`,
		"app/tsconfig.json":         `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
		"app/index.ts":              "export { value } from './generated/value_pb.js';\n",
		"app/generated/value_pb.ts": "export const value = 1;\n",
	})
	before := buildFileBytes(t, root)
	output, err := protoGazelle(t, root)
	if err == nil || !strings.Contains(output, "app/index.ts imports app/generated/value_pb.ts") || !strings.Contains(output, "excluded or ignored") {
		t.Fatalf("unused config authorized excluded source override: %v\n%s", err, output)
	}
	if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
		t.Fatalf("excluded source changed BUILD files: %s", diff)
	}
}

func TestProtoIndependentOwnersDoNotShareGeneratedSiblings(t *testing.T) {
	tree := protoTree()
	for p, content := range protoTree() {
		if strings.HasPrefix(p, "schema/") {
			tree[strings.Replace(p, "schema/", "other/", 1)] = strings.ReplaceAll(content, "/schema", "/other")
		}
	}
	tree["other/BUILD.bazel"] += "# gazelle:proto_import_prefix alternate\n"
	tree["other/messages/message.proto"] = strings.ReplaceAll(tree["other/messages/message.proto"], `"common/common.proto"`, `"alternate/common/common.proto"`)
	root := writeTree(t, tree)
	output, err := protoGazelle(t, root, ".")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	for _, owner := range []string{"schema", "other"} {
		b, err := os.ReadFile(filepath.Join(root, owner, "BUILD.bazel"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "//"+owner+"/messages:example_messages_proto") || !strings.Contains(string(b), ":json/common/example_common_proto") {
			t.Fatalf("incorrect owner projection: %s", b)
		}
	}
}

func TestProtoRemovingIdentityDeletesOnlyItsGeneratedWrappers(t *testing.T) {
	root := writeTree(t, protoTree())
	output, err := protoGazelle(t, root, "schema")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	file := filepath.Join(root, "schema/BUILD.bazel")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	configured, err := rule.LoadData(file, "schema", b)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range configured.Rules {
		if r.Kind() == "ts_proto_config" {
			r.Delete()
		}
	}
	b = configured.Format()
	lines := strings.Split(string(b), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "# gazelle:ts_proto") {
			continue
		}
		kept = append(kept, line)
	}
	text := strings.Replace(strings.Join(kept, "\n"), "ts_proto_library(\n    name = \"plain/common/example_common_proto\",", "# keep\nts_proto_library(\n    name = \"plain/common/example_common_proto\",", 1) + `
ts_proto_library(name = "handwritten", proto = "//schema/common:example_common_proto", out_dir = "custom", tsconfig = "//settings:tsconfig")
`
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	output, err = protoGazelle(t, root, "schema")
	if err != nil {
		t.Fatalf("config removal: %v\n%s", err, output)
	}
	b, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `name = "plain/messages/`) || strings.Contains(string(b), `name = "json/`) || !strings.Contains(string(b), `name = "handwritten"`) || !strings.Contains(string(b), `name = "plain/common/example_common_proto"`) {
		t.Fatalf("identity cleanup corrupted ownership: %s", b)
	}
}

func TestUnusedProtoOutputDirectoryDoesNotWithdrawAuthoredProgram(t *testing.T) {
	for _, pkg := range []string{"app/generated", "app/generated/nested"} {
		t.Run(pkg, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel": "module(name = \"unused_proto_directory\")\n",
				"app/BUILD.bazel": `load("@rules_typescript//proto:defs.bzl", "ts_proto_config")
ts_proto_config(name = "unused", out_dir = "generated", tsconfig = "//` + pkg + `:tsconfig")
`,
				pkg + "/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts","index.test.ts"]}`,
				pkg + "/index.ts":      "export { value } from './value_pb.js';\n",
				pkg + "/value_pb.ts":   "export const value = 1;\n",
				pkg + "/index.test.ts": "import { value } from './index.js'; export { value };\n",
				pkg + "/notes.txt":     "authored data\n",
			})
			var first map[string]string
			for _, pass := range []string{"initial", "rerun"} {
				output, err := protoGazelle(t, root)
				if err != nil {
					t.Fatalf("%s: %v\n%s", pass, err, output)
				}
				name := filepath.Base(pkg)
				compile := onDiskRule(t, root, pkg, "ts_compile", name)
				wantLabels(t, pass+" authored program", compile.AttrStrings("srcs"), []string{"index.ts", "notes.txt", "value_pb.ts"})
				wantStrings(t, pass+" no generated dependency", compile.AttrStrings("deps"), nil)
				test := onDiskRule(t, root, pkg, "ts_test", name+"_test")
				wantLabels(t, pass+" authored test", test.AttrStrings("srcs"), []string{"index.test.ts"})
				wantLabels(t, pass+" authored test dependency", test.AttrStrings("deps"), []string{":" + name})
				if first == nil {
					first = buildFileBytes(t, root)
				} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
					t.Fatalf("authored program changed on rerun: %s", diff)
				}
			}
		})
	}
}

func TestProtoOutputMembershipCannotCreateASecondCompilerOwner(t *testing.T) {
	for _, discovery := range []string{"explicit imported", "wildcard", "wildcard kept root", "wildcard mapped kept root", "wildcard mapped scope publisher"} {
		t.Run(discovery, func(t *testing.T) {
			tree := map[string]string{
				"MODULE.bazel": "module(name = \"native_proto_membership\")\n",
				"schema/BUILD.bazel": `load("@rules_typescript//proto:defs.bzl", "ts_proto_config")
# gazelle:proto_strip_import_prefix /schema
# gazelle:ts_proto json
ts_proto_config(name = "json", out_dir = "generated/json", tsconfig = "//schema/generated/json:tsconfig")
`,
				"schema/message.proto":                `syntax = "proto3"; package example.message; message Message {}`,
				"schema/generated/json/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts","message_pb.ts"]}`,
				"schema/generated/json/index.ts":      "export { value } from './message_pb.js';\n",
				"schema/generated/json/notes.txt":     "authored data\n",
			}
			wantSrcs := []string{"index.ts", "notes.txt"}
			wantDeps := []string{"//schema:json/example_message_proto"}
			wantTestSrcs := []string{"helper.test.ts"}
			compileKind, testKind, devKind := "ts_compile", "ts_test", "ts_dev_server"
			if discovery != "explicit imported" {
				tree["schema/generated/json/tsconfig.json"] = `{"compilerOptions":{"allowJs":true,"module":"preserve","moduleResolution":"bundler"},"include":["index.ts","message_pb.*"]}`
				tree["schema/generated/json/index.ts"] = "export const index = 1;\n"
				tree["schema/generated/json/message_pb.js"] = "export { helper } from '../../../fixtures/helper.js';\nimport './helper.test.js';\nexport { app } from './app.js';\n"
				tree["schema/generated/json/helper.test.ts"] = "export const test = 1;\n"
				tree["schema/generated/json/app.ts"] = "export const app = 1;\n"
				tree["fixtures/BUILD.bazel"] = loadDefs + `"ts_compile")
ts_compile(name = "fixtures", srcs = ["helper.js"])
`
				tree["fixtures/tsconfig.json"] = `{"compilerOptions":{"allowJs":true},"files":["helper.js"]}`
				tree["fixtures/helper.js"] = "export const helper = 1;\n"
				wantSrcs = append(wantSrcs, "message_pb.js", "helper.test.ts", "app.ts")
				wantDeps = []string{"//fixtures"}
			}
			if discovery == "wildcard mapped scope publisher" {
				delete(tree, "schema/generated/json/index.ts")
				tree["schema/generated/json/index.test.ts"] = "export const index = 1;\n"
				tree["schema/generated/json/tsconfig.json"] = strings.Replace(tree["schema/generated/json/tsconfig.json"], "index.ts", "index.test.ts", 1)
				tree["schema/generated/json/package.json"] = `{"type":"module"}`
				wantSrcs = slices.DeleteFunc(wantSrcs, func(source string) bool { return source == "index.ts" })
				wantTestSrcs = append(wantTestSrcs, "index.test.ts")
			}
			if strings.HasSuffix(discovery, "kept root") {
				tree["schema/generated/json/BUILD.bazel"] = loadDefs + `"ts_compile")
ts_compile(name = "json", srcs = [
    "index.ts",
    "kept.ts", # keep
]
)
`
				tree["schema/generated/json/kept.ts"] = "export const kept = 1;\n"
				wantSrcs = append(wantSrcs, "kept.ts")
			}
			if strings.HasPrefix(discovery, "wildcard mapped ") {
				compileKind, testKind, devKind = "custom_compile", "custom_test", "custom_dev"
				tree["schema/generated/json/BUILD.bazel"] += `# gazelle:map_kind ts_compile custom_compile //:defs.bzl
# gazelle:map_kind ts_test custom_test //:defs.bzl
# gazelle:map_kind ts_dev_server custom_dev //:defs.bzl
ts_test(name = "json_test", tags = ["authored-tag"], timeout = "short")
`
			}
			root := writeTree(t, tree)
			var first map[string]string
			for _, state := range []string{"cold", "materialized", "removed"} {
				outputFile := filepath.Join(root, "schema/generated/json/message_pb.ts")
				if state == "materialized" {
					writeFile(t, outputFile, "export const value = 'stale';\n")
				} else if state == "removed" {
					if err := os.Remove(outputFile); err != nil {
						t.Fatal(err)
					}
				}
				for _, pass := range []string{"initial", "rerun"} {
					output, err := protoGazelle(t, root)
					if err != nil {
						t.Fatalf("%s %s: %v\n%s", state, pass, err, output)
					}
					compiler := onDiskRule(t, root, "schema/generated/json", compileKind, "json")
					wantLabels(t, state+" "+pass+" authored membership", compiler.AttrStrings("srcs"), wantSrcs)
					wantLabels(t, state+" "+pass+" dependency closure", compiler.AttrStrings("deps"), wantDeps)
					if discovery != "explicit imported" {
						test := onDiskRule(t, root, "schema/generated/json", testKind, "json_test")
						wantLabels(t, state+" "+pass+" discovered test sources", test.AttrStrings("srcs"), wantTestSrcs)
						roots := test.AttrStrings("test_srcs")
						if test.Attr("test_srcs") == nil {
							roots = test.AttrStrings("srcs")
						}
						wantLabels(t, state+" "+pass+" executable test roots", roots, wantTestSrcs)
						wantLabels(t, state+" "+pass+" discovered test deps", test.AttrStrings("deps"), []string{":json"})
						dev := onDiskRule(t, root, "schema/generated/json", devKind, "dev")
						if dev.AttrString("entry_point") != ":json" {
							t.Fatalf("%s %s lost the discovered app entry: %v", state, pass, dev.AttrString("entry_point"))
						}
						if strings.HasPrefix(discovery, "wildcard mapped ") {
							wantStrings(t, "authored test tags", test.AttrStrings("tags"), []string{"authored-tag"})
							if test.AttrString("timeout") != "short" {
								t.Fatalf("%s %s lost the authored test timeout", state, pass)
							}
						}
					}
					onDiskRule(t, root, "schema", "ts_proto_library", "json/example_message_proto")
					if first == nil {
						first = buildFileBytes(t, root)
					} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("%s %s changed native output ownership: %s", state, pass, diff)
					}
				}
				for file, before := range tree {
					if strings.HasSuffix(file, "BUILD.bazel") {
						continue
					}
					after, err := os.ReadFile(filepath.Join(root, file))
					if err != nil || string(after) != before {
						t.Fatalf("discovery changed authored file %s: %v", file, err)
					}
				}
				projections, err := filepath.Glob(filepath.Join(root, "schema/generated/json/.gazelle-tsconfig-*.json"))
				if err != nil || len(projections) != 0 {
					t.Fatalf("discovery left temporary configurations: %v, %v", projections, err)
				}
				if discovery == "wildcard" && state == "materialized" {
					writeFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude fixtures/helper.js\n")
					before := buildFileBytes(t, root)
					output, err := protoGazelle(t, root, "schema")
					if err == nil || !strings.Contains(output, "imports fixtures/helper.js") || !strings.Contains(output, "excluded or ignored") {
						t.Fatalf("relisted closure bypassed native exclusions: %v\n%s", err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("rejected closure changed BUILD files: %s", diff)
					}
					if err := os.Remove(filepath.Join(root, "BUILD.bazel")); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
	for _, test := range []struct {
		kept  bool
		scope string
	}{{false, "none"}, {true, "none"}, {false, "unused"}, {true, "unused"}, {false, "retained"}, {true, "retained"}} {
		t.Run(fmt.Sprintf("generated-only roots cannot relocate parent exports/kept=%t/scope=%s", test.kept, test.scope), func(t *testing.T) {
			const child = "schema/generated/plain/common"
			tree := protoTree()
			tree["BUILD.bazel"] += "# gazelle:resolve proto common/common.proto //schema/common:example_common_proto\n"
			tree[child+"/tsconfig.json"] = `{"compilerOptions":{"types":[]},"files":["common_pb.ts"]}`
			tree[child+"/fixture.json"] = `{"value":42}`
			tree["app/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","resolveJsonModule":true,"types":[]},"files":["index.ts"]}`
			tree["app/index.ts"] = "import fixture from '../schema/generated/plain/common/fixture.json'; export const value = fixture.value;\n"
			if test.scope != "none" {
				tree[child+"/package.json"] = `{"type":"module"}`
				tree[child+"/unused.ts"] = "export const scoped = 1;\n"
				tree["app/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","resolveJsonModule":true,"types":[]},"files":["index.ts","../schema/generated/plain/common/unused.ts"]}`
				tree["schema/BUILD.bazel"] += "exports_files([\"generated/plain/common/package.json\", \"generated/plain/common/unused.ts\"], visibility = [\"//app:__pkg__\"])\n"
				if test.scope == "retained" {
					tree["app/index.ts"] += "export { scoped } from '../schema/generated/plain/common/unused';\n"
				}
			}
			if test.kept {
				tree["schema/BUILD.bazel"] += "# keep\n"
			}
			tree["schema/BUILD.bazel"] += "exports_files([\"generated/plain/common/fixture.json\"], visibility = [\"//app:__pkg__\"])\n"
			root := writeTree(t, tree)
			var first map[string]string
			for _, state := range []string{"cold", "materialized", "removed"} {
				outputFile := filepath.Join(root, child, "common_pb.ts")
				if state == "materialized" {
					writeFile(t, outputFile, "export const value = 'stale';\n")
				} else if state == "removed" {
					if err := os.Remove(outputFile); err != nil {
						t.Fatal(err)
					}
				}
				for _, args := range [][]string{nil, {"-index=false"}, {"-r=false", "app"}} {
					if output, err := protoGazelle(t, root, args...); err != nil {
						t.Fatalf("%s update %v: %v\n%s", state, args, err, output)
					}
					if _, err := os.Stat(filepath.Join(root, child, "BUILD.bazel")); !os.IsNotExist(err) {
						t.Fatalf("%s generated-only program created a package boundary: %v", state, err)
					}
					consumer := onDiskRule(t, root, "app", "ts_compile", "app")
					wantSrcs := []string{"index.ts", "//schema:generated/plain/common/fixture.json"}
					var wantScopes []string
					if test.scope == "retained" {
						wantSrcs = append(wantSrcs, "//schema:generated/plain/common/unused.ts")
						wantScopes = []string{"//schema:generated/plain/common/package.json"}
					}
					wantLabels(t, "borrowed source keeps its surviving package", consumer.AttrStrings("srcs"), wantSrcs)
					wantLabels(t, "only retained compiler inputs demand metadata", consumer.AttrStrings("package_scopes"), wantScopes)
					exported := false
					for _, r := range loadRules(t, root, "schema") {
						if r.Kind() != "exports_files" {
							continue
						}
						membership, err := exportSourceMembership(r)
						if err != nil {
							t.Fatal(err)
						}
						if len(membership.List) != 1 || membership.List[0].(*bzl.StringExpr).Value != "generated/plain/common/fixture.json" {
							continue
						}
						exported = r.ShouldKeep() == test.kept
						wantLabels(t, "parent export keeps explicit visibility", r.AttrStrings("visibility"), []string{"//app:__pkg__"})
					}
					if !exported {
						t.Fatalf("%s changed the authored parent export or its keep marker", state)
					}
					if first == nil {
						first = buildFileBytes(t, root)
					} else if diff := snapshotDiff(first, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("%s update %v changed protected BUILD bytes: %s", state, args, diff)
					}
					for name, before := range tree {
						if strings.HasSuffix(name, "BUILD.bazel") {
							continue
						}
						if got, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(got) != before {
							t.Fatalf("%s changed authored input %s: %v", state, name, err)
						}
					}
				}
			}
		})
	}
}

func TestProtoConflictingNativeOwnersCannotDeclareTheSameOutput(t *testing.T) {
	tree := protoTree()
	tree["schema/other/BUILD.bazel"] = "# gazelle:proto_strip_import_prefix /schema/other\n# gazelle:proto_import_prefix common\n"
	tree["schema/other/common.proto"] = `syntax = "proto3"; package example.other; message Other {}`
	root := writeTree(t, tree)
	output, err := protoGazelle(t, root, "schema")
	if err == nil || !strings.Contains(output, "generate the same proto output") {
		t.Fatalf("conflicting native ownership must fail: %v\n%s", err, output)
	}
	if b, err := os.ReadFile(filepath.Join(root, "schema/BUILD.bazel")); err != nil || string(b) != tree["schema/BUILD.bazel"] {
		t.Fatalf("conflict changed owner BUILD: %s (%v)", b, err)
	}
}

func TestProtoNativeImportOverrideCannotBorrowAnUnrelatedGeneratedOwner(t *testing.T) {
	tree := protoTree()
	tree["schema/other/BUILD.bazel"] = "# gazelle:proto_strip_import_prefix /schema/other\n# gazelle:proto_import_prefix common\n# gazelle:ts_proto none\n"
	tree["schema/other/common.proto"] = `syntax = "proto3"; package example.other; message Other {}`
	tree["schema/messages/BUILD.bazel"] = "# gazelle:resolve proto common/common.proto //schema/other:example_other_proto\n"
	root := writeTree(t, tree)
	output, err := protoGazelle(t, root, "schema")
	if err == nil || !strings.Contains(output, "0 generated providers in identity") {
		t.Fatalf("native override must not borrow a different schema: %v\n%s", err, output)
	}
}

func TestProtoConfigurationWithoutNativeLanguageCannotEraseItsGraph(t *testing.T) {
	c := emptyConfig()
	f := rule.EmptyFile("BUILD.bazel", "schema")
	r := rule.NewRule("ts_proto_config", "plain")
	r.SetAttr("out_dir", "generated")
	r.SetAttr("tsconfig", ":config")
	r.Insert(f)
	err := configureProto(c, "schema", f, defaultTsConfig())
	if err == nil || !strings.Contains(err.Error(), "requires the native proto language") {
		t.Fatalf("missing native owner must fail: %v", err)
	}
}

func TestProtoReusedLanguageCannotCarryAnEarlierInvocationGraph(t *testing.T) {
	language := NewLanguage()
	first := emptyConfig()
	language.RegisterFlags(flag.NewFlagSet("first", flag.ContinueOnError), "update", first)
	old := getConfig(first).protos
	old.identities["first"] = &protoIdentity{Name: "first", owner: "first", OutDir: "generated"}
	old.observations["first"] = []protoObservation{{native: label.New("", "first", "schema_proto")}}
	old.roots = []string{"first"}
	old.graphPath = "earlier.json"
	old.graph = &protoGraph{Roots: map[string]string{"@earlier//:schema": "@@earlier+//:schema"}}

	second := emptyConfig()
	second.WorkDir = second.RepoRoot
	flags := flag.NewFlagSet("second", flag.ContinueOnError)
	language.RegisterFlags(flags, "update", second)
	if err := language.CheckFlags(flags, second); err != nil {
		t.Fatal(err)
	}
	current := getConfig(second).protos
	if current == old || current.graph != nil || current.graphPath != "" || len(current.identities) != 0 || len(current.observations) != 0 || len(current.roots) != 1 || current.roots[0] != "" {
		t.Fatalf("a reused language carried the earlier generation graph: %+v", current)
	}
}

func TestProtoExternalNativeEdgesCannotBeRetargetedByLocalImportOverrides(t *testing.T) {
	c := emptyConfig()
	f := rule.EmptyFile("BUILD.bazel", "")
	f.Directives = []rule.Directive{{Key: "resolve", Value: "proto foreign/b.proto @renamed//:unused"}}
	(&resolve.Configurer{}).Configure(c, "", f)
	file := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(file, []byte(`{"roots":{"@renamed//:a":"@@a+//:a","@renamed//:unused":"@@a+//:unused"},"nodes":[{"label":"@@a+//:a","sources":["foreign/a.proto"],"deps":["@@b+//:b"]},{"label":"@@b+//:b","sources":["foreign/b.proto"]},{"label":"@@a+//:unused","sources":["foreign/b.proto"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	graph, err := loadProtoGraph(file)
	if err != nil {
		t.Fatal(err)
	}
	store := getConfig(c).protos
	store.graph = graph
	id := &protoIdentity{Name: "json", owner: "schema", OutDir: "generated"}
	observations, err := store.externalObservations([]protoObservation{{config: c, native: label.New("", "schema", "local"), identity: id, paths: []string{"local.proto"}, imports: []string{"foreign/a.proto"}}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, obs := range observations {
		seen[obs.native.String()] = true
		if obs.native.String() == "@@a+//:a" && (len(obs.nativeDeps) != 1 || obs.nativeDeps[0].String() != "@@b+//:b") {
			t.Fatalf("native A dependency was rewritten: %+v", obs.nativeDeps)
		}
	}
	if !seen["@@b+//:b"] || seen["@@a+//:unused"] {
		t.Fatalf("external availability or local override changed native closure: %v", seen)
	}
}

func TestProtoExplicitExternalOverrideCannotBeHiddenByLocalMembership(t *testing.T) {
	c := emptyConfig()
	f := rule.EmptyFile("BUILD.bazel", "")
	f.Directives = []rule.Directive{{Key: "resolve", Value: "proto shared.proto @external//:shared"}}
	(&resolve.Configurer{}).Configure(c, "", f)
	file := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(file, []byte(`{"roots":{"@external//:shared":"@@external+//:shared"},"nodes":[{"label":"@@external+//:shared","sources":["shared.proto"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	graph, err := loadProtoGraph(file)
	if err != nil {
		t.Fatal(err)
	}
	store := getConfig(c).protos
	store.graph = graph
	id := &protoIdentity{Name: "plain", owner: "schema", OutDir: "generated"}
	observations, err := store.externalObservations([]protoObservation{
		{config: c, native: label.New("", "schema", "local"), identity: id, paths: []string{"shared.proto"}},
		{config: c, native: label.New("", "schema", "consumer"), identity: id, paths: []string{"consumer.proto"}, imports: []string{"shared.proto"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, obs := range observations {
		if obs.native.String() == "@@external+//:shared" {
			return
		}
	}
	t.Fatal("local membership hid the explicit external native owner")
}

func TestProtoNativePackageMovesAncestorSourceExportWithoutCompilerProgram(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel": `module(name = "native_exports")`,
				"BUILD.bazel":  "",
				"schema/BUILD.bazel": `exports_files(["kept.txt", "messages/value.proto"], visibility=["//consumer:__pkg__"], licenses=["notice"])
exports_files(["messages/notes.txt"], visibility=[":__pkg__"], licenses=["restricted"])`,
				"schema/kept.txt":             "retained",
				"schema/messages/notes.txt":   "notes",
				"schema/messages/value.proto": `syntax = "proto3"; package example.messages; message Value { string name = 1; }`,
			})
			ancestorPath := filepath.Join(root, "schema/BUILD.bazel")
			ancestor := buildFileText(t, root, "schema")
			writeFile(t, ancestorPath, ancestor+"\nfilegroup(name = \"messages/forward\", srcs = [])\n")
			before := buildFileBytes(t, root)
			output, err := protoGazelle(t, root, fmt.Sprintf("-index=%t", indexed), "schema")
			if err == nil || !strings.Contains(output, "target //schema:messages/forward") {
				t.Fatalf("native package crossed an ancestor target: %v\n%s", err, output)
			}
			if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
				t.Fatalf("refused native package wrote BUILD files: %s", diff)
			}
			writeFile(t, ancestorPath, ancestor)
			output, err = protoGazelle(t, root, fmt.Sprintf("-index=%t", indexed), "schema")
			if err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
			parent := buildFileText(t, root, "schema")
			child := buildFileText(t, root, "schema/messages")
			if strings.Contains(parent, "messages/value.proto") || strings.Contains(parent, "messages/notes.txt") || !strings.Contains(parent, "kept.txt") {
				t.Fatalf("ancestor retains child-owned source:\n%s", parent)
			}
			file, err := rule.LoadFile(filepath.Join(root, "schema/messages/BUILD.bazel"), "schema/messages")
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]bool{}
			for _, r := range file.Rules {
				if r.Kind() == "ts_compile" || r.Kind() == "ts_proto_library" {
					t.Fatal("native package invented TypeScript program")
				}
				if r.Kind() != "exports_files" {
					continue
				}
				values, err := exportSourceMembership(r)
				if err != nil || len(values.List) != 1 {
					t.Fatalf("export source list lost:\n%s", child)
				}
				value, ok := values.List[0].(*bzl.StringExpr)
				if !ok || found[value.Value] {
					t.Fatalf("export source duplicated or invalid:\n%s", child)
				}
				found[value.Value] = true
				visibility, licenses := "//consumer:__pkg__", "notice"
				if value.Value == "notes.txt" {
					visibility, licenses = "//schema:__pkg__", "restricted"
				}
				if (value.Value != "value.proto" && value.Value != "notes.txt") || strings.Join(r.AttrStrings("visibility"), ",") != visibility || strings.Join(r.AttrStrings("licenses"), ",") != licenses {
					t.Fatalf("export contract lost:\n%s", child)
				}
			}
			if !found["value.proto"] || !found["notes.txt"] {
				t.Fatalf("native package lacks source export:\n%s", child)
			}
			output, err = protoGazelle(t, root, fmt.Sprintf("-index=%t", indexed), "schema")
			if err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
			if next := buildFileText(t, root, "schema/messages"); next != child {
				t.Fatalf("native export relocation unstable:\n%s", lineDiff(child, next))
			}
			if next := buildFileText(t, root, "schema"); next != parent {
				t.Fatalf("ancestor relocation unstable:\n%s", lineDiff(parent, next))
			}
		})
	}
}

func TestProtoAmbientDependenciesFollowSelectedConfigInsteadOfWrapperDirectory(t *testing.T) {
	for _, test := range []struct {
		name, selected, configName, configSource string
		wantAmbient                              bool
	}{
		{"generated_config", "//web:tsconfig", "tsconfig", "tsconfig.json", true},
		{"kept_custom_name", "//web:chosen", "chosen", "tsconfig.json", true},
		{"absolute_source", "//web:chosen", "chosen", "//web:tsconfig.json", true},
		{"raw_listed_source", "//web:tsconfig.json", "", "", true},
		{"raw_unlisted_config", "//web:compiler.json", "", "", false},
		{"custom_unlisted_source", "//web:chosen", "chosen", "compiler.json", false},
		{"target_named_like_raw_source", "//web:tsconfig.json", "tsconfig.json", "compiler.json", false},
		{"nonliteral_target_source", "//web:tsconfig.json", "tsconfig.json", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, tc := edgeRepo(t, edgeListings)
			c.RepoName = "fixture"
			tc.programs.programs["web"].Types = []explainfiles.TypeEntry{{Entry: "react", File: storeTypesReact}}
			tc.programs.programs["web"].Implicit = nil
			other := programOf(t, "schema", listingOf("schema", "schema/unrelated.ts"))
			other.Types = []explainfiles.TypeEntry{{Entry: "node", File: storeTypesNode}}
			tc.programs.record(other)
			lang := &tsLang{}
			ix := resolve.NewRuleIndex(func(*rule.Rule, string) resolve.Resolver { return lang })
			if test.configName != "" {
				file, err := rule.LoadData("web/BUILD.bazel", "web", []byte("ts_config(\n name = \""+test.configName+"\", # keep\n src = \""+test.configSource+"\",\n)\n"))
				if err != nil {
					t.Fatal(err)
				}
				if test.configSource == "" {
					file.Rules[0].SetAttr("src", &bzl.Ident{Name: "CONFIG_SOURCE"})
				}
				tc.programs.recordBuild(c, file.Pkg, file)
				ix.AddRule(c, file.Rules[0], file)
			}
			ix.Finish()
			id := &protoIdentity{Name: "plain", owner: "schema", Tsconfig: test.selected, Deps: []string{"@npm//:bufbuild_protobuf"}}
			wrapper := rule.NewRule("ts_proto_library", "plain/message_proto")
			wrapper.SetAttr("tsconfig", id.Tsconfig)
			from := label.New(c.RepoName, "schema", wrapper.Name())
			if !test.wantAmbient {
				p := tc.programs.compilerProgram(c, id.Tsconfig, id.owner)
				if err := compilerClosureError(p, from.String(), id.Tsconfig); err == nil {
					t.Fatal("unobserved selected config established an empty compiler closure")
				}
				return
			}
			want := []string{"@npm//:bufbuild_protobuf", "@npm//web:types_react"}
			for range 2 {
				resolveProtoLibrary(c, ix, wrapper, wrapper, &protoRuleImports{identity: id}, from)
				if got := wrapper.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
					t.Fatalf("deps = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestUnobservedProtoCompilerConfigCannotEraseExistingTypeInputs(t *testing.T) {
	tree := protoTree()
	tree["settings/tsconfig.json"] = `{"compilerOptions":{"types":["./ambient"]},"files":["ambient.d.ts"]}`
	tree["settings/ambient.d.ts"] = "declare const generatedAmbient: number;\n"
	tree["settings/alternate.d.ts"] = "declare const selectedAmbient: string;\n"
	tree["settings/BUILD.bazel"] += "exports_files([\"ambient.d.ts\", \"alternate.d.ts\"])\n"
	tree["schema/alternate.json"] = `{"compilerOptions":{"types":["../settings/alternate"]},"files":["../settings/alternate.d.ts"]}`
	root := writeTree(t, tree)
	if output, err := protoGazelle(t, root, "schema"); err != nil {
		t.Fatalf("observe authored compiler metadata: %v\n%s", err, output)
	}
	wrapper := onDiskRule(t, root, "schema", "ts_proto_library", "plain/common/example_common_proto")
	wantLabels(t, "selected program supplies ambient input", wrapper.AttrStrings("type_inputs"), []string{"//settings:ambient.d.ts"})
	build := strings.ReplaceAll(buildFileText(t, root, "schema"), "//settings:tsconfig", "@external//:tsconfig")
	writeFile(t, filepath.Join(root, "schema/BUILD.bazel"), build)
	before := buildFileBytes(t, root)
	for _, args := range [][]string{{"schema"}, {"-index=false", "schema"}} {
		output, err := protoGazelle(t, root, args...)
		if err == nil || !strings.Contains(output, "cannot discover its compiler closure") || !strings.Contains(output, "selected tsconfig \"@external//:tsconfig\"") {
			t.Fatalf("unobserved proto config published metadata with %v: %v\n%s", args, err, output)
		}
		if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
			t.Fatalf("rejected proto config changed BUILD files: %s", diff)
		}
	}
	for _, manual := range []string{"rule", "attributes"} {
		kept := strings.ReplaceAll(build, "ts_proto_library(\n", "# keep\nts_proto_library(\n")
		if manual == "attributes" {
			kept = strings.ReplaceAll(build, "    deps = ", "    # keep\n    deps = ")
			kept = strings.ReplaceAll(kept, "    type_inputs = ", "    # keep\n    type_inputs = ")
		}
		writeFile(t, filepath.Join(root, "schema/BUILD.bazel"), kept)
		if output, err := protoGazelle(t, root, "schema"); err != nil {
			t.Fatalf("kept %s did not retain manual proto metadata: %v\n%s", manual, err, output)
		}
		wrapper = onDiskRule(t, root, "schema", "ts_proto_library", "plain/common/example_common_proto")
		wantLabels(t, "manual proto metadata survives unobserved selected config", wrapper.AttrStrings("type_inputs"), []string{"//settings:ambient.d.ts"})
	}
	build = strings.ReplaceAll(build, "@external//:tsconfig", "//settings:tsconfig")
	file, err := rule.LoadData(filepath.Join(root, "schema/BUILD.bazel"), "schema", []byte(build))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range file.Rules {
		if r.Kind() == "ts_proto_library" {
			r.SetAttr("tsconfig", ":alternate.json")
			r.AttrComments("tsconfig").Suffix = append(r.AttrComments("tsconfig").Suffix, bzl.Comment{Token: "# keep"})
		}
	}
	writeFile(t, filepath.Join(root, "schema/BUILD.bazel"), string(file.Format()))
	for _, args := range [][]string{{"schema"}, {"-index=false", "schema"}} {
		if output, err := protoGazelle(t, root, args...); err != nil {
			t.Fatalf("observe kept wrapper configuration with %v: %v\n%s", args, err, output)
		}
		wrapper = onDiskRule(t, root, "schema", "ts_proto_library", "plain/common/example_common_proto")
		wantLabels(t, "kept wrapper config overrides identity metadata", wrapper.AttrStrings("type_inputs"), []string{"//settings:alternate.d.ts"})
		for _, identity := range []string{"plain", "json"} {
			message := onDiskRule(t, root, "schema", "ts_proto_library", identity+"/messages/example_messages_proto")
			wantLabels(t, "observed native imports retain their generated identity", message.AttrStrings("deps"), []string{"@npm//:bufbuild_protobuf", ":" + identity + "/common/example_common_proto"})
		}
	}
	build = strings.ReplaceAll(buildFileText(t, root, "schema"), ":alternate.json", "@external//:tsconfig")
	writeFile(t, filepath.Join(root, "schema/BUILD.bazel"), build)
	before = buildFileBytes(t, root)
	for _, args := range [][]string{{"schema"}, {"-index=false", "schema"}} {
		output, err := protoGazelle(t, root, args...)
		if err == nil || !strings.Contains(output, "selected tsconfig \"@external//:tsconfig\"") {
			t.Fatalf("unknown kept wrapper config reused identity metadata with %v: %v\n%s", args, err, output)
		}
		if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
			t.Fatalf("rejected kept wrapper config changed BUILD files: %s", diff)
		}
	}
}

func TestProtoAmbientSourceDependencySurvivesSeparateOwnerUpdates(t *testing.T) {
	for _, scenario := range []struct{ owned, inherited bool }{{true, false}, {false, false}, {false, true}} {
		t.Run(fmt.Sprintf("compiler_owner_%t_inherited_%t", scenario.owned, scenario.inherited), func(t *testing.T) {
			owned := scenario.owned
			tree := protoTree()
			tree["settings/tsconfig.json"] = `{"compilerOptions":{"module":"ESNext","moduleResolution":"Bundler","target":"ES2022","types":["./ambient"]},"files":["ambient.d.ts"]}`
			tree["settings/ambient.d.ts"] = "/// <reference path=\"./referenced.d.ts\" />\ndeclare const generatedAmbient: ReferencedAmbient;\n"
			tree["settings/referenced.d.ts"] = "import type { Value } from 'dependency';\ndeclare global { interface ReferencedAmbient extends Value { readonly token: unique symbol } }\n"
			tree["package.json"] = `{"private":true}`
			tree["node_modules/.modules.yaml"] = "layoutVersion: 5\n"
			tree["settings/package.json"] = `{"type":"module","dependencies":{"dependency":"1.0.0"}}`
			tree["schema/package.json"] = `{"private":true,"dependencies":{"dependency":"1.0.0"}}`
			tree["settings/node_modules/dependency/package.json"] = `{"name":"dependency","version":"1.0.0","types":"index.d.ts"}`
			tree["settings/node_modules/dependency/index.d.ts"] = "export interface Value { value: number }\n"
			tree[pnpmLockfileName] = `lockfileVersion: '9.0'
importers:
  .: {}
  settings:
    dependencies:
      dependency:
        specifier: 1.0.0
        version: 1.0.0
  schema:
    dependencies:
      dependency:
        specifier: 1.0.0
        version: 1.0.0
packages:
  dependency@1.0.0:
    resolution: {integrity: sha512-aaa}
snapshots:
  dependency@1.0.0: {}
`
			if scenario.inherited {
				for _, file := range []string{"package.json", "index.d.ts"} {
					original := "settings/node_modules/dependency/" + file
					tree["node_modules/dependency/"+file] = tree[original]
					delete(tree, original)
				}
				tree["package.json"] = tree["settings/package.json"]
				tree["settings/package.json"], tree["schema/package.json"] = `{"private":true}`, `{"private":true}`
				tree[pnpmLockfileName] = strings.Replace(tree[pnpmLockfileName], "  .: {}\n  settings:", "  .:", 1)
				tree[pnpmLockfileName] = strings.Replace(tree[pnpmLockfileName], "  schema:\n    dependencies:\n      dependency:\n        specifier: 1.0.0\n        version: 1.0.0\n", "  settings: {}\n  schema: {}\n", 1)
			}
			tree["settings/BUILD.bazel"] = `exports_files(["ambient.d.ts", "referenced.d.ts", "package.json"])`
			if owned {
				tree["settings/tsconfig.json"] = `{"compilerOptions":{"module":"ESNext","moduleResolution":"Bundler","target":"ES2022","types":["./ambient"]},"include":["contract.ts"]}`
				tree["settings/contract.ts"] = "export interface GeneratedContext { token: typeof generatedAmbient }\n"
			}
			root := writeTree(t, tree)
			var previous string
			for _, dir := range []string{"settings", "schema", "schema"} {
				output, err := protoGazelle(t, root, "-ts_verbose", dir)
				if err != nil {
					t.Fatalf("%s: %v\n%s", dir, err, output)
				}
				if dir != "schema" {
					continue
				}
				file := filepath.Join(root, "schema/BUILD.bazel")
				b, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				build, err := rule.LoadData(file, "schema", b)
				if err != nil {
					t.Fatal(err)
				}
				wrappers := 0
				for _, r := range build.Rules {
					if r.Kind() != "ts_proto_library" {
						continue
					}
					wrappers++
					if owned {
						if !slices.Contains(r.AttrStrings("deps"), "//settings") {
							t.Fatalf("%s dropped ambient owner:\n%s\n%s", r.Name(), output, b)
						}
						wantLabels(t, "ambient owner supplies its importer", r.AttrStrings("source_node_modules"), nil)
					} else {
						want := []string{"//settings:ambient.d.ts", "//settings:package.json", "//settings:referenced.d.ts"}
						if got := r.AttrStrings("type_inputs"); !slices.Equal(got, want) {
							t.Fatalf("%s dropped unowned ambient closure: type_inputs = %v, want %v\n%s\n%s", r.Name(), got, want, output, b)
						}
						wantImporters, dependency := []string{"//settings:node_modules"}, "@npm//settings:dependency"
						if scenario.inherited {
							wantImporters, dependency = nil, "@npm//:dependency"
						}
						wantLabels(t, "borrowed ambient declaration retains only a needed importer", r.AttrStrings("source_node_modules"), wantImporters)
						if !slices.Contains(r.AttrStrings("deps"), dependency) {
							t.Fatalf("%s dropped the borrowed declaration's package: %v", r.Name(), r.AttrStrings("deps"))
						}
					}
					if r.Attr("srcs") != nil {
						t.Fatalf("%s added ambient sources to the generated runtime program:\n%s", r.Name(), b)
					}
				}
				if wrappers == 0 {
					t.Fatalf("no proto wrappers generated:\n%s\n%s", output, b)
				}
				if previous != "" && previous != string(b) {
					t.Fatalf("repeat generation changed ambient closure:\n%s", b)
				}
				previous = string(b)
			}
		})
	}
}
