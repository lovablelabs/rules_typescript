package typescript

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path"
	"reflect"
	"runtime"
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

// The Lovable monorepo's workers/download/test program, listed from its root
// with --explainFiles; the parser's own tests read the same file.
const sampleListing = "../ts/tools/explainfiles/testdata/" +
	"workers_download_test.listing.txt"

// A store as a full run from the repository root leaves it: every directory
// in dirs walked, every listing parsed and recorded under its directory.
func storeOf(t *testing.T, dirs []string, listings map[string]string,
) *programStore {
	t.Helper()
	s := newProgramStore()
	for _, dir := range dirs {
		s.visit(dir, nil)
		s.walked[dir] = true
	}
	for dir, text := range listings {
		p := programOf(t, dir, text)
		for _, f := range p.Files {
			if s.walked[parentDir(f)] {
				s.files[parentDir(f)] = append(s.files[parentDir(f)], path.Base(f))
			}
		}
		s.record(p)
	}
	return s
}

func programOf(t *testing.T, dir, text string) *program {
	t.Helper()
	l, err := explainfiles.Parse(text)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	return &program{Listing: *l, config: tsconfigIn(dir), dir: dir}
}

// A listing whose every file matched the include pattern of dir's tsconfig.
func listingOf(dir string, files ...string) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f + "\n   Matched by include pattern '**/*' in '" +
			dir + "/tsconfig.json'\n")
	}
	return b.String()
}

const (
	storeNode = "../../../.local/share/pnpm/store/v11/links/@types/node/" +
		"22.13.13/9435199b93d373e137e4c716aec7ae4974ab7bb876d51e74a0919a6d6d" +
		"780519/node_modules/@types/node/index.d.ts"
	libES5 = "../../../.cache/bazel/_bazel_mikn/445afab5f80dc0267f608ab4b5ec" +
		"802c/external/rules_typescript++ts+tsgo_linux_amd64/lib/lib.es5.d.ts"
	bundledDOM   = "bundled:///libs/lib.dom.d.ts"
	bundledES22  = "bundled:///libs/lib.es2022.full.d.ts"
	workerConfig = "workers/download/worker-configuration.d.ts"
	sampleDir    = "workers/download/test"
)

func TestOwner_FirstParty(t *testing.T) {
	for f, want := range map[string]bool{
		"workers/download/test/env.d.ts":      true,
		"a.ts":                                true,
		"packages/figma-plugin/manifest.json": true,
		storeNode:                             false,
		libES5:                                false,
		bundledDOM:                            false,
		"/abs/x.ts":                           false,
		"web/node_modules/@lovable.dev/pulse/x.ts": false,
		"my_node_modules/x.ts":                     true,
	} {
		if got := firstParty(f); got != want {
			t.Errorf("firstParty(%q) = %v, want %v", f, got, want)
		}
	}
}

func TestOwner_SampleListingIsATestOnlyPackage(t *testing.T) {
	s := storeOf(t, []string{"", "workers", "workers/download",
		"workers/download/src", sampleDir}, nil)
	data, err := os.ReadFile(sampleListing)
	if err != nil {
		t.Fatal(err)
	}
	p := programOf(t, sampleDir, string(data))
	for _, f := range p.Files {
		if s.walked[parentDir(f)] {
			s.files[parentDir(f)] = append(s.files[parentDir(f)], path.Base(f))
		}
	}
	s.record(p)

	if got := s.packageDirs(); !slices.Equal(got, []string{sampleDir}) {
		t.Errorf("packages = %q, want [%s]", got, sampleDir)
	}
	for f, want := range map[string]string{
		"workers/download/test/index.spec.ts": sampleDir,
		"workers/download/test/env.d.ts":      sampleDir,
		"workers/download/src/index.ts":       "",
		workerConfig:                          "",
		storeNode:                             "",
		libES5:                                "",
	} {
		if got, found := s.owner(f); got != want || found != (want != "") {
			t.Errorf("owner(%s) = %q, want %q", f, got, want)
		}
	}
	got := s.srcs(sampleDir, defaultTsConfig())
	want := srcSet{
		test:        []string{"workers/download/test/index.spec.ts"},
		declaration: []string{"workers/download/test/env.d.ts"},
	}
	if !got.equal(want) {
		t.Errorf("srcs(%s) = %+v, want %+v", sampleDir, got, want)
	}

	// The two files the test program reaches outside its directory belong to
	// the packages there once those list them.
	s.record(programOf(t, "workers/download/src",
		listingOf("workers/download/src", "workers/download/src/index.ts")))
	s.record(programOf(t, "workers/download", listingOf("workers/download",
		"workers/download/src/index.ts", workerConfig)))
	for f, want := range map[string]string{
		"workers/download/src/index.ts": "workers/download/src",
		workerConfig:                    "workers/download",
	} {
		if got, found := s.owner(f); got != want || found != (want != "") {
			t.Errorf("owner(%s) = %q, want %q", f, got, want)
		}
	}
	got = s.srcs("workers/download", defaultTsConfig())
	if !got.equal(srcSet{declaration: []string{workerConfig}}) {
		t.Errorf("srcs(workers/download) = %+v: src/index.ts is src's", got)
	}
}

func TestOwner_NearestPackageThatListsTheFileOwnsIt(t *testing.T) {
	s := storeOf(t, []string{"", "app", "app/lib", "app/lib/deep"},
		map[string]string{
			"app": listingOf("app", "app/main.ts", "app/main.test.ts",
				"app/lib/c.ts", "app/lib/d.ts", "app/lib/deep/e.ts"),
			"app/lib": listingOf("app/lib", "app/lib/c.ts",
				"app/lib/deep/e.ts", "app/main.ts"),
		})
	for f, want := range map[string]string{
		"app/main.ts":       "app",
		"app/main.test.ts":  "app",
		"app/lib/c.ts":      "app/lib",
		"app/lib/deep/e.ts": "app/lib",
		"app/lib/d.ts":      "",
	} {
		if got, found := s.owner(f); got != want || found != (want != "") {
			t.Errorf("owner(%s) = %q, want %q", f, got, want)
		}
	}
	if got := s.srcs("app", defaultTsConfig()); !got.equal(srcSet{
		library: []string{"app/main.ts"},
		test:    []string{"app/main.test.ts"},
	}) {
		t.Errorf("srcs(app) = %+v, want main.ts and its test alone", got)
	}
	if got := s.srcs("app/lib", defaultTsConfig()); !got.equal(srcSet{
		library: []string{"app/lib/c.ts", "app/lib/deep/e.ts"},
	}) {
		t.Errorf("srcs(app/lib) = %+v, want c.ts and deep/e.ts", got)
	}

	want := "typescript: app/lib/d.ts: no package owns it: " +
		"app/lib/tsconfig.json, the nearest, does not list it; " +
		"listed by app/tsconfig.json\n"
	if got := captureLog(t, s.reportUnowned); got != want {
		t.Errorf("reportUnowned said:\n%s\nwant:\n%s", got, want)
	}
}

