package typescript

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"

	"github.com/mikn/rules_typescript/gazelle/incremental"
	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// fastPathMode is -ts_fastpath: on, verify or off.
var fastPathMode = "on"

// importFactsVersion names the shape of importFacts; a record of another shape is never used.
const importFactsVersion = 2

// importFacts is what one run's Resolve phase did with each listing's edges: enough to
// prove that an import between files one rule already compiles changes nothing.
type importFacts struct {
	Version int
	// Incomplete says why these facts cannot stand for the run; "" when they can.
	Incomplete string
	// Files holds every repository-relative path the facts name; the rest index it.
	Files      []string
	Listings   []listingFacts
	Selections []selectionFacts
	Resolves   []resolveFacts
	// Programs are the listings each package's srcs came from.
	Programs []programFacts
	// Outputs are the compiled files package manifests name, which make their owner emit.
	Outputs []string
	// Generated are the walked packages whose BUILD files the run generates.
	Generated []string
	// Named are the sources the extension read by a name it was given, not
	// through a listing or an edge.
	Named []string
	// Excludes are the walk's exclude patterns, Overrides the TypeScript resolve
	// directives, and Produced and Trees the files and directories BUILD files declare as outputs.
	Excludes, Overrides, Produced, Trees []string
}

// listingFacts is one listing the run used and the TypeScript sources it read.
type listingFacts struct {
	Key     string
	Sources []int32
	// Listed are the sources among them the listing holds, which existed when it was made.
	Listed []int32
	// Roots are the directories whose names its include globs enumerated, and
	// ReadDirs those holding a first-party path it read or probed.
	Roots, ReadDirs []int32
	// A files-list projection roots Base's roots plus more: its package's kept
	// sources, and with WithSrcs also package SrcsOf's srcs.
	Base, SrcsOf string
	WithSrcs     bool
}

type programFacts struct {
	Dir, Listing string
}

// selectionFacts is one selectProgram call: the listed files it did not reach and
// the resolution blocks it replaced the compiler's answer for.
type selectionFacts struct {
	Listing   string
	Unreached []int32
	Selected  []candidateKey
}

type candidateKey struct {
	From      int32
	Specifier string
	Block     int
}

// resolveFacts is one resolveEdges call: the deps it took unconditionally from
// generation, and the walks it made over a program's edges.
type resolveFacts struct {
	Unconditional []string
	Walks         []walkFacts
}

// walkFacts is one walk: the sources whose edges started it, every first-party file it
// reached, and what each edge from a starting source resolved to, by target.
type walkFacts struct {
	Listing      string
	Config       bool
	Initial      []int32
	Reached      []int32
	ReachedStore []int32
	Outcomes     []edgeOutcome
}

// edgeOutcome is what edgeDep answered for every starting edge into To: Owned when
// the answer came from the rule index's ownership, Conflict when two answers differed.
type edgeOutcome struct {
	To       int32
	Dep      string
	Source   bool
	Owned    bool
	Conflict bool
}

// importFactsRecorder collects importFacts while the Resolve phase runs.
type importFactsRecorder struct {
	mu    sync.Mutex
	ids   map[string]int32
	facts importFacts
	// replay holds the facts of a resolve record this run replays, which stand for the phase it skips.
	replay    []byte
	replaying bool
	sealed    func() []byte
	programs  map[string]string
	derived   map[string]derivation
	// sealing reads the store's manifest outputs and generated packages when the facts are sealed.
	sealing func() (outputs, generated []string)
}

type derivation struct {
	base     string
	srcsOf   string
	withSrcs bool
}

func newImportFactsRecorder(sealing func() (outputs, generated []string)) *importFactsRecorder {
	r := &importFactsRecorder{ids: map[string]int32{}, facts: importFacts{Version: importFactsVersion},
		programs: map[string]string{}, derived: map[string]derivation{}, sealing: sealing}
	r.sealed = sync.OnceValue(r.encode)
	return r
}

// program notes the listing a package's srcs now come from.
func (r *importFactsRecorder) program(p *program) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.programs[p.dir] = p.key
}

// configured notes what a BUILD file declares that a source it never named
// could meet: exclude patterns, TypeScript resolve overrides and declared outputs.
func (r *importFactsRecorder) configured(rel string, f *rule.File) {
	if r == nil || f == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range f.Directives {
		switch d.Key {
		case "exclude":
			r.facts.Excludes = append(r.facts.Excludes, path.Join(rel, d.Value))
		case "resolve", "resolve_regexp":
			// resolve src-lang [import-lang] import label: either language may be this one.
			if fields := strings.Fields(d.Value); len(fields) > 0 && (fields[0] == languageName || len(fields) == 4 && fields[1] == languageName) {
				r.facts.Overrides = append(r.facts.Overrides, rel+": "+d.Value)
			}
		}
	}
	for _, declared := range f.Rules {
		for _, attr := range []string{"out", "outs", "out_dir"} {
			var values []string
			literal := true
			switch expr := declared.Attr(attr).(type) {
			case nil:
				continue
			case *bzl.StringExpr:
				values = []string{expr.Value}
			case *bzl.ListExpr:
				for _, item := range expr.List {
					str, ok := item.(*bzl.StringExpr)
					literal = literal && ok
					if ok {
						values = append(values, str.Value)
					}
				}
			default:
				literal = false
			}
			// An output the file does not spell out could be anything in the package.
			if !literal {
				r.facts.Trees = append(r.facts.Trees, rel)
				continue
			}
			for _, v := range values {
				if attr == "out_dir" {
					r.facts.Trees = append(r.facts.Trees, path.Join(rel, v))
				} else {
					r.facts.Produced = append(r.facts.Produced, path.Join(rel, strings.TrimPrefix(v, ":")))
				}
			}
		}
	}
}

