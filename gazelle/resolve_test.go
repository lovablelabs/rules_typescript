package typescript

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// ---- helpers ---------------------------------------------------------------

func (s *programStore) compileImports(pkg string, set srcSet) *ruleImports {
	return &ruleImports{
		edges:      sourceEdges(s.programs[pkg].edgesBySource(), set.library, set.declaration),
		program:    s.programs[pkg],
		candidates: s.programs[pkg].ownedCandidates(set.library, set.declaration),
	}
}

func (s *programStore) testImportsForSources(lock *npmLock,
	pkg, compile, cfg string, set srcSet) *ruleImports {
	imps := s.testImports(lock, pkg, compile, cfg)
	imps.edges = sourceEdges(s.programs[pkg].edgesBySource(), set.library, set.declaration, set.test)
	imps.program = s.programs[pkg]
	imps.candidates = s.programs[pkg].ownedCandidates(set.library, set.declaration, set.test)
	return imps
}

// indexedRule is one rule to put in the RuleIndex: a kind, a target name, the
// Bazel package it lives in, and its srcs.
type indexedRule struct {
	kind   string
	name   string
	pkg    string
	srcs   []string
	deps   []string
	outDir string
	outs   []string
}

func newRule(ir indexedRule) (*rule.Rule, *rule.File) {
	r := rule.NewRule(ir.kind, ir.name)
	if ir.srcs != nil {
		r.SetAttr("srcs", ir.srcs)
	}
	if ir.deps != nil {
		r.SetAttr("deps", ir.deps)
	}
	if ir.outDir != "" {
		r.SetAttr("out_dir", ir.outDir)
	}
	if ir.outs != nil {
		r.SetAttr("outs", ir.outs)
	}
	return r, rule.EmptyFile("BUILD.bazel", ir.pkg)
}

// buildIndex indexes the given rules through the language's own Imports hook,
// which is what makes this exercise importsForRule and the resolver together.
func buildIndex(t *testing.T, c *config.Config, rules ...indexedRule) *resolve.RuleIndex {
	t.Helper()
	lang := &tsLang{}
	ix := resolve.NewRuleIndex(func(*rule.Rule, string) resolve.Resolver { return lang })
	files := map[string]*rule.File{}
	for _, ir := range rules {
		r, f := newRule(ir)
		if files[ir.pkg] == nil {
			files[ir.pkg] = f
		}
		files[ir.pkg].Rules = append(files[ir.pkg].Rules, r)
	}
	for pkg, f := range files {
		getConfig(c).programs.recordBuild(c, pkg, f)
	}
	for _, f := range files {
		for _, r := range f.Rules {
			ix.AddRule(c, r, f)
		}
	}
	ix.Finish()
	return ix
}

func emptyConfig() *config.Config {
	c := config.New()
	c.RepoRoot = "/tmp/fake-repo"
	c.Exts[languageName] = defaultTsConfig()
	(&resolve.Configurer{}).RegisterFlags(nil, "", c)
	return c
}

func loadedEmptyBuild(t *testing.T, buildPath, pkg string) *rule.File {
	t.Helper()
	f, err := rule.LoadData(buildPath, pkg, []byte(""))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// repoWithDirs roots c at a real tree holding dirs, for the rows whose expected

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func specStrings(specs []resolve.ImportSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		if s.Lang != languageName {
			out = append(out, s.Lang+":"+s.Imp)
			continue
		}
		out = append(out, s.Imp)
	}
	return out
}

// ---- importsForRule --------------------------------------------------------

// Every src by its repository path, the key an edge target is looked up by.
func TestImportsForRule_TsCompile(t *testing.T) {
	c := emptyConfig()
	r, f := newRule(indexedRule{
		kind: "ts_compile", name: "components", pkg: "src/components",
		srcs: []string{"index.ts", "Button.tsx", "helpers.ts", "logo.svg"},
	})

	got := specStrings(importsForRule(c, r, f))
	want := []string{
		"src/components/index.ts",
		"src/components/Button.tsx",
		"src/components/helpers.ts",
		"src/components/logo.svg",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("importsForRule(ts_compile) = %v, want %v", got, want)
	}
}

func TestImportsForRule_TsTestIsIndexed(t *testing.T) {
	c := emptyConfig()
	r, f := newRule(indexedRule{
		kind: "ts_test", name: "math_test", pkg: "src/lib",
		srcs: []string{"math.test.ts"},
	})

	got := specStrings(importsForRule(c, r, f))
	want := []string{"src/lib/math.test.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("importsForRule(ts_test) = %v, want %v", got, want)
	}
}

