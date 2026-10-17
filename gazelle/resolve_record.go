package typescript

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
)

// resolveRecordMode is -ts_resolve_record: on, verify or off.
var resolveRecordMode = "on"

const (
	resolveRecordName  = "resolve-record"
	resolveRecordMagic = "rules_typescript gazelle resolve record v4"
)

// resolveRecord is what one run's Resolve and AfterResolvingDeps left in the
// BUILD files and the log, for runs whose first Resolve sees the same state.
type resolveRecord struct {
	Magic, Key, State string
	// Reads are the extension's own, each as fileLine sees it; a BUILD file
	// read after the walk is by content.
	Reads []recordedRead
	// Dirs and Listings are those first read after the walk.
	Dirs     []recordedRead
	Listings []recordedListing
	// Outside are the directories State leaves out whose names the phase read,
	// and Trees the roots under which it read every such directory, each as outsideLine.
	Outside, Trees []recordedRead
	Files          []recordedFile
	Log            []string
	// Whole are the Reads the run read the content of by a name it was given.
	Whole []string
	// Facts are the phase's import facts, which a run that replays the phase hands its run record.
	Facts []byte
}

type recordedRead struct{ Path, Line string }

type recordedListing struct {
	Reads, ReadLines, Dirs, DirDigests []string
	// Scans are each read's scanSum, by Reads' order, as the listing's entry held them.
	Scans []uint64
}

func (l recordedListing) scanOf(i int) uint64 {
	if len(l.Scans) != len(l.Reads) {
		return 0
	}
	return l.Scans[i]
}

type recordedFile struct {
	Pkg     string
	Content []byte
}

// resolveRecorder carries one run's record from CheckFlags to the end of
// AfterResolvingDeps.
type resolveRecorder struct {
	file, key, cache string
	buildNames       []string
	generated        []generatedPackage
	indexed          []importAnswer
	started          bool
	// replay is the record a hit writes back.
	replay *resolveRecord
	state  string
	// Why this run writes no record, or "".
	why string
	// The directories and listings read before the first Resolve, which the state digest covers.
	walkDirs, walkListings map[string]bool
	log                    bytes.Buffer
	stderr                 io.Writer

	mu          sync.Mutex
	buildBodies map[string]string

	// outside is how much of each walked directory State leaves out; sorted is its keys in order.
	outside               map[string]dirScope
	sorted                []string
	outsideMu             sync.Mutex
	outsideRead, treeRead map[string]string
}

// noteOutside notes a read of dir's names unless State holds them.
func (rec *resolveRecorder) noteOutside(s *programStore, dir string) {
	if _, listed := s.files[dir]; listed && rec.outside[dir] == digested {
		return
	}
	rec.outsideMu.Lock()
	defer rec.outsideMu.Unlock()
	if _, seen := rec.outsideRead[dir]; !seen {
		rec.outsideRead[dir] = s.outsideLine(dir)
	}
}

// noteOutsideTree notes that the phase enumerated every walked directory under root.
func (rec *resolveRecorder) noteOutsideTree(s *programStore, root string) {
	rec.outsideMu.Lock()
	defer rec.outsideMu.Unlock()
	if _, seen := rec.treeRead[root]; !seen {
		rec.treeRead[root] = s.treeLine(root, rec.sorted)
	}
}

// dirNames is the walk's file names in dir, noted as a read during the Resolve phase.
func (s *programStore) dirNames(dir string) ([]string, bool) {
	if rec := resolvePhase.Load(); rec != nil {
		rec.noteOutside(s, dir)
	}
	names, ok := s.files[dir]
	return names, ok
}

// dirWalked is whether the walk generated dir, noted as a read during the Resolve phase.
func (s *programStore) dirWalked(dir string) (bool, bool) {
	if rec := resolvePhase.Load(); rec != nil {
		rec.noteOutside(s, dir)
	}
	walked, observed := s.walked[dir]
	return walked, observed
}

