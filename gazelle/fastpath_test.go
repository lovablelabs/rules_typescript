package typescript

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/mikn/rules_typescript/gazelle/incremental"
)

// fastPathTree is one package whose sources and test import each other, a
// second package the first imports from, and a directory no program lists.
func fastPathTree() map[string]string {
	return map[string]string{
		"MODULE.bazel":                 "module(name = \"fast_path\")\n",
		".bazelignore":                 "node_modules\n",
		"node_modules/.pnpm/lock.yaml": "lockfileVersion: '9.0'\n",
		"package.json":                 `{"type":"module"}`,
		"lib/tsconfig.json":            `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["*.ts"]}`,
		"lib/x.ts":                     "export const x = 1;\n",
		"lib/y.ts":                     "export const y = 2;\n",
		"app/tsconfig.json":            `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"paths":{"@app/*":["./*"]}},"include":["*.ts"]}`,
		"app/a.ts":                     "import { b } from './b';\nimport { x } from '../lib/x';\nexport const a = b + x;\n",
		"app/b.ts":                     "export const b = 1;\n",
		"app/c.ts":                     "import { b } from './b';\nexport const c = b;\n",
		"app/d.ts":                     "export const d = 4;\n",
		"app/e.ts":                     "export const e = 5;\n",
		"app/e.test.ts":                "import { d } from './d';\nexport const t = d;\n",
		"extra/z.ts":                   "export const z = 6;\n",
	}
}

// -ts_verbose is what says a run replayed through the fast path.
func fastPathReplayed(output string) bool { return strings.Contains(output, "fast path: replayed") }

// primeFastPath generates the tree, then records a run that changed nothing
// once every input is past the racy window.
func primeFastPath(t *testing.T, root, cache string, args ...string) {
	t.Helper()
	args = append([]string{"-ts_listing_cache=" + cache}, args...)
	for i := range 2 {
		if i > 0 {
			time.Sleep(pastRacyWindow)
		}
		if out, err := tsGazelle(t, root, args...); err != nil {
			t.Fatalf("gazelle %q: %v\n%s", args, err, out)
		}
	}
}

// Every edit the fast path takes must leave the BUILD files and log the full
// pass would: verify mode decides, runs that pass, and says so when they agree.
// Without -ts_verbose, whose per-listing edge counts an added import changes.
func TestFastPath_ReplayedEditsMatchTheFullPass(t *testing.T) {
	requireTsgo(t)
	for _, edit := range []struct {
		name  string
		files map[string]string
	}{
		{"a body edit", map[string]string{"app/c.ts": "import { b } from './b';\nexport const c = b + 1;\n"}},
		{"an import of a source another source of the rule imports", map[string]string{"app/d.ts": "import { b } from './b';\nexport const d = b;\n"}},
		{"a type-only import of such a source", map[string]string{"app/e.ts": "import type { b } from './b';\nexport const e: typeof b = 5;\n"}},
		{"the same source imported under another specifier", map[string]string{"app/c.ts": "import { b } from './b';\nimport { b as again } from '../app/b';\nexport const c = b + again;\n"}},
		{"an import beside a BUILD file rewritten with the same content", map[string]string{
			"app/d.ts":        "import { b } from './b';\nexport const d = b;\n",
			"app/BUILD.bazel": "",
		}},
		{"two sources importing such a source at once", map[string]string{
			"app/d.ts": "import { b } from './b';\nexport const d = b;\n",
			"app/e.ts": "import { b } from './b';\nexport const e = b;\n",
		}},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			root := writeTree(t, fastPathTree())
			cache := t.TempDir()
			primeFastPath(t, root, cache)
			if _, rewrite := edit.files["app/BUILD.bazel"]; rewrite {
				data, err := os.ReadFile(filepath.Join(root, "app/BUILD.bazel"))
				if err != nil {
					t.Fatal(err)
				}
				edit.files["app/BUILD.bazel"] = string(data)
			}
			writeWorkspace(t, root, edit.files)
			same := requireAsCacheless(t, root, edit.name)
			out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify")
			if err != nil || !strings.Contains(out, "fast path: verified") {
				t.Errorf("%s was not replayed and verified: %v\n%s", edit.name, err, out)
			}
			same()
		})
	}
}

// -ts_verbose logs each listing's edge count: an added import, which changes it,
// runs in full there, while a body edit, which changes no listing, still replays.
func TestFastPath_VerboseRunsReplayOnlyEditsThatKeepTheLog(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, fastPathTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache, "-ts_verbose")
	writeFile(t, filepath.Join(root, "app/c.ts"), "import { b } from './b';\nexport const c = b + 1;\n")
	if out := recordGazelle(t, root, cache); !fastPathReplayed(out) {
		t.Errorf("a body edit under -ts_verbose ran the full pass:\n%s", out)
	}
	writeFile(t, filepath.Join(root, "app/d.ts"), "import { b } from './b';\nexport const d = b;\n")
	same := requireAsCacheless(t, root, "an import under -ts_verbose")
	if out := recordGazelle(t, root, cache); !strings.Contains(out, "fast path: declined ("+declineVerboseLog+")") {
		t.Errorf("an import under -ts_verbose was not declined for its log:\n%s", out)
	}
	same()
}

// Each edit here can change a BUILD file, or changes what the proof reads; the
// fast path must decline it with its reason, and the full pass must run.
func TestFastPath_DeclinedEditsRunTheFullPass(t *testing.T) {
	requireTsgo(t)
	for _, edit := range []struct {
		name, code string
		do         func(t *testing.T, root string)
	}{
		{"a new test source", declineNotSource, editFile("app/f.test.ts", "export const f = 6;\n")},
		{"a new source beside a file of its stem", declineNotSource, editFile("app/b.tsx", "export const bx = 6;\n")},
		{"a new source that makes its package an app", declineNotSource, editFile("app/main.ts", "export const m = 6;\n")},
		{"a new source in a new directory", declineInputs, editFile("app/sub/f.ts", "export const f = 6;\n")},
		{"a new source beside a source no program lists", declineInputs, editFile("extra/y.ts", "export const y = 6;\n")},
		{"a new source importing another rule's source", declineWalkConditional, editFile("app/f.ts", "import { x } from '../lib/x';\nexport const f = x;\n")},
		{"a deleted source still imported", declineListingShape, removeFile("app/b.ts")},
		{"a deleted source and a BUILD edit", declineInputs, func(t *testing.T, root string) {
			removeFile("app/e.ts")(t, root)
			writeFile(t, filepath.Join(root, "app/BUILD.bazel"), "")
		}},
		{"a BUILD file emptied", declineBuildLoads, editFile("app/BUILD.bazel", "")},
		{"a tsconfig edit", declineInputs, editFile("app/tsconfig.json", `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["a.ts","b.ts"]}`)},
		{"a removed import", declineEdgeRemoved, editFile("app/c.ts", "export const c = 3;\n")},
		{"an import of a source nothing imports", declineSelectionWitness, editFile("app/d.ts", "import { e } from './e';\nexport const d = e;\n")},
		{"an import of a source only a test imports", declineWalkWitness, editFile("app/e.ts", "import { d } from './d';\nexport const e = d;\n")},
		{"an import that lists a new file", declineListingShape, editFile("app/e.ts", "import { z } from '../extra/z';\nexport const e = z;\n")},
		{"an import of another rule's source", declineWalkConditional, editFile("app/c.ts", "import { b } from './b';\nimport { x } from '../lib/x';\nexport const c = b + x;\n")},
		{"a second import of the same specifier", declineSpecifierReused, editFile("app/c.ts", "import { b } from './b';\nimport './b';\nexport const c = b;\n")},
		{"an import through a paths alias", declinePatch, editFile("app/d.ts", "import { b } from '@app/b';\nexport const d = b;\n")},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			root := writeTree(t, fastPathTree())
			cache := t.TempDir()
			primeFastPath(t, root, cache, "-ts_verbose")
			edit.do(t, root)
			same := requireAsCacheless(t, root, edit.name)
			out := recordGazelle(t, root, cache)
			if want := "fast path: declined (" + edit.code + ")"; !strings.Contains(out, want) {
				t.Errorf("%s: want %q in the log:\n%s", edit.name, want, out)
			}
			same()
		})
	}
}

func editFile(rel, body string) func(t *testing.T, root string) {
	return func(t *testing.T, root string) { writeFile(t, filepath.Join(root, rel), body) }
}

