package typescript

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/mikn/rules_typescript/gazelle/incremental"
	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// cachedEntries writes one fresh listing entry per file under a new cache dir and returns each entry's key.
func cachedEntries(t *testing.T, files ...string) map[string]string {
	t.Helper()
	root := t.TempDir()
	names, stats, dir := listingNames, listingStats, listingCacheDir
	listingCacheDir = t.TempDir()
	listingNames, listingStats = newListingNames(), &incremental.StatCache{}
	loadedListings = sync.Map{}
	listingHits.Store(0)
	listingAbsent.Store(0)
	t.Cleanup(func() {
		predecodes.finish()
		listingNames, listingStats, listingCacheDir = names, stats, dir
		loadedListings = sync.Map{}
	})
	keys := map[string]string{}
	for _, file := range files {
		abs := filepath.Join(root, file)
		if err := os.WriteFile(abs, []byte("export {};\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		entry := cachedListing{Listing: explainfiles.Listing{Files: []string{file}, Roots: []string{file}}, Reads: []string{abs}}
		entry.Fingerprint = listingFingerprint(entry.Reads, entry.Dirs)
		sum := sha256.Sum256([]byte(file))
		key := hex.EncodeToString(sum[:])
		writeListing(key, &entry)
		keys[file] = key
	}
	return keys
}

// The walk asks for each listing only when it reaches the directory, so a decode that waited for it ran on the main goroutine.
func TestListingPredecode_ServesAnEntryDecodedBeforeTheWalkAsks(t *testing.T) {
	keys := cachedEntries(t, "a.ts", "b.ts")
	predecodes.start(listingCacheDir, false)
	predecodes.wg.Wait()
	for _, key := range keys {
		if err := os.Remove(filepath.Join(listingCacheDir, key)); err != nil {
			t.Fatal(err)
		}
	}
	for file, key := range keys {
		p, _ := loadListing(listingID{key: key}, "tsconfig.json")
		if p == nil || !slices.Equal(p.Files, []string{file}) {
			t.Fatalf("listing of %s was not served from the decode ahead of the walk: %v", file, p)
		}
	}
	if got := listingHits.Load(); got != int64(len(keys)) {
		t.Fatalf("listing cache counted %d hit(s), want %d", got, len(keys))
	}
}

// -ts_parallel=verify decodes each entry again on the main goroutine and must fail on any difference.
func TestListingPredecode_VerifyFailsWhenAnEntryDiffersFromItsSequentialDecode(t *testing.T) {
	for _, tc := range []struct {
		name           string
		rewrite, fresh bool
		wantErr        bool
	}{
		{name: "unchanged"},
		{name: "rewritten after the decode", rewrite: true, wantErr: true},
		{name: "rewritten after the decode, still fresh", rewrite: true, fresh: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := cachedEntries(t, "a.ts")["a.ts"]
			predecodes.start(listingCacheDir, true)
			predecodes.wg.Wait()
			if tc.rewrite {
				entry := cachedListing{Listing: explainfiles.Listing{Files: []string{"other.ts"}}}
				if tc.fresh {
					old, err := readListing(filepath.Join(listingCacheDir, key))
					if err != nil {
						t.Fatal(err)
					}
					entry.Reads, entry.Dirs, entry.Fingerprint = old.Reads, old.Dirs, old.Fingerprint
				}
				writeListing(key, &entry)
			}
			_, _, err := predecodes.take(key)
			if errors.Is(err, errPredecodeMismatch) != tc.wantErr {
				t.Fatalf("take = %v, want a mismatch: %v", err, tc.wantErr)
			}
		})
	}
}

// A take that reaches a job while its worker decodes it must wait for that decode, not start a second one or miss it.
func TestListingPredecode_TakesRacingTheWorkersEachGetTheirOwnEntry(t *testing.T) {
	files := make([]string, 64)
	for i := range files {
		files[i] = fmt.Sprintf("f%d.ts", i)
	}
	keys := cachedEntries(t, files...)
	predecodes.start(listingCacheDir, false)
	var wg sync.WaitGroup
	for file, key := range keys {
		wg.Go(func() {
			if p, _ := loadListing(listingID{key: key}, "tsconfig.json"); p == nil || !slices.Equal(p.Files, []string{file}) {
				t.Errorf("listing of %s = %v", file, p)
			}
		})
	}
	wg.Wait()
	if hits, absent := listingHits.Load(), listingAbsent.Load(); hits != int64(len(keys)) || absent != 0 {
		t.Fatalf("listing cache counted %d hit(s) and %d absent, want %d and 0", hits, absent, len(keys))
	}
}

// A cache keeps entries for keys no run asks for any more; only the keys the last full run took are decoded ahead.
func TestListingPredecode_DecodesOnlyTheKeysTheLastRunTook(t *testing.T) {
	keys := cachedEntries(t, "live.ts", "orphan.ts")
	predecodes.start(listingCacheDir, false)
	if p, _ := loadListing(listingID{key: keys["live.ts"]}, "tsconfig.json"); p == nil {
		t.Fatal("live listing missing")
	}
	predecodes.finish()
	predecodes.start(listingCacheDir, false)
	predecodes.mu.Lock()
	queued := slices.Sorted(maps.Keys(predecodes.jobs))
	predecodes.mu.Unlock()
	if !slices.Equal(queued, []string{keys["live.ts"]}) {
		t.Fatalf("second run queued %q, want only the live key %q", queued, keys["live.ts"])
	}
}

// The parallel decode must not make BUILD bytes depend on scheduling: five warm runs at GOMAXPROCS 1 and at 30
// each equal a warm run that decodes on the main goroutine, and -ts_parallel=verify passes.
func TestListingPredecode_BuildBytesAreTheSameAtGOMAXPROCS1And30(t *testing.T) {
	requireTsgo(t)
	binary, err := runfiles.Rlocation(os.Getenv("PROTO_GAZELLE"))
	if err != nil {
		t.Fatal(err)
	}
	predecoded := regexp.MustCompile(`listing cache predecode: ([1-9][0-9]* decoded ahead, [0-9]+|[0-9]+ decoded ahead, [1-9][0-9]*) on the walk`)
	warmCache := regexp.MustCompile(`listing cache [1-9][0-9]* hit\(s\), 0 patched, 0 new, 0 stale`)
	for _, tc := range convergeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, cache := t.TempDir(), t.TempDir()
			writeWorkspace(t, root, tc.files)
			authored := buildFileBytes(t, root)
			warm := false
			run := func(procs string, args ...string) map[string]string {
				t.Helper()
				restoreBuildFiles(t, root, authored)
				command := exec.Command(binary, append([]string{"-repo_root=" + root, "-ts_listing_cache=" + cache, "-ts_verbose"}, args...)...)
				command.Dir = root
				command.Env = append(os.Environ(), "BUILD_WORKSPACE_DIRECTORY="+root, "GOMAXPROCS="+procs)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("gazelle %q at GOMAXPROCS=%s: %v\n%s", args, procs, err, output)
				}
				if warm && !warmCache.Match(output) {
					t.Fatalf("gazelle %q at GOMAXPROCS=%s did not take every listing from the cache:\n%s", args, procs, output)
				}
				if warm && !slices.Contains(args, "-ts_parallel=off") && !predecoded.Match(output) {
					t.Fatalf("gazelle %q at GOMAXPROCS=%s did not decode its listings through the predecode:\n%s", args, procs, output)
				}
				return buildFileBytes(t, root)
			}
			run("30", "-ts_parallel=off")
			warm = true
			want := run("30", "-ts_parallel=off")
			for _, procs := range []string{"1", "30"} {
				for i := range 5 {
					if diff := snapshotDiff(want, run(procs)); diff != "" {
						t.Fatalf("warm run %d at GOMAXPROCS=%s differs from the sequential decode: %s", i+1, procs, diff)
					}
				}
			}
			if diff := snapshotDiff(want, run("30", "-ts_parallel=verify")); diff != "" {
				t.Fatalf("-ts_parallel=verify changed BUILD files: %s", diff)
			}
		})
	}
}

// restoreBuildFiles puts back the authored BUILD files and removes the generated ones, leaving every source untouched.
func restoreBuildFiles(t *testing.T, root string, authored map[string]string) {
	t.Helper()
	for rel := range buildFileBytes(t, root) {
		if _, ok := authored[rel]; !ok {
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeWorkspace(t, root, authored)
}
