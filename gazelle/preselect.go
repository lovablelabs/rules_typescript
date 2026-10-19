package typescript

import (
	"cmp"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

type preselection struct {
	stamp    selectionStamp
	retained []string
	selected program
	files    map[string]bool
	notes    []func(*programStore)
	reads    *viewReads
}

// Every emission write a selection can read moves one of these.
type selectionStamp struct {
	epoch, listEpoch, outputsGeneration, rulesGeneration int
}

func (s *programStore) selectionStamp() selectionStamp {
	g := s.emission
	return selectionStamp{g.epoch, g.listEpoch, g.outputsGeneration, g.rulesGeneration}
}

// frozenView marks a store copy that shares the store's state for reading and
// keeps its own caches; it holds back recorder notes, and a load or a fatal bails.
type frozenView struct {
	configs map[*config.Config]*config.Config
	notes   []func(*programStore)
	reads   *viewReads
}

func (v *frozenView) later(note func(*programStore)) {
	v.notes = append(v.notes, note)
}

type frozenBail struct{}

// Nonzero while workers read frozen views; no other goroutine runs fatal then,
// as the main one waits and prefetch has stopped.
var frozenPhase atomic.Int32

// A fatal a worker reaches is the serial pass's to report, in its order.
func bailIfFrozen() {
	if frozenPhase.Load() > 0 {
		panic(frozenBail{})
	}
}

// recoverBail, deferred, ends a view's task at a bail; any other panic is a bug and propagates.
func recoverBail() {
	if r := recover(); r != nil {
		if _, ok := r.(frozenBail); !ok {
			panic(r)
		}
	}
}

// programSize orders selections largest first, so the longest does not start last.
func programSize(p *program) int {
	return len(p.candidates) + len(p.Files) + len(p.Edges)
}

func (s *programStore) preselect() map[string]*preselection {
	if s.emission == nil {
		return nil
	}
	var dirs []string
	for dir, input := range s.inputs {
		if input.program != nil {
			dirs = append(dirs, dir)
		}
	}
	slices.SortFunc(dirs, func(a, b string) int {
		if n := cmp.Compare(programSize(s.inputs[b].program), programSize(s.inputs[a].program)); n != 0 {
			return n
		}
		return strings.Compare(a, b)
	})
	stamp := s.selectionStamp()
	results := make([]*preselection, len(dirs))
	s.onViews(len(dirs), func(v *programStore, i int) { results[i] = v.preselectInput(dirs[i], stamp) })
	selections := make(map[string]*preselection, len(dirs))
	for i, dir := range dirs {
		selections[dir] = results[i]
	}
	return selections
}

func (s *programStore) onViews(n int, task func(v *programStore, i int)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	frozenPhase.Add(1)
	for range min(n, runtime.GOMAXPROCS(0)) {
		wg.Go(func() {
			v := s.freeze()
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				task(v, i)
			}
		})
	}
	wg.Wait()
	frozenPhase.Add(-1)
}

func (s *programStore) freeze() *programStore {
	g := *s.emission
	g.outputIndex = nil
	g.registered = maps.Clone(g.registered)
	v := *s
	v.emission = &g
	v.memo, v.regularFiles, v.manifests = nil, nil, nil
	v.importFacts, v.buildFacts = nil, nil
	v.frozen = &frozenView{configs: map[*config.Config]*config.Config{}}
	return &v
}

func (s *programStore) viewConfig(c *config.Config) *config.Config {
	if vc := s.frozen.configs[c]; vc != nil {
		return vc
	}
	vc := c.Clone()
	tc := *getConfig(c)
	tc.programs = s
	vc.Exts[languageName] = &tc
	s.frozen.configs[c] = vc
	return vc
}

func (s *programStore) preselectInput(dir string, stamp selectionStamp) *preselection {
	defer recoverBail()
	input := s.inputs[dir]
	c := s.viewConfig(input.config)
	s.frozen.reads = nil
	retained := s.keptProgramSources(c, dir)
	// Notes are per input on a reused view; the commit runs keptProgramSources itself, which notes its own.
	s.frozen.notes = nil
	s.frozen.reads = &viewReads{}
	selected := *input.program
	s.selectProgram(c, &selected, retained...)
	files := s.authoredFiles(&selected)
	return &preselection{stamp: stamp, retained: retained, selected: selected, files: files,
		notes: s.frozen.notes, reads: s.frozen.reads}
}

// observeFrozen bails unless loading pkgs would write nothing.
func (s *programStore) observeFrozen(pkgs []string) {
	seen := map[string]bool{}
	for _, pkg := range pkgs {
		for dir := pkg; !seen[dir]; dir = parentDir(dir) {
			seen[dir] = true
			if s.loadDir(dir, nil, true) {
				panic(frozenBail{})
			}
			if dir == "" {
				break
			}
		}
	}
}

// programRefresh is a package's program rules, merged again once the index is complete.
type programRefresh struct {
	args language.GenerateArgs
	res  language.GenerateResult
}

type refreshKey struct {
	c    *config.Config
	from *program
}

type refreshJob struct {
	key      refreshKey
	roots    []string
	selected program
	notes    []func(*programStore)
	reads    *viewReads
}

// viewReads are what a view's selection read: each directory's outputs (nil in the first pass, which the stamp
// covers) and each _pb.ts answer, which other languages' resolvers may give; the walk's names stay fixed.
type viewReads struct {
	dirs   map[string]*packageOutputs
	protos []protoRead
}

type protoRead struct {
	file      string
	generated bool
}

