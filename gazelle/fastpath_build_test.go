package typescript

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// buildEditTree has web importing base and util, which the walk reaches
// first, zone importing base, and early importing zone, which it reaches later.
func buildEditTree() map[string]string {
	config := `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]},"include":["*.ts"]}`
	return map[string]string{
		"MODULE.bazel":                 "module(name = \"build_edit\")\n",
		".bazelignore":                 "node_modules\n",
		"node_modules/.pnpm/lock.yaml": "lockfileVersion: '9.0'\n",
		"package.json":                 `{"type":"module"}`,
		"base/tsconfig.json":           config,
		"base/x.ts":                    "export const x = 1;\n",
		"early/tsconfig.json":          config,
		"early/e.ts":                   "import { z } from '../zone/z';\nexport const e = z;\n",
		"util/tsconfig.json":           config,
		"util/u.ts":                    "export const u = 2;\n",
		"web/tsconfig.json":            config,
		"web/a.ts":                     "import { x } from '../base/x';\nimport { u } from '../util/u';\nexport const a = x + u;\n",
		"web/b.ts":                     "export const b = 3;\n",
		"web/a.test.ts":                "import { a } from './a';\nexport const t = a;\n",
		"zone/tsconfig.json":           config,
		"zone/z.ts":                    "import { x } from '../base/x';\nexport const z = x;\n",
	}
}

// replaceIn edits a BUILD file the primed run wrote, failing when the text to
// replace is not there, so a case never passes on an edit it did not make.
func replaceIn(rel, old, replacement string) func(t *testing.T, root string) {
	return func(t *testing.T, root string) {
		t.Helper()
		file := filepath.Join(root, rel)
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), old) {
			t.Fatalf("%s holds no %q:\n%s", rel, old, data)
		}
		writeFile(t, file, strings.Replace(string(data), old, replacement, 1))
	}
}

func bothEdits(edits ...func(t *testing.T, root string)) func(t *testing.T, root string) {
	return func(t *testing.T, root string) {
		for _, edit := range edits {
			edit(t, root)
		}
	}
}

const webDeps = "    deps = [\n        \"//base\",\n        \"//util\",\n    ],\n"

// A hand edit to generated deps, which the merge puts back, is written back
// byte for byte as the full pass writes it, without the walk.
func TestFastPath_ABuildEditGazelleRevertsIsWrittenBackWithoutAWalk(t *testing.T) {
	requireTsgo(t)
	keptBase := map[string]string{"web/BUILD.bazel": "ts_compile(\n    name = \"web\",\n    deps = [\n        \"//base\",  # keep\n    ],\n)\n"}
	for _, edit := range []struct {
		name string
		tree map[string]string
		do   func(t *testing.T, root string)
	}{
		{"a generated deps attribute deleted", nil, replaceIn("web/BUILD.bazel", webDeps, "")},
		{"a generated dep deleted beside a kept one", keptBase, replaceIn("web/BUILD.bazel", "        \"//util\",\n", "")},
		{"one generated dep deleted", nil, replaceIn("web/BUILD.bazel", "        \"//util\",\n", "")},
		{"generated deps reordered", nil, replaceIn("web/BUILD.bazel", webDeps, "    deps = [\n        \"//util\",\n        \"//base\",\n    ],\n")},
		{"a test's generated dep deleted", nil, replaceIn("web/BUILD.bazel", "    deps = [\":web\"],\n", "")},
		{"generated deps deleted in two BUILD files", nil, bothEdits(
			replaceIn("web/BUILD.bazel", "        \"//util\",\n", ""),
			replaceIn("zone/BUILD.bazel", "    deps = [\"//base\"],\n", ""),
		)},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			tree := buildEditTree()
			maps.Copy(tree, edit.tree)
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache)
			edit.do(t, root)
			// Past the racy window, the write-back carries the record forward.
			time.Sleep(pastRacyWindow)
			same := requireAsCacheless(t, root, edit.name)
			if out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache); !slices.Equal(paths, []string{"fast path build edit"}) {
				t.Errorf("%s: the run took %q, want the BUILD file written back and no full pass:\n%s", edit.name, paths, out)
			}
			same()
			if out, paths := tracedGazelle(t, root, "-ts_listing_cache="+cache); !slices.Equal(paths, []string{"run record"}) {
				t.Errorf("%s: the run after the write-back took %q, want the record's replay:\n%s", edit.name, paths, out)
			}
		})
	}
}