// A ":" pins a file whose name opens a label (labels.go); a label naming a
// producer in the same file expands its outputs, whatever the label's spelling.
func TestImportsForRule_SourceLabelsShareFileIdentity(t *testing.T) {
	for _, kind := range []string{"ts_codegen", "genrule"} {
		for _, prefix := range []string{"", ":", "//web/routes:", "@fixture//web/routes:", "@//web/routes:", "@@//web/routes:"} {
			t.Run(kind+"/"+prefix, func(t *testing.T) {
				c := emptyConfig()
				c.RepoName = "fixture"
				r, f := newRule(indexedRule{
					kind: "ts_compile", name: "routes", pkg: "web/routes",
					srcs: []string{":@{$username}.tsx", prefix + "value.ts", prefix + "gen", "//other:thing.ts", "@npm//:x"},
				})
				producer := rule.NewRule(kind, "gen")
				producer.SetAttr("outs", []string{prefix + "value.ts"})
				f.Rules = append(f.Rules, producer)

				got := specStrings(importsForRule(c, r, f))
				want := []string{"web/routes/@{$username}.tsx", "web/routes/value.ts", "web/routes/value.ts", "other/thing.ts"}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("importsForRule = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestImportsForRule_ProducerLabelsShareCompilerOwnership(t *testing.T) {
	for _, producerKind := range []string{"ts_codegen", "genrule"} {
		for _, compilerPkg := range []string{"generated", "compiled"} {
			for _, source := range []string{"source", "value.ts"} {
				t.Run(producerKind+"/"+compilerPkg+"/"+source, func(t *testing.T) {
					c := emptyConfig()
					c.RepoRoot = t.TempDir()
					c.RepoName = "fixture"
					ix := buildIndex(t, c,
						indexedRule{kind: "ts_compile", name: "chosen", pkg: compilerPkg, srcs: []string{"//generated:" + source}},
						indexedRule{kind: producerKind, name: "source", pkg: "generated", outs: []string{":value.ts", "//generated:other.ts"}},
					)
					for _, file := range []string{"value.ts", "other.ts"} {
						spec := resolve.ImportSpec{Lang: languageName, Imp: "generated/" + file}
						wantHeld := source == "source" || file == "value.ts"
						for _, fromPkg := range []string{"app", compilerPkg} {
							from := label.New(c.RepoName, fromPkg, "chosen")
							got, held := ruleHolding(c, ix, spec, from, sourceRole)
							want := ""
							if wantHeld && fromPkg != compilerPkg {
								want = "//" + compilerPkg + ":chosen"
							}
							if held != wantHeld || got != want {
								t.Errorf("%s from %s: owner = %q, held = %t; want %q, %t", file, from, got, held, want, wantHeld)
							}
						}
					}
				})
			}
		}
	}
}

func TestDeclaredOutputLabelsUseProducerIdentity(t *testing.T) {
	for _, attr := range []string{"out", "outs"} {
		for _, test := range []struct {
			output string
			local  bool
		}{
			{"value.ts", true},
			{":value.ts", true},
			{"//generated:value.ts", true},
			{"@fixture//generated:value.ts", true},
			{"@//generated:value.ts", true},
			{"@@//generated:value.ts", true},
			{"//other:value.ts", false},
			{"@foreign//generated:value.ts", false},
			{"@@foreign//generated:value.ts", false},
			{"@@fixture//generated:value.ts", false},
		} {
			t.Run(attr+"/"+test.output, func(t *testing.T) {
				c := emptyConfig()
				c.RepoRoot = t.TempDir()
				c.RepoName = "fixture"
				s := getConfig(c).programs
				compiler, f := newRule(indexedRule{kind: "ts_compile", name: "compiled", pkg: "generated", srcs: []string{":producer"}})
				producer := rule.NewRule("genrule", "producer")
				if attr == "out" {
					producer.SetAttr(attr, test.output)
				} else {
					producer.SetAttr(attr, []string{test.output})
				}
				f.Rules = append(f.Rules, producer)
				s.recordBuild(c, "generated", f)
				var wantImports []string
				var wantProducer *rule.Rule
				wantPkg := ""
				if test.local {
					wantImports = []string{"generated/value.ts"}
					wantProducer, wantPkg = producer, "generated"
				}
				if got := specStrings(importsForRule(c, compiler, f)); !slices.Equal(got, wantImports) {
					t.Fatalf("importsForRule = %v, want %v", got, wantImports)
				}
				if got, pkg := s.outputProducer("generated/value.ts"); got != wantProducer || pkg != wantPkg {
					t.Fatalf("outputProducer = %v, %q; want %v, %q", got, pkg, wantProducer, wantPkg)
				}
			})
		}
	}
}

func TestRuleHoldingUnusableIndexClaimsCannotHideObservedCompiler(t *testing.T) {
	for _, index := range []struct {
		name string
		pkgs []string
	}{
		{"empty", nil},
		{"unrelated test", []string{"tests"}},
		{"filtered stale compiler", []string{"stale"}},
		{"full", []string{"tests", "stale", "lib"}},
		{"full reversed", []string{"lib", "stale", "tests"}},
	} {
		t.Run(index.name, func(t *testing.T) {
			c := emptyConfig()
			c.RepoRoot = t.TempDir()
			s := getConfig(c).programs
			files := map[string]*rule.File{}
			for _, declaration := range []indexedRule{
				{kind: "ts_compile", name: "owner", pkg: "lib", srcs: []string{"value.d.ts"}},
				{kind: "ts_test", name: "checks", pkg: "tests", srcs: []string{"//lib:value.d.ts"}},
				{kind: "ts_compile", name: "stale", pkg: "stale", srcs: []string{"//lib:value.d.ts"}},
			} {
				r, f := newRule(declaration)
				f.Rules = append(f.Rules, r)
				s.recordBuild(c, declaration.pkg, f)
				files[declaration.pkg] = f
			}
			s.walked["stale"] = true
			ix := resolve.NewRuleIndex(func(*rule.Rule, string) resolve.Resolver { return &tsLang{} })
			for _, pkg := range index.pkgs {
				f := files[pkg]
				ix.AddRule(c, f.Rules[0], f)
			}
			ix.Finish()
			spec := resolve.ImportSpec{Lang: languageName, Imp: "lib/value.d.ts"}
			for _, consumer := range []struct{ pkg, name, owner string }{
				{"app", "app", "//lib:owner"},
				{"lib", "owner", ""},
				{"tests", "checks", ""},
			} {
				from := label.New("", consumer.pkg, consumer.name)
				if got, held := ruleHolding(c, ix, spec, from, sourceRole); got != consumer.owner || !held {
					t.Errorf("%s: owner = %q, held = %t; want %q, true", from, got, held, consumer.owner)
				}
			}
		})
	}
}

func TestRuleHoldingRefreshedSourcesRespectMergedKeeps(t *testing.T) {
	for _, keep := range []string{"none", "item", "attribute", "rule"} {
		t.Run(keep, func(t *testing.T) {
			c := emptyConfig()
			c.RepoRoot = t.TempDir()
			tc := getConfig(c)
			s := tc.programs
			prefix, item, attr := "", "", ""
			switch keep {
			case "item":
				item = " # keep"
			case "attribute":
				attr = " # keep"
			case "rule":
				prefix = "# keep\n"
			}
			f, err := rule.LoadData("lib/BUILD.bazel", "lib", []byte(prefix+`ts_compile(
    name = "lib",
    tsconfig = "tsconfig.json",
    srcs = [
        "index.ts",
        "fallback.ts",`+item+`
        "helper.ts",
    ],`+attr+`
)
`))
			if err != nil {
				t.Fatal(err)
			}
			s.recordBuild(c, "lib", f)
			lang := &tsLang{}
			ix := resolve.NewRuleIndex(func(*rule.Rule, string) resolve.Resolver { return lang })
			ix.AddRule(c, f.Rules[0], f)
			for _, ir := range []indexedRule{
				{kind: "ts_codegen", name: "types", pkg: "generated", outs: []string{"value.d.ts"}},
				{kind: "ts_compile", name: "support", pkg: "support", srcs: []string{"value.ts"}},
			} {
				owner, file := newRule(ir)
				file.Rules = append(file.Rules, owner)
				s.recordBuild(c, ir.pkg, file)
				ix.AddRule(c, owner, file)
			}
			ix.Finish()
			s.visit("lib", []string{"index.ts", "fallback.ts", "helper.ts"})
			s.walked["lib"] = true
			s.visit("support", []string{"value.ts"})
			s.walked["support"] = true
			s.record(&program{dir: "support", Listing: explainfiles.Listing{Files: []string{"support/value.ts"}}})
			observation := &program{dir: "lib", Listing: explainfiles.Listing{
				Roots: []string{"lib/index.ts"},
				Files: []string{"lib/index.ts", "lib/fallback.ts", "lib/helper.ts", "support/value.ts", "generated/value.d.ts", "generated/stale.ts"},
				Edges: []explainfiles.Edge{
					importEdge("lib/index.ts", "#value", "lib/fallback.ts"),
					importEdge("lib/fallback.ts", "./helper", "lib/helper.ts"),
					importEdge("lib/helper.ts", "../support/value", "support/value.ts"),
					importEdge("lib/fallback.ts", "../generated/value", "generated/value.d.ts"),
					importEdge("generated/value.d.ts", "./stale", "generated/stale.ts"),
				},
			}, candidates: []resolutionCandidate{{from: "lib/index.ts", specifier: "#value", path: "generated/value.d.ts", file: true, resolved: "lib/fallback.ts"}}}
			r := rule.NewRule("ts_compile", "lib")
			r.SetAttr("tsconfig", "tsconfig.json")
			imps := &ruleImports{}
			s.inputs["lib"] = programInput{config: c, program: observation, refresh: func() {
				refreshProgramRules(language.GenerateArgs{Config: c, Rel: "lib", Dir: filepath.Join(c.RepoRoot, "lib"), File: f}, tc,
					language.GenerateResult{Gen: []*rule.Rule{r}, Imports: []any{imps}})
			}}
			s.selectInputs(ix)
			logged := captureLog(t, func() { resolveEdges(c, ix, r, imps, label.New("", "lib", "lib")) })
			if logged != "" {
				t.Fatal(logged)
			}
			wantSrcs, wantDeps := []string{"index.ts"}, []string{"//generated:types"}
			if keep != "none" {
				wantSrcs = []string{"fallback.ts", "helper.ts", "index.ts"}
				wantDeps = append(wantDeps, "//support")
			}
			wantStrings(t, "retained source closure", r.AttrStrings("srcs"), wantSrcs)
			wantStrings(t, "retained dependency closure", r.AttrStrings("deps"), wantDeps)
			for _, owner := range []*rule.Rule{f.Rules[0], r} {
				s.emission.setRule(emissionLabel(c.RepoName, "lib", ":lib"), owner)
				for _, file := range []string{"index.ts", "fallback.ts", "helper.ts"} {
					for _, pkg := range []string{"lib", "client"} {
						from := label.New("", pkg, "lib")
						got, held := ruleHolding(c, ix, resolve.ImportSpec{Lang: languageName, Imp: "lib/" + file}, from, sourceRole)
						wantHeld := file == "index.ts" || keep != "none"
						want := ""
						if wantHeld && pkg != "lib" {
							want = "//lib"
						}
						if got != want || held != wantHeld {
							t.Errorf("%s from %s: owner = %q, held = %t; want %q, %t", file, from, got, held, want, wantHeld)
						}
					}
				}
			}
		})
	}
}

func TestProgramSelectionRetainsOnlyMetadataRequiredBeforeTheSelectedProducer(t *testing.T) {
	for _, terminalOverride := range []bool{false, true} {
		for _, retainUnused := range []bool{false, true} {
			t.Run(fmt.Sprintf("terminal_override=%t/retained_unused=%t", terminalOverride, retainUnused), func(t *testing.T) {
				c := emptyConfig()
				s := getConfig(c).programs
				s.visit("fallback", []string{"value.ts"})
				s.index = buildIndex(t, c, indexedRule{kind: "ts_codegen", name: "types", pkg: "generated", outs: []string{"value.d.ts"}})
				if terminalOverride {
					f, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve typescript fallback/value.ts //owners:chosen\n"))
					if err != nil {
						t.Fatal(err)
					}
					(&resolve.Configurer{}).Configure(c, "", f)
				}
				p := &program{config: "app/tsconfig.json", Listing: explainfiles.Listing{
					Roots: []string{"app/index.ts"}, Files: []string{"app/index.ts", "app/unused.ts", "fallback/value.ts"},
					Edges: []explainfiles.Edge{importEdge("app/index.ts", "#value", "fallback/value.ts")},
				}, candidates: []resolutionCandidate{
					{from: "app/index.ts", specifier: "#value", path: "missing/value.ts", file: true, resolved: "fallback/value.ts"},
					{from: "app/index.ts", specifier: "#value", path: "settings/package.json", metadata: true, resolved: "fallback/value.ts"},
					{from: "app/index.ts", specifier: "#value", path: "generated/value.d.ts", file: true, resolved: "fallback/value.ts"},
					{from: "app/index.ts", specifier: "#value", path: "fallback/package.json", metadata: true, resolved: "fallback/value.ts"},
					{from: "app/index.ts", specifier: "#value", path: "fallback/value.ts", file: true, resolved: "fallback/value.ts"},
					{from: "app/unused.ts", specifier: "./other", path: "unused/package.json", metadata: true, block: 1},
					{from: "app/tsconfig.json", specifier: "./types", path: "types/package.json", metadata: true, kind: explainfiles.TypeReference, block: 2},
				}}
				if retainUnused {
					p.Roots = append(p.Roots, "app/unused.ts")
				}
				s.selectProgram(c, p)
				var metadata []string
				for _, candidate := range p.candidates {
					if candidate.metadata {
						metadata = append(metadata, candidate.path)
					}
				}
				want := []string{"settings/package.json"}
				selected := "generated/value.d.ts"
				if terminalOverride {
					want = append(want, "fallback/package.json")
					selected = "fallback/value.ts"
				}
				if retainUnused {
					want = append(want, "unused/package.json")
				}
				want = append(want, "types/package.json")
				wantStrings(t, "selected metadata", metadata, want)
				wantEdges := []explainfiles.Edge{importEdge("app/index.ts", "#value", selected)}
				if !slices.Equal(p.Edges, wantEdges) {
					t.Fatalf("metadata became a source edge: got %+v, want %+v", p.Edges, wantEdges)
				}
			})
		}
	}
}

func TestImportsForRule_UnknownKindIsNotImportable(t *testing.T) {
	c := emptyConfig()
	for _, kind := range []string{"genrule", "sh_binary", "filegroup"} {
		r, f := newRule(indexedRule{
			kind: kind, name: "thing", pkg: "src/app", srcs: []string{"index.ts"},
		})
		if got := importsForRule(c, r, f); got != nil {
			t.Errorf("importsForRule(%s) = %v, want nil (kind must not be indexed)",
				kind, got)
		}
	}
}

// ---- ts_codegen out_dir trees ----------------------------------------------

func TestImportsForRule_CodegenOutDirIsIndexed(t *testing.T) {
	c := emptyConfig()
	r, f := newRule(indexedRule{
		kind: "ts_codegen", name: "paraglide_messages", pkg: "web",
		srcs: []string{"i18n/settings.json"}, outDir: "shared/i18n/compiled",
	})

	got := specStrings(importsForRule(c, r, f))
	want := []string{
		"web/shared/i18n/compiled",
		"ts_codegen_tree:web/shared/i18n/compiled",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("importsForRule(ts_codegen) = %v, want %v", got, want)
	}
}

// Deepest root wins, so a tree generated inside another tree's directory
// answers for its own subtree instead of the outer one swallowing it.
func TestResolveCodegenTree_NearestRootWins(t *testing.T) {
	c := emptyConfig()
	outer := indexedRule{kind: "ts_codegen", name: "outer", srcs: []string{"a.json"}, outDir: "web/gen"}
	inner := indexedRule{kind: "ts_codegen", name: "inner", pkg: "web", srcs: []string{"b.json"}, outDir: "gen/inner"}
	ix := buildIndex(t, c, outer, inner)
	nativeInner := inner
	nativeInner.name = "indexed_inner"
	from := label.New("", "src/app", "app")

	for _, tc := range []struct {
		name  string
		index *resolve.RuleIndex
		inner string
	}{
		{"nil", nil, "inner"},
		{"empty", buildIndex(t, c), "inner"},
		{"partial", buildIndex(t, emptyConfig(), outer), "inner"},
		{"full", ix, "inner"},
		{"same_root", buildIndex(t, emptyConfig(), outer, nativeInner), "indexed_inner"},
	} {
		for imp, want := range map[string]string{
			"web/gen/thing":       "//:outer",
			"web/gen/inner/thing": "//web:" + tc.inner,
			"web/other/thing":     "",
		} {
			if got, known := resolveCodegenTree(getConfig(c).programs, tc.index, imp, from); got != want || known != (want != "") {
				t.Errorf("%s index: resolveCodegenTree(%q) = %q, %t; want %q, %t", tc.name, imp, got, known, want, want != "")
			}
		}
		if got, known := resolveCodegenTree(getConfig(c).programs, tc.index, "web/gen/inner/thing", label.New("", "web", tc.inner)); got != "" || !known {
			t.Errorf("%s index: self-owned tree lost generated identity: %q, %t; want \"\", true", tc.name, got, known)
		}
	}
}

// The walk climbs a namespaced key, so a ts_compile indexing the very path a
// specifier's parent names can never be reached by prefix.
func TestResolveCodegenTree_OnlyReachesCodegenTrees(t *testing.T) {
	c := emptyConfig()
	ix := buildIndex(t, c, indexedRule{
		kind: "ts_compile", name: "utils", pkg: "src/utils", srcs: []string{"index.ts"},
	})
	from := label.New("", "src/app", "app")

	if got, known := resolveCodegenTree(getConfig(c).programs, ix, "src/utils/missing", from); got != "" || known {
		t.Errorf("resolveCodegenTree reached a ts_compile: %q, %t; want \"\", false", got, known)
	}
}

// A relative specifier that escapes the workspace root must not walk past it.
func TestResolveCodegenTree_StopsAtTheRoot(t *testing.T) {
	c := emptyConfig()
	ix := buildIndex(t, c)
	from := label.New("", "src/app", "app")

	for _, imp := range []string{"", ".", "..", "../thing", "/abs/thing", "thing"} {
		if got, known := resolveCodegenTree(getConfig(c).programs, ix, imp, from); got != "" || known {
			t.Errorf("resolveCodegenTree(%q) = %q, %t; want \"\", false", imp, got, known)
		}
	}
}

// ---- the listing as the resolver -------------------------------------------

const (
	storeJsxRuntime = store + "@types/react/19.0.0/aaa/node_modules/" +
		"@types/react/jsx-runtime.d.ts"
	storeTypesNode = store + "@types/node/22.20.1/bbb/node_modules/" +
		"@types/node/index.d.ts"
	storeTypescript = store + "@/typescript/5.9.2/kkk/node_modules/" +
		"typescript/lib/typescript.d.ts"
	storeVite = store + "@/vite/8.2.2/ccc/node_modules/vite/dist/node/" +
		"index.d.ts"
)

func viaLine(spec, from, packageID string) string {
	line := `   Imported via "` + spec + `" from file '` + from + `'`
	if packageID != "" {
		line += ` with packageId '` + packageID + `'`
	}
	return line + "\n"
}

func includeLine(pkg string) string {
	return "   Matched by include pattern 'src/**/*' in '" + pkg +
		"/tsconfig.json'\n"
}

// web's program: npm edges under every reason form, a member by name and by
// path, a self edge, two unowned JSON files and the toolchain's lib.
var webListing = storeZod + "\n" +
	viaLine("zod", "web/src/a.ts", "zod/index.d.ts@3.24.2") +
	storeMarked + "\n" +
	viaLine("marked", "web/src/a.ts", "marked/lib/marked.d.ts@15.0.12") +
	storeViteClient + "\n" +
	"   Type library referenced via 'vite/client' from file " +
	"'web/src/vite-env.d.ts' with packageId 'vite/client.d.ts@8.2.2'\n" +
	storeJsxRuntime + "\n" +
	"   Imported via \"react/jsx-runtime\" from file 'web/src/App.tsx' with " +
	"packageId '@types/react/jsx-runtime.d.ts@19.0.0' to import 'jsx' and " +
	"'jsxs' factory functions\n" +
	storeTypescript + "\n" +
	viaLine("typescript", "web/src/a.test.ts",
		"typescript/lib/typescript.d.ts@5.9.2") +
	storeTypesNode + "\n" +
	"   Entry point of type library 'node' specified in compilerOptions with " +
	"packageId '@types/node/index.d.ts@22.20.1'\n" +
	libES5 + "\n" +
	"   Default library for target 'ES2022'\n" +
	"packages/ui/src/index.ts\n" +
	viaLine("@acme/ui", "web/src/a.ts", "@acme/ui-src/src/index.ts@0.0.0") +
	"packages/ui/src/util.ts\n" +
	viaLine("../../packages/ui/src/util", "web/src/a.ts", "") +
	"go/fixtures/x.json\n" +
	viaLine("../../go/fixtures/x.json", "web/src/a.ts", "") +
	"packages/figma/manifest.json\n" +
	viaLine("../../packages/figma/manifest.json", "web/src/a.ts", "") +
	"web/src/a.ts\n" + includeLine("web") +
	viaLine("./a", "web/src/a.test.ts", "") +
	"web/src/b.ts\n" + includeLine("web") +
	viaLine("./b", "web/src/a.ts", "") +
	"web/src/App.tsx\n" + includeLine("web") +
	"web/src/vite-env.d.ts\n" + includeLine("web") +
	"web/src/a.test.ts\n" + includeLine("web")

// packages/lib's program: the member's own name through an exports subpath,
// from a library file and from a test file (the canvas-sdk shape).
var libListing = "packages/lib/src/index.ts\n" + includeLine("packages/lib") +
	"packages/lib/src/wire/index.ts\n" + includeLine("packages/lib") +
	viaLine("@acme/lib/wire", "packages/lib/src/index.ts",
		"@acme/lib/src/wire/index.ts@0.0.0") +
	viaLine("@acme/lib/wire", "packages/lib/src/x.test.ts",
		"@acme/lib/src/wire/index.ts@0.0.0") +
	"packages/lib/src/x.test.ts\n" + includeLine("packages/lib")

var edgeListings = map[string]string{
	"web":          webListing,
	"packages/lib": libListing,
	"packages/ui": listingOf("packages/ui", "packages/ui/src/index.ts",
		"packages/ui/src/util.ts"),
	"packages/figma": listingOf("packages/figma", "packages/figma/src/code.ts"),
}

// edgeRepo is a run from the root over npmRepo's workspace -- its lockfile and
// manifests, an install, web/package.json's dependencies -- with the listings.
func edgeRepo(t *testing.T, listings map[string]string,
) (*config.Config, *tsConfig) {
	t.Helper()
	root, _ := npmRepo(t)
	writeFile(t, filepath.Join(root, "web/package.json"), `{"name": "web-app",
	  "dependencies": {"marked": "^15.0.0"},
	  "devDependencies": {"@types/react": "^19.0.0", "typescript": "5.9.2"}}`)
	writeFile(t, filepath.Join(root, "node_modules/.modules.yaml"),
		"layoutVersion: 5\n")
	c := config.New()
	c.RepoRoot = root
	(&resolve.Configurer{}).RegisterFlags(nil, "", c)
	configureTsConfig(c, "", nil, nil)
	tc := getConfig(c)
	tc.programs.visit("", []string{"package.json"})
	tc.programs.walked[""] = true
	for dir, text := range listings {
		p := programOf(t, dir, text)
		for _, f := range p.Files {
			if !firstParty(f) {
				continue
			}
			for d := parentDir(f); d != ""; d = parentDir(d) {
				if !tc.programs.walked[d] {
					tc.programs.visit(d, nil)
					tc.programs.walked[d] = true
				}
			}
			tc.programs.files[parentDir(f)] = append(tc.programs.files[parentDir(f)], path.Base(f))
		}
		tc.programs.record(p)
	}
	for dir := range tc.lock.importers {
		if _, err := os.Stat(filepath.Join(root, dir, "package.json")); err == nil && !slices.Contains(tc.programs.files[dir], "package.json") {
			tc.programs.files[dir] = append(tc.programs.files[dir], "package.json")
		}
	}
	tc.programs.files["web"] = append(tc.programs.files["web"], "vitest.config.mts")
	return c, tc
}

// The rules a run over edgeListings writes, as the index sees them.
var edgeRules = []indexedRule{
	{kind: "ts_compile", name: "web", pkg: "web",
		srcs: []string{"src/App.tsx", "src/a.ts", "src/b.ts", "src/vite-env.d.ts"}},
	{kind: "ts_test", name: "web_test", pkg: "web",
		srcs: []string{"src/a.test.ts", "src/vite-env.d.ts"}},
	{kind: "ts_compile", name: "ui", pkg: "packages/ui",
		srcs: []string{"src/index.ts", "src/util.ts"}},
	{kind: "ts_compile", name: "lib", pkg: "packages/lib",
		srcs: []string{"src/index.ts", "src/wire/index.ts"}},
	{kind: "ts_test", name: "lib_test", pkg: "packages/lib",
		srcs: []string{"src/x.test.ts"}},
	{kind: "ts_compile", name: "figma", pkg: "packages/figma",
		srcs: []string{"src/code.ts"}},
}

func resolveEdgesOf(t *testing.T, c *config.Config, ix *resolve.RuleIndex,
	kind, pkg, name string, imps *ruleImports) (*rule.Rule, string) {
	t.Helper()
	// Native generation selects the program before deriving this rule's
	// imports. These resolver fixtures supply that rule's entry edges directly.
	s := getConfig(c).programs
	previous := s.programs[pkg]
	p := &program{}
	if previous != nil {
		*p = *previous
		p.Edges = slices.Clone(previous.Edges)
		p.candidates = slices.Clone(previous.candidates)
	}
	if imps.program == nil {
		p.Types = nil
		p.Implicit = nil
	}
	p.Roots = nil
	for _, edge := range imps.edges {
		p.Roots = append(p.Roots, edge.From)
		if !slices.Contains(p.Edges, edge) {
			p.Edges = append(p.Edges, edge)
		}
	}
	for _, candidate := range imps.candidates {
		p.Roots = append(p.Roots, candidate.from)
		if !slices.Contains(p.candidates, candidate) {
			p.candidates = append(p.candidates, candidate)
		}
	}
	s.selectProgram(c, p)
	s.programs[pkg] = p
	defer func() { s.programs[pkg] = previous }()
	selected := *imps
	if selected.program == nil || selected.program == previous {
		selected.program = p
	}
	selected.edges = sourceEdges(p.edgesBySource(), p.Roots)
	selected.candidates = p.ownedCandidates(p.Roots)
	r := rule.NewRule(kind, name)
	from := label.New("", pkg, name)
	logged := captureLog(t, func() { resolveEdges(c, ix, r, &selected, from) })
	return r, logged
}

// One label per edge target, by where it sits and who owns it; the test file's
// edge is not the ts_compile's, and no types attribute is written.
func TestResolveEdges_CompileDepsFromTheListing(t *testing.T) {
	c, tc := edgeRepo(t, edgeListings)
	ix := buildIndex(t, c, edgeRules...)
	s := tc.programs

	imps := s.compileImports("web", s.srcs("web", tc))
	for _, e := range imps.edges {
		if e.From == "web/src/a.test.ts" {
			t.Errorf("compileImports carries the test file's edge %+v", e)
		}
	}
	r, logged := resolveEdgesOf(t, c, ix, "ts_compile", "web", "web", imps)
	want := []string{
		"//packages/ui",
		":node_modules/@acme/ui",
		"@npm//:types_node",
		"@npm//:vite",
		"@npm//:zod",
		"@npm//web:marked",
		"@npm//web:react",
	}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %q, want %q", got, want)
	}
	if r.Attr("types") != nil {
		t.Errorf("types = %v, want none: the tsconfig owns it", r.Attr("types"))
	}
	for _, want := range []string{
		"web/src/a.ts imports go/fixtures/x.json: no package owns it: " +
			"no tsconfig.json above it lists a file; no dep",
		"web/src/a.ts imports packages/figma/manifest.json: no package " +
			"owns it: packages/figma/tsconfig.json, the nearest, does not " +
			"list it; no dep",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
	if n := strings.Count(logged, "\n"); n != 2 {
		t.Errorf("%d log lines, want the two unowned reports:\n%s", n, logged)
	}
}

// A src the holding package's program does not list -- a regular file under
// it, by the walk -- is that rule's dep: the index answers before ownership.
func TestResolveEdges_DepOnAnUnlistedSrc(t *testing.T) {
	c, tc := edgeRepo(t, edgeListings)
	rules := append([]indexedRule{}, edgeRules...)
	for i, ir := range rules {
		if ir.pkg == "packages/figma" {
			rules[i].srcs = []string{"manifest.json", "src/code.ts"}
		}
	}
	ix := buildIndex(t, c, rules...)
	s := tc.programs
	r, logged := resolveEdgesOf(t, c, ix, "ts_compile", "web", "web",
		s.compileImports("web", s.srcs("web", tc)))
	want := []string{
		"//packages/figma",
		"//packages/ui",
		":node_modules/@acme/ui",
		"@npm//:types_node",
		"@npm//:vite",
		"@npm//:zod",
		"@npm//web:marked",
		"@npm//web:react",
	}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %q, want %q", got, want)
	}
	if strings.Contains(logged, "manifest.json") ||
		strings.Count(logged, "\n") != 1 {
		t.Errorf("log, want x.json's report alone:\n%s", logged)
	}
	// A test's membership supplies no TsInfo to another target.
	writeFile(t, filepath.Join(c.RepoRoot, "web/src/a.test.ts"), "import type { CompilerOptions } from 'typescript'; export type Options = CompilerOptions;\n")
	// The rewritten source no longer imports a.ts; its listing must agree.
	s.record(programOf(t, "web", listingOf("web", "web/src/b.ts", "web/src/a.test.ts")+
		storeTypescript+"\n"+viaLine("typescript", "web/src/a.test.ts", "typescript/lib/typescript.d.ts@5.9.2")))
	s.emission.rules[emissionLabel(c.RepoName, "web", ":web")].SetAttr("srcs", []string{"src/b.ts"})
	r, logged = resolveEdgesOf(t, c, ix, "ts_compile", "web", "web",
		&ruleImports{edges: []explainfiles.Edge{
			importEdge("web/src/b.ts", "./a.test", "web/src/a.test.ts")}})
	wantStrings(t, "test-owned source deps", r.AttrStrings("deps"), []string{"@npm//:typescript"})
	if logged != "" {
		t.Errorf("log %q; want none", logged)
	}
	wantStrings(t, "test-owned source", r.AttrStrings("srcs"), []string{":src/a.test.ts", "src/b.ts"})
	wantLabels(t, "test-owned source scope", r.AttrStrings("package_scopes"), []string{"package.json"})
}

func TestGeneratedDeclarationSharedWithTestSelectsExporter(t *testing.T) {
	c := emptyConfig()
	ix := buildIndex(t, c,
		indexedRule{kind: "ts_codegen", name: "types", pkg: "app", outs: []string{"value.d.ts"}},
		indexedRule{kind: "ts_test", name: "shared_test", pkg: "app", srcs: []string{":types"}},
	)
	for _, test := range []struct{ from, want string }{
		{"app", ":types"},
		{"shared_test", ""},
		{"types", ""},
	} {
		dep, generated := generatedDep(c, ix, getConfig(c), "app/value.d.ts", label.New("", "app", test.from))
		if dep != test.want || !generated {
			t.Errorf("from %s: dep = %q, generated = %t; want %q, true", test.from, dep, generated, test.want)
		}
	}
}

// ts_test.deps: the ts_compile, every owned file's edges, the vitest config's
// and the manifest union in D7 spelling, with no label said twice.
func TestResolveEdges_TestDepsCarryTheRuntime(t *testing.T) {
	c, tc := edgeRepo(t, edgeListings)
	ix := buildIndex(t, c, edgeRules...)
	s := tc.programs
	const cfg = "web/vitest.config.mts"
	s.vitestPrograms = configPrograms(map[string][]explainfiles.Edge{
		cfg: {importEdge(cfg, "vite", storeVite)}})

	set := s.srcs("web", tc)
	imps := s.testImportsForSources(tc.lock, "web", ":web", cfg, set)
	if imps.config != cfg {
		t.Errorf("config = %q, want %q", imps.config, cfg)
	}
	r, logged := resolveEdgesOf(t, c, ix, "ts_test", "web", "web_test", imps)
	want := []string{
		"//packages/ui",
		":node_modules/@acme/ui",
		":web",
		"@npm//:types_node",
		"@npm//:typescript",
		"@npm//:vite",
		"@npm//:zod",
		"@npm//web:marked",
		"@npm//web:react",
		"@npm//web:types_react",
	}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %q, want %q", got, want)
	}
	if n := strings.Count(logged, "\n"); n != 2 {
		t.Errorf("%d log lines, want the two unowned reports:\n%s", n, logged)
	}
	wantLabels(t, "config root retains its runtime scope without module edges", r.AttrStrings("config_srcs"), []string{"package.json"})
}

// ts_test.config_srcs: the modules the config's closure reaches, spelled from
// the test's package; one outside the config's package is said and gets none.
func TestResolveEdges_ConfigSrcsAreTheConfigsModules(t *testing.T) {
	c, tc := edgeRepo(t, edgeListings)
	ix := buildIndex(t, c, edgeRules...)
	s := tc.programs
	const (
		cfg     = "web/vitest.config.mts"
		plugin  = "web/plugins/define.ts"
		meta    = "web/plugins/meta.json"
		outside = "shared/vitest.base.ts"
	)
	s.visit("web/plugins", []string{"define.ts", "meta.json"})
	s.visit("shared", []string{"vitest.base.ts"})
	s.vitestPrograms = configPrograms(map[string][]explainfiles.Edge{
		cfg: {
			importEdge(cfg, "./plugins/define", plugin),
			importEdge(cfg, "../shared/vitest.base", outside),
		},
		plugin: {
			importEdge(plugin, "./meta.json", meta),
			importEdge(plugin, "zod", storeZod),
		},
		outside: {importEdge(outside, "vite", storeVite)},
	})
	set := s.srcs("web", tc)
	imps := s.testImportsForSources(tc.lock, "web", ":web", cfg, set)
	r, logged := resolveEdgesOf(t, c, ix, "ts_test", "web", "web_test", imps)
	want := []string{"package.json", "plugins/define.ts", "plugins/meta.json"}
	wantLabels(t, "config_srcs", r.AttrStrings("config_srcs"), want)
	for _, dep := range []string{"@npm//:zod", "@npm//:vite"} {
		if !hasLabel(r.AttrStrings("deps"), dep) {
			t.Errorf("deps %q lack %s, the closure's npm edge",
				r.AttrStrings("deps"), dep)
		}
	}
	if !strings.Contains(logged, outside) ||
		!strings.Contains(logged, "config_srcs") {
		t.Errorf("log, want %s said as outside the config's package:\n%s",
			outside, logged)
	}

	// The same config from a test in a package below: the config's package's.
	r, _ = resolveEdgesOf(t, c, ix, "ts_test", "web/test", "test_test",
		&ruleImports{config: cfg})
	want = []string{"//web:package.json", "//web:plugins/define.ts", "//web:plugins/meta.json"}
	wantLabels(t, "config_srcs from web/test", r.AttrStrings("config_srcs"), want)
}

func TestConfigDependenciesRetainTheirImporterWithoutTestProgram(t *testing.T) {
	for _, test := range []struct {
		name, consumer string
		program, kept  bool
	}{
		{"ordinary program", "consumer", true, false},
		{"nil program", "consumer", false, false},
		{"shared importer", "settings/test", true, false},
		{"kept deps omit config", "consumer", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, tc := edgeRepo(t, nil)
			tc.lock = &npmLock{
				names: map[string]bool{"dependency": true},
				importers: map[string]*pnpmImporter{
					"":         {deps: map[string]string{}},
					"consumer": {deps: map[string]string{"dependency": "1.0.0"}},
					"settings": {deps: map[string]string{"dependency": "2.0.0"}},
				},
			}
			const cfg, helper = "settings/vitest.config.mts", "settings/helper.mjs"
			entry := test.consumer + "/index.test.ts"
			for _, file := range []string{cfg, helper, entry} {
				writeFile(t, filepath.Join(c.RepoRoot, file), "")
			}
			tc.programs.visit("settings", []string{path.Base(cfg), path.Base(helper)})
			tc.programs.visit(test.consumer, []string{path.Base(entry)})
			ix := buildIndex(t, c, indexedRule{kind: "node_modules", name: "node_modules", pkg: "settings"})
			if test.kept {
				f, err := rule.LoadData("BUILD.bazel", test.consumer, []byte("ts_test(name = \"test\", deps = [], # keep\n)\n"))
				if err != nil {
					t.Fatal(err)
				}
				tc.programs.recordBuild(c, test.consumer, f)
			}
			configEdge := importEdge(helper, "dependency", "settings/node_modules/dependency/index.d.ts")
			tc.programs.vitestPrograms = configPrograms(map[string][]explainfiles.Edge{
				cfg:    {importEdge(cfg, "./helper.mjs", helper)},
				helper: {configEdge},
			})
			imps := &ruleImports{config: cfg}
			wantDeps := []string{"@npm//settings:dependency"}
			if test.program {
				importer := tc.lock.importerAbove(test.consumer)
				imps.edges = []explainfiles.Edge{importEdge(entry, "dependency", importer+"/node_modules/dependency/index.d.ts")}
				imps.program = &program{Listing: explainfiles.Listing{Roots: []string{entry}, Edges: imps.edges}}
				if importer != "settings" {
					wantDeps = append(wantDeps, "@npm//consumer:dependency")
				}
			}
			r := rule.NewRule("ts_test", "test")
			r.SetAttr("srcs", []string{"index.test.ts"})
			from := label.New("", test.consumer, r.Name())
			deps := resolveEdges(c, ix, r, imps, from)
			retainSourceImporters(c, r, from, imps.program, deps)
			var wantContexts []string
			if test.consumer == "consumer" && !test.kept {
				wantContexts = []string{"//settings:node_modules"}
			}
			wantLabels(t, "config dependency lost its supplying importer", r.AttrStrings("source_node_modules"), wantContexts)
			wantLabels(t, "distinct npm resolutions remain declared", r.AttrStrings("deps"), wantDeps)
			wantStrings(t, "config helpers do not become test sources", r.AttrStrings("srcs"), []string{"index.test.ts"})
			if !hasLabel(r.AttrStrings("config_srcs"), "//settings:helper.mjs") {
				t.Fatalf("config helper lost its source identity: %v", r.AttrStrings("config_srcs"))
			}
		})
	}
}

func TestKeptConfigImporterCannotOmitItsSupplyingContext(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":               "module(name = \"config_importer\")\n",
		"node_modules/.modules.yaml": "layoutVersion: 5\n",
		pnpmLockfileName: `lockfileVersion: '9.0'
importers:
  .: {}
  app:
    dependencies:
      dependency: {specifier: 1.0.0, version: 1.0.0}
  settings:
    dependencies:
      dependency: {specifier: 2.0.0, version: 2.0.0}
packages:
  dependency@1.0.0: {}
  dependency@2.0.0: {}
snapshots:
  dependency@1.0.0: {}
  dependency@2.0.0: {}
`,
		"app/tsconfig.json":          `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.test.ts"]}`,
		"app/index.test.ts":          "import { value } from 'dependency'; export { value };\n",
		"app/package.json":           `{"dependencies":{"dependency":"1.0.0"}}`,
		"settings/package.json":      `{"dependencies":{"dependency":"2.0.0"}}`,
		"settings/vitest.config.mts": "import { value } from './helper.mjs'; export default { value };\n",
		"settings/helper.mjs":        "export { value } from 'dependency';\n",
		"settings/BUILD.bazel":       "exports_files([\"vitest.config.mts\", \"helper.mjs\", \"package.json\"], visibility = [\"//visibility:public\"])\n",
	})
	for _, pkg := range []string{"app", "settings"} {
		writeFile(t, filepath.Join(root, pkg, "node_modules/dependency/package.json"), `{"name":"dependency","types":"index.d.ts"}`)
		writeFile(t, filepath.Join(root, pkg, "node_modules/dependency/index.d.ts"), "export declare const value: number;\n")
	}
	build := loadDefs + `"ts_test")
ts_test(
    name = "app_test",
    srcs = ["index.test.ts"],
    config = "//settings:vitest.config.mts", # keep
)
`
	writeFile(t, filepath.Join(root, "app/BUILD.bazel"), build)
	for _, args := range [][]string{nil, {"-index=false", "app"}} {
		output, err := protoGazelle(t, root, args...)
		if err != nil {
			t.Fatalf("sibling config importers %v: %v\n%s", args, err, output)
		}
		r := onDiskRule(t, root, "app", "ts_test", "app_test")
		wantLabels(t, "config dependency lost its supplying importer", r.AttrStrings("source_node_modules"), []string{"//settings:node_modules"})
		wantLabels(t, "test and config retain distinct dependencies", r.AttrStrings("deps"), []string{"@npm//app:dependency", "@npm//settings:dependency"})
	}
	for _, context := range []string{"[]", `["//settings:node_modules"]`} {
		writeFile(t, filepath.Join(root, "app/BUILD.bazel"), strings.Replace(build, "    config =", "    source_node_modules = "+context+", # keep\n    config =", 1))
		before := buildFileBytes(t, root)
		output, err := protoGazelle(t, root, "-index=false", "app")
		if context == "[]" {
			if err == nil || !strings.Contains(output, "kept source_node_modules") || !strings.Contains(output, "//settings:node_modules") {
				t.Fatalf("kept config importer omission was not rejected: %v\n%s", err, output)
			}
			if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
				t.Fatalf("rejected config importer changed BUILD files: %s", diff)
			}
		} else if err != nil {
			t.Fatalf("declared config importer was rejected: %v\n%s", err, output)
		}
	}
}

