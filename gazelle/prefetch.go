package typescript

import (
	"context"
	"errors"
	"maps"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// A listing reads only the source tree, which nothing writes during the walk but the
// projection configs projectionConfigs keeps from the listings that could read them,
// so a prefetched listing is the one the subdirectory's Configure would run, and a
// speculated one the one GenerateRules would for the same roots.
type listingPrefetch struct {
	mu        sync.Mutex
	cond      *sync.Cond
	ctx       context.Context
	cancel    context.CancelCauseFunc
	jobs      map[string]*listingJob
	requested map[string]bool
	// Keyed by speculationKey; a speculation runs ahead of every queued prefetch.
	speculations map[string]*listingJob
	queue        []*listingJob
	workers      int
	running      sync.WaitGroup
	stopped      bool
}

type listingJob struct {
	ctx                 context.Context
	repoRoot, cfg, tsgo string
	roots               []string
	once                sync.Once
	p                   *program
	err                 error
}

// Replaced by tests, which list no real program.
var runCompilerListing = func(ctx context.Context, repoRoot, cfg, tsgo string) (*program, error) {
	return listCompilerProgramContext(ctx, repoRoot, cfg, tsgo, nil)
}

func (j *listingJob) run() {
	j.once.Do(func() {
		if j.roots == nil {
			j.p, j.err = runCompilerListing(j.ctx, j.repoRoot, j.cfg, j.tsgo)
			return
		}
		j.p, j.err = listCompilerProgramContext(j.ctx, j.repoRoot, j.cfg, j.tsgo, nil, j.roots...)
	})
}

var errListingUntaken = errors.New("no listing took it before the walk ended")

var maxPrefetchWorkers = 8

func prefetchWorkers() int {
	return max(1, min(maxPrefetchWorkers, runtime.NumCPU()/2))
}

// Only a complete recursive update configures every subdirectory, so only it prefetches.
func (s *programStore) prefetchSubdirListings(c *config.Config, tc *tsConfig, rel string, dirInfo func(string) (walk.DirInfo, error)) {
	if dirInfo == nil || !s.fullWalk {
		return
	}
	tsgo, err := s.findBinary()
	if err != nil {
		return
	}
	info, err := dirInfo(rel)
	if err != nil {
		return
	}
	for _, sub := range info.Subdirs {
		dir := path.Join(rel, sub)
		if s.listsProgram(c, tc, dir, dirInfo) {
			s.prefetchListing(c.RepoRoot, tsconfigIn(dir), tsgo)
		}
	}
}

// listsProgram is configureTsConfig's decision to list dir's tsconfig.json, made from
// the parent's configuration; a subdirectory whose own directives could change it is never prefetched.
func (s *programStore) listsProgram(c *config.Config, tc *tsConfig, dir string, dirInfo func(string) (walk.DirInfo, error)) bool {
	info, err := dirInfo(dir)
	if err != nil || info.File == nil && tc.updateOnly || info.File != nil && slices.ContainsFunc(info.File.Directives, mayStopListing) || !slices.Contains(info.RegularFiles, "tsconfig.json") {
		return false
	}
	foreign := tc.foreignManifest != ""
	if s.metadataIdentity(c, path.Join(dir, "package.json")) == authoredInput {
		foreign = tc.lock != nil && tc.lock.importers[dir] == nil
	}
	if foreign || s.generatedOutput(s.index, dir, false) {
		return false
	}
	switch s.metadataIdentity(c, tsconfigIn(dir)) {
	case generatedInput, unknownInput:
		return false
	}
	if handWrittenTsConfigIn(filepath.Join(c.RepoRoot, filepath.FromSlash(dir)), c.RepoRoot) == "" {
		return false
	}
	resolved, err := s.resolveCompilerConfig(c, tsconfigIn(dir), dirInfo)
	return resolved != nil && !errors.Is(err, tsconfig.ErrAdmission)
}

// mayStopListing is whether a directive could keep its directory's Configure from
// listing the tsconfig.json there: an exclusion, or a generation mode.
func mayStopListing(d rule.Directive) bool {
	return d.Key == "exclude" || d.Key == "generation_mode"
}

func (s *programStore) prefetchListing(repoRoot, cfg, tsgo string) {
	f := s.listingPool()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped || f.requested[cfg] {
		return
	}
	f.requested[cfg] = true
	job := &listingJob{ctx: f.ctx, repoRoot: repoRoot, cfg: cfg, tsgo: tsgo}
	f.jobs[cfg] = job
	f.queue = append(f.queue, job)
	f.wake()
}

func (s *programStore) listingPool() *listingPrefetch {
	if s.prefetch == nil {
		ctx, cancel := context.WithCancelCause(context.Background())
		s.prefetch = &listingPrefetch{ctx: ctx, cancel: cancel, jobs: map[string]*listingJob{}, requested: map[string]bool{}, speculations: map[string]*listingJob{}}
		s.prefetch.cond = sync.NewCond(&s.prefetch.mu)
	}
	return s.prefetch
}

// wake starts a worker while there are fewer than the cap; f.mu is held.
func (f *listingPrefetch) wake() {
	if f.workers < prefetchWorkers() {
		f.workers++
		f.running.Go(f.work)
	}
	f.cond.Signal()
}

func speculationKey(repoRoot, cfg, tsgo string, roots []string) string {
	return strings.Join(append([]string{repoRoot, cfg, tsgo}, roots...), "\x00")
}

// speculate starts listing cfg over roots, which takeSpeculation hands to the
// first listing asked for with the same roots.
func (s *programStore) speculate(repoRoot, cfg, tsgo string, roots []string) {
	f := s.listingPool()
	f.mu.Lock()
	defer f.mu.Unlock()
	key := speculationKey(repoRoot, cfg, tsgo, roots)
	if f.stopped || f.speculations[key] != nil {
		return
	}
	job := &listingJob{ctx: f.ctx, repoRoot: repoRoot, cfg: cfg, tsgo: tsgo, roots: roots}
	f.speculations[key] = job
	f.queue = slices.Insert(f.queue, 0, job)
	f.wake()
	s.say("%s: listing %d roots ahead of the walk", cfg, len(roots))
}

// takeSpeculation is the speculated listing of cfg over exactly roots, if one was started.
func (s *programStore) takeSpeculation(repoRoot, cfg, tsgo string, roots []string) (*program, bool, error) {
	f := s.prefetch
	if f == nil {
		return nil, false, nil
	}
	key := speculationKey(repoRoot, cfg, tsgo, roots)
	f.mu.Lock()
	job := f.speculations[key]
	delete(f.speculations, key)
	f.mu.Unlock()
	if job == nil {
		return nil, false, nil
	}
	job.run()
	return job.p, true, job.err
}

// live is whether job is still wanted: no listing has taken it.
func (f *listingPrefetch) live(job *listingJob) bool {
	if job.roots == nil {
		return f.jobs[job.cfg] == job
	}
	return f.speculations[speculationKey(job.repoRoot, job.cfg, job.tsgo, job.roots)] == job
}

func (f *listingPrefetch) work() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		for len(f.queue) == 0 && !f.stopped {
			f.cond.Wait()
		}
		if f.stopped {
			return
		}
		job := f.queue[0]
		f.queue = f.queue[1:]
		if !f.live(job) {
			continue
		}
		f.mu.Unlock()
		job.run()
		f.mu.Lock()
	}
}

