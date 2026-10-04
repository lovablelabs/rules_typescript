package typescript

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/walk"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// A listing reads only the source tree, which nothing writes during the walk but the
// projection configs projectionConfigs excludes, so a prefetched listing is the one
// the subdirectory's Configure would run.
type listingPrefetch struct {
	mu        sync.Mutex
	cond      *sync.Cond
	ctx       context.Context
	cancel    context.CancelFunc
	jobs      map[string]*listingJob
	requested map[string]bool
	queue     []*listingJob
	workers   int
	stopped   bool
}

type listingJob struct {
	ctx                 context.Context
	repoRoot, cfg, tsgo string
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
		projectionConfigs.RLock()
		defer projectionConfigs.RUnlock()
		j.p, j.err = runCompilerListing(j.ctx, j.repoRoot, j.cfg, j.tsgo)
	})
}

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
// the parent's configuration; a subdirectory with directives of its own is never prefetched.
func (s *programStore) listsProgram(c *config.Config, tc *tsConfig, dir string, dirInfo func(string) (walk.DirInfo, error)) bool {
	info, err := dirInfo(dir)
	if err != nil || info.File == nil && tc.updateOnly || info.File != nil && len(info.File.Directives) > 0 || !slices.Contains(info.RegularFiles, "tsconfig.json") {
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

func (s *programStore) prefetchListing(repoRoot, cfg, tsgo string) {
	if s.prefetch == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.prefetch = &listingPrefetch{ctx: ctx, cancel: cancel, jobs: map[string]*listingJob{}, requested: map[string]bool{}}
		s.prefetch.cond = sync.NewCond(&s.prefetch.mu)
	}
	f := s.prefetch
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped || f.requested[cfg] {
		return
	}
	f.requested[cfg] = true
	job := &listingJob{ctx: f.ctx, repoRoot: repoRoot, cfg: cfg, tsgo: tsgo}
	f.jobs[cfg] = job
	f.queue = append(f.queue, job)
	if f.workers < prefetchWorkers() {
		f.workers++
		go f.work()
	}
	f.cond.Signal()
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
		if f.jobs[job.cfg] != job {
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
	defer f.mu.Unlock()
	f.stopped = true
	f.cancel()
	clear(f.jobs)
	f.queue = nil
	f.cond.Broadcast()
}
