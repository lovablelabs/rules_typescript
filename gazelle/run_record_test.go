package typescript

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/mikn/rules_typescript/gazelle/incremental"
)

func runRecordTree() map[string]string {
	return map[string]string{
		"MODULE.bazel":                 "module(name = \"run_record\")\n",
		".bazelignore":                 "node_modules\n",
		"node_modules/.pnpm/lock.yaml": "lockfileVersion: '9.0'\n",
		"package.json":                 `{"type":"module"}`,
		"app/tsconfig.json":            `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["*.ts"]}`,
		"app/a.ts":                     "import { b } from './b';\nexport const a = b;\n",
		"app/b.ts":                     "export const b = 1;\n",
		"app/c.ts":                     "export const c = 2;\n",
		"app/sub/tsconfig.json":        `{"extends":"../tsconfig.json","include":["*.ts"]}`,
		"app/sub/s.ts":                 "import { a } from '../a';\nexport const s = a;\n",
	}
}

// tsGazelle runs the TypeScript-only binary, the one a record replays for.
func tsGazelle(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	binary, err := runfiles.Rlocation(os.Getenv("TS_GAZELLE"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, append([]string{"-repo_root=" + root}, args...)...)
	command.Dir = root
	command.Env = append(os.Environ(), "BUILD_WORKSPACE_DIRECTORY="+root)
	b, err := command.CombinedOutput()
	return string(b), err
}

// -ts_verbose is what says a run replayed.
func recordGazelle(t *testing.T, root, cache string, args ...string) string {
	t.Helper()
	output, err := tsGazelle(t, root, append([]string{"-ts_listing_cache=" + cache, "-ts_verbose"}, args...)...)
	if err != nil {
		t.Fatalf("gazelle %q: %v\n%s", args, err, output)
	}
	return output
}

func replayed(output string) bool { return strings.Contains(output, "run record: replayed") }

func copyTree(t *testing.T, root string) string {
	t.Helper()
	dst := t.TempDir()
	if err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(file)
			if err == nil {
				err = os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o755)
			}
			if err == nil {
				err = os.Symlink(link, filepath.Join(dst, rel))
			}
			return err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		writeFile(t, filepath.Join(dst, rel), string(data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return dst
}

// A candidate whose inputs were stamped within racyWindow of its run's start
// is not kept, so the fixture first ages past it.
const pastRacyWindow = racyWindow + 100*time.Millisecond

// primeRunRecord runs until a run that changed nothing has recorded, and
// proves the next one replays.
func primeRunRecord(t *testing.T, root, cache string) {
	t.Helper()
	time.Sleep(pastRacyWindow)
	recordGazelle(t, root, cache)
	if out := recordGazelle(t, root, cache); !replayed(out) {
		t.Fatalf("a rerun of an unchanged tree did not replay its record:\n%s", out)
	}
}

func TestRunRecord_ReplaysOnlyWhenEveryInputIsUnchanged(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	recordGazelle(t, root, cache)
	primeRunRecord(t, root, cache)
	write := func(rel, body string) func() {
		return func() { writeFile(t, filepath.Join(root, rel), body) }
	}
	for _, edit := range []struct {
		name string
		do   func()
	}{
		{"a body edit", write("app/a.ts", "import { b } from './b';\nexport const a = b + 1;\n")},
		{"an import edit", write("app/a.ts", "import { c } from './c';\nexport const a = c;\n")},
		{"a new source", write("app/d.ts", "export const d = 4;\n")},
		{"a removed source", func() {
			if err := os.Remove(filepath.Join(root, "app/d.ts")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a BUILD edit", write("app/BUILD.bazel", "")},
		{"a tsconfig edit", write("app/tsconfig.json", `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["a.ts","c.ts"]}`)},
		{"a package.json edit", write("package.json", `{"type":"module","name":"run-record"}`)},
		{"an install", write("node_modules/.pnpm/lock.yaml", "lockfileVersion: '9.0'\nimporters: {}\n")},
	} {
		edit.do()
		reference := copyTree(t, root)
		if out, err := tsGazelle(t, reference); err != nil {
			t.Fatalf("%s: cache-less reference run: %v\n%s", edit.name, err, out)
		}
		if out := recordGazelle(t, root, cache); replayed(out) {
			t.Errorf("%s replayed the record of the tree before it", edit.name)
		}
		if diff := snapshotDiff(buildFileBytes(t, reference), buildFileBytes(t, root)); diff != "" {
			t.Errorf("%s: BUILD files differ from a cache-less run: %s", edit.name, diff)
		}
		primeRunRecord(t, root, cache)
	}
	if out := recordGazelle(t, root, cache, "-strict"); replayed(out) {
		t.Errorf("other arguments replayed the record:\n%s", out)
	}
	if out := recordGazelle(t, root, cache, "-mode=print"); replayed(out) || !strings.Contains(out, "ts_compile(") {
		t.Errorf("-mode=print did not print the BUILD files:\n%s", out)
	}
}

func TestRunRecord_ARunThatWroteABuildFileIsNeverRecorded(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	recordGazelle(t, root, cache)
	primeRunRecord(t, root, cache)
	build := filepath.Join(root, "app/sub/BUILD.bazel")
	before, err := os.ReadFile(build)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "app/sub/s.ts"), "export const s = 1;\n")
	recordGazelle(t, root, cache)
	if after, _ := os.ReadFile(build); string(after) == string(before) {
		t.Fatal("dropping the import left app/sub/BUILD.bazel unchanged")
	}
	// Back to the bytes the run saw: a record taken from that run would now match.
	writeFile(t, build, string(before))
	reference := copyTree(t, root)
	if out, err := tsGazelle(t, reference); err != nil {
		t.Fatalf("cache-less reference run: %v\n%s", err, out)
	}
	if out := recordGazelle(t, root, cache); replayed(out) {
		t.Errorf("replayed the record of a run that wrote a BUILD file:\n%s", out)
	}
	if diff := snapshotDiff(buildFileBytes(t, reference), buildFileBytes(t, root)); diff != "" {
		t.Errorf("BUILD files differ from a cache-less run: %s", diff)
	}
	writeFile(t, build, string(before))
	for range 2 {
		if out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_verbose", "-mode=diff"); err == nil || replayed(out) {
			t.Fatalf("-mode=diff with a difference passed (%v):\n%s", err, out)
		}
	}
}

func TestRunRecord_VerifyPassesAnHonestReplay(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	recordGazelle(t, root, cache)
	primeRunRecord(t, root, cache)
	before := buildFileBytes(t, root)
	out := recordGazelle(t, root, cache, "-ts_run_record=verify")
	if !strings.Contains(out, "run record: verified") {
		t.Errorf("verify did not report a verified replay:\n%s", out)
	}
	if diff := snapshotDiff(before, buildFileBytes(t, root)); diff != "" {
		t.Errorf("verify changed BUILD files: %s", diff)
	}
	if out := recordGazelle(t, root, cache, "-ts_run_record=off"); replayed(out) {
		t.Errorf("-ts_run_record=off replayed:\n%s", out)
	}
}

func TestRunRecord_AReplayWritesTheRecordedRunsLog(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	recordGazelle(t, root, cache)
	time.Sleep(pastRacyWindow)
	full := recordGazelle(t, root, cache)
	replay := recordGazelle(t, root, cache)
	if !replayed(replay) {
		t.Fatalf("the rerun did not replay:\n%s", replay)
	}
	if !strings.HasPrefix(replay, full) || strings.Count(strings.TrimPrefix(replay, full), "\n") != 1 {
		t.Errorf("a replay's log is not the recorded run's log and one line saying so:\n--- recorded run\n%s--- replay\n%s", full, replay)
	}
}

func TestRunRecord_VerifyFailsWhenAFullPassLogsOtherwise(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	recordGazelle(t, root, cache)
	primeRunRecord(t, root, cache)
	records, _ := filepath.Glob(filepath.Join(cache, "run-record*"))
	if len(records) != 1 {
		t.Fatalf("want one record, have %q", records)
	}
	data, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatal(err)
	}
	// Change one byte of the log's last line naming a config; the facts after it name configs too.
	at := bytes.LastIndex(data, []byte("tsconfig.json: "))
	if at < 0 {
		t.Fatalf("the record holds no log naming a config:\n%s", data)
	}
	data[at+len("tsconfig.json")-1] = 'N'
	if err := os.WriteFile(records[0], data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_verbose", "-ts_run_record=verify")
	if err == nil || !strings.Contains(out, "run record: a full pass without caches") {
		t.Fatalf("verify passed a replay whose log a full pass contradicts (%v):\n%s", err, out)
	}
	if out := recordGazelle(t, root, cache); replayed(out) {
		t.Errorf("the record survived a failed verify:\n%s", out)
	}
}

// A record forged for a tree a full pass would change stands for any
// fingerprint blind spot; verify must catch it.
func TestRunRecord_VerifyFailsWhenAFullPassWouldWrite(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	recordGazelle(t, root, cache)
	primeRunRecord(t, root, cache)
	writeFile(t, filepath.Join(root, "app/sub/BUILD.bazel"), "")
	records, _ := filepath.Glob(filepath.Join(cache, "run-record*"))
	if len(records) != 1 {
		t.Fatalf("want one record, have %q", records)
	}
	rec := loadRunRecord(records[0])
	buildNames := []string{"BUILD.bazel", "BUILD"}
	walk, err := rec.read(buildNames, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec.digest, rec.walkDigest = rec.digests(walk.inputs)
	data, err := rec.encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(records[0], data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_verbose", "-ts_run_record=verify")
	if err == nil || !strings.Contains(out, "run record: a full pass without caches differs") {
		t.Fatalf("verify passed a replay that a full pass contradicts (%v):\n%s", err, out)
	}
	if out := recordGazelle(t, root, cache); replayed(out) {
		t.Errorf("the record survived a failed verify:\n%s", out)
	}
}

func TestRunRecord_AnInputStampedJustBeforeTheRunIsNotKept(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	recordGazelle(t, root, cache)
	primeRunRecord(t, root, cache)
	// Same bytes, rewritten: a later same-size write within one timestamp tick
	// would leave this stat as it is.
	writeFile(t, filepath.Join(root, "app/b.ts"), "export const b = 1;\n")
	recordGazelle(t, root, cache)
	if out := recordGazelle(t, root, cache); replayed(out) {
		t.Errorf("kept a record whose input was stamped just before its run:\n%s", out)
	}
	primeRunRecord(t, root, cache)
}

// The record covers what the TypeScript language reads; another language's
// inputs are not in it, so a binary carrying one always runs in full.
func TestRunRecord_ABinaryWithAnotherLanguageNeverReplays(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, runRecordTree())
	cache := t.TempDir()
	for i := range 3 {
		if i == 1 {
			time.Sleep(pastRacyWindow)
		}
		out, err := protoGazelle(t, root, "-ts_listing_cache="+cache, "-ts_verbose")
		if err != nil {
			t.Fatalf("gazelle: %v\n%s", err, out)
		}
		if replayed(out) {
			t.Fatalf("run %d of a binary with the proto language replayed:\n%s", i+1, out)
		}
	}
}

// Each edit keeps every mtime the run could see from before it started, which
// an mtime cut-off would let through.
func TestRunRecord_AnInputThatMovedWhileTheRunReadItIsNotKept(t *testing.T) {
	saved := runInputs
	t.Cleanup(func() { runInputs, loadedListings = saved, sync.Map{} })
	loadedListings = sync.Map{}
	old := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name string
		edit func(root, stash string)
	}{
		{"none", func(string, string) {}},
		{"an old file moved into a walked directory", func(root, stash string) {
			rename(t, filepath.Join(stash, "x.ts"), filepath.Join(root, "app/x.ts"))
		}},
		{"a read file replaced by an old one", func(root, stash string) {
			rename(t, filepath.Join(stash, "x.ts"), filepath.Join(root, "lib/l.ts"))
		}},
		{"a read file removed", func(root, _ string) {
			if err := os.Remove(filepath.Join(root, "lib/l.ts")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a read link retargeted", func(root, stash string) {
			link := filepath.Join(root, "lib/link.ts")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(stash, "x.ts"), link); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		root, stash := t.TempDir(), t.TempDir()
		for _, file := range []string{"app/a.ts", "app/BUILD.bazel", "lib/l.ts", "lib/m.ts"} {
			writeFile(t, filepath.Join(root, file), "export {};\n")
		}
		writeFile(t, filepath.Join(stash, "x.ts"), "export const x = 1;\n")
		if err := os.Symlink(filepath.Join(root, "lib/m.ts"), filepath.Join(root, "lib/link.ts")); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"app/a.ts", "lib/l.ts", "lib/m.ts"} {
			if err := os.Chtimes(filepath.Join(root, file), old, old); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(filepath.Join(stash, "x.ts"), old, old); err != nil {
			t.Fatal(err)
		}
		in := &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}, key: "k", repoRoot: root, buildNames: []string{"BUILD.bazel"}}
		runInputs = in
		app := filepath.Join(root, "app")
		line, _, err := dirLine(app, in.buildNames, "")
		if err != nil {
			t.Fatal(err)
		}
		in.dirs["app"] = &dirSnapshot{abs: app, line: line}
		noteRead(filepath.Join(root, "lib/l.ts"))
		noteRead(filepath.Join(root, "lib/link.ts"))
		tc.edit(root, stash)
		rec, why := in.candidate()
		if why != "" {
			t.Fatalf("%s: no candidate: %s", tc.name, why)
		}
		if moved := tc.name != "none"; moved == rec.matches(in.buildNames, "", nil) {
			t.Errorf("%s: the candidate taken before the edit matches the tree after it: %t", tc.name, !moved)
		}
	}
}

func rename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

// Gazelle 2 lets a language configure only in Configure, after recordableRun
// read c.Exts; the run must still refuse to record its unrecorded inputs.
func TestRunRecord_ALanguageConfiguringAfterCheckFlagsIsNeverRecorded(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel":      "module(name = \"late_language\")\n",
		"app/tsconfig.json": includeTs,
		"app/a.ts":          "export const a = 1;\n",
	})
	saved := runInputs
	t.Cleanup(func() { runInputs = saved })
	in := &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}, key: "k", repoRoot: root}
	runInputs = in
	t.Cleanup(in.pending.Wait)
	generateAll(t, root, func(c *config.Config) {
		c.Exts["late"] = struct{}{}
	})
	if !strings.Contains(in.incomplete, "another language configures late") {
		t.Errorf("a run with another language's config is recordable; incomplete = %q", in.incomplete)
	}
}