// An edit the merge may not revert, or one the walk reads before the merge,
// declines with its reason and runs the full pass.
func TestFastPath_ABuildEditTheMergeMayKeepRunsTheFullPass(t *testing.T) {
	requireTsgo(t)
	// zone, which early's crawl reaches first, imports yard, which the walk reaches after early.
	crawledFirst := map[string]string{
		"yard/tsconfig.json": buildEditTree()["base/tsconfig.json"],
		"yard/y.ts":          "export const y = 1;\n",
		"zone/z.ts":          "import { x } from '../base/x';\nimport { y } from '../yard/y';\nexport const z = x + y;\n",
	}
	for _, edit := range []struct {
		name, code string
		tree       map[string]string
		do         func(t *testing.T, root string)
	}{
		{name: "a keep comment on a generated dep", code: declineBuildKeep, do: replaceIn("web/BUILD.bazel", "\"//util\",\n", "\"//util\",  # keep\n")},
		{name: "a directive added", code: declineBuildDirective, do: replaceIn("web/BUILD.bazel", "load(", "# gazelle:resolve typescript nowhere //nowhere\n\nload(")},
		{name: "an attribute Gazelle does not manage", code: declineBuildAttr, do: replaceIn("web/BUILD.bazel", "    tsconfig = \":tsconfig\",\n    visibility", "    testonly = True,\n    tsconfig = \":tsconfig\",\n    visibility")},
		{name: "a managed attribute other than deps", code: declineBuildAttr, do: replaceIn("web/BUILD.bazel", "\"//visibility:public\"],\n    deps", "\"//visibility:private\"],\n    deps")},
		{name: "a dep added by hand", code: declineBuildAttr, do: replaceIn("web/BUILD.bazel", "        \"//util\",\n", "        \"//util\",\n        \"//zone\",\n")},
		{name: "a rule added by hand", code: declineBuildRules, do: replaceIn("web/BUILD.bazel", "ts_config(", "filegroup(\n    name = \"extra\",\n    srcs = [\"b.ts\"],\n)\n\nts_config(")},
		{name: "a rule renamed", code: declineBuildRename, do: replaceIn("web/BUILD.bazel", "name = \"web_test\"", "name = \"web_tests\"")},
		{name: "a load edited", code: declineBuildLoads, do: replaceIn("web/BUILD.bazel", ", \"ts_test\")", ")")},
		{name: "a deleted dep whose crawl loads a package the walk reaches later", code: declineBuildCrawl, do: replaceIn("early/BUILD.bazel", "    deps = [\"//zone\"],\n", "")},
		{name: "a deleted dep of a rule another package's crawl reached first", code: declineBuildCrawl, tree: crawledFirst, do: replaceIn("zone/BUILD.bazel", "        \"//yard\",\n", "")},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			tree := buildEditTree()
			maps.Copy(tree, edit.tree)
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache, "-ts_verbose")
			edit.do(t, root)
			same := requireAsCacheless(t, root, edit.name)
			if out := recordGazelle(t, root, cache); !strings.Contains(out, "fast path: declined ("+edit.code+")") {
				t.Errorf("%s: want a %s decline in the log:\n%s", edit.name, edit.code, out)
			}
			same()
		})
	}
}

