package typescript

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

var memoMode = "off"

// readDigest is one read of a task by value, which holds across runs where a stamp does not.
type readDigest struct {
	category, name string
	digest         [sha256.Size]byte
}

// question is a task's calls to one oracle that stays fixed within a run: the argument lists it asked, by id
// in the run's question table or the store's, and a digest of each list with its answer, which no order moves.
type question struct {
	oracle string
	args   []uint32
	sum    [sha256.Size]byte
}

// taskEntry is what a task started from, read, asked and did, as data a later run can check by value.
type taskEntry struct {
	kind, key string
	inputs    [sha256.Size]byte
	reads     []readDigest
	questions []question
	out       []output
	// Why no later run may replay the task: it did what an entry cannot repeat. Empty for a storable task.
	never string
}

// memoFailed ends the run on a stored entry whose reads held and whose outputs differ.
var memoFailed = log.Fatalf

// taskRecorder is -ts_memo's entries, in the order the tasks ran.
type taskRecorder struct {
	entries []taskEntry
	// Time spent recording, and in the tasks themselves, which include their questions.
	took, inTasks time.Duration
	// Digests of what tasks share, by pointer: programs, lockfile, proto store, indexes and package configurations.
	sums  map[any][sha256.Size]byte
	rules map[string]stampedSum
	// The digests of the newest holding and proto index, which are rebuilt when a rule is stored.
	holdings struct {
		index *holdingIndex
		sum   [sha256.Size]byte
	}
	protos struct {
		index *protoIndex
		sum   [sha256.Size]byte
	}
	// Why a package configuration cannot be digested, by configuration.
	opaque map[*config.Config]string
	// The question table and the logs of queued tasks, which only the worker touches until settle.
	questions *questionTable
	jobs      chan questionJob
	worker    sync.WaitGroup
	answered  []answeredJob
	free      []*taskLogs
	// Nil without a listing cache, which keeps the entries nowhere.
	store *storeFile
	// verify compares each task with the last pass's entry, and tallies the outcomes with their time.
	verify bool
	tally  map[string]int
	spent  map[string]time.Duration
}

// openStore finds the last pass's store, which only a full pass past the gates uses: no replay or partial run opens it.
func (r *taskRecorder) openStore(fs *flag.FlagSet, c *config.Config, fullWalk bool) error {
	if r == nil || !fullWalk || listingCacheDir == "" {
		return nil
	}
	binary := binaryIdentity()
	if binary == "" {
		return nil
	}
	cache, err := filepath.Abs(listingCacheDir)
	if err != nil {
		return err
	}
	key := recordKey(memoStoreMagic, fs, c, "ts_memo", "ts_resolve_record", "ts_listing_cache", "ts_listing_patch_verify", "ts_parallel", "cpuprofile", "memprofile", "trace")
	r.store = &storeFile{path: memoStorePath(cache, key), key: key, binary: binary}
	if r.verify {
		r.store.startLoad()
	}
	return nil
}

// ruleSum is ruleDigest of the rule stored at key, kept while no write moves the key's stamp. A nil
// recorder, which a speculation without -ts_memo has, digests every time.
func (r *taskRecorder) ruleSum(g *emissionGraph, key string, rl *rule.Rule) (sum [sha256.Size]byte, opaque bool) {
	if r == nil || rl == nil {
		body := ruleBodyDigest(rl)
		return body.sum, body.partial()
	}
	stamp := g.stamps[key]
	if hit, ok := r.rules[key]; ok && hit.rule == rl && hit.stamp == stamp {
		return hit.sum, hit.opaque
	}
	body := ruleBodyDigest(rl)
	hit := stampedSum{rl, stamp, body.sum, body.partial()}
	r.rules[key] = hit
	return hit.sum, hit.opaque
}

// stampedSum is a rule's digest, and whether it leaves out what the rule points to: shared state, or a value
// the state digest cannot read.
type stampedSum struct {
	rule   *rule.Rule
	stamp  int
	sum    [sha256.Size]byte
	opaque bool
}

// queue hands a task's log to the worker, which a record pass needs only once it writes the store.
func (r *taskRecorder) queue(job questionJob) {
	if r.jobs == nil {
		r.jobs = make(chan questionJob, 64)
		r.worker.Go(func() {
			for job := range r.jobs {
				r.answered = append(r.answered, r.questions.answer(job))
			}
		})
	}
	r.jobs <- job
}

type answeredJob struct {
	index     int
	questions []question
	never     string
}