func TestConfigInputsRetainAncestorScopesOnlyForAdmittedModules(t *testing.T) {
	c := emptyConfig()
	c.RepoRoot = writeTree(t, map[string]string{
		"package.json":             `{"type":"module"}`,
		"config/vitest.config.mts": "export default {};",
		"config/helper.ts":         "export const value = 1;",
		"outside/helper.ts":        "export const value = 2;",
		"outside/package.json":     `{"type":"module"}`,
	})
	s := getConfig(c).programs
	s.visit("", []string{"package.json"})
	s.visit("config", []string{"vitest.config.mts", "helper.ts"})
	s.visit("outside", []string{"helper.ts"})
	s.recordBuild(c, "", rule.EmptyFile("BUILD.bazel", ""))
	from := label.New("", "app", "app_test")
	var got []string
	captureLog(t, func() {
		got, _ = configSrcLabels(c, nil, []string{"config/vitest.config.mts", "config/helper.ts", "outside/helper.ts"}, "config/vitest.config.mts", from, nil)
	})
	wantLabels(t, "admitted config Files retain the ancestor scope", got, []string{"//:config/helper.ts", "//:package.json"})
}

func TestResolveEdges_ConfigSrcsDoNotCrossBazelPackages(t *testing.T) {
	for _, boundary := range []string{"none", "BUILD", "BUILD.bazel", "generated program", "generated importer", "generated tsconfig", "generated Go only", "alternate BUILD directory"} {
		t.Run(boundary, func(t *testing.T) {
			c, tc := edgeRepo(t, edgeListings)
			const cfg = "web/vitest.config.mts"
			const plugin = "web/plugins/nested/define.ts"
			const pkg = "web/plugins"
			wantPkg := "web"
			args := language.GenerateArgs{Config: c, Rel: pkg, Dir: filepath.Join(c.RepoRoot, pkg)}
			switch boundary {
			case "BUILD", "BUILD.bazel":
				args.File = loadedEmptyBuild(t, filepath.Join(c.RepoRoot, pkg, boundary), pkg)
				wantPkg = pkg
			case "generated program":
				tc.programs.record(programOf(t, pkg, listingOf(pkg, plugin)))
				wantPkg = pkg
			case "generated importer":
				tc.lock.importers[pkg] = &pnpmImporter{}
				wantPkg = pkg
			case "generated tsconfig":
				writeFile(t, filepath.Join(c.RepoRoot, pkg, "tsconfig.json"), "{}")
				tc.programs.extended[pkg] = true
				wantPkg = pkg
			case "generated Go only":
				args.OtherGen = []*rule.Rule{rule.NewRule("go_library", "plugins")}
				wantPkg = pkg
			case "alternate BUILD directory":
				c.ReadBuildFilesDir = t.TempDir()
				args.File = loadedEmptyBuild(t, filepath.Join(c.ReadBuildFilesDir, pkg, "BUILD.bazel"), pkg)
				wantPkg = pkg
			}
			tc.programs.recordBuild(c, pkg, args.File)
			tc.programs.visit(pkg, args.RegularFiles)
			generateRules(args)
			tc.programs.visit("web/plugins/nested", []string{"define.ts"})
			tc.programs.vitestPrograms = configPrograms(map[string][]explainfiles.Edge{
				cfg: {importEdge(cfg, "./plugins/nested/define", plugin)},
			})
			ix := buildIndex(t, c, edgeRules...)
			for _, from := range []string{"web", "web/test", pkg} {
				r, _ := resolveEdgesOf(t, c, ix, "ts_test", from, "test",
					&ruleImports{config: cfg})
				name := strings.TrimPrefix(plugin, wantPkg+"/")
				want := "//" + wantPkg + ":" + name
				if from == wantPkg {
					want = name
				}
				scope := "//web:package.json"
				if from == "web" {
					scope = "package.json"
				}
				wantLabels(t, "config modules and scope retain their Bazel packages", r.AttrStrings("config_srcs"), []string{want, scope})
			}
		})
	}
}

