package typescript

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"
	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/mikn/rules_typescript/gazelle/incremental"
)

// runRecordMode is -ts_run_record: on, verify or off.
var runRecordMode = "on"

const (
	runRecordName  = "run-record"
	runRecordMagic = "rules_typescript gazelle run record v4"
	// Set by the run that spawns a recording run: where it leaves its candidate.
	runRecordCandidateEnv = "RULES_TYPESCRIPT_GAZELLE_RUN_RECORD_CANDIDATE"
	// A test hook: a file each process appends the path it took to.
	runTraceEnv = "RULES_TYPESCRIPT_GAZELLE_TRACE"
	// git's racy-clean rule: a stamp this close to the run's start may hide a
	// same-size rewrite within one timestamp tick.
	racyWindow = 2 * time.Second
)

var processStart = time.Now()

// traceRun notes under runTraceEnv which path produced this process's output.
func traceRun(path string) {
	file := os.Getenv(runTraceEnv)
	if file == "" {
		return
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("typescript: trace: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, path)
}

// runInputs collects what a recording run reads; nil in any other run.
var runInputs = func() *inputSet {
	if os.Getenv(runRecordCandidateEnv) == "" {
		return nil
	}
	return &inputSet{files: map[string]string{}, dirs: map[string]*dirSnapshot{}}
}()

// inputSet holds each input's line as the run first saw it.
type inputSet struct {
	mu    sync.Mutex
	files map[string]string
	dirs  map[string]*dirSnapshot
	// named are the sources among files read by a name the extension was given.
	named      map[string]bool
	pending    sync.WaitGroup
	incomplete string
	key        string
	repoRoot   string
	cache      string
	buildNames []string
	// facts are the Resolve phase's import facts, sealed when it ends or taken from a replayed resolve record.
	facts []byte
	// listings are the reads of listings a replayed resolve record's phase read.
	listings []recordedListing
	// builds collects the build snapshot written beside the candidate.
	builds *buildFactsRecorder
	// whole are the inputs the extension read the content of by a name it was given.
	whole map[string]bool
}

type dirSnapshot struct {
	abs, line string
	err       error
}

var snapshotSlots = make(chan struct{}, 4*runtime.GOMAXPROCS(0))

// noteRead notes a read the extension makes by a name it was given; a source's
// existence read so is named, which no listing or edge accounts for.
func noteRead(path string) { noteInput(path, true, false) }

// noteSourceRead notes a read the listings' patch or Resolve's edges make of a
// source they reach.
func noteSourceRead(path string) { noteInput(path, false, false) }

func noteInput(path string, named, content bool) {
	// A projection is its listing's own, derived from configs noted apart, and gone when the run ends.
	if isProjection(path) {
		return
	}
	noteResolvePhaseRead(path)
	in := runInputs
	if in == nil {
		return
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	in.mu.Lock()
	_, seen := in.files[path]
	if named && programCandidate(path) {
		if in.named == nil {
			in.named = map[string]bool{}
		}
		in.named[path] = true
	}
	if content {
		if in.whole == nil {
			in.whole = map[string]bool{}
		}
		in.whole[path] = true
	}
	in.mu.Unlock()
	if seen {
		return
	}
	line, _ := fileLine(path)
	in.mu.Lock()
	if _, seen := in.files[path]; !seen {
		in.files[path] = line
	}
	in.mu.Unlock()
}

func noteIncomplete(reason string) {
	if in := runInputs; in != nil {
		in.mu.Lock()
		if in.incomplete == "" {
			in.incomplete = reason
		}
		in.mu.Unlock()
	}
}

func statInput(path string) (fs.FileInfo, error) {
	noteRead(path)
	return os.Stat(path)
}

func lstatInput(path string) (fs.FileInfo, error) {
	noteRead(path)
	return os.Lstat(path)
}

// noteWholeRead notes a read of an input's content by a name the extension was given.
func noteWholeRead(path string) { noteInput(path, true, true) }

func readInput(path string) ([]byte, error) {
	noteWholeRead(path)
	return os.ReadFile(path)
}

func statSourceInput(path string) (fs.FileInfo, error) {
	noteSourceRead(path)
	return os.Stat(path)
}

func lstatSourceInput(path string) (fs.FileInfo, error) {
	noteSourceRead(path)
	return os.Lstat(path)
}

func readSourceInput(path string) ([]byte, error) {
	noteSourceRead(path)
	return os.ReadFile(path)
}

// noteDirectiveFiles notes the files f's directive_file directives name: from
// Gazelle 0.54 the walk reads them into f's directives, outside the record.
func noteDirectiveFiles(repoRoot, rel string, f *rule.File) {
	if runInputs == nil || f == nil || !bytes.Contains(f.Content, []byte("directive_file")) {
		return
	}
	for line := range bytes.SplitSeq(f.Content, []byte("\n")) {
		if m := directiveFileLine.FindSubmatch(line); m != nil {
			noteRead(filepath.Join(repoRoot, filepath.FromSlash(rel), filepath.FromSlash(string(m[1]))))
		}
	}
}

var directiveFileLine = regexp.MustCompile(`^\s*#\s*gazelle:directive_file\s+(.*?)\s*$`)

// recordedDirInfo is walk.GetDirInfo for a directory the run depends on. The
// snapshot is taken off the walk's goroutine, right after Gazelle read it.
func recordedDirInfo(rel string) (walk.DirInfo, error) {
	info, err := walk.GetDirInfo(rel)
	in := runInputs
	if in == nil || err != nil || in.key == "" {
		return info, err
	}
	abs := filepath.Join(in.repoRoot, filepath.FromSlash(rel))
	if inCacheDir(abs, in.cache) {
		return info, err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if _, seen := in.dirs[rel]; !seen {
		snap := &dirSnapshot{abs: abs}
		in.dirs[rel] = snap
		in.pending.Go(func() {
			snapshotSlots <- struct{}{}
			snap.line, _, snap.err = dirLine(abs, in.buildNames, in.cache)
			<-snapshotSlots
		})
	}
	return info, err
}

func inCacheDir(path, dir string) bool {
	return dir != "" && (path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)))
}

// runRecord names its inputs; digest is over their lines, walkDigest over
// the walked directories' alone. exit, stdout and stderr are the recorded
// run's, which a replay writes back.
type runRecord struct {
	key, digest, walkDigest string
	// dirs are walked, files the extension opened, reads and roots what the
	// listings stat'ed and enumerated, judged as the listing cache judges them.
	dirs, files, reads, roots []string
	exit                      int
	stdout, stderr            []byte
	// facts are the run's import facts and sums its inputs' inputSums, which
	// the fast path reads when the record no longer matches.
	facts []byte
	sums  []uint64
	// scans are each read's scanSum, by reads' order; 0 where none stands for it.
	scans []uint64
}

func (r *runRecord) lists() []*[]string { return []*[]string{&r.dirs, &r.files, &r.reads, &r.roots} }

func (r *runRecord) encode() ([]byte, error) {
	var b bytes.Buffer
	if len(r.sums) != 0 && len(r.sums) != len(r.dirs)+len(r.files)+len(r.reads)+len(r.roots) {
		return nil, fmt.Errorf("%d input sums for %d inputs", len(r.sums), len(r.dirs)+len(r.files)+len(r.reads)+len(r.roots))
	}
	if len(r.scans) != 0 && len(r.scans) != len(r.reads) {
		return nil, fmt.Errorf("%d scan sums for %d reads", len(r.scans), len(r.reads))
	}
	fmt.Fprintf(&b, "%s\n%s\n%s\n%s\n%d %d %d %d\n%d %d %d %d %d %d\n", runRecordMagic, r.key, r.digest, r.walkDigest,
		len(r.dirs), len(r.files), len(r.reads), len(r.roots), r.exit, len(r.stdout), len(r.stderr), len(r.facts), len(r.sums), len(r.scans))
	for _, list := range r.lists() {
		for _, p := range *list {
			if strings.ContainsAny(p, "\n\r") {
				return nil, fmt.Errorf("an input path holds a line break: %q", p)
			}
			b.WriteString(p)
			b.WriteByte('\n')
		}
	}
	b.Write(r.stdout)
	b.Write(r.stderr)
	b.Write(r.facts)
	sums := make([]byte, 0, 8*(len(r.sums)+len(r.scans)))
	for _, list := range [][]uint64{r.sums, r.scans} {
		for _, sum := range list {
			sums = binary.BigEndian.AppendUint64(sums, sum)
		}
	}
	b.Write(sums)
	return b.Bytes(), nil
}

func loadRunRecord(file string) *runRecord {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	line := func() (string, bool) {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return "", false
		}
		l := string(data[:i])
		data = data[i+1:]
		return l, true
	}
	var header [6]string
	for i := range header {
		var ok bool
		if header[i], ok = line(); !ok {
			return nil
		}
	}
	if header[0] != runRecordMagic {
		return nil
	}
	r := &runRecord{key: header[1], digest: header[2], walkDigest: header[3]}
	numbers := func(text string, n int) []int {
		fields := strings.Fields(text)
		if len(fields) != n {
			return nil
		}
		out := make([]int, n)
		for i, f := range fields {
			v, err := strconv.Atoi(f)
			if err != nil || v < 0 {
				return nil
			}
			out[i] = v
		}
		return out
	}
	counts, tail := numbers(header[4], len(r.lists())), numbers(header[5], 6)
	if counts == nil || tail == nil {
		return nil
	}
	for i, list := range r.lists() {
		for range counts[i] {
			p, ok := line()
			if !ok {
				return nil
			}
			*list = append(*list, p)
		}
	}
	if n := tail[4]; n != 0 && n != counts[0]+counts[1]+counts[2]+counts[3] {
		return nil
	}
	if n := tail[5]; n != 0 && n != counts[2] {
		return nil
	}
	rest := len(data)
	for _, n := range tail[1:4] {
		if n > rest {
			return nil
		}
		rest -= n
	}
	if rest != 8*(tail[4]+tail[5]) {
		return nil
	}
	r.exit = tail[0]
	r.stdout, data = data[:tail[1]:tail[1]], data[tail[1]:]
	r.stderr, data = data[:tail[2]:tail[2]], data[tail[2]:]
	r.facts, data = data[:tail[3]:tail[3]], data[tail[3]:]
	words := make([]uint64, tail[4]+tail[5])
	for i := range words {
		words[i] = binary.BigEndian.Uint64(data[8*i:])
	}
	r.sums = words[:tail[4]:tail[4]]
	if tail[5] != 0 {
		r.scans = words[tail[4]:]
	}
	return r
}

func writeAtomically(file string, data []byte) error {
	return writeAtomicallyWith(file, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

func writeAtomicallyWith(file string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	if err := write(tmp); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// inputLines are the lines of a record's inputs, in its order.
type inputLines struct{ dirs, files, reads, roots []string }

func (r *runRecord) digests(l inputLines) (digest, walkDigest string) {
	walkDigest = digestOfWalk(r.dirs, l.dirs)
	h := sha256.New()
	fmt.Fprintf(h, "walk %s\n", walkDigest)
	for i, file := range r.files {
		fmt.Fprintf(h, "file %s %s\n", file, l.files[i])
	}
	for i, read := range r.reads {
		fmt.Fprintf(h, "read %s %s\n", read, l.reads[i])
	}
	for i, root := range r.roots {
		fmt.Fprintf(h, "names %s %s\n", root, l.roots[i])
	}
	return hex.EncodeToString(h.Sum(nil)), walkDigest
}

func digestOfWalk(dirs, lines []string) string {
	h := sha256.New()
	for i, dir := range dirs {
		fmt.Fprintf(h, "%s\n%s\n", dir, lines[i])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// newest is the latest change stamp among inputs, for the racy-clean rule.
type newest struct {
	mu sync.Mutex
	at time.Time
}

func (n *newest) see(t time.Time) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if t.After(n.at) {
		n.at = t
	}
	n.mu.Unlock()
}

func dirLines(dirs, buildNames []string, skip string, stamps *newest) ([]string, error) {
	lines := make([]string, len(dirs))
	errs := make([]error, len(dirs))
	parallel(len(dirs), func(i int) {
		var t time.Time
		lines[i], t, errs[i] = dirLine(dirs[i], buildNames, skip)
		stamps.see(t)
	})
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return lines, nil
}

func fileLines(files []string, stamps *newest) []string {
	lines := make([]string, len(files))
	parallel(len(files), func(i int) {
		var t time.Time
		lines[i], t = fileLine(files[i])
		stamps.see(t)
	})
	return lines
}

func listingReadLines(reads []string, stamps *newest) []string {
	lines := make([]string, len(reads))
	parallel(len(reads), func(i int) {
		lines[i] = incremental.EntryLine(reads[i])
		if stamps != nil {
			_, t := fileLine(reads[i])
			stamps.see(t)
		}
	})
	return lines
}

func rootLines(roots []string) []string {
	names := newListingNames()
	lines := make([]string, len(roots))
	parallel(len(roots), func(i int) { lines[i] = names.Digest(roots[i]) })
	return lines
}

func parallel(n int, do func(i int)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(n, 4*runtime.GOMAXPROCS(0)) {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				do(i)
			}
		})
	}
	wg.Wait()
}

// buildEntry is a dirLine's entry for a BUILD file: its content stands for its stat.
func buildEntry(name string, mode fs.FileMode, data []byte) string {
	return fmt.Sprintf("b %s %o %x", name, mode, sha256.Sum256(data))
}

// dirLine names every entry of dir, and stamps is the latest change among
// its files. A projection lives only inside one listing and the listing cache
// outside the inputs, so neither counts; a subdirectory counts by name.
func dirLine(dir string, buildNames []string, skip string) (string, time.Time, error) {
	var stamp time.Time
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "absent", stamp, nil
	}
	if err != nil {
		return "", stamp, err
	}
	var b strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		file := filepath.Join(dir, name)
		if isProjection(name) || file == skip {
			continue
		}
		if entry.IsDir() {
			fmt.Fprintf(&b, "d %s\n", name)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			fmt.Fprintf(&b, "gone %s\n", name)
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(file)
			fmt.Fprintf(&b, "l %s -> %s %s: ", name, target, statLine(info))
			stamp = later(stamp, changed(info))
			if info, err = os.Stat(file); err != nil {
				b.WriteString("dangling\n")
				continue
			}
		}
		switch {
		case info.IsDir():
			fmt.Fprintf(&b, "d %s\n", name)
			continue
		case info.Mode().IsRegular() && slices.Contains(buildNames, name):
			data, err := os.ReadFile(file)
			if err != nil {
				return "", stamp, err
			}
			b.WriteString(buildEntry(name, info.Mode(), data) + "\n")
		default:
			fmt.Fprintf(&b, "f %s %s\n", name, statLine(info))
		}
		stamp = later(stamp, changed(info))
	}
	return b.String(), stamp, nil
}

