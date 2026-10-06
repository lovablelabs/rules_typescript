package typescript

// The fixtures and mutations convergeGazelle is driven with: the shapes the
// package model meets on a workspace, each a tsconfig.json program or more.

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

type convergeMutation struct {
	kind   string
	write  map[string]string
	extend map[string]string // appended to a file that may not exist yet
	remove []string
}

type convergeCase struct {
	name      string
	files     map[string]string
	mutations []convergeMutation
}

// ---- fixtures ---------------------------------------------------------------

func writeWorkspace(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func appendFile(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// captureLog collects what the generator writes to the standard logger, which
// is where Gazelle's own diagnostics go.
func captureLog(t *testing.T, body func()) string {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	body()
	return buf.String()
}

func contains(haystack []string, needle string) bool {
	for _, entry := range haystack {
		if entry == needle {
			return true
		}
	}
	return false
}

const (
	convergePlainPkg = `{"name":"w","dependencies":{"zod":"3.24.2"}}` + "\n"

	// The root's tsconfig.json has no include: refused, and a ts_config only
	// because src's chain extends it.
	convergeRootTsConfig = `{"compilerOptions":{"strict":true}}` + "\n"

	convergeSrcTsConfig = `{"extends":"../tsconfig.json",` +
		`"compilerOptions":{"lib":["es2022"]},"include":["**/*"]}` + "\n"

	convergeAliasTsConfig = `{"compilerOptions":{"lib":["es2022"],"paths":{` +
		`"@/*":["./src/*"],"@lib/*":["../lib/src/*"]}},` +
		`"include":["src/**/*","e2e/**/*"]}` + "\n"

	// Appended without its load line: FixLoads writes the file's one load, and
	// a second, hand-placed one would part the two trees by position alone.
	convergeWorkerTypesCodegen = `
ts_codegen(
    name = "worker_types",
    srcs = ["wrangler.jsonc"],
    outs = ["worker-configuration.d.ts"],
    args = [
        "--config",
        "wrangler.jsonc",
        "--out",
        "{out}",
    ],
    generator = "@rules_typescript//tools/codegen:wrangler_types",
)
`
)

func convergeFixture(t *testing.T, name string) convergeCase {
	t.Helper()
	for _, tc := range convergeCases() {
		if tc.name == name {
			return tc
		}
	}
	t.Fatalf("no %q fixture in convergeCases()", name)
	return convergeCase{}
}

// The root links @w/core and declares zod; @w/core declares zod too.
const convergeMemberLock = `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      '@w/core':
        specifier: workspace:*
        version: link:packages/core
      zod:
        specifier: 3.24.2
        version: 3.24.2

  packages/core:
    dependencies:
      zod:
        specifier: 3.24.2
        version: 3.24.2

  packages/app: {}

packages:

  zod@3.24.2:
    resolution: {integrity: sha512-aaa}

snapshots:

  zod@3.24.2: {}
`

func convergeCases() []convergeCase {
	return []convergeCase{
		{
			// One program under src, its tests and data files with it; the
			// root's tsconfig.json is the base the program extends.
			name: "plain",
			files: map[string]string{
				"package.json":         convergePlainPkg,
				"tsconfig.json":        convergeRootTsConfig,
				"shared/tsconfig.json": includeTs,
				"shared/util.ts":       "export const util = 1;\n",
				"src/tsconfig.json":    convergeSrcTsConfig,
				"src/vitest.config.ts": "export default { test: { globals: true } };\n",
				"src/index.ts": "export * from \"./lib/helper\";\n" +
					"export { util } from \"../shared/util\";\n",
				"src/routes/index.ts":    "export const routes = 1;\n",
				"src/routes/home.ts":     "export const home = 1;\n",
				"src/lib/helper.ts":      "export const helper = 1;\n",
				"src/lib/helper.test.ts": "export const t = 1;\n",
				"src/lib/tokens.json":    `{"a":1}` + "\n",
				"src/icons/logo.svg":     "<svg/>\n",
			},
			mutations: []convergeMutation{
				{kind: "add_colocated_module", write: map[string]string{"src/routes/home.data.ts": "export const data = 1;\n"}},
				{kind: "add_folder_route", write: map[string]string{
					"src/routes/panel/index.ts": "export * from \"./view\";\n",
					"src/routes/panel/view.ts":  "export const view = 1;\n",
				}},
				{kind: "add_flat_route", write: map[string]string{"src/routes/about.ts": "export const about = 1;\n"}},
				{kind: "add_nested_route_dir", write: map[string]string{"src/routes/admin/users/list.ts": "export const list = 1;\n"}},
				// Outside every program: nothing changes.
				{kind: "add_root_shared_file", write: map[string]string{"version.ts": "export const version = \"1\";\n"}},
				{kind: "add_shared_dir_no_ts", write: map[string]string{
					"styles/globals.css": ".a{color:red}\n",
					"data/config.json":   `{"a":1}` + "\n",
				}},
				{kind: "add_shared_dir_with_ts", write: map[string]string{"lib2/extra.ts": "export const extra = 1;\n"}},
				{kind: "add_data_file", write: map[string]string{
					"src/lib/fixture.snap": "snap\n",
				}},
				{kind: "add_file_to_existing_target", write: map[string]string{"src/lib/format.ts": "export const format = 1;\n"}},
				// The config imports a module of its package: config_srcs.
				{kind: "config_imports_a_module", write: map[string]string{
					"src/vitest.config.ts": "import { p } from \"./plugins/p\";\n" +
						"export default { plugins: [p()], test: { globals: true } };\n",
					"src/plugins/p.ts": "export const p = () => ({ name: \"p\" });\n",
				}},
				{kind: "delete_route", remove: []string{"src/routes/home.ts"}},
				{kind: "delete_data_file", remove: []string{"src/lib/tokens.json"}},
				{kind: "delete_only_sources_in_dir", remove: []string{"src/routes/index.ts", "src/routes/home.ts"}},
				{kind: "delete_the_tests", remove: []string{"src/lib/helper.test.ts"}},
			},
		},
		{
			// Imports through a tsconfig `paths` alias into another package: the
			// listing names the file and its owner is the dep.
			name: "alias_imports",
			files: map[string]string{
				"package.json":      convergePlainPkg,
				"lib/tsconfig.json": includeSrc,
				"lib/src/helper.ts": "export const helper = 1;\n",
				"lib/src/util.ts":   "export const util = 1;\n",
				"app/tsconfig.json": convergeAliasTsConfig,
				"app/src/index.ts": "import { helper } from \"@lib/helper\";\n" +
					"export const a = helper;\n",
				"app/src/main.ts": "import { own } from \"@/own\";\n" +
					"export const main = own;\n",
				"app/src/own.ts": "export const own = 1;\n",
				"app/src/main.test.ts": "import type { helper } from " +
					"\"@lib/helper\";\nexport const t = typeof helper;\n",
				"app/e2e/smoke.test.ts": "import { own } from \"@/own\";\n" +
					"export const t = own;\n",
			},
			mutations: []convergeMutation{
				// An import through the second alias: the run recomputes deps.
				{kind: "add_alias_import", write: map[string]string{
					"app/src/extra.ts": "import { util } from \"@lib/util\";\n" +
						"export const b = util;\n",
				}},
				// The library's last aliased import goes; the test keeps its own.
				{kind: "drop_alias_import", write: map[string]string{
					"app/src/index.ts": "export const a = 1;\n",
				}},
				{kind: "add_alias_import_in_new_dir", write: map[string]string{
					"app/src/panel/view.ts": "import { util } from " +
						"\"@lib/util\";\nexport const v = util;\n",
				}},
				{kind: "add_file_to_existing_target", write: map[string]string{
					"app/src/format.ts": "export const format = 1;\n",
				}},
				{kind: "delete_alias_importing_file", remove: []string{"app/src/index.ts"}},
			},
		},
		{
			// The pnpm workspace-member shape, sources one directory down; a
			// member without a tsconfig.json is no package.
			name: "pnpm_member",
			files: map[string]string{
				"package.json":                   convergePlainPkg,
				"pnpm-workspace.yaml":            "packages:\n  - packages/*\n",
				"pnpm-lock.yaml":                 convergeMemberLock,
				"node_modules/.modules.yaml":     "hoistPattern:\n  - '*'\n",
				"tsconfig.json":                  convergeRootTsConfig,
				"packages/core/package.json":     `{"name":"@w/core","version":"1.0.0"}` + "\n",
				"packages/core/tsconfig.json":    `{"compilerOptions":{"lib":["es2022"]}}` + "\n",
				"packages/core/src/index.ts":     "export * from \"./util\";\n",
				"packages/core/src/util.ts":      "export const util = 1;\n",
				"packages/core/src/util.test.ts": "export const t = 1;\n",
				"packages/app/package.json":      `{"name":"@w/app","version":"1.0.0"}` + "\n",
				"packages/app/src/main.ts":       "export const main = 1;\n",
			},
			mutations: []convergeMutation{
				{kind: "add_file_to_existing_target", write: map[string]string{"packages/core/src/format.ts": "export const format = 1;\n"}},
				{kind: "add_member_with_tsconfig", write: map[string]string{
					"packages/ui/package.json":  `{"name":"@w/ui","version":"1.0.0"}` + "\n",
					"packages/ui/tsconfig.json": `{"compilerOptions":{"jsx":"react-jsx"}}` + "\n",
					"packages/ui/src/button.ts": "export const button = 1;\n",
				}},
				{kind: "add_jsx_member", write: map[string]string{
					"packages/icons/package.json":  `{"name":"@w/icons","version":"1.0.0"}` + "\n",
					"packages/icons/tsconfig.json": `{"compilerOptions":{"jsx":"react-jsx","jsxImportSource":"preact"}}` + "\n",
					"packages/icons/src/icon.tsx":  "export const icon: string = <div />;\n",
				}},
				{kind: "add_tsconfig_to_member", write: map[string]string{
					"packages/app/tsconfig.json": `{"compilerOptions":{"strict":false}}` + "\n",
				}},
				{kind: "delete_member_tsconfig", remove: []string{"packages/core/tsconfig.json"}},
				{kind: "add_nested_source_dir", write: map[string]string{"packages/core/src/nested/leaf.ts": "export const leaf = 1;\n"}},
			},
		},
		{
			// A worker: a program over its sources and the declaration its
			// tsconfig names, a test program extending it, a vitest config.
			name: "worker",
			files: map[string]string{
				"package.json":        `{"name":"w"}` + "\n",
				"worker/package.json": `{"name":"worker"}` + "\n",
				"worker/vitest.config.mts": "export default { test: " +
					"{ globals: true } };\n",
				"worker/tsconfig.json": `{"compilerOptions":{"lib":["es2022"],` +
					`"types":["./worker-configuration.d.ts"]},` +
					`"include":["src/**/*","worker-configuration.d.ts"]}` + "\n",
				"worker/worker-configuration.d.ts": "declare const WORKER_ENV: string;\n",
				"worker/wrangler.jsonc":            `{"name":"w","main":"src/handler.ts"}` + "\n",
				"worker/src/handler.ts":            "export const env = WORKER_ENV;\n",
				"worker/test/tsconfig.json": `{"extends":"../tsconfig.json",` +
					`"compilerOptions":{"types":["../worker-configuration.d.ts"]},` +
					`"include":["*.ts"]}` + "\n",
				"worker/test/handler.test.ts": "export const t = WORKER_ENV;\n",
			},
			mutations: []convergeMutation{
				{
					kind:   "replace_declaration_with_codegen",
					remove: []string{"worker/worker-configuration.d.ts"},
					extend: map[string]string{"worker/BUILD.bazel": convergeWorkerTypesCodegen},
				},
				{kind: "add_a_test_beside_the_sources", write: map[string]string{
					"worker/src/handler.test.ts": "export const t = WORKER_ENV;\n",
				}},
			},
		},
	}
}

// ---- the property ----------------------------------------------------------

// For every fixture workspace: generate, mutate, generate again, and require
// what one generation over the mutated tree produces.
func TestConvergeAfterMutation(t *testing.T) {
	requireTsgo(t)
	for _, tc := range convergeCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, mut := range tc.mutations {
				t.Run(mut.kind, func(t *testing.T) { runConvergeCase(t, tc, mut) })
			}
		})
	}
}