// The walk's listing stands in for the stat of each input it holds. Each edit
// must move every input's line and the newest stamp as its own stat would,
// and the record must stop matching.
func TestRunRecord_InputsInAWalkedDirectoryReadAsTheirOwnStat(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name string
		edit func(root string)
	}{
		{"none", func(string) {}},
		{"a read file rewritten", func(root string) { writeFile(t, filepath.Join(root, "app/a.ts"), "export const a = 22;\n") }},
		{"a file added where a read probed", func(root string) { writeFile(t, filepath.Join(root, "app/x.ts"), "export {};\n") }},
		{"a read file rewritten to its size and mtime", func(root string) {
			writeFile(t, filepath.Join(root, "app/a.ts"), "export const a = 2;\n")
			if err := os.Chtimes(filepath.Join(root, "app/a.ts"), old, old); err != nil {
				t.Fatal(err)
			}
		}},
		{"a read outside the walk rewritten", func(root string) { writeFile(t, filepath.Join(root, "lib/l.ts"), "export const l = 22;\n") }},
		{"a name added under a names root", func(root string) { writeFile(t, filepath.Join(root, "lib/m.ts"), "export {};\n") }},
	} {
		root := writeTree(t, map[string]string{
			"app/a.ts":                     "export const a = 1;\n",
			"app/BUILD.bazel":              "",
			"app/Upper.ts":                 "export {};\n",
			"intl/\u00e9.ts":               "export {};\n",
			"app/sub/s.ts":                 "export {};\n",
			"app/.gazelle-tsconfig-1.json": "{}",
			"app/cache/entry":              "",
			"app/node_modules/p/i.d.ts":    "export {};\n",
			"lib/l.ts":                     "export const l = 1;\n",
		})
		if err := os.Symlink(filepath.Join(root, "lib/l.ts"), filepath.Join(root, "app/link.ts")); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"app/a.ts", "lib/l.ts"} {
			if err := os.Chtimes(filepath.Join(root, file), old, old); err != nil {
				t.Fatal(err)
			}
		}
		// The newest input is one the walk holds.
		time.Sleep(50 * time.Millisecond)
		writeFile(t, filepath.Join(root, "app/BUILD.bazel"), "")
		abs := func(rels ...string) []string {
			out := make([]string, len(rels))
			for i, rel := range rels {
				out[i] = filepath.Join(root, rel)
			}
			return out
		}
		buildNames, skip := []string{"BUILD.bazel"}, filepath.Join(root, "app/cache")
		inputs := abs("app/a.ts", "app/BUILD.bazel", "app/sub", "app/link.ts", "app/x.ts", "app/upper.ts", "app/.gazelle-tsconfig-1.json",
			"app/cache", "app/node_modules/p/i.d.ts", "gone/g.ts", "intl/e\u0301.ts", "lib/l.ts")
		rec := &runRecord{dirs: abs("app", "app/node_modules/p", "gone", "intl"), files: inputs, reads: inputs, roots: abs("app/sub", "lib")}
		byStat := func(stamps *newest) inputLines {
			unwalked, err := (&runRecord{files: rec.files, reads: rec.reads, roots: rec.roots}).read(buildNames, skip, stamps)
			if err != nil {
				t.Fatal(err)
			}
			lines := unwalked.inputs
			if lines.dirs, err = dirLines(rec.dirs, buildNames, skip, stamps); err != nil {
				t.Fatal(err)
			}
			return lines
		}
		rec.digest, rec.walkDigest = rec.digests(byStat(nil))
		// Past one tick of the file system's coarse ctime clock; a record's racy window covers the tick itself.
		time.Sleep(50 * time.Millisecond)
		tc.edit(root)
		own, viaWalk := &newest{}, &newest{}
		want := byStat(own)
		walk, err := rec.read(buildNames, skip, viaWalk)
		if err != nil {
			t.Fatal(err)
		}
		for kind, got := range map[string][2][]string{"file": {want.files, walk.inputs.files}, "read": {want.reads, walk.inputs.reads}, "root": {want.roots, walk.inputs.roots}} {
			for i := range got[0] {
				if got[0][i] != got[1][i] {
					t.Errorf("%s: %s %d reads %q through the walk, %q by its own stat", tc.name, kind, i, got[1][i], got[0][i])
				}
			}
		}
		if !own.at.Equal(viaWalk.at) {
			t.Errorf("%s: the newest input is stamped %v through the walk, %v by its own stat", tc.name, viaWalk.at, own.at)
		}
		for rel, want := range map[string]bool{"app/a.ts": true, "app/x.ts": true, "app/sub": true, "gone/g.ts": true, "app/link.ts": false,
			"app/upper.ts": false, "intl/e\u0301.ts": false, "app/\u212a.ts": false, "intl/x.ts": false, "app/.gazelle-tsconfig-1.json": false, "app/.GAZELLE-TSCONFIG-1.json": false, "app/cache": false, "app/Cache": false} {
			if _, ok := walk.entry(filepath.Join(root, rel)); ok != want {
				t.Errorf("%s: %s takes its line from the walk: %t; want %t", tc.name, rel, ok, want)
			}
		}
		if _, ok := walk.entry(root + "/app/link.ts/../a.ts"); ok {
			t.Errorf("%s: a path through a link takes its line from the walk", tc.name)
		}
		if moved := tc.name != "none"; moved == rec.matches(buildNames, skip, nil) {
			t.Errorf("%s: the record taken before the edit matches the tree after it: %t", tc.name, !moved)
		}
	}
}

