package typescript

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mikn/rules_typescript/gazelle/incremental"
	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// listingCacheDir is -ts_listing_cache: tsgo listings kept across runs, each
// reused while every file, probe, config and include directory it read is unchanged.
var listingCacheDir string

// listingVerbose is -ts_verbose, for the cache's own reasons.
var listingVerbose bool

// listingPatchVerify is -ts_listing_patch_verify.
var listingPatchVerify bool

var listingNames = newListingNames()

// An include glob matches only source and JSON names, so a BUILD file or a
// README appearing cannot change what a listing enumerates.
func newListingNames() *incremental.NameTree {
	return &incremental.NameTree{Skip: listingNameSkipped, File: func(name string) bool {
		switch filepath.Ext(name) {
		case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", ".json":
			return true
		}
		return false
	}}
}

var listingStats = &incremental.StatCache{}

// A projection exists only while its own listing runs, under a random name.
func listingNameSkipped(name string) bool {
	return name == "node_modules" || isProjection(name)
}

var listingHits, listingAbsent, listingStale atomic.Int64

func logListingCache() {
	predecodes.finish()
	pendingWrites.Wait()
	if listingCacheDir != "" {
		log.Printf("typescript: listing cache %d hit(s), %d patched, %d new, %d stale", listingHits.Load(), listingPatched.Load(), listingAbsent.Load(), listingStale.Load())
	}
}

type cachedListing struct {
	Listing     explainfiles.Listing
	Candidates  []cachedCandidate
	Reads, Dirs []string
	Fingerprint string
	// For patchListing: the stat line of each read, the name digest of each
	// dir, and each listed source's specifiers, at the time of the listing.
	Root                  string
	ReadLines, DirDigests []string
	Sources               map[string]cachedSource
	// From PatchVersion 2: how the program resolves, which files its include
	// globs match, and the TypeScript files under its include roots they do not.
	PatchVersion int
	Resolution   resolutionFacts
	Config       *patchConfig
	Unmatched    []string
	// A files-list projection's files, as its key names them.
	FilesList []string
}

type cachedCandidate struct {
	From, Specifier, Path string
	File, Metadata        bool
	Kind                  explainfiles.EdgeKind
	Block                 int
	Resolved, Skipped     string
}

// listingID names a cached listing. A files-list projection's key also names
// its family, the listings of the same base with other files lists, which a new
// list is patched from.
type listingID struct {
	key, family string
	files       []string
}

func listingKey(repoRoot, tsgo, subject string, args []string) listingID {
	h := sha256.New()
	fmt.Fprintf(h, "v1\n%s\n%s\n", repoRoot, subject)
	if info, err := os.Stat(tsgo); err == nil {
		fmt.Fprintf(h, "tsgo %s %d %d\n", tsgo, info.Size(), info.ModTime().UnixNano())
	}
	var id listingID
	for _, arg := range args {
		if isProjection(arg) {
			data, _ := os.ReadFile(arg)
			// The projection excludes itself by its random name.
			data = bytes.ReplaceAll(data, []byte(filepath.Base(arg)), []byte(".gazelle-tsconfig-*"))
			if base, files, ok := splitFilesList(repoRoot, filepath.Dir(arg), data); ok {
				data, id.files = base, files
				fmt.Fprintf(h, "files-list projection\n")
			}
			fmt.Fprintf(h, "projection %s %s\n", filepath.Dir(arg), data)
			continue
		}
		fmt.Fprintf(h, "arg %s\n", arg)
	}
	id.key = hex.EncodeToString(h.Sum(nil))
	if id.files != nil {
		id.family = id.key
		f := sha256.New()
		for _, file := range id.files {
			fmt.Fprintf(f, "%s\n", file)
		}
		id.key += "-" + hex.EncodeToString(f.Sum(nil))
	}
	return id
}

// splitFilesList is a projection without its files list, and the list's files
// relative to repoRoot; ok is false when the projection has no files list.
func splitFilesList(repoRoot, dir string, data []byte) (base []byte, files []string, ok bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil, nil, false
	}
	var names []string
	if json.Unmarshal(fields["files"], &names) != nil || len(names) == 0 {
		return nil, nil, false
	}
	for _, name := range names {
		rel, err := filepath.Rel(repoRoot, filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return nil, nil, false
		}
		files = append(files, filepath.ToSlash(rel))
	}
	delete(fields, "files")
	base, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, false
	}
	return base, files, true
}

func isProjection(arg string) bool {
	return strings.HasPrefix(filepath.Base(arg), ".gazelle-tsconfig-")
}