func removeFile(rel string) func(t *testing.T, root string) {
	return func(t *testing.T, root string) {
		if err := os.Remove(filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
}

// A record kept while the fast path was off holds no facts to prove an edit
// with, so the edit runs in full however small it is.
func TestFastPath_ARecordWithoutFactsIsNeverReplayedForAnEdit(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, fastPathTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache, "-ts_verbose", "-ts_fastpath=off")
	writeFile(t, filepath.Join(root, "app/c.ts"), "import { b } from './b';\nexport const c = b + 1;\n")
	if out := recordGazelle(t, root, cache); !strings.Contains(out, "fast path: declined ("+declineNoFacts+")") {
		t.Errorf("a record without facts was not declined for lack of them:\n%s", out)
	}
}

// An accepted edit outside the racy window carries the record forward, so it
// matches the edited tree, and a later edit is proved against the moved record.
func TestFastPath_AReplayLeavesARecordOfTheEditedTree(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, fastPathTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	for _, edit := range []struct{ file, body string }{
		{"app/d.ts", "import { b } from './b';\nexport const d = b;\n"},
		{"app/e.ts", "import { b } from './b';\nexport const e = b;\n"},
	} {
		writeFile(t, filepath.Join(root, edit.file), edit.body)
		time.Sleep(pastRacyWindow)
		same := requireAsCacheless(t, root, edit.file)
		if out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify"); err != nil || !strings.Contains(out, "fast path: verified") {
			t.Fatalf("the edit to %s was not replayed and verified: %v\n%s", edit.file, err, out)
		}
		same()
		matches, err := filepath.Glob(filepath.Join(cache, runRecordName+"-*"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("want one run record, have %v (%v)", matches, err)
		}
		if rec := loadRunRecord(matches[0]); rec == nil || !rec.matches([]string{"BUILD.bazel", "BUILD"}, cache, nil) {
			t.Errorf("after the edit to %s the record does not stand for the edited tree", edit.file)
		}
	}
}

// -ts_fastpath=verify checks a replay against the full pass: a record whose
// log the full pass no longer writes fails the run and is removed.
func TestFastPath_VerifyFailsWhenTheFullPassLogsOtherwise(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, fastPathTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	matches, err := filepath.Glob(filepath.Join(cache, runRecordName+"-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one run record, have %v (%v)", matches, err)
	}
	rec := loadRunRecord(matches[0])
	if rec == nil {
		t.Fatalf("%s does not load as a run record", matches[0])
	}
	rec.stderr = append(rec.stderr, "gazelle: typescript: a line the full pass does not write\n"...)
	data, err := rec.encode()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, matches[0], string(data))
	writeFile(t, filepath.Join(root, "app/d.ts"), "import { b } from './b';\nexport const d = b;\n")
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify")
	if err == nil || !strings.Contains(out, "fast path: the full pass logs otherwise") {
		t.Errorf("verify passed a record the full pass contradicts: %v\n%s", err, out)
	}
	if _, err := os.Stat(matches[0]); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("verify kept the record it failed: %v", err)
	}
}

// The converge cases under -ts_fastpath=verify: whatever the fast path decides
// on each mutation, the run passes its check and writes what a cache-less run does.
func TestFastPath_VerifyPassesOnEveryConvergeMutation(t *testing.T) {
	requireTsgo(t)
	for _, tc := range convergeCases() {
		for _, mut := range tc.mutations {
			t.Run(tc.name+"/"+mut.kind, func(t *testing.T) {
				t.Parallel()
				root := writeTree(t, tc.files)
				cache := t.TempDir()
				primeFastPath(t, root, cache)
				applyMutation(t, root, mut)
				same := requireAsCacheless(t, root, mut.kind)
				out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify")
				if err != nil {
					t.Fatalf("the verified run failed: %v\n%s", err, out)
				}
				same()
			})
		}
	}
}

// A run that replays its Resolve phase reads none of that phase's inputs; its
// run record must hold them all the same, or a change to one replays stale.
func TestFastPath_AReplayedResolvePhaseLeavesItsReadsInTheRunRecord(t *testing.T) {
	root := t.TempDir()
	read := filepath.Join(root, "pkg", "package.json")
	listed := filepath.Join(root, "pkg", "listed.ts")
	writeFile(t, read, "{}\n")
	writeFile(t, listed, "export {};\n")
	saved := runInputs
	t.Cleanup(func() { runInputs, loadedListings = saved, sync.Map{} })
	loadedListings = sync.Map{}
	runInputs = &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}, key: "k", repoRoot: root}
	line, _ := fileLine(read)
	listedLine := incremental.EntryLine(listed)
	(&resolveRecord{
		Reads:    []recordedRead{{Path: read, Line: line}},
		Dirs:     []recordedRead{{Path: filepath.Join(root, "pkg"), Line: "f package.json\n"}},
		Listings: []recordedListing{{Reads: []string{listed}, ReadLines: []string{listedLine}}},
	}).noteReplayedReads()
	rec, why := runInputs.candidate()
	if why != "" {
		t.Fatal(why)
	}
	for _, want := range []struct {
		list []string
		path string
	}{{rec.files, read}, {rec.dirs, filepath.Join(root, "pkg")}, {rec.reads, listed}} {
		if !slices.Contains(want.list, want.path) {
			t.Errorf("the run record lacks %s, which the replayed phase read", want.path)
		}
	}
}

// A run that replays Resolve takes the phase's scans and whole reads from the
// record: a read only scanned keeps its scan, a whole one none, as in the run
// that recorded them.
func TestFastPath_AReplayedResolvePhaseKeepsItsScansAndWholeReads(t *testing.T) {
	root := t.TempDir()
	whole, scanned := filepath.Join(root, "pkg", "vitest.ts"), filepath.Join(root, "pkg", "listed.ts")
	for _, f := range []string{whole, scanned} {
		writeFile(t, f, "export {};\n")
	}
	saved := runInputs
	t.Cleanup(func() { runInputs, loadedListings = saved, sync.Map{} })
	loadedListings = sync.Map{}
	runInputs = &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}, key: "k", repoRoot: root}
	wholeLine, _ := fileLine(whole)
	scannedLine, _ := fileLine(scanned)
	(&resolveRecord{
		Reads:    []recordedRead{{Path: scanned, Line: scannedLine}, {Path: whole, Line: wholeLine}},
		Whole:    []string{whole},
		Listings: []recordedListing{{Reads: []string{scanned, whole}, ReadLines: []string{incremental.EntryLine(scanned), incremental.EntryLine(whole)}, Scans: []uint64{7, 9}}},
	}).noteReplayedReads()
	rec, why := runInputs.candidate()
	if why != "" {
		t.Fatal(why)
	}
	if want := []string{scanned, whole}; !slices.Equal(rec.reads, want) || !slices.Equal(rec.scans, []uint64{7, wholeScan}) {
		t.Errorf("the run record's scans of %q are %v, want [7 %d] for %q", rec.reads, rec.scans, wholeScan, want)
	}
}

// A run whose Resolve phase replays from the resolve record walks nothing; the
// run record it keeps must carry that record's facts, complete, or the next
// edit would be proved against none.
func TestFastPath_ARunThatReplaysResolveKeepsItsFacts(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["app/notes.md"] = "a data file\n"
	root := writeTree(t, tree)
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	resolveRecords, err := filepath.Glob(filepath.Join(cache, resolveRecordName+"-*"))
	if err != nil || len(resolveRecords) != 1 {
		t.Fatalf("want one resolve record, have %v (%v)", resolveRecords, err)
	}
	before, err := os.Stat(resolveRecords[0])
	if err != nil {
		t.Fatal(err)
	}
	// A data file's stat is the run record's input but not Resolve's.
	writeFile(t, filepath.Join(root, "app/notes.md"), "a data file\n")
	time.Sleep(pastRacyWindow)
	if out, err := tsGazelle(t, root, "-ts_listing_cache="+cache); err != nil {
		t.Fatalf("the run after touching a data file failed: %v\n%s", err, out)
	}
	if after, err := os.Stat(resolveRecords[0]); err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the run after touching a data file resolved again instead of replaying (%v)", err)
	}
	runRecords, err := filepath.Glob(filepath.Join(cache, runRecordName+"-*"))
	if err != nil || len(runRecords) != 1 {
		t.Fatalf("want one run record, have %v (%v)", runRecords, err)
	}
	rec := loadRunRecord(runRecords[0])
	if rec == nil {
		t.Fatalf("%s does not load as a run record", runRecords[0])
	}
	facts, err := decodeImportFacts(rec.facts)
	if err != nil || facts.Incomplete != "" || len(facts.Resolves) == 0 || len(facts.Selections) == 0 {
		t.Fatalf("the replaying run's record holds no complete facts: %v", err)
	}
	writeFile(t, filepath.Join(root, "app/d.ts"), "import { b } from './b';\nexport const d = b;\n")
	same := requireAsCacheless(t, root, "an import after a resolve replay")
	if out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify"); err != nil || !strings.Contains(out, "fast path: verified") {
		t.Errorf("an import after a run that replayed Resolve was not replayed and verified: %v\n%s", err, out)
	}
	same()
}