// The pass reads every input of every list, inside and outside the walk, as
// its own stat reads it: an input it skipped reads the same before and after
// its edit, and a file or read it left unstamped falls out of the racy-clean rule.
func TestRunRecord_TheReadPassSeesEveryInputModifiedOrNew(t *testing.T) {
	const n = 64
	tree := map[string]string{}
	rec := &runRecord{}
	root := t.TempDir()
	edits := map[string]string{}
	for i := range n {
		for _, dir := range []string{fmt.Sprintf("walked/d%02d", i), fmt.Sprintf("outside/d%02d", i)} {
			tree[dir+"/file.ts"], tree[dir+"/read.ts"] = "export {};\n", "export const r = 1;\n"
			abs := filepath.Join(root, dir)
			rec.files = append(rec.files, filepath.Join(abs, "file.ts"), filepath.Join(abs, "new-file.ts"))
			rec.reads = append(rec.reads, filepath.Join(abs, "read.ts"), filepath.Join(abs, "new-read.ts"))
			edits[dir+"/file.ts"], edits[dir+"/new-file.ts"] = "export const f = 2;\n", "export const f = 2;\n"
			edits[dir+"/read.ts"], edits[dir+"/new-read.ts"] = "export const read = 2;\n", "export const read = 2;\n"
		}
		rec.dirs = append(rec.dirs, filepath.Join(root, fmt.Sprintf("walked/d%02d", i)))
		tree[fmt.Sprintf("roots/r%02d/a%02d.ts", i, i)] = "export {};\n"
		rec.roots = append(rec.roots, filepath.Join(root, fmt.Sprintf("roots/r%02d", i)))
		edits[fmt.Sprintf("roots/r%02d/b.ts", i)] = ""
	}
	writeWorkspace(t, root, tree)
	before, err := rec.read(nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, root, edits)
	after, err := rec.read(nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var own inputLines
	stamped := map[string]time.Time{}
	for _, f := range rec.files {
		line, stamp := fileLine(f)
		own.files = append(own.files, line)
		stamped["file"] = later(stamped["file"], stamp)
	}
	for _, r := range rec.reads {
		_, stamp := fileLine(r)
		own.reads = append(own.reads, incremental.EntryLine(r))
		stamped["read"] = later(stamped["read"], stamp)
	}
	own.roots = rootLines(rec.roots)
	for kind, lists := range map[string][3][]string{
		"file": {before.inputs.files, after.inputs.files, own.files},
		"read": {before.inputs.reads, after.inputs.reads, own.reads},
		"root": {before.inputs.roots, after.inputs.roots, own.roots},
	} {
		for i := range lists[0] {
			if lists[0][i] == lists[1][i] || lists[1][i] != lists[2][i] {
				t.Errorf("%s %d reads %q before its edit, %q after it, and %q by its own stat", kind, i, lists[0][i], lists[1][i], lists[2][i])
			}
		}
	}
	for kind, only := range map[string]*runRecord{"file": {files: rec.files}, "read": {reads: rec.reads}} {
		got := &newest{}
		if _, err := only.read(nil, "", got); err != nil {
			t.Fatal(err)
		}
		if !got.at.Equal(stamped[kind]) {
			t.Errorf("the newest %s is stamped %v by the pass, %v by its own stat", kind, got.at, stamped[kind])
		}
	}
}