// rooted notes that listed is base's program with more roots: the package's kept
// sources, and with withSrcs the srcs of package srcsOf.
func (r *importFactsRecorder) rooted(listed, base *program, srcsOf string, withSrcs bool) {
	if r == nil || listed.key == "" || !strings.Contains(listed.key, "-") {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d := derivation{base: base.key, srcsOf: srcsOf, withSrcs: withSrcs}
	if was, seen := r.derived[listed.key]; base.key == "" || seen && was != d {
		r.derived[listed.key] = derivation{}
		return
	}
	r.derived[listed.key] = d
}

// replayed makes the recorder stand for a Resolve phase this run replays from
// a record: its facts are the record's, or incomplete when it kept none.
func (r *importFactsRecorder) replayed(facts []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replay, r.replaying = facts, true
}

// id interns file; the caller holds mu.
func (r *importFactsRecorder) id(file string) int32 {
	if id, ok := r.ids[file]; ok {
		return id
	}
	id := int32(len(r.facts.Files))
	r.ids[file] = id
	r.facts.Files = append(r.facts.Files, file)
	return id
}

func (r *importFactsRecorder) ids32(files []string) []int32 {
	out := make([]int32, 0, len(files))
	for _, f := range files {
		out = append(out, r.id(f))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (r *importFactsRecorder) incomplete(why string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.facts.Incomplete == "" {
		r.facts.Incomplete = why
	}
}

// selection notes one selectProgram call over a listing's files.
func (r *importFactsRecorder) selection(p *program, listed []string, reached map[string]bool, selected []candidateKeyText) {
	if r == nil {
		return
	}
	if p.key == "" {
		if len(p.Edges) > 0 {
			r.incomplete("a program with edges was selected without a cached listing")
		}
		return
	}
	var unreached []string
	for _, f := range listed {
		if firstParty(f) && !reached[f] {
			unreached = append(unreached, f)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := selectionFacts{Listing: p.key, Unreached: r.ids32(unreached)}
	for _, k := range selected {
		s.Selected = append(s.Selected, candidateKey{From: r.id(k.from), Specifier: k.specifier, Block: k.block})
	}
	r.facts.Selections = append(r.facts.Selections, s)
}

type candidateKeyText struct {
	from, specifier string
	block           int
}

// resolveCall is one resolveEdges call being recorded; nil when nothing records.
type resolveCall struct {
	r     *importFactsRecorder
	facts resolveFacts
}

func (r *importFactsRecorder) beginResolve() *resolveCall {
	if r == nil {
		return nil
	}
	return &resolveCall{r: r}
}

// unconditional notes a dep the call takes from generation, whatever its edges.
func (rc *resolveCall) unconditional(dep string) {
	if rc != nil {
		rc.facts.Unconditional = append(rc.facts.Unconditional, dep)
	}
}

func (rc *resolveCall) end() {
	if rc == nil {
		return
	}
	slices.Sort(rc.facts.Unconditional)
	rc.facts.Unconditional = slices.Compact(rc.facts.Unconditional)
	rc.r.mu.Lock()
	defer rc.r.mu.Unlock()
	rc.r.facts.Resolves = append(rc.r.facts.Resolves, rc.facts)
}

// walkRecord is one walk being recorded; normalize spells a dep as the walking rule's deps are keyed.
type walkRecord struct {
	call      *resolveCall
	listing   string
	config    bool
	initial   []string
	outcomes  map[string]edgeOutcome
	normalize func(string) string
}

func (rc *resolveCall) beginWalk(p *program, initial []string, config bool, normalize func(string) string) *walkRecord {
	if rc == nil || p == nil {
		return nil
	}
	if p.key == "" {
		if len(p.Edges) > 0 {
			rc.r.incomplete("a program with edges was walked without a cached listing")
		}
		return nil
	}
	return &walkRecord{call: rc, listing: p.key, config: config, initial: initial, outcomes: map[string]edgeOutcome{}, normalize: normalize}
}

// outcome notes what a starting edge into to resolved to.
func (w *walkRecord) outcome(to, dep string, source, owned bool) {
	if w == nil || !firstParty(to) {
		return
	}
	next := edgeOutcome{Dep: dep, Source: source, Owned: owned}
	if was, seen := w.outcomes[to]; seen && was != next {
		next = was
		next.Conflict = true
	}
	w.outcomes[to] = next
}

func (w *walkRecord) end(reached map[string]bool, reachedStore map[string]bool) {
	if w == nil {
		return
	}
	r := w.call.r
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := func(m map[string]bool) []string {
		var out []string
		for f := range m {
			if firstParty(f) {
				out = append(out, f)
			}
		}
		return out
	}
	facts := walkFacts{Listing: w.listing, Config: w.config, Initial: r.ids32(w.initial),
		Reached: r.ids32(keys(reached)), ReachedStore: r.ids32(keys(reachedStore))}
	for to, o := range w.outcomes {
		o.To, o.Dep = r.id(to), w.normalize(o.Dep)
		facts.Outcomes = append(facts.Outcomes, o)
	}
	slices.SortFunc(facts.Outcomes, func(a, b edgeOutcome) int { return cmp.Compare(a.To, b.To) })
	w.call.facts.Walks = append(w.call.facts.Walks, facts)
}

// seal is the recorded facts, encoded, with every listing the run used.
func (r *importFactsRecorder) seal() []byte {
	if r == nil {
		return nil
	}
	return r.sealed()
}

func (r *importFactsRecorder) encode() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replaying {
		if facts := r.replayedFacts(); facts != nil {
			return facts
		}
		r.facts = importFacts{Version: importFactsVersion, Incomplete: "the replayed resolve record keeps no usable import facts"}
	}
	entries := map[string]*cachedListing{}
	for k, v := range loadedListings.Range {
		entries[k.(string)] = v.(*cachedListing)
	}
	for _, key := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[key]
		var sources, listed, roots, dirs []string
		for _, f := range entry.Listing.Files {
			if firstParty(f) && patchableSource(f) {
				listed = append(listed, f)
			}
		}
		for _, read := range entry.Reads {
			rel, ok := repoRelative(entry.Root, read)
			if !ok || !firstParty(rel) {
				continue
			}
			if patchableSource(rel) {
				sources = append(sources, rel)
			}
			dirs = append(dirs, parentDir(rel))
		}
		for _, dir := range entry.Dirs {
			if rel, ok := repoRelative(entry.Root, dir); ok {
				roots = append(roots, rel)
			}
		}
		d := r.derived[key]
		r.facts.Listings = append(r.facts.Listings, listingFacts{Key: key, Sources: r.ids32(sources), Listed: r.ids32(listed),
			Roots: r.ids32(roots), ReadDirs: r.ids32(dirs), Base: d.base, SrcsOf: d.srcsOf, WithSrcs: d.withSrcs})
	}
	for _, dir := range slices.Sorted(maps.Keys(r.programs)) {
		r.facts.Programs = append(r.facts.Programs, programFacts{Dir: dir, Listing: r.programs[dir]})
	}
	if r.sealing != nil {
		r.facts.Outputs, r.facts.Generated = r.sealing()
	}
	if in := runInputs; in != nil {
		in.mu.Lock()
		for abs := range in.named {
			if rel, ok := repoRelative(in.repoRoot, abs); ok {
				r.facts.Named = append(r.facts.Named, rel)
			}
		}
		in.mu.Unlock()
		slices.Sort(r.facts.Named)
	}
	data, err := r.facts.encode()
	if err != nil {
		if listingVerbose {
			log.Printf("typescript: fast path: no import facts recorded: %v", err)
		}
		return nil
	}
	return data
}

func (f *importFacts) encode() ([]byte, error) {
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(f); err != nil {
		return nil, fmt.Errorf("encoding import facts: %w", err)
	}
	return b.Bytes(), nil
}

// replayedFacts is the replayed record's facts when they decode and name every
// listing this run read, else nil; the caller holds mu.
func (r *importFactsRecorder) replayedFacts() []byte {
	facts, err := decodeImportFacts(r.replay)
	if err != nil {
		return nil
	}
	known := map[string]bool{}
	for _, l := range facts.Listings {
		known[l.Key] = true
	}
	for k := range loadedListings.Range {
		if !known[k.(string)] {
			return nil
		}
	}
	return r.replay
}

func repoRelative(root, abs string) (string, bool) {
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func decodeImportFacts(data []byte) (*importFacts, error) {
	if len(data) == 0 {
		return nil, errNoFacts
	}
	var facts importFacts
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&facts); err != nil {
		return nil, fmt.Errorf("decoding import facts: %w", err)
	}
	if facts.Version != importFactsVersion {
		return nil, fmt.Errorf("import facts of version %d, want %d", facts.Version, importFactsVersion)
	}
	if err := facts.validate(); err != nil {
		return nil, err
	}
	return &facts, nil
}

// validate checks that every index names a file and every list is in the
// order the fast path searches it.
func (f *importFacts) validate() error {
	n := int32(len(f.Files))
	ok := func(ids []int32) bool {
		return slices.IsSorted(ids) && !slices.ContainsFunc(ids, func(id int32) bool { return id < 0 || id >= n })
	}
	for _, l := range f.Listings {
		if !ok(l.Sources) || !ok(l.Listed) || !ok(l.Roots) || !ok(l.ReadDirs) {
			return errFactsIndex
		}
	}
	for _, s := range f.Selections {
		if !ok(s.Unreached) || slices.ContainsFunc(s.Selected, func(k candidateKey) bool { return k.From < 0 || k.From >= n }) {
			return errFactsIndex
		}
	}
	for _, r := range f.Resolves {
		for _, w := range r.Walks {
			if !ok(w.Initial) || !ok(w.Reached) || !ok(w.ReachedStore) ||
				slices.ContainsFunc(w.Outcomes, func(o edgeOutcome) bool { return o.To < 0 || o.To >= n }) ||
				!slices.IsSortedFunc(w.Outcomes, func(a, b edgeOutcome) int { return cmp.Compare(a.To, b.To) }) {
				return errFactsIndex
			}
		}
	}
	return nil
}

var errFactsIndex = errors.New("import facts name a file they do not hold, or hold a list out of order")

var errNoFacts = errors.New("the record keeps no import facts")

// ---- the record's input sums ------------------------------------------------

// inputSums are a record's inputs' lines, eight bytes of SHA-256 each, in the
// record's order: dirs (with listed sources' stats left out), files, reads, roots.
func inputSums(l inputLines) []uint64 {
	sums := make([]uint64, 0, len(l.dirs)+len(l.files)+len(l.reads)+len(l.roots))
	for _, line := range l.dirs {
		sums = append(sums, lineSum(dirShape(line)))
	}
	for _, list := range [][]string{l.files, l.reads, l.roots} {
		for _, line := range list {
			sums = append(sums, lineSum(line))
		}
	}
	return sums
}

func lineSum(line string) uint64 {
	sum := sha256.Sum256([]byte(line))
	return binary.BigEndian.Uint64(sum[:8])
}

// dirShape is a dirLine without the stat of each TypeScript source in it: what
// the fast path requires unchanged, since a source's content reaches the output
// only through the listings that read it.
func dirShape(line string) string {
	var b strings.Builder
	for entry := range strings.SplitSeq(line, "\n") {
		if entry == "" {
			continue
		}
		if name, ok := sourceEntryName(entry); ok {
			fmt.Fprintf(&b, "f %s\n", name)
			continue
		}
		b.WriteString(entry)
		b.WriteByte('\n')
	}
	return b.String()
}

// sourceEntryName is the name of a dirLine "f <name> <stat>" entry for a patchable
// source; a stat is five fields.
func sourceEntryName(entry string) (string, bool) {
	rest, ok := strings.CutPrefix(entry, "f ")
	if !ok {
		return "", false
	}
	cut := len(rest)
	for range 5 {
		cut = strings.LastIndexByte(rest[:cut], ' ')
		if cut < 0 {
			return "", false
		}
	}
	name := rest[:cut]
	return name, patchableSource(name)
}

// ---- the decision -------------------------------------------------------------

// Reason codes for a decline, logged under -ts_verbose.
const (
	declineNoFacts          = "no-facts"
	declineFactsIncomplete  = "facts-incomplete"
	declineInputs           = "inputs"
	declineNotSource        = "not-source"
	declineNoListing        = "no-listing"
	declineEntry            = "entry"
	declineOtherRead        = "other-read"
	declinePatch            = "patch-refused"
	declineListingShape     = "listing-shape"
	declineEdgeRemoved      = "edge-removed"
	declineEdgeKind         = "edge-kind"
	declineSpecifierReused  = "specifier-reused"
	declineCandidate        = "candidate"
	declineSelectionReach   = "selection-reach"
	declineSelectionWitness = "selection-witness"
	declineWalkStore        = "walk-store"
	declineWalkConfig       = "walk-config"
	declineWalkWitness      = "walk-witness"
	declineWalkConflict     = "walk-conflict"
	declineWalkNotOwned     = "walk-not-owned"
	declineWalkOrder        = "walk-order"
	declineWalkConditional  = "walk-conditional"
	declineVerboseLog       = "verbose-log"
	// A gained, renamed or deleted source's own reasons.
	declineMode      = "mode"
	declineOverride  = "override"
	declineExcluded  = "excluded"
	declineGenerated = "generated"
	declineEmission  = "emission"
	declineOwner     = "owner"
	declineKept      = "kept"
	declineFilesList = "files-list"
	declineNotListed = "not-listed"
)

// decline is why the fast path does not stand for this run.
type decline struct{ code, detail string }

func declined(code, format string, a ...any) *decline {
	return &decline{code: code, detail: fmt.Sprintf(format, a...)}
}

// fastPatch is one listing the edit stales, patched as a full run patches it,
// beside the listing and candidates its entry held before. A files-list
// projection whose list the edit changes is kept under newKey.
type fastPatch struct {
	key, newKey string
	entry       *cachedListing
	p           *program
	listing     explainfiles.Listing
	candidates  []cachedCandidate
}

// fastPathDecision is an accepted edit: the patched listings and what the
// record's inputs read now.
type fastPathDecision struct {
	patches []fastPatch
	lines   inputLines
	// files are what patching read, which a full run notes as the extension's own reads.
	files map[string]string
	// changed are the inputs whose lines moved; stamps the newest change among all inputs.
	changed []string
	stamps  *newest
	// edits are the BUILD files a gained, renamed or deleted source changes.
	edits []*packageEdit
	// buildNames and skipDir, the listing cache, are what a walked directory's line is read with.
	buildNames []string
	skipDir    string
}

// editedDirLine is dir's line once e is written: was with its BUILD file's
// content sum moved, which it must be as the directory reads now.
func (d *fastPathDecision) editedDirLine(dir, was string, e *packageEdit) (string, bool) {
	name := filepath.Base(e.file)
	prefix := fmt.Sprintf("b %s %o ", name, e.mode)
	var b strings.Builder
	moved := false
	for entry := range strings.SplitSeq(was, "\n") {
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, prefix) {
			entry = fmt.Sprintf("%s%x", prefix, sha256.Sum256(e.content))
			moved = true
		}
		b.WriteString(entry)
		b.WriteByte('\n')
	}
	now, _, err := dirLine(dir, d.buildNames, d.skipDir)
	return now, moved && err == nil && now == b.String()
}

// decideFastPath says whether rec still stands for the tree although its inputs
// moved: every change is an edit to a TypeScript source whose listings patch,
// or a library source gained or lost beside a source of the same rule, and
// every import the edit adds or drops lies between files that every rule
// reading the importer already resolves the same way. A gained or lost source
// changes its rule's srcs and nothing else, which the decision's edits write.
func decideFastPath(rec *runRecord, repoRoot string, buildNames []string, dirs []string, stamps *newest, cliExcludes string) (*fastPathDecision, *decline) {
	if len(rec.facts) == 0 || len(rec.sums) != len(rec.dirs)+len(rec.files)+len(rec.reads)+len(rec.roots) {
		return nil, declined(declineNoFacts, "the record keeps no import facts")
	}
	var moved []int
	for i := range rec.dirs {
		if lineSum(dirShape(dirs[i])) != rec.sums[i] {
			moved = append(moved, i)
		}
	}
	facts, err := decodeImportFacts(rec.facts)
	if err != nil {
		return nil, declined(declineNoFacts, "%v", err)
	}
	if facts.Incomplete != "" {
		return nil, declined(declineFactsIncomplete, "%s", facts.Incomplete)
	}
	var delta *sourceDelta
	if len(moved) > 0 {
		var why *decline
		if delta, why = sourceChanges(rec, facts, repoRoot, dirs, moved); why != nil {
			return nil, why
		}
	}
	lines := inputLines{dirs: dirs, files: fileLines(rec.files, stamps), reads: listingReadLines(rec.reads, stamps), roots: rootLines(rec.roots)}
	sums := inputSums(lines)
	// A walked directory's shape holds its BUILD file's content, so a BUILD file read by stat there may move.
	walked := setOf(rec.dirs)
	changed := map[string]bool{}
	at := len(rec.dirs)
	for li, list := range [][]string{rec.files, rec.reads} {
		for i, file := range list {
			if sums[at+i] == rec.sums[at+i] || slices.Contains(buildNames, filepath.Base(file)) && walked[filepath.Dir(file)] {
				continue
			}
			if rel, ok := repoRelative(repoRoot, file); li == 0 && ok && delta.has(rel) && has(facts.Named, rel) {
				return nil, declined(declineOtherRead, "the extension read %s, which the edit gained or lost, by a name it was given", rel)
			}
			changed[file] = true
		}
		at += len(list)
	}
	changedRoots := map[string]bool{}
	rootSums := map[string]uint64{}
	for i, root := range rec.roots {
		rootSums[root] = rec.sums[at+i]
		if sums[at+i] == rec.sums[at+i] {
			continue
		}
		rel, ok := repoDir(repoRoot, root)
		covers := false
		for f := range mapsKeys(delta.gained, delta.lost) {
			covers = covers || ok && within(f, rel)
		}
		if delta.empty() || !covers {
			return nil, declined(declineInputs, "the names under %s changed", root)
		}
		changedRoots[rel] = true
	}
	decision := &fastPathDecision{lines: lines, files: map[string]string{}, changed: slices.Sorted(maps.Keys(changed)), stamps: stamps}
	if len(decision.changed) == 0 && delta.empty() {
		return decision, nil
	}
	sources := map[string][]int{}
	for i, l := range facts.Listings {
		for _, id := range l.Sources {
			sources[facts.Files[id]] = append(sources[facts.Files[id]], i)
		}
	}
	affected := map[int]bool{}
	changedRel := map[string]bool{}
	for _, abs := range decision.changed {
		rel, ok := repoRelative(repoRoot, abs)
		if ok && delta.has(rel) {
			continue
		}
		if !ok || !patchableSource(rel) {
			return nil, declined(declineNotSource, "%s changed and is no TypeScript source", abs)
		}
		if info, err := os.Lstat(abs); err != nil || !info.Mode().IsRegular() {
			return nil, declined(declineNotSource, "%s is no longer a regular file", rel)
		}
		if len(sources[rel]) == 0 {
			return nil, declined(declineNoListing, "%s is no source of a listing the recorded run used", rel)
		}
		for _, i := range sources[rel] {
			affected[i] = true
		}
		changedRel[rel] = true
	}
	var proof *deltaProof
	if !delta.empty() {
		var why *decline
		if proof, why = proveDelta(rec, facts, repoRoot, buildNames, dirs, delta, cliExcludes); why != nil {
			return nil, why
		}
		deltaDirs := map[string]bool{}
		for f := range mapsKeys(delta.gained, delta.lost) {
			deltaDirs[parentDir(f)] = true
			for _, i := range sources[f] {
				affected[i] = true
			}
		}
		for i, l := range facts.Listings {
			if slices.ContainsFunc(l.Roots, func(id int32) bool { return changedRoots[facts.Files[id]] }) ||
				slices.ContainsFunc(l.ReadDirs, func(id int32) bool { return deltaDirs[facts.Files[id]] }) {
				affected[i] = true
			}
		}
	}
	readSums := map[string]uint64{}
	for i, read := range rec.reads {
		readSums[read] = rec.sums[len(rec.dirs)+len(rec.files)+i]
	}
	ids := make(map[string]int32, len(facts.Files))
	for i, f := range facts.Files {
		ids[f] = int32(i)
	}
	// Patching notes its reads as a full run's extension would, and the record takes them
	// on; the gate runs before anything else that reads runInputs.
	capture := &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}, repoRoot: repoRoot}
	saved := runInputs
	runInputs = capture
	defer func() { runInputs = saved }()
	reads := listingReads{repoRoot: repoRoot, readSums: readSums, rootSums: rootSums, changed: changedRel, delta: delta, changedRoots: changedRoots}
	if proof != nil {
		reads.owners = map[string]string{}
		for f, e := range proof.owners {
			reads.owners[f] = e.pkg
		}
	}
	patches, why := patchListings(facts, affected, reads)
	if why != nil {
		return nil, why
	}
	var mates map[string][]string
	if proof != nil {
		mates = proof.mates
	}
	whys := make([]*decline, len(patches))
	parallel(len(patches), func(i int) {
		whys[i] = checkPatch(facts, ids, patches[i], changedRel, delta, mates)
	})
	for _, why := range whys {
		if why != nil {
			return nil, why
		}
	}
	if proof != nil {
		if why := proof.listed(facts, patches, delta); why != nil {
			return nil, why
		}
		for _, e := range proof.edits {
			if why := e.render(); why != nil {
				return nil, why
			}
		}
		decision.edits = proof.edits
	}
	decision.patches = patches
	capture.mu.Lock()
	maps.Copy(decision.files, capture.files)
	capture.mu.Unlock()
	return decision, nil
}