func TestOwner_NoInputsIsNotAPackage(t *testing.T) {
	s := storeOf(t, []string{"", "site", "site/script", "site/src"},
		map[string]string{
			"site":        listingOf("site", "site/src/a.ts", "site/script/b.ts"),
			"site/script": noInputsOutput + "\n",
		})
	if got := s.packageDirs(); !slices.Equal(got, []string{"site"}) {
		t.Errorf("packages = %q, want [site]: TS18003 names no file", got)
	}
	if got, found := s.owner("site/script/b.ts"); !found || got != "site" {
		t.Errorf("owner(site/script/b.ts) = %q, want site", got)
	}
	if got := captureLog(t, s.reportUnowned); got != "" {
		t.Errorf("reportUnowned said:\n%s\nwant nothing", got)
	}
}

func TestOwner_RefusedAndLibraryOnlyListingsAreNotPackages(t *testing.T) {
	s := storeOf(t, []string{"", "libs", "embedded", "vendored"},
		map[string]string{
			"libs": libES5 + "\n   Default library\n" + storeNode +
				"\n   Entry point of type library 'node' specified in compilerOptions\n",
			"embedded": bundledES22 + "\n   Default library for target 'es2022'\n" +
				bundledDOM + "\n   Library referenced via 'dom' from file '" +
				bundledES22 + "'\n",
			"vendored": "vendored/node_modules/x/index.d.ts\n" +
				"   Matched by include pattern '**/*' in 'vendored/tsconfig.json'\n",
		})
	s.record(&program{dir: "", refused: "neither include nor files"})
	if got := s.packageDirs(); len(got) != 0 {
		t.Errorf("packages = %q, want none", got)
	}
	if got, found := s.owner("vendored/node_modules/x/index.d.ts"); found {
		t.Errorf("owner of a node_modules path = %q, want none", got)
	}
}

func TestOwner_OutDirFilesAreNeverSrcs(t *testing.T) {
	compiled := "web/shared/i18n/compiled"
	s := storeOf(t, []string{"", "web", "web/src", "web/shared",
		"web/shared/i18n", compiled}, map[string]string{
		"web": listingOf("web", "web/src/a.ts", compiled+"/messages.ts",
			compiled+"/en.ts"),
	})
	c := emptyConfig()
	c.Exts[languageName].(*tsConfig).programs = s
	tc := getConfig(c)
	r, f := newRule(indexedRule{kind: "ts_codegen", name: "messages", pkg: "web", outDir: "shared/i18n/compiled"})
	f.Rules = append(f.Rules, r)
	s.recordBuild(c, "web", f)
	got := s.srcs("web", tc)
	if !got.equal(srcSet{library: []string{"web/src/a.ts"}}) {
		t.Errorf("srcs(web) = %+v, want a.ts alone: compiled/ is an out_dir", got)
	}
	if got, found := s.owner(compiled + "/messages.ts"); !found || got != "web" {
		t.Errorf("owner of an out_dir file = %q, want web: it is web's program", got)
	}
}

func TestOwner_AFileUnderAnUnwalkedDirectoryIsUnowned(t *testing.T) {
	s := storeOf(t, []string{"", "app"}, map[string]string{
		"app": listingOf("app", "app/main.ts", "app/gen/x.ts",
			"app/gen/deep/y.ts"),
	})
	for _, f := range []string{"app/gen/x.ts", "app/gen/deep/y.ts"} {
		if got, found := s.owner(f); found {
			t.Errorf("owner(%s) = %q, want none: app/gen was not walked", f, got)
		}
	}
	got := s.srcs("app", defaultTsConfig())
	if !got.equal(srcSet{library: []string{"app/main.ts"}}) {
		t.Errorf("srcs(app) = %+v, want main.ts alone", got)
	}
	want := "typescript: app/gen/deep/y.ts: under app/gen/deep, which this " +
		"run did not walk (excluded or ignored); listed by app/tsconfig.json\n" +
		"typescript: app/gen/x.ts: under app/gen, which this run did not " +
		"walk (excluded or ignored); listed by app/tsconfig.json\n"
	if got := captureLog(t, s.reportUnowned); got != want {
		t.Errorf("reportUnowned said:\n%s\nwant:\n%s", got, want)
	}
}

// The source-built compiler embeds its libs and lists them under its
// bundled:/// scheme, where the lockfile's binary lists them under ../.
func TestOwner_TheCompilersEmbeddedLibsAreItsOwn(t *testing.T) {
	s := storeOf(t, []string{"", "app"}, map[string]string{
		"app": listingOf("app", "app/main.ts") +
			bundledES22 + "\n   Default library for target 'es2022'\n" +
			bundledDOM + "\n   Library referenced via 'dom' from file '" +
			bundledES22 + "'\n",
	})
	if got := s.packageDirs(); !slices.Equal(got, []string{"app"}) {
		t.Errorf("packages = %q, want [app]", got)
	}
	if got, found := s.owner(bundledDOM); found {
		t.Errorf("owner(%s) = %q, want none: it is the compiler's", bundledDOM, got)
	}
	got := s.srcs("app", defaultTsConfig())
	if !got.equal(srcSet{library: []string{"app/main.ts"}}) {
		t.Errorf("srcs(app) = %+v, want main.ts alone", got)
	}
	if got := captureLog(t, s.reportUnowned); got != "" {
		t.Errorf("reportUnowned said:\n%s\nwant nothing: the libs are the "+
			"compiler's", got)
	}
}

func TestOwner_NoPackageAboveIsSaid(t *testing.T) {
	s := storeOf(t, []string{"", "web", "schema", "schema/fixtures"},
		map[string]string{
			"web": listingOf("web", "web/a.ts", "schema/fixtures/evidence.json"),
		})
	want := "typescript: schema/fixtures/evidence.json: no package owns it: " +
		"no tsconfig.json above it lists a file; listed by web/tsconfig.json\n"
	if got := captureLog(t, s.reportUnowned); got != want {
		t.Errorf("reportUnowned said:\n%s\nwant:\n%s", got, want)
	}
}