// walkedDirs is every walked directory at or under root, in order, noted as a
// read of them all during the Resolve phase.
func (s *programStore) walkedDirs(root string) []string {
	if rec := resolvePhase.Load(); rec != nil {
		rec.noteOutsideTree(s, root)
	}
	var dirs []string
	for dir := range s.files {
		if within(dir, root) {
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// outsideLine is what dirNames and dirWalked read of dir.
func (s *programStore) outsideLine(dir string) string {
	d := newHasher()
	names, listed := s.files[dir]
	walked, observed := s.walked[dir]
	d.bool(listed)
	d.strs(names)
	d.bool(observed)
	d.bool(walked)
	sum := d.sum()
	return hex.EncodeToString(sum[:8])
}

// treeLine is the outsideLine of every directory of outside at or under root.
func (s *programStore) treeLine(root string, outside []string) string {
	d := newHasher()
	for _, dir := range outside {
		if within(dir, root) {
			d.str(dir)
			d.str(s.outsideLine(dir))
		}
	}
	sum := d.sum()
	return hex.EncodeToString(sum[:8])
}

// outsideHolds says whether every directory State leaves out reads as the recorded phase read it.
func (r *resolveRecord) outsideHolds(s *programStore, outside []string) bool {
	for _, read := range r.Outside {
		if s.outsideLine(read.Path) != read.Line {
			return false
		}
	}
	for _, read := range r.Trees {
		if s.treeLine(read.Path, outside) != read.Line {
			return false
		}
	}
	return true
}

// resolvePhase is the recorder between the first Resolve and the end of
// AfterResolvingDeps, the span whose BUILD file reads count by content.
var resolvePhase atomic.Pointer[resolveRecorder]

func noteResolvePhaseRead(path string) {
	if rec := resolvePhase.Load(); rec != nil && isBuildFile(path, rec.buildNames) {
		rec.noteBuildRead(path)
	}
}

func isBuildFile(path string, buildNames []string) bool {
	return slices.Contains(buildNames, filepath.Base(path))
}

func buildFileLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "absent"
	}
	return fmt.Sprintf("b %x", sha256.Sum256(data))
}

func (rec *resolveRecorder) noteBuildRead(path string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if _, seen := rec.buildBodies[path]; !seen {
		rec.buildBodies[path] = buildFileLine(path)
	}
}

// resolveRecordFile is where this run's record lives, keyed by everything a
// run is asked to do except where its caches live and how it is checked or profiled.
func resolveRecordFile(fs *flag.FlagSet, c *config.Config) (file, key, cache string, err error) {
	if cache, err = filepath.Abs(listingCacheDir); err != nil {
		return "", "", "", fmt.Errorf("-ts_listing_cache: %w", err)
	}
	key = recordKey(resolveRecordMagic, fs, c, "ts_run_record", "ts_resolve_record", "ts_fastpath", "ts_listing_cache", "ts_listing_patch_verify", "ts_parallel", "cpuprofile", "memprofile", "trace")
	return filepath.Join(cache, resolveRecordName+"-"+key[:16]), key, cache, nil
}

// startResolveRecord readies a record for a run a run record could stand for,
// and makes the run note the extension's reads if nothing else does.
func startResolveRecord(fs *flag.FlagSet, c *config.Config, s *programStore) error {
	if resolveRecordMode == "off" || !recordableRun(fs, c, s.fullWalk) {
		return nil
	}
	file, key, cache, err := resolveRecordFile(fs, c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return fmt.Errorf("-ts_listing_cache: %w", err)
	}
	if runInputs == nil {
		runInputs = &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}, key: key, repoRoot: c.RepoRoot, cache: cache, buildNames: c.ValidBuildFileNames}
		if exe, err := os.Executable(); err == nil {
			noteRead(exe)
		}
	}
	for _, name := range []string{".bazelignore", "REPO.bazel"} {
		noteRead(filepath.Join(c.RepoRoot, name))
	}
	s.resolution = &resolveRecorder{file: file, key: key, cache: cache, buildNames: c.ValidBuildFileNames, buildBodies: map[string]string{}}
	return nil
}

