package typescript

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
)

var readsetMode = "off"

// readSet is what a speculated task read of the state other tasks write.
type readSet struct {
	rules map[string]ruleRead
	// Each directory's output declarations, nil for a missing BUILD file or an unobserved rule.
	dirs map[string]*packageOutputs
	// Each package's BUILD file and its rule list, which a read finds a rule absent from.
	files map[string]fileRead
	// keysGeneration at a scan of every stored key, or -1; rulesGeneration at a proto index read, or -1.
	keys, rulesGen int
}

type fileRead struct {
	file  *rule.File
	rules []*rule.Rule
}

type ruleRead struct {
	stamp  int
	digest [sha256.Size]byte
}

func newReadSet() *readSet {
	return &readSet{rules: map[string]ruleRead{}, dirs: map[string]*packageOutputs{}, files: map[string]fileRead{}, keys: -1, rulesGen: -1}
}

// ruleDigest leaves out the selected-imports memo, which reads fill in place with what they would compute.
func ruleDigest(r *rule.Rule) [sha256.Size]byte {
	if r == nil {
		return [sha256.Size]byte{}
	}
	view := rule.NewRule(r.Kind(), r.Name())
	for _, key := range r.AttrKeys() {
		view.SetAttr(key, r.Attr(key))
		if comments := r.AttrComments(key); comments != nil {
			*view.AttrComments(key) = *comments
		}
	}
	for _, key := range r.PrivateAttrKeys() {
		if key != "_ts_selected_imports" {
			view.SetPrivateAttr(key, r.PrivateAttr(key))
		}
	}
	return digestRuleBody(view).sum
}