func runConvergeCase(t *testing.T, tc convergeCase, mut convergeMutation) {
	t.Helper()

	scratch := t.TempDir()
	writeWorkspace(t, scratch, tc.files)
	applyMutation(t, scratch, mut)
	captureLog(t, func() { convergeGazelle(t, scratch) })
	want := convergeSnapshot(t, scratch)

	twoRun := t.TempDir()
	writeWorkspace(t, twoRun, tc.files)
	captureLog(t, func() { convergeGazelle(t, twoRun) })
	applyMutation(t, twoRun, mut)
	logged := captureLog(t, func() { convergeGazelle(t, twoRun) })
	got := convergeSnapshot(t, twoRun)

	diff := snapshotDiff(want, got)
	switch {
	case snapshotDiff(sortedLists(want), sortedLists(got)) != "":
		t.Errorf("%s/%s does not converge.\n"+
			"  \"-\" is one generation over the mutated tree; \"+\" is generate, mutate, generate.\n%s\n"+
			"second run said:\n%s",
			tc.name, mut.kind, diff, indentLog(logged))
	case diff != "":
		t.Errorf("%s/%s converges on the same targets in a different attribute order, so a "+
			"from-scratch checkout and an incremental one disagree:\n%s", tc.name, mut.kind, diff)
	}

	if dangling := danglingLabels(t, twoRun); len(dangling) > 0 {
		t.Errorf("%s/%s left %d label(s) no target satisfies, which fails analysis for the whole workspace:\n      %s",
			tc.name, mut.kind, len(dangling), strings.Join(dangling, "\n      "))
	}
	if crossing := crossesPackageBoundary(t, twoRun); len(crossing) > 0 {
		t.Errorf("%s/%s left %d src(s) under another package, which fails "+
			"analysis for the whole workspace:\n      %s",
			tc.name, mut.kind, len(crossing), strings.Join(crossing, "\n      "))
	}
}