func TestResolveEdges_GeneratedConfigStagesScalarFiles(t *testing.T) {
	for _, producerKind := range []string{"ts_codegen", "genrule"} {
		for _, ext := range []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", ".json"} {
			for _, owner := range []string{"producer", "compiler", "override"} {
				for _, present := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/owner=%s/present=%t", producerKind, ext, owner, present), func(t *testing.T) {
						c, tc := edgeRepo(t, edgeListings)
						const cfg = "web/vitest.config.mts"
						file := "web/generated/value" + ext
						const stale = "web/generated/stale-secret.ts"
						tc.programs.emission.files["web/generated"] = rule.EmptyFile(filepath.Join(c.RepoRoot, "web/generated/BUILD.bazel"), "web/generated")
						tc.programs.visit("web/generated", nil)
						producer := indexedRule{kind: producerKind, name: "make_config", pkg: "web", outs: []string{"generated/value" + ext, "generated/companion.mjs"}}
						rules := []indexedRule{producer}
						var wantDeps []string
						if producerKind == "ts_codegen" {
							wantDeps = []string{"//web:make_config"}
						}
						if owner != "producer" {
							rules = append(rules,
								indexedRule{kind: "ts_compile", name: "config", pkg: "web", srcs: []string{":make_config"}, deps: []string{"//web/generated:helper"}},
								indexedRule{kind: "ts_compile", name: "helper", pkg: "web/generated", srcs: []string{"helper.mjs"}},
							)
							writeFile(t, filepath.Join(c.RepoRoot, "web/generated/helper.mjs"), "export const value = 1;\n")
							wantDeps = []string{"//web:config"}
						}
						if owner == "override" {
							f, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve typescript "+file+" //web:override_runtime\n"))
							if err != nil {
								t.Fatal(err)
							}
							(&resolve.Configurer{}).Configure(c, "", f)
							wantDeps = []string{"//web:override_runtime"}
						}
						ix := buildIndex(t, c, rules...)
						p := &program{Listing: explainfiles.Listing{Roots: []string{cfg}, Files: []string{cfg}}}
						resolved := ""
						if present {
							resolved = file
							p.Files = append(p.Files, file)
							p.Edges = append(p.Edges, importEdge(cfg, "./generated/value"+ext, file))
							if ext == ".json" {
								writeFile(t, filepath.Join(c.RepoRoot, file), `{"value":-1}`)
							} else {
								writeFile(t, filepath.Join(c.RepoRoot, file), "export { value } from './stale-secret.ts';\n")
								writeFile(t, filepath.Join(c.RepoRoot, stale), "export const value = -1;\n")
								p.Files = append(p.Files, stale)
								p.Edges = append(p.Edges, importEdge(file, "./stale-secret.ts", stale))
							}
						}
						p.candidates = []resolutionCandidate{{from: cfg, specifier: "./generated/value" + ext, path: file, file: true, resolved: resolved}}
						tc.programs.vitestPrograms = map[string]*program{cfg: p}
						r, logged := resolveEdgesOf(t, c, ix, "ts_test", "web/test", "test_test", &ruleImports{config: cfg})
						wantLabels(t, "generated config staging uses the producer package", r.AttrStrings("config_srcs"), []string{"//web:generated/value" + ext, "//web:package.json"})
						wantStrings(t, "generated config retains its declared runtime owner", r.AttrStrings("deps"), wantDeps)
						wantStrings(t, "generated config is no program source", r.AttrStrings("srcs"), nil)
						if logged != "" {
							t.Fatal(logged)
						}
						r, logged = resolveEdgesOf(t, c, ix, "ts_test", "web/test", "test_test", &ruleImports{config: file})
						wantStrings(t, "generated config root retains the same runtime owner", r.AttrStrings("deps"), wantDeps)
						var wantRootScope []string
						if programCandidate(file) {
							wantRootScope = []string{"//web:package.json"}
						}
						wantLabels(t, "generated config root retains its authored scope", r.AttrStrings("config_srcs"), wantRootScope)
						wantStrings(t, "generated config root is no program source", r.AttrStrings("srcs"), nil)
						if logged != "" {
							t.Fatal(logged)
						}
					})
				}
			}
		}
	}
}

func TestResolveEdges_GeneratedConfigSelfImport(t *testing.T) {
	c, tc := edgeRepo(t, edgeListings)
	const cfg = "web/vitest.config.mts"
	const file = "web/generated/value.mjs"
	ix := buildIndex(t, c,
		indexedRule{kind: "genrule", name: "source", pkg: "web", outs: []string{"generated/value.mjs"}},
		indexedRule{kind: "ts_compile", name: "compiled", pkg: "web", srcs: []string{":source"}},
		indexedRule{kind: "ts_test", name: "web_test", pkg: "web", srcs: []string{":source"}},
	)
	tc.programs.emission.rules[emissionLabel(c.RepoName, "web", ":web_test")].AddComment("# keep")
	tc.programs.emission.files["web"].Sync()
	tc.programs.vitestPrograms = configPrograms(map[string][]explainfiles.Edge{
		cfg: {importEdge(cfg, "./generated/value.mjs", file)},
	})
	r, logged := resolveEdgesOf(t, c, ix, "ts_test", "web", "web_test", &ruleImports{config: cfg})
	wantLabels(t, "self-owned config staging", r.AttrStrings("config_srcs"), []string{"generated/value.mjs", "package.json"})
	wantStrings(t, "self import needs no dependency", r.AttrStrings("deps"), nil)
	if logged != "" {
		t.Fatal(logged)
	}
}

// A self-import through an exports subpath lands on the member's own file:
// nothing from the ts_compile, the compile alone from the ts_test, no line.
func TestResolveEdges_MemberSelfImport(t *testing.T) {
	c, tc := edgeRepo(t, edgeListings)
	ix := buildIndex(t, c, edgeRules...)
	s := tc.programs
	set := s.srcs("packages/lib", tc)

	r, logged := resolveEdgesOf(t, c, ix, "ts_compile", "packages/lib", "lib",
		s.compileImports("packages/lib", set))
	if r.Attr("deps") != nil || logged != "" {
		t.Errorf("ts_compile deps = %v, log %q; want none", r.Attr("deps"), logged)
	}
	r, logged = resolveEdgesOf(t, c, ix, "ts_test", "packages/lib", "lib_test",
		s.testImportsForSources(tc.lock, "packages/lib", ":lib", "", set))
	want := []string{":lib"}
	got := r.AttrStrings("deps")
	if !reflect.DeepEqual(got, want) || logged != "" {
		t.Errorf("ts_test deps = %q, log %q; want %q and no line", got, logged, want)
	}
}

// A file under a declared out_dir is that codegen's, whatever the program
// listed; a types entry naming an absent codegen out is a dep on it (D9).
func TestResolveEdges_CodegenOutputs(t *testing.T) {
	const gen = "worker/gen/types.d.ts"
	c, tc := edgeRepo(t, map[string]string{
		"worker": listingOf("worker", "worker/src/index.ts") + gen + "\n" +
			viaLine("../gen/types", "worker/src/index.ts", ""),
		"worker2": listingOf("worker2", "worker2/src/index.ts"),
	})
	writeFile(t, filepath.Join(c.RepoRoot, "worker2/tsconfig.json"),
		`{"compilerOptions": {"types": ["./worker-configuration.d.ts"]}}`)
	ix := buildIndex(t, c,
		indexedRule{kind: "ts_codegen", name: "tree", pkg: "worker", outDir: "gen"},
		indexedRule{kind: "ts_codegen", name: "wt", pkg: "worker2", outs: []string{"worker-configuration.d.ts"}},
		indexedRule{kind: "ts_compile", name: "worker", pkg: "worker",
			srcs: []string{"src/index.ts"}},
		indexedRule{kind: "ts_compile", name: "worker2", pkg: "worker2",
			srcs: []string{"src/index.ts"}},
	)
	s := tc.programs

	f, err := rule.LoadData("BUILD.bazel", "worker",
		[]byte(`ts_codegen(name = "tree", out_dir = "gen")`))
	if err != nil {
		t.Fatal(err)
	}
	configureTsConfig(c, "worker", f, nil)
	set := s.srcs("worker", getConfig(c))
	if len(set.declaration) != 0 {
		t.Errorf("srcs(worker) = %+v: the out_dir's file is no src", set)
	}
	r, logged := resolveEdgesOf(t, c, ix, "ts_compile", "worker", "worker",
		s.compileImports("worker", set))
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, []string{":tree"}) {
		t.Errorf("worker deps = %q, log %q; want [:tree]", got, logged)
	}

	c.Exts[languageName] = tc
	f, err = rule.LoadData("BUILD.bazel", "worker2",
		[]byte(`ts_codegen(name = "wt", outs = ["worker-configuration.d.ts"])`))
	if err != nil {
		t.Fatal(err)
	}
	configureTsConfig(c, "worker2", f, nil)
	r, logged = resolveEdgesOf(t, c, ix, "ts_compile", "worker2", "worker2",
		s.compileImports("worker2", s.srcs("worker2", getConfig(c))))
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, []string{":wt"}) {
		t.Errorf("worker2 deps = %q, log %q; want [:wt]", got, logged)
	}
}