// A run over one subtree has not listed the packages above it, so it cannot
// tell an unlisted owner from a missing one.
func TestOwner_ReportNeedsTheRootWalked(t *testing.T) {
	s := storeOf(t, []string{"app", "app/lib"}, map[string]string{
		"app":     listingOf("app", "app/lib/d.ts"),
		"app/lib": listingOf("app/lib", "app/lib/c.ts"),
	})
	if got := captureLog(t, s.reportUnowned); got != "" {
		t.Errorf("a partial run reported:\n%s", got)
	}
}

func TestOwner_ClassifyByName(t *testing.T) {
	for f, want := range map[string]fileClass{
		"pkg/a.ts":              libraryFile,
		"pkg/a.tsx":             libraryFile,
		"pkg/a.mts":             libraryFile,
		"pkg/a.test.ts":         testFile,
		"pkg/a.spec.tsx":        testFile,
		"pkg/test/a.test.mjs":   testFile,
		"pkg/a.d.ts":            declarationFile,
		"pkg/a.d.mts":           declarationFile,
		"pkg/a.d.cts":           declarationFile,
		"pkg/a.test.d.ts":       declarationFile,
		"pkg/testing/helper.ts": libraryFile,
	} {
		if got := classify(f); got != want {
			t.Errorf("classify(%s) = %v, want %v", f, got, want)
		}
	}
}

func TestOwner_CensusUnderVerbose(t *testing.T) {
	s := storeOf(t, []string{"", "app", "site", "site/script"},
		map[string]string{
			"app":         listingOf("app", "app/a.ts"),
			"site":        listingOf("site", "site/a.ts"),
			"site/script": noInputsOutput + "\n",
		})
	s.record(&program{dir: "", refused: "neither include nor files"})
	if got := captureLog(t, s.reportCensus); got != "" {
		t.Errorf("the census was said without -ts_verbose:\n%s", got)
	}
	s.verbose = true
	want := "typescript: 4 tsconfig.json: 2 packages, 1 refused, " +
		"1 listing no first-party file\n"
	if got := captureLog(t, s.reportCensus); got != want {
		t.Errorf("the census said:\n%s\nwant:\n%s", got, want)
	}
}

func TestOwner_AFileUnderAForeignProjectIsUnowned(t *testing.T) {
	s := newProgramStore()
	s.foreign["app/example"] = "app/example/package.json"
	for _, dir := range []string{"", "app"} {
		s.visit(dir, nil)
		s.walked[dir] = true
	}
	s.visit("app", []string{"main.ts"})
	s.visit("app/example", []string{"notes.md", "x.ts"})
	s.walked["app/example"] = true
	s.record(programOf(t, "app",
		listingOf("app", "app/main.ts", "app/example/x.ts")))
	if got, found := s.owner("app/example/x.ts"); found {
		t.Errorf("owner(app/example/x.ts) = %q, want none: a foreign project's",
			got)
	}
	got := s.srcs("app", defaultTsConfig())
	if !got.equal(srcSet{library: []string{"app/main.ts"}}) {
		t.Errorf("srcs(app) = %+v, want main.ts alone", got)
	}
	want := "typescript: app/example/x.ts: under app/example, a project pnpm " +
		"installs nothing for: app/example/package.json is no importer in " +
		"pnpm-lock.yaml; listed by app/tsconfig.json\n"
	if got := captureLog(t, s.reportUnowned); got != want {
		t.Errorf("reportUnowned said:\n%s\nwant:\n%s", got, want)
	}
}

// Selections taken from workers must leave the store and its recorder as the serial pass does;
// a view's _pb.ts answer that another language's cross-resolver changes must not be taken.
func TestPreselection_TakenSelectionsMatchTheSerialPass(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross_resolver=%t", cross), func(t *testing.T) { preselectionsMatchSerial(t, cross) })
	}
}

// sortedSelections are the recorded selections with their candidates in block order, as map iteration chose them.
func sortedSelections(s *programStore) []selectionFacts {
	selections := slices.Clone(s.importFacts.facts.Selections)
	for i := range selections {
		selections[i].Selected = slices.SortedFunc(slices.Values(selections[i].Selected), func(a, b candidateKey) int { return cmp.Compare(a.Block, b.Block) })
	}
	return selections
}

type protoCrossResolver struct{}

func (protoCrossResolver) CrossResolve(_ *config.Config, _ *resolve.RuleIndex, imp resolve.ImportSpec, _ string) []resolve.FindResult {
	if imp.Imp != "v1.proto" {
		return nil
	}
	return []resolve.FindResult{{Label: label.New("", "api", "v1_proto")}}
}

