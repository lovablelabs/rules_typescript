package typescript

// Convergence, not idempotence: a create-if-absent generator passes a
// byte-identical rerun and fails this, since the repairing run emits nothing.

import (
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
)

// ---- one gazelle invocation -------------------------------------------------

// The native command owns walking, merging, indexing, resolution and writes.
func convergeGazelle(t *testing.T, repoRoot string) {
	t.Helper()
	output, err := protoGazelle(t, repoRoot)
	if err != nil {
		t.Fatalf("gazelle: %v\n%s", err, output)
	}
	if output != "" {
		log.Print(strings.TrimSuffix(output, "\n"))
	}
}

func TestRootPackageScopesRemainVisibleAfterUnusedPublisherIsRemoved(t *testing.T) {
	requireTsgo(t)
	var unclaimed map[string]string
	for _, initial := range []string{"absent", "empty", "claimed"} {
		t.Run(initial, func(t *testing.T) {
			tree := map[string]string{
				"MODULE.bazel":      "module(name = \"root_scope\")\n",
				"package.json":      `{"type":"module"}`,
				"app/tsconfig.json": `{"files":["index.ts"]}`,
				"app/index.ts":      "export const value = 1;\n",
			}
			if initial == "empty" {
				tree["BUILD.bazel"] = ""
			}
			if initial == "claimed" {
				tree["metadata/BUILD.bazel"] = loadDefs + `"ts_compile")
# keep
ts_compile(name = "scope", srcs = ["//:package.json"], visibility = ["//visibility:public"])
`
			}
			root := writeTree(t, tree)
			for _, step := range []string{"cold", "new consumer", "new scope", "rerun"} {
				if step == "new consumer" {
					writeFile(t, filepath.Join(root, "sibling/tsconfig.json"), `{"files":["index.ts"]}`)
					writeFile(t, filepath.Join(root, "sibling/index.ts"), "export const sibling = 2;\n")
				}
				if step == "new scope" {
					writeFile(t, filepath.Join(root, "nested/package.json"), `{"type":"module"}`)
					writeFile(t, filepath.Join(root, "nested/app/tsconfig.json"), `{"files":["index.ts"]}`)
					writeFile(t, filepath.Join(root, "nested/app/index.ts"), "export const nested = 3;\n")
					before := buildFileBytes(t, root)
					output, err := protoGazelle(t, root, "-index=false", "-r=false", "nested/app")
					if err == nil || !strings.Contains(output, "not selected for publication") || !strings.Contains(output, "nested/package.json") {
						t.Fatalf("partial generation did not require the scope's owner: %v\n%s", err, output)
					}
					if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
						t.Fatalf("refused partial generation changed BUILD files: %s", diff)
					}
				}
				before := convergeSnapshot(t, root)
				convergeGazelle(t, root)
				assertNoDanglingLabels(t, root)
				if r := ruleNamed(loadRules(t, root, ""), "ts_compile", "root"); r != nil {
					t.Fatal("unused scope publisher remains in the root package")
				}
				exported := map[string]int{}
				for _, r := range loadRules(t, root, "") {
					if r.Kind() != "exports_files" {
						continue
					}
					list, err := exportSourceMembership(r)
					if err != nil || len(list.List) != 1 {
						t.Fatalf("invalid scope export: %v", err)
					}
					file, literal := list.List[0].(*bzl.StringExpr)
					if literal {
						exported[file.Value]++
						wantStrings(t, "root scope source access", r.AttrStrings("visibility"), []string{"//:__subpackages__"})
					}
				}
				if exported["package.json"] != 1 {
					t.Fatalf("root package scope has %d exports, want one", exported["package.json"])
				}
				if step == "new scope" || step == "rerun" {
					if exported["nested/package.json"] != 1 {
						t.Fatalf("new package scope has %d exports, want one", exported["nested/package.json"])
					}
					wantLabels(t, "new consumer's nearest scope", onDiskRule(t, root, "nested/app", "ts_compile", "app").AttrStrings("package_scopes"), []string{"//:nested/package.json"})
				}
				if step == "rerun" {
					if diff := snapshotDiff(before, convergeSnapshot(t, root)); diff != "" {
						t.Fatalf("scope publication changed on rerun: %s", diff)
					}
				}
			}
			if initial == "absent" {
				unclaimed = convergeSnapshot(t, root)
			} else if initial == "empty" {
				if diff := snapshotDiff(unclaimed, convergeSnapshot(t, root)); diff != "" {
					t.Fatalf("an initially empty BUILD changed publication: %s", diff)
				}
			}
		})
	}
}