// resolveVerifyEnv names the file a verify's replaying child marks when it replays.
const resolveVerifyEnv = "RULES_TYPESCRIPT_GAZELLE_RESOLVE_RECORD_VERIFY"

func noteVerifiedReplay() {
	if file := os.Getenv(resolveVerifyEnv); file != "" {
		if err := os.WriteFile(file, []byte("replayed"), 0o644); err != nil {
			fatalf("typescript: resolve record: %v", err)
		}
	}
}

// gateOnResolveVerify runs a replay and a full pass with the record off as
// children: only the replay's own output shows what it wrote to the tree.
func gateOnResolveVerify(fs *flag.FlagSet, c *config.Config, fullWalk bool) error {
	if resolveRecordMode != "verify" || !recordableRun(fs, c, fullWalk) {
		return nil
	}
	file, _, _, err := resolveRecordFile(fs, c)
	if err != nil {
		return err
	}
	os.Exit(verifyResolveRecord(fs, file))
	return nil
}

func verifyResolveRecord(fs *flag.FlagSet, file string) int {
	marker, err := os.CreateTemp("", "resolve-record-verify-")
	if err == nil {
		err = marker.Close()
	}
	if err != nil {
		log.Printf("typescript: resolve record: %v", err)
		return 1
	}
	defer os.Remove(marker.Name())
	off := []string{"-ts_resolve_record=off", "-ts_run_record=off"}
	noCandidate := runRecordCandidateEnv + "="
	code, stdout, stderr := runSelf(argsWith(fs, "-ts_resolve_record=on", "-ts_run_record=off"), resolveVerifyEnv+"="+marker.Name(), noCandidate)
	if mark, _ := os.ReadFile(marker.Name()); string(mark) != "replayed" {
		log.Print("typescript: resolve record: not verified; the record did not replay")
		return code
	}
	diffMode := flagValue(fs, "mode") == "diff"
	full := slices.Concat(off, []string{"-mode=diff"})
	var want []byte
	if diffMode {
		full, want = off, stdout
	}
	fullCode, fullStdout, fullStderr := runSelfTo(io.Discard, io.Discard, argsWith(fs, full...), noCandidate)
	why := ""
	switch {
	case !bytes.Equal(fullStdout, want) && diffMode:
		why = "prints another diff than the replay:\n" + string(fullStdout)
	case !bytes.Equal(fullStdout, want):
		files := diffedFiles(fullStdout)
		if len(files) == 0 {
			files = []string{"BUILD files"}
		}
		why = "would write " + strings.Join(files, ", ") + " otherwise than the replay:\n" + string(fullStdout)
	case fullCode != code:
		why = fmt.Sprintf("exits %d where the replay exits %d", fullCode, code)
	case !slices.Equal(extensionLines(fullStderr), extensionLines(stderr)):
		why = fmt.Sprintf("logs otherwise than the replay:\n--- replay\n%s--- full pass\n%s", stderr, fullStderr)
	}
	if why == "" {
		log.Print("typescript: resolve record: verified; a full pass with the record off writes and logs what the replay did")
		return code
	}
	os.Remove(file)
	log.Printf("typescript: resolve record: a full pass with the record off %s; the record is removed", why)
	if !diffMode {
		if repaired, _, _ := runSelf(argsWith(fs, off...), noCandidate); repaired != 0 {
			log.Printf("typescript: resolve record: the full pass that rewrites the replay's BUILD files exits %d", repaired)
		}
	}
	return 1
}

func diffedFiles(diff []byte) []string {
	var files []string
	for line := range strings.SplitSeq(string(diff), "\n") {
		if name, ok := strings.CutPrefix(line, "+++ "); ok && strings.TrimSpace(name) != "" {
			files = append(files, strings.Fields(name)[0])
		}
	}
	return files
}

