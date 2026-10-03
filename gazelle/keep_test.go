package typescript

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
)

// nonLiteralCase is one managed attribute, in the workspace whose generator
// writes it. class picks the expression shapes that can stand in for its value.
type nonLiteralCase struct {
	workspace string
	extra     map[string]string
	pkg       string
	kind      string
	target    string
	attr      string
	class     string // "list" or "scalar"
	consumer  string
}

// managedAttrCases names every attribute in keep.go's managedAttrs, in a
// workspace where generation writes that rule -- so a managed attribute that
// stops being covered here is one this test stops asking about.
func managedAttrCases() []nonLiteralCase {
	managed := func(workspace, pkg, kind, target, attr, class string,
	) nonLiteralCase {
		return nonLiteralCase{workspace: workspace, pkg: pkg, kind: kind,
			target: target, attr: attr, class: class}
	}
	consumedBy := func(c nonLiteralCase, consumer string) nonLiteralCase {
		c.consumer = consumer
		return c
	}
	cases := []nonLiteralCase{
		consumedBy(managed("plain", "src", "ts_compile", "src", "srcs", "list"), "//src:src_test"),
		managed("plain", "shared", "ts_compile", "shared", "package_scopes", "list"),
		managed("plain", "src", "ts_compile", "src", "deps", "list"),
		managed("plain", "src", "ts_compile", "src", "visibility", "list"),
		managed("plain", "src", "ts_compile", "src", "tsconfig", "scalar"),
		managed("plain", "src", "ts_test", "src_test", "srcs", "list"),
		managed("plain", "src", "ts_test", "src_test", "deps", "list"),
		managed("plain", "src", "ts_test", "src_test", "tsconfig", "scalar"),
		managed("plain", "src", "ts_test", "src_test", "config", "scalar"),
		managed("plain", "", "ts_config", "tsconfig", "src", "scalar"),
		managed("plain", "", "ts_config", "tsconfig", "visibility", "list"),
		managed("plain", "src", "ts_config", "tsconfig", "deps", "list"),

		managed("pnpm_member", "packages/core", "ts_compile", "core",
			"node_modules", "scalar"),
		managed("pnpm_member", "packages/core", "ts_test", "core_test", "deps",
			"list"),
		managed("pnpm_member", "packages/core", "ts_test", "core_test",
			"node_modules", "scalar"),
		managed("pnpm_member", "packages/core", "node_modules", "node_modules",
			"deps", "list"),
		managed("pnpm_member", "packages/core", "node_modules", "node_modules",
			"parent", "scalar"),
		managed("pnpm_member", "", "node_modules", "node_modules", "hoist",
			"scalar"),
		managed("pnpm_member", "packages/core", "node_modules", "node_modules",
			"visibility", "list"),
		managed("pnpm_member", "", "node_modules_member", "node_modules/@w/core",
			"member", "scalar"),
		managed("pnpm_member", "", "node_modules_member", "node_modules/@w/core",
			"visibility", "list"),

		consumedBy(managed("worker", "worker", "filegroup", "vitest_config", "srcs", "list"), "//worker/test:test_test"),
		managed("worker", "worker", "filegroup", "vitest_config", "visibility",
			"list"),
	}
	for _, attr := range []string{"entry_point", "node_modules", "plugin", "visibility"} {
		class := "scalar"
		if attr == "visibility" {
			class = "list"
		}
		c := managed("pnpm_member", "packages/app", "ts_dev_server", "dev", attr, class)
		c.extra = map[string]string{"packages/app/tsconfig.json": `{"include":["src/**/*.ts"]}`}
		cases = append(cases, c)
	}
	return cases

}

// The expression shapes that can stand in for a value of each class.
var nonLiteralShapes = map[string][]string{
	"list":   {"ident", "concat", "select", "mixed"},
	"scalar": {"ident"},
}

func TestNonLiteralAttrValue(t *testing.T) {
	fixtures := map[string]convergeCase{}
	for _, tc := range convergeCases() {
		fixtures[tc.name] = tc
	}

	for _, nc := range managedAttrCases() {
		tc, ok := fixtures[nc.workspace]
		if !ok {
			t.Fatalf("no %q fixture for %s(%s).%s", nc.workspace, nc.kind, nc.target, nc.attr)
		}
		for _, shape := range nonLiteralShapes[nc.class] {
			name := fmt.Sprintf("%s/%s.%s/%s", nc.workspace, nc.kind, nc.attr, shape)
			t.Run(name, func(t *testing.T) { runNonLiteralCase(t, tc, nc, shape) })
		}
	}
}