// logs are a task's maps of what it asked, empty, reused from a finished task: a task asks thousands of questions,
// and growing a map to hold them costs more than asking.
func (r *taskRecorder) logs() *taskLogs {
	if n := len(r.free); n > 0 {
		logs := r.free[n-1]
		r.free = r.free[:n-1]
		return logs
	}
	return &taskLogs{configs: map[string]*config.Config{}, dirs: map[dirNamesKey]struct{}{}, selected: map[unsafe.Pointer]askKey{}, rooted: map[bool]bool{}}
}

func (r *taskRecorder) reuse(logs *taskLogs) {
	clear(logs.configs)
	clear(logs.dirs)
	clear(logs.selected)
	clear(logs.rooted)
	logs.lookups = nil
	r.free = append(r.free, logs)
}

// taskLogs skip a task's repeated config, directory-names, selected-imports and rooted questions before their answers are built.
type taskLogs struct {
	configs  map[string]*config.Config
	dirs     map[dirNamesKey]struct{}
	selected map[unsafe.Pointer]askKey
	rooted   map[bool]bool
	lookups  *config.Config
}

// settle waits for the worker and puts each task's questions in its entry.
func (r *taskRecorder) settle() {
	if r == nil || r.jobs == nil {
		return
	}
	close(r.jobs)
	r.worker.Wait()
	r.jobs = nil
	for _, done := range r.answered {
		r.entries[done.index].questions, r.entries[done.index].never = done.questions, done.never
	}
	r.answered = nil
}

func newTaskRecorder(verify bool) *taskRecorder {
	return &taskRecorder{sums: map[any][sha256.Size]byte{}, rules: map[string]stampedSum{}, opaque: map[*config.Config]string{}, questions: newQuestionTable(),
		verify: verify, tally: map[string]int{}, spent: map[string]time.Duration{}}
}

func (r *taskRecorder) memo(of any, digest func() [sha256.Size]byte) [sha256.Size]byte {
	sum, ok := r.sums[of]
	if !ok {
		sum = digest()
		r.sums[of] = sum
	}
	return sum
}

func (r *taskRecorder) holdingsSum(h *holdingIndex) [sha256.Size]byte {
	if r.holdings.index != h {
		r.holdings.index, r.holdings.sum = h, holdingsDigest(h)
	}
	return r.holdings.sum
}

func (r *taskRecorder) protoIndexSum(p *protoIndex) [sha256.Size]byte {
	if r.protos.index != p {
		r.protos.index, r.protos.sum = p, protoIndexDigest(p)
	}
	return r.protos.sum
}

// foreignAt is the manifest above dir that the lockfile has no importer for, "" when it has one.
func (s *programStore) foreignAt(dir string) string {
	m := s.foreign[dir]
	if recording() {
		ask("foreign", []string{dir}, m)
	}
	return m
}

// namesKey is a directory's names by the slice the walk keeps them in, which a load of the directory replaces.
type namesKey struct {
	dir string
	n   int
	at  *string
}

func namesOf(dir string, names []string) namesKey {
	return namesKey{dir, len(names), unsafe.SliceData(names)}
}

// dirNamesKey is a dirNames answer, which a task asks again only when the walk's names or whether it saw dir moved.
type dirNamesKey struct {
	namesKey
	ok bool
}

// namesSum is a digest of the file names the walk saw in dir.
func (r *taskRecorder) namesSum(dir string, names []string) string {
	sum := r.memo(namesOf(dir, names), func() [sha256.Size]byte {
		d := newHasher()
		d.strs(names)
		return d.sum()
	})
	return string(sum[:])
}

// recording is whether the running task is recorded, which makes the oracles log their questions.
func recording() bool { return activeRun != nil && activeRun.recorder != nil }

// askKey is an oracle and the arguments a task asked it with, the fourth on joined to the third by 0s.
type askKey struct {
	oracle  string
	a, b, c string
	n       int
}

func newAskKey(oracle string, args []string) askKey {
	key := askKey{oracle: oracle, n: len(args)}
	switch len(args) {
	case 0:
	case 1:
		key.a = args[0]
	case 2:
		key.a, key.b = args[0], args[1]
	case 3:
		key.a, key.b, key.c = args[0], args[1], args[2]
	default:
		key.a, key.b, key.c = args[0], args[1], strings.Join(args[2:], "\x00")
	}
	return key
}

// joined spells the arguments as a store keeps them, each preceded by a 0.
func (k askKey) joined() string {
	switch k.n {
	case 0:
		return ""
	case 1:
		return "\x00" + k.a
	case 2:
		return "\x00" + k.a + "\x00" + k.b
	default:
		return "\x00" + k.a + "\x00" + k.b + "\x00" + k.c
	}
}