// extensionLines is this extension's lines of a log, which a replay writes
// back; Gazelle's own lines from inside Resolve it does not.
func extensionLines(logged []byte) []string {
	return slices.DeleteFunc(logLines(logged), func(line string) bool { return !strings.Contains(line, "typescript: ") })
}

func (rec *resolveRecorder) noteGenerated(args language.GenerateArgs, res language.GenerateResult) {
	if rec != nil && !rec.started {
		rec.generated = append(rec.generated, generatedPackage{args: args, res: res})
	}
}

func (rec *resolveRecorder) noteImports(c *config.Config, r *rule.Rule, f *rule.File, specs []resolve.ImportSpec) {
	if rec != nil && !rec.started {
		rec.indexed = append(rec.indexed, importAnswer{label: label.New(c.RepoName, f.Pkg, r.Name()), kind: r.Kind(), specs: slices.Clone(specs)})
	}
}

// begin runs at the first Resolve, when every BUILD file is merged and the
// index is complete, and says whether this run replays.
func (rec *resolveRecorder) begin(s *programStore) bool {
	if rec == nil {
		return false
	}
	if rec.started {
		return rec.replay != nil
	}
	rec.started = true
	rec.outside = s.outsideDirs(rec.generated)
	rec.sorted = slices.Sorted(maps.Keys(rec.outside))
	rec.state, rec.why = s.stateDigest(rec.generated, rec.indexed, rec.outside)
	if rec.why == "" {
		rec.why = s.untrackedRules(rec.generated)
	}
	rec.generated, rec.indexed = nil, nil
	if rec.why == "" {
		if old := loadResolveRecord(rec.file); old != nil && old.Key == rec.key && old.State == rec.state && old.outsideHolds(s, rec.sorted) && old.readsHold(rec.buildNames, rec.cache) {
			rec.replay = old
			s.importFacts.replayed(old.Facts)
			old.noteReplayedReads()
			noteVerifiedReplay()
			return true
		}
	}
	rec.walkListings = map[string]bool{}
	loadedListings.Range(func(k, _ any) bool {
		rec.walkListings[k.(string)] = true
		return true
	})
	runInputs.mu.Lock()
	rec.walkDirs = make(map[string]bool, len(runInputs.dirs))
	for rel := range runInputs.dirs {
		rec.walkDirs[rel] = true
	}
	runInputs.mu.Unlock()
	rec.outsideRead, rec.treeRead = map[string]string{}, map[string]string{}
	rec.stderr = log.Writer()
	log.SetOutput(io.MultiWriter(rec.stderr, &rec.log))
	resolvePhase.Store(rec)
	return false
}

// untrackedRules is why a replay could not stand for this run: Gazelle puts
// a generated rule in a BUILD file the emission graph does not hold.
func (s *programStore) untrackedRules(generated []generatedPackage) string {
	for _, g := range generated {
		if f := s.emission.files[g.args.Rel]; len(g.res.Gen) > 0 && (f == nil || f.File == nil) {
			return "rules generated for " + orRepoRoot(g.args.Rel) + ", whose BUILD file is not tracked"
		}
	}
	return ""
}

// trackedFiles is every BUILD file the emission graph holds, which Gazelle's
// post-resolve merge and AfterResolvingDeps are the last to edit.
func (s *programStore) trackedFiles() []string {
	var pkgs []string
	for pkg, f := range s.emission.files {
		if f.File != nil {
			pkgs = append(pkgs, pkg)
		}
	}
	slices.Sort(pkgs)
	return pkgs
}

func loadResolveRecord(file string) *resolveRecord {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var rec resolveRecord
	if gob.NewDecoder(bytes.NewReader(data)).Decode(&rec) != nil || rec.Magic != resolveRecordMagic || !slices.IsSorted(rec.Whole) {
		return nil
	}
	return &rec
}

// readsHold says whether every read the record names would read the same now.
func (r *resolveRecord) readsHold(buildNames []string, cache string) bool {
	if r.changed(buildNames, cache, nil) != "" {
		return false
	}
	for _, l := range r.Listings {
		if !slices.Equal(listingReadLines(l.Reads, nil), l.ReadLines) || !slices.Equal(rootLines(l.Dirs), l.DirDigests) {
			return false
		}
	}
	return true
}