// Core # gazelle:resolve names the label for a file no rule indexes; the
// override is read before ownership, so the report goes too.
func TestResolveEdges_OverrideIsTheEscapeHatch(t *testing.T) {
	c, tc := edgeRepo(t, edgeListings)
	ix := buildIndex(t, c, edgeRules...)
	f, err := rule.LoadData("BUILD.bazel", "", []byte(
		"# gazelle:resolve typescript go/fixtures/x.json //go/fixtures:json\n"))
	if err != nil {
		t.Fatal(err)
	}
	(&resolve.Configurer{}).Configure(c, "", f)
	s := tc.programs

	r, logged := resolveEdgesOf(t, c, ix, "ts_compile", "web", "web",
		s.compileImports("web", s.srcs("web", tc)))
	if deps := r.AttrStrings("deps"); !hasLabel(deps, "//go/fixtures:json") {
		t.Errorf("deps = %q, want //go/fixtures:json among them", deps)
	}
	if strings.Contains(logged, "go/fixtures/x.json") {
		t.Errorf("the overridden file was still reported:\n%s", logged)
	}
	t.Run("eligible override bypasses missing native provider", func(t *testing.T) {
		c, tc := edgeRepo(t, map[string]string{"web": listingOf("web", "web/src/a.ts")})
		s := tc.programs
		const file = "web/generated/value_pb.ts"
		s.visit("web/generated", []string{"value_pb.ts"})
		f, err := rule.LoadData("BUILD.bazel", "", []byte(
			"# gazelle:resolve typescript "+file+" //schema:custom\n"))
		if err != nil {
			t.Fatal(err)
		}
		(&resolve.Configurer{}).Configure(c, "", f)
		lang := &tsLang{}
		ix := resolve.NewRuleIndex(func(*rule.Rule, string) resolve.Resolver { return lang })
		wrapper := rule.NewRule("ts_proto_library", "generated")
		wrapper.SetAttr("proto", "//schema:missing")
		wrapper.SetAttr("out_dir", "generated")
		wrappers := rule.EmptyFile("BUILD.bazel", "web")
		wrappers.Rules = append(wrappers.Rules, wrapper)
		s.recordBuild(c, wrappers.Pkg, wrappers)
		ix.AddRule(c, wrapper, wrappers)
		ix.Finish()
		r, logged := resolveEdgesOf(t, c, ix, "ts_compile", "web", "web", &ruleImports{
			edges: []explainfiles.Edge{importEdge("web/src/a.ts", "../../"+file, file)},
		})
		wantStrings(t, "deps", r.AttrStrings("deps"), []string{"//schema:custom"})
		if logged != "" {
			t.Errorf("eligible override reported unresolved provider: %s", logged)
		}
		const cfg = "web/vitest.config.mts"
		s.vitestPrograms = configPrograms(map[string][]explainfiles.Edge{
			cfg: {importEdge(cfg, "./generated/value_pb", file)},
		})
		for _, scalar := range []bool{false, true} {
			t.Run(fmt.Sprintf("scalar=%t", scalar), func(t *testing.T) {
				wantDeps := []string{"//schema:custom"}
				if scalar {
					ix = buildIndex(t, c,
						indexedRule{kind: "genrule", name: "source", pkg: "web", outs: []string{"generated/value_pb.ts"}},
						indexedRule{kind: "ts_compile", name: "compiled", pkg: "web", srcs: []string{":source"}},
					)
				}
				r, logged := resolveEdgesOf(t, c, ix, "ts_test", "web", "web_test", &ruleImports{config: cfg})
				wantLabels(t, "a config File needs staging even with an override", r.AttrStrings("config_srcs"), []string{"generated/value_pb.ts", "package.json"})
				wantStrings(t, "the override supplies the config runtime closure", r.AttrStrings("deps"), wantDeps)
				if logged != "" {
					t.Errorf("eligible config override reported unresolved provider: %s", logged)
				}
			})
		}
	})
}

// Without a lockfile there is no hub: an npm edge gets no label, said once.
func TestResolveEdges_NoLockfileNoNpmLabel(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"name": "w"}`)
	c := config.New()
	c.RepoRoot = root
	(&resolve.Configurer{}).RegisterFlags(nil, "", c)
	configureTsConfig(c, "", nil, nil)
	tc := getConfig(c)
	s := tc.programs
	s.visit("", []string{"package.json"})
	s.visit("app", []string{"a.ts", "b.ts"})
	s.walked["app"] = true
	s.record(programOf(t, "app", listingOf("app", "app/a.ts", "app/b.ts")+
		storeZod+"\n"+viaLine("zod", "app/a.ts", "")+viaLine("zod", "app/b.ts", "")))
	ix := buildIndex(t, c, indexedRule{kind: "ts_compile", name: "app",
		pkg: "app", srcs: []string{"a.ts", "b.ts"}})

	r, logged := resolveEdgesOf(t, c, ix, "ts_compile", "app", "app",
		s.compileImports("app", s.srcs("app", tc)))
	if r.Attr("deps") != nil {
		t.Errorf("deps = %v, want none", r.Attr("deps"))
	}
	if n := strings.Count(logged, pnpmLockfileName); n != 1 {
		t.Errorf("the missing lockfile was said %d times, want once:\n%s", n, logged)
	}
}

// The exact repository path of every src is what an edge target is looked up
// by; the module-form keys beside it are the specifier ladder's.
func TestImportsForRule_IndexesTheExactPath(t *testing.T) {
	r, f := newRule(indexedRule{kind: "ts_compile", name: "w", pkg: "w",
		srcs: []string{"src/a.ts", "src/types.d.ts", "data.json", "m.d.mts"}})
	got := specStrings(importsForRule(emptyConfig(), r, f))
	for _, want := range []string{"w/src/a.ts", "w/src/types.d.ts",
		"w/data.json", "w/m.d.mts"} {
		if !contains(got, want) {
			t.Errorf("specs %q lack the exact path %q", got, want)
		}
	}
	if n := strings.Count(strings.Join(got, "\n")+"\n", "w/data.json\n"); n != 1 {
		t.Errorf("w/data.json indexed %d times, want once", n)
	}
}

// ---- the Workers pool -------------------------------------------------------

// worker/ exports a pool config naming ./wrangler.jsonc and declares the pool;
// worker/test is a package of its own; lock says where istanbul is declared.
func poolRepo(t *testing.T, lock string) (*config.Config, *tsConfig) {
	t.Helper()
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		pnpmLockfileName:             lock,
		"node_modules/.modules.yaml": "layoutVersion: 5\n",
		"package.json":               rootManifest,
		"worker/package.json": `{"name":"worker","devDependencies":` +
			`{"@cloudflare/vitest-pool-workers":"0.18.4","vitest":"4.1.11"}}` +
			"\n",
		"worker/vitest.config.mts": poolConfig("./wrangler.jsonc"),
		"worker/wrangler.jsonc":    `{"main":"src/index.ts"}` + "\n",
	})
	c := config.New()
	c.RepoRoot = root
	(&resolve.Configurer{}).RegisterFlags(nil, "", c)
	configureTsConfig(c, "", nil, nil)
	tc := getConfig(c)
	for _, dir := range []string{"", "worker", "worker/src", "worker/test"} {
		tc.programs.walked[dir] = true
	}
	tc.programs.visit("", []string{"package.json"})
	tc.programs.visit("worker", []string{"package.json", "vitest.config.mts", "wrangler.jsonc"})
	tc.programs.visit("worker/src", []string{"index.ts"})
	tc.programs.visit("worker/test", []string{"a.test.ts"})
	tc.programs.record(programOf(t, "worker",
		listingOf("worker", "worker/src/index.ts")))
	tc.programs.record(programOf(t, "worker/test", "worker/src/index.ts\n"+
		viaLine("../src/index", "worker/test/a.test.ts", "")+
		"worker/test/a.test.ts\n"+includeLine("worker/test")))
	return c, tc
}

const poolRepoLock = `lockfileVersion: '9.0'

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
      vitest:
        specifier: 4.1.11
        version: 4.1.11

packages:

  '@cloudflare/vitest-pool-workers@0.18.4':
    resolution: {integrity: sha512-aaa}

  '@vitest/coverage-istanbul@4.1.11':
    resolution: {integrity: sha512-bbb}

  vitest@4.1.11:
    resolution: {integrity: sha512-ccc}

snapshots:

  '@cloudflare/vitest-pool-workers@0.18.4': {}

  '@vitest/coverage-istanbul@4.1.11': {}

  vitest@4.1.11: {}
`

const poolRepoLockNoIstanbul = `lockfileVersion: '9.0'

importers:

  .: {}

  worker:
    devDependencies:
      '@cloudflare/vitest-pool-workers':
        specifier: 0.18.4
        version: 0.18.4
      vitest:
        specifier: 4.1.11
        version: 4.1.11

packages:

  '@cloudflare/vitest-pool-workers@0.18.4':
    resolution: {integrity: sha512-aaa}

  vitest@4.1.11:
    resolution: {integrity: sha512-ccc}

snapshots:

  '@cloudflare/vitest-pool-workers@0.18.4': {}

  vitest@4.1.11: {}
`

const poolCfg = "worker/vitest.config.mts"

var poolEdge = importEdge(poolCfg, "@cloudflare/vitest-pool-workers",
	store+"@cloudflare/vitest-pool-workers/0.18.4/iii/node_modules/"+
		"@cloudflare/vitest-pool-workers/dist/index.d.ts")

var poolRules = []indexedRule{
	{kind: "ts_compile", name: "worker", pkg: "worker",
		srcs: []string{"src/index.ts"}},
	{kind: "ts_test", name: "test_test", pkg: "worker/test",
		srcs: []string{"a.test.ts"}},
}

// The pooled test's rule, resolved with the given config edges.
func resolvePooledTest(t *testing.T, c *config.Config, tc *tsConfig,
	edges []explainfiles.Edge) (*rule.Rule, string) {
	t.Helper()
	ix := buildIndex(t, c, poolRules...)
	s := tc.programs
	s.vitestPrograms = configPrograms(map[string][]explainfiles.Edge{poolCfg: edges})
	imps := s.testImportsForSources(tc.lock, "worker/test", "", poolCfg,
		s.srcs("worker/test", tc))
	return resolveEdgesOf(t, c, ix, "ts_test", "worker/test", "test_test", imps)
}

// The config's edge names the pool: the test names the filegroup over the
// wrangler config, runs istanbul coverage, and carries istanbul in D7 spelling.
func TestResolveEdges_WorkersPoolWritesTheTestsAttributes(t *testing.T) {
	c, tc := poolRepo(t, poolRepoLock)
	r, logged := resolvePooledTest(t, c, tc, []explainfiles.Edge{poolEdge})
	if got := r.AttrString("wrangler_config"); got != "//worker:wrangler_config" {
		t.Errorf("wrangler_config = %q, want //worker:wrangler_config", got)
	}
	if got := r.AttrString("coverage_provider"); got != "istanbul" {
		t.Errorf("coverage_provider = %q, want istanbul", got)
	}
	want := []string{"//worker", "@npm//:vitest_coverage-istanbul",
		"@npm//worker:cloudflare_vitest-pool-workers", "@npm//worker:vitest"}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %q, want %q", got, want)
	}
	if logged != "" {
		t.Errorf("log, want nothing:\n%s", logged)
	}
}

// No pool edge: none of it, whatever the config names in a literal.
func TestResolveEdges_NoPoolEdgeWritesNoPoolAttributes(t *testing.T) {
	c, tc := poolRepo(t, poolRepoLock)
	r, _ := resolvePooledTest(t, c, tc, nil)
	for _, attr := range []string{"wrangler_config", "coverage_provider"} {
		if r.Attr(attr) != nil {
			t.Errorf("%s = %q, want unset: the config runs no pool", attr,
				r.AttrString(attr))
		}
	}
	want := []string{"//worker", "@npm//worker:cloudflare_vitest-pool-workers",
		"@npm//worker:vitest"}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %q, want %q", got, want)
	}
}

func TestResolveEdges_UnlinkedCoverageDoesNotBreakWorkersTestAnalysis(t *testing.T) {
	for name, lock := range map[string]string{
		"absent":           poolRepoLockNoIstanbul,
		"transitive only":  strings.Replace(poolRepoLock, "  .:\n    devDependencies:\n      '@vitest/coverage-istanbul':\n        specifier: 4.1.11\n        version: 4.1.11", "  .: {}", 1),
		"sibling importer": strings.Replace(poolRepoLock, "  .:\n", "  sibling:\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			c, tc := poolRepo(t, lock)
			r, logged := resolvePooledTest(t, c, tc, []explainfiles.Edge{poolEdge})
			if got := r.AttrString("wrangler_config"); got != "//worker:wrangler_config" {
				t.Errorf("wrangler_config = %q, want //worker:wrangler_config", got)
			}
			if r.Attr("coverage_provider") != nil {
				t.Errorf("coverage_provider = %q, want unset: istanbul is not linked",
					r.AttrString("coverage_provider"))
			}
			want := []string{"//worker", "@npm//worker:cloudflare_vitest-pool-workers",
				"@npm//worker:vitest"}
			if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
				t.Errorf("deps = %q, want %q", got, want)
			}
			if !strings.Contains(logged, "@vitest/coverage-istanbul") ||
				strings.Count(logged, "\n") != 1 {
				t.Errorf("log, want one line naming @vitest/coverage-istanbul:\n%s", logged)
			}
		})
	}
}

// The config beside the tests: the wrangler config by its name in the test's
// own package, as `config` is.
func TestResolveEdges_SamePackagePoolNamesTheFile(t *testing.T) {
	c, tc := poolRepo(t, poolRepoLock)
	ix := buildIndex(t, c, poolRules...)
	s := tc.programs
	s.vitestPrograms = configPrograms(map[string][]explainfiles.Edge{poolCfg: {poolEdge}})
	imps := s.testImportsForSources(tc.lock, "worker", ":worker", poolCfg,
		s.srcs("worker", tc))
	r, _ := resolveEdgesOf(t, c, ix, "ts_test", "worker", "worker_test", imps)
	if got := r.AttrString("wrangler_config"); got != "wrangler.jsonc" {
		t.Errorf("wrangler_config = %q, want wrangler.jsonc", got)
	}
}

// scripts' program: a store file's `/// <reference types>` answered by the
// chain (the root's @types/node) and one answered beside the file's own tree.
var scriptsListing = storeZod + "\n" +
	viaLine("zod", "scripts/run.ts", "zod/index.d.ts@3.24.2") +
	storeTypesReact + "\n" +
	"   Type library referenced via 'react' from file '" + storeZod + "'\n" +
	storeMarked + "\n" +
	viaLine("marked", "scripts/run.test.ts", "marked/lib/marked.d.ts@15.0.12") +
	storeTypesNode + "\n" +
	"   Type library referenced via 'node' from file '" + storeMarked + "'\n" +
	"scripts/run.ts\n" + includeLine("scripts") +
	viaLine("./run", "scripts/run.test.ts", "") +
	"scripts/run.test.ts\n" + includeLine("scripts")