func applyMutation(t *testing.T, root string, mut convergeMutation) {
	t.Helper()
	if len(mut.write) > 0 {
		writeWorkspace(t, root, mut.write)
	}
	for rel, body := range mut.extend {
		appendFile(t, root, rel, body)
	}
	for _, rel := range mut.remove {
		if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
}

func indentLog(logged string) string {
	logged = strings.TrimRight(logged, "\n")
	if logged == "" {
		return "      (nothing)"
	}
	var b strings.Builder
	for _, line := range strings.Split(logged, "\n") {
		fmt.Fprintf(&b, "      %s\n", line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- hand-authored values in Gazelle-managed attributes ---------------------

// The generators recompute these attributes from the tree on every run, so a
// value a user writes into one survives only with a "# keep" -- and a run that
// drops one has to name it. Every case above starts from a generated tree and
// mutates sources; none of them authors a value, which is why the deletion went
// unseen.
type handAuthoredCase struct {
	workspace string
	extra     map[string]string
	drop      []string

	pkg    string
	kind   string
	target string
	attr   string
	shape  string // "list" or "scalar"
	value  string
}

const handVendorPackage = `filegroup(
    name = "vendor_hand",
    srcs = ["legacy.js"],
    visibility = ["//visibility:public"],
)
`

func handAuthoredCases() []handAuthoredCase {
	return []handAuthoredCase{
		{
			workspace: "plain", pkg: "src", kind: "ts_compile", target: "src",
			attr: "srcs", shape: "list", value: "legacy.js",
			extra: map[string]string{"src/legacy.js": "export const legacy = 1;\n"},
		},
		{
			workspace: "plain", pkg: "src", kind: "ts_compile", target: "src",
			attr: "visibility", shape: "list", value: "//vendor:__pkg__",
			extra: map[string]string{
				"vendor/BUILD.bazel": handVendorPackage,
				"vendor/legacy.js":   "export const legacy = 1;\n",
			},
		},
		{
			workspace: "plain", pkg: "src", kind: "ts_compile", target: "src",
			attr: "tsconfig", shape: "scalar", value: "//:tsconfig_build",
			extra: map[string]string{
				"BUILD.bazel": loadDefs + "\"ts_config\")\nts_config(name = \"tsconfig_build\", src = \"build.json\")\n",
				"build.json":  `{"extends":"./tsconfig.json","include":["src/**/*"]}`,
			},
		},
	}
}

// TestHandAuthoredAttrValue pins both halves of the contract: a value carrying
// "# keep" survives the next run, and one without it is replaced out loud.
func TestHandAuthoredAttrValue(t *testing.T) {
	requireTsgo(t)
	fixtures := map[string]convergeCase{}
	for _, tc := range convergeCases() {
		fixtures[tc.name] = tc
	}

	for _, hc := range handAuthoredCases() {
		tc, ok := fixtures[hc.workspace]
		if !ok {
			t.Fatalf("no %q fixture for %s(%s).%s", hc.workspace, hc.kind, hc.target, hc.attr)
		}
		name := fmt.Sprintf("%s/%s.%s", hc.workspace, hc.kind, hc.attr)

		// Twice: a run that keeps the value but loses the "# keep" that held it
		// has only moved the deletion one run out.
		t.Run(name+"/keep", func(t *testing.T) {
			root, logged := handAuthoredRun(t, tc, hc, true)
			for run := 2; run <= 3; run++ {
				if !contains(declaredAttrValues(t, root, hc), hc.value) {
					t.Fatalf("%s(%s).%s lost the hand-authored %q on run %d even though it "+
						"carries a \"# keep\", so a declared build input disappeared:\n%s\nthe run "+
						"said:\n%s", hc.kind, hc.target, hc.attr, hc.value, run,
						indent(buildFileText(t, root, hc.pkg)), indentLog(logged))
				}
				logged = captureLog(t, func() { convergeGazelle(t, root) })
			}
		})

		t.Run(name+"/replaced", func(t *testing.T) {
			root, logged := handAuthoredRun(t, tc, hc, false)
			switch {
			case contains(declaredAttrValues(t, root, hc), hc.value):
				t.Errorf("%s(%s).%s kept the hand-authored %q with no \"# keep\". Gazelle owns "+
					"the attribute, so either the generator merges and the docs say so, or it "+
					"replaces and this case is wrong.",
					hc.kind, hc.target, hc.attr, hc.value)
			case !strings.Contains(logged, hc.value):
				t.Errorf("%s(%s).%s dropped the hand-authored %q and said nothing about it. A "+
					"declared build input disappearing with no diagnostic is the defect this "+
					"generator exists to remove.\nthe run said:\n%s",
					hc.kind, hc.target, hc.attr, hc.value, indentLog(logged))
			}
		})
	}
}

func handAuthoredRun(t *testing.T, tc convergeCase, hc handAuthoredCase, keep bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{}
	for rel, body := range tc.files {
		files[rel] = body
	}
	for rel, body := range hc.extra {
		files[rel] = body
	}
	for _, rel := range hc.drop {
		delete(files, rel)
	}
	writeWorkspace(t, root, files)
	captureLog(t, func() { convergeGazelle(t, root) })
	handAuthorAttr(t, root, hc, keep)
	logged := captureLog(t, func() { convergeGazelle(t, root) })
	return root, logged
}

// handAuthorAttr edits the generated BUILD file the way a user would: the value
// joins a list or replaces a scalar, marked "# keep" on its own element line
// and, for a scalar, on the line above the attribute.
func handAuthorAttr(t *testing.T, root string, hc handAuthoredCase, keep bool) {
	t.Helper()
	buildPath := filepath.Join(root, filepath.FromSlash(hc.pkg), "BUILD.bazel")
	data, err := os.ReadFile(buildPath)
	if err != nil {
		t.Fatal(err)
	}

	var target *rule.Rule
	for _, r := range loadRules(t, root, hc.pkg) {
		if r.Kind() == hc.kind && r.Name() == hc.target {
			target = r
		}
	}
	if target == nil {
		t.Fatalf("generation wrote no %s(%s) in %s, so there is nothing to hand-edit:\n%s",
			hc.kind, hc.target, buildPath, data)
	}

	const indent = "    "
	var replacement []string
	switch hc.shape {
	case "scalar":
		if keep {
			replacement = append(replacement, indent+"# keep")
		}
		replacement = append(replacement, fmt.Sprintf("%s%s = %q,", indent, hc.attr, hc.value))
	default:
		replacement = []string{
			indent + hc.attr + " = [",
			handListLines(append(attrValues(target, hc.attr), hc.value), hc.value, keep, indent),
			indent + "],",
		}
	}

	blocks := strings.Split(string(data), "\n\n")
	edited := false
	for i, block := range blocks {
		if !strings.Contains(block, hc.kind+"(") || !strings.Contains(block, fmt.Sprintf("name = %q", hc.target)) {
			continue
		}
		lines := replaceAttrLines(strings.Split(block, "\n"), hc.attr, replacement)
		if lines == nil {
			t.Fatalf("could not place %s in the %s(%s) block:\n%s", hc.attr, hc.kind, hc.target, block)
		}
		blocks[i] = strings.Join(lines, "\n")
		edited = true
		break
	}
	if !edited {
		t.Fatalf("no %s(%s) block in %s:\n%s", hc.kind, hc.target, buildPath, data)
	}
	if err := os.WriteFile(buildPath, []byte(strings.Join(blocks, "\n\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// replaceAttrLines swaps the attribute's whole assignment -- however many lines
// it spans -- for replacement, or inserts it after name when it is absent.
func replaceAttrLines(lines []string, attr string, replacement []string) []string {
	splice := func(at, through int) []string {
		out := append([]string(nil), lines[:at]...)
		out = append(out, replacement...)
		return append(out, lines[through+1:]...)
	}
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), attr+" = ") {
			continue
		}
		depth := 0
		for j := i; j < len(lines); j++ {
			// Braces too, so a dict value ends where its brace closes.
			depth += strings.Count(lines[j], "[") + strings.Count(lines[j], "(") +
				strings.Count(lines[j], "{")
			depth -= strings.Count(lines[j], "]") + strings.Count(lines[j], ")") +
				strings.Count(lines[j], "}")
			if depth <= 0 {
				return splice(i, j)
			}
		}
		return nil
	}
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "name = ") {
			return splice(i+1, i)
		}
	}
	return nil
}

func handListLines(values []string, marked string, keep bool, indent string) string {
	var b strings.Builder
	for i, v := range values {
		suffix := ""
		if keep && v == marked {
			suffix = "  # keep"
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s    %q,%s", indent, v, suffix)
	}
	return b.String()
}

func declaredAttrValues(t *testing.T, root string, hc handAuthoredCase) []string {
	t.Helper()
	for _, r := range loadRules(t, root, hc.pkg) {
		if r.Kind() != hc.kind || r.Name() != hc.target {
			continue
		}
		return attrValues(r, hc.attr)
	}
	return nil
}

func buildFileText(t *testing.T, root, pkg string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pkg), "BUILD.bazel"))
	if err != nil {
		return "(no BUILD file)"
	}
	return string(data)
}

// ---- a tsconfig `types` entry ----------------------------------------------

// A path-shaped `types` entry is a dep on the target staging the file: the
// ts_compile holding a checked-in declaration, or the ts_codegen writing it.
func TestTypesEntryIsADepOnTheTargetStagingIt(t *testing.T) {
	requireTsgo(t)
	fixture := convergeFixture(t, "worker")
	var mutation convergeMutation
	for _, mut := range fixture.mutations {
		if mut.kind == "replace_declaration_with_codegen" {
			mutation = mut
		}
	}
	if mutation.kind == "" {
		t.Fatal("the worker fixture lost its replace_declaration_with_codegen mutation")
	}

	// The test program names the declaration the worker's package owns.
	checkedIn := t.TempDir()
	writeWorkspace(t, checkedIn, fixture.files)
	captureLog(t, func() { convergeGazelle(t, checkedIn) })
	r := onlyRuleOfKind(t, checkedIn, "worker/test", "ts_test")
	if deps := r.AttrStrings("deps"); !contains(deps, "//worker") {
		t.Errorf("%s(%s) in //worker/test has deps = %v, want //worker: the "+
			"target holding worker-configuration.d.ts is what stages the "+
			"declaration the tsconfig names in `types`:\n%s",
			r.Kind(), r.Name(), deps,
			indent(buildFileText(t, checkedIn, "worker/test")))
	}
	wantLabels(t, "config retains its module scope", r.AttrStrings("config_srcs"), []string{"//worker:package.json"})
	wantLabels(t, "compiler dependency supplies the test scope", r.AttrStrings("package_scopes"), nil)
	requireNoTsConfigTypesFilegroup(t, checkedIn)

	// Both programs name a declaration only the codegen writes.
	generated := t.TempDir()
	writeWorkspace(t, generated, fixture.files)
	applyMutation(t, generated, mutation)
	captureLog(t, func() { convergeGazelle(t, generated) })
	for _, program := range []struct{ pkg, kind, want string }{
		{"worker", "ts_compile", ":worker_types"},
		{"worker/test", "ts_test", "//worker:worker_types"},
	} {
		r := onlyRuleOfKind(t, generated, program.pkg, program.kind)
		if deps := r.AttrStrings("deps"); !contains(deps, program.want) {
			t.Errorf("%s(%s) in //%s has deps = %v, want %s: the codegen is what "+
				"stages the declaration the tsconfig names in `types`:\n%s",
				r.Kind(), r.Name(), program.pkg, deps, program.want,
				indent(buildFileText(t, generated, program.pkg)))
		}
		if program.kind == "ts_test" {
			wantLabels(t, "config retains its module scope", r.AttrStrings("config_srcs"), []string{"//worker:package.json"})
			wantLabels(t, "codegen dependency leaves the test scope explicit", r.AttrStrings("package_scopes"), []string{"//worker:package.json"})
		}
	}
	requireNoTsConfigTypesFilegroup(t, generated)
	if dangling := danglingLabels(t, generated); len(dangling) > 0 {
		t.Errorf("the codegen dep left %d label(s) no target satisfies:\n      %s", len(dangling), strings.Join(dangling, "\n      "))
	}
	if crossing := crossesPackageBoundary(t, generated); len(crossing) > 0 {
		t.Errorf("%d src(s) sit under another package:\n      %s",
			len(crossing), strings.Join(crossing, "\n      "))
	}

	for _, selected := range []struct{ src, config string }{
		{"tsconfig.build.json", fixture.files["worker/test/tsconfig.json"]},
		{"config/tsconfig.build.json", `{"extends":"../../tsconfig.json","compilerOptions":{"types":["../../worker-configuration.d.ts"]},"include":["../*.ts"]}`},
	} {
		t.Run("selected_config_keeps_missing_ancestor_codegen/"+selected.src, func(t *testing.T) {
			root := t.TempDir()
			writeWorkspace(t, root, fixture.files)
			applyMutation(t, root, mutation)
			writeWorkspace(t, root, map[string]string{
				"worker/test/" + selected.src: selected.config,
				"worker/test/BUILD.bazel": fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_config")
ts_config(
    name = "tsconfig",
    src = %q, # keep
)
`, selected.src),
			})
			var before map[string]string
			for _, wrapper := range []string{
				`{"files":[],"include":[],"references":[{"path":"./.bazel/tsconfig/test.json"}]}`,
				fmt.Sprintf(`{"extends":%q,"compilerOptions":{"types":[]},"files":[],"include":[],"references":[{"path":"./.bazel/tsconfig/test.json"}]}`, "./"+selected.src),
			} {
				writeWorkspace(t, root, map[string]string{"worker/test/tsconfig.json": wrapper})
				captureLog(t, func() { convergeGazelle(t, root) })
				r := onlyRuleOfKind(t, root, "worker/test", "ts_test")
				if deps := r.AttrStrings("deps"); !contains(deps, "//worker:worker_types") {
					t.Fatalf("selected config %s lost its missing ancestor codegen dependency: deps = %v, wrapper = %s",
						selected.src, deps, wrapper)
				}
				if config := onlyRuleOfKind(t, root, "worker/test", "ts_config"); config.AttrString("src") != selected.src {
					t.Fatalf("selected config src = %q, want %q", config.AttrString("src"), selected.src)
				}
				if before == nil {
					before = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("solution wrapper changed the generated targets:\n%s", diff)
				}
				captureLog(t, func() { convergeGazelle(t, root) })
				if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("selected config targets changed on the second run:\n%s", diff)
				}
			}
		})
	}
}

// Failed analysis in gazelle_roundtrip: the authored scope staged over //pooled's rewritten package.json.
func TestConfigScopeFollowsAnEmittedPublisher(t *testing.T) {
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, convergeFixture(t, "worker").files)
	captureLog(t, func() { convergeGazelle(t, root) })
	build := filepath.Join(root, "worker", "BUILD.bazel")
	f, err := rule.LoadFile(build, "worker")
	if err != nil {
		t.Fatal(err)
	}
	emitted := false
	for _, r := range f.Rules {
		if r.Kind() == "ts_compile" && r.Name() == "worker" {
			r.SetAttr("emit", true)
			emitted = true
		}
	}
	if !emitted {
		t.Fatalf("no ts_compile(worker) to emit:\n%s", indent(buildFileText(t, root, "worker")))
	}
	writeFile(t, build, strings.Replace(string(f.Format()), "emit = True,", "emit = True,  # keep", 1))
	captureLog(t, func() { convergeGazelle(t, root) })
	r := onlyRuleOfKind(t, root, "worker/test", "ts_test")
	wantLabels(t, "the emitted publisher owns the config scope", r.AttrStrings("config_srcs"), nil)
	if deps := r.AttrStrings("deps"); !contains(deps, "//worker") {
		t.Errorf("deps = %v, want the scope publisher //worker:\n%s", deps, indent(buildFileText(t, root, "worker/test")))
	}
}

func onlyRuleOfKind(t *testing.T, root, pkg, kind string) *rule.Rule {
	t.Helper()
	var found []*rule.Rule
	for _, r := range loadRules(t, root, pkg) {
		if r.Kind() == kind {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("//%s holds %d %s rules, want one:\n%s", pkg, len(found), kind, indent(buildFileText(t, root, pkg)))
	}
	return found[0]
}

func requireNoTsConfigTypesFilegroup(t *testing.T, root string) {
	t.Helper()
	for _, pkg := range convergePackages(t, root) {
		if ruleNamed(loadRules(t, root, pkg), "filegroup", "tsconfig_types") != nil {
			t.Errorf("//%s writes filegroup(tsconfig_types); a program reaches a checked-in declaration through the target that owns it and a generated one through the codegen:\n%s",
				pkg, indent(buildFileText(t, root, pkg)))
		}
	}
}

func TestEditorRefreshDoesNotChangeDiscoveredInputs(t *testing.T) {
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		"tsconfig.json":                       `{"files":["index.ts"]}`,
		"index.ts":                            "export const root = 1;\n",
		"pkg/tsconfig.json":                   `{"files":["index.ts"]}`,
		"pkg/index.ts":                        "export const nested = 1;\n",
		"pkg/.bazel/other.json":               `{}`,
		"pkg/.bazel/tsconfig-other/data.json": `{}`,
		"pkg/.hidden/data.json":               `{}`,
	})
	convergeGazelle(t, root)
	before := convergeSnapshot(t, root)
	for _, name := range []string{".bazel/other.json", ".bazel/tsconfig-other/data.json", ".hidden/data.json"} {
		if !strings.Contains(before["pkg/BUILD.bazel"], name) {
			t.Fatalf("unrelated authored data missing before refresh: %s", name)
		}
	}
	writeWorkspace(t, root, map[string]string{
		".bazel/tsconfig/root_test.json":       `{"files":["../../index.ts"]}`,
		"pkg/.bazel/tsconfig/pkg_test.json":    `{"files":["../../index.ts"]}`,
		"pkg/.bazel/tsconfig/nested/data.json": `{}`,
	})
	convergeGazelle(t, root)
	if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
		t.Fatalf("editor refresh changed discovered inputs:\n%s", diff)
	}
	writeWorkspace(t, root, map[string]string{
		"pkg/.bazel/tsconfig/tsconfig.json": `{"extends":"./missing-authored-config.json"}`,
	})
	convergeGazelle(t, root)
	if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
		t.Fatalf("editor cache config changed discovered inputs:\n%s", diff)
	}
	for _, dir := range []string{".bazel/tsconfig", "pkg/.bazel/tsconfig"} {
		if err := os.RemoveAll(filepath.Join(root, dir)); err != nil {
			t.Fatal(err)
		}
	}
	convergeGazelle(t, root)
	if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
		t.Fatalf("editor cache deletion changed discovered inputs:\n%s", diff)
	}
}

func TestEquivalentKeptConfigLabelsPreserveCompileInputs(t *testing.T) {
	requireTsgo(t)
	for _, filename := range []string{"tsconfig.json", "tsconfig.build.json"} {
		for _, prefix := range []string{"", ":", "//app:", "@//app:"} {
			for _, keep := range []string{"attribute", "rule"} {
				src := prefix + filename
				t.Run(filename+"/"+src+"/"+keep, func(t *testing.T) {
					attrKeep, ruleKeep := "", ""
					if keep == "attribute" {
						attrKeep = " # keep"
					} else {
						ruleKeep = " # keep"
					}
					files := map[string]string{
						"app/tsconfig.json": `{"files":[],"references":[]}`,
						"app/input.ts":      "import { value } from '../lib/value';\nexport const result = value;\n",
						"app/ignored.ts":    "export const ignored = 1;\n",
						"lib/tsconfig.json": `{"files":["value.ts"]}`,
						"lib/value.ts":      "export const value = 1;\n",
						"app/BUILD.bazel": fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_config")
ts_config(
    name = "tsconfig",
    src = %q,%s
)%s
`, src, attrKeep, ruleKeep),
					}
					files["app/"+filename] = `{"files":["input.ts"]}`
					root := writeTree(t, files)
					convergeGazelle(t, root)
					config := onlyRuleOfKind(t, root, "app", "ts_config")
					if got := config.AttrString("src"); got != src {
						t.Fatalf("kept config src = %q, want %q", got, src)
					}
					compiled := onlyRuleOfKind(t, root, "app", "ts_compile")
					wantStrings(t, "selected program inputs", compiled.AttrStrings("srcs"), []string{"input.ts"})
					wantStrings(t, "selected program dependencies", compiled.AttrStrings("deps"), []string{"//lib"})
					if got := compiled.AttrString("tsconfig"); got != ":tsconfig" {
						t.Fatalf("compile tsconfig = %q, want :tsconfig", got)
					}
					before := convergeSnapshot(t, root)
					convergeGazelle(t, root)
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("equivalent kept label changed on the second run:\n%s", diff)
					}
				})
			}
		}
	}
}