// changed names the first file or directory the record reads otherwise now, or is "".
func (r *resolveRecord) changed(buildNames []string, cache string, stamps *newest) string {
	lines := make([]string, len(r.Reads))
	parallel(len(r.Reads), func(i int) {
		if path := r.Reads[i].Path; isBuildFile(path, buildNames) {
			lines[i] = buildFileLine(path)
		} else {
			var t time.Time
			lines[i], t = fileLine(path)
			stamps.see(t)
		}
	})
	for i, read := range r.Reads {
		if lines[i] != read.Line {
			return read.Path + " changed"
		}
	}
	dirs := make([]string, len(r.Dirs))
	for i, dir := range r.Dirs {
		dirs[i] = dir.Path
	}
	lines, err := dirLines(dirs, buildNames, cache, stamps)
	if err != nil {
		return err.Error()
	}
	for i, dir := range r.Dirs {
		if lines[i] != dir.Line {
			return dir.Path + " changed"
		}
	}
	return ""
}

// applyReplay writes the recorded BUILD files back over the ones this run
// merged without resolving, and the recorded log.
func (rec *resolveRecorder) applyReplay(s *programStore) {
	files := map[string][]byte{}
	for _, f := range rec.replay.Files {
		files[f.Pkg] = f.Content
	}
	pkgs := s.trackedFiles()
	if !slices.Equal(pkgs, slices.Sorted(maps.Keys(files))) {
		fatalf("typescript: resolve record: the record holds other BUILD files than this run; rerun with -ts_resolve_record=off")
	}
	recorded := make([]*rule.File, len(pkgs))
	errs := make([]error, len(pkgs))
	parallel(len(pkgs), func(i int) {
		f := s.emission.files[pkgs[i]]
		recorded[i], errs[i] = rule.LoadData(f.Path, f.Pkg, files[pkgs[i]])
	})
	for i, pkg := range pkgs {
		f := s.emission.files[pkg]
		if errs[i] != nil {
			fatalf("typescript: resolve record: %s: %v; rerun with -ts_resolve_record=off", f.Path, errs[i])
		}
		f.File, f.Rules, f.Loads, f.Directives = recorded[i].File, recorded[i].Rules, recorded[i].Loads, recorded[i].Directives
	}
	for _, line := range rec.replay.Log {
		fmt.Fprintln(log.Writer(), line)
	}
	if listingVerbose {
		log.Print("typescript: resolve record: replayed; Resolve and AfterResolvingDeps start from the state of a recorded run")
	}
}

// finish runs at the end of AfterResolvingDeps of a run that did the work
// and writes its record.
func (rec *resolveRecorder) finish(s *programStore) {
	resolvePhase.Store(nil)
	log.SetOutput(rec.stderr)
	next, why := rec.candidate(s, rec.log.Bytes())
	if why == "" {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(next); err != nil {
			why = err.Error()
		} else if err := writeAtomically(rec.file, b.Bytes()); err != nil {
			why = err.Error()
		}
	}
	if why != "" && listingVerbose {
		log.Printf("typescript: resolve record: not recorded; %s", why)
	}
}

// captureTracked formats every tracked BUILD file as Gazelle will see it
// next: its load fixing syncs the file before anything else.
func (s *programStore) captureTracked() []recordedFile {
	pkgs := s.trackedFiles()
	files := make([]recordedFile, len(pkgs))
	parallel(len(pkgs), func(i int) {
		f := s.emission.files[pkgs[i]]
		f.Sync()
		files[i] = recordedFile{Pkg: pkgs[i], Content: bzl.Format(f.File)}
	})
	return files
}

// phaseLines is the extension's own lines of a phase's log, in order.
func phaseLines(logged []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(logged), "\n") {
		if strings.Contains(line, "typescript: ") && !strings.Contains(line, "typescript: resolve record:") {
			out = append(out, line)
		}
	}
	return out
}