func preselectionsMatchSerial(t *testing.T, cross bool) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	const n = 16
	run := func(parallel bool) (*programStore, int, map[string]string) {
		rec := &resolveRecorder{outsideRead: map[string]string{}, treeRead: map[string]string{}}
		resolvePhase.Store(rec)
		defer resolvePhase.Store(nil)
		c := emptyConfig()
		root, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve typescript shared/data.json //shared:data\n# gazelle:resolve typescript loose/value.json //loose:value\n"))
		if err != nil {
			t.Fatal(err)
		}
		(&resolve.Configurer{}).Configure(c, "", root)
		s := getConfig(c).programs
		s.importFacts = newImportFactsRecorder(nil)
		var outs []string
		for i := range n {
			outs = append(outs, fmt.Sprintf("%q", fmt.Sprintf("g%d.ts", i)))
		}
		for pkg, build := range map[string]string{
			"gen":    "genrule(name = \"g\", outs = [" + strings.Join(outs, ", ") + "])\nts_codegen(name = \"tree\", out_dir = \"tree\")",
			"api":    `ts_proto_library(name = "v1_ts", proto = ":v1_proto", out_dir = ".")`,
			"shared": "filegroup(name = \"data\", srcs = [\"data.json\"])",
		} {
			f, err := rule.LoadData(pkg+"/BUILD.bazel", pkg, []byte(build))
			if err != nil {
				t.Fatal(err)
			}
			s.recordBuild(c, pkg, f)
			s.walked[pkg] = true
		}
		s.visit("shared", []string{"data.json"})
		for i := range n {
			pkg := fmt.Sprintf("p%d", i)
			f, err := rule.LoadData(pkg+"/BUILD.bazel", pkg, []byte(fmt.Sprintf("ts_compile(name = %q, srcs = [\"index.ts\", \"//gen:g\", \"//shared:data\"])", pkg)))
			if err != nil {
				t.Fatal(err)
			}
			s.recordBuild(c, pkg, f)
			s.visit(pkg, []string{"index.ts", "orphan.ts"})
			s.walked[pkg] = true
		}
		s.index = buildIndex(t, c)
		if cross {
			s.index = resolve.NewRuleIndex(func(*rule.Rule, string) resolve.Resolver { return &tsLang{} }, protoCrossResolver{})
			s.index.Finish()
		}
		s.generatedFile("gen/g0.ts", true)
		for i := range n {
			pkg, generated := fmt.Sprintf("p%d", i), fmt.Sprintf("gen/g%d.ts", i)
			index := pkg + "/index.ts"
			s.inputs[pkg] = programInput{config: s.inputs[pkg].config, program: &program{dir: pkg, key: "listing-" + pkg, Listing: explainfiles.Listing{
				Roots: []string{index},
				Files: []string{index, pkg + "/orphan.ts", generated, "shared/data.json", "gen/tree/x.ts", "api/v1_pb.ts"},
				Edges: []explainfiles.Edge{importEdge(index, "../gen/x", generated), importEdge(index, "../shared/data.json", "shared/data.json"),
					importEdge(index, "../gen/tree/x", "gen/tree/x.ts"), importEdge(index, "../api/v1_pb", "api/v1_pb.ts")},
			}, candidates: []resolutionCandidate{
				{from: index, specifier: "../gen/x", path: generated, file: true, resolved: generated},
				{from: index, specifier: "../shared/data.json", path: "shared/data.json", file: true, resolved: "shared/data.json", block: 1},
				{from: index, specifier: "../gen/tree/x", path: "gen/tree/x.ts", file: true, resolved: "gen/tree/x.ts", block: 2},
				{from: index, specifier: "../api/v1_pb", path: "api/v1_pb.ts", file: true, resolved: "api/v1_pb.ts", block: 3},
				{from: index, specifier: "../loose/value.json", path: "loose/value.json", file: true, resolved: "loose/value.json", block: 4},
			}}}
		}
		var pre map[string]*preselection
		if parallel {
			pre = s.preselect()
		}
		taken := 0
		for i := range n {
			if s.selectInputFrom(fmt.Sprintf("p%d", i), pre[fmt.Sprintf("p%d", i)]) {
				taken++
			}
		}
		return s, taken, rec.outsideRead
	}
	serial, _, serialOutside := run(false)
	parallel, taken, parallelOutside := run(true)
	if len(serialOutside) == 0 || !maps.Equal(parallelOutside, serialOutside) {
		t.Errorf("Resolve-phase outside reads = %v, want the serial pass's %v", parallelOutside, serialOutside)
	}
	if want := map[bool]int{false: n, true: 0}[cross]; taken != want {
		t.Fatalf("%d of %d preselections taken from an unchanged store, want %d", taken, n, want)
	}
	if !reflect.DeepEqual(sortedSelections(parallel), sortedSelections(serial)) || !maps.Equal(parallel.importFacts.looked, serial.importFacts.looked) ||
		!maps.Equal(parallel.importFacts.programs, serial.importFacts.programs) {
		t.Errorf("recorded selections %v, looked up %v; the serial pass records %v, %v",
			sortedSelections(parallel), parallel.importFacts.looked, sortedSelections(serial), serial.importFacts.looked)
	}
	if !reflect.DeepEqual(parallel.packages, serial.packages) {
		t.Errorf("packages = %v, want the serial pass's %v", parallel.packages, serial.packages)
	}
	for dir, p := range serial.programs {
		got := parallel.programs[dir]
		if !slices.Equal(got.Files, p.Files) || !slices.Equal(got.Edges, p.Edges) || !slices.Equal(got.candidates, p.candidates) {
			t.Errorf("program %s = %v %v, want the serial pass's %v %v", dir, got.Files, got.Edges, p.Files, p.Edges)
		}
	}
}

// A view that mispredicts kept sources must leave the input to the serial pass.
func TestPreselection_KeptSourcesTheViewMispredictsAreSelectedSerially(t *testing.T) {
	c := emptyConfig()
	s := getConfig(c).programs
	lib, err := rule.LoadData("lib/BUILD.bazel", "lib", []byte(`ts_compile(
    name = "lib",
    srcs = [
        "extra.ts",  # keep
    ] + select({"@platforms//os:linux": ["index.ts"]}),
)
`))
	if err != nil {
		t.Fatal(err)
	}
	s.recordBuild(c, "lib", lib)
	s.visit("lib", []string{"index.ts", "extra.ts"})
	s.walked["lib"] = false
	s.index = buildIndex(t, c)
	s.inputs["lib"] = programInput{config: s.inputs["lib"].config, program: &program{dir: "lib", Listing: explainfiles.Listing{
		Roots: []string{"lib/index.ts"},
		Files: []string{"lib/index.ts", "lib/extra.ts"},
	}}}
	s.selectInputFrom("lib", s.preselect()["lib"])
	if !slices.Contains(s.programs["lib"].Files, "lib/extra.ts") {
		t.Errorf("lib's program files = %v, want the kept lib/extra.ts retained", s.programs["lib"].Files)
	}
}