// listingReads is what a listing's entry must have read for the record to
// stand for it: every read the edit left alone reads the same now, and every
// one it touched read what the record says.
type listingReads struct {
	repoRoot           string
	readSums, rootSums map[string]uint64
	changed            map[string]bool
	delta              *sourceDelta
	changedRoots       map[string]bool
	// owners are each gained source's package.
	owners map[string]string
}

// patchListings patches every affected listing from its entry and every
// files-list projection whose list the edit changes, a projection after the
// listing it adds roots to, as a full run would ask for each.
func patchListings(facts *importFacts, affected map[int]bool, reads listingReads) ([]fastPatch, *decline) {
	byKey := map[string]int{}
	for i, l := range facts.Listings {
		byKey[l.Key] = i
	}
	structural := !reads.delta.empty()
	pending := maps.Clone(affected)
	if structural {
		for i, l := range facts.Listings {
			if l.Base != "" {
				pending[i] = true
			}
		}
	}
	done := map[int]*fastPatch{}
	// unchanged are the entries of listings the edit leaves as they were.
	unchanged := map[int]*cachedListing{}
	// Every entry a round may read is decoded at once, side by side.
	wanted := map[int]bool{}
	for i := range pending {
		wanted[i] = true
		if b, known := byKey[facts.Listings[i].Base]; known && structural {
			wanted[b] = true
		}
	}
	order := slices.Sorted(maps.Keys(wanted))
	entries := make([]*cachedListing, len(order))
	errs := make([]error, len(order))
	parallel(len(order), func(j int) {
		entries[j], errs[j] = readListing(filepath.Join(listingCacheDir, facts.Listings[order[j]].Key))
	})
	type decodedEntry struct {
		entry *cachedListing
		err   error
	}
	decoded := map[int]decodedEntry{}
	for j, i := range order {
		decoded[i] = decodedEntry{entries[j], errs[j]}
	}
	load := func(i int) (*cachedListing, *decline) {
		d, seen := decoded[i]
		if !seen {
			d.entry, d.err = readListing(filepath.Join(listingCacheDir, facts.Listings[i].Key))
		}
		if d.err != nil {
			return nil, declined(declineEntry, "listing %s: %v", facts.Listings[i].Key, d.err)
		}
		return d.entry, nil
	}
	for len(pending) > 0 {
		var ready []int
		for i := range pending {
			l := facts.Listings[i]
			if !structural || l.Base == "" {
				ready = append(ready, i)
				continue
			}
			b, known := byKey[l.Base]
			if !known {
				return nil, declined(declineFilesList, "listing %s adds roots to %s, which the record does not name", l.Key, l.Base)
			}
			if pending[b] {
				continue
			}
			if done[b] == nil && unchanged[b] == nil {
				entry, why := load(b)
				if why != nil {
					return nil, why
				}
				unchanged[b] = entry
			}
			ready = append(ready, i)
		}
		if len(ready) == 0 {
			return nil, declined(declineFilesList, "the record's files lists derive from one another in a cycle")
		}
		slices.Sort(ready)
		patched := make([]*fastPatch, len(ready))
		kept := make([]*cachedListing, len(ready))
		whys := make([]*decline, len(ready))
		parallel(len(ready), func(j int) {
			i := ready[j]
			l := facts.Listings[i]
			entry, why := load(i)
			if why != nil {
				whys[j] = why
				return
			}
			filesList := entry.FilesList
			if structural && entry.FilesList != nil {
				if l.Base == "" {
					whys[j] = declined(declineFilesList, "listing %s is a files list the record does not derive", l.Key)
					return
				}
				b := byKey[l.Base]
				was, now := explainfiles.Listing{}, explainfiles.Listing{}
				if p := done[b]; p != nil {
					was, now = p.listing, p.p.Listing
				} else {
					was, now = unchanged[b].Listing, unchanged[b].Listing
				}
				if filesList, whys[j] = derivedFilesList(entry, l, was, now, reads.delta, reads.owners); whys[j] != nil {
					return
				}
			}
			p, why, same := patchAffected(l.Key, entry, filesList, reads)
			switch {
			case why != nil:
				whys[j] = why
			case same:
				kept[j] = entry
			default:
				p.newKey = filesListKey(l.Key, filesList)
				patched[j] = &p
			}
		})
		for j, i := range ready {
			if whys[j] != nil {
				return nil, whys[j]
			}
			if patched[j] != nil {
				done[i] = patched[j]
			} else {
				unchanged[i] = kept[j]
			}
			delete(pending, i)
		}
	}
	var patches []fastPatch
	for _, i := range slices.Sorted(maps.Keys(done)) {
		patches = append(patches, *done[i])
	}
	return patches, nil
}

