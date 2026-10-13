package typescript

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
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
	resolveRecordMagic = "rules_typescript gazelle resolve record v1"
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
	Files    []recordedFile
	Log      []string
}

type recordedRead struct{ Path, Line string }

type recordedListing struct{ Reads, ReadLines, Dirs, DirDigests []string }

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
	// replay is the record a hit writes back; verify the one a full pass is checked against.
	replay, verify *resolveRecord
	state          string
	// Why this run writes no record, or "".
	why string
	// The directories and listings read before the first Resolve, which the state digest covers.
	walkDirs, walkListings map[string]bool
	log                    bytes.Buffer
	stderr                 io.Writer

	mu          sync.Mutex
	buildBodies map[string]string
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

// startResolveRecord readies a record for a run a run record could stand for,
// and makes the run note the extension's reads if nothing else does.
func startResolveRecord(fs *flag.FlagSet, c *config.Config, s *programStore) error {
	if resolveRecordMode == "off" || !recordableRun(fs, c, s.fullWalk) {
		return nil
	}
	cache, err := filepath.Abs(listingCacheDir)
	if err != nil {
		return fmt.Errorf("-ts_listing_cache: %w", err)
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return fmt.Errorf("-ts_listing_cache: %w", err)
	}
	// Everything a run is asked to do except where its caches live and how it is checked or profiled.
	key := recordKey(resolveRecordMagic, fs, c, "ts_run_record", "ts_resolve_record", "ts_listing_cache", "ts_listing_patch_verify", "ts_parallel", "cpuprofile", "memprofile", "trace")
	if runInputs == nil {
		runInputs = &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}, key: key, repoRoot: c.RepoRoot, cache: cache, buildNames: c.ValidBuildFileNames}
		if exe, err := os.Executable(); err == nil {
			noteRead(exe)
		}
	}
	for _, name := range []string{".bazelignore", "REPO.bazel"} {
		noteRead(filepath.Join(c.RepoRoot, name))
	}
	s.resolution = &resolveRecorder{
		file: filepath.Join(cache, resolveRecordName+"-"+key[:16]), key: key, cache: cache,
		buildNames: c.ValidBuildFileNames, buildBodies: map[string]string{},
	}
	return nil
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
	rec.state, rec.why = s.stateDigest(rec.generated, rec.indexed)
	if rec.why == "" {
		rec.why = s.untrackedRules(rec.generated)
	}
	rec.generated, rec.indexed = nil, nil
	if rec.why == "" {
		if old := loadResolveRecord(rec.file); old != nil && old.Key == rec.key && old.State == rec.state && old.readsHold(rec.buildNames, rec.cache) {
			if resolveRecordMode == "verify" {
				rec.verify = old
			} else {
				rec.replay = old
				return true
			}
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
	if gob.NewDecoder(bytes.NewReader(data)).Decode(&rec) != nil || rec.Magic != resolveRecordMagic {
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
		log.Fatalf("typescript: resolve record: the record holds other BUILD files than this run; rerun with -ts_resolve_record=off")
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
			log.Fatalf("typescript: resolve record: %s: %v; rerun with -ts_resolve_record=off", f.Path, errs[i])
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

// finish runs at the end of AfterResolvingDeps of a run that did the work:
// it checks a verified record, or writes this run's.
func (rec *resolveRecorder) finish(s *programStore) {
	resolvePhase.Store(nil)
	log.SetOutput(rec.stderr)
	logged := rec.log.Bytes()
	if old := rec.verify; old != nil {
		if why := old.differs(s.captureTracked(), logged); why != "" {
			os.Remove(rec.file)
			log.Fatalf("typescript: resolve record: the full pass %s; the record is removed", why)
		}
		log.Print("typescript: resolve record: verified; the full pass writes and logs what the record does")
		return
	}
	next, why := rec.candidate(s, logged)
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

func (r *resolveRecord) differs(files []recordedFile, logged []byte) string {
	want := map[string][]byte{}
	for _, f := range r.Files {
		want[f.Pkg] = f.Content
	}
	if len(want) != len(files) {
		return fmt.Sprintf("holds %d BUILD files where the record has %d", len(files), len(want))
	}
	for _, f := range files {
		if !bytes.Equal(want[f.Pkg], f.Content) {
			return "writes " + orRepoRoot(f.Pkg) + " otherwise than the record"
		}
	}
	if got, want := slices.Sorted(slices.Values(phaseLines(logged))), slices.Sorted(slices.Values(r.Log)); !slices.Equal(got, want) {
		return fmt.Sprintf("logs otherwise than the record:\n--- record\n%s\n--- full pass\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
	return ""
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
	for path, line := range in.files {
		if !isBuildFile(path, rec.buildNames) {
			reads[path] = line
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
	next := &resolveRecord{Magic: resolveRecordMagic, Key: rec.key, State: rec.state, Dirs: dirs, Log: phaseLines(logged)}
	for _, path := range slices.Sorted(maps.Keys(reads)) {
		next.Reads = append(next.Reads, recordedRead{Path: path, Line: reads[path]})
	}
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
		next.Listings = append(next.Listings, recordedListing{Reads: entry.Reads, ReadLines: entry.ReadLines, Dirs: entry.Dirs, DirDigests: entry.DirDigests})
	}
	if !stamps.at.Before(processStart.Add(-racyWindow)) {
		return nil, "an input changed within " + racyWindow.String() + " of the run's start"
	}
	next.Files = s.captureTracked()
	return next, ""
}