// A directory read is a probe of its existence; its stamp moves with every
// projection written into it. A link is read through, so its target counts.
func fileLine(file string) (string, time.Time) {
	info, err := os.Lstat(file)
	switch {
	case err != nil:
		return "absent", time.Time{}
	case info.IsDir():
		return "dir", time.Time{}
	case info.Mode()&fs.ModeSymlink == 0:
		return statLine(info), changed(info)
	}
	target, _ := os.Readlink(file)
	line := statLine(info) + " -> " + target
	switch to, err := os.Stat(file); {
	case err != nil:
		return line + " dangling", changed(info)
	case to.IsDir():
		return line + " dir", changed(info)
	default:
		return line + " " + statLine(to), later(changed(info), changed(to))
	}
}

func statLine(info fs.FileInfo) string {
	var ino uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	return fmt.Sprintf("%o %d %d %d %d", info.Mode(), info.Size(), info.ModTime().UnixNano(), changeTime(info).UnixNano(), ino)
}

// changed is the later of a file's mtime and ctime.
func changed(info fs.FileInfo) time.Time { return later(info.ModTime(), changeTime(info)) }

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// recordKey is what selects a record rather than what its inputs hold: the
// binary, the run's configuration, and every flag but those in skip.
func recordKey(magic string, fs *flag.FlagSet, c *config.Config, skip ...string) string {
	h := sha256.New()
	exe, _ := os.Executable()
	fmt.Fprintf(h, "%s\nroot %s\nwd %s\nfix %t\nbuild %q\nTSGO %q\nexe %s\n", magic, c.RepoRoot, c.WorkDir, c.ShouldFix, c.ValidBuildFileNames, os.Getenv("TSGO"), exe)
	fs.VisitAll(func(f *flag.Flag) {
		if !slices.Contains(skip, f.Name) {
			fmt.Fprintf(h, "flag %s=%q\n", f.Name, f.Value.String())
		}
	})
	for _, arg := range fs.Args() {
		fmt.Fprintf(h, "arg %q\n", arg)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func checkModeFlag(flag, mode string) error {
	switch mode {
	case "on", "verify", "off":
		return nil
	}
	return fmt.Errorf("-%s=%q: want on, verify or off", flag, mode)
}

// A profile asks for the work a replay skips.
func runRecordApplies(fs *flag.FlagSet, c *config.Config, fullWalk bool) bool {
	return runRecordMode != "off" && recordableRun(fs, c, fullWalk) && flagValue(fs, "cpuprofile") == "" && flagValue(fs, "memprofile") == ""
}

func flagValue(fs *flag.FlagSet, name string) string {
	if f := fs.Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}

// A replay stands only for a full, recursive fix or diff run of a binary
// whose one language is this one, reading and writing BUILD files in the tree
// and writing nothing else: another language's inputs are not recorded.
func recordableRun(fs *flag.FlagSet, c *config.Config, fullWalk bool) bool {
	if listingCacheDir == "" || !fullWalk || c.ReadBuildFilesDir != "" || c.WriteBuildFilesDir != "" {
		return false
	}
	for name := range c.Exts {
		if name != languageName && !strings.HasPrefix(name, "_") {
			return false
		}
	}
	if mode := flagValue(fs, "mode"); mode != "fix" && mode != "diff" {
		return false
	}
	return flagValue(fs, "patch") == ""
}

// gateOnRunRecord exits when the record replays. Otherwise the full pass runs
// as a child, which gathers a candidate record, and this process exits with
// the child's status; it returns only in the process that does the work.
func gateOnRunRecord(fs *flag.FlagSet, c *config.Config, fullWalk bool) error {
	if in := runInputs; in != nil {
		cache, _ := filepath.Abs(listingCacheDir)
		in.mu.Lock()
		in.key, in.repoRoot, in.cache, in.buildNames = recordKey(runRecordMagic, fs, c, "ts_run_record", "ts_fastpath"), c.RepoRoot, cache, c.ValidBuildFileNames
		in.mu.Unlock()
		if exe, err := os.Executable(); err == nil {
			noteRead(exe)
		}
		// Beside the listings' own reads: what an install rewrites.
		noteRead(filepath.Join(c.RepoRoot, "node_modules", ".modules.yaml"))
		noteRead(filepath.Join(c.RepoRoot, "node_modules", ".pnpm", "lock.yaml"))
		return nil
	}
	if !runRecordApplies(fs, c, fullWalk) {
		return nil
	}
	cache, err := filepath.Abs(listingCacheDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	key := recordKey(runRecordMagic, fs, c, "ts_run_record", "ts_fastpath")
	file := filepath.Join(cache, runRecordName+"-"+key[:16])
	if rec := loadRunRecord(file); rec != nil && rec.key == key {
		stamps := &newest{}
		matched, dirs := rec.check(c.ValidBuildFileNames, cache, stamps)
		if matched {
			if runRecordMode == "verify" {
				os.Exit(verifyReplay(fs, file, rec))
			}
			traceRun("run record")
			os.Stdout.Write(rec.stdout)
			os.Stderr.Write(rec.stderr)
			if listingVerbose {
				log.Print("typescript: run record: replayed; every input is unchanged since a run that changed nothing")
			}
			os.Exit(rec.exit)
		}
		if dirs != nil {
			tryFastPath(fs, c, file, rec, dirs, stamps)
		}
	}
	candidate := fmt.Sprintf("%s.candidate.%d", file, os.Getpid())
	code, stdout, stderr := runSelf(os.Args[1:], runRecordCandidateEnv+"="+candidate)
	if code == 0 {
		promoteRunRecord(candidate, file, key, c.ValidBuildFileNames, cache, stdout, stderr)
	}
	os.Remove(candidate)
	os.Remove(buildSnapshotFile(candidate))
	os.Exit(code)
	return nil
}

// matches compares the walk first: most edits change it, and it is the
// smaller part to read.
func (r *runRecord) matches(buildNames []string, cache string, stamps *newest) bool {
	ok, _ := r.check(buildNames, cache, stamps)
	return ok
}

// check is matches, and on a mismatch the walked directories' lines as they
// read now, or nil when they could not be read.
func (r *runRecord) check(buildNames []string, cache string, stamps *newest) (bool, []string) {
	dirs, err := dirLines(r.dirs, buildNames, cache, stamps)
	if err != nil {
		return false, nil
	}
	if digestOfWalk(r.dirs, dirs) != r.walkDigest {
		return false, dirs
	}
	digest, _ := r.digests(inputLines{dirs, fileLines(r.files, stamps), listingReadLines(r.reads, stamps), rootLines(r.roots)})
	return digest == r.digest, dirs
}

// The candidate holds the inputs as the run read them. They still hold only
// if nothing moved while it ran and it wrote no BUILD file; no input may be
// stamped inside the racy window; and the run could have written every BUILD
// file it wanted, since Gazelle logs a failed write and still exits 0.
func promoteRunRecord(candidate, file, key string, buildNames []string, cache string, stdout, stderr []byte) {
	rec := loadRunRecord(candidate)
	if rec == nil || rec.key != key {
		return
	}
	refuse := func(why string) {
		if listingVerbose {
			log.Printf("typescript: run record: not kept; %s", why)
		}
	}
	stamps := &newest{}
	if !rec.matches(buildNames, cache, stamps) {
		refuse("an input moved or a BUILD file was written during the run")
		return
	}
	if !stamps.at.Before(processStart.Add(-racyWindow)) {
		refuse("an input changed within " + racyWindow.String() + " of the run's start")
		return
	}
	var unwritable atomic.Bool
	parallel(len(rec.dirs), func(i int) {
		paths := []string{rec.dirs[i]}
		for _, name := range buildNames {
			if _, err := os.Lstat(filepath.Join(rec.dirs[i], name)); err == nil {
				paths = append(paths, filepath.Join(rec.dirs[i], name))
			}
		}
		for _, p := range paths {
			if syscall.Access(p, 2) != nil {
				unwritable.Store(true)
			}
		}
	})
	if unwritable.Load() {
		refuse("a walked directory or BUILD file is not writable")
		return
	}
	rec.exit, rec.stdout, rec.stderr = 0, stdout, stderr
	data, err := rec.encode()
	if err == nil {
		err = writeAtomically(file, data)
	}
	if err != nil {
		log.Printf("typescript: run record: %v", err)
		return
	}
	if err := os.Rename(buildSnapshotFile(candidate), buildSnapshotFile(file)); errors.Is(err, fs.ErrNotExist) {
		os.Remove(buildSnapshotFile(file))
	} else if err != nil {
		log.Printf("typescript: run record: %v", err)
	}
}

// writeRunCandidate runs at the end of the last callback before Gazelle
// writes; the spawning process keeps the candidate only if this run exits 0.
func writeRunCandidate() {
	in := runInputs
	if in == nil || in.key == "" || os.Getenv(runRecordCandidateEnv) == "" {
		return
	}
	pendingWrites.Wait()
	in.pending.Wait()
	in.mu.Lock()
	defer in.mu.Unlock()
	rec, why := in.candidate()
	if why == "" {
		var data []byte
		var err error
		if data, err = rec.encode(); err == nil {
			err = writeAtomically(os.Getenv(runRecordCandidateEnv), data)
		}
		if err != nil {
			why = err.Error()
		}
	}
	if why == "" && in.builds != nil {
		data, err := in.builds.seal(rec.walkDigest)
		if err == nil {
			err = writeAtomically(buildSnapshotFile(os.Getenv(runRecordCandidateEnv)), data)
		}
		if err != nil && listingVerbose {
			log.Printf("typescript: fast path: no build snapshot recorded: %v", err)
		}
	}
	if why != "" && listingVerbose {
		log.Printf("typescript: run record: not recorded; %s", why)
	}
}

// candidate is the record of the inputs as the run first saw them, each
// listing's as the listing itself saw them, or why there is none.
func (in *inputSet) candidate() (*runRecord, string) {
	if in.incomplete != "" {
		return nil, in.incomplete
	}
	rec := &runRecord{key: in.key}
	var lines inputLines
	for _, rel := range slices.Sorted(maps.Keys(in.dirs)) {
		snap := in.dirs[rel]
		if snap.err != nil {
			return nil, snap.err.Error()
		}
		rec.dirs = append(rec.dirs, snap.abs)
		lines.dirs = append(lines.dirs, snap.line)
	}
	rec.files = slices.Sorted(maps.Keys(in.files))
	for _, file := range rec.files {
		lines.files = append(lines.files, in.files[file])
	}
	reads, roots := map[string]string{}, map[string]string{}
	scans := map[string]uint64{}
	var why string
	merge := func(into map[string]string, path, line string) {
		if was, ok := into[path]; ok && was != line {
			why = path + " changed between two listings that read it"
		}
		into[path] = line
	}
	loadedListings.Range(func(_, v any) bool {
		entry := v.(*cachedListing)
		if len(entry.ReadLines) != len(entry.Reads) || len(entry.DirDigests) != len(entry.Dirs) {
			why = "a listing kept no read-time state"
			return false
		}
		for i, read := range entry.Reads {
			merge(reads, read, entry.ReadLines[i])
			mergeScan(scans, read, entry.scanOf(i))
		}
		for i, dir := range entry.Dirs {
			merge(roots, dir, entry.DirDigests[i])
		}
		return why == ""
	})
	for _, l := range in.listings {
		for i, read := range l.Reads {
			merge(reads, read, l.ReadLines[i])
			mergeScan(scans, read, l.scanOf(i))
		}
		for i, dir := range l.Dirs {
			merge(roots, dir, l.DirDigests[i])
		}
	}
	if why != "" {
		return nil, why
	}
	rec.reads, rec.roots = slices.Sorted(maps.Keys(reads)), slices.Sorted(maps.Keys(roots))
	rec.scans = make([]uint64, len(rec.reads))
	for i, read := range rec.reads {
		lines.reads = append(lines.reads, reads[read])
		if in.whole[read] {
			rec.scans[i] = wholeScan
		} else {
			rec.scans[i] = scans[read]
		}
	}
	for _, root := range rec.roots {
		lines.roots = append(lines.roots, roots[root])
	}
	rec.digest, rec.walkDigest = rec.digests(lines)
	rec.sums, rec.facts = inputSums(lines, rec.dirs, in.repoRoot, decodeNameReads(in.facts)), in.facts
	return rec, ""
}

func (r *runRecord) scansByRead() map[string]uint64 {
	scans := map[string]uint64{}
	if len(r.scans) == len(r.reads) {
		for i, read := range r.reads {
			scans[read] = r.scans[i]
		}
	}
	return scans
}

func (e *cachedListing) scanOf(i int) uint64 {
	rel, ok := repoRelative(e.Root, e.Reads[i])
	if !ok || !regularLine(e.ReadLines[i]) {
		return 0
	}
	return e.Sources[rel].Sum
}

// mergeScan keeps a read's scan sum unless two listings disagree on it.
func mergeScan(into map[string]uint64, read string, sum uint64) {
	if was, seen := into[read]; seen && was != sum {
		sum = 0
	}
	into[read] = sum
}

// verifyReplay runs the pass a replay stands for, without any cache, and
// fails unless it changes nothing and exits and logs as the record does.
func verifyReplay(fs *flag.FlagSet, file string, rec *runRecord) int {
	code, stdout, stderr := runSelf(argsWith(fs, "-mode=diff", "-ts_listing_cache=", "-ts_run_record=off"))
	fail := func(why string) int {
		os.Remove(file)
		log.Printf("typescript: run record: a full pass without caches %s; the record is removed", why)
		return 1
	}
	switch {
	case code == 1 && len(stdout) > 0:
		return fail("differs from the replayed record (diff above)")
	case code != rec.exit:
		return fail(fmt.Sprintf("exits %d where the record exits %d", code, rec.exit))
	case !slices.Equal(logLines(stderr), logLines(rec.stderr)):
		return fail(fmt.Sprintf("logs otherwise than the record:\n--- record\n%s--- full pass\n%s", rec.stderr, stderr))
	}
	log.Print("typescript: run record: verified; a full pass without caches changes nothing and logs the same")
	return 0
}

// argsWith is this run's arguments with flags added ahead of its directories.
func argsWith(fs *flag.FlagSet, flags ...string) []string {
	args := slices.Clone(os.Args[1:])
	at := len(args) - fs.NArg()
	if at > 0 && args[at-1] == "--" {
		at--
	}
	return slices.Insert(args, at, flags...)
}

// logLines is a log's lines without the caches' own, in an order that does
// not depend on how concurrent listings interleaved.
func logLines(log []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(log), "\n") {
		if line != "" && !strings.Contains(line, "typescript: listing cache") && !strings.Contains(line, "typescript: run record:") &&
			!strings.Contains(line, "typescript: resolve record:") && !strings.Contains(line, "typescript: fast path:") {
			out = append(out, line)
		}
	}
	slices.Sort(out)
	return out
}

// runSelf runs this binary with args and returns its exit status and what it
// wrote, which it also passes through.
func runSelf(args []string, env ...string) (int, []byte, []byte) {
	return runSelfTo(os.Stdout, os.Stderr, args, env...)
}

// runSelfTo is runSelf passing the child's output through to stdout and stderr.
func runSelfTo(stdoutTo, stderrTo io.Writer, args []string, env ...string) (int, []byte, []byte) {
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("typescript: run record: %v", err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(exe, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout, cmd.Stderr = io.MultiWriter(stdoutTo, &stdout), io.MultiWriter(stderrTo, &stderr)
	cmd.Env = os.Environ()
	if rf, err := runfiles.Env(); err == nil {
		cmd.Env = append(cmd.Env, rf...)
	}
	cmd.Env = append(cmd.Env, env...)
	dieWithParent(cmd)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer func() {
		signal.Stop(signals)
		close(signals)
	}()
	if err := cmd.Start(); err != nil {
		log.Fatalf("typescript: run record: %v", err)
	}
	go func() {
		for s := range signals {
			cmd.Process.Signal(s)
		}
	}()
	cmd.Wait()
	if cmd.ProcessState == nil {
		return 1, stdout.Bytes(), stderr.Bytes()
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal()), stdout.Bytes(), stderr.Bytes()
	}
	return cmd.ProcessState.ExitCode(), stdout.Bytes(), stderr.Bytes()
}
