package typescript

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
