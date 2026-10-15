package typescript

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
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
	dirs, err := dirLines(rec.dirs, buildNames, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec.digest, rec.walkDigest = rec.digests(inputLines{dirs, fileLines(rec.files, nil), listingReadLines(rec.reads, nil), rootLines(rec.roots)})
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