// web/tools' program: the same reference under an importer that declares the
// @types package.
var toolsListing = storeMarked + "\n" +
	viaLine("marked", "web/tools/gen.ts", "marked/lib/marked.d.ts@15.0.12") +
	storeTypesReact + "\n" +
	"   Type library referenced via 'react' from file '" + storeMarked + "'\n" +
	"web/tools/gen.ts\n" + includeLine("web/tools")

// A store file's `/// <reference types>` the chain answers is the edge of the
// rule whose files reach the file, spelled as the declaring importer's.
func TestResolveEdges_StoreFileTypeReferenceIsTheChains(t *testing.T) {
	listings := maps.Clone(edgeListings)
	listings["scripts"] = scriptsListing
	listings["web/tools"] = toolsListing
	c, tc := edgeRepo(t, listings)
	rules := append(slices.Clone(edgeRules),
		indexedRule{kind: "ts_compile", name: "scripts", pkg: "scripts",
			srcs: []string{"run.ts"}},
		indexedRule{kind: "ts_test", name: "scripts_test", pkg: "scripts",
			srcs: []string{"run.test.ts"}},
		indexedRule{kind: "ts_compile", name: "tools", pkg: "web/tools",
			srcs: []string{"gen.ts"}})
	ix := buildIndex(t, c, rules...)
	s := tc.programs

	set := s.srcs("scripts", tc)
	r, logged := resolveEdgesOf(t, c, ix, "ts_compile", "scripts", "scripts",
		s.compileImports("scripts", set))
	want := []string{"@npm//:zod"}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
		t.Errorf("scripts deps = %q, want %q: the test file alone reaches "+
			"marked, and no importer on the chain declares @types/react",
			got, want)
	}
	if logged != "" {
		t.Errorf("scripts logged:\n%s", logged)
	}
	r, logged = resolveEdgesOf(t, c, ix, "ts_test", "scripts", "scripts_test",
		s.testImportsForSources(tc.lock, "scripts", ":scripts", "", set))
	want = []string{":scripts", "@npm//:marked", "@npm//:types_node",
		"@npm//:zod"}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
		t.Errorf("scripts_test deps = %q, want %q", got, want)
	}
	if logged != "" {
		t.Errorf("scripts_test logged:\n%s", logged)
	}

	r, logged = resolveEdgesOf(t, c, ix, "ts_compile", "web/tools", "tools",
		s.compileImports("web/tools", s.srcs("web/tools", tc)))
	want = []string{"@npm//web:marked", "@npm//web:types_react"}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, want) {
		t.Errorf("tools deps = %q, want %q", got, want)
	}
	if logged != "" {
		t.Errorf("tools logged:\n%s", logged)
	}
	t.Run("generated selection retains npm runtime without fallback type closure", func(t *testing.T) {
		tc.lock.importers[""].deps["marked"] = "15.0.12"
		rules = append(rules, indexedRule{kind: "ts_codegen", name: "marked", pkg: "generated", outs: []string{"marked.d.ts"}})
		ix := buildIndex(t, c, rules...)
		s.programs["scripts"].candidates = []resolutionCandidate{{from: "scripts/run.test.ts", specifier: "marked", path: "generated/marked.d.ts", file: true, resolved: storeMarked}}
		r, logged := resolveEdgesOf(t, c, ix, "ts_test", "scripts", "scripts_test", s.testImportsForSources(tc.lock, "scripts", ":scripts", "", set))
		wantStrings(t, "runtime dep without fallback type closure", r.AttrStrings("deps"), []string{"//generated:marked", ":scripts", "@npm//:marked", "@npm//:zod"})
		if logged != "" {
			t.Fatalf("generated selection reported fallback: %s", logged)
		}
	})
}

func TestResolveEdges_DeclarationRetainsDeclaredRuntime(t *testing.T) {
	for _, test := range []struct {
		pkg, spec, want string
		memberView      bool
	}{
		{"scripts", "zod", "@npm//:zod", false},
		{"web", "marked", "@npm//web:marked", false},
		{"web", "tailwindcss-v3", "@npm//web:tailwindcss-v3", false},
		{"web", "@acme/ui", ":node_modules/@acme/ui", true},
		{"web", "@acme/lib/wire", "@npm//web:acme_lib", true},
		{"packages/lib", "@acme/lib/wire", "", false},
		{"scripts", "fsevents", "", false},
		{"scripts", "local-alias", "", false},
	} {
		t.Run(test.pkg+"/"+test.spec, func(t *testing.T) {
			for _, input := range []string{"generated absent", "generated present", "authored"} {
				t.Run(input, func(t *testing.T) {
					c, tc := edgeRepo(t, edgeListings)
					const declaration = "generated/value.d.ts"
					file := path.Join(test.pkg, "index.ts")
					resolved := storeZod
					if input != "generated absent" {
						resolved = declaration
					}
					edge := importEdge(file, test.spec, resolved)
					candidate := resolutionCandidate{from: file, specifier: test.spec, path: declaration, file: true, resolved: resolved}
					edges := []explainfiles.Edge{edge}
					var rules []indexedRule
					var wantDeps, wantSrcs, wantTypeInputs []string
					if input == "authored" {
						tc.programs.emission.files["generated"] = loadedEmptyBuild(t, filepath.Join(c.RepoRoot, "generated/BUILD.bazel"), "generated")
						writeFile(t, filepath.Join(c.RepoRoot, declaration), "export declare const value: number;\n")
						writeFile(t, filepath.Join(c.RepoRoot, file), "import { value } from \""+test.spec+"\"; console.log(value);\n")
						tc.programs.visit("generated", []string{"value.d.ts"})
						if !test.memberView {
							wantSrcs = []string{"//generated:value.d.ts"}
							wantTypeInputs = []string{"//:package.json"}
						}
					} else {
						rules = []indexedRule{{kind: "ts_codegen", name: "types", pkg: "generated", outs: []string{"value.d.ts"}}}
						wantDeps = []string{"//generated:types"}
						edges = append(edges, explainfiles.Edge{From: resolved, To: storeTypesNode, Kind: explainfiles.TypeReference, Specifier: "node"})
					}
					tc.programs.programs[test.pkg] = &program{
						candidates: []resolutionCandidate{candidate},
						Listing:    explainfiles.Listing{Edges: edges},
					}
					ix := buildIndex(t, c, rules...)
					r, logged := resolveEdgesOf(t, c, ix, "ts_compile", test.pkg, "consumer", &ruleImports{edges: []explainfiles.Edge{edge}, candidates: []resolutionCandidate{candidate}})
					if test.want != "" {
						wantDeps = append(wantDeps, test.want)
					}
					wantStrings(t, "runtime deps without obsolete declaration closure", r.AttrStrings("deps"), wantDeps)
					wantStrings(t, "unowned authored declaration remains a source", r.AttrStrings("srcs"), wantSrcs)
					wantLabels(t, "unowned authored declaration retains compiler scope", r.AttrStrings("type_inputs"), wantTypeInputs)
					if logged != "" {
						t.Fatal(logged)
					}
				})
			}
		})
	}
}

func TestResolveEdges_DeclarationImportDoesNotInferRuntimeOwners(t *testing.T) {
	for _, traced := range []bool{false, true} {
		for _, owner := range []string{"compiler", "producer", "absent", "excluded"} {
			t.Run(fmt.Sprintf("traced=%t/%s", traced, owner), func(t *testing.T) {
				c, tc := edgeRepo(t, edgeListings)
				const declaration = "foreign/companion.d.mts"
				const companion = "foreign/companion.mjs"
				tc.programs.visit("foreign", nil)
				if owner != "absent" {
					writeFile(t, filepath.Join(c.RepoRoot, companion), "")
					if owner != "excluded" {
						tc.programs.files["foreign"] = append(tc.programs.files["foreign"], "companion.mjs")
					}
				}
				edge := importEdge("web/index.ts", "../foreign/companion.mjs", declaration)
				imps := &ruleImports{edges: []explainfiles.Edge{edge}}
				p := tc.programs.programs["web"]
				p.Edges = []explainfiles.Edge{edge}
				if traced {
					p.candidates = []resolutionCandidate{{from: edge.From, specifier: edge.Specifier, path: declaration, file: true, resolved: declaration}}
					imps.candidates = p.candidates
				}
				rules := []indexedRule{{kind: "ts_codegen", name: "types", pkg: "foreign", outs: []string{"companion.d.mts"}}}
				wantDeps := []string{"//foreign:types"}
				switch owner {
				case "compiler":
					rules = append(rules, indexedRule{kind: "ts_compile", name: "runtime", pkg: "foreign", srcs: []string{"companion.mjs"}})
				case "producer":
					rules = append(rules, indexedRule{kind: "ts_codegen", name: "runtime", pkg: "foreign", outs: []string{"companion.mjs"}})
				}
				r, logged := resolveEdgesOf(t, c, buildIndex(t, c, rules...), "ts_compile", "web", "web", imps)
				wantStrings(t, "owned, absent or excluded companions are no sources", r.AttrStrings("srcs"), nil)
				wantStrings(t, "declaration owner only", r.AttrStrings("deps"), wantDeps)
				if logged != "" {
					t.Fatal(logged)
				}
			})
		}
	}
}

func TestResolveEdges_KnownTypeEdgesDoNotRequireExcludedCompanions(t *testing.T) {
	for _, edge := range []explainfiles.Edge{
		{From: "web/index.ts", To: "foreign/companion.d.mts", Kind: explainfiles.TypeReference, Specifier: "../foreign/companion.d.mts"},
		{From: "web/index.ts", To: "foreign/companion.d.mts", Kind: explainfiles.Reference, Specifier: "../foreign/companion.d.mts"},
		{From: "web/index.ts", To: "foreign/companion.d.mts", Kind: explainfiles.Augmentation, Specifier: "../foreign/companion.mjs"},
		importEdge("web/index.d.ts", "../foreign/companion.mjs", "foreign/companion.d.mts"),
	} {
		t.Run(fmt.Sprintf("%s/%d", edge.From, edge.Kind), func(t *testing.T) {
			c, tc := edgeRepo(t, edgeListings)
			writeFile(t, filepath.Join(c.RepoRoot, "foreign/companion.mjs"), "export const value = 1;\n")
			tc.programs.visit("foreign", nil)
			ix := buildIndex(t, c, indexedRule{kind: "ts_codegen", name: "types", pkg: "foreign", outs: []string{"companion.d.mts"}})
			imps := &ruleImports{edges: []explainfiles.Edge{edge}}
			if edge.Kind.ModuleSpecifier() {
				imps.candidates = []resolutionCandidate{{from: edge.From, specifier: edge.Specifier, path: edge.To, resolved: edge.To, file: true}}
			}
			r, logged := resolveEdgesOf(t, c, ix, "ts_compile", "web", "web", imps)
			wantStrings(t, "type owner", r.AttrStrings("deps"), []string{"//foreign:types"})
			wantStrings(t, "excluded companion stays out", r.AttrStrings("srcs"), nil)
			if logged != "" {
				t.Fatal(logged)
			}
		})
	}
}

func TestResolveEdges_KnownTypeEdgesDoNotTraverseEligibleCompanions(t *testing.T) {
	for _, edge := range []explainfiles.Edge{
		{From: "web/index.ts", To: "foreign/companion.d.mts", Kind: explainfiles.Reference},
		{From: "web/index.ts", To: "foreign/companion.d.mts", Kind: explainfiles.TypeReference},
		{From: "web/index.ts", To: "foreign/companion.d.mts", Kind: explainfiles.Augmentation, Specifier: "../foreign/companion.mjs"},
		importEdge("web/index.d.ts", "../foreign/companion.mjs", "foreign/companion.d.mts"),
	} {
		for _, owned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%d/owned=%t", edge.From, edge.Kind, owned), func(t *testing.T) {
				c, tc := edgeRepo(t, edgeListings)
				writeFile(t, filepath.Join(c.RepoRoot, "foreign/companion.mjs"), "export { value } from './private.mjs';\n")
				writeFile(t, filepath.Join(c.RepoRoot, "foreign/private.mjs"), "export const value = 1;\n")
				tc.programs.visit("foreign", []string{"companion.mjs"})
				tc.programs.programs["web"].Edges = []explainfiles.Edge{
					edge, importEdge("foreign/companion.mjs", "./private.mjs", "foreign/private.mjs"),
				}
				rules := []indexedRule{{kind: "ts_codegen", name: "types", pkg: "foreign", outs: []string{"companion.d.mts"}}}
				if owned {
					rules = append(rules, indexedRule{kind: "ts_compile", name: "runtime", pkg: "foreign", srcs: []string{"companion.mjs"}})
				}
				imps := &ruleImports{edges: []explainfiles.Edge{edge}}
				if edge.Kind.ModuleSpecifier() {
					imps.candidates = []resolutionCandidate{{from: edge.From, specifier: edge.Specifier, path: edge.To, resolved: edge.To, file: true}}
				}
				r, logged := resolveEdgesOf(t, c, buildIndex(t, c, rules...), "ts_compile", "web", "web", imps)
				wantStrings(t, "only the declaration owner", r.AttrStrings("deps"), []string{"//foreign:types"})
				wantStrings(t, "no runtime sources", r.AttrStrings("srcs"), nil)
				if logged != "" {
					t.Fatal(logged)
				}
			})
		}
	}
}