// ask logs a question to an oracle fixed within a run, once per arguments; an oracle that answers the
// same arguments twice differently is not fixed, and keeps the task out of the store.
func ask(oracle string, args []string, answer ...string) {
	if run := activeRun; run != nil && run.recorder != nil {
		run.asked.asked = append(run.asked.asked, askedQuestion{newAskKey(oracle, args), strings.Join(answer, "\x00"), run.asked.next()})
	}
}

// taskQuestions are appended by the task and interned by the recorder's worker. n numbers them across kinds, which
// orders a moving oracle against the task's own reason to stay out of the store.
type taskQuestions struct {
	asked     []askedQuestion
	index     []indexQuestion
	overrides []overrideQuestion
	sources   []sourceQuestion
	owners    []ownerAnswer
	n         uint32
}

func (q *taskQuestions) next() (at uint32) {
	at = q.n
	q.n++
	return at
}

type askedQuestion struct {
	key    askKey
	answer string
	at     uint32
}

// indexQuestion keeps what the lookup found by value: a caller may reorder the slice it returned.
type indexQuestion struct {
	imp   string
	first resolve.FindResult
	rest  []resolve.FindResult
	at    uint32
}

func (q indexQuestion) found() []resolve.FindResult {
	if q.first.Label == label.NoLabel {
		return nil
	}
	return append([]resolve.FindResult{q.first}, q.rest...)
}

type overrideQuestion struct {
	imp string
	dep label.Label
	ok  bool
	at  uint32
}

type sourceQuestion struct {
	file   string
	listed bool
	at     uint32
}

var questionLogs = sync.Pool{New: func() any { return &taskQuestions{} }}

func (q *taskQuestions) reuse() {
	q.asked, q.index, q.overrides, q.sources, q.owners, q.n = q.asked[:0], q.index[:0], q.overrides[:0], q.sources[:0], q.owners[:0], 0
	questionLogs.Put(q)
}

// lookupUnder notes the configuration an override or index lookup answers under, which its arguments leave out: a
// task whose lookups answer under two configurations is never stored, so a replay asks under the task's own.
func (run *taskRun) lookupUnder(c *config.Config) {
	switch {
	case run.logs.lookups == nil:
		run.logs.lookups = c
	case run.logs.lookups != c:
		run.keepOut("lookups under two configurations")
	}
}

// ownerAnswer is whether target's completion is pending, or whether visiting key promotes target.
type ownerAnswer struct {
	promotion   bool
	key, target string
	holds       bool
}

// noteOwnerState logs an owner answer, which the worker folds into the task's: each adds its own digest to a sum, so
// their order does not matter and an owner asked again after a nested completion ran is not a moving oracle.
func noteOwnerState(answer ownerAnswer) {
	activeRun.asked.owners = append(activeRun.asked.owners, answer)
}

func (t *questionTable) ownerSum(answer ownerAnswer) [sha256.Size]byte {
	sum, ok := t.ownerSums[answer]
	if !ok {
		fields := []string{"pending", answer.target}
		if answer.promotion {
			fields = []string{"promotion", answer.key, answer.target}
		}
		fields = append(fields, strconv.FormatBool(answer.holds))
		sum = questionSum("owner state", newAskKey("owner state", fields).joined(), "")
		t.ownerSums[answer] = sum
	}
	return sum
}

// lookupFold is a sum of digests, which no order of adding them moves and a repeated one counts in.
type lookupFold struct {
	total [4]uint64
	added bool
}

func (f *lookupFold) add(sum [sha256.Size]byte) {
	f.added = true
	for i := range f.total {
		f.total[i] += binary.LittleEndian.Uint64(sum[8*i:])
	}
}

func (f *lookupFold) sum() (sum [sha256.Size]byte) {
	for i, part := range f.total {
		binary.LittleEndian.PutUint64(sum[8*i:], part)
	}
	return sum
}

// questionSum is the digest of one argument list asked of oracle with its answer, which a replay recomputes.
func questionSum(oracle, args, answer string) [sha256.Size]byte {
	d := newHasher()
	d.str(oracle)
	d.str(args)
	d.str(answer)
	return d.sum()
}

func groupSum(oracle string, n int, folded [sha256.Size]byte) [sha256.Size]byte {
	d := newHasher()
	d.str(oracle)
	d.int(n)
	d.w.Write(folded[:])
	return d.sum()
}