// compilerListing is listCompilerProgram without exclusions, from the prefetch when it holds the listing.
func (s *programStore) compilerListing(repoRoot, cfg, tsgo string) (*program, error) {
	if f := s.prefetch; f != nil {
		f.mu.Lock()
		job := f.jobs[cfg]
		delete(f.jobs, cfg)
		f.mu.Unlock()
		if job != nil && job.repoRoot == repoRoot && job.tsgo == tsgo {
			job.run()
			return job.p, job.err
		}
	}
	return runCompilerListing(context.Background(), repoRoot, cfg, tsgo)
}

// stopPrefetch drops every listing no directory took and kills those still running.
func (s *programStore) stopPrefetch() {
	f := s.prefetch
	if f == nil {
		return
	}
	f.mu.Lock()
	f.stopped = true
	f.cancel(errListingUntaken)
	clear(f.jobs)
	untaken := slices.Collect(maps.Values(f.speculations))
	clear(f.speculations)
	f.queue = nil
	f.cond.Broadcast()
	f.mu.Unlock()
	for _, job := range untaken {
		s.say("%s: no listing asked for the %d roots listed ahead of the walk", job.cfg, len(job.roots))
	}
	// A worker still listing reads the run's caches, which the run tears down next.
	f.running.Wait()
}