func runNonLiteralCase(t *testing.T, tc convergeCase, nc nonLiteralCase, shape string) {
	t.Helper()

	root := t.TempDir()
	files := map[string]string{}
	for rel, body := range tc.files {
		files[rel] = body
	}
	for rel, body := range nc.extra {
		files[rel] = body
	}
	writeWorkspace(t, root, files)
	captureLog(t, func() { convergeGazelle(t, root) })
	builds := buildFileBytes(t, root)

	authored := writeNonLiteralAttr(t, root, nc, shape, false)
	buildPath := filepath.Join(root, filepath.FromSlash(nc.pkg), "BUILD.bazel")

	if nc.attr == "srcs" && (nc.kind == "ts_compile" || nc.kind == "ts_test") && shape == "concat" {
		assertUnknownInputsRefused(t, root, nc, label.New("", nc.pkg, nc.target).String())
	} else {
		logged := captureLog(t, func() { convergeGazelle(t, root) })
		text := buildFileText(t, root, nc.pkg)

		if declaredAttrExpr(t, root, nc) == nil {
			t.Fatalf("%s(%s).%s is gone after the merge: the attribute was deleted rather than "+
				"replaced or left alone, so nothing declares the inputs it named.\n%s",
				nc.kind, nc.target, nc.attr, indent(text))
		}
		if !rewriteReported(logged, buildPath, nc) {
			t.Fatalf("%s(%s).%s held a %s expression the merger cannot reconcile and the run said "+
				"nothing -- it has to name the file, the rule, the attribute and \"# keep\". "+
				"Whether Gazelle replaced the value or stopped maintaining it, the user is left "+
				"believing it is still recomputed.\n%s\nthe run said:\n%s",
				nc.kind, nc.target, nc.attr, shape, indent(text), indentLog(logged))
		}
		if missing := missingFrom(declaredStrings(t, root, nc.pkg), authored.values); len(missing) > 0 &&
			exprShape(declaredAttrExpr(t, root, nc)) == shape {
			t.Fatalf("%s(%s).%s kept its %s shape but lost %v: a value vanished out of an expression "+
				"the merge reported it would leave alone.\n%s\nthe run said:\n%s",
				nc.kind, nc.target, nc.attr, shape, missing, indent(text), indentLog(logged))
		}

		logged = captureLog(t, func() { convergeGazelle(t, root) })
		stillUnmergeable := !isLiteralAttrValue(declaredAttrExpr(t, root, nc))
		if got := rewriteReported(logged, buildPath, nc); got != stillUnmergeable {
			t.Fatalf("%s(%s).%s is %s after the merge and the next run reported it: %v. The "+
				"diagnostic and the file disagree.\n%s\nthe run said:\n%s",
				nc.kind, nc.target, nc.attr, exprShape(declaredAttrExpr(t, root, nc)), got,
				indent(buildFileText(t, root, nc.pkg)), indentLog(logged))
		}
	}

	keptRoot := t.TempDir()
	for rel := range files {
		if name := filepath.Base(rel); name == "BUILD" || name == "BUILD.bazel" {
			delete(files, rel)
		}
	}
	maps.Copy(files, builds)
	writeWorkspace(t, keptRoot, files)
	if got := buildFileBytes(t, keptRoot); !maps.Equal(got, builds) {
		t.Fatalf("kept fixture BUILD paths or bytes differ from initial generation:\nwant: %v\ngot: %v", builds, got)
	}
	assertKeepHoldsExpr(t, keptRoot, nc, shape)
}