func TestKeptConfigReferencesPreserveCompileTargets(t *testing.T) {
	requireTsgo(t)
	for _, src := range []string{"//config:baseline.json", "//config:baseline", "@settings//:baseline.json", ":baseline"} {
		t.Run(src, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"app/tsconfig.json":    `{"files":["input.ts"]}`,
				"app/input.ts":         "export const value = 1;\n",
				"config/baseline.json": `{"compilerOptions":{"strict":true}}`,
				"config/BUILD.bazel": `exports_files(["baseline.json"], visibility = ["//visibility:public"])
filegroup(name = "baseline", srcs = ["baseline.json"], visibility = ["//visibility:public"])
`,
				"app/BUILD.bazel": fmt.Sprintf(`load("@rules_typescript//ts:defs.bzl", "ts_config")
ts_config(
    name = "tsconfig",
    src = %q,
) # keep
filegroup(name = "baseline", srcs = ["tsconfig.json"])
`, src),
			})
			convergeGazelle(t, root)
			config := onlyRuleOfKind(t, root, "app", "ts_config")
			if got := config.AttrString("src"); got != src {
				t.Fatalf("kept config src = %q, want %q", got, src)
			}
			compiled := onlyRuleOfKind(t, root, "app", "ts_compile")
			wantStrings(t, "authored program inputs", compiled.AttrStrings("srcs"), []string{"input.ts"})
			if got := compiled.AttrString("tsconfig"); got != ":tsconfig" {
				t.Fatalf("compile tsconfig = %q, want :tsconfig", got)
			}
			if dangling := danglingLabels(t, root); len(dangling) != 0 {
				t.Fatalf("kept config has dangling labels: %v", dangling)
			}
			before := convergeSnapshot(t, root)
			convergeGazelle(t, root)
			if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
				t.Fatalf("kept config reference changed on the second run:\n%s", diff)
			}
		})
	}
}