// questionTable gives each argument list an id and each answer to it a pair with a digest computed once, and marks
// the pair the task being interned logged for an id, so a repeated question digests nothing again.
type questionTable struct {
	ids map[askKey]uint32
	// The ids of the TypeScript index, override and source file questions, by the one argument that varies.
	index, overrides, sources map[string]uint32
	questionArgs
	latest    []uint32
	byName    map[string]uint32
	pairs     []answerPair
	marks     []questionMark
	epoch     uint32
	ownerSums map[ownerAnswer][sha256.Size]byte
}

type answerPair struct {
	arg    uint32
	answer string
	sum    [sha256.Size]byte
	// What an index or override lookup found, which a repeated lookup compares before spelling its answer.
	found []resolve.FindResult
	dep   label.Label
	ok    bool
}

// questionMark is the task that last asked an argument list, by epoch, and the pair it logged.
type questionMark struct{ epoch, pair uint32 }

// noPair is the latest pair of an argument list no task has answered yet.
const noPair = ^uint32(0)

func newQuestionTable() *questionTable {
	return &questionTable{ids: map[askKey]uint32{}, index: map[string]uint32{}, overrides: map[string]uint32{}, sources: map[string]uint32{},
		byName: map[string]uint32{}, ownerSums: map[ownerAnswer][sha256.Size]byte{}}
}

func (t *questionTable) typedID(ids map[string]uint32, by, oracle string, args ...string) uint32 {
	id, ok := ids[by]
	if !ok {
		id = t.newArg(newAskKey(oracle, args))
		ids[by] = id
	}
	return id
}

func (t *questionTable) newArg(key askKey) uint32 {
	o, known := t.byName[key.oracle]
	if !known {
		o = uint32(len(t.oracles))
		t.oracles = append(t.oracles, key.oracle)
		t.byName[key.oracle] = o
	}
	id := uint32(len(t.args))
	t.oracleOf = append(t.oracleOf, o)
	t.args = append(t.args, key.joined())
	t.latest = append(t.latest, noPair)
	t.marks = append(t.marks, questionMark{})
	return id
}

func (t *questionTable) newPair(id uint32, answer string) uint32 {
	t.pairs = append(t.pairs, answerPair{arg: id, answer: answer, sum: questionSum(t.oracles[t.oracleOf[id]], t.args[id], answer)})
	t.latest[id] = uint32(len(t.pairs) - 1)
	return t.latest[id]
}

// interning is one task's log being interned: its pairs, and the first repeat that met another answer.
type interning struct {
	t       *questionTable
	pairs   []uint32
	moving  string
	movedAt uint32
}

func (in *interning) spelled(id, at uint32, answer string) {
	in.log(id, at, func(p *answerPair) bool { return p.answer == answer }, func() string { return answer }, nil)
}

// log logs the answer to question id once per task: matches is whether a pair holds it, spell spells it for a new
// pair, and keep keeps what a later matches compares.
func (in *interning) log(id, at uint32, matches func(*answerPair) bool, spell func() string, keep func(*answerPair)) {
	t := in.t
	if mark := t.marks[id]; mark.epoch == t.epoch {
		if !matches(&t.pairs[mark.pair]) && (in.moving == "" || at < in.movedAt) {
			in.moving, in.movedAt = t.oracles[t.oracleOf[id]], at
		}
		return
	}
	pair := t.latest[id]
	if pair == noPair || !matches(&t.pairs[pair]) {
		pair = t.newPair(id, spell())
		if keep != nil {
			keep(&t.pairs[pair])
		}
	}
	t.marks[id] = questionMark{t.epoch, pair}
	in.pairs = append(in.pairs, pair)
}