// derivedFilesList is the files list a full run now asks of a projection over
// base: base's roots now, and the roots it added to base's roots before, less
// those the edit lost or base now lists; with the package's srcs, every gained
// source base does not list joins them.
func derivedFilesList(entry *cachedListing, l listingFacts, baseWas, baseNow explainfiles.Listing, delta *sourceDelta, owners map[string]string) ([]string, *decline) {
	if !slices.IsSorted(entry.FilesList) {
		return nil, declined(declineFilesList, "listing %s's files list is out of order", l.Key)
	}
	wasRoots, nowFiles := setOf(baseWas.Roots), setOf(baseNow.Files)
	roots := slices.Clone(baseNow.Roots)
	for _, f := range entry.FilesList {
		if !wasRoots[f] && !delta.isLost(f) && !nowFiles[f] {
			roots = append(roots, f)
		}
	}
	if l.WithSrcs {
		for f := range delta.gained {
			if pkg, owned := owners[f]; owned && pkg == l.SrcsOf && !nowFiles[f] {
				roots = append(roots, f)
			}
		}
	}
	slices.Sort(roots)
	return slices.Compact(roots), nil
}

// filesListKey is the cache key a files-list projection of key's family has
// for files, as listingKey names it.
func filesListKey(key string, files []string) string {
	family, _, ok := strings.Cut(key, "-")
	if !ok || files == nil {
		return key
	}
	return family + "-" + filesListDigest(files)
}

