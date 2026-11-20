package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/mikn/rules_typescript/tests/integration/harness"
)

const (
	store = "node_modules/.pnpm/strip-ansi@7.2.0/node_modules/strip-ansi"
	link  = "node_modules/.pnpm/strip-ansi@7.2.0/node_modules/ansi-regex"
	want  = "../../ansi-regex@6.5.0/node_modules/ansi-regex"
)

var (
	tsgo   = "tsgo_" + runtime.GOOS + "_" + runtime.GOARCH
	cached = []string{"npm__strip-ansi__7_2_0", "npm__ansi-regex__6_5_0", tsgo, "pnpm", "tools_fixture"}
)

var processes = regexp.MustCompile(`(?m)^INFO: \d+ processes: (.*)\.$`)

func main() {
	harness.Run(harness.Config{
		Name:         "store_cache",
		WorkspaceRel: "tests/integration/store_cache/workspace",
		Renames:      map[string]string{"BUILD.bazel.tpl": "BUILD.bazel"},
	}, func(it *harness.IT) {
		writeTools(it)
		first, err := it.BazelLog("build_1.log", "build", "//:"+store)
		if err != nil {
			first.Dump()
			it.Fail("bazel build //:%s exited non-zero: %v",
				store, err)
		}
		requireStore(it)
		it.Pass("the store tree is real files, its dependency a relative link")
		contents := repoContentsCache(it)
		linked := cachedRepos(it, contents)
		it.Pass("the npm spokes, tsgo, pnpm and tools are linked from the repo contents cache")
		requireUnpinnedUncached(it)
		it.Pass("a tools_toolchain_repo with an empty integrity is not in the repo contents cache")

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
		if again := cachedRepos(it, contents); !maps.Equal(again, linked) {
			it.Fail("the fresh output base refetched: %v, want %v", again, linked)
		}
		it.Pass("the fresh output base links the same repo contents cache entries")

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

func repoContentsCache(it *harness.IT) string {
	repositoryCache := strings.TrimSpace(it.BazelStdout("info", "repository_cache"))
	resolved, err := filepath.EvalSymlinks(filepath.Join(repositoryCache, "contents"))
	if err != nil {
		it.Fail("no repo contents cache under %s: %v", repositoryCache, err)
	}
	return resolved
}

// The store build fetches no toolchain, and the pinned tools release is not
// fetched in tests: tools_fixture.bzl points tools_toolchain_repo at this.
func writeTools(it *harness.IT) {
	out, err := os.Create(it.Path("tools.tar.gz"))
	if err != nil {
		it.Fail("cannot create tools.tar.gz: %v", err)
	}
	gz := gzip.NewWriter(out)
	archive := tar.NewWriter(gz)
	for _, binary := range []string{"tsaction", "ts_launcher", "lcov_merger", "copy_to_workspace"} {
		header := &tar.Header{Name: "tools/" + binary, Mode: 0o755, Typeflag: tar.TypeReg}
		if err := archive.WriteHeader(header); err != nil {
			it.Fail("cannot add %s to tools.tar.gz: %v", binary, err)
		}
	}
	for _, closer := range []interface{ Close() error }{archive, gz, out} {
		if err := closer.Close(); err != nil {
			it.Fail("cannot finish tools.tar.gz: %v", err)
		}
	}
	sum := sha256.Sum256([]byte(it.Read(it.Path("tools.tar.gz"))))
	it.Write(it.Path("tools.integrity"), "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
}

// Bazel links a repository from the repo contents cache only when its rule
// returned repo_metadata(reproducible = True); otherwise it is a directory.
func cachedRepos(it *harness.IT, contents string) map[string]string {
	it.MustBazel("fetch", "--repo=@@rules_typescript++ts+"+tsgo,
		"--repo=@@rules_typescript++npm+pnpm", "--repo=@tools_fixture")
	external := filepath.Join(it.OutputBase, "external")
	entries := map[string]string{}
	for _, spoke := range cached {
		names := it.Glob(external, "", "+"+spoke)
		if len(names) != 1 {
			it.Fail("want one %s repository under %s, got %v", spoke, external, names)
		}
		path := filepath.Join(external, names[0])
		if _, err := os.Readlink(path); err != nil {
			it.Fail("%s is not a link into the repo contents cache: %v", path, err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			it.Fail("resolving %s: %v", path, err)
		}
		if !strings.HasPrefix(resolved, contents+string(filepath.Separator)) {
			it.Fail("%s -> %s, want a path under %s", path, resolved, contents)
		}
		entries[spoke] = resolved
	}
	return entries
}

// attr.string(mandatory = True) accepts "", and an unpinned download is not
// reproducible.
func requireUnpinnedUncached(it *harness.IT) {
	it.MustBazel("fetch", "--repo=@tools_unpinned")
	external := filepath.Join(it.OutputBase, "external")
	names := it.Glob(external, "", "+tools_unpinned")
	if len(names) != 1 {
		it.Fail("want one tools_unpinned repository under %s, got %v", external, names)
	}
	info, err := os.Lstat(filepath.Join(external, names[0]))
	if err != nil {
		it.Fail("cannot stat %s: %v", names[0], err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		it.Fail("%s has no integrity and is a link into the repo contents cache", names[0])
	}
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