func TestNestedSelectedBaseKeepsConfigChainAcrossEditorRefresh(t *testing.T) {
	requireTsgo(t)
	for _, selected := range []struct{ base, filename string }{
		{"abase", "tsconfig.build.json"},
		{"abase", "tsconfig.json"},
		{"zbase", "tsconfig.build.json"},
		{"zbase", "tsconfig.json"},
	} {
		base, filename := selected.base, selected.filename
		t.Run(base+"/"+filename, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"tsconfig.json": `{"compilerOptions":{"strict":true},"files":["root.ts"]}`,
				"root.ts":       "export const root = 1;\n",
				"app/BUILD.bazel": loadDefs + `"ts_config")
ts_config(
    name = "tsconfig",
    src = "tsconfig.build.json", # keep
)
`,
				"app/tsconfig.build.json": fmt.Sprintf(`{"extends":"../%s/config/%s","files":["input.ts"]}`, base, filename),
				"app/tsconfig.json":       `{"files":[],"include":[],"references":[]}`,
				"app/input.ts":            fmt.Sprintf("import { value } from '../%s/value';\nexport const result = value;\n", base),
				base + "/BUILD.bazel": fmt.Sprintf(loadDefs+`"ts_config")
ts_config(
    name = "tsconfig",
    src = "config/%s", # keep
)
`, filename),
				base + "/config/" + filename:   `{"extends":"../../tsconfig.json","files":["../value.ts"]}`,
				base + "/tsconfig.json":        `{"files":[],"include":[],"references":[]}`,
				base + "/value.ts":             "export const value = 1;\n",
				base + "/nested/tsconfig.json": fmt.Sprintf(`{"extends":"../config/%s","files":["input.ts"]}`, filename),
				base + "/nested/input.ts":      "export const nested = 1;\n",
			})
			var before map[string]string
			for _, step := range []struct {
				name  string
				files map[string]string
			}{
				{name: "authored"},
				{
					name: "app_editor_refresh",
					files: map[string]string{
						"app/tsconfig.json":            `{"_comment":"bazel run //:refresh_tsconfig","extends":"./tsconfig.build.json","compilerOptions":{"types":[]},"files":[],"include":[],"references":[{"path":"./.bazel/tsconfig/app.json"}]}`,
						"app/.bazel/tsconfig/app.json": `{"files":["../../input.ts"]}`,
					},
				},
				{
					name: "base_editor_refresh",
					files: map[string]string{
						base + "/tsconfig.json":             fmt.Sprintf(`{"_comment":"bazel run //:refresh_tsconfig","extends":"./config/%s","compilerOptions":{"strict":false},"files":[],"include":[],"references":[{"path":"./.bazel/tsconfig/base.json"}]}`, filename),
						base + "/.bazel/tsconfig/base.json": `{"files":["../../value.ts"]}`,
					},
				},
			} {
				t.Run(step.name, func(t *testing.T) {
					writeWorkspace(t, root, step.files)
					convergeGazelle(t, root)
					for _, config := range []struct {
						pkg, src string
						deps     []string
					}{
						{"app", "tsconfig.build.json", []string{"//" + base + ":tsconfig"}},
						{base, "config/" + filename, []string{"//:tsconfig"}},
						{base + "/nested", "tsconfig.json", []string{"//" + base + ":tsconfig"}},
						{"", "tsconfig.json", nil},
					} {
						r := onlyRuleOfKind(t, root, config.pkg, "ts_config")
						if got := r.AttrString("src"); got != config.src {
							t.Fatalf("//%s:tsconfig source = %q, want %q", config.pkg, got, config.src)
						}
						wantStrings(t, config.pkg+" config chain", r.AttrStrings("deps"), config.deps)
					}
					compiled := onlyRuleOfKind(t, root, "app", "ts_compile")
					wantStrings(t, "selected program inputs", compiled.AttrStrings("srcs"), []string{"input.ts"})
					wantStrings(t, "selected program dependencies", compiled.AttrStrings("deps"), []string{"//" + base})
					if got := compiled.AttrString("tsconfig"); got != ":tsconfig" {
						t.Fatalf("compile tsconfig = %q, want :tsconfig", got)
					}
					nested := onlyRuleOfKind(t, root, base+"/nested", "ts_compile")
					wantStrings(t, "independent nested program inputs", nested.AttrStrings("srcs"), []string{"input.ts"})
					if dangling := danglingLabels(t, root); len(dangling) != 0 {
						t.Fatalf("config chain has dangling labels: %v", dangling)
					}
					if before == nil {
						before = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("editor refresh changed the generated graph:\n%s", diff)
					}
					convergeGazelle(t, root)
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("config chain changed on the second run:\n%s", diff)
					}
					for _, name := range []string{"BUILD.bazel", "BUILD"} {
						if _, err := os.Stat(filepath.Join(root, base, "config", name)); !os.IsNotExist(err) {
							t.Fatalf("selected config gained a child BUILD boundary %s: %v", name, err)
						}
					}
				})
			}
		})
	}
}