// answer interns a task's log: its questions, and why no entry may be stored for it, a moving oracle when one moved
// before the task's own reason.
func (t *questionTable) answer(job questionJob) answeredJob {
	t.epoch++
	in := interning{t: t}
	log := job.log
	for _, q := range log.asked {
		id, ok := t.ids[q.key]
		if !ok {
			id = t.newArg(q.key)
			t.ids[q.key] = id
		}
		in.spelled(id, q.at, q.answer)
	}
	for _, q := range log.index {
		found := q.found()
		spell := func() string { return spelledFound(found) }
		in.log(t.typedID(t.index, q.imp, "index", languageName, q.imp, languageName), q.at,
			func(p *answerPair) bool { return sameFound(p.found, found) || p.answer == spell() }, spell,
			func(p *answerPair) { p.found = found })
	}
	for _, q := range log.overrides {
		spell := func() string { return q.dep.String() + "\x00" + strconv.FormatBool(q.ok) }
		in.log(t.typedID(t.overrides, q.imp, "override", languageName, q.imp, languageName), q.at,
			func(p *answerPair) bool { return p.dep == q.dep && p.ok == q.ok || p.answer == spell() }, spell,
			func(p *answerPair) { p.dep, p.ok = q.dep, q.ok })
	}
	for _, q := range log.sources {
		in.spelled(t.typedID(t.sources, q.file, "sourceFile", q.file), q.at, strconv.FormatBool(q.listed))
	}
	var owner lookupFold
	for _, answer := range log.owners {
		owner.add(t.ownerSum(answer))
	}
	never := job.never
	if in.moving != "" && (never == "" || in.movedAt < job.neverAt) {
		never = "moving oracle " + in.moving
	}
	log.reuse()
	return answeredJob{job.index, t.questions(in.pairs, owner), never}
}

// questions is a task's questions grouped by oracle, the oracles in name order and each one's argument lists by id,
// with its owner answers as one more.
func (t *questionTable) questions(pairs []uint32, owner lookupFold) []question {
	byOracle := make([][]uint64, len(t.oracles))
	for _, pair := range pairs {
		arg := t.pairs[pair].arg
		o := t.oracleOf[arg]
		byOracle[o] = append(byOracle[o], uint64(arg)<<32|uint64(pair))
	}
	var out []question
	for o, logged := range byOracle {
		if len(logged) == 0 {
			continue
		}
		slices.Sort(logged)
		q := question{oracle: t.oracles[o], args: make([]uint32, len(logged))}
		var fold lookupFold
		for i, entry := range logged {
			q.args[i] = uint32(entry >> 32)
			fold.add(t.pairs[uint32(entry)].sum)
		}
		q.sum = groupSum(q.oracle, len(q.args), fold.sum())
		out = append(out, q)
	}
	if owner.added {
		out = append(out, question{oracle: "owner state", sum: owner.sum()})
	}
	slices.SortFunc(out, func(a, b question) int { return strings.Compare(a.oracle, b.oracle) })
	return out
}

// questionJob is a recorded task's log, which the worker turns into the questions of the entry at index, with the
// task's own reason to stay out of the store and how many questions it had asked when it found it.
type questionJob struct {
	index   int
	log     *taskQuestions
	never   string
	neverAt uint32
}

// sameQuestions is whether two entries asked each oracle as many argument lists, with the same answers.
func sameQuestions(a, b []question) bool {
	return slices.EqualFunc(a, b, func(x, y question) bool {
		return x.oracle == y.oracle && x.sum == y.sum && len(x.args) == len(y.args)
	})
}

// findOverride is resolve.FindRuleWithOverride; a recorded task folds the lookup into its question.
func findOverride(c *config.Config, spec resolve.ImportSpec, lang string) (label.Label, bool) {
	dep, ok := resolve.FindRuleWithOverride(c, spec, lang)
	if run := activeRun; recording() {
		run.lookupUnder(c)
		if spec.Lang == languageName && lang == languageName {
			run.asked.overrides = append(run.asked.overrides, overrideQuestion{spec.Imp, dep, ok, run.asked.next()})
		} else {
			ask("override", []string{spec.Lang, spec.Imp, lang}, dep.String()+"\x00"+strconv.FormatBool(ok))
		}
	}
	return dep, ok
}

// findRules is ix.FindRulesByImport; a recorded task folds the lookup into its question.
func findRules(ix *resolve.RuleIndex, c *config.Config, spec resolve.ImportSpec, lang string) []resolve.FindResult {
	var found []resolve.FindResult
	if c != nil {
		found = ix.FindRulesByImportWithConfig(c, spec, lang)
	} else {
		found = ix.FindRulesByImport(spec, lang)
	}
	if run := activeRun; recording() {
		if c != nil {
			run.lookupUnder(c)
		}
		if spec.Lang == languageName && lang == languageName {
			q := indexQuestion{imp: spec.Imp, at: run.asked.next()}
			if len(found) > 0 {
				q.first = found[0]
			}
			if len(found) > 1 {
				q.rest = slices.Clone(found[1:])
			}
			run.asked.index = append(run.asked.index, q)
		} else {
			oracle := "index"
			if c != nil {
				// A lookup with a configuration also asks the cross-resolvers.
				oracle = "index (config)"
			}
			ask(oracle, []string{spec.Lang, spec.Imp, lang}, spelledFound(found))
		}
	}
	return found
}