// observedOutputs is dir's BUILD file and its output declarations, nil while one of its rules is unobserved.
func (s *programStore) observedOutputs(dir string) (*rule.File, *packageOutputs) {
	f := s.emission.files[dir]
	var outputs *packageOutputs
	if f != nil {
		outputs = s.packageOutputs(dir, f)
	}
	if s.frozen != nil && s.frozen.reads != nil && s.frozen.reads.dirs != nil {
		s.frozen.reads.dirs[dir] = outputs
	}
	return f, outputs
}

// prerefresh selects, on worker views, what each refresh is predicted to select: every
// program rule's program with the rule's current sources as roots.
func (s *programStore) prerefresh() map[refreshKey][]*refreshJob {
	if s.emission == nil {
		return nil
	}
	var refreshes []*programRefresh
	for _, dir := range slices.Sorted(maps.Keys(s.inputs)) {
		if refresh := s.inputs[dir].refresh; refresh != nil {
			refreshes = append(refreshes, refresh)
		}
	}
	predicted := make([][]refreshJob, len(refreshes))
	s.onViews(len(refreshes), func(v *programStore, i int) { predicted[i] = v.predictRefresh(refreshes[i]) })
	jobs := slices.Concat(predicted...)
	slices.SortStableFunc(jobs, func(a, b refreshJob) int { return cmp.Compare(programSize(b.key.from), programSize(a.key.from)) })
	results := make([]*refreshJob, len(jobs))
	s.onViews(len(jobs), func(v *programStore, i int) { results[i] = v.selectOnView(jobs[i]) })
	selections := map[refreshKey][]*refreshJob{}
	for _, result := range results {
		if result != nil {
			selections[result.key] = append(selections[result.key], result)
		}
	}
	return selections
}

// predictRefresh mirrors refreshProgramRules' choice of program and roots on the rules as they stand.
func (s *programStore) predictRefresh(refresh *programRefresh) (jobs []refreshJob) {
	defer recoverBail()
	args := refresh.args
	c := s.viewConfig(args.Config)
	file := args.File
	if file == nil {
		file = &rule.File{Pkg: args.Rel}
	}
	var library *rule.Rule
	for _, generated := range refresh.res.Gen {
		r := canonicalRule(c, generated)
		if !managedProgramRule(r, args.Rel) {
			continue
		}
		effective := existingRule(args, r.Kind(), r.Name())
		if effective == nil {
			effective = generated
		}
		cfg := compilerConfigSource(c, effective.AttrString("tsconfig"), args.Rel)
		sourceRules := []*rule.Rule{effective}
		if r.Kind() == "ts_compile" {
			library = effective
		} else if library != nil && cfg != "" && cfg == compilerConfigSource(c, library.AttrString("tsconfig"), args.Rel) {
			sourceRules = append(sourceRules, library)
		}
		if cfg == "" || cfg != tsconfigIn(parentDir(cfg)) {
			continue
		}
		var roots []string
		for _, sourceRule := range sourceRules {
			for _, spec := range automaticProgramImports(c, effective, sourceRule, file) {
				roots = append(roots, spec.Imp)
			}
		}
		p := s.inputs[parentDir(cfg)].program
		if p == nil {
			p = s.programs[parentDir(cfg)]
		}
		if p != nil && !p.manifest && p.refused == "" {
			jobs = append(jobs, refreshJob{key: refreshKey{args.Config, p}, roots: roots})
		}
	}
	return jobs
}

func (s *programStore) selectOnView(job refreshJob) *refreshJob {
	defer recoverBail()
	// A job reads every value itself, not through an earlier job's memo, so its reads are complete.
	s.memo, s.frozen.notes = nil, nil
	s.frozen.reads = &viewReads{dirs: map[string]*packageOutputs{}}
	job.selected = *job.key.from
	job.selected.Roots = job.roots
	s.selectProgram(s.viewConfig(job.key.c), &job.selected)
	job.notes, job.reads = s.frozen.notes, s.frozen.reads
	return &job
}

// refreshSelection is p selected from roots: a view's selection when it read what the store holds now.
func (s *programStore) refreshSelection(c *config.Config, p *program, roots []string) *program {
	key := refreshKey{c, p}
	for i, pre := range s.prerefreshed[key] {
		if slices.Equal(pre.roots, roots) && s.readsHold(c, pre.reads) {
			s.prerefreshed[key] = slices.Delete(s.prerefreshed[key], i, i+1)
			for _, note := range pre.notes {
				note(s)
			}
			return &pre.selected
		}
	}
	selected := *p
	selected.Roots = roots
	s.selectProgram(c, &selected)
	return &selected
}

func (s *programStore) readsHold(c *config.Config, reads *viewReads) bool {
	for dir, read := range reads.dirs {
		// nil, read for a missing BUILD file or an unobserved rule, holds only for a missing file.
		if f := s.emission.files[dir]; (f == nil) != (read == nil) || f != nil && !sameOutputs(read, s.packageOutputs(dir, f)) {
			return false
		}
	}
	// In the view's order, so the commit asks other languages' resolvers nothing past its first difference.
	for _, read := range reads.protos {
		if s.generatedProgramFile(c, s.index, read.file, true) != read.generated {
			return false
		}
	}
	return true
}

// sameOutputs is whether b, nil while a rule is unobserved, declares a's outputs and trees at the same rule
// positions; refreshes rename no rule in place.
func sameOutputs(a, b *packageOutputs) bool {
	return b != nil && (a.incomplete < 0) == (b.incomplete < 0) && maps.Equal(a.files, b.files) && slices.Equal(a.trees, b.trees)
}