func TestNestedSelectedConfigSiblingBasePreservesPackageBoundary(t *testing.T) {
	requireTsgo(t)
	for _, explicitBase := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_base_%t", explicitBase), func(t *testing.T) {
			deps, base := "", ""
			if explicitBase {
				deps = `    deps = [":base"], # keep
`
				base = `ts_config(name = "base", src = "config/tsconfig.json") # keep
`
			}
			root := writeTree(t, map[string]string{
				"app/BUILD.bazel": loadDefs + `"ts_config")
ts_config(
    name = "tsconfig",
    src = "config/build.json", # keep
` + deps + `)
` + base,
				"app/config/build.json":    `{"extends":"./tsconfig.json","files":["../input.ts"]}`,
				"app/config/tsconfig.json": `{"compilerOptions":{"strict":true},"files":[],"include":[]}`,
				"app/input.ts":             "export const value: string = 'value';\n",
			})
			var before map[string]string
			for run := range 2 {
				logged := captureLog(t, func() { convergeGazelle(t, root) })
				for _, name := range []string{"BUILD.bazel", "BUILD"} {
					if _, err := os.Stat(filepath.Join(root, "app/config", name)); !os.IsNotExist(err) {
						t.Fatalf("run %d split the kept source label with app/config/%s: %v", run+1, name, err)
					}
				}
				selected := ruleNamed(loadRules(t, root, "app"), "ts_config", "tsconfig")
				if selected == nil || selected.AttrString("src") != "config/build.json" {
					t.Fatalf("run %d lost the ancestor-owned selected config", run+1)
				}
				var wantDeps []string
				if explicitBase {
					wantDeps = []string{":base"}
					base := ruleNamed(loadRules(t, root, "app"), "ts_config", "base")
					if base == nil || base.AttrString("src") != "config/tsconfig.json" {
						t.Fatalf("run %d lost the explicit base contract", run+1)
					}
				} else {
					for _, required := range []string{"app/config/build.json", "explicit ts_config", "existing BUILD package", "//app:tsconfig", "# keep"} {
						if !strings.Contains(logged, required) {
							t.Errorf("missing explicit base guidance %q:\n%s", required, logged)
						}
					}
				}
				wantStrings(t, "selected config dependencies", selected.AttrStrings("deps"), wantDeps)
				compiled := onlyRuleOfKind(t, root, "app", "ts_compile")
				if !contains(compiled.AttrStrings("srcs"), "input.ts") || compiled.AttrString("tsconfig") != ":tsconfig" {
					t.Fatalf("run %d withdrew or changed the selected program: %s", run+1, buildFileText(t, root, "app"))
				}
				if dangling := danglingLabels(t, root); len(dangling) != 0 {
					t.Fatalf("run %d produced dangling labels: %v", run+1, dangling)
				}
				if before == nil {
					before = convergeSnapshot(t, root)
				} else if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("selected config changed on the second run:\n%s", diff)
				}
			}
		})
	}
}