// A selection made before a store write must not be committed after it: here
// the write declares a file the selection treated as authored.
func TestPreselection_AWriteAfterTheSnapshotIsSeenByTheCommit(t *testing.T) {
	c := emptyConfig()
	s := getConfig(c).programs
	lib, err := rule.LoadData("lib/BUILD.bazel", "lib", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.recordBuild(c, "lib", lib)
	s.visit("lib", []string{"index.ts"})
	s.walked["lib"] = true
	s.visit("gen", []string{"value.ts"})
	s.walked["gen"] = true
	s.index = buildIndex(t, c)
	s.inputs["lib"] = programInput{config: c, program: &program{dir: "lib", Listing: explainfiles.Listing{
		Roots: []string{"lib/index.ts"},
		Files: []string{"lib/index.ts", "gen/value.ts"},
		Edges: []explainfiles.Edge{importEdge("lib/index.ts", "../gen/value", "gen/value.ts")},
	}}}
	pre := s.preselect()["lib"]
	if pre == nil || !pre.files["gen/value.ts"] {
		t.Fatalf("preselection = %+v, want gen/value.ts authored before the write", pre)
	}
	gen, err := rule.LoadData("gen/BUILD.bazel", "gen", []byte(`genrule(name = "value", outs = ["value.ts"])`))
	if err != nil {
		t.Fatal(err)
	}
	s.recordBuild(c, "gen", gen)
	s.selectInputFrom("lib", pre)
	if s.packages["lib"]["gen/value.ts"] {
		t.Error("lib's package keeps gen/value.ts, which a genrule now outputs: the commit took the stale preselection")
	}
	if slices.Contains(s.programs["lib"].Files, "gen/value.ts") {
		t.Errorf("lib's program files = %v, want the generated gen/value.ts dropped", s.programs["lib"].Files)
	}
}

// A view must not load a BUILD file into the shared store; the serial pass loads it in order.
func TestPreselection_ASelectionThatMustLoadIsLeftToTheSerialPass(t *testing.T) {
	c := emptyConfig()
	s := getConfig(c).programs
	lib, err := rule.LoadData("lib/BUILD.bazel", "lib", []byte(`ts_compile(
    name = "lib",
    srcs = [
        "index.ts",
        "//gen:files",  # keep
    ],
)
`))
	if err != nil {
		t.Fatal(err)
	}
	s.recordBuild(c, "lib", lib)
	s.visit("lib", []string{"index.ts"})
	s.walked["lib"] = true
	gen, err := rule.LoadData("gen/BUILD.bazel", "gen", []byte(`filegroup(name = "files", srcs = ["//shared:value.ts"])`))
	if err != nil {
		t.Fatal(err)
	}
	s.emission.setFile("gen", gen)
	s.inputs["gen"] = programInput{config: c}
	s.visit("shared", []string{"value.ts"})
	s.index = buildIndex(t, c)
	s.inputs["lib"] = programInput{config: s.inputs["lib"].config, program: &program{dir: "lib", Listing: explainfiles.Listing{
		Roots: []string{"lib/index.ts"},
		Files: []string{"lib/index.ts", "shared/value.ts"},
	}}}
	pre := s.preselect()["lib"]
	if s.emission.rules["//gen:files"] != nil {
		t.Fatal("a view stored //gen:files in the shared store")
	}
	if pre != nil {
		t.Errorf("preselection retained %v, want none: the selection reads //gen:files before the store loads it", pre.retained)
	}
	s.selectInputFrom("lib", pre)
	if s.emission.rules["//gen:files"] == nil || !slices.Contains(s.programs["lib"].Files, "shared/value.ts") {
		t.Errorf("lib's program files = %v, want the kept shared/value.ts after the serial pass loads //gen:files", s.programs["lib"].Files)
	}
}

// A fatal a view reaches is the serial pass's to report, in its order.
func TestPreselection_AFatalInAViewIsLeftToTheSerialPass(t *testing.T) {
	c := emptyConfig()
	s := getConfig(c).programs
	lib, err := rule.LoadData("lib/BUILD.bazel", "lib", []byte(`ts_compile(
    name = "lib",
    srcs = [
        "index.ts",
        "@other//:value",  # keep
    ],
)
`))
	if err != nil {
		t.Fatal(err)
	}
	s.recordBuild(c, "lib", lib)
	s.visit("lib", []string{"index.ts"})
	s.walked["lib"] = true
	s.index = buildIndex(t, c)
	s.inputs["lib"] = programInput{config: s.inputs["lib"].config, program: &program{dir: "lib", Listing: explainfiles.Listing{
		Roots: []string{"lib/index.ts"},
		Files: []string{"lib/index.ts"},
	}}}
	if pre := s.preselect()["lib"]; pre != nil {
		t.Errorf("preselection retained %v, want none for a selection that ends in a fatal", pre.retained)
	}
}

// refreshStore is n packages' programs, each importing gen/value.ts and api/v1_pb.ts, with their refreshes pending.
func refreshStore(t *testing.T, n int) (*programStore, *config.Config, []*program) {
	t.Helper()
	c := emptyConfig()
	root, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve typescript shared/data.json //shared:data\n"+
		"# gazelle:resolve typescript loose/value.json //loose:value\n# gazelle:resolve typescript gen/data.json //gen:data\n"))
	if err != nil {
		t.Fatal(err)
	}
	(&resolve.Configurer{}).Configure(c, "", root)
	s := getConfig(c).programs
	s.importFacts = newImportFactsRecorder(nil)
	for pkg, build := range map[string]string{"": "", "gen": `genrule(name = "value")`, "api": `ts_proto_library(name = "v1_ts", proto = "//:v1_proto", out_dir = ".")`} {
		s.recordBuild(c, pkg, loadBuild(t, pkg, build))
	}
	s.visit("gen", []string{"value.ts"})
	s.walked["gen"] = true
	s.visit("shared", []string{"data.json"})
	var programs []*program
	for i := range n {
		pkg := fmt.Sprintf("lib%d", i)
		index := pkg + "/index.ts"
		lib := loadBuild(t, pkg, fmt.Sprintf(`ts_compile(name = %q, srcs = ["index.ts"], tsconfig = "tsconfig.json")`, pkg))
		s.recordBuild(c, pkg, lib)
		s.visit(pkg, []string{"index.ts", "orphan.ts", "tsconfig.json"})
		s.walked[pkg] = true
		p := &program{dir: pkg, key: "listing-" + pkg, Listing: explainfiles.Listing{
			Roots: []string{index},
			Files: []string{index, pkg + "/orphan.ts", "gen/value.ts", "shared/data.json", "api/v1_pb.ts", "gen/tree/x.ts", "gen/data.json", "loose/value.json"},
			Edges: []explainfiles.Edge{importEdge(index, "../gen/value", "gen/value.ts"), importEdge(index, "../shared/data.json", "shared/data.json"),
				importEdge(index, "../api/v1_pb", "api/v1_pb.ts"), importEdge(index, "../gen/tree/x", "gen/tree/x.ts")},
		}, candidates: []resolutionCandidate{
			{from: index, specifier: "../gen/value", path: "gen/value.ts", file: true, resolved: "gen/value.ts"},
			{from: index, specifier: "../shared/data.json", path: "shared/data.json", file: true, resolved: "shared/data.json", block: 1},
			{from: index, specifier: "../loose/value.json", path: "loose/value.json", file: true, resolved: "loose/value.json", block: 2},
			{from: index, specifier: "../gen/data.json", path: "gen/data.json", file: true, resolved: "gen/data.json", block: 3},
		}}
		s.inputs[pkg] = programInput{config: s.inputs[pkg].config, program: p, refresh: &programRefresh{
			language.GenerateArgs{Config: c, Rel: pkg, File: lib},
			language.GenerateResult{Gen: []*rule.Rule{rule.NewRule("ts_compile", pkg)}, Imports: []any{&ruleImports{}}},
		}}
		programs = append(programs, p)
	}
	s.index = buildIndex(t, c)
	return s, c, programs
}

