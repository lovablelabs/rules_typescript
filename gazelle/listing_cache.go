package typescript

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mikn/rules_typescript/gazelle/incremental"
	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// listingCacheDir is -ts_listing_cache: tsgo listings kept across runs, each
// reused while every file, probe, config and include directory it read is unchanged.
var listingCacheDir string

// listingVerbose is -ts_verbose, for the cache's own reasons.
var listingVerbose bool

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
}

type cachedCandidate struct {
	From, Specifier, Path string
	File, Metadata        bool
	Kind                  explainfiles.EdgeKind
	Block                 int
	Resolved, Skipped     string
}

func listingKey(repoRoot, tsgo, subject string, args []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\n%s\n%s\n", repoRoot, subject)
	if info, err := os.Stat(tsgo); err == nil {
		fmt.Fprintf(h, "tsgo %s %d %d\n", tsgo, info.Size(), info.ModTime().UnixNano())
	}
	for _, arg := range args {
		if isProjection(arg) {
			data, _ := os.ReadFile(arg)
			// The projection excludes itself by its random name.
			data = bytes.ReplaceAll(data, []byte(filepath.Base(arg)), []byte(".gazelle-tsconfig-*"))
			fmt.Fprintf(h, "projection %s %s\n", filepath.Dir(arg), data)
			continue
		}
		fmt.Fprintf(h, "arg %s\n", arg)
	}
	return hex.EncodeToString(h.Sum(nil))
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
		entry.Candidates = append(entry.Candidates, cachedCandidate{
			From: c.from, Specifier: c.specifier, Path: c.path, File: c.file, Metadata: c.metadata,
			Kind: c.kind, Block: c.block, Resolved: c.resolved, Skipped: c.skipped,
		})
	}
	loadedListings.Store(key, entry)
	return programFromEntry(entry, p.config)
}

func loadListing(key, subject string) *program {
	if v, ok := loadedListings.Load(key); ok {
		listingHits.Add(1)
		return programFromEntry(v.(*cachedListing), subject)
	}
	data, err := os.ReadFile(filepath.Join(listingCacheDir, key))
	if err != nil {
		listingAbsent.Add(1)
		return nil
	}
	var entry cachedListing
	if gob.NewDecoder(bytes.NewReader(data)).Decode(&entry) != nil {
		listingAbsent.Add(1)
		return nil
	}
	if listingFingerprint(entry.Reads, entry.Dirs) != entry.Fingerprint {
		if p := patchListing(&entry, key, subject); p != nil {
			p = remember(key, &entry, p)
			// Nothing writes the entry after remember, so encoding it alongside the run is safe.
			pendingWrites.Add(1)
			go func() {
				defer pendingWrites.Done()
				writeListing(key, &entry)
			}()
			return p
		}
		if listingVerbose {
			log.Printf("typescript: listing cache: %s listed again: %v", subject, lastPatchRefusal.Load())
		}
		listingStale.Add(1)
		return nil
	}
	listingHits.Add(1)
	return remember(key, &entry, programFromEntry(&entry, subject))
}

func saveListing(key, repoRoot, subject string, args []string, p *program) {
	abs := func(rel string) string {
		if filepath.IsAbs(rel) {
			return filepath.Clean(rel)
		}
		return filepath.Join(repoRoot, filepath.FromSlash(rel))
	}
	entry := cachedListing{Listing: p.Listing}
	for _, file := range p.Files {
		if !strings.HasPrefix(file, embeddedLibs) {
			entry.Reads = append(entry.Reads, abs(file))
		}
	}
	for _, c := range p.candidates {
		entry.Candidates = append(entry.Candidates, cachedCandidate{
			From: c.from, Specifier: c.specifier, Path: c.path, File: c.file, Metadata: c.metadata,
			Kind: c.kind, Block: c.block, Resolved: c.resolved, Skipped: c.skipped,
		})
		for _, probed := range []string{c.path, c.resolved} {
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
		entry.Dirs = append(entry.Dirs, includeRoots(filepath.Dir(cfg), resolved)...)
	}
	slices.Sort(entry.Reads)
	entry.Reads = slices.Compact(entry.Reads)
	slices.Sort(entry.Dirs)
	entry.Dirs = slices.Compact(entry.Dirs)
	entry.Fingerprint = listingFingerprint(entry.Reads, entry.Dirs)
	recordPatchState(&entry, repoRoot)
	loadedListings.Store(key, &entry)
	if os.MkdirAll(listingCacheDir, 0o755) != nil {
		return
	}
	writeListing(key, &entry)
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