// Diff mode prints what the full pass would write; the write-back has nothing to print.
func TestFastPath_ABuildEditInDiffModeRunsTheFullPass(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, buildEditTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	primeFastPath(t, root, cache, "-ts_verbose", "-mode=diff")
	replaceIn("web/BUILD.bazel", "        \"//util\",\n", "")(t, root)
	out, _ := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_verbose", "-mode=diff")
	if !strings.Contains(out, "fast path: declined ("+declineMode+")") || !strings.Contains(out, "+        \"//util\",") {
		t.Errorf("a BUILD edit in diff mode did not run the full pass and print its diff:\n%s", out)
	}
}

// -ts_fastpath=verify runs the full pass on the edited tree: an edit the proof
// accepts is one that pass writes back as recorded.
func TestFastPath_VerifyPassesABuildEditTheMergeReverts(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, buildEditTree())
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	replaceIn("web/BUILD.bazel", webDeps, "")(t, root)
	same := requireAsCacheless(t, root, "deps deleted")
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify")
	if err != nil || !strings.Contains(out, "fast path: verified") {
		t.Errorf("the deleted deps were not written back and verified: %v\n%s", err, out)
	}
	same()
}

// A snapshot claiming a hand-written rule was generated makes the proof accept a
// deletion the full pass keeps; only a pass over the edited tree can tell.
func TestFastPath_VerifyCatchesAWriteBackOfAnEditTheMergeKeeps(t *testing.T) {
	requireTsgo(t)
	tree := buildEditTree()
	tree["web/BUILD.bazel"] = "ts_test(\n    name = \"hand_test\",\n    srcs = [\"b.ts\"],\n    deps = [\":web\"],\n)\n"
	root := writeTree(t, tree)
	cache := t.TempDir()
	primeFastPath(t, root, cache)
	records, err := filepath.Glob(filepath.Join(cache, runRecordName+"-*"))
	if err != nil || len(records) != 1 {
		t.Fatalf("want one run record, have %v (%v)", records, err)
	}
	snap := loadBuildSnapshot(buildSnapshotFile(records[0]))
	if snap == nil {
		t.Fatalf("no build snapshot beside %s", records[0])
	}
	file := filepath.Join(root, "web/BUILD.bazel")
	planted := false
	for i := range snap.Packages {
		if snap.Packages[i].File == file {
			snap.Packages[i].Rules = append(snap.Packages[i].Rules, snapshotRule{Kind: "ts_test", Name: "hand_test"})
			planted = true
		}
	}
	if !planted {
		t.Fatalf("the snapshot holds no %s", file)
	}
	data, err := snap.encode()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, buildSnapshotFile(records[0]), string(data))
	replaceIn("web/BUILD.bazel", "    srcs = [\"b.ts\"],\n    deps = [\":web\"],\n", "    srcs = [\"b.ts\"],\n")(t, root)
	edited, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tsGazelle(t, root, "-ts_listing_cache="+cache, "-ts_fastpath=verify")
	if err == nil || !strings.Contains(out, "fast path: the full pass leaves") {
		t.Errorf("verify passed a write-back the full pass does not make: %v\n%s", err, out)
	}
	if after, err := os.ReadFile(file); err != nil || !bytes.Equal(after, edited) {
		t.Errorf("verify left web/BUILD.bazel otherwise than the full pass writes it (%v):\n%s", err, after)
	}
	if _, err := os.Stat(records[0]); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("verify kept the record it failed: %v", err)
	}
}

// Incomplete facts can miss the Resolve phase's lookups, and the BUILD-edit
// path reads directory shapes before it reads whether the facts are complete.
func TestFastPath_IncompleteFactsKeepNoNameReads(t *testing.T) {
	data, err := (&importFacts{Version: importFactsVersion, Incomplete: "replayed", names: nameReads{Paths: []string{"app/logo.svg"}}}).encode()
	if err != nil {
		t.Fatal(err)
	}
	if names := decodeNameReads(data); names != nil {
		t.Errorf("incomplete facts decode name reads %v, so a file only Resolve looked up could drop out of a directory's shape", names.Paths)
	}
	if facts, err := decodeImportFacts(data); err != nil || facts.Incomplete == "" {
		t.Errorf("incomplete facts no longer decode as incomplete: %v", err)
	}
}