// patchAffected patches one listing the edit stales from its cache entry, which
// must be the one the record saw: every read the edit did not touch reads the
// same, and every touched source read what the record says it did. same says
// the entry still stands as it is.
func patchAffected(key string, entry *cachedListing, filesList []string, reads listingReads) (fastPatch, *decline, bool) {
	if entry.Root != reads.repoRoot || len(entry.ReadLines) != len(entry.Reads) || len(entry.DirDigests) != len(entry.Dirs) {
		return fastPatch{}, declined(declineEntry, "listing %s keeps no patch state for this root", key), false
	}
	touched := false
	for i, read := range entry.Reads {
		rel, _ := repoRelative(reads.repoRoot, read)
		if reads.changed[rel] || reads.delta.has(rel) {
			want := "file"
			if reads.delta.isGained(rel) {
				want = "absent"
			}
			sum, recorded := reads.readSums[read]
			if !recorded || sum != lineSum(entry.ReadLines[i]) || existence(entry.ReadLines[i]) != want {
				return fastPatch{}, declined(declineEntry, "listing %s read %s otherwise than the record", key, rel), false
			}
			touched = true
			continue
		}
		if listingStats.Line(read) != entry.ReadLines[i] {
			return fastPatch{}, declined(declineOtherRead, "listing %s: %s changed and the record does not say so", key, read), false
		}
	}
	for i, dir := range entry.Dirs {
		if listingNames.Digest(dir) == entry.DirDigests[i] {
			continue
		}
		rel, _ := repoRelative(reads.repoRoot, dir)
		if sum, recorded := reads.rootSums[dir]; !reads.changedRoots[rel] || !recorded || sum != lineSum(entry.DirDigests[i]) {
			return fastPatch{}, declined(declineOtherRead, "listing %s: the names under %s changed", key, dir), false
		}
		touched = true
	}
	if !touched && slices.Equal(filesList, entry.FilesList) {
		return fastPatch{}, nil, true
	}
	// The patch leaves the entry's listing and candidates as they were until remember.
	old := fastPatch{key: key, entry: entry, listing: entry.Listing, candidates: entry.Candidates}
	p, why := tryPatchListing(entry, "", filesList)
	if p == nil {
		return fastPatch{}, declined(declinePatch, "listing %s: %s", key, why), false
	}
	old.p = p
	return old, nil, false
}

// listed proves each gained source a member of its package's program, reached
// from the rule's own sources, and each lost one gone from it.
func (proof *deltaProof) listed(facts *importFacts, patches []fastPatch, delta *sourceDelta) *decline {
	programs := map[string]string{}
	for _, p := range facts.Programs {
		programs[p.Dir] = p.Listing
	}
	byKey := map[string]*fastPatch{}
	for i := range patches {
		byKey[patches[i].key] = &patches[i]
	}
	for _, f := range slices.Sorted(mapsKeys(proof.owners)) {
		e := proof.owners[f]
		patch := byKey[programs[e.pkg]]
		if patch == nil {
			return declined(declineNotListed, "%s's program, listing %q, does not change with %s", orRepoRoot(e.pkg), programs[e.pkg], f)
		}
		files := setOf(patch.p.Files)
		if delta.isLost(f) {
			if files[f] {
				return declined(declineNotListed, "%s's program still lists %s", orRepoRoot(e.pkg), f)
			}
			continue
		}
		if !files[f] {
			return declined(declineNotListed, "%s's program does not list %s", orRepoRoot(e.pkg), f)
		}
		reached := slices.Contains(patch.p.Roots, f) || slices.ContainsFunc(patch.p.Edges, func(edge explainfiles.Edge) bool {
			return edge.To == f && e.srcs[pkgRelative(e.pkg, edge.From)] && !delta.has(edge.From)
		})
		if !reached {
			return declined(declineNotListed, "%s's program lists %s only through a file %s does not compile", orRepoRoot(e.pkg), f, ruleLabel(e))
		}
	}
	return nil
}