// tracedGazelle runs the binary and returns its log and the paths each of its
// processes took, from the trace hook.
func tracedGazelle(t *testing.T, root string, args ...string) (string, []string) {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "trace")
	binary, err := runfiles.Rlocation(os.Getenv("TS_GAZELLE"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, append([]string{"-repo_root=" + root}, args...)...)
	command.Dir = root
	command.Env = append(os.Environ(), "BUILD_WORKSPACE_DIRECTORY="+root, runTraceEnv+"="+trace)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("gazelle %q: %v\n%s", args, err, out)
	}
	data, err := os.ReadFile(trace)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return string(out), strings.Split(strings.TrimSpace(string(data)), "\n")
}

// A source gained, renamed or deleted inside a package changes only that
// rule's srcs: the fast path writes that one edit, byte for byte the full
// pass's, without running it.
func TestFastPath_AGainedRenamedOrDeletedSourceNoLongerForcesAFullPass(t *testing.T) {
	requireTsgo(t)
	keptTest := map[string]string{
		"app/sub/k.test.ts": "import { d } from '../d';\nexport const k = d;\n",
		"app/BUILD.bazel":   "ts_test(\n    name = \"app_test\",\n    srcs = [\n        \"e.test.ts\",\n        \"sub/k.test.ts\",  # keep\n    ],\n)\n",
	}
	for _, edit := range []struct {
		name string
		tree map[string]string
		do   func(t *testing.T, root string)
	}{
		{"a new source one source imports", nil, func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "app/f.ts"), "export const f = 6;\n")
			writeFile(t, filepath.Join(root, "app/c.ts"), "import { b } from './b';\nimport { f } from './f';\nexport const c = b + f;\n")
		}},
		{"a new source the include roots", nil, editFile("app/f.ts", "import { b } from './b';\nexport const f = b;\n")},
		{"new sources whose names a byte sort orders otherwise", nil, func(t *testing.T, root string) {
			for _, name := range []string{"a-b.ts", "a.b.ts", "A2.ts", "b10.ts", "b9.ts"} {
				writeFile(t, filepath.Join(root, "app", name), "export const n = 1;\n")
			}
		}},
		{"a package's one source gains a sibling", map[string]string{"lib/y.ts": ""}, editFile("lib/w.ts", "export const w = 1;\n")},
		{"new sources in two packages", nil, func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "app/f.ts"), "export const f = 6;\n")
			writeFile(t, filepath.Join(root, "lib/w.ts"), "export const w = 1;\n")
		}},
		{"a new source a test imports", nil, func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "app/f.ts"), "export const f = 6;\n")
			writeFile(t, filepath.Join(root, "app/e.test.ts"), "import { d } from './d';\nimport { f } from './f';\nexport const t = d + f;\n")
		}},
		{"a source renamed with its importers", nil, func(t *testing.T, root string) {
			renameFile(t, root, "app/b.ts", "app/bee.ts")
			writeFile(t, filepath.Join(root, "app/a.ts"), "import { b } from './bee';\nimport { x } from '../lib/x';\nexport const a = b + x;\n")
			writeFile(t, filepath.Join(root, "app/c.ts"), "import { b } from './bee';\nexport const c = b;\n")
		}},
		{"a renamed source keeps its import of another rule", nil, func(t *testing.T, root string) { renameFile(t, root, "app/a.ts", "app/aa.ts") }},
		{"a source deleted and inlined by its importers", nil, func(t *testing.T, root string) {
			removeFile("app/b.ts")(t, root)
			writeFile(t, filepath.Join(root, "app/a.ts"), "const b = 1;\nimport { x } from '../lib/x';\nexport const a = b + x;\n")
			writeFile(t, filepath.Join(root, "app/c.ts"), "const b = 1;\nexport const c = b;\n")
		}},
		{"a deleted source only a test imported", nil, func(t *testing.T, root string) {
			removeFile("app/d.ts")(t, root)
			writeFile(t, filepath.Join(root, "app/e.test.ts"), "const d = 4;\nexport const t = d;\n")
		}},
		{"a leaf source deleted", nil, removeFile("app/e.ts")},
		{"a new source beside a files list a kept test source adds", keptTest, editFile("app/f.ts", "export const f = 6;\n")},
		{"a source renamed beside a files list a kept test source adds", keptTest, func(t *testing.T, root string) {
			renameFile(t, root, "app/e.ts", "app/ee.ts")
		}},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			tree := fastPathTree()
			for name, body := range edit.tree {
				if body == "" {
					delete(tree, name)
				} else {
					tree[name] = body
				}
			}
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache)
			edit.do(t, root)
			same := requireAsCacheless(t, root, edit.name)
			out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache)
			if !slices.Equal(paths, []string{"fast path edit"}) {
				t.Errorf("%s: the run took %q, want the fast path's BUILD edit and no full pass:\n%s", edit.name, paths, out)
			}
			same()
		})
	}
}

func renameFile(t *testing.T, root, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(root, from), filepath.Join(root, to)); err != nil {
		t.Fatal(err)
	}
}

// The fast path's BUILD edit outside the racy window leaves a record of the
// edited tree, so the next run on it replays instead of running in full.
func TestFastPath_TheRunAfterAWrittenBuildEditNoLongerRunsInFull(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, fastPathTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	writeFile(t, filepath.Join(root, "app/f.ts"), "export const f = 6;\n")
	time.Sleep(pastRacyWindow)
	same := requireAsCacheless(t, root, "a new source")
	if _, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache); !slices.Equal(paths, []string{"fast path edit"}) {
		t.Fatalf("the new source took %q, want the fast path's BUILD edit", paths)
	}
	same()
	if out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache); !slices.Equal(paths, []string{"run record"}) {
		t.Errorf("the run after the edit took %q, want the record's replay:\n%s", paths, out)
	}
	same()
}

// -ts_fastpath=verify writes the edit, then checks it against the full pass:
// facts that miss an exclude make the edit wrong, and verify puts the BUILD
// file back and removes the record.
func TestFastPath_VerifyLeavesNoWrongBuildEditBehind(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["app/BUILD.bazel"] = "# gazelle:exclude f.ts\n"
	root := writeTree(t, tree)
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	matches, err := filepath.Glob(filepath.Join(cache, runRecordName+"-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one run record, have %v (%v)", matches, err)
	}
	rec := loadRunRecord(matches[0])
	if rec == nil {
		t.Fatalf("%s does not load as a run record", matches[0])
	}
	facts, err := decodeImportFacts(rec.facts)
	if err != nil || len(facts.Excludes) != 1 {
		t.Fatalf("the record's facts hold no exclude pattern (%v)", err)
	}
	facts.Excludes = nil
	if rec.facts, err = facts.encode(); err != nil {
		t.Fatal(err)
	}
	data, err := rec.encode()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, matches[0], string(data))
	before, err := os.ReadFile(filepath.Join(root, "app/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "app/f.ts"), "export const f = 6;\n")
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify")
	if err == nil || !strings.Contains(out, "fast path: the full pass writes BUILD files the fast path does not") {
		t.Errorf("verify passed an edit the full pass does not write: %v\n%s", err, out)
	}
	if after, err := os.ReadFile(filepath.Join(root, "app/BUILD.bazel")); err != nil || !bytes.Equal(after, before) {
		t.Errorf("verify left the edit it failed in app/BUILD.bazel (%v):\n%s", err, after)
	}
	if _, err := os.Stat(matches[0]); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("verify kept the record it failed: %v", err)
	}
}

