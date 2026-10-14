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
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/bazelbuild/bazel-gazelle/config"

	"github.com/mikn/rules_typescript/gazelle/incremental"
	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// fastPathMode is -ts_fastpath: on, verify or off.
var fastPathMode = "on"

// importFactsVersion names the shape of importFacts; a record of another shape is never used.
const importFactsVersion = 1

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
}

// listingFacts is one listing the run used and the TypeScript sources it read.
type listingFacts struct {
	Key     string
	Sources []int32
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
}

func newImportFactsRecorder() *importFactsRecorder {
	r := &importFactsRecorder{ids: map[string]int32{}, facts: importFacts{Version: importFactsVersion}}
	r.sealed = sync.OnceValue(r.encode)
	return r
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
		var sources []string
		for _, read := range entry.Reads {
			if rel, ok := repoRelative(entry.Root, read); ok && patchableSource(rel) {
				sources = append(sources, rel)
			}
		}
		r.facts.Listings = append(r.facts.Listings, listingFacts{Key: key, Sources: r.ids32(sources)})
	}
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(&r.facts); err != nil {
		if listingVerbose {
			log.Printf("typescript: fast path: no import facts recorded: %v", err)
		}
		return nil
	}
	return b.Bytes()
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
		if !ok(l.Sources) {
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
)

// decline is why the fast path does not stand for this run.
type decline struct{ code, detail string }

func declined(code, format string, a ...any) *decline {
	return &decline{code: code, detail: fmt.Sprintf(format, a...)}
}

// fastPatch is one listing the edit stales, patched as a full run patches it,
// beside the listing and candidates its entry held before.
type fastPatch struct {
	key        string
	entry      *cachedListing
	p          *program
	listing    explainfiles.Listing
	candidates []cachedCandidate
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
}

