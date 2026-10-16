package typescript

import (
	"bytes"
	"encoding/gob"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/language"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// resolveGazelle runs with the run record off, so every run reaches Resolve.
func resolveGazelle(t *testing.T, root, cache string, args ...string) string {
	t.Helper()
	return recordGazelle(t, root, cache, append([]string{"-ts_run_record=off"}, args...)...)
}

func resolveReplayed(output string) bool { return strings.Contains(output, "resolve record: replayed") }

// primeResolveRecord runs until the tree is a fixed point and a run that
// started from it has recorded.
func primeResolveRecord(t *testing.T, root, cache string, args ...string) {
	t.Helper()
	resolveGazelle(t, root, cache, args...)
	time.Sleep(pastRacyWindow)
	resolveGazelle(t, root, cache, args...)
}

var depsAttr = regexp.MustCompile(`(?s)\n    deps = \[.*?\],`)

func editBuild(t *testing.T, root, rel string, edit func(string) string) {
	t.Helper()
	file := filepath.Join(root, rel)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	edited := edit(string(data))
	if edited == string(data) {
		t.Fatalf("the edit left %s as it was:\n%s", rel, data)
	}
	writeFile(t, file, edited)
}

// editDeps rewrites the first rule's deps.
func editDeps(build string, edit func(string) string) string {
	at := depsAttr.FindStringIndex(build)
	if at == nil {
		return build
	}
	return build[:at[0]] + edit(build[at[0]:at[1]]) + build[at[1]:]
}

func dropDeps(build string) string {
	return editDeps(build, func(string) string { return "" })
}

// requireAsCacheless runs a cache-less Gazelle on a copy of the tree as it is
// now and returns a check that the tree ends up with the same BUILD files.
func requireAsCacheless(t *testing.T, root, what string) func() {
	t.Helper()
	reference := copyTree(t, root)
	if out, err := tsGazelle(t, reference); err != nil {
		t.Fatalf("%s: cache-less reference run: %v\n%s", what, err, out)
	}
	return func() {
		t.Helper()
		if diff := snapshotDiff(buildFileBytes(t, reference), buildFileBytes(t, root)); diff != "" {
			t.Errorf("%s: BUILD files differ from a cache-less run: %s", what, diff)
		}
	}
}

func TestResolveRecord_ReplaysWhenOnlyResolvedAttributesWereEdited(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	primeResolveRecord(t, root, cache)
	for _, edit := range []struct {
		name string
		do   func(string) string
	}{
		{"deps deleted", dropDeps},
		{"a stale dep added", func(build string) string {
			return editDeps(build, func(deps string) string { return strings.Replace(deps, "[", `["//app/nowhere", `, 1) })
		}},
	} {
		editBuild(t, root, "app/sub/BUILD.bazel", edit.do)
		same := requireAsCacheless(t, root, edit.name)
		if out := resolveGazelle(t, root, cache); !resolveReplayed(out) {
			t.Errorf("%s: Resolve ran again although it starts from the recorded state:\n%s", edit.name, out)
		}
		same()
	}
}

func TestResolveRecord_ResolvesInFullWhenItsStartingStateChanged(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	write := func(rel, body string) func() {
		return func() { writeFile(t, filepath.Join(root, rel), body) }
	}
	build := func(edit func(string) string) func() {
		return func() { editBuild(t, root, "app/sub/BUILD.bazel", edit) }
	}
	for _, edit := range []struct {
		name string
		do   func()
	}{
		{"a body edit's neighbour import", write("app/a.ts", "import { c } from './c';\nexport const a = c;\n")},
		{"a new source", write("app/d.ts", "export const d = 4;\n")},
		{"kept deps", build(func(b string) string {
			return editDeps(b, func(deps string) string { return strings.Replace(deps, "[", `["//app/nowhere", `, 1) + "  # keep" })
		})},
		{"a new rule", build(func(b string) string { return b + "\nfilegroup(\n    name = \"extra\",\n    srcs = [\"s.ts\"],\n)\n" })},
		{"a directive", build(func(b string) string { return "# gazelle:resolve typescript nowhere //app\n" + b })},
		{"a tsconfig edit", write("app/tsconfig.json", `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["a.ts","b.ts"]}`)},
		{"a package.json edit", write("package.json", `{"type":"module","name":"resolve-record"}`)},
		{"a new directory with a source", write("tools/x/y.ts", "export const y = 1;\n")},
		{"a new directory of data files in a package", write("app/assets/logo.svg", "<svg/>\n")},
		{"a new directory with a tsconfig.json", write("lib/tsconfig.json", `{"include":["*.ts"]}`)},
		{"a new directory with a package.json", write("pkg/package.json", `{"name":"pkg"}`)},
		{"a new directory with a BUILD file", write("tools/b/BUILD.bazel", "")},
	} {
		primeResolveRecord(t, root, cache)
		edit.do()
		same := requireAsCacheless(t, root, edit.name)
		if out := resolveGazelle(t, root, cache); resolveReplayed(out) {
			t.Errorf("%s replayed the record of the state before it:\n%s", edit.name, out)
		}
		same()
	}
}

// A directory with no TypeScript, tsconfig.json, package.json or generated rule
// holds nothing Resolve reads but its names, which the record checks as read.
func TestResolveRecord_ReplaysWhenOnlyADirectoryWithoutTypeScriptChanged(t *testing.T) {
	requireTsgo(t)
	tree := runRecordTree()
	tree["svc/BUILD.bazel"] = "sh_library(\n    name = \"svc\",\n    srcs = [\"run.sh\"],\n)\n"
	tree["svc/run.sh"] = "true\n"
	root := writeTree(t, tree)
	cache := t.TempDir()
	for _, edit := range []struct{ name, rel string }{
		{"a new directory", "tools/gen/main.go"},
		{"a new directory below it", "tools/gen/sub/data.txt"},
		{"a new file beside a BUILD file", "svc/extra.sh"},
	} {
		primeResolveRecord(t, root, cache)
		writeFile(t, filepath.Join(root, edit.rel), "package main\n")
		same := requireAsCacheless(t, root, edit.name)
		if out := resolveGazelle(t, root, cache); !resolveReplayed(out) {
			t.Errorf("%s: Resolve ran again although nothing it reads changed:\n%s", edit.name, out)
		}
		same()
	}
}

// The digest leaves those directories' names out only because every read of
// them during the phase is noted; an unnoted read would replay a stale phase.
func TestResolveRecord_AReadOfADirectoryTheDigestLeavesOutMustHold(t *testing.T) {
	s := newProgramStore()
	s.files["app"], s.visited["app"], s.walked["app"] = []string{"a.ts"}, []string{"app/a.ts"}, true
	s.files["tools"], s.walked["tools"] = []string{"x.json"}, true
	rec := &resolveRecorder{outside: s.outsideDirs(nil), outsideRead: map[string]string{}, treeRead: map[string]string{}}
	rec.sorted = slices.Sorted(maps.Keys(rec.outside))
	resolvePhase.Store(rec)
	t.Cleanup(func() { resolvePhase.Store(nil) })
	s.sourceFile("app/a.ts")
	s.sourceFile("tools/x.json")
	s.sourceFile("gone/y.json")
	s.walkedDirs("tools")
	resolvePhase.Store(nil)
	if got := slices.Sorted(maps.Keys(rec.outsideRead)); !slices.Equal(got, []string{"gone", "tools"}) {
		t.Fatalf("noted reads of %q, want the two directories the digest leaves out", got)
	}
	r := &resolveRecord{Outside: sortedReads(rec.outsideRead), Trees: sortedReads(rec.treeRead)}
	holds := func() bool {
		outside := s.outsideDirs(nil)
		return r.outsideHolds(s, slices.Sorted(maps.Keys(outside)))
	}
	if !holds() {
		t.Fatal("the reads do not hold for the state they were noted in")
	}
	for _, c := range []struct {
		name         string
		edit, revert func()
	}{
		{"a read directory gained a file", func() { s.files["tools"] = []string{"x.json", "y.json"} }, func() { s.files["tools"] = []string{"x.json"} }},
		{"a read absent directory appeared", func() { s.files["gone"] = []string{"y.json"} }, func() { delete(s.files, "gone") }},
		{"an enumerated tree gained a directory", func() { s.files["tools/sub"] = []string{"z.txt"} }, func() { delete(s.files, "tools/sub") }},
	} {
		c.edit()
		if holds() {
			t.Errorf("%s, but the noted reads still hold", c.name)
		}
		c.revert()
	}
}

// Map order and the digest's own parallelism differ between processes; a
// record written by one must hold for the next.
func TestResolveRecord_StateDigestIsTheSameInEveryProcess(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	primeResolveRecord(t, root, cache)
	before := buildFileBytes(t, root)
	for _, procs := range []string{"1", "4", "1", "16", "2"} {
		t.Setenv("GOMAXPROCS", procs)
		if out := resolveGazelle(t, root, cache); !resolveReplayed(out) {
			t.Fatalf("GOMAXPROCS=%s: an unchanged tree did not replay:\n%s", procs, out)
		}
		if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
			t.Fatalf("GOMAXPROCS=%s: a replay changed BUILD files: %s", procs, diff)
		}
	}
}

