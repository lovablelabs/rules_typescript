package typescript

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"
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
		(&resolve.Configurer{}).RegisterFlags(nil, "", c)
		tc := defaultTsConfig()
		tc.programs.tsgoFlag, tc.programs.fullWalk = tsgo, fullWalk
		c.Exts[languageName] = tc
		dirInfo := func(rel string) (walk.DirInfo, error) {
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