func assertKeepHoldsExpr(t *testing.T, root string, nc nonLiteralCase, shape string) {
	t.Helper()

	authored := writeNonLiteralAttr(t, root, nc, shape, true)
	expression := bzl.FormatString(declaredAttrExpr(t, root, nc))
	buildPath := filepath.Join(root, filepath.FromSlash(nc.pkg), "BUILD.bazel")
	builds := buildFileBytes(t, root)
	ownerships := []string{"attribute"}
	if (nc.kind == "ts_compile" || nc.kind == "ts_test") && (nc.attr == "srcs" || nc.attr == "package_scopes" || nc.attr == "tsconfig" || nc.attr == "config") {
		ownerships = append(ownerships, "rule", "ignored")
	}
	for _, ownership := range ownerships {
		writeWorkspace(t, root, builds)
		if ownership == "rule" {
			keepFixtureRule(t, root, label.New("", nc.pkg, nc.target).String())
		} else if ownership == "ignored" {
			writeFile(t, buildPath, "# gazelle:ignore\n"+buildFileText(t, root, nc.pkg))
		}
		consumer := ""
		if ownership == "attribute" && len(ownerships) > 1 && (nc.attr == "srcs" || shape != "mixed") {
			consumer = label.New("", nc.pkg, nc.target).String()
		} else if ownership != "ignored" {
			consumer = nc.consumer
		}
		if consumer != "" {
			assertUnknownInputsRefused(t, root, nc, consumer)
			if consumer == label.New("", nc.pkg, nc.target).String() {
				continue
			}
			keepFixtureRule(t, root, consumer)
		}
		for _, pass := range []string{"initial", "repeated"} {
			logged := captureLog(t, func() { convergeGazelle(t, root) })
			text := buildFileText(t, root, nc.pkg)
			if got := bzl.FormatString(declaredAttrExpr(t, root, nc)); got != expression {
				t.Fatalf("%s ownership changed the kept expression on %s run:\nwant: %s\ngot: %s", ownership, pass, expression, got)
			}
			if got := exprShape(declaredAttrExpr(t, root, nc)); got != shape {
				t.Fatalf("%s ownership changed %s(%s).%s from %s to %s on %s run:\n%s\n%s",
					ownership, nc.kind, nc.target, nc.attr, shape, got, pass, indent(text), indentLog(logged))
			}
			if missing := missingFrom(declaredStrings(t, root, nc.pkg), authored.values); len(missing) > 0 {
				t.Fatalf("%s ownership lost %v from %s(%s).%s on %s run:\n%s\n%s",
					ownership, missing, nc.kind, nc.target, nc.attr, pass, indent(text), indentLog(logged))
			}
			if rewriteReported(logged, buildPath, nc) {
				t.Fatalf("%s ownership still reported a rewrite of %s(%s).%s on %s run:\n%s",
					ownership, nc.kind, nc.target, nc.attr, pass, indentLog(logged))
			}
		}
	}
}

func keepFixtureRule(t *testing.T, root, target string) {
	t.Helper()
	consumer, err := label.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	buildPath := filepath.Join(root, filepath.FromSlash(consumer.Pkg), "BUILD.bazel")
	file, err := rule.LoadFile(buildPath, consumer.Pkg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range file.Rules {
		if r.Name() == consumer.Name {
			r.AddComment("# keep")
			writeFile(t, buildPath, string(file.Format()))
			return
		}
	}
	t.Fatalf("fixture has no rule %s to keep", target)
}

func assertUnknownInputsRefused(t *testing.T, root string, nc nonLiteralCase, consumer string) {
	t.Helper()
	before := buildFileBytes(t, root)
	output, err := protoGazelle(t, root)
	if err == nil {
		t.Fatalf("opaque %s(%s).%s allowed automatic closure:\n%s", nc.kind, nc.target, nc.attr, output)
	}
	owner := label.New("", nc.pkg, nc.target).String()
	facts := []string{consumer + " cannot discover its compiler closure", owner + " has unsupported srcs membership", "explicit source-file labels", "keep the whole rule or ignore its package"}
	if nc.attr == "tsconfig" {
		facts = []string{consumer + " cannot discover its compiler closure", `selected tsconfig ""`, "authored, readable compiler configuration", "keep the whole rule or ignore its package"}
	}
	if nc.attr == "package_scopes" {
		facts = []string{consumer + " requires package scope package.json", "kept package_scopes omit it", "deps do not supply its runtime scope input", "retain the scope or supply it through a compiler dependency"}
	}
	if nc.attr == "config" || nc.kind == "filegroup" && nc.attr == "srcs" {
		reason := "config is an expression"
		if nc.kind == "filegroup" {
			reason = owner + " has expression-valued srcs"
		}
		facts = []string{consumer + " cannot establish its selected config root", reason, "refusing to regenerate config runtime inputs", "keep the whole ts_test rule and maintain its runtime inputs manually"}
	}
	for _, fact := range facts {
		if !strings.Contains(output, fact) {
			t.Errorf("opaque %s(%s).%s refusal lacks %q:\n%s", nc.kind, nc.target, nc.attr, fact, output)
		}
	}
	if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
		t.Fatalf("unsupported %s(%s).%s changed BUILD files: %s", nc.kind, nc.target, nc.attr, diff)
	}
}

// The rewrite line: the file, the rule, the attribute and the way out. Every
// part is load-bearing -- a diagnostic missing any of them cannot be acted on.
func rewriteReported(logged, buildPath string, nc nonLiteralCase) bool {
	for _, line := range strings.Split(logged, "\n") {
		if !strings.Contains(line, buildPath) {
			continue
		}
		if !strings.Contains(line, fmt.Sprintf("%s(%s)", nc.kind, nc.target)) {
			continue
		}
		if strings.Contains(line, nc.attr) && strings.Contains(line, `"# keep"`) {
			return true
		}
	}
	return false
}