// checkPatch proves that what the patch changed in one listing changes no
// output but the delta's srcs: the listing keeps its files and roots but for
// the sources gained and lost, its types, every other importer's edges and
// resolutions as they were, and every edge it gains or loses is a relative
// import between listed sources that each recorded selection and walk over
// this listing already treats as it treats the target, or the target's mates.
func checkPatch(facts *importFacts, ids map[string]int32, patch fastPatch, changed map[string]bool, delta *sourceDelta, mates map[string][]string) *decline {
	key := patch.key
	old, p := patch.listing, patch.p
	touched := func(f string) bool { return changed[f] || delta.has(f) }
	for _, part := range []struct {
		name  string
		equal bool
	}{
		{"files", equalExcept(old.Files, p.Files, delta.has)},
		{"roots", equalExcept(old.Roots, p.Roots, delta.has)},
		{"type entries", slices.Equal(old.Types, p.Types) && slices.Equal(old.Implicit, p.Implicit)},
		{"diagnostics", slices.Equal(old.Diagnostics, p.Diagnostics)},
		{"unresolved imports", slices.EqualFunc(old.Unresolved, p.Unresolved, unresolvedEqual)},
	} {
		if !part.equal {
			return declined(declineListingShape, "listing %s changes its %s", key, part.name)
		}
	}
	for _, f := range p.Files {
		if delta.isLost(f) {
			return declined(declineListingShape, "listing %s still lists %s, which is gone", key, f)
		}
	}
	// A patch keeps every other importer's edges and resolution blocks in order.
	if !equalExcept(old.Edges, p.Edges, func(e explainfiles.Edge) bool { return touched(e.From) }) {
		return declined(declineListingShape, "listing %s changes an edge of a file the edit did not touch", key)
	}
	if !equalExceptFunc(patch.candidates, p.candidates, func(c cachedCandidate) bool { return touched(c.From) },
		func(c resolutionCandidate) bool { return touched(c.from) }, func(a cachedCandidate, b resolutionCandidate) bool { return a == b.cached() }) {
		return declined(declineCandidate, "listing %s changes a resolution of a file the edit did not touch", key)
	}
	have := map[explainfiles.Edge]int{}
	oldSpecifiers := map[importKey]bool{}
	for _, e := range old.Edges {
		if touched(e.From) {
			have[e]++
			oldSpecifiers[importKey{e.From, e.Specifier}] = true
		}
	}
	var added, removed []explainfiles.Edge
	for _, e := range p.Edges {
		if !touched(e.From) {
			continue
		}
		if have[e] > 0 {
			have[e]--
			continue
		}
		added = append(added, e)
	}
	removedSpecifiers := map[importKey]bool{}
	for _, e := range old.Edges {
		if have[e] == 0 {
			continue
		}
		have[e]--
		if !delta.isLost(e.From) && !delta.isLost(e.To) {
			return declined(declineEdgeRemoved, "listing %s loses %s -> %s", key, e.From, e.To)
		}
		removed = append(removed, e)
		removedSpecifiers[importKey{e.From, e.Specifier}] = true
	}
	held := map[cachedCandidate]int{}
	for _, c := range patch.candidates {
		if touched(c.From) {
			held[c]++
			oldSpecifiers[importKey{c.From, c.Specifier}] = true
		}
	}
	var fresh []resolutionCandidate
	for _, c := range p.candidates {
		if !touched(c.from) {
			continue
		}
		if held[c.cached()] > 0 {
			held[c.cached()]--
			continue
		}
		fresh = append(fresh, c)
	}
	for _, c := range patch.candidates {
		if held[c] > 0 && !delta.isLost(c.From) && !removedSpecifiers[importKey{c.From, c.Specifier}] {
			return declined(declineCandidate, "listing %s drops a resolution of %q from %s", key, c.Specifier, c.From)
		}
	}
	// A gained source's edge that a lost one in its directory had to the same
	// target moved with the source: the rule's dep for it stands either way.
	type movedEdge struct {
		dir, specifier, to string
		kind               explainfiles.EdgeKind
	}
	movedFrom := func(e explainfiles.Edge) movedEdge { return movedEdge{parentDir(e.From), e.Specifier, e.To, e.Kind} }
	leaving := map[movedEdge]int{}
	for _, e := range removed {
		if delta.isLost(e.From) && !delta.has(e.To) {
			leaving[movedFrom(e)]++
		}
	}
	moved := map[movedEdge]int{}
	for _, e := range added {
		if m := movedFrom(e); delta.isGained(e.From) && !delta.has(e.To) && leaving[m] > 0 {
			leaving[m]--
			moved[m]++
		}
	}
	isMoved := func(e explainfiles.Edge, counts map[movedEdge]int) bool {
		if m := movedFrom(e); counts[m] > 0 {
			counts[m]--
			return true
		}
		return false
	}
	movedOut, movedIn := maps.Clone(moved), maps.Clone(moved)
	oldFiles := setOf(old.Files)
	for _, e := range removed {
		if why := relativeSourceEdge(e, oldFiles); why != nil {
			return why
		}
		if delta.isLost(e.From) && isMoved(e, movedOut) {
			continue
		}
		if why := checkWalks(facts, ids, key, e, nil); why != nil {
			return why
		}
	}
	if len(added) == 0 {
		if len(fresh) > 0 {
			return declined(declineCandidate, "listing %s resolves otherwise with no edge added", key)
		}
		if listingVerbose && (len(removed) > 0 || !slices.Equal(old.Files, p.Files) || !slices.Equal(old.Roots, p.Roots)) {
			return declined(declineVerboseLog, "-ts_verbose logs each listing's counts, which listing %s's edit changes", key)
		}
		return nil
	}
	listed := setOf(p.Files)
	addedSpecifiers := map[importKey]bool{}
	for _, e := range added {
		if why := relativeSourceEdge(e, listed); why != nil {
			return why
		}
		if oldSpecifiers[importKey{e.From, e.Specifier}] && !removedSpecifiers[importKey{e.From, e.Specifier}] || addedSpecifiers[importKey{e.From, e.Specifier}] {
			return declined(declineSpecifierReused, "%s imports %q more than once", e.From, e.Specifier)
		}
		addedSpecifiers[importKey{e.From, e.Specifier}] = true
	}
	// Gazelle reads the BUILD facts of every directory a listing probes; a new probe must add none.
	probed := map[string]bool{}
	for _, f := range old.Files {
		probed[parentDir(f)] = true
	}
	for _, c := range patch.candidates {
		probed[parentDir(c.Path)] = true
	}
	freshPaths := map[string]bool{}
	for _, c := range fresh {
		switch {
		case !addedSpecifiers[importKey{c.from, c.specifier}]:
			return declined(declineCandidate, "listing %s resolves %q from %s anew", key, c.specifier, c.from)
		case c.metadata || c.skipped != "":
			return declined(declineCandidate, "%s's %q probes package metadata or skips a directory", c.from, c.specifier)
		case firstParty(c.path) && !probed[parentDir(c.path)]:
			return declined(declineCandidate, "%s's %q probes %s, a directory listing %s does not", c.from, c.specifier, parentDir(c.path), key)
		case firstParty(c.path) && (slices.Contains(facts.Produced, c.path) || slices.ContainsFunc(facts.Trees, func(tree string) bool { return within(c.path, tree) })):
			return declined(declineGenerated, "%s's %q probes %s, a declared output", c.from, c.specifier, c.path)
		}
		freshPaths[c.path] = true
	}
	// Witnesses are other importers' blocks probing a path the new blocks probe.
	type blockID struct {
		from  string
		block int
	}
	probing := map[blockID]bool{}
	for _, c := range patch.candidates {
		if (!touched(c.From) || delta.isLost(c.From)) && freshPaths[c.Path] {
			probing[blockID{c.From, c.Block}] = true
		}
	}
	var near []resolutionCandidate
	for _, c := range patch.candidates {
		if probing[blockID{c.From, c.Block}] {
			near = append(near, c.resolutionCandidate())
		}
	}
	witnesses := map[string][]candidateKeyText{}
	for _, b := range candidateBlocks(near) {
		witnesses[b.probes] = append(witnesses[b.probes], b.key)
	}
	if why := checkSelections(facts, ids, key, added, candidateBlocks(fresh), witnesses, delta, mates); why != nil {
		return why
	}
	for _, e := range added {
		if delta.isGained(e.From) && isMoved(e, movedIn) {
			continue
		}
		if why := checkWalks(facts, ids, key, e, mates); why != nil {
			return why
		}
	}
	if listingVerbose {
		return declined(declineVerboseLog, "-ts_verbose logs each listing's edge count, which listing %s's new imports change", key)
	}
	return nil
}

// relativeSourceEdge refuses an edge the fast path cannot follow: anything but
// a relative import between the listing's first-party sources.
func relativeSourceEdge(e explainfiles.Edge, listed map[string]bool) *decline {
	switch {
	case e.Kind != explainfiles.Import || !strings.HasPrefix(e.Specifier, "./") && !strings.HasPrefix(e.Specifier, "../"):
		return declined(declineEdgeKind, "%s -> %q is no relative import", e.From, e.Specifier)
	case !firstParty(e.To) || isDeclarationFile(e.To) || !listed[e.To] || !patchableSource(e.To):
		return declined(declineEdgeKind, "%s -> %q reaches %s, no listed source", e.From, e.Specifier, e.To)
	}
	return nil
}

