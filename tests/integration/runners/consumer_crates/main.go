package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mikn/rules_typescript/tests/integration/harness"
)

var rustVersion = regexp.MustCompile(`rust\.toolchain\([^)]*version = "([^"]+)"`)

func rustPin(it *harness.IT, module string) string {
	m := rustVersion.FindStringSubmatch(it.Read(module))
	if m == nil {
		it.Fail("%s pins no rust.toolchain version", module)
	}
	return m[1]
}

func main() {
	harness.Run(harness.Config{
		Name:         "consumer_crates",
		WorkspaceRel: "tests/integration/consumer_crates/workspace",
		Renames:      map[string]string{"BUILD.bazel.tpl": "BUILD.bazel"},
	}, func(it *harness.IT) {
		toolRust := rustPin(it, filepath.Join(it.RulesTSRoot, "MODULE.bazel"))
		consumerRust := rustPin(it, it.Path("MODULE.bazel"))
		if toolRust == consumerRust {
			it.Fail("the consumer pins the tools' Rust %s, so the isolation check proves nothing", toolRust)
		}
		toolDir := "_" + strings.ReplaceAll(toolRust, ".", "_") + "_rust_toolchain/"
		consumerDir := "_" + strings.ReplaceAll(consumerRust, ".", "_") + "_rust_toolchain/"

		it.MustBazel("mod", "tidy")
		it.MustBazel("build", "//:hello", "//:hello_commonjs", "//:consumer", "@rules_typescript//oj:oj", "--output_groups=+declarations,+_validation")
		it.Pass("a ts_compile builds beside the workspace's own hub named crates")

		if !strings.Contains(it.Read(it.Bin("src/commonjs.js")), "exports.first") {
			it.Fail("the prebuilt compiler did not emit CommonJS exports")
		}
		if !strings.Contains(it.Read(it.Bin("src/commonjs.d.ts")), "ReadonlyArray<string>") {
			it.Fail("the prebuilt compiler did not emit the standard-library declaration")
		}
		it.Pass("the prebuilt compiler finds its libraries during config, CommonJS emit, declaration emit and typecheck")

		var graphActions struct {
			Targets []struct {
				ID    json.Number `json:"id"`
				Label string      `json:"label"`
			} `json:"targets"`
			Actions []struct {
				TargetID  json.Number `json:"targetId"`
				Arguments []string    `json:"arguments"`
			} `json:"actions"`
		}
		query := it.BazelStdout("aquery", `mnemonic("Rustc.*", deps(set(//:consumer //:hello @rules_typescript//oj:oj)))`, "--output=jsonproto")
		if err := json.Unmarshal([]byte(query), &graphActions); err != nil {
			it.Fail("decode Rust compiler actions: %v", err)
		}
		labels := map[json.Number]string{}
		for _, target := range graphActions.Targets {
			labels[target.ID] = target.Label
		}
		consumer, tools, macros, scripts := 0, 0, 0, 0
		for _, action := range graphActions.Actions {
			label := labels[action.TargetID]
			want := ""
			if strings.HasSuffix(label, "//:consumer") {
				want = consumerDir
				consumer++
			}
			tool := strings.Contains(label, "rules_typescript_crates__") || strings.Contains(label, "oj_crates__") || strings.HasSuffix(label, "//oxc_cli:oxc-bazel_source")
			if tool {
				want = toolDir
				tools++
			}
			if want == "" {
				continue
			}
			compiler := ""
			for _, arg := range action.Arguments {
				if strings.HasSuffix(arg, "/bin/rustc") || strings.HasSuffix(arg, "/bin/rustc.exe") {
					compiler = arg
				}
			}
			if !strings.Contains(compiler, want) {
				it.Fail("%s uses compiler %q; expected %s", label, compiler, want)
			}
			for i, arg := range action.Arguments {
				if tool && (arg == "--crate-type=proc-macro" || (arg == "--crate-type" && i+1 < len(action.Arguments) && action.Arguments[i+1] == "proc-macro")) {
					macros++
				}
			}
			if tool && strings.HasSuffix(label, ":_bs_") {
				scripts++
			}
		}
		if consumer == 0 || tools == 0 || macros == 0 || scripts == 0 {
			it.Fail("missing Rust isolation evidence: consumer=%d tools=%d proc_macros=%d build_scripts=%d", consumer, tools, macros, scripts)
		}
		output := it.BazelStdout("run", "//:consumer")
		if !strings.Contains(output, "consumer ") {
			it.Fail("consumer executable did not run: %s", output)
		}
		it.MustBazel("run", "@rules_typescript//oxc_cli:oxc-bazel", "--", "--help")
		it.MustBazel("run", "@rules_typescript//oj:oj", "--", "--help")
		it.Pass("native tools keep Rust %s across exec dependencies without changing the consumer's Rust %s", toolRust, consumerRust)

		graph := it.BazelStdout("mod", "graph", "--depth=1")
		if !strings.Contains(graph, "rules_rs@0.2.1") {
			it.Fail("bazel mod graph names no rules_rs@0.2.1:\n%s", graph)
		}
		it.Pass("rules_rs is pinned by the consumer")

		hub := strings.TrimSpace(it.BazelStdout("query", "@crates//:all"))
		if !regexp.MustCompile(`crates//:cfg[-_]if`).MatchString(hub) {
			it.Fail("@crates//:all lists %q, not the workspace's cfg-if", hub)
		}
		it.Pass("@crates is the workspace's hub:\n%s", hub)
	})
}