// ---- authoring the expression ----------------------------------------------

type authoredExpr struct {
	values []string
}

// writeNonLiteralAttr rewrites the generated attribute as the named shape,
// carrying the values generation derived plus one only the user knows about.
func writeNonLiteralAttr(t *testing.T, root string, nc nonLiteralCase, shape string, keep bool) authoredExpr {
	t.Helper()

	buildPath := filepath.Join(root, filepath.FromSlash(nc.pkg), "BUILD.bazel")
	var target *rule.Rule
	for _, r := range loadRules(t, root, nc.pkg) {
		if r.Kind() == nc.kind && r.Name() == nc.target {
			target = r
		}
	}
	if target == nil {
		t.Fatalf("generation wrote no %s(%s) in %s, so there is nothing to hand-edit:\n%s",
			nc.kind, nc.target, buildPath, buildFileText(t, root, nc.pkg))
	}

	hand := handValueFor(nc)
	generated := attrValues(target, nc.attr)
	values := append(append([]string(nil), generated...), hand)

	const indent = "    "
	quoted := func(vs []string) string { return strings.Join(quotedEach(vs), ", ") }

	var prelude, replacement string
	switch shape {
	case "ident":
		if nc.class == "scalar" {
			prelude = fmt.Sprintf("_HAND = %q", hand)
			values = []string{hand}
		} else {
			prelude = fmt.Sprintf("_HAND = [%s]", quoted(values))
		}
		replacement = indent + nc.attr + " = _HAND,"
	case "concat":
		replacement = fmt.Sprintf("%s%s = [%s] + [%q],", indent, nc.attr, quoted(generated), hand)
	case "select":
		replacement = fmt.Sprintf("%s%s = select({\"//conditions:default\": [%s]}),",
			indent, nc.attr, quoted(values))
	case "mixed":
		prelude = fmt.Sprintf("_HAND = %q", hand)
		replacement = fmt.Sprintf("%s%s = [%s],", indent, nc.attr,
			strings.Join(append(quotedEach(generated), "_HAND"), ", "))
	default:
		t.Fatalf("no expression for shape %q", shape)
	}

	data, err := os.ReadFile(buildPath)
	if err != nil {
		t.Fatal(err)
	}
	blocks := strings.Split(string(data), "\n\n")
	edited := false
	for i, block := range blocks {
		if !strings.Contains(block, nc.kind+"(") || !strings.Contains(block, fmt.Sprintf("name = %q", nc.target)) {
			continue
		}
		if keep {
			replacement = indent + "# keep\n" + replacement
		}
		lines := replaceAttrLines(strings.Split(block, "\n"), nc.attr, []string{replacement})
		if lines == nil {
			t.Fatalf("could not place %s in the %s(%s) block:\n%s", nc.attr, nc.kind, nc.target, block)
		}
		blocks[i] = strings.Join(lines, "\n")
		edited = true
		break
	}
	if !edited {
		t.Fatalf("no %s(%s) block in %s:\n%s", nc.kind, nc.target, buildPath, data)
	}
	body := strings.Join(blocks, "\n\n")
	if prelude != "" {
		body = prelude + "\n\n" + body
	}
	file, err := rule.LoadData(buildPath, nc.pkg, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, buildPath, string(file.Format()))
	return authoredExpr{values: values}
}

// handValueFor is the one value in the expression generation cannot derive: the
// element whose disappearance is the defect.
func handValueFor(nc nonLiteralCase) string {
	switch {
	case nc.attr == "visibility":
		return "//vendor:__pkg__"
	case nc.attr == "srcs":
		return "hand_extra.ts"
	case nc.attr == "deps":
		return "@npm//:sharp"
	case nc.class == "scalar":
		return "hand.config.mjs"
	default:
		return "//vendor:vendor_hand"
	}
}

// ---- reading the expression back -------------------------------------------

func declaredAttrExpr(t *testing.T, root string, nc nonLiteralCase) bzl.Expr {
	t.Helper()
	for _, r := range loadRules(t, root, nc.pkg) {
		if r.Kind() == nc.kind && r.Name() == nc.target {
			return r.Attr(nc.attr)
		}
	}
	return nil
}