func TestResolveEdges_ForeignJSONKeepsItsPackageAndOwner(t *testing.T) {
	for _, test := range []struct{ name, producer, owner string }{
		{"unowned source", "", ""},
		{"existing TsInfo owner", "", "compiler"},
		{"metadata-only owner cannot supply a JSON module", "", "scope"},
		{"generated File", "genrule", ""},
		{"generated compiler owner", "genrule", "compiler"},
		{"generated override precedes compiler owner", "genrule", "override"},
		{"generated self owner keeps producer label", "genrule", "self"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, tc := edgeRepo(t, edgeListings)
			file := "foreign/fixtures/value.json"
			if test.owner == "scope" {
				file = "foreign/package.json"
			}
			tc.programs.emission.files["web"] = loadedEmptyBuild(t, filepath.Join(c.RepoRoot, "web/BUILD.bazel"), "web")
			tc.programs.emission.files["foreign"] = loadedEmptyBuild(t, filepath.Join(c.RepoRoot, "foreign/BUILD.bazel"), "foreign")
			var rules []indexedRule
			src := strings.TrimPrefix(file, "foreign/")
			if test.producer == "" {
				writeFile(t, filepath.Join(c.RepoRoot, file), `{ "answer": 42 }`)
				tc.programs.visit(path.Dir(file), []string{path.Base(file)})
			} else {
				rules = append(rules, indexedRule{kind: test.producer, name: "values", pkg: "foreign", outs: []string{src}})
				src = ":values"
			}
			if test.owner == "self" {
				rules = append(rules, indexedRule{kind: "ts_test", name: "web_test", pkg: "web", srcs: []string{"//foreign:values"}})
			} else if test.owner != "" {
				rules = append(rules, indexedRule{kind: "ts_compile", name: "fixtures", pkg: "foreign", srcs: []string{src}})
			}
			if test.owner == "override" {
				f, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve typescript "+file+" //foreign:override\n"))
				if err != nil {
					t.Fatal(err)
				}
				(&resolve.Configurer{}).Configure(c, "", f)
			}
			ix := buildIndex(t, c, rules...)
			if test.owner == "scope" {
				owner := tc.programs.emission.rules[emissionLabel(c.RepoName, "foreign", ":fixtures")]
				owner.SetAttr("srcs", []string{})
				owner.SetAttr("package_scopes", []string{src})
			}
			r := rule.NewRule("ts_test", "web_test")
			r.SetAttr("srcs", []string{"test.ts"})
			if test.owner == "self" {
				tc.programs.emission.rules[emissionLabel(c.RepoName, "web", ":web_test")].SetAttr("srcs", []string{"test.ts", "//foreign:values"})
				tc.programs.emission.rules[emissionLabel(c.RepoName, "web", ":web_test")].AddComment("# keep")
				tc.programs.emission.files["web"].Sync()
				r.SetAttr("srcs", []string{"test.ts", "//foreign:values"})
			}
			edge := importEdge("web/test.ts", "../"+file, file)
			imps := &ruleImports{edges: []explainfiles.Edge{edge, edge}}
			logged := captureLog(t, func() { resolveEdges(c, ix, r, imps, label.New("", "web", "web_test")) })
			wantSrcs := []string{"//foreign:" + strings.TrimPrefix(file, "foreign/"), "test.ts"}
			var wantDeps []string
			if test.owner != "" && test.owner != "scope" {
				wantSrcs = []string{"test.ts"}
				wantDeps = []string{"//foreign:fixtures"}
				if test.owner == "override" {
					wantDeps = []string{"//foreign:override"}
				} else if test.owner == "self" {
					wantSrcs = []string{"//foreign:values", "test.ts"}
					wantDeps = nil
				}
			}
			wantStrings(t, "srcs", r.AttrStrings("srcs"), wantSrcs)
			wantLabels(t, "scope metadata", r.AttrStrings("package_scopes"), []string{"package.json"})
			wantStrings(t, "deps", r.AttrStrings("deps"), wantDeps)
			if logged != "" {
				t.Fatalf("resolved JSON reported missing: %s", logged)
			}
		})
	}
}

func TestResolveEdges_ForeignJSONColonDoesNotPoisonLabels(t *testing.T) {
	c, tc := edgeRepo(t, edgeListings)
	tc.programs.visit("foreign", []string{"value:invalid.json"})
	const file = "foreign/value:invalid.json"
	tc.programs.emission.files["foreign"] = rule.EmptyFile(filepath.Join(c.RepoRoot, "foreign/BUILD.bazel"), "foreign")
	writeFile(t, filepath.Join(c.RepoRoot, file), `{ "answer": 42 }`)
	imps := &ruleImports{edges: []explainfiles.Edge{importEdge("web/test.ts", "../foreign/value:invalid.json", file)}}
	r, logged := resolveEdgesOf(t, c, buildIndex(t, c), "ts_test", "web", "web_test", imps)
	if len(r.AttrStrings("srcs")) != 0 || len(r.AttrStrings("deps")) != 0 {
		t.Fatalf("unlabelable JSON became an input: %s", r.AttrStrings("srcs"))
	}
	if !strings.Contains(logged, "no Bazel label can name it") {
		t.Fatalf("unlabelable input has no diagnostic: %s", logged)
	}
}

func TestResolveEdges_SourceDependencyKeepsNpmImporterOwnership(t *testing.T) {
	for _, consumer := range []string{"borrowed", "own supplied identity", "own undeclared", "own other identity", "emitted", "emitted declaration retains private npm ownership", "unknown declaration retains private npm ownership", "config retains supplied npm ownership"} {
		t.Run(consumer, func(t *testing.T) {
			c, tc := edgeRepo(t, nil)
			consumerDir := "consumer"
			if consumer == "own supplied identity" {
				consumerDir = "lib/consumer"
			}
			tc.lock = &npmLock{
				names: map[string]bool{"nested-value": true},
				importers: map[string]*pnpmImporter{
					"":          {deps: map[string]string{}},
					"lib":       {deps: map[string]string{"nested-value": "1.0.0"}},
					consumerDir: {deps: map[string]string{}},
				},
			}
			if consumer == "own other identity" {
				tc.lock.importers[consumerDir].deps["nested-value"] = "2.0.0"
			}
			const npmFile = "lib/node_modules/nested-value/index.d.ts"
			libraryFile := "index.ts"
			if strings.Contains(consumer, "declaration") {
				libraryFile = "index.d.ts"
			}
			entry := consumerDir + "/index.test.ts"
			for _, file := range []string{"lib/" + libraryFile, entry} {
				writeFile(t, filepath.Join(c.RepoRoot, file), "")
				tc.programs.visit(parentDir(file), []string{path.Base(file)})
				tc.programs.walked[parentDir(file)] = true
			}
			tc.programs.record(programOf(t, "lib", listingOf("lib", "lib/"+libraryFile)))
			specifier := "../lib/index.js"
			if consumer == "own supplied identity" {
				specifier = "../index.js"
			}
			rootEdge := importEdge(entry, specifier, "lib/"+libraryFile)
			p := &program{
				Listing: explainfiles.Listing{
					Roots: []string{entry},
					Edges: []explainfiles.Edge{rootEdge, importEdge("lib/"+libraryFile, "nested-value", npmFile)},
				},
			}
			want := []string{"//lib"}
			if strings.HasPrefix(consumer, "own ") {
				resolved := npmFile
				identity := "@npm//:nested-value"
				if consumer == "own supplied identity" {
					identity = "@npm//lib:nested-value"
				} else if consumer == "own other identity" {
					identity = "@npm//consumer:nested-value"
					resolved = "consumer/node_modules/nested-value/index.d.ts"
				}
				p.Edges = append(p.Edges, importEdge(entry, "nested-value", resolved))
				want = append(want, identity)
			}
			ix := buildIndex(t, c, indexedRule{
				kind: "ts_compile", name: "lib", pkg: "lib", srcs: []string{libraryFile},
				deps: []string{"@npm//lib:nested-value"},
			})
			sourceOwner := func(owner string) emissionMode {
				if strings.HasPrefix(consumer, "unknown declaration") {
					return unknownEmission
				}
				lbl, err := label.Parse(owner)
				if err == nil && lbl.Pkg == "lib" && lbl.Name == "lib" && !strings.HasPrefix(consumer, "emitted") {
					return sourceEmission
				}
				return emittedEmission
			}
			r := rule.NewRule("ts_test", "consumer_test")
			r.SetAttr("srcs", []string{"index.test.ts"})
			from := label.New("", consumerDir, r.Name())
			logged := captureLog(t, func() {
				imps := &ruleImports{program: p, edges: sourceEdges(p.edgesBySource(), p.Roots)}
				if consumer == "config retains supplied npm ownership" {
					imps.config = "lib/" + libraryFile
					tc.programs.vitestPrograms = configPrograms(map[string][]explainfiles.Edge{
						imps.config: {importEdge(imps.config, "nested-value", npmFile)},
					})
					want = append(want, "@npm//lib:nested-value")
				}
				deps := resolveEdges(c, ix, r, imps, from, sourceOwner)
				removeSuppliedProgramInputs(c, r, from, []string{"index.test.ts"}, sourceOwner, map[string]bool{}, deps)
			})
			if logged != "" {
				t.Fatalf("npm ownership reported missing: %s", logged)
			}
			wantLabels(t, "consumer retains only its required npm identities", r.AttrStrings("deps"), want)
			wantStrings(t, "library sources remain owned", r.AttrStrings("srcs"), []string{"index.test.ts"})
			wantLabels(t, "consumer scope remains metadata", r.AttrStrings("package_scopes"), []string{"//:package.json"})
		})
	}
}

type memberLinkFixture struct {
	c           *config.Config
	r           *rule.Rule
	from        label.Label
	roots       []string
	deps        map[string]dependencyRequirement
	sourceOwner func(string) emissionMode
	want        []string
}

func newMemberLinkFixture(t *testing.T, use string) memberLinkFixture {
	c, tc := edgeRepo(t, nil)
	tc.lock.members["shared"] = "packages/shared"
	dep := "//lib:node_modules/shared"
	if strings.HasSuffix(use, "alias") {
		dep = "//lib:member_alias"
	} else if use == "other importer" {
		dep = "//consumer:node_modules/shared"
	}
	roots, librarySources := []string{"index.ts"}, []string{"index.ts"}
	if use == "declaration root" {
		roots = append(roots, "shape.d.ts")
		librarySources = append(librarySources, "//consumer:shape.d.ts")
	}
	buildIndex(t, c,
		indexedRule{kind: "ts_compile", name: "lib", pkg: "lib", srcs: librarySources, deps: []string{"//lib:node_modules/shared"}},
		indexedRule{kind: "ts_compile", name: "shared", pkg: "packages/shared", srcs: []string{"index.ts"}},
	)
	for _, pkg := range []string{"lib", "consumer"} {
		link := rule.NewRule("node_modules_member", "node_modules/shared")
		link.SetAttr("member", "@npm//:shared")
		link.SetAttr("visibility", []string{":__pkg__"})
		file := rule.EmptyFile("BUILD.bazel", pkg)
		file.Rules = append(file.Rules, link)
		alias := rule.NewRule("alias", "member_alias")
		alias.SetAttr("actual", ":node_modules/shared")
		file.Rules = append(file.Rules, alias)
		tc.programs.recordBuild(c, pkg, file)
	}
	r := rule.NewRule("ts_compile", "consumer")
	r.SetAttr("srcs", roots)
	r.SetAttr("deps", []string{"//lib", dep})
	deps := map[string]dependencyRequirement{
		"//lib": {edges: []explainfiles.Edge{importEdge("consumer/index.ts", "../lib/index.js", "lib/index.ts")}},
		dep:     {edges: []explainfiles.Edge{importEdge("lib/index.ts", "shared", "packages/shared/index.ts")}},
	}
	want := []string{"//lib"}
	if strings.HasPrefix(use, "own") {
		required := deps[dep]
		required.edges = append(required.edges, importEdge("consumer/index.ts", "shared", "packages/shared/index.ts"))
		deps[dep] = required
	}
	if use == "declaration root" {
		deps[dep] = dependencyRequirement{edges: []explainfiles.Edge{importEdge("consumer/shape.d.ts", "shared", "packages/shared/index.ts")}}
	}
	if strings.HasPrefix(use, "own") || use == "other importer" || use == "declaration root" {
		want = append(want, dep)
	}
	sourceOwner := func(key string) emissionMode {
		owner, err := label.Parse(key)
		if err == nil && (owner.Pkg == "lib" && owner.Name == "lib" || owner.Pkg == "packages/shared" && owner.Name == "shared") {
			return sourceEmission
		}
		return emittedEmission
	}
	return memberLinkFixture{c: c, r: r, from: label.New("", "consumer", "consumer"), roots: roots, deps: deps, sourceOwner: sourceOwner, want: want}
}

var memberLinkUses = []string{"borrowed", "borrowed alias", "own", "own alias", "other importer", "declaration root"}

func TestRemoveSuppliedProgramInputsKeepsMemberLinkOwnership(t *testing.T) {
	for _, use := range memberLinkUses {
		t.Run(use, func(t *testing.T) {
			f := newMemberLinkFixture(t, use)
			removeSuppliedProgramInputs(f.c, f.r, f.from, f.roots, f.sourceOwner, map[string]bool{}, f.deps)
			wantLabels(t, "only consumer-required member identities remain", f.r.AttrStrings("deps"), f.want)
			wantStrings(t, "consumer keeps its own inputs", f.r.AttrStrings("srcs"), f.roots)
		})
	}
}

// The fast minimization is only correct while its supply matches suppliedProgramInputs exactly.
func TestFixedSupplyMatchesExactSupplyAndMinimization(t *testing.T) {
	for _, use := range memberLinkUses {
		t.Run(use, func(t *testing.T) {
			exact := newMemberLinkFixture(t, use)
			removeSuppliedProgramInputs(exact.c, exact.r, exact.from, exact.roots, exact.sourceOwner, map[string]bool{}, exact.deps)

			// The fixture's sourceOwner has no side effects, so every owner counts as settled.
			fast := newMemberLinkFixture(t, use)
			memo := newDependencyMemo(getConfig(fast.c).programs)
			retained, called := map[string]bool{}, map[string]bool{}
			for dep := range fast.deps {
				retained[dep] = true
				key, _ := memo.programTarget(fast.c, fast.from.Pkg, dep)
				called[key] = true
			}
			supply := newFixedSupply(fast.c, fast.from, fast.deps, retained, map[string]bool{}, fast.sourceOwner, nil, memo)
			supplied, suppliedDeps := suppliedProgramInputs(fast.c, fast.from, slices.Sorted(maps.Keys(retained)), fast.sourceOwner, nil, newDependencyMemo(getConfig(fast.c).programs))
			for file := range supply.readFiles {
				role, exact := supplied[resolve.ImportSpec{Lang: languageName, Imp: file}]
				if got, want := supply.supplied(file), exact && role == sourceRole; got != want {
					t.Errorf("fixed supply of %s = %v, exact = %v", file, got, want)
				}
			}
			for target := range supply.readTargets {
				if got, want := supply.targets[target] > 0, suppliedDeps[target]; got != want {
					t.Errorf("fixed supply of target %s = %v, exact = %v", target, got, want)
				}
			}

			removeSuppliedProgramInputs(fast.c, fast.r, fast.from, fast.roots, fast.sourceOwner, called, fast.deps)
			wantLabels(t, "fast minimization keeps the exact deps", fast.r.AttrStrings("deps"), exact.r.AttrStrings("deps"))
			wantStrings(t, "fast minimization keeps the exact srcs", fast.r.AttrStrings("srcs"), exact.r.AttrStrings("srcs"))
		})
	}
}