// equalExcept says whether a and b hold the same elements in the same order once
// every element skip names is left out of both.
func equalExcept[T comparable](a, b []T, skip func(T) bool) bool {
	return equalExceptFunc(a, b, skip, skip, func(x, y T) bool { return x == y })
}

func equalExceptFunc[A, B any](a []A, b []B, skipA func(A) bool, skipB func(B) bool, eq func(A, B) bool) bool {
	i, j := 0, 0
	for {
		for i < len(a) && skipA(a[i]) {
			i++
		}
		for j < len(b) && skipB(b[j]) {
			j++
		}
		if i == len(a) || j == len(b) {
			return i == len(a) && j == len(b)
		}
		if !eq(a[i], b[j]) {
			return false
		}
		i, j = i+1, j+1
	}
}

func unresolvedEqual(a, b explainfiles.Unresolved) bool {
	return a.Kind == b.Kind && a.From == b.From && a.Specifier == b.Specifier && slices.Equal(a.Candidates, b.Candidates)
}

// candidateBlock is one resolution's candidates, in order, as selectProgram
// reads them: two blocks with equal probes are replaced or kept alike.
type candidateBlock struct {
	key    candidateKeyText
	probes string
}

func candidateBlocks(candidates []resolutionCandidate) []candidateBlock {
	var blocks []candidateBlock
	var probes strings.Builder
	for i, c := range candidates {
		fmt.Fprintf(&probes, "%d %s\x00%s\x00%s\x00%t %t\n", c.kind, c.path, c.resolved, c.skipped, c.file, c.metadata)
		if i+1 == len(candidates) || candidates[i+1].block != c.block || candidates[i+1].from != c.from {
			blocks = append(blocks, candidateBlock{key: candidateKeyText{c.from, c.specifier, c.block}, probes: probes.String()})
			probes.Reset()
		}
	}
	return blocks
}

// checkSelections: a selection that reaches an importer must already reach the
// target, and must leave each new resolution as the compiler gave it, as it
// left a resolution with the same probes from another importer. A resolution
// into a gained source has none; nothing can replace it, since the proof found
// no override and no declared output among its probes.
func checkSelections(facts *importFacts, ids map[string]int32, key string, added []explainfiles.Edge,
	fresh []candidateBlock, witnesses map[string][]candidateKeyText, delta *sourceDelta, mates map[string][]string) *decline {
	for _, s := range facts.Selections {
		if s.Listing != key {
			continue
		}
		selected := map[candidateKeyText]bool{}
		for _, k := range s.Selected {
			selected[candidateKeyText{facts.Files[k.From], k.Specifier, k.Block}] = true
		}
		for _, e := range added {
			if from, known := ids[e.From]; known && !delta.isGained(e.From) && has(s.Unreached, from) {
				continue
			} else if !known && !delta.isGained(e.From) {
				return declined(declineNoListing, "%s is not in the record's facts", e.From)
			}
			if to, known := ids[e.To]; known && !delta.isGained(e.To) && has(s.Unreached, to) {
				return declined(declineSelectionReach, "a selection of listing %s reaches %s but not %s", key, e.From, e.To)
			}
			if delta.isGained(e.To) {
				continue
			}
			for _, b := range fresh {
				if b.key.from != e.From || b.key.specifier != e.Specifier {
					continue
				}
				if !slices.ContainsFunc(witnesses[b.probes], func(w candidateKeyText) bool { return !selected[w] }) {
					return declined(declineSelectionWitness, "no other importer's resolution with the same probes stands for %s's %q", e.From, e.Specifier)
				}
			}
		}
	}
	return nil
}

// has says whether a sorted list holds v.
func has[T cmp.Ordered](sorted []T, v T) bool {
	_, found := slices.BinarySearch(sorted, v)
	return found
}

// checkWalks: a walk that follows the importer's edges must already have
// resolved a starting edge into the target through the rule index, so the
// edge, added or dropped, resolves the same way: to the walking rule itself,
// whose sources include the target, or to a dep the rule takes unconditionally.
// A gained source starts every walk its mates start, and resolves as they do.
func checkWalks(facts *importFacts, ids map[string]int32, key string, e explainfiles.Edge, mates map[string][]string) *decline {
	mateIDs := func(f string) ([]int32, bool) {
		var out []int32
		for _, m := range mates[f] {
			id, known := ids[m]
			if !known {
				return nil, false
			}
			out = append(out, id)
		}
		return out, len(out) > 0
	}
	from, fromKnown := ids[e.From]
	fromMates, fromGained := []int32(nil), false
	if !fromKnown {
		if fromMates, fromGained = mateIDs(e.From); !fromGained {
			return declined(declineNoListing, "%s is not in the record's facts", e.From)
		}
	}
	to, toKnown := ids[e.To]
	var toMates []int32
	if !toKnown {
		toMates, _ = mateIDs(e.To)
	}
	for _, r := range facts.Resolves {
		for _, w := range r.Walks {
			if w.Listing != key {
				continue
			}
			var store, starts bool
			if fromKnown {
				store, starts = has(w.ReachedStore, from), has(w.Initial, from) || has(w.Reached, from)
			} else {
				store = slices.ContainsFunc(fromMates, func(id int32) bool { return has(w.ReachedStore, id) })
				starts = slices.ContainsFunc(fromMates, func(id int32) bool { return has(w.Initial, id) })
			}
			switch {
			case store:
				return declined(declineWalkStore, "a walk over listing %s reaches %s through an npm store", key, e.From)
			case !starts:
				continue
			case w.Config:
				return declined(declineWalkConfig, "a vitest config's walk over listing %s reaches %s", key, e.From)
			}
			o, targetStarts, why := walkOutcome(w, key, e.To, to, toKnown, toMates)
			if why != nil {
				return why
			}
			switch {
			case o.Conflict:
				return declined(declineWalkConflict, "a walk over listing %s resolved edges into %s two ways", key, e.To)
			case !o.Owned:
				return declined(declineWalkNotOwned, "a walk over listing %s resolved %s other than by its owner", key, e.To)
			case o.Dep == "" && !o.Source, (o.Dep == "" || o.Source) && !targetStarts:
				return declined(declineWalkOrder, "a walk over listing %s would reach %s, which it does not start from", key, e.To)
			case o.Dep != "" && !has(r.Unconditional, o.Dep):
				return declined(declineWalkConditional, "a walk over listing %s adds %s to the conditional dep %s", key, e.To, o.Dep)
			}
		}
	}
	return nil
}

// walkOutcome is what w resolved for starting edges into the target, or, for a
// gained target, into its mates, which must agree; and whether w starts from it.
func walkOutcome(w walkFacts, key, target string, to int32, toKnown bool, toMates []int32) (edgeOutcome, bool, *decline) {
	find := func(id int32) (edgeOutcome, bool) {
		i, found := slices.BinarySearchFunc(w.Outcomes, id, func(o edgeOutcome, id int32) int { return cmp.Compare(o.To, id) })
		if !found {
			return edgeOutcome{}, false
		}
		return w.Outcomes[i], true
	}
	if toKnown {
		o, found := find(to)
		if !found {
			return o, false, declined(declineWalkWitness, "a walk over listing %s resolved no starting edge into %s", key, target)
		}
		return o, has(w.Initial, to), nil
	}
	var witness *edgeOutcome
	starts := false
	for _, id := range toMates {
		starts = starts || has(w.Initial, id)
		o, found := find(id)
		if !found {
			continue
		}
		o.To = 0
		if witness != nil && *witness != o {
			return o, false, declined(declineWalkConflict, "a walk over listing %s resolved %s's mates two ways", key, target)
		}
		witness = &o
	}
	if witness == nil {
		return edgeOutcome{}, false, declined(declineWalkWitness, "a walk over listing %s resolved no edge into a mate of %s", key, target)
	}
	return *witness, starts, nil
}

// ---- standing for the run -------------------------------------------------

