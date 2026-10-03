package typescript

import (
	"os"
	"path"
	"slices"
	"strings"
	"testing"

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