func loadBuild(t *testing.T, pkg, build string) *rule.File {
	t.Helper()
	f, err := rule.LoadData(path.Join(pkg, "BUILD.bazel"), pkg, []byte(build))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A refresh's selection taken from a view must leave the recorders and the program as the serial selection does.
func TestPrerefresh_TakenSelectionsMatchTheSerialSelection(t *testing.T) {
	run := func(predicted bool) (*programStore, *program, map[string]string) {
		rec := &resolveRecorder{outsideRead: map[string]string{}, treeRead: map[string]string{}}
		resolvePhase.Store(rec)
		defer resolvePhase.Store(nil)
		s, c, programs := refreshStore(t, 1)
		key := refreshKey{c, programs[0]}
		if predicted {
			s.prerefreshed = s.prerefresh()
			if len(s.prerefreshed[key]) != 1 {
				t.Fatalf("predicted %v, want lib0's selection from its stored sources", s.prerefreshed)
			}
		}
		selected := s.refreshSelection(c, programs[0], []string{"lib0/index.ts"})
		if predicted && len(s.prerefreshed[key]) != 0 {
			t.Error("lib0's predicted selection was not taken from an unchanged store")
		}
		return s, selected, rec.outsideRead
	}
	serial, serialProgram, serialOutside := run(false)
	parallel, parallelProgram, parallelOutside := run(true)
	if len(serialOutside) == 0 || !maps.Equal(parallelOutside, serialOutside) {
		t.Errorf("Resolve-phase outside reads = %v, want the serial selection's %v", parallelOutside, serialOutside)
	}
	if !reflect.DeepEqual(sortedSelections(parallel), sortedSelections(serial)) || !maps.Equal(parallel.importFacts.looked, serial.importFacts.looked) {
		t.Errorf("recorded selections %v, looked up %v; the serial selection records %v, %v", sortedSelections(parallel),
			parallel.importFacts.looked, sortedSelections(serial), serial.importFacts.looked)
	}
	if !slices.Equal(parallelProgram.Files, serialProgram.Files) || !slices.Equal(parallelProgram.Edges, serialProgram.Edges) {
		t.Errorf("selected %v %v, want the serial selection's %v %v", parallelProgram.Files, parallelProgram.Edges, serialProgram.Files, serialProgram.Edges)
	}
}

// A view's selection must not be taken once a value it read has changed: here a stored rule
// changes what one file is. One worker selects both programs, so the second reads its first's memo.
func TestPrerefresh_AStaleReadIsSelectedAgain(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for _, tc := range []struct {
		name, file, key, pkg, build string
		selected                    bool
	}{
		{"genrule output", "gen/value.ts", "//gen:value", "gen", `genrule(name = "value", outs = ["value.ts"])`, false},
		{"codegen tree", "gen/tree/x.ts", "//gen:value", "gen", `ts_codegen(name = "value", out_dir = "tree")`, false},
		{"nonliteral outputs", "gen/data.json", "//gen:value", "gen", `genrule(name = "value", outs = OUTS)`, true},
		{"new BUILD file", "loose/value.json", "", "loose", `genrule(name = "value", outs = OUTS)`, true},
		{"proto provider", "api/v1_pb.ts", "//:v1_proto", "", `proto_library(name = "v1_proto", srcs = ["v1.proto"])`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, programs := refreshStore(t, 2)
			if tc.file != "gen/data.json" {
				// gen/data.json's override reads gen without the memo, which would hide a memo read the second job missed.
				for _, p := range programs {
					p.candidates = slices.DeleteFunc(p.candidates, func(c resolutionCandidate) bool { return c.path == "gen/data.json" })
				}
			}
			s.prerefreshed = s.prerefresh()
			for _, p := range programs {
				if pre := s.prerefreshed[refreshKey{c, p}]; len(pre) != 1 || slices.Contains(pre[0].selected.Files, tc.file) == tc.selected {
					t.Fatalf("predicted %v, want %s's selection before the write", s.prerefreshed, p.dir)
				}
			}
			if f := loadBuild(t, tc.pkg, tc.build); tc.key == "" {
				s.recordBuild(c, tc.pkg, f)
			} else {
				s.emission.setRule(tc.key, f.Rules[0])
			}
			for _, p := range programs {
				if selected := s.refreshSelection(c, p, []string{p.dir + "/index.ts"}); slices.Contains(selected.Files, tc.file) != tc.selected {
					t.Errorf("%s's selected files = %v, want %s selected: %t", p.dir, selected.Files, tc.file, tc.selected)
				}
			}
		})
	}
}

// A view's selection from other roots must not answer a refresh whose sources changed.
func TestPrerefresh_OtherRootsAreSelectedAgain(t *testing.T) {
	s, c, programs := refreshStore(t, 1)
	s.prerefreshed = s.prerefresh()
	if len(s.prerefreshed[refreshKey{c, programs[0]}]) != 1 {
		t.Fatalf("predicted %v, want lib0's selection from its stored sources", s.prerefreshed)
	}
	if selected := s.refreshSelection(c, programs[0], []string{"lib0/orphan.ts"}); !slices.Contains(selected.Files, "lib0/orphan.ts") {
		t.Errorf("selected files = %v, want the root lib0/orphan.ts", selected.Files)
	}
}

// The first pass's stamp does not cover proto answers, which other languages' resolvers may give;
// here a proto observation comes to make a _pb.ts the view read as authored generated.
func TestPreselection_AProtoAnswerThatChangesIsSelectedAgain(t *testing.T) {
	s, c, _ := refreshStore(t, 1)
	pre := s.preselect()["lib0"]
	if pre == nil || !pre.files["api/v1_pb.ts"] {
		t.Fatalf("preselection = %v, want api/v1_pb.ts authored before the observation", pre)
	}
	protos := getConfig(c).protos
	protos.observations["proto"] = append(protos.observations["proto"], protoObservation{native: label.New("", "", "v1_proto"), paths: []string{"v1.proto"}})
	s.selectInputFrom("lib0", pre)
	if slices.Contains(s.programs["lib0"].Files, "api/v1_pb.ts") {
		t.Errorf("lib0's program files = %v, want the now generated api/v1_pb.ts dropped", s.programs["lib0"].Files)
	}
}