func spelledFound(found []resolve.FindResult) string {
	var answer []string
	for _, f := range found {
		answer = append(answer, f.Label.String(), strconv.Itoa(len(f.Embeds)))
		for _, embedded := range f.Embeds {
			answer = append(answer, embedded.String())
		}
	}
	return strings.Join(answer, "\x00")
}

func sameFound(a, b []resolve.FindResult) bool {
	return slices.EqualFunc(a, b, func(x, y resolve.FindResult) bool { return x.Label == y.Label && slices.Equal(x.Embeds, y.Embeds) })
}

func edgeArgs(e explainfiles.Edge, pkg string, m *manifest) []string {
	return []string{strconv.Itoa(int(e.Kind)), e.From, e.To, e.Specifier, pkg, manifestKey(m)}
}

// manifestKey names a manifest and a digest of its contents, which nearestManifest's answer is. It holds no 0, so an
// argument list it is in splits back apart.
func manifestKey(m *manifest) string {
	if m == nil {
		return ""
	}
	d := newHasher()
	d.strs(m.deps)
	sum := d.sum()
	return fmt.Sprintf("%q %q %x", m.dir, m.name, sum)
}

func outputsDigest(o *packageOutputs) [sha256.Size]byte {
	d := newHasher()
	d.bool(o != nil)
	if o != nil {
		d.bool(o.incomplete < 0)
		sortedEach(d, o.files, d.int)
		each(d, o.trees, func(t outputTree) {
			d.str(t.root)
			d.int(t.rule)
		})
		each(d, o.producers, func(r *rule.Rule) {
			d.str(r.Name())
			d.str(r.Kind())
		})
	}
	return d.sum()
}

// fileDigest is a BUILD file's text and the kinds and names of its rules, which a read finds a rule absent from.
func fileDigest(read fileRead) [sha256.Size]byte {
	d := newHasher()
	d.bool(read.file != nil)
	if read.file != nil {
		d.str(read.file.Pkg)
		d.int(len(read.file.Content))
		d.w.Write(read.file.Content)
	}
	each(d, read.rules, func(r *rule.Rule) {
		d.str(r.Kind())
		d.str(r.Name())
	})
	return d.sum()
}

// holdingsDigest is the stored rules by label, split by whether each is a ts_compile: the index a holdings scan reads.
func holdingsDigest(h *holdingIndex) [sha256.Size]byte {
	var compilers, others []string
	for _, found := range h.all {
		compilers = append(compilers, found.Label.String())
	}
	for _, list := range h.selves {
		for _, found := range list {
			others = append(others, found.Label.String())
		}
	}
	slices.Sort(compilers)
	slices.Sort(others)
	d := newHasher()
	d.strs(compilers)
	d.strs(others)
	return d.sum()
}

func protoIndexDigest(p *protoIndex) [sha256.Size]byte {
	d := newHasher()
	for _, providers := range []map[resolve.ImportSpec][]label.Label{p.natives, p.wrappers} {
		specs := slices.SortedFunc(maps.Keys(providers), compareImportSpec)
		each(d, specs, func(spec resolve.ImportSpec) {
			d.importSpec(spec)
			var labels []string
			for _, l := range providers[spec] {
				labels = append(labels, l.String())
			}
			slices.Sort(labels)
			d.strs(labels)
		})
	}
	return d.sum()
}

// digests are the reads by value: rules, outputs, BUILD files, the key set and the proto index, each by name.
func (rs *readSet) digests(s *programStore, r *taskRecorder) []readDigest {
	var out []readDigest
	for _, key := range slices.Sorted(maps.Keys(rs.rules)) {
		out = append(out, readDigest{"rule", key, rs.rules[key].digest})
	}
	for _, dir := range slices.Sorted(maps.Keys(rs.dirs)) {
		out = append(out, readDigest{"dir", dir, outputsDigest(rs.dirs[dir])})
	}
	for _, pkg := range slices.Sorted(maps.Keys(rs.files)) {
		out = append(out, readDigest{"file", pkg, rs.files[pkg].sum})
	}
	if rs.keys >= 0 {
		out = append(out, readDigest{"keys", "", rs.keysSum})
	}
	if rs.rulesGen >= 0 {
		out = append(out, readDigest{"proto", "", rs.protoSum})
	}
	return out
}