// An entry no part of the run read, by name or by content, cannot change what
// the run writes: a Go file beside the walk, or a JSON file whose content no
// listing and no rule reads, moves no BUILD file.
func TestFastPath_AnEntryNothingReadsTakesTheFastPath(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["tools/gen.go"] = "package tools\n"
	tree["app/messages/en.json"] = `{"greeting":"hello"}`
	newSource := editFile("app/f.ts", "import { b } from './b';\nexport const f = b;\n")
	for _, edit := range []struct {
		name, path string
		do         func(t *testing.T, root string)
	}{
		{"a new source beside an edited Go file", "fast path edit", func(t *testing.T, root string) {
			newSource(t, root)
			editFile("tools/gen.go", "package tools\n\nconst Gen = 1\n")(t, root)
		}},
		{"a new source beside a Go file added and another deleted", "fast path edit", func(t *testing.T, root string) {
			newSource(t, root)
			editFile("tools/other.go", "package tools\n")(t, root)
			removeFile("tools/gen.go")(t, root)
		}},
		{"a new source beside an edited JSON its package holds as data", "fast path edit", func(t *testing.T, root string) {
			newSource(t, root)
			editFile("app/messages/en.json", `{"greeting":"hello, world"}`)(t, root)
		}},
		{"an import beside an edited Go file", "fast path", func(t *testing.T, root string) {
			editFile("app/d.ts", "import { b } from './b';\nexport const d = b;\n")(t, root)
			editFile("tools/gen.go", "package tools\n\nconst Gen = 1\n")(t, root)
		}},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache)
			edit.do(t, root)
			same := requireAsCacheless(t, root, edit.name)
			out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache)
			if !slices.Equal(paths, []string{edit.path}) {
				t.Errorf("%s: the run took %q, want %q and no full pass:\n%s", edit.name, paths, edit.path, out)
			}
			same()
		})
	}
}

// An entry the run read keeps the full pass: a JSON a source imports, a file a
// listing probed for, a JSON whose name joins its package's srcs, a package.json.
func TestFastPath_AnEntryTheRunReadsStillRunsTheFullPass(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["tools/gen.go"] = "package tools\n"
	tree["app/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"resolveJsonModule":true},"include":["*.ts"]}`
	tree["app/data.json"] = `{"d":4}`
	tree["app/d.ts"] = "import data from './data.json';\nexport const d = data.d;\n"
	tree["app/messages/de.json"] = `{"greeting":"hallo"}`
	tree["app/c.ts"] = "import { b } from './b';\nimport '../extra/style.css';\nexport const c = b;\n"
	newSource := editFile("app/f.ts", "import { b } from './b';\nexport const f = b;\n")
	for _, edit := range []struct {
		name, code string
		do         func(t *testing.T, root string)
	}{
		{"a new source beside an edited JSON a source imports", declineNotSource, func(t *testing.T, root string) {
			newSource(t, root)
			editFile("app/data.json", `{"d":5}`)(t, root)
		}},
		{"a new source beside a JSON its package gains as data", declineInputs, func(t *testing.T, root string) {
			newSource(t, root)
			editFile("app/messages/en.json", `{"greeting":"hello"}`)(t, root)
		}},
		{"a stylesheet added where an import probed for it", declineNotSource, editFile("extra/style.css", "a {}\n")},
		{"a new source beside a package.json added next to a Go file", declineInputs, func(t *testing.T, root string) {
			newSource(t, root)
			editFile("tools/package.json", `{"name":"tools"}`)(t, root)
		}},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache, "-ts_verbose")
			edit.do(t, root)
			same := requireAsCacheless(t, root, edit.name)
			out := recordGazelle(t, root, cache)
			if want := "fast path: declined (" + edit.code + ")"; !strings.Contains(out, want) {
				t.Errorf("%s: want %q in the log:\n%s", edit.name, want, out)
			}
			same()
		})
	}
}

// A directory the edit adds that holds nothing the run reads, such as a new Go
// package, generates nothing, so the edit around it still takes the fast path,
// and the record it leaves still sees a source added there later.
func TestFastPath_AGainedDirectoryNothingReadsTakesTheFastPath(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["tools/gen.go"] = "package tools\n"
	for _, edit := range []struct {
		name, path, then string
		do               func(t *testing.T, root string)
		extra            map[string]string
	}{
		{"a new Go package", "fast path", "tools/emulator/x.ts", editFile("tools/emulator/runtime.go", "package emulator\n"), nil},
		{"a new Go package no ignore_directories pattern matches", "fast path", "tools/emulator/x.ts", editFile("tools/emulator/runtime.go", "package emulator\n"),
			map[string]string{"REPO.bazel": "ignore_directories([\"**/node_modules\", \"bazel-*\", \"tools/coverage\", \"**/emulators\"])\n"}},
		{"a new nested directory of data", "fast path", "tools/fixtures/a/b/x.ts", func(t *testing.T, root string) {
			editFile("tools/fixtures/a/b/data.yaml", "a: 1\n")(t, root)
			editFile("tools/fixtures/notes.md", "notes\n")(t, root)
		}, nil},
		{"a new Go package beside an import", "fast path", "tools/emulator/x.ts", func(t *testing.T, root string) {
			editFile("tools/emulator/runtime.go", "package emulator\n")(t, root)
			editFile("app/d.ts", "import { b } from './b';\nexport const d = b;\n")(t, root)
		}, nil},
		{"a new Go package beside a new source", "fast path edit", "", func(t *testing.T, root string) {
			editFile("tools/emulator/runtime.go", "package emulator\n")(t, root)
			editFile("app/f.ts", "import { b } from './b';\nexport const f = b;\n")(t, root)
		}, nil},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			tree := maps.Clone(tree)
			maps.Copy(tree, edit.extra)
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache)
			edit.do(t, root)
			time.Sleep(pastRacyWindow)
			same := requireAsCacheless(t, root, edit.name)
			out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache)
			if !slices.Equal(paths, []string{edit.path}) {
				t.Errorf("%s: the run took %q, want %q and no full pass:\n%s", edit.name, paths, edit.path, out)
			}
			same()
			if edit.then == "" {
				return
			}
			editFile(edit.then, "export const x = 1;\n")(t, root)
			time.Sleep(pastRacyWindow)
			same = requireAsCacheless(t, root, edit.name+", then a source in it")
			if out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache); !slices.Equal(paths, []string{"full pass"}) {
				t.Errorf("%s: a source added in the gained directory took %q, want the full pass:\n%s", edit.name, paths, out)
			}
			same()
		})
	}
}

// A gained directory the run would read declines: one holding a source, a
// BUILD file or a package.json, one inside a package whose names the run reads
// every one of, here as data, and ones the walk excludes: by a directive, as
// .git, or by an ignore_directories pattern matching it or a directory above it.
func TestFastPath_AGainedDirectoryTheRunReadsStillRunsTheFullPass(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["tools/gen.go"] = "package tools\n"
	tree["data/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["src/*.ts"]}`
	tree["data/src/m.ts"] = "export const m = 1;\n"
	tree["data/messages/en.yaml"] = "greeting: hello\n"
	tree["BUILD.bazel"] = "# gazelle:exclude tools/skipped\n"
	for _, edit := range []struct {
		name  string
		extra map[string]string
		do    func(t *testing.T, root string)
		// why is the decline's code and detail, where only one check may decline.
		why string
	}{
		{"a directory with a source", nil, editFile("tools/emulator/x.ts", "export const x = 1;\n"), ""},
		{"a directory with a BUILD file", nil, editFile("tools/emulator/BUILD.bazel", ""), ""},
		{"a directory with a package.json", nil, editFile("tools/emulator/deep/package.json", `{"name":"e"}`), ""},
		{"a directory of data in a package", nil, editFile("data/fixtures/de.yaml", "greeting: hallo\n"), ""},
		{"a directory the walk excludes", nil, editFile("tools/skipped/gen.go", "package skipped\n"), declineExcluded + "): the exclude pattern tools/skipped"},
		{"a directory .bazelignore names", map[string]string{".bazelignore": "node_modules\n./tools/ignored\n"},
			editFile("tools/ignored/data.txt", "data\n"), declineExcluded + "): .bazelignore names tools/ignored"},
		{"a nested git clone", nil, editFile("tools/vendored/.git/HEAD", "ref: refs/heads/main\n"), declineExcluded + "): the walk skips .git"},
		{"a directory an ignore_directories pattern matches", map[string]string{"REPO.bazel": "ignore_directories([\"**/node_modules\", \"**/coverage\"])\n"},
			editFile("tools/coverage/lcov.info", "TN:\n"), declineExcluded + "): REPO.bazel's ignore_directories pattern **/coverage could match tools/coverage"},
		{"a directory under one an ignore_directories pattern matches", map[string]string{"REPO.bazel": "ignore_directories([\"tools/cov*\"])\n"},
			editFile("tools/coverage/html/index.html", "<p></p>\n"), declineExcluded + "): REPO.bazel's ignore_directories pattern tools/cov* could match tools/coverage"},
		{"a directory holding a declared output", map[string]string{"tools/BUILD.bazel": "genrule(name = \"g\", outs = [\"gen/out.txt\"], cmd = \"true\")\n"},
			editFile("tools/gen/notes.md", "notes\n"), declineGenerated + "): tools/gen, a gained directory, holds or lies in a declared output"},
		{"ignore_directories of no string literal", map[string]string{"REPO.bazel": "DIRS = [\"x\"]\n\nignore_directories(DIRS)\n"},
			editFile("tools/emulator/runtime.go", "package emulator\n"), declineExcluded + "): REPO.bazel's ignore_directories takes no list literal"},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			tree := maps.Clone(tree)
			maps.Copy(tree, edit.extra)
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache, "-ts_verbose")
			edit.do(t, root)
			same := requireAsCacheless(t, root, edit.name)
			out := recordGazelle(t, root, cache)
			switch {
			case edit.why != "" && !strings.Contains(out, "fast path: declined ("+edit.why):
				t.Errorf("%s: want the fast path declined because %s:\n%s", edit.name, edit.why, out)
			case edit.why == "" && !strings.Contains(out, "fast path: declined ("+declineInputs+")"):
				t.Errorf("%s: want the fast path declined:\n%s", edit.name, out)
			}
			same()
		})
	}
}

