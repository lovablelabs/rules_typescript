package typescript

import (
	"context"
	"flag"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"

	"github.com/mikn/rules_typescript/gazelle/incremental"
)

type listingCall struct {
	cfg, tsgo   string
	cancellable bool
}

// fakeListings replaces the compiler with block, recording every listing it is asked for.
func fakeListings(t *testing.T, workers int, block func(ctx context.Context, cfg string)) (calls func() []listingCall) {
	t.Helper()
	var mu sync.Mutex
	var recorded []listingCall
	previous, previousWorkers := runCompilerListing, maxPrefetchWorkers
	runCompilerListing = func(ctx context.Context, _, cfg, tsgo string) (*program, error) {
		mu.Lock()
		recorded = append(recorded, listingCall{cfg, tsgo, ctx.Done() != nil})
		mu.Unlock()
		if block != nil {
			block(ctx, cfg)
		}
		return &program{config: cfg + "@" + tsgo}, nil
	}
	maxPrefetchWorkers = workers
	t.Cleanup(func() { runCompilerListing, maxPrefetchWorkers = previous, previousWorkers })
	return func() []listingCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(recorded)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestPrefetch_TakingAQueuedListingRunsItOnceWithoutWaitingForTheBusyWorker(t *testing.T) {
	release := make(chan struct{})
	calls := fakeListings(t, 1, func(_ context.Context, cfg string) {
		if cfg == "a/tsconfig.json" {
			<-release
		}
	})
	s := newProgramStore()
	s.prefetchListing("/repo", "a/tsconfig.json", "/tsgo")
	waitFor(t, "the worker to start a", func() bool { return len(calls()) == 1 })
	s.prefetchListing("/repo", "b/tsconfig.json", "/tsgo")
	p, err := s.compilerListing("/repo", "b/tsconfig.json", "/tsgo")
	if err != nil || p.config != "b/tsconfig.json@/tsgo" {
		t.Fatalf("queued listing = %v, %v", p, err)
	}
	close(release)
	s.stopPrefetch()
	time.Sleep(10 * time.Millisecond)
	got := calls()
	if len(got) != 2 || got[1].cfg != "b/tsconfig.json" {
		t.Fatalf("listings = %v, want a once and b once", got)
	}
}

func TestPrefetch_TakingARunningListingWaitsForTheWorkersResult(t *testing.T) {
	release := make(chan struct{})
	calls := fakeListings(t, 1, func(context.Context, string) { <-release })
	s := newProgramStore()
	s.prefetchListing("/repo", "a/tsconfig.json", "/tsgo")
	waitFor(t, "the worker to start a", func() bool { return len(calls()) == 1 })
	taken := make(chan *program)
	go func() {
		p, _ := s.compilerListing("/repo", "a/tsconfig.json", "/tsgo")
		taken <- p
	}()
	select {
	case p := <-taken:
		t.Fatalf("took %v before the worker finished", p)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if p := <-taken; p == nil || p.config != "a/tsconfig.json@/tsgo" {
		t.Fatalf("took %v", p)
	}
	if got := calls(); len(got) != 1 {
		t.Fatalf("listings = %v, want the worker's alone", got)
	}
}

func TestPrefetch_AListingForAnotherTsgoIsNotReusedButListedSerially(t *testing.T) {
	calls := fakeListings(t, 1, nil)
	s := newProgramStore()
	s.prefetchListing("/repo", "a/tsconfig.json", "/old-tsgo")
	p, err := s.compilerListing("/repo", "a/tsconfig.json", "/tsgo")
	if err != nil || p.config != "a/tsconfig.json@/tsgo" {
		t.Fatalf("listing = %v, %v; want one by /tsgo", p, err)
	}
	got := calls()
	if !slices.Contains(got, listingCall{"a/tsconfig.json", "/tsgo", false}) {
		t.Fatalf("listings = %v, want a serial one by /tsgo", got)
	}
}

func TestPrefetch_StopCancelsTheRunningListingAndDropsQueuedOnes(t *testing.T) {
	cancelled := make(chan struct{})
	calls := fakeListings(t, 1, func(ctx context.Context, cfg string) {
		if cfg == "a/tsconfig.json" && ctx.Done() != nil {
			<-ctx.Done()
			close(cancelled)
		}
	})
	s := newProgramStore()
	s.prefetchListing("/repo", "a/tsconfig.json", "/tsgo")
	waitFor(t, "the worker to start a", func() bool { return len(calls()) == 1 })
	s.prefetchListing("/repo", "b/tsconfig.json", "/tsgo")
	s.stopPrefetch()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("stopPrefetch left the running listing alive")
	}
	s.prefetchListing("/repo", "c/tsconfig.json", "/tsgo")
	if _, err := s.compilerListing("/repo", "b/tsconfig.json", "/tsgo"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	want := []listingCall{{"a/tsconfig.json", "/tsgo", true}, {"b/tsconfig.json", "/tsgo", false}}
	if got := calls(); !slices.Equal(got, want) {
		t.Fatalf("listings = %v, want %v", got, want)
	}
}

// diskDirInfo is walk.GetDirInfo read straight from root's tree.
func diskDirInfo(root string) func(rel string) (walk.DirInfo, error) {
	return func(rel string) (walk.DirInfo, error) {
		var info walk.DirInfo
		entries, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			return info, err
		}
		for _, e := range entries {
			if e.IsDir() {
				info.Subdirs = append(info.Subdirs, e.Name())
			} else if e.Name() == "BUILD.bazel" {
				info.File, err = rule.LoadFile(filepath.Join(root, rel, e.Name()), rel)
				if err != nil {
					return info, err
				}
			} else {
				info.RegularFiles = append(info.RegularFiles, e.Name())
			}
		}
		return info, nil
	}
}

func TestPrefetch_ASubdirectoryConfigureWouldNotListIsNeverListed(t *testing.T) {
	for _, fullWalk := range []bool{true, false} {
		root := t.TempDir()
		writeWorkspace(t, root, map[string]string{
			"pnpm-lock.yaml":             "lockfileVersion: '9.0'\nimporters:\n  .: {}\n  listed: {}\n",
			"node_modules/.modules.yaml": "layoutVersion: 5\n",
			"listed/package.json":        `{"name":"listed"}`,
			"listed/tsconfig.json":       includeTs,
			"foreign/package.json":       `{"name":"foreign"}`,
			"foreign/tsconfig.json":      includeTs,
			"generated/tsconfig.json":    `{"_comment":"` + generatedTsConfigMarker + `","include":["*.ts"]}`,
			"unreadable/tsconfig.json":   `{"include":`,
			"directive/BUILD.bazel":      "# gazelle:exclude x.ts\n",
			"directive/tsconfig.json":    includeTs,
			"mode/BUILD.bazel":           "# gazelle:generation_mode update_only\n",
			"mode/tsconfig.json":         includeTs,
			"admission/BUILD.bazel":      `genrule(name = "base", outs = ["base.json"], cmd = "")` + "\n",
			"admission/base.json":        includeTs,
			"admission/tsconfig.json":    `{"extends":"./base.json"}`,
			"output/BUILD.bazel":         `ts_codegen(name = "tree", out_dir = "tree")` + "\n",
			"output/tree/tsconfig.json":  includeTs,
			"nothing/a.ts":               "export {};\n",
		})
		tsgo := filepath.Join(root, "tsgo")
		writeFile(t, tsgo, "")
		calls := fakeListings(t, 1, nil)
		c := config.New()
		c.RepoRoot = root
		seedResolveConfig(c)
		tc := defaultTsConfig()
		tc.programs.tsgoFlag, tc.programs.fullWalk = tsgo, fullWalk
		c.Exts[languageName] = tc
		dirInfo := diskDirInfo(root)
		configureTsConfig(c, "", nil, dirInfo)
		output, err := dirInfo("output")
		if err != nil {
			t.Fatal(err)
		}
		configureTsConfig(c, "output", output.File, dirInfo)
		var requested []string
		if f := tc.programs.prefetch; f != nil {
			for cfg := range f.requested {
				requested = append(requested, path.Dir(cfg))
			}
			tc.programs.stopPrefetch()
		}
		slices.Sort(requested)
		want := []string{"listed"}
		if !fullWalk {
			want = nil
		}
		if !slices.Equal(requested, want) {
			t.Errorf("full walk %v: prefetched %v, want %v (listings %v)", fullWalk, requested, want, calls())
		}
	}
}

type listingSpan struct {
	projection bool
	start, end time.Time
}

// walkWithListingCache is one full recursive run under -ts_listing_cache, with
// every tsgo listing slowed by delay and recorded.
func walkWithListingCache(t *testing.T, root string, delay time.Duration) []listingSpan {
	t.Helper()
	requireTsgo(t)
	var mu sync.Mutex
	var spans []listingSpan
	uncachedListing = func(ctx context.Context, repoRoot, tsgo, subject string, args []string) (*program, error) {
		span := listingSpan{projection: slices.ContainsFunc(args, isProjection), start: time.Now()}
		time.Sleep(delay)
		p, err := runListingUncached(ctx, repoRoot, tsgo, subject, args)
		span.end = time.Now()
		mu.Lock()
		spans = append(spans, span)
		mu.Unlock()
		return p, err
	}
	names, stats := listingNames, listingStats
	listingNames, listingStats = newListingNames(), &incremental.StatCache{}
	sourceSpecs, loadedListings = sync.Map{}, sync.Map{}
	t.Cleanup(func() {
		listingCacheDir, uncachedListing = "", runListingUncached
		listingNames, listingStats = names, stats
		sourceSpecs, loadedListings = sync.Map{}, sync.Map{}
	})
	c := &config.Config{RepoRoot: root, WorkDir: root, Strict: true, Exts: map[string]any{}}
	lang := &tsLang{}
	cexts := []config.Configurer{&walk.Configurer{}, &resolve.Configurer{}, lang}
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.Bool("r", true, "")
	for _, ext := range cexts {
		ext.RegisterFlags(flags, "update", c)
	}
	if err := flags.Parse([]string{"-ts_listing_cache=" + t.TempDir(), "-ts_run_record=off", "-ts_resolve_record=off", "-ts_fastpath=off"}); err != nil {
		t.Fatal(err)
	}
	for _, ext := range cexts {
		if err := ext.CheckFlags(flags, c); err != nil {
			t.Fatal(err)
		}
	}
	captureLog(t, func() {
		lang.Before(context.Background())
		walk.Walk(c, cexts, []string{root}, walk.VisitAllUpdateSubdirsMode, func(dir, rel string, c *config.Config, update bool, f *rule.File, subdirs, regularFiles, genFiles []string) {
			if update {
				lang.GenerateRules(language.GenerateArgs{
					Config: c, Dir: dir, Rel: rel, File: f,
					Subdirs: subdirs, RegularFiles: regularFiles, GenFiles: genFiles,
				})
			}
		})
		lang.DoneGeneratingRules()
	})
	pendingWrites.Wait()
	return spans
}

// A cold run listed a large program three times over, each listing waiting for
// the one before: the tsconfig's own, then its kept sources, then the sources
// with each declaration's JavaScript twin. The last two need only the first.
func TestPrefetch_ListingsThatNeedOnlyTheConfigsOwnRunTogether(t *testing.T) {
	root := writeTree(t, map[string]string{
		"MODULE.bazel": "module(name = \"cold\")\n",
		"app/BUILD.bazel": `ts_compile(
    name = "app",
    srcs = [
        "tool.ts",  # keep
    ],
)
`,
		"app/tsconfig.json":     `{"compilerOptions":{"module":"esnext","moduleResolution":"bundler","types":[]},"include":["src/**/*"]}`,
		"app/src/a.ts":          "export const a = 1;\n",
		"app/src/catalog.d.mts": "export declare const c: number;\n",
		"app/src/catalog.mjs":   "export const c = 1;\n",
		"app/tool.ts":           "export const tool = 1;\n",
	})
	const delay = 300 * time.Millisecond
	var projections []listingSpan
	for _, span := range walkWithListingCache(t, root, delay) {
		if span.projection {
			projections = append(projections, span)
		}
	}
	if len(projections) != 2 {
		t.Fatalf("listed %d projections of app/tsconfig.json, want 2: its kept sources, and those with catalog.mjs", len(projections))
	}
	first, second := projections[0], projections[1]
	if first.start.After(second.start) {
		first, second = second, first
	}
	if !second.start.Before(first.end) {
		t.Errorf("the projection listings ran one after another (the second started %v after the first ended), want them overlapping", second.start.Sub(first.end))
	}
}

// A large program's BUILD file had a directive, so its listing waited for the
// walk to reach it when its parent's Configure could have started it.
func TestPrefetch_ASubdirectoryWhoseDirectivesKeepItsListingIsPrefetched(t *testing.T) {
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		"app/BUILD.bazel":   "# gazelle:lang typescript\n# gazelle:resolve typescript x //x\n",
		"app/tsconfig.json": includeTs,
	})
	tsgo := filepath.Join(root, "tsgo")
	writeFile(t, tsgo, "")
	fakeListings(t, 1, nil)
	c := config.New()
	c.RepoRoot = root
	seedResolveConfig(c)
	tc := defaultTsConfig()
	tc.programs.tsgoFlag, tc.programs.fullWalk = tsgo, true
	c.Exts[languageName] = tc
	configureTsConfig(c, "", nil, diskDirInfo(root))
	t.Cleanup(tc.programs.stopPrefetch)
	if f := tc.programs.prefetch; f == nil || !f.requested["app/tsconfig.json"] {
		t.Fatal("app/tsconfig.json was not prefetched from its parent's Configure")
	}
}