// stale names the first read another task's write has moved since it was recorded.
// A rule whose body changed while its stamp held is a write site that misses stamp.
func (rs *readSet) stale(s *programStore) (string, error) {
	g := s.emission
	stale := ""
	for _, key := range slices.Sorted(maps.Keys(rs.rules)) {
		read := rs.rules[key]
		if g.stamps[key] != read.stamp {
			stale = cmp.Or(stale, "rule")
		} else if ruleDigest(g.rules[key]) != read.digest {
			return "", fmt.Errorf("%s changed without moving its stamp", key)
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(rs.dirs)) {
		read := rs.dirs[dir]
		if f := g.files[dir]; (f == nil) != (read == nil) || f != nil && !sameProducers(read, s.packageOutputs(dir, f)) {
			stale = cmp.Or(stale, "outputs")
		}
	}
	for pkg, read := range rs.files {
		if f := g.files[pkg]; f != read.file || f != nil && !slices.Equal(f.Rules, read.rules) {
			stale = cmp.Or(stale, "file")
		}
	}
	if rs.keys >= 0 && rs.keys != g.keysGeneration {
		stale = cmp.Or(stale, "keys")
	}
	if rs.rulesGen >= 0 && rs.rulesGen != g.rulesGeneration {
		stale = cmp.Or(stale, "proto index")
	}
	return stale, nil
}

// sameProducers is sameOutputs with each producer's name and kind, which a caller reads.
func sameProducers(a, b *packageOutputs) bool {
	return sameOutputs(a, b) && slices.EqualFunc(a.producers, b.producers, func(x, y *rule.Rule) bool {
		return x.Name() == y.Name() && x.Kind() == y.Kind()
	})
}

// buildFile is pkg's BUILD file, which a speculation records reading with its rule list.
func (s *programStore) buildFile(pkg string) *rule.File {
	f := s.emission.files[pkg]
	if run := activeRun; run != nil && run.dry {
		if _, seen := run.reads.files[pkg]; !seen {
			var rules []*rule.Rule
			if f != nil {
				rules = slices.Clone(f.Rules)
			}
			run.reads.files[pkg] = fileRead{f, rules}
		}
	}
	return f
}

// taskRun is one task's outputs in order: a speculation's, or a verified serial run's.
type taskRun struct {
	dry    bool
	reads  *readSet
	clones map[*rule.Rule]*rule.Rule
	out    []string
	prev   io.Writer
	bail   string
	// The pending owner a completion's speculation bailed on.
	waiting string
	// Time in runs verified inside this one, which the tally counts there.
	nested time.Duration
	// Nonzero inside a package's output declarations, which dirs records by value.
	quiet int
}

// readsetFailed ends the run: a broken read set, not an edge fatal another task deferred.
var readsetFailed = log.Fatalf

// activeRun is the task -ts_readsets=verify captures, set only on the main goroutine.
var activeRun *taskRun

func dryRun() bool { return activeRun != nil && activeRun.dry }

// noteOutput records a task's outputs while a run is captured; out is not built otherwise.
func noteOutput(out func() []string) {
	if run := activeRun; run != nil {
		run.out = append(run.out, out()...)
	}
}

// Write captures a log line, which a speculation holds back.
func (run *taskRun) Write(p []byte) (int, error) {
	run.out = append(run.out, "log "+string(p))
	if run.dry {
		return len(p), nil
	}
	return run.prev.Write(p)
}

// read records the rule stored at key and answers a speculation's own rules with their clones.
func (run *taskRun) read(g *emissionGraph, key string, r *rule.Rule) *rule.Rule {
	if _, seen := run.reads.rules[key]; !seen && run.quiet == 0 {
		run.reads.rules[key] = ruleRead{g.stamps[key], ruleDigest(r)}
	}
	if clone := run.clones[r]; clone != nil {
		return clone
	}
	return r
}

// own clones a rule a speculation writes, before it reads it. canonicalRule's copy r of a
// kind-mapped rule shares its attributes with mapped, so r's clone shares them with mapped's.
func (run *taskRun) own(r, mapped *rule.Rule) *rule.Rule {
	if run.clones[mapped] == nil {
		run.clones[mapped] = cloneRule(mapped)
	}
	if run.clones[r] == nil {
		copied := *run.clones[mapped]
		copied.SetKind(r.Kind())
		run.clones[r] = &copied
	}
	return run.clones[r]
}

// cloneRule copies r's attributes, their comments and its private attributes into
// a rule that shares no syntax with r.
func cloneRule(r *rule.Rule) *rule.Rule {
	if r.ShouldKeep() || len(r.Args()) > 0 {
		panic(frozenBail{"kept rule"})
	}
	clone := rule.NewRule(r.Kind(), r.Name())
	for _, key := range r.AttrKeys() {
		f, err := bzl.ParseBuild("clone", []byte("_ = "+bzl.FormatString(r.Attr(key))))
		if err != nil || len(f.Stmt) != 1 {
			panic(frozenBail{"unparsable attribute"})
		}
		clone.SetAttr(key, f.Stmt[0].(*bzl.AssignExpr).RHS)
		if comments := r.AttrComments(key); comments != nil {
			*clone.AttrComments(key) = *comments
		}
	}
	for _, key := range r.PrivateAttrKeys() {
		clone.SetPrivateAttr(key, r.PrivateAttr(key))
	}
	return clone
}

func ruleText(r *rule.Rule) string {
	var b strings.Builder
	b.WriteString(r.Kind())
	for _, key := range r.AttrKeys() {
		fmt.Fprintf(&b, " %s=%s", key, bzl.FormatString(r.Attr(key)))
	}
	return b.String()
}

// speculateTask runs task with every shared write held back and a load, a fatal or a
// pending owner bailing; each task reads every value itself, not an earlier one's memo.
func (s *programStore) speculateTask(task func()) (run *taskRun) {
	run = &taskRun{dry: true, reads: newReadSet(), clones: map[*rule.Rule]*rule.Rule{}}
	memo, outputIndex, facts := s.memo, s.emission.outputIndex, s.importFacts
	s.memo, s.emission.outputIndex, s.importFacts = nil, nil, nil
	defer func() {
		s.memo, s.emission.outputIndex, s.importFacts = memo, outputIndex, facts
		if p := recover(); p != nil {
			bail, ok := p.(frozenBail)
			if !ok {
				panic(fmt.Sprintf("%v\n%s", p, debug.Stack()))
			}
			run.bail = bail.why
		}
	}()
	run.capture(task)
	return run
}

func (run *taskRun) capture(task func()) {
	parent := activeRun
	run.prev = log.Writer()
	activeRun = run
	log.SetOutput(run)
	defer func() {
		activeRun = parent
		log.SetOutput(run.prev)
	}()
	task()
}

// readsetVerifier is -ts_readsets=verify's speculations, by first-pass rule or
// completing program's key, and its tally by outcome.
type readsetVerifier struct {
	specs map[any]*taskRun
	tally map[string]int
	took  map[string]time.Duration
}

func (v *readsetVerifier) take(key any) *taskRun {
	if v == nil {
		return nil
	}
	spec := v.specs[key]
	delete(v.specs, key)
	return spec
}

// verified runs task serially as without -ts_readsets; a speculation of it whose
// reads still hold must have produced the same outputs.
func (s *programStore) verified(kind, what string, spec *taskRun, task func()) {
	if s.verify == nil {
		task()
		return
	}
	outcome, agreed := kind+" unspeculated", false
	if spec != nil && spec.bail != "" {
		outcome = kind + " bail " + spec.bail
	} else if spec != nil {
		stale, err := spec.reads.stale(s)
		if err != nil {
			readsetFailed("typescript: -ts_readsets=verify: %s: %v", what, err)
		}
		outcome, agreed = kind+" agreed", stale == ""
		if !agreed {
			outcome = kind + " conflict " + stale
		}
	}
	run := &taskRun{}
	start := time.Now()
	run.capture(task)
	took := time.Since(start)
	s.verify.took[outcome] += took - run.nested
	s.verify.tally[outcome]++
	if activeRun != nil {
		activeRun.nested += took
	}
	if agreed && !slices.Equal(spec.out, run.out) {
		i := 0
		for i < min(len(spec.out), len(run.out)) && spec.out[i] == run.out[i] {
			i++
		}
		at := func(out []string) string {
			if i < len(out) {
				return out[i]
			}
			return "(nothing)"
		}
		readsetFailed("typescript: -ts_readsets=verify: %s: a speculation whose reads held differs from the serial run, so its read set misses a read:\nspeculated: %s\nserial:     %s", what, at(spec.out), at(run.out))
	}
}

func (v *readsetVerifier) report() {
	var parts []string
	for _, outcome := range slices.Sorted(maps.Keys(v.tally)) {
		parts = append(parts, fmt.Sprintf("%d %s (%d ms)", v.tally[outcome], outcome, v.took[outcome].Milliseconds()))
	}
	log.Printf("typescript: -ts_readsets=verify: %s", strings.Join(parts, ", "))
}