// -ts_fastpath=verify runs the full pass behind a gained directory's replay: a
// record forged to say the run never read every name of the package the
// directory joins as data fails the run and is removed.
func TestFastPath_VerifyCatchesAForgedGainedDirectory(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["data/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["src/*.ts"]}`
	tree["data/src/m.ts"] = "export const m = 1;\n"
	tree["data/messages/en.yaml"] = "greeting: hello\n"
	root := writeTree(t, tree)
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	recordFile, rec := onlyRunRecord(t, cache)
	facts, err := decodeImportFacts(rec.facts)
	if err != nil || !has(facts.names.Dirs, "data") {
		t.Fatalf("the record does not say the run read every name in data: %v", err)
	}
	facts.names.Dirs = slices.DeleteFunc(facts.names.Dirs, func(dir string) bool { return dir == "data" })
	if rec.facts, err = facts.encode(); err != nil {
		t.Fatal(err)
	}
	dirs, err := dirLines(rec.dirs, []string{"BUILD.bazel", "BUILD"}, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := inputLines{dirs: dirs, files: fileLines(rec.files, nil), reads: listingReadLines(rec.reads, nil), roots: rootLines(rec.roots)}
	rec.sums = inputSums(lines, rec.dirs, root, &facts.names)
	writeRunRecord(t, recordFile, rec)
	editFile("data/fixtures/de.yaml", "greeting: hallo\n")(t, root)
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify")
	if err == nil || !strings.Contains(out, "fast path: the full pass writes BUILD files the fast path does not") {
		t.Errorf("verify passed a forged gained directory: %v\n%s", err, out)
	}
	if _, err := os.Stat(recordFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("verify kept the forged record: %v", err)
	}
}

// A subpath import ("#..."), which package.json's imports map to a file, takes
// its dep from the file's owner as a relative import does, so the fast path
// follows one; a bare specifier, which may name a workspace member, still declines.
func TestFastPath_ASubpathImportTakesTheFastPath(t *testing.T) {
	requireTsgo(t)
	tree := subpathTree()
	for _, edit := range []struct {
		name, body, path string
	}{
		{"a subpath import of a source of the rule", "import { b } from '#app/b';\nexport const d = b;\n", "fast path"},
		{"a bare specifier", "import { b } from '@app/b';\nexport const d = b;\n", "full pass"},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache)
			editFile("app/d.ts", edit.body)(t, root)
			time.Sleep(pastRacyWindow)
			same := requireAsCacheless(t, root, edit.name)
			if out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache); !slices.Equal(paths, []string{edit.path}) {
				t.Errorf("%s: the run took %q, want %q:\n%s", edit.name, paths, edit.path, out)
			}
			same()
		})
	}
}

// A new subpath import probes package.json, which Resolve checks for every probe a
// program keeps; one only the library's unreached test made stands for none.
func TestFastPath_ASubpathImportProbingMetadataNoKeptResolutionProbedDeclines(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, subpathTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache, "-ts_verbose")
	editFile("app/d.ts", "import { y } from '#lib/y';\nexport const d = y;\n")(t, root)
	time.Sleep(pastRacyWindow)
	same := requireAsCacheless(t, root, "a subpath import only a test made")
	if out := recordGazelle(t, root, cache); !strings.Contains(out, "fast path: declined ("+declineCandidate+"): app/d.ts's \"#lib/y\" probes package metadata") {
		t.Errorf("want the fast path declined for the metadata probe:\n%s", out)
	}
	same()
}

// subpathTree's app imports through package.json's imports and a paths alias,
// and its test imports lib/y.ts, which its library does not.
func subpathTree() map[string]string {
	tree := fastPathTree()
	tree["package.json"] = `{"type":"module","imports":{"#lib/*":"./lib/*.ts","#app/*":"./app/*.ts"}}`
	tree["app/a.ts"] = "import { b } from './b';\nexport const a = b;\n"
	tree["app/c.ts"] = "import { b } from '#app/b';\nimport { b as again } from '@app/b';\nexport const c = b + again;\n"
	tree["app/e.test.ts"] = "import { d } from './d';\nimport { y } from '#lib/y';\nexport const t = d + y;\n"
	return tree
}

// -ts_fastpath=verify runs the full pass behind a subpath import's replay: facts
// forged to say the library reaches its test and lib/y.ts and resolves that to a
// dep it always takes let a new dep through, which fails the run and removes the record.
func TestFastPath_VerifyCatchesAForgedSubpathImport(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, subpathTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	recordFile, rec := onlyRunRecord(t, cache)
	facts, err := decodeImportFacts(rec.facts)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int32{}
	for i, f := range facts.Files {
		ids[f] = int32(i)
	}
	y, d, test := ids["lib/y.ts"], ids["app/d.ts"], ids["app/e.test.ts"]
	forged := false
	forgedOutcome := edgeOutcome{To: y, Dep: "//lib", Owned: true}
	for ri, r := range facts.Resolves {
		for wi, w := range r.Walks {
			if w.Config || !has(w.Initial, d) && !has(w.Reached, d) {
				continue
			}
			if i, found := slices.BinarySearchFunc(w.Outcomes, y, func(o edgeOutcome, id int32) int { return cmp.Compare(o.To, id) }); found {
				w.Outcomes[i] = forgedOutcome
			} else {
				w.Outcomes = slices.Insert(w.Outcomes, i, forgedOutcome)
			}
			facts.Resolves[ri].Walks[wi] = w
			facts.Resolves[ri].Unconditional = mergeSorted(facts.Resolves[ri].Unconditional, slices.Values([]string{"//lib"}))
			forged = forged || !has(w.Initial, test)
		}
	}
	if !forged {
		t.Fatal("the facts hold no walk over the library's sources")
	}
	for i, sel := range facts.Selections {
		facts.Selections[i].Unreached = slices.DeleteFunc(sel.Unreached, func(id int32) bool { return id == test || id == y })
	}
	if rec.facts, err = facts.encode(); err != nil {
		t.Fatal(err)
	}
	writeRunRecord(t, recordFile, rec)
	editFile("app/d.ts", "import { y } from '#lib/y';\nexport const d = y;\n")(t, root)
	time.Sleep(pastRacyWindow)
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify")
	if err == nil || !strings.Contains(out, "fast path: the full pass writes BUILD files the fast path does not") {
		t.Errorf("verify passed a forged subpath import: %v\n%s", err, out)
	}
	if _, err := os.Stat(recordFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("verify kept the forged record: %v", err)
	}
}

// onlyRunRecord is the cache's one run record and where it lies.
func onlyRunRecord(t *testing.T, cache string) (string, *runRecord) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(cache, runRecordName+"-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one run record, have %v (%v)", matches, err)
	}
	rec := loadRunRecord(matches[0])
	if rec == nil {
		t.Fatalf("%s does not load as a run record", matches[0])
	}
	return matches[0], rec
}

func writeRunRecord(t *testing.T, file string, rec *runRecord) {
	t.Helper()
	data, err := rec.encode()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, string(data))
}

// listingFiles are the listing cache's entries by name, with what they hold.
func listingFiles(t *testing.T, cache string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	records := []string{runRecordName, resolveRecordName, buildSnapshotName}
	files := map[string]string{}
	for _, e := range entries {
		if e.Type().IsRegular() && !slices.ContainsFunc(records, func(record string) bool { return strings.HasPrefix(e.Name(), record) }) {
			data, err := os.ReadFile(filepath.Join(cache, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[e.Name()] = string(data)
		}
	}
	return files
}

// A body edit replays from the record's scans without reading or writing a
// listing, and an import edit after it, to another source or the same one,
// still patches past the entry it left stale.
func TestFastPath_ABodyEditReplaysWithoutTouchingAListing(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, fastPathTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	for _, edit := range []struct{ file, body, path string }{
		{"app/c.ts", "import { b } from './b';\nexport const c = b + 1;\n", "fast path scan"},
		{"app/d.ts", "import { b } from './b';\nexport const d = b;\n", "fast path"},
		{"app/c.ts", "import { b } from './b';\nexport const c = b + 2;\n", "fast path scan"},
		{"app/c.ts", "import { b } from './b';\nimport { b as again } from '../app/b';\nexport const c = b + again;\n", "fast path"},
	} {
		listings := listingFiles(t, cache)
		writeFile(t, filepath.Join(root, edit.file), edit.body)
		time.Sleep(pastRacyWindow)
		same := requireAsCacheless(t, root, edit.file)
		out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache)
		if !slices.Equal(paths, []string{edit.path}) {
			t.Fatalf("the edit to %s took %q, want %q and no full pass:\n%s", edit.file, paths, edit.path, out)
		}
		same()
		if edit.path == "fast path scan" && !maps.Equal(listingFiles(t, cache), listings) {
			t.Errorf("the replay of a body edit to %s wrote the listing cache", edit.file)
		}
	}
}

// aliasedConfig maps specifiers with no slash, which a misread regex can hide whole.
const aliasedConfig = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"paths":{"yy":["../lib/y.ts"],"bb":["./b.ts"]}},"include":["*.ts"]}`

// nonNullMisread imports spec where the scanner reads a regex from a division after x!.
func nonNullMisread(spec string) string {
	return "declare const x: number;\nconst z = x! / 2; import { v } from '" + spec + "'; const w = 4 / 2;\nexport const d2 = v + z + w;\n"
}

// An edit whose imports tsgo reads otherwise than the scanner must not replay as
// a body edit or patch: an import moved into a block, which tsgo does not resolve,
// a from split off its clause, and a source the scanner misreads, from the start or after the edit.
func TestFastPath_AnImportTsgoReadsOtherwiseIsNoBodyEdit(t *testing.T) {
	requireTsgo(t)
	topLevel := "export function d() {\n  return 4;\n}\nimport { y } from '../lib/y';\nexport const d2 = y;\n"
	for _, edit := range []struct{ name, before, after string }{
		{"an import moved into a function body", topLevel, "export function d() {\n  return 4;\nimport { y } from '../lib/y';\n}\nexport const d2 = 0;\n"},
		{"an import moved into a class body", topLevel, "export class D {\n  n = 4;\nimport { y } from '../lib/y';\n}\nexport const d2 = 0;\n"},
		{"a from split off its clause", "import '../lib/y';\nexport const d2 = 0;\n", "declare const from: unknown;\nfrom\n'../lib/y';\nexport const d2 = 0;\n"},
		{"a source misread as a regex", "export {};\nconst n = {} / 2; import { y } from '../lib/y'; // '\nexport const d2 = y + n;\n",
			"export {};\nconst n = {} / 2; import { b } from './b'; // '\nexport const d2 = b + n;\n"},
		{"a source misread after a non-null assertion", nonNullMisread("yy"), nonNullMisread("bb")},
		{"an import an edit hides behind a regex after an object literal", "export const d2 = 0;\n",
			"export {};\nconst n = {} / 2; import { y } from '../lib/y'; // '\nexport const d2 = y + n;\n"},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			tree := fastPathTree()
			tree["app/tsconfig.json"] = aliasedConfig
			tree["app/a.ts"] = "import { b } from './b';\nexport const a = b;\n"
			tree["app/d.ts"] = edit.before
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache)
			editFile("app/d.ts", edit.after)(t, root)
			time.Sleep(pastRacyWindow)
			same := requireAsCacheless(t, root, edit.name)
			if out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache); slices.Contains(paths, "fast path scan") {
				t.Errorf("%s replayed as a body edit:\n%s", edit.name, out)
			}
			same()
		})
	}
}

// A listing entry an older binary wrote keeps the scan of a source the scanner
// misreads, which patch-state version 4 leaves out; patching it past an import
// edit there would keep the edge tsgo no longer lists, so it is listed again.
func TestListingPatch_AnEntryOfAnOlderPatchStateIsListedAgain(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["app/a.ts"] = "import { b } from './b';\nexport const a = b;\n"
	tree["app/tsconfig.json"] = aliasedConfig
	tree["app/d.ts"] = nonNullMisread("yy")
	root := writeTree(t, tree)
	cache := t.TempDir()
	off := []string{"-ts_listing_cache=" + cache, "-ts_fastpath=off", "-ts_run_record=off", "-ts_resolve_record=off"}
	if out, err := tsGazelle(t, root, off...); err != nil {
		t.Fatalf("gazelle: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	forged := 0
	for _, e := range entries {
		file := filepath.Join(cache, e.Name())
		entry, err := readListing(file)
		if err != nil || entry.Sources == nil || !slices.Contains(entry.Listing.Files, "app/d.ts") {
			continue
		}
		if _, kept := entry.Sources["app/d.ts"]; kept {
			t.Fatalf("%s keeps the scan of app/d.ts, which misses an import tsgo resolved", e.Name())
		}
		src, ok := scanSpecs(filepath.Join(root, "app/d.ts"))
		if !ok {
			t.Fatal("app/d.ts does not scan")
		}
		entry.Sources["app/d.ts"] = cachedSource{Specs: src.Specs, Rest: src.Rest}
		entry.PatchVersion = 3
		writeListingFile(file, entry)
		forged++
	}
	if forged == 0 {
		t.Fatal("no listing entry lists app/d.ts")
	}
	editFile("app/d.ts", nonNullMisread("bb"))(t, root)
	time.Sleep(pastRacyWindow)
	same := requireAsCacheless(t, root, "an import edit past an older entry")
	if out, err := tsGazelle(t, root, off...); err != nil {
		t.Fatalf("gazelle: %v\n%s", err, out)
	}
	same()
}

// A run that replays Resolve stands for the phase's whole reads by the ones the
// record names, not by every read: a source the phase only scanned keeps its
// scan, so a body edit after it still replays from the scans.
func TestFastPath_ABodyEditAfterAResolveReplayStillReplaysFromTheScans(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["BUILD.bazel"] = "# gazelle:exclude tools/skipped\n"
	tree["tools/gen.go"] = "package tools\n"
	root := writeTree(t, tree)
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	// Without the fast path the import edit runs in full, and the resolve record it leaves read app/c.ts to patch its listing.
	writeFile(t, filepath.Join(root, "app/c.ts"), "import { b } from './b';\nimport { x } from '../lib/x';\nexport const c = b + x;\n")
	time.Sleep(pastRacyWindow)
	if out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=off"); err != nil {
		t.Fatalf("the import edit failed: %v\n%s", err, out)
	}
	resolveRecords, err := filepath.Glob(filepath.Join(cache, resolveRecordName+"-*"))
	if err != nil || len(resolveRecords) != 1 {
		t.Fatalf("want one resolve record, have %v (%v)", resolveRecords, err)
	}
	if rec := loadResolveRecord(resolveRecords[0]); rec == nil || !slices.ContainsFunc(rec.Reads, func(r recordedRead) bool { return r.Path == filepath.Join(root, "app/c.ts") }) {
		t.Fatal("the resolve record does not hold the read of app/c.ts")
	}
	before, err := os.Stat(resolveRecords[0])
	if err != nil {
		t.Fatal(err)
	}
	// The fast path declines an excluded directory, which Resolve never reads.
	writeFile(t, filepath.Join(root, "tools/skipped/gen.go"), "package skipped\n")
	time.Sleep(pastRacyWindow)
	if out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache); !slices.Contains(paths, "full pass") {
		t.Fatalf("the run after a new excluded directory took %q, want the full pass:\n%s", paths, out)
	}
	if after, err := os.Stat(resolveRecords[0]); err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the run after a new excluded directory resolved again instead of replaying (%v)", err)
	}
	writeFile(t, filepath.Join(root, "app/c.ts"), "import { b } from './b';\nimport { x } from '../lib/x';\nexport const c = b + x + 1;\n")
	time.Sleep(pastRacyWindow)
	same := requireAsCacheless(t, root, "a body edit after a resolve replay")
	if out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache); !slices.Equal(paths, []string{"fast path scan"}) {
		t.Errorf("a body edit after a resolve replay took %q, want the scan replay:\n%s", paths, out)
	}
	same()
}

// -ts_fastpath=verify runs the full pass behind a scan replay: a record whose
// scan of an edited source was forged to match fails the run and is removed.
func TestFastPath_VerifyCatchesAForgedScanReplay(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, fastPathTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	edited := filepath.Join(root, "lib/y.ts")
	writeFile(t, edited, "import { a } from '../app/a';\nexport const y = a;\n")
	recordFile, rec := onlyRunRecord(t, cache)
	j, found := slices.BinarySearch(rec.reads, edited)
	if !found || rec.scans[j] == 0 {
		t.Fatalf("the record holds no scan of %s", edited)
	}
	data, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	src, err := incremental.Scan(edited, data)
	if err != nil {
		t.Fatal(err)
	}
	rec.scans[j] = scanSum(src)
	writeRunRecord(t, recordFile, rec)
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify")
	if err == nil || !strings.Contains(out, "fast path: the full pass writes BUILD files the fast path does not") {
		t.Errorf("verify passed a forged scan replay: %v\n%s", err, out)
	}
	if _, err := os.Stat(recordFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("verify kept the forged record: %v", err)
	}
}

// A source the extension read whole by name, as a workers config is for its
// wrangler literals, may matter beyond its scan, so the record keeps no scan of it.
func TestFastPath_ASourceReadWholeByNameKeepsNoScan(t *testing.T) {
	root := t.TempDir()
	whole, listed := filepath.Join(root, "w", "vitest.ts"), filepath.Join(root, "w", "a.ts")
	sources := map[string]cachedSource{}
	for _, f := range []string{whole, listed} {
		writeFile(t, f, "export const config = './wrangler.jsonc';\n")
		src, ok := scanSpecs(f)
		if !ok {
			t.Fatalf("%s does not scan", f)
		}
		rel, _ := repoRelative(root, f)
		sources[rel] = src
	}
	saved := runInputs
	t.Cleanup(func() { runInputs, loadedListings = saved, sync.Map{} })
	loadedListings = sync.Map{}
	runInputs = &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}, key: "k", repoRoot: root}
	reads := []string{listed, whole}
	loadedListings.Store("listing", &cachedListing{Root: root, Reads: reads, Sources: sources,
		ReadLines: []string{incremental.EntryLine(listed), incremental.EntryLine(whole)}})
	if _, err := readInput(whole); err != nil {
		t.Fatal(err)
	}
	rec, why := runInputs.candidate()
	if why != "" {
		t.Fatal(why)
	}
	if !slices.Equal(rec.reads, reads) || rec.scans[0] == 0 || rec.scans[0] == wholeScan || rec.scans[1] != wholeScan {
		t.Errorf("the record's scans of %q are %v, want one only for the source no one read whole", rec.reads, rec.scans)
	}
}

// scanSum must cover every field of a scan: each is something a listing or its
// patch reads of a source.
func TestScanSum_EveryFieldOfAScanMovesTheSum(t *testing.T) {
	base := incremental.Scanned{Specs: []string{"import:./a"}, Rest: "r", Resolutions: map[string]int{"./a": 1}, Counted: true, Exact: true}
	v := reflect.ValueOf(&base).Elem()
	for i := range v.NumField() {
		moved := base
		moved.Specs, moved.Resolutions = slices.Clone(base.Specs), maps.Clone(base.Resolutions)
		f := reflect.ValueOf(&moved).Elem().Field(i)
		switch f.Kind() {
		case reflect.Slice:
			f.Set(reflect.Append(f, reflect.ValueOf("import:./b")))
		case reflect.String:
			f.SetString(f.String() + "x")
		case reflect.Map:
			f.SetMapIndex(reflect.ValueOf("./a"), reflect.ValueOf(2))
		case reflect.Bool:
			f.SetBool(!f.Bool())
		default:
			t.Fatalf("no change made to field %s of kind %s; extend this test", v.Type().Field(i).Name, f.Kind())
		}
		if scanSum(moved) == scanSum(base) {
			t.Errorf("scanSum ignores incremental.Scanned.%s", v.Type().Field(i).Name)
		}
	}
}

// An uncounted or inexact scan may miss an import tsgo reads, so it keeps no sum
// to replay from, even when an edit leaves it as it was.
func TestScanSum_AnUncountedOrInexactScanHasNone(t *testing.T) {
	for _, src := range []incremental.Scanned{
		{Specs: []string{"import:./a"}, Rest: "r", Counted: false, Exact: true},
		{Specs: []string{"import:./a"}, Rest: "r", Counted: true, Exact: false},
	} {
		if sum := scanSum(src); sum != 0 {
			t.Errorf("scanSum(%+v) = %d, want 0", src, sum)
		}
	}
}

// Whatever a source edit changes in tsgo's listing, the scan sum moves with it,
// so the scan replay never stands for an edit a listing sees; a body edit moves
// neither. Diagnostics are left out: a replay, patched or not, logs the recorded run's.
func TestScanSum_EveryEditThatChangesAListingMovesTheSum(t *testing.T) {
	requireTsgo(t)
	tsgo, err := newProgramStore().binary()
	if err != nil {
		t.Fatal(err)
	}
	const was = "import { b } from './b';\nexport const c = b;\n"
	const lazy = "export const lazy = () => import('./b');\n"
	for _, edit := range []struct {
		name, file, body string
		bodyEdit         bool
		from             string
	}{
		{name: "a body edit", file: "c.ts", body: "import { b } from './b';\nexport const c = b + 1;\n", bodyEdit: true},
		{name: "a comment and a string that mention imports", file: "c.ts", body: "import { b } from './b';\n// the import of ./d waits\nexport const c = b + ' require ./d'.length;\n", bodyEdit: true},
		{name: "a syntax error, which only a diagnostic shows", file: "c.ts", body: "import { b } from './b';\nexport const c = b +;\n", bodyEdit: true},
		{name: "a jsx body edit", file: "v.tsx", body: "export const v = <div>two</div>;\n", bodyEdit: true},
		{name: "a side-effect import", file: "c.ts", body: "import './d';\n" + was},
		{name: "a type-only import", file: "c.ts", body: "import type { D } from './d';\n" + was},
		{name: "a dynamic import", file: "c.ts", body: was + "export const lazy = () => import('./d');\n"},
		{name: "a second dynamic import of the same module", file: "c.ts", body: "import { b } from './b';\nexport const c = () => import('./b');\nexport const e = () => import('./b');\n"},
		{name: "a comment in a dynamic import that tsgo resolves it again for", file: "c.ts", body: was + "export const lazy = () => import(/* import */ './b');\n", from: was + lazy},
		{name: "a dynamic import of a template literal", file: "c.ts", body: was + "export const lazy = () => import(`./d`);\n"},
		{name: "a template-literal specifier renamed", file: "c.ts", body: was + "export const lazy = () => import(`./b`);\n", from: was + "export const lazy = () => import(`./nope`);\n"},
		{name: "an import-equals require", file: "c.ts", body: "import d = require('./d');\n" + was},
		{name: "an export-from", file: "c.ts", body: "export { d } from './d';\n" + was},
		{name: "an export-star", file: "c.ts", body: "export * from './d';\n" + was},
		{name: "a JSON import", file: "c.ts", body: "import data from './data.json';\n" + was},
		{name: "a triple-slash path reference", file: "c.ts", body: "/// <reference path=\"./d.ts\" />\n" + was},
		{name: "a triple-slash types reference", file: "c.ts", body: "/// <reference types=\"dep\" />\n" + was},
		{name: "a triple-slash lib reference", file: "c.ts", body: "/// <reference lib=\"webworker\" />\n" + was},
		{name: "a module augmentation", file: "c.ts", body: was + "declare module './b' {\n  interface B { extra: number }\n}\n"},
		{name: "a JSDoc import type in JavaScript", file: "j.js", body: "/** @type {import('./d').D} */\nexport const j = 1;\n"},
		{name: "a require in JavaScript", file: "j.js", body: "const d = require('./d');\nexport const j = d;\n"},
		{name: "a jsxImportSource pragma", file: "v.tsx", body: "/** @jsxImportSource dep */\nexport const v = <div>one</div>;\n"},
		{name: "a file that stops being a module", file: "c.ts", body: "const c = 1;\n"},
		{name: "an import hidden by a division after an object literal", file: "c.ts", body: "import { b } from './b';\nconst n = {} / 2; import { d } from './d'; // '\nexport const c = b + n;\n"},
		{name: "an import moved out from under an if head", file: "c.ts", body: was + "declare const x: boolean;\nimport './d';\n", from: was + "declare const x: boolean;\nif (x) import './d';\n"},
		{name: "an import moved out from under a label", file: "c.ts", body: was + "import './d';\n", from: was + "l: import './d';\n"},
		{name: "an import moved out from under an else", file: "c.ts", body: was + "declare const x: boolean;\nimport './d';\n", from: was + "declare const x: boolean;\nif (x) {} else import './d';\n"},
		{name: "a path reference moved to the header", file: "c.ts", body: "/// <reference path=\"./d.ts\" />\nexport const c = 1;\n", from: "export const c = 1;\n/// <reference path=\"./d.ts\" />\n"},
		{name: "a jsxImportSource pragma moved to the header", file: "v.tsx", body: "/** @jsxImportSource dep */\nexport const v = <div>one</div>;\n", from: "export const v = <div>one</div>;\n/** @jsxImportSource dep */\n"},
		{name: "a jsxImportSource pragma moved out of a JSX expression to the header", file: "v.tsx", body: "/** @jsxImportSource dep */\nexport const v = <div>{}</div>;\n", from: "export const v = <div>{/** @jsxImportSource dep */}</div>;\n"},
		{name: "a jsxImportSource pragma on a later line of a pragma comment", file: "v.tsx", body: "/**\n * @jsxRuntime automatic\n * @jsxImportSource dep\n */\nexport const v = <div>one</div>;\n", from: "/**\n * @jsxRuntime automatic\n */\nexport const v = <div>one</div>;\n"},
		{name: "an upper-case jsxImportSource pragma", file: "v.tsx", body: "/** @JSXImportSource dep */\nexport const v = <div>one</div>;\n"},
		{name: "two jsxImportSource pragmas swapped, the last of which wins", file: "v.tsx", body: "/** @jsxImportSource react */\n/** @jsxImportSource dep */\nexport const v = <div>one</div>;\n", from: "/** @jsxImportSource dep */\n/** @jsxImportSource react */\nexport const v = <div>one</div>;\n"},
		{name: "a file that stops being a module beside a dynamic import", file: "c.ts", body: "declare module './b' {\n  interface B { extra: number }\n}\nconst lazy = () => import('./b');\n", from: "declare module './b' {\n  interface B { extra: number }\n}\nexport const lazy = () => import('./b');\n"},
		{name: "a file that stops being a module as its only import.meta goes", file: "c.ts", body: "declare module './b' {\n  interface B { extra: number }\n}\nfunction f() { return 1; }\n", from: "declare module './b' {\n  interface B { extra: number }\n}\nfunction f() { return import.meta.url; }\n"},
		{name: "an import beside a division after a non-null assertion", file: "c.ts", body: was + "declare const x: number;\nconst z = x! / 2; import \"dep\"; const w = 4 / 2;\n", from: was + "declare const x: number;\nconst z = x! / 2; const w = 4 / 2;\n"},
		{name: "an import beside a regex after an if head", file: "c.ts", body: was + "declare const s: string;\nif (s) /'/.test(s); import \"./d\"; // '\n",
			from: was + "declare const s: string;\nif (s) /'/.test(s); // '\n"},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			tree := map[string]string{
				"tsconfig.json":                     `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"jsx":"react-jsx","allowJs":true,"resolveJsonModule":true},"include":["*.ts","*.tsx","*.js"]}` + "\n",
				"b.ts":                              "export interface B { b: number }\nexport const b = 1;\n",
				"c.ts":                              was,
				"d.ts":                              "export interface D { d: number }\nexport const d = 4;\n",
				"data.json":                         `{"d":4}` + "\n",
				"v.tsx":                             "export const v = <div>one</div>;\n",
				"j.js":                              "export const j = 1;\n",
				"node_modules/dep/package.json":     `{"name":"dep","version":"1.0.0","types":"index.d.ts"}` + "\n",
				"node_modules/dep/index.d.ts":       "export declare const dep: number;\n",
				"node_modules/dep/jsx-runtime.d.ts": "export declare function jsx(...args: unknown[]): unknown;\n",
			}
			if edit.from != "" {
				tree[edit.file] = edit.from
			}
			root := writeTree(t, tree)
			args := []string{"-p", "tsconfig.json", "--noEmit", "--listFilesOnly", "--explainFiles", "--pretty", "false"}
			// A listing tsgo's output fails is one the full pass fails on.
			list := func() string {
				p, err := runListingUncached(context.Background(), root, tsgo, "tsconfig.json", args)
				if err != nil {
					return err.Error()
				}
				p.Diagnostics = nil
				return fmt.Sprintf("%+v\n%+v\n%+v\n%q", p.Listing, p.candidates, p.resolution, p.refused)
			}
			sum := func() (uint64, bool) {
				data, err := os.ReadFile(filepath.Join(root, edit.file))
				if err != nil {
					t.Fatal(err)
				}
				src, err := incremental.Scan(edit.file, data)
				return scanSum(src), err == nil && scanSum(src) != 0
			}
			before := list()
			sumBefore, scannedBefore := sum()
			writeFile(t, filepath.Join(root, edit.file), edit.body)
			after := list()
			sumAfter, scanned := sum()
			listingMoved, sumMoved := before != after, !scannedBefore || !scanned || sumAfter != sumBefore
			switch {
			case listingMoved && !sumMoved:
				t.Errorf("%s changes the listing but not the scan sum:\n--- before\n%s\n--- after\n%s", edit.name, before, after)
			case edit.bodyEdit && (listingMoved || sumMoved):
				t.Errorf("%s moves the listing (%t) or the scan sum (%t), which a body edit must not", edit.name, listingMoved, sumMoved)
			case !edit.bodyEdit && !listingMoved:
				t.Errorf("%s leaves the listing as it was, so it tests nothing here", edit.name)
			}
		})
	}
}