// tryFastPath replays rec for a tree it no longer matches when decideFastPath
// proves the full pass would write and log what rec holds, after writing the
// BUILD edits a gained or lost source makes; it returns only when the full
// pass has to run.
func tryFastPath(fs *flag.FlagSet, c *config.Config, file string, rec *runRecord, dirs []string, stamps *newest) {
	if fastPathMode == "off" {
		return
	}
	decision, why := decideFastPath(rec, c.RepoRoot, c.ValidBuildFileNames, dirs, stamps, flagValue(fs, "exclude"))
	if mode := flagValue(fs, "mode"); why == nil && len(decision.edits) > 0 && mode != "fix" {
		why = declined(declineMode, "-mode=%s; only fix writes the BUILD edit", mode)
	}
	if why != nil {
		if listingVerbose {
			log.Printf("typescript: fast path: declined (%s): %s", why.code, why.detail)
		}
		return
	}
	decision.buildNames, decision.skipDir = c.ValidBuildFileNames, filepath.Dir(file)
	if fastPathMode == "verify" {
		os.Exit(verifyFastPath(fs, file, rec, decision))
	}
	if err := decision.writeEdits(); err != nil {
		log.Printf("typescript: fast path: %v; running the full pass", err)
		return
	}
	decision.carryForward(file, rec, true)
	if len(decision.edits) > 0 {
		traceRun("fast path edit")
	} else {
		traceRun("fast path")
	}
	os.Stdout.Write(rec.stdout)
	os.Stderr.Write(rec.stderr)
	if listingVerbose {
		log.Printf("typescript: fast path: replayed; %d listing(s) patched, %d BUILD file(s) written, and every import the edit adds or drops is one the rules already resolve",
			len(decision.patches), len(decision.edits))
	}
	os.Exit(rec.exit)
}

// writeEdits writes every BUILD edit, or none: a failed write puts back the ones before it.
func (d *fastPathDecision) writeEdits() error {
	for i, e := range d.edits {
		if err := e.write(false); err != nil {
			return errors.Join(err, d.restoreEdits(i))
		}
	}
	return nil
}

// restoreEdits writes back the original of the first n edits.
func (d *fastPathDecision) restoreEdits(n int) error {
	var errs []error
	for _, e := range d.edits[:n] {
		errs = append(errs, e.write(true))
	}
	return errors.Join(errs...)
}

// verifyFastPath writes the decision's BUILD edits, runs the full pass the fast
// path stands for and fails unless it changes no BUILD file and exits and logs
// as the record does; a failure puts the BUILD files back.
func verifyFastPath(fs *flag.FlagSet, file string, rec *runRecord, decision *fastPathDecision) int {
	if err := decision.writeEdits(); err != nil {
		log.Printf("typescript: fast path: %v", err)
		return 1
	}
	args := slices.Clone(os.Args[1:])
	at := len(args) - fs.NArg()
	if at > 0 && args[at-1] == "--" {
		at--
	}
	args = slices.Insert(args, at, "-mode=diff", "-ts_fastpath=off", "-ts_run_record=off", "-ts_resolve_record=off")
	code, stdout, stderr := runSelf(args)
	fail := func(why string) int {
		os.Remove(file)
		if err := decision.restoreEdits(len(decision.edits)); err != nil {
			log.Printf("typescript: fast path: %v", err)
		}
		log.Printf("typescript: fast path: the full pass %s; the record is removed and the BUILD files are as they were", why)
		return 1
	}
	switch {
	case code == 1 && len(stdout) > 0:
		return fail("writes BUILD files the fast path does not (diff above)")
	case code != rec.exit:
		return fail(fmt.Sprintf("exits %d where the record exits %d", code, rec.exit))
	case !slices.Equal(logLines(stderr), logLines(rec.stderr)):
		return fail(fmt.Sprintf("logs otherwise than the record:\n--- record\n%s--- full pass\n%s", rec.stderr, stderr))
	}
	decision.carryForward(file, rec, false)
	log.Print("typescript: fast path: verified; the full pass writes and logs what the fast path does")
	return rec.exit
}

// carryForward rewrites rec for the tree as it is now, as the full pass would
// have recorded it: the patched listings' reads join the record's, and patching's
// reads join the extension's. Every line read by content is read again; a moved
// one, or any change inside the racy window, keeps the old record. The patched
// listings are written with the record or not at all, so the next run finds
// both as one run left them; a verified run's full pass has written its own.
func (d *fastPathDecision) carryForward(file string, rec *runRecord, writeListings bool) {
	reads := map[string]string{}
	for i, read := range rec.reads {
		reads[read] = d.lines.reads[i]
	}
	recheck := map[string]bool{}
	for _, f := range d.changed {
		recheck[f] = true
	}
	for _, patch := range d.patches {
		for i, read := range patch.entry.Reads {
			if was, seen := reads[read]; !seen || was != patch.entry.ReadLines[i] {
				recheck[read] = true
			}
			reads[read] = patch.entry.ReadLines[i]
		}
	}
	files := map[string]string{}
	for i, f := range rec.files {
		files[f] = d.lines.files[i]
	}
	for f, line := range d.files {
		if _, seen := files[f]; !seen {
			files[f] = line
			recheck[f] = true
		}
	}
	for f := range recheck {
		now, stamp := fileLine(f)
		d.stamps.see(stamp)
		if line, read := reads[f]; (read && incremental.EntryLine(f) != line) || (files[f] != "" && files[f] != now) {
			if listingVerbose {
				log.Printf("typescript: fast path: record not carried forward; %s moved during the run", f)
			}
			return
		}
	}
	if !d.stamps.at.Before(processStart.Add(-racyWindow)) {
		if listingVerbose {
			log.Printf("typescript: fast path: record not carried forward; an input changed within %s of the run's start", racyWindow)
		}
		return
	}
	next := &runRecord{key: rec.key, dirs: rec.dirs, roots: rec.roots, exit: rec.exit, stdout: rec.stdout, stderr: rec.stderr, facts: rec.facts}
	next.files = slices.Sorted(maps.Keys(files))
	next.reads = slices.Sorted(maps.Keys(reads))
	lines := inputLines{dirs: slices.Clone(d.lines.dirs), roots: d.lines.roots}
	if len(d.edits) > 0 {
		for _, e := range d.edits {
			line, ok := d.editedDirLine(rec.dirs[e.dirIndex], d.lines.dirs[e.dirIndex], e)
			if !ok {
				if listingVerbose {
					log.Printf("typescript: fast path: record not carried forward; %s moved during the run", rec.dirs[e.dirIndex])
				}
				return
			}
			lines.dirs[e.dirIndex] = line
			// The walked directory's line holds the BUILD file's content, which stands for its stat.
			if _, read := files[e.file]; read {
				files[e.file], _ = fileLine(e.file)
			}
		}
		// The facts name the tree before the edit; the next change runs in full and records anew.
		facts, err := (&importFacts{Version: importFactsVersion, Incomplete: "the record was carried past a BUILD edit the fast path wrote"}).encode()
		if err != nil {
			log.Printf("typescript: fast path: %v", err)
			return
		}
		next.facts = facts
	}
	for _, f := range next.files {
		lines.files = append(lines.files, files[f])
	}
	for _, r := range next.reads {
		lines.reads = append(lines.reads, reads[r])
	}
	next.digest, next.walkDigest = next.digests(lines)
	next.sums = inputSums(lines)
	data, err := next.encode()
	if err != nil {
		log.Printf("typescript: fast path: %v", err)
		return
	}
	// The process exits next, so the entries are written without being kept for the run.
	if writeListings {
		parallel(len(d.patches), func(i int) {
			d.patches[i].entry.take(d.patches[i].p)
			writeListing(d.patches[i].newKey, d.patches[i].entry)
		})
	}
	if err := writeAtomically(file, data); err != nil {
		log.Printf("typescript: fast path: %v", err)
	}
}