// exprShape names the shape of an expression in the vocabulary the cases are
// written in, so a rewrite into a plain list is a shape change rather than a
// value comparison.
func exprShape(e bzl.Expr) string {
	switch v := e.(type) {
	case *bzl.Ident:
		return "ident"
	case *bzl.BinaryExpr:
		return "concat"
	case *bzl.ListExpr:
		for _, el := range v.List {
			if _, ok := el.(*bzl.StringExpr); !ok {
				return "mixed"
			}
		}
		return "literal list"
	case *bzl.StringExpr:
		return "string"
	case *bzl.CallExpr:
		callee, ok := v.X.(*bzl.Ident)
		if !ok {
			return "call"
		}
		if callee.Name == "select" {
			return "select"
		}
		return callee.Name + "()"
	}
	return fmt.Sprintf("%T", e)
}

// declaredStrings is every string literal in the BUILD file, so a value the
// attribute reaches through a module-level variable counts as declared.
func declaredStrings(t *testing.T, root, pkg string) []string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(pkg), "BUILD.bazel")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := bzl.ParseBuild(path, data)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	bzl.Walk(parsed, func(x bzl.Expr, _ []bzl.Expr) {
		if s, ok := x.(*bzl.StringExpr); ok {
			out = append(out, s.Value)
		}
	})
	return out
}

func quotedEach(vs []string) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, fmt.Sprintf("%q", v))
	}
	return out
}

func missingFrom(have, want []string) []string {
	var missing []string
	for _, v := range want {
		if !contains(have, v) {
			missing = append(missing, v)
		}
	}
	return missing
}

// ---- the drop diagnostic ---------------------------------------------------

// TestHandAuthoredAttrValue/*/replaced is the half of the drop diagnostic that
// has to fire: a value generation cannot derive disappears, and the run names
// it. TestDeletedPathIsNotReportedAsDropped is the half that has to stay quiet.
// Every fixture mutation that removes something is an ordinary deletion, and
// the advice -- hold it with "# keep" -- would name a source nothing provides,
// which fails analysis rather than surviving the run.
func TestDeletedPathIsNotReportedAsDropped(t *testing.T) {
	for _, tc := range convergeCases() {
		for _, mut := range tc.mutations {
			if len(mut.remove) == 0 {
				continue
			}
			t.Run(tc.name+"/"+mut.kind, func(t *testing.T) {
				root := t.TempDir()
				writeWorkspace(t, root, tc.files)
				captureLog(t, func() { convergeGazelle(t, root) })
				applyMutation(t, root, mut)

				logged := captureLog(t, func() { convergeGazelle(t, root) })
				for _, line := range strings.Split(logged, "\n") {
					if !strings.Contains(line, "is no longer declared") {
						continue
					}
					for _, gone := range mut.remove {
						if !strings.Contains(line, gone) {
							continue
						}
						t.Fatalf("%s was deleted and the run told the user to hold it with "+
							"\"# keep\". Doing that names a source nothing provides, so the "+
							"advice fails analysis instead of surviving the run:\n%s",
							gone, indentLog(line))
					}
				}
			})
		}
	}
}

// TestManagedAttrCasesCoverGeneratedAttrs discovers the (kind, attribute) pairs
// generation actually writes across the fixtures and requires a case for each.
// Hand-listing them is how ts_compile.deps came to be uncovered while four
// framework rules were: the list and the generators drifted apart.
func TestManagedAttrCasesCoverGeneratedAttrs(t *testing.T) {
	covered := map[string]struct{}{}
	for _, nc := range managedAttrCases() {
		covered[nc.kind+"."+nc.attr] = struct{}{}
	}

	kinds := (&tsLang{}).Kinds()
	var missing []string
	seen := map[string]struct{}{}

	for _, tc := range convergeCases() {
		root := t.TempDir()
		writeWorkspace(t, root, tc.files)
		captureLog(t, func() { convergeGazelle(t, root) })

		for _, pkg := range convergePackages(t, root) {
			for _, r := range loadRules(t, root, pkg) {
				info, known := kinds[r.Kind()]
				if !known {
					continue
				}
				for attr := range info.MergeableAttrs {
					if r.Attr(attr) == nil {
						continue
					}
					pair := r.Kind() + "." + attr
					if _, ok := covered[pair]; ok {
						continue
					}
					if _, ok := seen[pair]; ok {
						continue
					}
					seen[pair] = struct{}{}
					missing = append(missing, fmt.Sprintf("%s (%s fixture, %s(%s))",
						pair, tc.name, r.Kind(), r.Name()))
				}
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("generation writes %d mergeable attribute(s) no case in managedAttrCases() asks "+
			"about, so nothing checks what happens to a hand-authored value there:\n      %s",
			len(missing), strings.Join(missing, "\n      "))
	}
}