// A replayed record's state leaves out directories whose names this run's walk
// may have read; facts without those reads let a later fast path miss an edit to them.
func TestImportFacts_AReplayKeepsTheWalksOwnReads(t *testing.T) {
	saved := runInputs
	t.Cleanup(func() { runInputs, loadedListings = saved, sync.Map{} })
	loadedListings = sync.Map{}
	root := t.TempDir()
	recorded := importFacts{Version: importFactsVersion, Named: []string{"a/x.js"},
		names: nameReads{Version: importFactsVersion, Paths: []string{"a/package.json"}, Dirs: []string{"a"}}}
	data, err := recorded.encode()
	if err != nil {
		t.Fatal(err)
	}
	replay := func(looked, listed, named string) *importFactsRecorder {
		runInputs = &inputSet{repoRoot: root, named: map[string]bool{filepath.Join(root, named): true}}
		r := newImportFactsRecorder(nil)
		r.lookedUp(looked)
		r.enumerated(listed)
		r.replayed(data)
		return r
	}
	if same := replay("a/package.json", "a", "a/x.js").seal(); !bytes.Equal(same, data) {
		t.Error("a replay whose walk read nothing new changed the recorded facts")
	}
	facts, err := decodeImportFacts(replay("tools/package.json", "tools", "tools/y.js").seal())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"looked-up names", facts.names.Paths, []string{"a/package.json", "tools/package.json"}},
		{"enumerated directories", facts.names.Dirs, []string{"a", "tools"}},
		{"named reads", facts.Named, []string{"a/x.js", "tools/y.js"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, c.got, c.want)
		}
	}
}

// A gained directory whose own path the run looked up, or that sorts after every
// lookup, must be judged without indexing past them.
func TestFastPath_AGainedDirectoryTheRunLookedUpDeclines(t *testing.T) {
	facts := &importFacts{names: nameReads{Paths: []string{"app/a.ts", "tools/thing"}}}
	why := facts.readsNoneOf("/repo", map[string]string{"/repo/tools/thing": ""})
	if why == nil || why.code != declineInputs {
		t.Errorf("a gained directory the run looked up was not declined: %v", why)
	}
	under := &importFacts{names: nameReads{Paths: []string{"app/a.ts", "tools/thing/x.json"}}}
	if why := under.readsNoneOf("/repo", map[string]string{"/repo/tools/thing": ""}); why == nil || why.code != declineInputs {
		t.Errorf("a gained directory holding a path the run looked up was not declined: %v", why)
	}
	if why := facts.readsNoneOf("/repo", map[string]string{"/repo/zz": ""}); why != nil {
		t.Errorf("a gained directory sorting after every lookup was declined: %v", why)
	}
}