func TestSelectedConfigBaseDoesNotCrossNestedBuildPackage(t *testing.T) {
	for _, selected := range []struct{ base, filename string }{
		{"abase", "tsconfig.build.json"},
		{"abase", "tsconfig.json"},
		{"zbase", "tsconfig.build.json"},
		{"zbase", "tsconfig.json"},
	} {
		base, filename := selected.base, selected.filename
		for _, nestedOwner := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/nested_owner_%t", base, filename, nestedOwner), func(t *testing.T) {
				files := map[string]string{
					"app/tsconfig.json":          fmt.Sprintf(`{"extends":"../%s/config/%s","files":["input.ts"]}`, base, filename),
					"app/input.ts":               "export const value = 1;\n",
					base + "/config/" + filename: `{"compilerOptions":{"strict":true},"files":["input.ts"]}`,
					base + "/config/input.ts":    "export const nested = 1;\n",
					base + "/config/BUILD.bazel": fmt.Sprintf(`exports_files([%q])`, filename),
					base + "/BUILD.bazel": fmt.Sprintf(loadDefs+`"ts_config")
ts_config(name = "tsconfig", src = "config/%s") # keep
`, filename),
				}
				var want []string
				if nestedOwner {
					files[base+"/config/BUILD.bazel"] = fmt.Sprintf(loadDefs+`"ts_config")
ts_config(name = "tsconfig", src = %q) # keep
`, filename)
				}
				if nestedOwner || filename == "tsconfig.json" {
					want = []string{"//" + base + "/config:tsconfig"}
				}
				g := generateAll(t, writeTree(t, files), func(c *config.Config) {
					c.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}
				})
				selected := mustRule(t, g.results["app"], "ts_config", tsConfigTargetName)
				wantStrings(t, "base owner within the nearest BUILD package", selected.AttrStrings("deps"), want)
				if len(want) != 0 {
					nested := mustRule(t, g.results[base+"/config"], "ts_compile", "config")
					wantStrings(t, "existing child package inputs", nested.AttrStrings("srcs"), []string{"input.ts"})
				}
			})
		}
	}
}