// A view must not answer from a package with an unobserved rule: what it read before reaching that rule
// goes unrecorded. Nothing under gen but gen/value.ts is read, so no other scan reaches the rule first.
func TestPrerefresh_AnUnobservedRuleInAReadPackageIsSelectedAgain(t *testing.T) {
	s, c, programs := refreshStore(t, 1)
	p := programs[0]
	p.candidates = slices.DeleteFunc(p.candidates, func(c resolutionCandidate) bool { return strings.HasPrefix(c.path, "gen/") })
	p.Edges = slices.DeleteFunc(p.Edges, func(e explainfiles.Edge) bool { return strings.HasPrefix(e.To, "gen/") && e.To != "gen/value.ts" })
	gen := loadBuild(t, "gen", "genrule(name = \"value\", outs = [\"value.ts\"])\nfilegroup(name = \"other\")")
	s.emission.setFile("gen", gen)
	s.emission.setRule("//gen:value", gen.Rules[0])
	s.prerefreshed = s.prerefresh()
	s.emission.setRule("//gen:value", loadBuild(t, "gen", `genrule(name = "value")`).Rules[0])
	if selected := s.refreshSelection(c, programs[0], []string{"lib0/index.ts"}); !slices.Contains(selected.Files, "gen/value.ts") {
		t.Errorf("selected files = %v, want gen/value.ts, which no rule outputs any more", selected.Files)
	}
}

// A fatal a prediction reaches is the refresh's to report, in its order.
func TestPrerefresh_AFatalInAPredictionIsLeftToTheRefresh(t *testing.T) {
	s, _, _ := refreshStore(t, 1)
	s.inputs["lib0"].refresh.args.File.Rules[0].SetAttr("srcs", []string{"index.ts", "@other//:value"})
	if predicted := s.prerefresh(); len(predicted) != 0 {
		t.Errorf("predicted %v, want none for a rule whose sources end in a fatal", predicted)
	}
}

// Views crawling one registered package write only their own registration and output index.
func TestPreselection_ViewsCrawlingARegisteredPackageWriteOnlyTheirOwnState(t *testing.T) {
	const n = 32
	c := emptyConfig()
	s := getConfig(c).programs
	gen := loadBuild(t, "gen", `filegroup(name = "files", srcs = ["value.ts"])`)
	s.recordBuild(c, "gen", gen)
	s.emission.recordRegistered("gen", gen)
	s.visit("gen", []string{"value.ts"})
	for i := range n {
		pkg := fmt.Sprintf("lib%d", i)
		s.recordBuild(c, pkg, loadBuild(t, pkg, fmt.Sprintf("ts_compile(\n    name = %q,\n    srcs = [\n        \"index.ts\",\n        \"//gen:files\",  # keep\n    ],\n)\n", pkg)))
		s.visit(pkg, []string{"index.ts"})
		s.walked[pkg] = true
		s.inputs[pkg] = programInput{config: s.inputs[pkg].config, program: &program{dir: pkg, Listing: explainfiles.Listing{
			Roots: []string{pkg + "/index.ts"},
			Files: []string{pkg + "/index.ts", "gen/value.ts"},
		}}}
	}
	s.index = buildIndex(t, c)
	s.emission.listEpoch++
	s.packageOutputs("gen", gen)
	registered, outputs := s.emission.registered["gen"], maps.Clone(s.emission.outputIndex)
	pre := s.preselect()
	if s.emission.registered["gen"].epoch != registered.epoch || !maps.Equal(s.emission.outputIndex, outputs) {
		t.Error("views wrote the store's registration or output index")
	}
	for i := range n {
		if p := pre[fmt.Sprintf("lib%d", i)]; p == nil || !slices.Equal(p.retained, []string{"gen/value.ts"}) {
			t.Fatalf("lib%d's preselection = %v, want it to keep gen/value.ts", i, p)
		}
	}
}

// Gazelle's merge logs a srcs that is not a list; a run must log it once, from the serial pass.
func TestPreselection_ANonListSrcsIsLoggedOnce(t *testing.T) {
	c := emptyConfig()
	s := getConfig(c).programs
	s.recordBuild(c, "lib", loadBuild(t, "lib", `ts_compile(name = "lib", srcs = glob(["*.ts"]))`))
	s.visit("lib", []string{"index.ts"})
	s.walked["lib"] = false
	s.index = buildIndex(t, c)
	s.inputs["lib"] = programInput{config: s.inputs["lib"].config, program: &program{dir: "lib", Listing: explainfiles.Listing{
		Roots: []string{"lib/index.ts"},
		Files: []string{"lib/index.ts"},
	}}}
	logged := captureLog(t, func() { s.selectInputFrom("lib", s.preselect()["lib"]) })
	if n := strings.Count(logged, "could not merge expression"); n != 1 {
		t.Errorf("logged %q, want the merge's message once", logged)
	}
}

// A view's reads reach the Resolve-phase recorder only when its selection is taken.
func TestPrerefresh_AViewsReadsAreNotedOnlyWhenTaken(t *testing.T) {
	rec := &resolveRecorder{outsideRead: map[string]string{}, treeRead: map[string]string{}}
	resolvePhase.Store(rec)
	defer resolvePhase.Store(nil)
	s, c, programs := refreshStore(t, 1)
	s.prerefreshed = s.prerefresh()
	if len(rec.outsideRead) != 0 {
		t.Errorf("Resolve-phase outside reads %v before any selection was taken", rec.outsideRead)
	}
	s.refreshSelection(c, programs[0], []string{"lib0/index.ts"})
	if _, read := rec.outsideRead["loose"]; !read {
		t.Errorf("Resolve-phase outside reads %v, want the taken selection's read of loose", rec.outsideRead)
	}
}

// A read package whose rule is deleted while its file still lists it is unobserved, which no view read.
func TestPrerefresh_AReadPackageThatBecomesUnobservedIsSelectedAgain(t *testing.T) {
	s, c, programs := refreshStore(t, 1)
	s.prerefreshed = s.prerefresh()
	s.emission.deleteRule("//gen:value")
	s.refreshSelection(c, programs[0], []string{"lib0/index.ts"})
	if len(s.prerefreshed[refreshKey{c, programs[0]}]) != 1 {
		t.Error("lib0's predicted selection was taken after gen became unobserved")
	}
}

