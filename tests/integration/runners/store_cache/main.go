package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mikn/rules_typescript/tests/integration/harness"
)

const (
	store = "node_modules/.pnpm/strip-ansi@7.2.0/node_modules/strip-ansi"
	link  = "node_modules/.pnpm/strip-ansi@7.2.0/node_modules/ansi-regex"
	want  = "../../ansi-regex@6.5.0/node_modules/ansi-regex"
)

var processes = regexp.MustCompile(`(?m)^INFO: \d+ processes: (.*)\.$`)

func main() {
	harness.Run(harness.Config{
		Name:         "store_cache",
		WorkspaceRel: "tests/integration/store_cache/workspace",
		Renames:      map[string]string{"BUILD.bazel.tpl": "BUILD.bazel"},
	}, func(it *harness.IT) {
		first, err := it.BazelLog("build_1.log", "build", "//:"+store)
		if err != nil {
			first.Dump()
			it.Fail("bazel build //:%s exited non-zero: %v",
				store, err)
		}
		requireStore(it)
		it.Pass("the store tree is real files, its dependency a relative link")

		// The harness's .bazelrc carries --disk_cache; expunging leaves it as
		// the only place the tree can come back from.
		it.MustBazel("clean", "--expunge")
		second, err := it.BazelLog("build_2.log", "build", "//:"+store)
		if err != nil {
			second.Dump()
			it.Fail("the rebuild over the disk cache exited non-zero: %v", err)
		}
		kinds := processKinds(second.Text)
		if len(kinds) == 0 {
			second.Dump()
			it.Fail("no process summary in the rebuild's output")
		}
		for _, kind := range kinds {
			cached := strings.HasSuffix(kind, "disk cache hit")
			if !cached && !strings.HasSuffix(kind, "internal") {
				second.Dump()
				it.Fail("the rebuild ran %q: a store tree comes back from "+
					"the disk cache, and its links are internal", kind)
			}
		}
		it.Pass("a fresh output base over the disk cache executes no "+
			"NpmStore: %s", strings.Join(kinds, ", "))
		requireStore(it)
		it.Pass("the restored tree holds files only, the link its relative target")

		runTest(it, "test_nobuild_runfile_links.log", "--nobuild_runfile_links")
		runTest(it, "test_noenable_runfiles.log", "--noenable_runfiles")
		it.MustBazel("clean", "--expunge")
		runTest(it, "test_download_minimal.log", "--remote_download_outputs=minimal")
	})
}

// runTest runs the workspace's ts_test under one runfiles mode: CI's two, and
// the manifest-only layout only --noenable_runfiles gives a local test.
func runTest(it *harness.IT, logName, flag string) {
	log, err := it.BazelLog(logName, "test", "//:strip_ansi_test", flag)
	if err != nil {
		log.Dump()
		it.Fail("bazel test //:strip_ansi_test %s exited non-zero: %v", flag, err)
	}
	it.Pass("//:strip_ansi_test passes under %s", flag)
}

func processKinds(text string) []string {
	m := processes.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	return strings.Split(m[1], ", ")
}

func requireStore(it *harness.IT) {
	tree := it.Bin(store)
	it.RequireFile(filepath.Join(tree, "package.json"),
		"the store tree holds no package.json")
	it.RequireFile(filepath.Join(tree, "index.js"),
		"the store tree holds no index.js")
	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			it.Fail("%s: a symlink inside the store tree", path)
		}
		return nil
	}
	if err := filepath.WalkDir(tree, walk); err != nil {
		it.Fail("walking %s: %v", tree, err)
	}
	target, err := os.Readlink(it.Bin(link))
	if err != nil {
		it.Fail("%s: not a symlink: %v", it.Bin(link), err)
	}
	if target != want {
		it.Fail("%s -> %s, want %s", link, target, want)
	}
	it.RequireFile(filepath.Join(it.Bin(link), "package.json"),
		"the link does not resolve to the ansi-regex tree")
}