func TestResolveEdges_UnownedClosureStopsAtExistingOwners(t *testing.T) {
	for _, owner := range []string{"none", "kept by self", "kept root-qualified source", "same package owner", "indexed", "override", "tree", "dependency only"} {
		t.Run(owner, func(t *testing.T) {
			c, tc := edgeRepo(t, edgeListings)
			const a = "foreign/a.ts"
			const b = "foreign/nested/b.ts"
			for _, file := range []string{a, b, "foreign/nested/value.json", "foreign/unused.ts"} {
				writeFile(t, filepath.Join(c.RepoRoot, file), "")
			}
			tc.programs.emission.files["foreign"] = loadedEmptyBuild(t, filepath.Join(c.RepoRoot, "foreign/BUILD.bazel"), "foreign")
			tc.programs.emission.files["foreign/nested"] = loadedEmptyBuild(t, filepath.Join(c.RepoRoot, "foreign/nested/BUILD.bazel"), "foreign/nested")
			tc.programs.visit("foreign", []string{"a.ts", "unused.ts"})
			tc.programs.visit("foreign/nested", []string{"b.ts", "value.json"})
			rootEdge := importEdge("web/test.ts", "../foreign/a", a)
			tc.programs.programs["web"].Types = nil
			tc.programs.programs["web"].Edges = []explainfiles.Edge{
				rootEdge,
				importEdge(a, "./nested/b", b),
				importEdge(b, "../a", a),
				importEdge(b, "./value.json", "foreign/nested/value.json"),
				importEdge(a, "zod", storeZod),
				{From: storeZod, To: storeTypesNode, Kind: explainfiles.TypeReference, Specifier: "node"},
			}
			tc.programs.programs["web"].candidates = []resolutionCandidate{
				{from: a, path: "cold/output"},
				{from: b, path: "second/output"},
				{from: "foreign/unused.ts", path: "unused/output"},
			}
			rules := []indexedRule{
				{kind: "ts_codegen", name: "generated", pkg: "", outDir: "cold"},
				{kind: "ts_codegen", name: "second", pkg: "", outDir: "second"},
				{kind: "ts_codegen", name: "unused", pkg: "", outDir: "unused"},
			}
			switch owner {
			case "kept by self":
				rules = append(rules, indexedRule{kind: "ts_test", name: "web_test", pkg: "web", srcs: []string{"//foreign:a.ts"}})
			case "same package owner":
				rules = append(rules, indexedRule{kind: "ts_compile", name: "other", pkg: "web", srcs: []string{"//foreign:a.ts"}})
			case "indexed":
				rules = append(rules, indexedRule{kind: "ts_compile", name: "owner", pkg: "foreign", srcs: []string{"a.ts"}})
			case "override":
				f, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve typescript foreign/a.ts //foreign:owner\n"))
				if err != nil {
					t.Fatal(err)
				}
				(&resolve.Configurer{}).Configure(c, "", f)
			case "tree":
				rules = append(rules, indexedRule{kind: "ts_codegen", name: "owner", pkg: "", outDir: "foreign"})
			}
			ix := buildIndex(t, c, rules...)
			tc.programs.programs["web"].Roots = []string{rootEdge.From}
			tc.programs.selectProgram(c, tc.programs.programs["web"])
			if owner == "dependency only" {
				_, source := edgeDep(c, ix, tc, rootEdge, label.New("", "web", "web_test"), map[string]bool{}, nil, nil)
				if source {
					t.Fatal("dependency-only resolver selected a direct source")
				}
				return
			}
			r := rule.NewRule("ts_test", "web_test")
			if owner == "kept by self" {
				r.SetAttr("srcs", []string{"//foreign:a.ts"})
			} else if owner == "kept root-qualified source" {
				r.SetAttr("srcs", []string{"@//foreign:a.ts"})
			}
			logged := captureLog(t, func() {
				resolveEdges(c, ix, r, &ruleImports{
					program: tc.programs.programs["web"],
					edges:   []explainfiles.Edge{rootEdge},
				}, label.New("", "web", "web_test"))
			})
			if logged != "" {
				t.Fatalf("resolved closure reported missing: %s", logged)
			}
			var wantSrcs, wantScopes []string
			wantDeps := []string{"//foreign:owner"}
			if owner == "none" || owner == "kept by self" || owner == "kept root-qualified source" {
				wantSrcs = []string{"//foreign/nested:b.ts", "//foreign/nested:value.json", "//foreign:a.ts"}
				wantScopes = []string{"//:package.json"}
				if owner == "kept root-qualified source" {
					wantSrcs[2] = "@//foreign:a.ts"
				}
				wantDeps = []string{"//:generated", "//:second", "@npm//:types_node", "@npm//:zod"}
			} else if owner == "same package owner" {
				wantDeps = []string{":other"}
			} else if owner == "tree" {
				wantDeps = []string{"//:owner"}
			}
			wantStrings(t, "srcs", r.AttrStrings("srcs"), wantSrcs)
			wantLabels(t, "scope metadata", r.AttrStrings("package_scopes"), wantScopes)
			wantStrings(t, "deps", r.AttrStrings("deps"), wantDeps)
		})
	}
}

func TestKeptGeneratedRootsRetainSiblingRuntimeProducer(t *testing.T) {
	for _, forwarded := range []bool{false, true} {
		name := "direct"
		if forwarded {
			name = "filegroup and alias"
		}
		t.Run(name, func(t *testing.T) {
			source, forwarding := "//generated:entry.ts", ""
			if forwarded {
				source = ":roots"
				forwarding = "filegroup(name = \"roots\", srcs = [\"//forward:root\"])\n"
			}
			tree := map[string]string{
				"MODULE.bazel": "module(name = \"kept_generated_roots\")\n",
				"BUILD.bazel":  "# gazelle:exclude poison\n",
				"app/BUILD.bazel": loadDefs + `"ts_compile")
` + forwarding + `ts_compile(
    name = "app",
    srcs = ["index.ts", "` + source + `"], # keep
)
`,
				"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"},"files":["index.ts"]}`,
				"app/index.ts":      "export const local = 1;\n",
				"generated/BUILD.bazel": loadDefs + `"ts_codegen")
ts_codegen(name = "producer", outs = ["entry.ts", "helper.mjs"], generator = ":generator", visibility = ["//visibility:public"])
`,
				"poison/secret.ts":   "export type Secret = string;\n",
				"poison/runtime.mjs": "export const hidden = true;\n",
			}
			if forwarded {
				tree["forward/BUILD.bazel"] = `alias(name = "root", actual = "//forward/nested:roots", visibility = ["//visibility:public"])`
				tree["forward/nested/BUILD.bazel"] = `filegroup(name = "roots", srcs = ["//generated:entry.ts"], visibility = ["//visibility:public"])`
			}
			root := writeTree(t, tree)
			var absent map[string]string
			for _, state := range []string{"outputs absent", "stale outputs present"} {
				if state == "stale outputs present" {
					writeFile(t, filepath.Join(root, "generated/entry.ts"), "import { helper } from './helper.mjs'; export const value = helper; export type { Secret } from '../poison/secret';\n")
					writeFile(t, filepath.Join(root, "generated/helper.mjs"), "export { hidden } from '../poison/runtime.mjs'; export const helper = 1;\n")
				}
				for _, args := range [][]string{nil, {"-index=false", "-r=false", "app"}} {
					output, err := protoGazelle(t, root, args...)
					if err != nil {
						t.Fatalf("%s kept generated root with %v: %v\n%s", state, args, err, output)
					}
					consumer := onDiskRule(t, root, "app", "ts_compile", "app")
					wantStrings(t, state+" kept root spelling and membership", consumer.AttrStrings("srcs"), []string{"index.ts", source})
					wantLabels(t, state+" generated root retains its sibling runtime closure", consumer.AttrStrings("deps"), []string{"//generated:producer"})
					producer := onDiskRule(t, root, "generated", "ts_codegen", "producer")
					wantStrings(t, state+" producer still supplies the unselected sibling", producer.AttrStrings("outs"), []string{"entry.ts", "helper.mjs"})
					snapshot := convergeSnapshot(t, root)
					if absent == nil {
						absent = snapshot
					} else if diff := snapshotDiff(absent, snapshot); diff != "" {
						t.Fatalf("%s changed generated root closure: %s", state, diff)
					}
				}
			}
		})
	}
}

func TestGeneratedScalarSourcesRetainAuthoredScopeWithoutReadingCheckout(t *testing.T) {
	for _, selection := range []string{"import", "kept root"} {
		t.Run(selection, func(t *testing.T) {
			appBuild, index := "", "export { value } from '#generated';\n"
			if selection == "kept root" {
				appBuild = loadDefs + `"ts_compile")
ts_compile(
    name = "app",
    srcs = ["index.ts", "//app/generated:value.mjs"], # keep
)
`
				index = "export const local = 1;\n"
			}
			root := writeTree(t, map[string]string{
				"MODULE.bazel":      "module(name = \"generated_source_scope\")\n",
				"BUILD.bazel":       "# gazelle:exclude poison\n",
				"package.json":      `{"type":"module","imports":{"#fs":"node:missing"}}`,
				"app/BUILD.bazel":   appBuild,
				"app/package.json":  `{"type":"module","imports":{"#generated":"./generated/value.mjs"}}`,
				"app/tsconfig.json": `{"compilerOptions":{"allowJs":true,"module":"preserve","moduleResolution":"bundler","types":[]},"files":["index.ts"]}`,
				"app/index.ts":      index,
				"app/generated/BUILD.bazel": `genrule(
    name = "producer",
    outs = ["value.mjs"],
    cmd = "echo 'export { readFile as value } from \"#fs\";' > $@",
    visibility = ["//visibility:public"],
)
exports_files(["package.json"], visibility = ["//visibility:public"])
`,
				"app/generated/package.json": `{"type":"module","imports":{"#fs":"node:fs"}}`,
				"poison/secret.mjs":          "export const hidden = true;\n",
			})
			var absent map[string]string
			for _, state := range []string{"absent", "stale checkout"} {
				t.Run(state, func(t *testing.T) {
					if state == "stale checkout" {
						writeFile(t, filepath.Join(root, "app/generated/value.mjs"), "export { hidden as value } from '../../poison/secret.mjs';\n")
					}
					for _, args := range [][]string{nil, {"-index=false", "-r=false", "app"}} {
						output, err := protoGazelle(t, root, args...)
						if err != nil {
							t.Fatalf("%s generated source with %v: %v\n%s", state, args, err, output)
						}
						consumer := onDiskRule(t, root, "app", "ts_compile", "app")
						wantLabels(t, state+" retains the declared producer File", consumer.AttrStrings("srcs"), []string{"//app/generated:value.mjs", "index.ts"})
						wantLabels(t, state+" retains the nearest authored imports map", consumer.AttrStrings("package_scopes"), []string{"//app/generated:package.json", "package.json"})
						wantLabels(t, state+" source-only producer has no inferred dependency closure", consumer.AttrStrings("deps"), nil)
						producer := onDiskRule(t, root, "app/generated", "genrule", "producer")
						wantStrings(t, state+" keeps the scalar output identity", producer.AttrStrings("outs"), []string{"value.mjs"})
						snapshot := convergeSnapshot(t, root)
						if absent == nil {
							absent = snapshot
						} else if diff := snapshotDiff(absent, snapshot); diff != "" {
							t.Fatalf("%s changed generated source scope: %s", state, diff)
						}
					}
				})
			}
		})
	}
}

func TestGeneratedProducerLabelCannotBecomeTestRoot(t *testing.T) {
	c := config.New()
	c.RepoName = "test_roots"
	from := label.New(c.RepoName, "app", "app_test")
	roots := []string{"index.test.ts"}
	for _, helper := range []string{"//generated:helper.ts", "//generated:helper", "//generated:helper.d.ts"} {
		t.Run(helper, func(t *testing.T) {
			r := rule.NewRule("ts_test", from.Name)
			sources := []string{"//app:index.test.ts", helper}
			r.SetAttr("srcs", sources)
			retainTestRoots(c, r, from, roots)
			wantStrings(t, "producer label cannot enlarge execution roots", r.AttrStrings("test_srcs"), roots)
			wantStrings(t, "producer remains a compiler and runtime input", r.AttrStrings("srcs"), sources)
		})
	}
}

func TestResolveEdges_ExcludedSiblingOutputsKeepDeclaredOwners(t *testing.T) {
	for _, file := range []string{"value.d.ts", "value.ts"} {
		for _, overridden := range []bool{false, true} {
			name := file
			if overridden {
				name += " with override"
			}
			t.Run(name, func(t *testing.T) {
				c, _ := edgeRepo(t, edgeListings)
				output := path.Join("generated", file)
				writeFile(t, filepath.Join(c.RepoRoot, output), "export declare const value: number;\n")
				rules := []indexedRule{{kind: "ts_codegen", name: "producer", pkg: "generated", outs: []string{file}}}
				want := "//generated:producer"
				if file == "value.ts" {
					rules = append(rules, indexedRule{kind: "ts_compile", name: "compiled", pkg: "generated", srcs: []string{file}})
					want = "//generated:compiled"
				}
				if overridden {
					f, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve typescript "+output+" //generated:custom\n"))
					if err != nil {
						t.Fatal(err)
					}
					(&resolve.Configurer{}).Configure(c, "", f)
					want = "//generated:custom"
				}
				r, logged := resolveEdgesOf(t, c, buildIndex(t, c, rules...), "ts_compile", "web", "web", &ruleImports{
					edges: []explainfiles.Edge{importEdge("web/index.ts", "../generated/value", output)},
				})
				wantStrings(t, "deps", r.AttrStrings("deps"), []string{want})
				if len(r.AttrStrings("srcs")) != 0 || logged != "" {
					t.Fatalf("excluded generated copy became a source or failed resolution: %v, %s", r.AttrStrings("srcs"), logged)
				}
			})
		}
	}
}

func configPrograms(byFrom map[string][]explainfiles.Edge) map[string]*program {
	p := &program{}
	for _, file := range slices.Sorted(maps.Keys(byFrom)) {
		p.Roots = append(p.Roots, file)
		p.Edges = append(p.Edges, byFrom[file]...)
	}
	programs := map[string]*program{}
	for file := range byFrom {
		programs[file] = p
	}
	return programs
}