func TestResolveRecord_VerifyPassesAnHonestRecord(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	primeResolveRecord(t, root, cache)
	editBuild(t, root, "app/sub/BUILD.bazel", dropDeps)
	same := requireAsCacheless(t, root, "verify")
	out := resolveGazelle(t, root, cache, "-ts_resolve_record=verify")
	if !strings.Contains(out, "resolve record: verified") {
		t.Errorf("verify did not check the record against a full pass:\n%s", out)
	}
	same()
	if out := resolveGazelle(t, root, cache, "-ts_resolve_record=off"); resolveReplayed(out) {
		t.Errorf("-ts_resolve_record=off replayed:\n%s", out)
	}
}

// A forged record stands for any blind spot in the state digest; verify must catch it.
func TestResolveRecord_VerifyFailsOnAForgedRecord(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	primeResolveRecord(t, root, cache)
	records, _ := filepath.Glob(filepath.Join(cache, resolveRecordName+"-*"))
	if len(records) != 1 {
		t.Fatalf("want one resolve record, have %q", records)
	}
	rec := loadResolveRecord(records[0])
	if rec == nil {
		t.Fatal("the resolve record does not load")
	}
	i := slices.IndexFunc(rec.Files, func(f recordedFile) bool { return f.Pkg == "app/sub" })
	if i < 0 {
		t.Fatalf("the record publishes no app/sub: %v", rec.Files)
	}
	rec.Files[i].Content = []byte(dropDeps(string(rec.Files[i].Content)))
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(records[0], b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_verbose", "-ts_run_record=off", "-ts_resolve_record=verify")
	if err == nil || !strings.Contains(out, "resolve record: a full pass with the record off would write app/sub/BUILD.bazel otherwise than the replay") {
		t.Fatalf("verify passed a record a full pass contradicts (%v):\n%s", err, out)
	}
	if out := resolveGazelle(t, root, cache); resolveReplayed(out) {
		t.Errorf("the record survived a failed verify:\n%s", out)
	}
}

// With both records, a BUILD-only edit misses the run record and replays Resolve.
func TestResolveRecord_ReplaysBehindARunRecordMiss(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	recordGazelle(t, root, cache)
	primeRunRecord(t, root, cache)
	editBuild(t, root, "app/sub/BUILD.bazel", dropDeps)
	same := requireAsCacheless(t, root, "deps deleted")
	// The fast path would write the recorded file back before any walk.
	out := recordGazelle(t, root, cache, "-ts_fastpath=off")
	if replayed(out) || !resolveReplayed(out) {
		t.Errorf("a BUILD-only edit did not miss the run record and replay Resolve:\n%s", out)
	}
	same()
}

// The digest names every field of the state it covers, or says why not; a
// field added to one of these types must be put on one side.
func TestResolveRecord_StateDigestClassifiesEveryField(t *testing.T) {
	for _, c := range []struct {
		typ            reflect.Type
		digested, left []string
	}{
		{reflect.TypeFor[programStore](), []string{"repoConfig", "emission", "inputs", "completePrograms", "index", "tsgoFlag", "verbose", "tsgo", "skipped",
			"programs", "configs", "packages", "generatedPackages", "visited", "files", "walked", "bases", "extended", "foreign",
			"foreignSaid", "vitestPrograms", "compilerPrograms", "wranglerConfigs", "installChecked", "regularFiles", "fullWalk",
			"manifests", "noLockSaid"}, []string{"memo", "protoIndex", "prefetch", "resolution", "importFacts", "buildFacts", "early"}},
		{reflect.TypeFor[emissionGraph](), []string{"roots", "outputs", "rules", "files", "lock"},
			[]string{"versions", "epoch", "listEpoch", "outputsGeneration", "rulesGeneration", "outputIndex", "registered", "crawled", "crawledPkgs", "lastTouched"}},
		{reflect.TypeFor[program](), []string{"Listing", "candidates", "resolution", "config", "dir", "refused", "manifest", "key"}, []string{"bySource", "byImporter", "moduleEdges", "byDir"}},
		{reflect.TypeFor[resolutionFacts](), []string{"Kinds", "RelativeTargets", "RootDirs", "Substitutions", "SubstitutionTargets"}, nil},
		{reflect.TypeFor[programInput](), []string{"config", "program", "discoveryErr", "refresh"}, nil},
		{reflect.TypeFor[tsConfig](), []string{"protos", "protoIdentities", "protoEnabled", "foreignManifest", "updateOnly", "programs", "lock", "lockLoaded"}, nil},
		{reflect.TypeFor[protoStore](), []string{"graphPath", "graph", "roots", "recursive", "identities", "observations", "runtimeImports"}, nil},
		{reflect.TypeFor[protoIdentity](), []string{"Name", "OutDir", "Tsconfig", "NodeModules", "Deps", "Options", "owner"}, nil},
		{reflect.TypeFor[protoObservation](), []string{"nativeDeps", "config", "native", "identity", "paths", "imports"}, nil},
		{reflect.TypeFor[protoRuleImports](), []string{"nativeDeps", "config", "identity", "imports"}, nil},
		{reflect.TypeFor[protoGraph](), []string{"Nodes", "Roots"}, []string{"nodes", "sources", "labels"}},
		{reflect.TypeFor[protoGraphNode](), []string{"Label", "Sources", "Deps"}, nil},
		{reflect.TypeFor[npmLock](), []string{"names", "importers", "members"}, []string{"memberLabels"}},
		{reflect.TypeFor[pnpmImporter](), []string{"deps", "links"}, nil},
		{reflect.TypeFor[manifest](), []string{"dir", "name", "deps"}, nil},
		{reflect.TypeFor[ruleImports](), []string{"program", "candidates", "sources", "edges", "config", "deps", "srcDrops"}, nil},
		{reflect.TypeFor[srcDrops](), []string{"args", "have", "dropped"}, nil},
		{reflect.TypeFor[resolutionCandidate](), []string{"from", "specifier", "path", "file", "metadata", "kind", "block", "resolved", "skipped"}, nil},
		{reflect.TypeFor[cachedListing](), []string{"Listing", "Candidates", "Resolution"},
			[]string{"Reads", "Configs", "PackageExtends", "Dirs", "Fingerprint", "Root", "ReadLines", "DirDigests", "Sources", "PatchVersion", "Config", "Unmatched", "FilesList", "StaleDiagnostics"}},
		{reflect.TypeFor[cachedCandidate](), []string{"From", "Specifier", "Path", "File", "Metadata", "Kind", "Block", "Resolved", "Skipped"}, nil},
		{reflect.TypeFor[explainfiles.Listing](), []string{"Files", "Roots", "Edges", "Types", "Implicit", "Diagnostics", "Unresolved", "Resolutions", "Candidates", "PackageTargets"}, nil},
		{reflect.TypeFor[explainfiles.Edge](), []string{"Kind", "From", "To", "Specifier"}, nil},
		{reflect.TypeFor[explainfiles.TypeEntry](), []string{"Entry", "File"}, nil},
		{reflect.TypeFor[explainfiles.Unresolved](), []string{"Kind", "From", "Specifier", "Candidates"}, nil},
		{reflect.TypeFor[explainfiles.FailedLookup](), []string{"File", "Candidate"}, nil},
		{reflect.TypeFor[explainfiles.Resolution](), []string{"Edge", "Candidates", "Substitutions", "FromPackage"}, nil},
		{reflect.TypeFor[explainfiles.ResolutionCandidate](), []string{"Kind", "From", "Specifier", "Path"}, nil},
		{reflect.TypeFor[config.Config](), []string{"WorkDir", "RepoRoot", "RepoName", "ReadBuildFilesDir", "WriteBuildFilesDir", "ValidBuildFileNames",
			"ShouldFix", "Strict", "IndexLibraries", "IndexLazy", "KindMap", "AliasMap", "Repos", "Langs", "Exts", "Bzlmod", "ModuleToApparentName"}, nil},
		{reflect.TypeFor[language.GenerateArgs](), []string{"Config", "Dir", "Rel", "File", "Subdirs", "RegularFiles", "GenFiles", "OtherEmpty", "OtherGen"}, nil},
		{reflect.TypeFor[language.GenerateResult](), []string{"Gen", "Empty", "Imports", "RelsToIndex"}, nil},
	} {
		var have []string
		for field := range c.typ.Fields() {
			have = append(have, field.Name)
		}
		want := append(slices.Clone(c.digested), c.left...)
		slices.Sort(have)
		slices.Sort(want)
		if !slices.Equal(have, want) {
			t.Errorf("%s has fields %v; the state digest classifies %v", c.typ, have, want)
		}
	}
}

// A record is untrusted input: whatever the cache directory holds must load as
// a record or as none, never panic.
func TestResolveRecord_ACorruptRecordLoadsAsNone(t *testing.T) {
	var valid bytes.Buffer
	if err := gob.NewEncoder(&valid).Encode(resolveRecord{Magic: resolveRecordMagic, Files: []recordedFile{{Pkg: "app", Content: []byte("x")}}}); err != nil {
		t.Fatal(err)
	}
	var otherMagic bytes.Buffer
	if err := gob.NewEncoder(&otherMagic).Encode(resolveRecord{Magic: "another tool's record"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"not gob", []byte("BUILD.bazel\n")},
		{"truncated", valid.Bytes()[:valid.Len()/2]},
		{"another magic", otherMagic.Bytes()},
	} {
		file := filepath.Join(t.TempDir(), resolveRecordName)
		if err := os.WriteFile(file, c.data, 0o644); err != nil {
			t.Fatal(err)
		}
		if rec := loadResolveRecord(file); rec != nil {
			t.Errorf("%s: loaded as %+v", c.name, rec)
		}
	}
}

// A listing writes its projection beside the tsconfig.json and removes it;
// noted as an input, it read as changed and no cold run kept a record.
func TestResolveRecord_AProjectionIsNeverAnInput(t *testing.T) {
	saved := runInputs
	t.Cleanup(func() { runInputs = saved })
	runInputs = &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}}
	dir := t.TempDir()
	base, projection := filepath.Join(dir, "tsconfig.json"), filepath.Join(dir, ".gazelle-tsconfig-1.json")
	writeFile(t, base, "{}")
	writeFile(t, projection, `{"extends":"./tsconfig.json"}`)
	readInput(base)
	readInput(projection)
	if _, ok := runInputs.files[base]; !ok {
		t.Errorf("the base config read is not an input: %v", runInputs.files)
	}
	if _, ok := runInputs.files[projection]; ok {
		t.Errorf("the projection read is an input: %v", runInputs.files)
	}
}