// configSum digests a package's configuration by value, through the digest of the proto store its packages share. The
// lockfile is not in it: the lock questions cover what a task reads of it.
func (r *taskRecorder) configSum(c *config.Config) [sha256.Size]byte {
	return r.memo(c, func() [sha256.Size]byte {
		tc := getConfig(c)
		own := *tc
		own.lock, own.protos = nil, nil
		hasLock := tc.lock != nil
		shared := *c
		shared.Exts = map[string]any{}
		maps.Copy(shared.Exts, c.Exts)
		shared.Exts[languageName] = &own
		shared.Repos = nil
		d := newHasher()
		d.config(&shared)
		r.opaque[c] = d.opaque
		var first *rule.Rule
		if len(c.Repos) > 0 {
			first = c.Repos[0]
		}
		repos := r.memo(reposKey{first, len(c.Repos)}, func() [sha256.Size]byte {
			e := newHasher()
			e.repos(c.Repos)
			return e.sum()
		})
		d.w.Write(repos[:])
		protos := r.memo(tc.protos, func() [sha256.Size]byte {
			e := newHasher()
			e.protoStore(tc.protos)
			return e.sum()
		})
		d.bool(hasLock)
		d.w.Write(protos[:])
		return d.sum()
	})
}

// noteSelected asks an owner's selected imports: a task that reads the memo has not made the reads behind it.
// The digest is kept per map, which recomputing the imports replaces.
func (s *programStore) noteSelected(pkg string, owner *rule.Rule, imports map[resolve.ImportSpec]bool) {
	if !recording() {
		return
	}
	at, key := reflect.ValueOf(imports).UnsafePointer(), askKey{a: pkg, b: owner.Name()}
	if activeRun.logs.selected[at] == key {
		return
	}
	activeRun.logs.selected[at] = key
	sum := s.recorder.memo(at, func() [sha256.Size]byte {
		specs := slices.SortedFunc(maps.Keys(imports), compareImportSpec)
		d := newHasher()
		each(d, specs, func(spec resolve.ImportSpec) {
			d.importSpec(spec)
			d.bool(imports[spec])
		})
		return d.sum()
	})
	ask("selected imports", []string{pkg, owner.Name()}, string(sum[:]))
}

// noteInputs records what a recorded task starts from: its generated and merged rules, its imports with
// the program they name, and its package's configuration.
func noteInputs(c *config.Config, r, effective *rule.Rule, imps *ruleImports, from label.Label) {
	if !recording() {
		return
	}
	rec := activeRun.recorder
	d := newHasher()
	d.str(from.String())
	// Only a run that records import facts gives a task facts outputs.
	d.bool(getConfig(c).programs.importFacts != nil)
	for _, rl := range []*rule.Rule{r, effective} {
		body := ruleBodyDigest(rl)
		if body.partial() {
			bail("opaque rule")
		}
		d.w.Write(body.sum[:])
	}
	d.str(rec.programSum(imps.program))
	each(d, imps.candidates, d.candidate)
	d.strs(imps.sources)
	each(d, imps.edges, d.edge)
	d.str(imps.config)
	d.strs(imps.deps)
	if drops := imps.srcDrops; drops != nil {
		d.strs(drops.dropped)
		sum := ruleDigest(drops.have)
		d.w.Write(sum[:])
	}
	activeRun.inputs = d.sum()
	rec.noteConfig(c, from.Pkg)
}

// noteConfig asks pkg's configuration, which a task reads for its own package and for the rules it canonicalizes.
// A quiet read's configurations are in the digest that covers it.
func (r *taskRecorder) noteConfig(c *config.Config, pkg string) {
	if activeRun.quiet > 0 || activeRun.logs.configs[pkg] == c {
		return
	}
	activeRun.logs.configs[pkg] = c
	sum := r.configSum(c)
	if r.opaque[c] != "" {
		bail("opaque config")
	}
	ask("config", []string{pkg}, string(sum[:]))
}

func (r *taskRecorder) programSum(p *program) string {
	if p == nil {
		return ""
	}
	sum := r.memo(p, func() [sha256.Size]byte { return programDigest(p) })
	return string(sum[:])
}

// add makes the entry of a task that ran captured as run.
func (r *taskRecorder) add(s *programStore, kind, key string, run *taskRun) taskEntry {
	start := time.Now()
	parent := activeRun
	activeRun = nil
	defer func() {
		activeRun = parent
		r.took += time.Since(start)
	}()
	e := taskEntry{kind: kind, key: key, inputs: run.inputs, reads: run.reads.digests(s, r), out: run.out}
	job := questionJob{len(r.entries), run.asked, run.never, run.neverAt}
	if r.verify {
		done := r.questions.answer(job)
		e.questions, e.never = done.questions, done.never
	} else {
		r.queue(job)
	}
	r.entries = append(r.entries, e)
	return e
}