// A walk answer must not outlive a write that changes it: the walk has no index yet, so its
// selections and owner lookups share one memo per callback, stamped by the writes below.
func TestWalkMemo_AnAnswerIsRecomputedAfterEachWriteThatChangesIt(t *testing.T) {
	treeOwner := func(s *programStore) string {
		owner, found := codegenTreeOwner(s, s.index, "gen/tree/x.ts")
		if !found {
			return ""
		}
		return owner.Label.Name
	}
	generated := func(s *programStore) string { return fmt.Sprint(s.generatedFile("gen/value.ts", true)) }
	for _, tc := range []struct {
		name, build string
		answer      func(*programStore) string
		write       func(t *testing.T, c *config.Config, s *programStore)
		before      string
		after       string
	}{
		{"a recorded BUILD file", `genrule(name = "value", outs = ["value.ts"])`, generated, func(t *testing.T, c *config.Config, s *programStore) {
			f, err := rule.LoadData("gen/BUILD.bazel", "gen", []byte(`filegroup(name = "other")`))
			if err != nil {
				t.Fatal(err)
			}
			s.recordBuild(c, "gen", f)
		}, "true", "false"},
		{"a stored rule", `filegroup(name = "value")`, generated, func(t *testing.T, c *config.Config, s *programStore) {
			r := rule.NewRule("genrule", "value")
			r.SetAttr("outs", []string{"value.ts"})
			s.emission.setRule(emissionLabel(c.RepoName, "gen", ":value"), r)
		}, "false", "true"},
		{"a rule merged out of the list", "ts_codegen(name = \"a\", out_dir = \"tree\")\nts_codegen(name = \"b\", out_dir = \"tree\")", treeOwner,
			func(t *testing.T, c *config.Config, s *programStore) {
				f := s.emission.files["gen"]
				f.Rules = f.Rules[1:]
				s.ruleListsMayChange()
			}, "a", "b"},
		{"the rule index", "", treeOwner, func(t *testing.T, c *config.Config, s *programStore) {
			lang := &tsLang{}
			ix := resolve.NewRuleIndex(func(*rule.Rule, string) resolve.Resolver { return lang })
			r, f := newRule(indexedRule{kind: "ts_codegen", name: "indexed", pkg: "gen", outDir: "tree"})
			// Indexed through another store, whose callbacks move only its own counters.
			ix.AddRule(emptyConfig(), r, f)
			ix.Finish()
			s.index = ix
		}, "", "indexed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := emptyConfig()
			s := getConfig(c).programs
			f, err := rule.LoadData("gen/BUILD.bazel", "gen", []byte(tc.build))
			if err != nil {
				t.Fatal(err)
			}
			s.recordBuild(c, "gen", f)
			s.walked["gen"] = true
			if got := tc.answer(s); got != tc.before {
				t.Fatalf("before the write: %q, want %q", got, tc.before)
			}
			tc.write(t, c, s)
			if got := tc.answer(s); got != tc.after {
				t.Errorf("after %s: %q, want %q; a memoized answer outlived the write", tc.name, got, tc.after)
			}
		})
	}
}

// Answers the memo already holds must leave the same selection and import facts as computing them: a hit
// answers what the lookup would and skips no note the selection makes.
func TestWalkMemo_AWarmSelectionRecordsWhatAColdOneDoes(t *testing.T) {
	run := func(warm bool) *programStore {
		c := emptyConfig()
		root, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve typescript shared/data.json //shared:data\n"))
		if err != nil {
			t.Fatal(err)
		}
		(&resolve.Configurer{}).Configure(c, "", root)
		s := getConfig(c).programs
		for pkg, build := range map[string]string{
			"gen":    "genrule(name = \"g\", outs = [\"g.ts\"])\nts_codegen(name = \"tree\", out_dir = \"tree\")",
			"shared": `filegroup(name = "data", srcs = ["data.json"])`,
			"lib":    `ts_compile(name = "lib", srcs = ["index.ts"])`,
		} {
			f, err := rule.LoadData(pkg+"/BUILD.bazel", pkg, []byte(build))
			if err != nil {
				t.Fatal(err)
			}
			s.recordBuild(c, pkg, f)
			s.walked[pkg] = true
		}
		s.visit("shared", []string{"data.json"})
		s.visit("lib", []string{"index.ts", "util.ts"})
		index := "lib/index.ts"
		files := []string{index, "lib/util.ts", "gen/g.ts", "gen/tree/x.ts", "shared/data.json"}
		if warm {
			for _, f := range files {
				s.outputProducer(f)
				codegenTreeOwner(s, nil, f)
			}
		}
		s.importFacts = newImportFactsRecorder(nil)
		s.inputs["lib"] = programInput{config: s.inputs["lib"].config, program: &program{dir: "lib", key: "listing-lib", Listing: explainfiles.Listing{
			Roots: []string{index},
			Files: files,
			Edges: []explainfiles.Edge{importEdge(index, "./util", "lib/util.ts"), importEdge(index, "../gen/g", "gen/g.ts"),
				importEdge(index, "../gen/tree/x", "gen/tree/x.ts"), importEdge(index, "../shared/data.json", "shared/data.json")},
		}, candidates: []resolutionCandidate{
			{from: index, specifier: "../gen/g", path: "gen/g.ts", file: true, resolved: "gen/g.ts"},
			{from: index, specifier: "../gen/tree/x", path: "gen/tree/x.ts", file: true, resolved: "gen/tree/x.ts", block: 1},
			{from: index, specifier: "../shared/data.json", path: "shared/data.json", file: true, resolved: "shared/data.json", block: 2},
		}}}
		s.selectInput("lib")
		return s
	}
	cold, warm := run(false), run(true)
	// A cold selection hits its own answers too, so the authored files are spelled out.
	if want := []string{"lib/index.ts", "lib/util.ts", "shared/data.json"}; !slices.Equal(warm.programs["lib"].Files, want) {
		t.Errorf("lib's program files = %v, want %v without the generated gen/g.ts and gen/tree/x.ts", warm.programs["lib"].Files, want)
	}
	if len(cold.importFacts.looked) == 0 || len(sortedSelections(cold)) == 0 {
		t.Fatalf("the cold selection recorded no lookup or selection: %v %v", cold.importFacts.looked, sortedSelections(cold))
	}
	if !reflect.DeepEqual(sortedSelections(warm), sortedSelections(cold)) || !maps.Equal(warm.importFacts.looked, cold.importFacts.looked) ||
		!maps.Equal(warm.importFacts.programs, cold.importFacts.programs) {
		t.Errorf("warm selection recorded %v, looked up %v; the cold one records %v, %v",
			sortedSelections(warm), warm.importFacts.looked, sortedSelections(cold), cold.importFacts.looked)
	}
	if !reflect.DeepEqual(warm.packages, cold.packages) {
		t.Errorf("packages = %v, want the cold selection's %v", warm.packages, cold.packages)
	}
	if got, want := warm.programs["lib"], cold.programs["lib"]; !slices.Equal(got.Files, want.Files) || !slices.Equal(got.Edges, want.Edges) ||
		!slices.Equal(got.candidates, want.candidates) {
		t.Errorf("lib's program = %v %v, want the cold selection's %v %v", got.Files, got.Edges, want.Files, want.Edges)
	}
}