// candidate is this run's record, or why there is none: every read must
// still read as it first did and none may be stamped inside the racy window.
func (rec *resolveRecorder) candidate(s *programStore, logged []byte) (*resolveRecord, string) {
	if rec.why != "" {
		return nil, rec.why
	}
	in := runInputs
	pendingWrites.Wait()
	in.pending.Wait()
	in.mu.Lock()
	incomplete := in.incomplete
	reads := map[string]string{}
	var whole []string
	for path, line := range in.files {
		if !isBuildFile(path, rec.buildNames) {
			reads[path] = line
			if in.whole[path] {
				whole = append(whole, path)
			}
		}
	}
	var dirs []recordedRead
	for _, rel := range slices.Sorted(maps.Keys(in.dirs)) {
		if snap := in.dirs[rel]; !rec.walkDirs[rel] {
			if snap.err != nil {
				incomplete = snap.err.Error()
			}
			dirs = append(dirs, recordedRead{Path: snap.abs, Line: snap.line})
		}
	}
	in.mu.Unlock()
	if incomplete != "" {
		return nil, incomplete
	}
	rec.mu.Lock()
	maps.Copy(reads, rec.buildBodies)
	rec.mu.Unlock()
	next := &resolveRecord{Magic: resolveRecordMagic, Key: rec.key, State: rec.state, Dirs: dirs, Log: phaseLines(logged), Facts: s.importFacts.seal()}
	rec.outsideMu.Lock()
	next.Outside, next.Trees = sortedReads(rec.outsideRead), sortedReads(rec.treeRead)
	rec.outsideMu.Unlock()
	next.Reads = sortedReads(reads)
	slices.Sort(whole)
	next.Whole = whole
	stamps := &newest{}
	if why := next.changed(rec.buildNames, rec.cache, stamps); why != "" {
		return nil, why + " during the run"
	}
	listings := map[string]*cachedListing{}
	loadedListings.Range(func(k, v any) bool {
		if !rec.walkListings[k.(string)] {
			listings[k.(string)] = v.(*cachedListing)
		}
		return true
	})
	for _, key := range slices.Sorted(maps.Keys(listings)) {
		entry := listings[key]
		if len(entry.ReadLines) != len(entry.Reads) || len(entry.DirDigests) != len(entry.Dirs) {
			return nil, "a listing kept no read-time state"
		}
		scans := make([]uint64, len(entry.Reads))
		for i := range entry.Reads {
			scans[i] = entry.scanOf(i)
		}
		next.Listings = append(next.Listings, recordedListing{Reads: entry.Reads, ReadLines: entry.ReadLines, Dirs: entry.Dirs, DirDigests: entry.DirDigests, Scans: scans})
	}
	if !stamps.at.Before(processStart.Add(-racyWindow)) {
		return nil, "an input changed within " + racyWindow.String() + " of the run's start"
	}
	next.Files = s.captureTracked()
	return next, ""
}

func sortedReads(lines map[string]string) []recordedRead {
	reads := make([]recordedRead, 0, len(lines))
	for _, path := range slices.Sorted(maps.Keys(lines)) {
		reads = append(reads, recordedRead{Path: path, Line: lines[path]})
	}
	return reads
}

// noteReplayedReads makes a replaying run's own record read what the replayed
// phase read, which this run skips: its files, directories and listings.
func (r *resolveRecord) noteReplayedReads() {
	in := runInputs
	if in == nil {
		return
	}
	for _, read := range r.Reads {
		if has(r.Whole, read.Path) {
			noteWholeRead(read.Path)
		} else {
			noteRead(read.Path)
		}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, dir := range r.Dirs {
		rel, ok := repoRelative(in.repoRoot, dir.Path)
		if !ok {
			continue
		}
		if _, seen := in.dirs[rel]; !seen {
			in.dirs[rel] = &dirSnapshot{abs: dir.Path, line: dir.Line}
		}
	}
	in.listings = append(in.listings, r.Listings...)
}