func listingFingerprint(reads, dirs []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", listingStats.Digest(reads))
	for _, dir := range dirs {
		fmt.Fprintf(h, "%s %s\n", dir, listingNames.Digest(dir))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// loadedListings holds each entry a run has decoded and validated, so a listing
// asked for again in the run is rebuilt from memory.
var loadedListings sync.Map

// remember interns p and keeps its strings in the entry, which the run then reuses.
func remember(key string, entry *cachedListing, p *program) *program {
	internProgram(p)
	entry.Listing = p.Listing
	entry.Candidates = make([]cachedCandidate, 0, len(p.candidates))
	for _, c := range p.candidates {
		entry.Candidates = append(entry.Candidates, c.cached())
	}
	entry.Resolution = p.resolution
	loadedListings.Store(key, entry)
	kept := programFromEntry(entry, p.config)
	kept.key = key
	return kept
}

// loadListing is the cached listing for id, and whether it was patched.
func loadListing(id listingID, subject string) (*program, bool) {
	if v, ok := loadedListings.Load(id.key); ok {
		listingHits.Add(1)
		p := programFromEntry(v.(*cachedListing), subject)
		p.key = id.key
		return p, false
	}
	entry, fresh, err := predecodes.take(id.key)
	if errors.Is(err, errPredecodeMismatch) {
		log.Fatal(err)
	}
	if errors.Is(err, fs.ErrNotExist) && id.family != "" {
		return patchFromFamily(id, subject)
	}
	if err != nil {
		listingAbsent.Add(1)
		return nil, false
	}
	if !fresh {
		if p := patchListing(entry, subject, id.files); p != nil {
			return keepPatched(id, entry, p), true
		}
		if listingVerbose {
			log.Printf("typescript: listing cache: %s listed again: %v", subject, lastPatchRefusal.Load())
		}
		listingStale.Add(1)
		return nil, false
	}
	listingHits.Add(1)
	return remember(id.key, entry, programFromEntry(entry, subject)), false
}

func keepPatched(id listingID, entry *cachedListing, p *program) *program {
	p = remember(id.key, entry, p)
	// Nothing writes the entry after remember, so encoding it alongside the run is safe.
	pendingWrites.Go(func() { writeListing(id.key, entry) })
	return p
}

// patchFromFamily patches a files list with no entry of its own from its
// family's entries, nearest first, until one patches.
func patchFromFamily(id listingID, subject string) (*program, bool) {
	entries := familyEntries(id)
	if len(entries) == 0 {
		listingAbsent.Add(1)
		return nil, false
	}
	for _, entry := range entries {
		if p := patchListing(entry, subject, id.files); p != nil {
			return keepPatched(id, entry, p), true
		}
		if listingVerbose {
			log.Printf("typescript: listing cache: %s not patched from a files list of %d: %v", subject, len(entry.FilesList), lastPatchRefusal.Load())
		}
	}
	if listingVerbose {
		log.Printf("typescript: listing cache: %s listed again: no entry of its files list's family patched", subject)
	}
	listingStale.Add(1)
	return nil, false
}

func readListing(file string) (*cachedListing, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return decodeEntry(file, data)
}

func decodeEntry(file string, data []byte) (*cachedListing, error) {
	var entry cachedListing
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&entry); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &entry, nil
}

// familyCandidates bounds how many of a family's latest entries a new files
// list decodes and tries.
const familyCandidates = 4

// familyEntries are the latest entries of id's family, those whose files list
// differs from id's in TypeScript files alone first, then by fewest files apart.
func familyEntries(id listingID) []*cachedListing {
	dirEntries, err := os.ReadDir(listingCacheDir)
	if err != nil {
		return nil
	}
	type member struct {
		name    string
		modTime time.Time
	}
	var members []member
	for _, d := range dirEntries {
		rest, ok := strings.CutPrefix(d.Name(), id.family+"-")
		// A name with a dot is an entry still being written.
		if !ok || strings.Contains(rest, ".") {
			continue
		}
		if info, err := d.Info(); err == nil {
			members = append(members, member{d.Name(), info.ModTime()})
		}
	}
	slices.SortFunc(members, func(a, b member) int { return b.modTime.Compare(a.modTime) })
	want := setOf(id.files)
	type candidate struct {
		entry    *cachedListing
		scripts  bool
		distance int
	}
	var candidates []candidate
	for _, m := range members[:min(len(members), familyCandidates)] {
		entry, err := readListing(filepath.Join(listingCacheDir, m.name))
		if err != nil || entry.FilesList == nil {
			continue
		}
		c := candidate{entry: entry}
		have := setOf(entry.FilesList)
		for _, pair := range [][2]map[string]bool{{want, have}, {have, want}} {
			for f := range pair[0] {
				if !pair[1][f] {
					c.distance++
					c.scripts = c.scripts || rootStem(f) == ""
				}
			}
		}
		if listingVerbose {
			log.Printf("typescript: listing cache: files list of %d: family entry %s of %d files, %d apart", len(want), m.name, len(entry.FilesList), c.distance)
		}
		candidates = append(candidates, c)
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int {
		if a.scripts != b.scripts {
			if a.scripts {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.distance, b.distance)
	})
	entries := make([]*cachedListing, len(candidates))
	for i, c := range candidates {
		entries[i] = c.entry
	}
	return entries
}

func saveListing(id listingID, repoRoot, subject string, args []string, p *program) {
	abs := func(rel string) string {
		if filepath.IsAbs(rel) {
			return filepath.Clean(rel)
		}
		return filepath.Join(repoRoot, filepath.FromSlash(rel))
	}
	entry := cachedListing{Listing: p.Listing, Resolution: p.resolution, FilesList: id.files}
	for _, file := range p.Files {
		if !strings.HasPrefix(file, embeddedLibs) {
			entry.Reads = append(entry.Reads, abs(file))
		}
	}
	for _, c := range p.candidates {
		entry.Candidates = append(entry.Candidates, c.cached())
		for _, probed := range []string{c.path, c.resolved, c.skipped} {
			if probed != "" && !strings.HasPrefix(probed, embeddedLibs) {
				entry.Reads = append(entry.Reads, abs(probed))
			}
		}
	}
	var configs []string
	if filepath.Ext(subject) == ".json" {
		configs = append(configs, abs(subject))
	} else {
		// A vitest config is listed by name under --ignoreConfig: no chain, no globs.
		entry.Reads = append(entry.Reads, abs(subject))
	}
	for i, arg := range args {
		if arg == "-p" && i+1 < len(args) && isProjection(args[i+1]) {
			configs = append(configs, args[i+1])
		}
	}
	var project *tsconfig.Resolved
	projectDir := ""
	for _, cfg := range configs {
		resolved, err := tsconfig.ResolveWithAdmission(cfg, func(path string) error {
			if !isProjection(path) {
				entry.Reads = append(entry.Reads, path)
			}
			return nil
		})
		if err != nil {
			return
		}
		project, projectDir = resolved, filepath.Dir(cfg)
	}
	slices.Sort(entry.Reads)
	entry.Reads = slices.Compact(entry.Reads)
	// A projection's include replaces its base's, so only the last config's globs enumerate.
	if project != nil {
		entry.Dirs = includeRoots(projectDir, project)
	}
	slices.Sort(entry.Dirs)
	entry.Dirs = slices.Compact(entry.Dirs)
	entry.Fingerprint = listingFingerprint(entry.Reads, entry.Dirs)
	recordPatchState(&entry, repoRoot, project, projectDir)
	loadedListings.Store(id.key, &entry)
	if os.MkdirAll(listingCacheDir, 0o755) != nil {
		return
	}
	writeListing(id.key, &entry)
}

// includeRoots are the directories a config's include globs enumerate: each
// pattern's literal prefix, or the config's directory when nothing narrows it.
func includeRoots(cfgDir string, r *tsconfig.Resolved) []string {
	if r.Include == nil && r.Files != nil {
		return nil
	}
	dir, patterns := cfgDir, []string{"**/*"}
	if r.Include != nil {
		patterns = *r.Include
		if r.IncludeDir != "" {
			dir = r.IncludeDir
		}
	}
	var roots []string
	for _, pattern := range patterns {
		var literal []string
		for _, segment := range strings.Split(filepath.ToSlash(pattern), "/") {
			if strings.ContainsAny(segment, "*?[{") {
				break
			}
			literal = append(literal, segment)
		}
		root := filepath.Join(dir, filepath.FromSlash(strings.Join(literal, "/")))
		if info, err := os.Stat(root); err == nil && !info.IsDir() {
			root = filepath.Dir(root)
		}
		roots = append(roots, root)
	}
	return roots
}

// A listing's strings are substrings of tsgo's whole output; interning copies
// them out, so the output can be freed and every repeated path is held once.
var listingStrings sync.Map

func internString(s string) string {
	if v, ok := listingStrings.Load(s); ok {
		return v.(string)
	}
	v, _ := listingStrings.LoadOrStore(strings.Clone(s), strings.Clone(s))
	return v.(string)
}

func internProgram(p *program) {
	for _, list := range [][]string{p.Files, p.Roots, p.Diagnostics} {
		for i := range list {
			list[i] = internString(list[i])
		}
	}
	for i := range p.Edges {
		e := &p.Edges[i]
		e.From, e.To, e.Specifier = internString(e.From), internString(e.To), internString(e.Specifier)
	}
	for _, types := range [][]explainfiles.TypeEntry{p.Types, p.Implicit} {
		for i := range types {
			types[i].Entry, types[i].File = internString(types[i].Entry), internString(types[i].File)
		}
	}
	for i := range p.candidates {
		c := &p.candidates[i]
		c.from, c.specifier, c.path = internString(c.from), internString(c.specifier), internString(c.path)
		c.resolved, c.skipped = internString(c.resolved), internString(c.skipped)
	}
}