// decideFastPath says whether rec still stands for the tree although its inputs
// moved: every change is an edit to a TypeScript source whose listings patch,
// and every import the edit adds lies between files that every rule reading the
// importer already resolves the same way.
func decideFastPath(rec *runRecord, repoRoot string, buildNames []string, dirs []string, stamps *newest) (*fastPathDecision, *decline) {
	if len(rec.facts) == 0 || len(rec.sums) != len(rec.dirs)+len(rec.files)+len(rec.reads)+len(rec.roots) {
		return nil, declined(declineNoFacts, "the record keeps no import facts")
	}
	for i, dir := range rec.dirs {
		if lineSum(dirShape(dirs[i])) != rec.sums[i] {
			return nil, declined(declineInputs, "%s gained, lost or changed an entry other than a source's stat", dir)
		}
	}
	facts, err := decodeImportFacts(rec.facts)
	if err != nil {
		return nil, declined(declineNoFacts, "%v", err)
	}
	if facts.Incomplete != "" {
		return nil, declined(declineFactsIncomplete, "%s", facts.Incomplete)
	}
	lines := inputLines{dirs: dirs, files: fileLines(rec.files, stamps), reads: listingReadLines(rec.reads, stamps), roots: rootLines(rec.roots)}
	sums := inputSums(lines)
	// A walked directory's shape holds its BUILD file's content, so a BUILD file read by stat there may move.
	walked := setOf(rec.dirs)
	changed := map[string]bool{}
	at := len(rec.dirs)
	for _, list := range [][]string{rec.files, rec.reads} {
		for i, file := range list {
			if sums[at+i] != rec.sums[at+i] && !(slices.Contains(buildNames, filepath.Base(file)) && walked[filepath.Dir(file)]) {
				changed[file] = true
			}
		}
		at += len(list)
	}
	for i, root := range rec.roots {
		if sums[at+i] != rec.sums[at+i] {
			return nil, declined(declineInputs, "the names under %s changed", root)
		}
	}
	decision := &fastPathDecision{lines: lines, files: map[string]string{}, changed: slices.Sorted(maps.Keys(changed)), stamps: stamps}
	if len(decision.changed) == 0 {
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
	keys := make([]string, 0, len(affected))
	for _, i := range slices.Sorted(maps.Keys(affected)) {
		keys = append(keys, facts.Listings[i].Key)
	}
	patches := make([]fastPatch, len(keys))
	whys := make([]*decline, len(keys))
	parallel(len(keys), func(i int) {
		patches[i], whys[i] = patchAffected(keys[i], repoRoot, readSums, changedRel)
		if whys[i] == nil {
			whys[i] = checkPatch(facts, ids, keys[i], patches[i], changedRel)
		}
	})
	for i := range keys {
		if whys[i] != nil {
			return nil, whys[i]
		}
	}
	decision.patches = patches
	capture.mu.Lock()
	maps.Copy(decision.files, capture.files)
	capture.mu.Unlock()
	return decision, nil
}

// patchAffected patches one listing the edit stales from its cache entry, which
// must be the one the record saw: every read the edit did not touch reads the
// same, and every touched source read what the record says it did.
func patchAffected(key, repoRoot string, readSums map[string]uint64, changed map[string]bool) (fastPatch, *decline) {
	entry, err := readListing(filepath.Join(listingCacheDir, key))
	if err != nil {
		return fastPatch{}, declined(declineEntry, "listing %s: %v", key, err)
	}
	if entry.Root != repoRoot || len(entry.ReadLines) != len(entry.Reads) || len(entry.DirDigests) != len(entry.Dirs) {
		return fastPatch{}, declined(declineEntry, "listing %s keeps no patch state for this root", key)
	}
	for i, read := range entry.Reads {
		rel, _ := repoRelative(repoRoot, read)
		if changed[rel] {
			sum, recorded := readSums[read]
			if !recorded || sum != lineSum(entry.ReadLines[i]) || existence(entry.ReadLines[i]) != "file" {
				return fastPatch{}, declined(declineEntry, "listing %s read %s otherwise than the record", key, rel)
			}
			continue
		}
		if listingStats.Line(read) != entry.ReadLines[i] {
			return fastPatch{}, declined(declineOtherRead, "listing %s: %s changed and the record does not say so", key, read)
		}
	}
	for i, dir := range entry.Dirs {
		if listingNames.Digest(dir) != entry.DirDigests[i] {
			return fastPatch{}, declined(declineOtherRead, "listing %s: the names under %s changed", key, dir)
		}
	}
	// The patch leaves the entry's listing and candidates as they were until remember.
	old := fastPatch{key: key, entry: entry, listing: entry.Listing, candidates: entry.Candidates}
	p, why := tryPatchListing(entry, "", entry.FilesList)
	if p == nil {
		return fastPatch{}, declined(declinePatch, "listing %s: %s", key, why)
	}
	old.p = p
	return old, nil
}

// checkPatch proves that what the patch changed in one listing changes no
// output: the listing keeps its files, roots and types, every other importer's
// edges and resolutions as they were, loses nothing, and every edge it gains is
// a relative import between listed sources that each recorded selection and
// walk over this listing already treats as it treats the target.
func checkPatch(facts *importFacts, ids map[string]int32, key string, patch fastPatch, changed map[string]bool) *decline {
	old, p := patch.listing, patch.p
	for _, part := range []struct {
		name  string
		equal bool
	}{
		{"files", slices.Equal(old.Files, p.Files)},
		{"roots", slices.Equal(old.Roots, p.Roots)},
		{"type entries", slices.Equal(old.Types, p.Types) && slices.Equal(old.Implicit, p.Implicit)},
		{"diagnostics", slices.Equal(old.Diagnostics, p.Diagnostics)},
		{"unresolved imports", slices.EqualFunc(old.Unresolved, p.Unresolved, unresolvedEqual)},
	} {
		if !part.equal {
			return declined(declineListingShape, "listing %s changes its %s", key, part.name)
		}
	}
	// A patch keeps every other importer's edges and resolution blocks in order.
	if !equalExcept(old.Edges, p.Edges, func(e explainfiles.Edge) bool { return changed[e.From] }) {
		return declined(declineListingShape, "listing %s changes an edge of a file the edit did not touch", key)
	}
	if !equalExceptFunc(patch.candidates, p.candidates, func(c cachedCandidate) bool { return changed[c.From] },
		func(c resolutionCandidate) bool { return changed[c.from] }, func(a cachedCandidate, b resolutionCandidate) bool { return a == b.cached() }) {
		return declined(declineCandidate, "listing %s changes a resolution of a file the edit did not touch", key)
	}
	have := map[explainfiles.Edge]int{}
	oldSpecifiers := map[importKey]bool{}
	for _, e := range old.Edges {
		if changed[e.From] {
			have[e]++
			oldSpecifiers[importKey{e.From, e.Specifier}] = true
		}
	}
	var added []explainfiles.Edge
	for _, e := range p.Edges {
		if !changed[e.From] {
			continue
		}
		if have[e] > 0 {
			have[e]--
			continue
		}
		added = append(added, e)
	}
	for _, e := range old.Edges {
		if have[e] > 0 {
			return declined(declineEdgeRemoved, "listing %s loses %s -> %s", key, e.From, e.To)
		}
	}
	held := map[cachedCandidate]int{}
	for _, c := range patch.candidates {
		if changed[c.From] {
			held[c]++
			oldSpecifiers[importKey{c.From, c.Specifier}] = true
		}
	}
	var fresh []resolutionCandidate
	for _, c := range p.candidates {
		if !changed[c.from] {
			continue
		}
		if held[c.cached()] > 0 {
			held[c.cached()]--
			continue
		}
		fresh = append(fresh, c)
	}
	for _, c := range patch.candidates {
		if held[c] > 0 {
			return declined(declineCandidate, "listing %s drops a resolution of %q from %s", key, c.Specifier, c.From)
		}
	}
	if len(added) == 0 {
		if len(fresh) > 0 {
			return declined(declineCandidate, "listing %s resolves otherwise with no edge added", key)
		}
		return nil
	}
	listed := setOf(p.Files)
	addedSpecifiers := map[importKey]bool{}
	for _, e := range added {
		switch {
		case e.Kind != explainfiles.Import || !strings.HasPrefix(e.Specifier, "./") && !strings.HasPrefix(e.Specifier, "../"):
			return declined(declineEdgeKind, "%s gains %q, no relative import", e.From, e.Specifier)
		case !firstParty(e.To) || isDeclarationFile(e.To) || !listed[e.To] || !patchableSource(e.To):
			return declined(declineEdgeKind, "%s gains %q into %s, no listed source", e.From, e.Specifier, e.To)
		case oldSpecifiers[importKey{e.From, e.Specifier}] || addedSpecifiers[importKey{e.From, e.Specifier}]:
			return declined(declineSpecifierReused, "%s imports %q more than once", e.From, e.Specifier)
		}
		addedSpecifiers[importKey{e.From, e.Specifier}] = true
	}
	// Gazelle reads the BUILD facts of every directory a listing probes; a new probe must add none.
	probed := map[string]bool{}
	for _, f := range p.Files {
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
		if !changed[c.From] && freshPaths[c.Path] {
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
	if why := checkSelections(facts, ids, key, added, candidateBlocks(fresh), witnesses); why != nil {
		return why
	}
	for _, e := range added {
		if why := checkWalks(facts, ids, key, e); why != nil {
			return why
		}
	}
	if listingVerbose {
		return declined(declineVerboseLog, "-ts_verbose logs each listing's edge count, which listing %s's new imports change", key)
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
// left a resolution with the same probes from another importer.
func checkSelections(facts *importFacts, ids map[string]int32, key string, added []explainfiles.Edge,
	fresh []candidateBlock, witnesses map[string][]candidateKeyText) *decline {
	for _, s := range facts.Selections {
		if s.Listing != key {
			continue
		}
		selected := map[candidateKeyText]bool{}
		for _, k := range s.Selected {
			selected[candidateKeyText{facts.Files[k.From], k.Specifier, k.Block}] = true
		}
		for _, e := range added {
			from, known := ids[e.From]
			if !known {
				return declined(declineNoListing, "%s is not in the record's facts", e.From)
			}
			if has(s.Unreached, from) {
				continue
			}
			if to, known := ids[e.To]; known && has(s.Unreached, to) {
				return declined(declineSelectionReach, "a selection of listing %s reaches %s but not %s", key, e.From, e.To)
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
// resolved a starting edge into the target through the rule index, so the new
// edge resolves the same way: to the walking rule itself, whose sources include
// the target, or to a dep the rule takes unconditionally.
func checkWalks(facts *importFacts, ids map[string]int32, key string, e explainfiles.Edge) *decline {
	from, known := ids[e.From]
	if !known {
		return declined(declineNoListing, "%s is not in the record's facts", e.From)
	}
	to, toKnown := ids[e.To]
	for _, r := range facts.Resolves {
		for _, w := range r.Walks {
			if w.Listing != key {
				continue
			}
			switch {
			case has(w.ReachedStore, from):
				return declined(declineWalkStore, "a walk over listing %s reaches %s through an npm store", key, e.From)
			case !has(w.Initial, from) && !has(w.Reached, from):
				continue
			case w.Config:
				return declined(declineWalkConfig, "a vitest config's walk over listing %s reaches %s", key, e.From)
			}
			i, found := slices.BinarySearchFunc(w.Outcomes, to, func(o edgeOutcome, id int32) int { return cmp.Compare(o.To, id) })
			if !toKnown || !found {
				return declined(declineWalkWitness, "a walk over listing %s reaching %s resolved no starting edge into %s", key, e.From, e.To)
			}
			o, starts := w.Outcomes[i], has(w.Initial, to)
			switch {
			case o.Conflict:
				return declined(declineWalkConflict, "a walk over listing %s resolved edges into %s two ways", key, e.To)
			case !o.Owned:
				return declined(declineWalkNotOwned, "a walk over listing %s resolved %s other than by its owner", key, e.To)
			case o.Dep == "" && !o.Source, (o.Dep == "" || o.Source) && !starts:
				return declined(declineWalkOrder, "a walk over listing %s would reach %s, which it does not start from", key, e.To)
			case o.Dep != "" && !has(r.Unconditional, o.Dep):
				return declined(declineWalkConditional, "a walk over listing %s adds %s to the conditional dep %s", key, e.To, o.Dep)
			}
		}
	}
	return nil
}

// ---- standing for the run -------------------------------------------------

// tryFastPath replays rec for a tree it no longer matches when decideFastPath
// proves the full pass would write and log what rec holds; it returns only when
// the full pass has to run.
func tryFastPath(fs *flag.FlagSet, c *config.Config, file string, rec *runRecord, dirs []string, stamps *newest) {
	if fastPathMode == "off" {
		return
	}
	decision, why := decideFastPath(rec, c.RepoRoot, c.ValidBuildFileNames, dirs, stamps)
	if why != nil {
		if listingVerbose {
			log.Printf("typescript: fast path: declined (%s): %s", why.code, why.detail)
		}
		return
	}
	if fastPathMode == "verify" {
		os.Exit(verifyFastPath(fs, file, rec, decision))
	}
	decision.carryForward(file, rec, true)
	os.Stdout.Write(rec.stdout)
	os.Stderr.Write(rec.stderr)
	if listingVerbose {
		log.Printf("typescript: fast path: replayed; %d listing(s) patched, and every import the edit adds is one the rules already resolve", len(decision.patches))
	}
	os.Exit(rec.exit)
}

// verifyFastPath runs the full pass the fast path stands for and fails unless
// it changes no BUILD file and exits and logs as the record does.
func verifyFastPath(fs *flag.FlagSet, file string, rec *runRecord, decision *fastPathDecision) int {
	args := slices.Clone(os.Args[1:])
	at := len(args) - fs.NArg()
	if at > 0 && args[at-1] == "--" {
		at--
	}
	args = slices.Insert(args, at, "-mode=diff", "-ts_fastpath=off", "-ts_run_record=off", "-ts_resolve_record=off")
	code, stdout, stderr := runSelf(args)
	fail := func(why string) int {
		os.Remove(file)
		log.Printf("typescript: fast path: the full pass %s; the record is removed", why)
		return 1
	}
	switch {
	case code == 1 && len(stdout) > 0:
		return fail("writes BUILD files the record does not hold (diff above)")
	case code != rec.exit:
		return fail(fmt.Sprintf("exits %d where the record exits %d", code, rec.exit))
	case !slices.Equal(logLines(stderr), logLines(rec.stderr)):
		return fail(fmt.Sprintf("logs otherwise than the record:\n--- record\n%s--- full pass\n%s", rec.stderr, stderr))
	}
	decision.carryForward(file, rec, false)
	log.Print("typescript: fast path: verified; the full pass writes and logs what the record does")
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
	lines := inputLines{dirs: d.lines.dirs, roots: d.lines.roots}
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
	if writeListings {
		for _, patch := range d.patches {
			remember(patch.key, patch.entry, patch.p)
			writeListing(patch.key, patch.entry)
		}
	}
	if err := writeAtomically(file, data); err != nil {
		log.Printf("typescript: fast path: %v", err)
	}
}