// ---- snapshots --------------------------------------------------------------

var convergeNameAttr = regexp.MustCompile(`(?m)^\s*name = "([^"]*)"`)

// Rules sorted by kind and name: a merger append and a fresh insert of the
// same rule set have to compare equal, since rule order carries no meaning.
func convergeSnapshot(t *testing.T, repoRoot string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(repoRoot, func(p string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (entry.Name() != "BUILD.bazel" && entry.Name() != "BUILD") {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return err
		}
		text, rules := canonicalBuild(string(data))
		// An emptied BUILD file and no BUILD file both declare no targets; the
		// property is about the rules, so they compare the same.
		if rules > 0 {
			out[filepath.ToSlash(rel)] = text
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func canonicalBuild(text string) (string, int) {
	var blocks []string
	rules := 0
	for _, block := range strings.Split(text, "\n\n") {
		block = strings.Trim(block, "\n")
		if strings.TrimSpace(block) == "" {
			continue
		}
		blocks = append(blocks, block)
		if key := blockKey(block); strings.HasPrefix(key, "1:") {
			rules++
		}
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blockKey(blocks[i]) < blockKey(blocks[j]) })
	return strings.Join(blocks, "\n\n") + "\n", rules
}

func blockKey(block string) string {
	head := ""
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, `"""`) {
			continue
		}
		head = trimmed
		break
	}
	if strings.HasPrefix(head, "load(") {
		return "0:" + block
	}
	kind, _, isCall := strings.Cut(head, "(")
	if m := convergeNameAttr.FindStringSubmatch(block); isCall && m != nil {
		return "1:" + kind + ":" + m[1]
	}
	return "2:" + block
}

// ---- the diff is the failure message ---------------------------------------

func snapshotDiff(want, got map[string]string) string {
	paths := map[string]bool{}
	for p := range want {
		paths[p] = true
	}
	for p := range got {
		paths[p] = true
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)

	var b strings.Builder
	for _, p := range ordered {
		if want[p] == got[p] {
			continue
		}
		switch {
		case want[p] == "":
			fmt.Fprintf(&b, "\n--- %s (only the two-run tree has it)\n%s", p, indent(got[p]))
		case got[p] == "":
			fmt.Fprintf(&b, "\n--- %s (missing from the two-run tree)\n%s", p, indent(want[p]))
		default:
			fmt.Fprintf(&b, "\n--- %s\n%s", p, lineDiff(want[p], got[p]))
		}
	}
	return b.String()
}

// List elements in order: a label appended to an existing attribute has to
// compare equal to the same label generated in place.
func sortedLists(snapshot map[string]string) map[string]string {
	out := make(map[string]string, len(snapshot))
	for p, text := range snapshot {
		lines := strings.Split(text, "\n")
		sorted := make([]string, 0, len(lines))
		for i := 0; i < len(lines); {
			if !isListElement(lines[i]) {
				sorted = append(sorted, lines[i])
				i++
				continue
			}
			j := i
			for j < len(lines) && isListElement(lines[j]) {
				j++
			}
			run := append([]string(nil), lines[i:j]...)
			sort.Strings(run)
			sorted = append(sorted, run...)
			i = j
		}
		out[p] = strings.Join(sorted, "\n")
	}
	return out
}

func isListElement(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `",`)
}

func indent(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("      " + line + "\n")
	}
	return b.String()
}

// lineDiff prints the lines that differ with three lines of context, marking
// the from-scratch side "-" and the two-run side "+".
func lineDiff(want, got string) string {
	a := strings.Split(strings.TrimRight(want, "\n"), "\n")
	b := strings.Split(strings.TrimRight(got, "\n"), "\n")

	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	type edit struct {
		sign rune
		text string
	}
	var edits []edit
	for i, j := 0, 0; i < len(a) || j < len(b); {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			edits = append(edits, edit{' ', a[i]})
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			edits = append(edits, edit{'+', b[j]})
			j++
		default:
			edits = append(edits, edit{'-', a[i]})
			i++
		}
	}

	keep := make([]bool, len(edits))
	for i, e := range edits {
		if e.sign == ' ' {
			continue
		}
		for j := max(0, i-3); j < min(len(edits), i+4); j++ {
			keep[j] = true
		}
	}
	var b2 strings.Builder
	gap := false
	for i, e := range edits {
		if !keep[i] {
			gap = true
			continue
		}
		if gap {
			b2.WriteString("      @@\n")
			gap = false
		}
		fmt.Fprintf(&b2, "    %c %s\n", e.sign, e.text)
	}
	return b2.String()
}