// previous's held is whether every read the entry recorded holds in the state the task starts from.
func (r *taskRecorder) previous(s *programStore, kind, key string) (e taskEntry, found, held bool) {
	if !r.verify || r.store == nil {
		return
	}
	store := r.store.load()
	if store == nil {
		return
	}
	if e, found = store.entry(taskID{kind, key}); !found {
		return
	}
	parent := activeRun
	activeRun = nil
	defer func() { activeRun = parent }()
	return e, true, s.storedReadsHold(r, e.reads)
}

// storedReadsHold is whether each read, by value, is what the state holds now.
func (s *programStore) storedReadsHold(r *taskRecorder, reads []readDigest) bool {
	g := s.emission
	for _, read := range reads {
		var now [sha256.Size]byte
		switch read.category {
		case "rule":
			now, _ = r.ruleSum(g, read.name, g.rules[read.name])
		case "dir":
			_, outputs := s.observedOutputs(read.name)
			now = outputsDigest(outputs)
		case "file":
			f := g.files[read.name]
			var rules []*rule.Rule
			if f != nil {
				rules = f.Rules
			}
			now = fileDigest(fileRead{file: f, rules: rules})
		case "keys":
			now = r.holdingsSum(s.storedHoldings(s.repoConfig.RepoName))
		case "proto":
			now = r.protoIndexSum(s.protoProviderIndex())
		}
		if now != read.digest {
			return false
		}
	}
	return true
}

// judge tallies a task against the last pass's entry. A task whose inputs, reads and questions all hold
// must repeat its outputs; when it does not, a read is missing from the entry.
func (r *taskRecorder) judge(now, before taskEntry, found, held bool, took time.Duration) {
	if !r.verify {
		return
	}
	outcome := "clean"
	switch {
	case now.never != "":
		outcome = "not storable"
	case !found:
		outcome = "new"
	case !held:
		outcome = "stale reads"
	case now.inputs != before.inputs:
		outcome = "stale inputs"
	case !sameQuestions(now.questions, before.questions):
		outcome = "stale questions"
	}
	r.tally[outcome]++
	r.spent[outcome] += took
	if outcome != "clean" {
		return
	}
	if was, is, differs := firstDifference(before.out, now.out); differs {
		if r.store != nil {
			r.store.drop()
		}
		memoFailed("typescript: -ts_memo=verify: missing read: %s %s: stored %s, now %s", now.kind, now.key, was, is)
	}
}

// startStore keeps this pass's entries; with no entry, as when Resolve replayed a record, the last pass's stay.
func (r *taskRecorder) startStore() {
	r.settle()
	if r != nil && r.store != nil && len(r.entries) > 0 {
		r.store.startWrite(r.questions.questionArgs, r.entries)
	}
}

func (r *taskRecorder) waitStore() {
	if r != nil && r.store != nil {
		r.store.write.Wait()
	}
}

func counted(counts map[string]int) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%d %s", counts[name], name))
	}
	return strings.Join(parts, ", ")
}

func (r *taskRecorder) report() {
	r.settle()
	never, oracles := map[string]int{}, map[string]int{}
	var storable, reads, questions int
	for _, e := range r.entries {
		reads += len(e.reads)
		for _, q := range e.questions {
			questions += len(q.args)
			oracles[q.oracle] += len(q.args)
		}
		if e.never == "" {
			storable++
		} else {
			never[e.never]++
		}
	}
	log.Printf("typescript: -ts_memo=%s: %d tasks (%d ms), %d storable, never stored: %s; %d reads, %d questions (%s), %d ms recording",
		memoMode, len(r.entries), r.inTasks.Milliseconds(), storable, counted(never), reads, questions, counted(oracles), r.took.Milliseconds())
	if r.verify {
		var parts []string
		for _, outcome := range slices.Sorted(maps.Keys(r.tally)) {
			parts = append(parts, fmt.Sprintf("%d %s (%d ms)", r.tally[outcome], outcome, r.spent[outcome].Milliseconds()))
		}
		log.Printf("typescript: -ts_memo=verify: %s", strings.Join(parts, ", "))
	}
}

// coldMemos makes a recorded task reach every value itself: the memos over listings and outputs would
// otherwise answer without the task asking, or reading, what stands behind the answer.
func (s *programStore) coldMemos() {
	if s.recorder != nil && s.emission != nil {
		s.memo = nil
	}
}
