package typescript

import (
	"bytes"
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/merger"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

// Gazelle 0.54's v2 compat update loop writes every BUILD file before
// OnFinish, which is AfterResolvingDeps.
func gazelleWritesFirst() bool {
	var pcs [32]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	for {
		frame, more := frames.Next()
		if strings.HasPrefix(frame.Function, "github.com/bazel-contrib/bazel-gazelle/v2/compat.") {
			return true
		}
		if !more {
			return false
		}
	}
}

// earlyFinish is the generated rules of a run whose Gazelle writes before
// AfterResolvingDeps, which the first Resolve resolves, merges and finishes.
type earlyFinish struct {
	generated []generatedPackage
	done      bool
}

func (e *earlyFinish) noteGenerated(args language.GenerateArgs, res language.GenerateResult) {
	if e != nil {
		e.generated = append(e.generated, generatedPackage{args: args, res: res})
	}
}

// finishBeforeWriting does Gazelle's resolve, post-resolve merge and
// AfterResolvingDeps here, or replays them, then retires the generated rules.
func (l *tsLang) finishBeforeWriting(ix *resolve.RuleIndex) {
	s, e := l.programs, l.programs.early
	e.done = true
	rec := s.resolution
	if rec.begin(s) {
		rec.applyReplay(s)
	} else {
		l.speculateFirstPass(ix)
		for _, g := range e.generated {
			for i, r := range g.res.Gen {
				l.resolveRule(g.args.Config, ix, r, g.res.Imports[i], label.New(g.args.Config.RepoName, g.args.Rel, r.Name()))
			}
		}
		for _, g := range e.generated {
			if f := s.emission.files[g.args.Rel]; f != nil && f.File != nil {
				merger.MergeFile(f, g.res.Empty, g.res.Gen, merger.PostResolve, l.mergeKinds(g.args.Config), g.args.Config.AliasMap)
			}
		}
		l.finishRules()
		if rec != nil {
			rec.finish(s)
		}
	}
	retired := 0
	for _, g := range e.generated {
		f := s.emission.files[g.args.Rel]
		if f == nil || f.File == nil {
			continue
		}
		for _, r := range slices.Concat(g.res.Gen, g.res.Empty) {
			if !slices.Contains(f.Rules, r) {
				// No kind info and a unique name: Gazelle's post-resolve merge
				// matches nothing, and the rule it then inserts is deleted.
				r.SetKind("_ts_finished")
				r.SetName("_ts_finished_" + strconv.Itoa(retired))
				r.Delete()
				retired++
			}
		}
	}
	e.generated = nil
}

// speculateFirstPass speculates, under -ts_readsets=verify, every generated rule's
// edge resolution once selection, which the first Resolve runs, is done.
func (l *tsLang) speculateFirstPass(ix *resolve.RuleIndex) {
	s := l.programs
	if s.verify == nil {
		return
	}
	s.ruleListsMayChange()
	s.selectInputs(ix)
	for _, g := range s.early.generated {
		for i, live := range g.res.Gen {
			imps, ok := g.res.Imports[i].(*ruleImports)
			if !ok || imps == nil {
				continue
			}
			c := g.args.Config
			from := label.New(c.RepoName, g.args.Rel, live.Name())
			r := canonicalRule(c, live)
			if _, skip := mergedRule(c, r, from); skip {
				continue
			}
			// The serial pass's first edge resolution makes the check, which a speculation must not.
			s.requireInstall(c.RepoRoot)
			s.verify.specs[live] = s.speculateTask(func() {
				effective, skip := mergedRule(c, r, from)
				if skip {
					return
				}
				sourceRoots := slices.Clone(effective.AttrStrings("srcs"))
				owned := activeRun.own(r, live)
				if effective == r {
					effective = owned
				}
				resolveRuleEdges(c, ix, owned, effective, imps, from, sourceRoots)
			})
		}
	}
}

// mergeKinds follows map_kind chains to the end, as Gazelle's merge does.
func (l *tsLang) mergeKinds(c *config.Config) map[string]rule.KindInfo {
	kinds := maps.Clone(tsKinds())
	for from, info := range tsKinds() {
		seen := map[string]bool{from: true}
		for mapped, ok := c.KindMap[from]; ok && !seen[mapped.KindName]; mapped, ok = c.KindMap[mapped.KindName] {
			seen[mapped.KindName] = true
			kinds[mapped.KindName] = info
		}
	}
	return kinds
}

// Only a run no Resolve reached finishes its rules after Gazelle wrote them.
func (s *programStore) requireWritten(written []recordedFile) {
	for _, f := range written {
		if !bytes.Equal(s.emission.files[f.Pkg].Format(), f.Content) {
			fatalf("typescript: AfterResolvingDeps changes %s after Gazelle wrote it: this Gazelle writes first, and no generated rule reached Resolve, where the extension finishes its rules before the write. Did you mean to update a package this language generates rules for?", orRepoRoot(f.Pkg))
		}
	}
}