// ---- dangling labels -------------------------------------------------------

// Every in-workspace label no rule declares and no file satisfies: a
// workspace-wide analysis failure no further Gazelle run clears.
func danglingLabels(t *testing.T, repoRoot string) []string {
	t.Helper()
	var out []string
	for _, dir := range convergePackages(t, repoRoot) {
		for _, r := range loadRules(t, repoRoot, dir) {
			for _, attr := range r.AttrKeys() {
				for _, v := range attrValues(r, attr) {
					if !isWorkspaceLabel(v) {
						continue
					}
					abs := absLabel(dir, v)
					pkg, name := splitLabel(abs)
					if attr == "visibility" && (name == "__pkg__" || name == "__subpackages__") {
						continue
					}
					if pkg == "visibility" || pkg == "conditions" {
						continue
					}
					if labelResolves(t, repoRoot, pkg, name) {
						continue
					}
					out = append(out, fmt.Sprintf("%s named by %s(%s) in %s",
						abs, r.Kind(), r.Name(), path.Join(dir, "BUILD.bazel")))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestDanglingLabelsRejectsVisibilityPseudoTargetsUsedAsSources(t *testing.T) {
	root := writeTree(t, map[string]string{
		"BUILD.bazel": `filegroup(
    name = "files",
    srcs = ["//other:__pkg__", "//other:__subpackages__", "//other:missing"],
    visibility = ["//other:__pkg__", "//other:__subpackages__", "//other:missing_group"],
)
`,
		"other/BUILD.bazel": "",
	})
	wantStrings(t, "visibility grants are not source or package-group targets", danglingLabels(t, root), []string{
		"//other:__pkg__ named by filegroup(files) in BUILD.bazel",
		"//other:__subpackages__ named by filegroup(files) in BUILD.bazel",
		"//other:missing named by filegroup(files) in BUILD.bazel",
		"//other:missing_group named by filegroup(files) in BUILD.bazel",
	})
}

// A label resolves to a rule in that package, or to a source file that package
// holds. Not to any file that happens to sit at the path: a directory with no
// BUILD file is not a package at all, so Bazel cannot load `//dir:file` there
// however well the file stats -- which is exactly the dangling label an os.Stat
// on its own says yes to, and the reason this walk once passed a workspace that
// failed at analysis.
func labelResolves(t *testing.T, repoRoot, pkg, name string) bool {
	for _, r := range loadRules(t, repoRoot, pkg) {
		if r.Name() == name {
			return true
		}
		// The store macro declares every target under its name: the trees,
		// their links and the hoist, none a rule this file holds.
		if r.Kind() == "npm_virtual_store" && strings.HasPrefix(name, r.Name()+"/") {
			return true
		}
	}
	full := path.Join(pkg, name)
	info, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(full)))
	if err != nil || info.IsDir() {
		return false
	}
	// A source file belongs to the innermost package above it, and no other
	// package can name it as one.
	holder, inPackage := enclosingPackage(repoRoot, full)
	return inPackage && holder == pkg
}

// enclosingPackage is the innermost directory at or above the file's own that
// holds a BUILD file -- the package Bazel reads a source label for it out of --
// and whether any directory up to the root is one.
func enclosingPackage(repoRoot, filePath string) (string, bool) {
	dir := path.Dir(filePath)
	if dir == "." {
		dir = ""
	}
	for {
		for _, name := range []string{"BUILD.bazel", "BUILD"} {
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(dir), name)); err == nil {
				return dir, true
			}
		}
		if dir == "" {
			return "", false
		}
		if parent := path.Dir(dir); parent == "." || parent == dir {
			dir = ""
		} else {
			dir = parent
		}
	}
}

// Every srcs entry naming a file under a directory that is a package of its
// own: Bazel reads a source label out of the innermost package above the file.
func crossesPackageBoundary(t *testing.T, repoRoot string) []string {
	t.Helper()
	var out []string
	for _, dir := range convergePackages(t, repoRoot) {
		for _, r := range loadRules(t, repoRoot, dir) {
			for _, src := range r.AttrStrings("srcs") {
				if strings.HasPrefix(src, "//") || strings.HasPrefix(src, "@") {
					continue
				}
				full := path.Join(dir, strings.TrimPrefix(src, ":"))
				holder, inPackage := enclosingPackage(repoRoot, full)
				if inPackage && holder != dir {
					out = append(out, fmt.Sprintf(
						"%s named by %s(%s) in %s sits in package %s", full,
						r.Kind(), r.Name(), path.Join(dir, "BUILD.bazel"), holder))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ---- small helpers ---------------------------------------------------------

func loadRules(t *testing.T, repoRoot, pkg string) []*rule.Rule {
	t.Helper()
	buildPath := filepath.Join(repoRoot, filepath.FromSlash(pkg), "BUILD.bazel")
	if _, err := os.Stat(buildPath); err != nil {
		return nil
	}
	f, err := rule.LoadFile(buildPath, pkg)
	if err != nil {
		t.Fatal(err)
	}
	return f.Rules
}

// convergePackages lists every directory holding a BUILD file, root first.
func convergePackages(t *testing.T, repoRoot string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(repoRoot, func(p string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != "BUILD.bazel" {
			return err
		}
		rel, err := filepath.Rel(repoRoot, filepath.Dir(p))
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func attrValues(r *rule.Rule, attr string) []string {
	if v := r.AttrString(attr); v != "" {
		return []string{v}
	}
	return r.AttrStrings(attr)
}

func isWorkspaceLabel(v string) bool {
	return strings.HasPrefix(v, "//") || strings.HasPrefix(v, ":")
}

func absLabel(pkg, lbl string) string {
	if after, ok := strings.CutPrefix(lbl, ":"); ok {
		return "//" + pkg + ":" + after
	}
	return lbl
}

func splitLabel(lbl string) (pkg, name string) {
	body := strings.TrimPrefix(lbl, "//")
	if pkg, name, ok := strings.Cut(body, ":"); ok {
		return pkg, name
	}
	return body, path.Base(body)
}

func ruleNamed(rules []*rule.Rule, kind, name string) *rule.Rule {
	for _, r := range rules {
		if r.Kind() == kind && r.Name() == name {
			return r
		}
	}
	return nil
}

func TestForeignJSONImportRemovalPreservesOnlyExplicitKeep(t *testing.T) {
	requireTsgo(t)
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			const exports = "exports_files([\"value.json\"], visibility = [\"//app:__pkg__\"])\n"
			const source = "import value from '../fixtures/value.json'; export const answer: number = value.answer;\n"
			root := writeTree(t, map[string]string{
				"app/tsconfig.json":    `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true},"files":["index.ts"]}`,
				"app/index.ts":         source,
				"fixtures/value.json":  `{ "answer": 42 }`,
				"fixtures/BUILD.bazel": exports,
			})
			convergeGazelle(t, root)
			buildPath := filepath.Join(root, "app/BUILD.bazel")
			initial, err := os.ReadFile(buildPath)
			if err != nil {
				t.Fatal(err)
			}
			const foreign = "//fixtures:value.json"
			if !strings.Contains(string(initial), foreign) {
				t.Fatalf("missing foreign JSON source: %s", initial)
			}
			retainedLog := captureLog(t, func() { convergeGazelle(t, root) })
			if strings.Contains(retainedLog, `"`+foreign+`" is no longer declared`) {
				t.Fatalf("retained compiler input reported dropped: %s", retainedLog)
			}
			if keep {
				writeFile(t, buildPath, strings.Replace(string(initial), `"`+foreign+`",`, `"`+foreign+`", # keep`, 1))
			}
			writeFile(t, filepath.Join(root, "app/index.ts"), "export const answer: number = 42;\n")
			convergeGazelle(t, root)
			var found bool
			for _, r := range loadRules(t, root, "app") {
				if r.Kind() == "ts_compile" {
					found = slices.Contains(r.AttrStrings("srcs"), foreign)
				}
			}
			if found != keep {
				t.Fatalf("foreign JSON retained=%t, keep=%t", found, keep)
			}
			writeFile(t, filepath.Join(root, "app/index.ts"), source)
			convergeGazelle(t, root)
			var restored bool
			for _, r := range loadRules(t, root, "app") {
				if r.Kind() == "ts_compile" {
					restored = slices.Contains(r.AttrStrings("srcs"), foreign)
				}
			}
			if !restored {
				t.Fatal("restored import did not restore JSON source")
			}
			actual, err := os.ReadFile(filepath.Join(root, "fixtures/BUILD.bazel"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(actual), `visibility = ["//app:__pkg__"]`) {
				t.Fatalf("fixture visibility changed: %s", actual)
			}
		})
	}
}

func TestForeignSourceClosureSurvivesRerunsAndDisappearsAfterImportRemoval(t *testing.T) {
	requireTsgo(t)
	const source = "import { answer } from '../fixtures/a'; export { answer };\n"
	const exports = `exports_files(
    [
        "a.ts",
        "nested/b.ts",
        "package.json",
        "value.json",
    ],
    visibility = ["//app:__pkg__"],
)
`
	root := writeTree(t, map[string]string{
		"MODULE.bazel":               "module(name = \"source_closure\")\n",
		pnpmLockfileName:             "lockfileVersion: '9.0'\nimporters:\n  .: {}\n",
		"node_modules/.modules.yaml": "",
		"app/tsconfig.json":          `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","resolveJsonModule":true},"files":["index.ts"]}`,
		"app/index.ts":               source,
		"fixtures/BUILD.bazel":       exports,
		"fixtures/package.json":      `{"name":"foreign-project"}`,
		"fixtures/a.ts":              "export { answer } from './nested/b';\n",
		"fixtures/nested/b.ts":       "import data from '../value.json'; export const answer = data.answer;\n",
		"fixtures/value.json":        `{"answer":42}`,
		"fixtures/unused.ts":         "import 'does-not-exist';\n",
	})
	convergeGazelle(t, root)
	initial := buildFileText(t, root, "app")
	wantSources := []string{"//fixtures:a.ts", "//fixtures:nested/b.ts", "//fixtures:value.json", "index.ts"}
	wantScopes := []string{"//fixtures:package.json"}
	assertInputs := func(stage string, sources, scopes []string) {
		t.Helper()
		for _, r := range loadRules(t, root, "app") {
			if r.Kind() == "ts_compile" {
				wantLabels(t, stage+" srcs", r.AttrStrings("srcs"), sources)
				wantLabels(t, stage+" package_scopes", r.AttrStrings("package_scopes"), scopes)
				return
			}
		}
		t.Fatal("missing compile rule")
	}
	assertInputs("initial", wantSources, wantScopes)
	logged := captureLog(t, func() { convergeGazelle(t, root) })
	if strings.Contains(logged, "is no longer declared") || buildFileText(t, root, "app") != initial {
		t.Fatalf("retained source closure did not converge: %s", logged)
	}
	assertInputs("repeated", wantSources, wantScopes)
	buildPath := filepath.Join(root, "app/BUILD.bazel")
	writeFile(t, buildPath, strings.ReplaceAll(initial, "//fixtures:", "@//fixtures:"))
	logged = captureLog(t, func() { convergeGazelle(t, root) })
	if strings.Contains(logged, "is no longer declared") {
		t.Fatalf("equivalent source labels reported dropped: %s", logged)
	}
	assertInputs("equivalent labels", wantSources, wantScopes)
	writeFile(t, filepath.Join(root, "app/index.ts"), "export const answer = 42;\n")
	logged = captureLog(t, func() { convergeGazelle(t, root) })
	assertInputs("removed import", []string{"index.ts"}, nil)
	if !strings.Contains(logged, "is no longer declared") {
		t.Fatalf("removed source imports were not reported: %s", logged)
	}
	writeFile(t, filepath.Join(root, "app/index.ts"), source)
	convergeGazelle(t, root)
	assertInputs("restored import", wantSources, wantScopes)
	actual, err := os.ReadFile(filepath.Join(root, "fixtures/BUILD.bazel"))
	if err != nil || string(actual) != exports {
		t.Fatalf("foreign source owner changed: %s, %v", actual, err)
	}
	if _, err := os.Stat(filepath.Join(root, "fixtures/nested/BUILD.bazel")); !os.IsNotExist(err) {
		t.Fatalf("foreign source directory became a Bazel package: %v", err)
	}
}

// gazelle_roundtrip's emitted binary hit ERR_MODULE_NOT_FOUND for the sibling helper.mjs.
func TestRetainedForeignDeclarationKeepsItsJavaScriptTwin(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, map[string]string{
		"MODULE.bazel":               "module(name = \"declaration_twin\")\n",
		pnpmLockfileName:             "lockfileVersion: '9.0'\nimporters:\n  .: {}\n  app: {}\n",
		"pnpm-workspace.yaml":        "packages:\n  - app\n",
		"node_modules/.modules.yaml": "",
		"package.json":               `{"private":true}`,
		"BUILD.bazel":                "exports_files([\"package.json\"], visibility = [\"//visibility:public\"])\n",
		"app/package.json":           `{"type":"module"}`,
		"app/tsconfig.json":          `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","allowJs":true,"types":[]},"files":["index.ts"]}`,
		"app/index.ts":               "import { offset } from '../fixtures/helper.mjs';\nimport data from '../fixtures/value.json' with { type: 'json' };\nconsole.log(data.value + offset);\n",
		"app/BUILD.bazel":            "load(\"@rules_typescript//ts:defs.bzl\", \"ts_binary\")\n\nts_binary(name = \"run\", entry_point = \":app\")\n",
		"fixtures/BUILD.bazel":       "exports_files([\"helper.d.mts\", \"helper.mjs\", \"value.json\"], visibility = [\"//app:__pkg__\"])\n",
		"fixtures/helper.d.mts":      "export declare const offset: number;\n",
		"fixtures/helper.mjs":        "export const offset = 2;\n",
		"fixtures/value.json":        `{"value":40}`,
	})
	convergeGazelle(t, root)
	compile := ruleNamed(loadRules(t, root, "app"), "ts_compile", "app")
	if compile == nil {
		t.Fatal("missing compile rule")
	}
	wantLabels(t, "srcs", compile.AttrStrings("srcs"), []string{"//fixtures:helper.d.mts", "//fixtures:helper.mjs", "//fixtures:value.json", "index.ts"})
	if !slices.Contains(compile.AttrStrings("deps"), "//:root") {
		t.Fatalf("the twin's runtime scope has no publisher in deps: %q", compile.AttrStrings("deps"))
	}
}

func TestGeneratedTreeImportRetainsDirectOwnerWithoutOutputFiles(t *testing.T) {
	requireTsgo(t)
	cases := []struct {
		name, paths, specifier string
		treeManifest           string
		kind, directives       string
		wantGenerator          bool
	}{
		{name: "inherited alias", paths: `{"#shared/*":["./shared/*"]}`, specifier: "#shared/generated/runtime", wantGenerator: true},
		{name: "generated first fallback", paths: `{"#shared/value":["./shared/value"],"#module":["./shared/generated/runtime","./shared/fallback"]}`, specifier: "#module", wantGenerator: true},
		{name: "authored first fallback", paths: `{"#shared/value":["./shared/value"],"#module":["./shared/fallback","./shared/generated/runtime"]}`, specifier: "#module"},
		{name: "exact alias overrides wildcard", paths: `{"#shared/*":["./shared/*"],"#shared/generated/runtime":["./shared/fallback"]}`, specifier: "#shared/generated/runtime"},
		{name: "tree root candidate", paths: `{"#shared/*":["./shared/*"],"#module":["./shared/generated"]}`, specifier: "#module", wantGenerator: true},
		{name: "stale redirect cannot change tree owner", paths: `{"#shared/*":["./shared/*"],"#module":["./shared/generated"]}`, specifier: "#module", treeManifest: `{"types":"../../../redirect/value.d.ts"}`, wantGenerator: true},
		{name: "unowned missing import", paths: `{"#shared/*":["./shared/*"]}`, specifier: "#shared/missing/runtime"},
		{name: "aliased producer", paths: `{"#shared/value":["./shared/value"],"#generated":["../producer/tree/runtime"]}`, specifier: "#generated", kind: "wrapped_codegen", directives: "# gazelle:alias_kind wrapped_codegen ts_codegen\n", wantGenerator: true},
		{name: "mapped indexed producer", paths: `{"#shared/value":["./shared/value"],"#generated":["../producer/tree/runtime"]}`, specifier: "#generated", kind: "wrapped_codegen", directives: "# gazelle:ignore\n# gazelle:map_kind ts_codegen wrapped_codegen //:codegen.bzl\n", wantGenerator: true},
		{name: "mapped aliased producer", paths: `{"#shared/value":["./shared/value"],"#generated":["../producer/tree/runtime"]}`, specifier: "#generated", kind: "wrapped_codegen", directives: "# gazelle:map_kind ts_codegen wrapped_codegen //:codegen.bzl\n# gazelle:alias_kind wrapped_codegen ts_codegen\n", wantGenerator: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			kind, load := tc.kind, "//:codegen.bzl"
			producerPkg, outDir := "producer", "tree"
			if kind == "" {
				kind, load = "ts_codegen", "@rules_typescript//ts:defs.bzl"
				producerPkg, outDir = "app", "shared/generated"
			}
			writeWorkspace(t, root, map[string]string{
				"package.json":              `{"name":"fixture"}`,
				"app/tsconfig.json":         `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":` + tc.paths + `},"include":["shared/**/*.ts"]}`,
				"app/shared/value.ts":       "export const value = 1;\n",
				"app/shared/fallback.ts":    "export const generated = 'authored';\n",
				"app/plugins/tsconfig.json": `{"extends":"../tsconfig.json","include":["*.ts"]}`,
				"app/plugins/consumer.ts":   "import { value } from '#shared/value';\nimport { generated } from '" + tc.specifier + "';\nexport const result = [value, generated];\n",
			})
			writeFile(t, filepath.Join(root, producerPkg, "BUILD.bazel"), tc.directives+fmt.Sprintf(`load(%q, %q)
%s(
    name = "generated",
    generator = "//:generator",
    out_dir = %q,
    visibility = ["//visibility:public"],
)
`, load, kind, kind, outDir))
			consumers := map[string]string{"app/plugins": "ts_compile"}
			if tc.kind != "" {
				writeFile(t, filepath.Join(root, "app/config-consumer/tsconfig.json"), `{"extends":"../tsconfig.json","include":["*.ts"]}`)
				writeFile(t, filepath.Join(root, "app/config-consumer/consumer.test.ts"), "import { value } from '#shared/value';\nexport const result = value;\n")
				writeFile(t, filepath.Join(root, "app/config-consumer/vitest.config.mts"), "import { generated } from '../../producer/tree/runtime';\nexport default { generated };\n")
				consumers["app/config-consumer"] = "ts_test"
			}
			if tc.treeManifest != "" {
				writeFile(t, filepath.Join(root, "redirect/tsconfig.json"), `{"files":["value.d.ts"]}`)
				writeFile(t, filepath.Join(root, "redirect/value.d.ts"), "export declare const generated: string;\n")
			}
			wantDeps := []string{"//app"}
			if tc.wantGenerator {
				wantDeps = append(wantDeps, "//"+producerPkg+":generated")
			}
			var cold map[string]string
			for _, state := range []string{"cold", "materialized", "deleted"} {
				switch state {
				case "materialized":
					for _, name := range []string{"runtime", "index"} {
						writeFile(t, filepath.Join(root, producerPkg, outDir, name+".d.ts"), "export declare const generated: string;\n")
					}
					if tc.treeManifest != "" {
						writeFile(t, filepath.Join(root, producerPkg, outDir, "package.json"), tc.treeManifest)
					}
				case "deleted":
					if err := os.RemoveAll(filepath.Join(root, producerPkg, outDir)); err != nil {
						t.Fatal(err)
					}
				}
				for pass := 1; pass <= 2; pass++ {
					captureLog(t, func() { convergeGazelle(t, root) })
					for pkg, kind := range consumers {
						found := false
						for _, r := range loadRules(t, root, pkg) {
							if r.Kind() != kind {
								continue
							}
							found = true
							what := fmt.Sprintf("%s pass %d %s", state, pass, pkg)
							wantLabels(t, what+" deps", r.AttrStrings("deps"), wantDeps)
							var configScopes []string
							if kind == "ts_test" {
								configScopes = []string{"//:package.json"}
							}
							wantLabels(t, what+" config_srcs", r.AttrStrings("config_srcs"), configScopes)
						}
						if !found {
							t.Fatalf("%s: nested consumer has no %s owner", pkg, kind)
						}
					}
					if cold == nil {
						cold = convergeSnapshot(t, root)
					} else if diff := snapshotDiff(cold, convergeSnapshot(t, root)); diff != "" {
						t.Errorf("%s pass %d changed cold-tree BUILD files:%s", state, pass, diff)
					}
				}
			}
		})
	}
}
