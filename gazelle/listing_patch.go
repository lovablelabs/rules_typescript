package typescript

import (
	"bytes"
	"encoding/gob"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mikn/rules_typescript/gazelle/incremental"
	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

var listingPatched atomic.Int64

// pendingWrites are patched entries still being written; the run waits for
// them before it ends, so a later run reads either the old entry or the new one.
var pendingWrites sync.WaitGroup

// lastPatchRefusal says why the latest patchListing listed again, for -ts_verbose.
var lastPatchRefusal atomic.Value

func refuse(reason string) *program {
	lastPatchRefusal.Store(reason)
	return nil
}

type cachedSource struct {
	Specs []string
	Rest  string
}

var sourceSpecs sync.Map // absolute path -> cachedSource, or nil when unscannable

func scanSpecs(abs string) (cachedSource, bool) {
	if v, ok := sourceSpecs.Load(abs); ok {
		src, ok := v.(cachedSource)
		return src, ok
	}
	data, err := os.ReadFile(abs)
	var src cachedSource
	if err == nil {
		src.Specs, src.Rest, err = incremental.Specifiers(abs, data)
	}
	if err != nil {
		sourceSpecs.Store(abs, nil)
		return cachedSource{}, false
	}
	sourceSpecs.Store(abs, src)
	return src, true
}

func patchableSource(rel string) bool {
	return !strings.HasPrefix(rel, "../") && !strings.HasPrefix(rel, embeddedLibs) &&
		!strings.Contains(rel, "node_modules/") && incremental.SignatureSource(rel)
}

// recordPatchState keeps what patchListing needs: each read's stat line, each
// include root's name digest, and every listed source's specifiers.
func recordPatchState(entry *cachedListing, repoRoot string) {
	entry.Root = repoRoot
	entry.ReadLines = make([]string, len(entry.Reads))
	for i, read := range entry.Reads {
		entry.ReadLines[i] = listingStats.Line(read)
	}
	entry.DirDigests = make([]string, len(entry.Dirs))
	for i, dir := range entry.Dirs {
		entry.DirDigests[i] = listingNames.Digest(dir)
	}
	entry.Sources = map[string]cachedSource{}
	for _, file := range entry.Listing.Files {
		if !patchableSource(file) {
			continue
		}
		if src, ok := scanSpecs(filepath.Join(repoRoot, filepath.FromSlash(file))); ok {
			entry.Sources[file] = src
		}
	}
}

func extensionClass(rel string) string {
	switch path.Ext(rel) {
	case ".mts", ".mjs":
		return "m"
	case ".cts", ".cjs":
		return "c"
	}
	return "x"
}

// patchListing updates a listing made stale only by import edits, each added specifier
// resolved as an unchanged file in the same directory, mode and format resolves it.
func patchListing(entry *cachedListing, key, subject string) *program {
	if entry.Sources == nil || len(entry.ReadLines) != len(entry.Reads) || len(entry.DirDigests) != len(entry.Dirs) {
		return refuse("no patch state")
	}
	for i, dir := range entry.Dirs {
		if listingNames.Digest(dir) != entry.DirDigests[i] {
			return refuse("an include root gained or lost a file")
		}
	}
	changed := map[string]cachedSource{}
	for i, read := range entry.Reads {
		if listingStats.Line(read) == entry.ReadLines[i] {
			continue
		}
		rel, err := filepath.Rel(entry.Root, read)
		rel = filepath.ToSlash(rel)
		old, ok := entry.Sources[rel]
		if err != nil || !ok {
			return refuse("a changed read is not a listed source")
		}
		now, ok := scanSpecs(read)
		if !ok || now.Rest != old.Rest {
			return refuse("a changed source changed more than its imports")
		}
		changed[rel] = now
	}
	if len(changed) == 0 {
		return refuse("nothing changed")
	}
	p := programFromEntry(entry, subject)
	files := map[string]bool{}
	for _, f := range p.Files {
		files[f] = true
	}
	if missing := unreached(p, files); len(missing) > 0 {
		return refuse("the listing has files its edges do not reach, such as " + strings.Join(missing[:min(3, len(missing))], ", "))
	}
	block := 0
	for _, c := range p.candidates {
		block = max(block, c.block+1)
	}
	for rel, now := range changed {
		old := entry.Sources[rel]
		for _, spec := range old.Specs {
			if slices.Contains(now.Specs, spec) {
				continue
			}
			_, text, _ := strings.Cut(spec, ":")
			if slices.ContainsFunc(now.Specs, func(s string) bool { _, t, _ := strings.Cut(s, ":"); return t == text }) {
				return refuse("an import text kept in another mode")
			}
			p.Edges = slices.DeleteFunc(p.Edges, func(e explainfiles.Edge) bool { return e.From == rel && e.Specifier == text })
			p.candidates = slices.DeleteFunc(p.candidates, func(c resolutionCandidate) bool { return c.from == rel && c.specifier == text })
		}
		for _, spec := range now.Specs {
			if slices.Contains(old.Specs, spec) {
				continue
			}
			_, text, _ := strings.Cut(spec, ":")
			precedent := ""
			for other, src := range entry.Sources {
				if _, edited := changed[other]; !edited && other != rel && path.Dir(other) == path.Dir(rel) &&
					extensionClass(other) == extensionClass(rel) && slices.Contains(src.Specs, spec) {
					precedent = other
					break
				}
			}
			if precedent == "" {
				return refuse("no precedent in the directory")
			}
			var probes []resolutionCandidate
			for _, c := range p.candidates {
				if c.from == precedent && c.specifier == text {
					probes = append(probes, c)
				}
			}
			resolved := ""
			for i := range probes {
				probes[i].from, probes[i].block = rel, block
				resolved = probes[i].resolved
			}
			block++
			p.candidates = append(p.candidates, probes...)
			found := false
			for _, e := range p.Edges {
				if e.From == precedent && e.Specifier == text {
					e.From = rel
					p.Edges = append(p.Edges, e)
					found = true
					break
				}
			}
			if resolved != "" && (!found || !files[resolved]) {
				return refuse("the precedent resolution is not a listed edge")
			}
		}
		entry.Sources[rel] = now
	}
	reached := reachable(p)
	for f := range reached {
		if !files[f] {
			return refuse("a file left or entered the program")
		}
	}
	p.Files = slices.DeleteFunc(p.Files, func(f string) bool { return !reached[f] })
	entry.Listing = p.Listing
	entry.Candidates = entry.Candidates[:0]
	for _, c := range p.candidates {
		entry.Candidates = append(entry.Candidates, cachedCandidate{
			From: c.from, Specifier: c.specifier, Path: c.path, File: c.file, Metadata: c.metadata,
			Kind: c.kind, Block: c.block, Resolved: c.resolved, Skipped: c.skipped,
		})
	}
	for i, read := range entry.Reads {
		entry.ReadLines[i] = listingStats.Line(read)
	}
	entry.Fingerprint = listingFingerprint(entry.Reads, entry.Dirs)
	listingPatched.Add(1)
	return p
}

func programFromEntry(entry *cachedListing, subject string) *program {
	p := &program{Listing: entry.Listing, config: subject}
	p.Files = slices.Clone(p.Files)
	p.Edges = slices.Clone(p.Edges)
	for _, c := range entry.Candidates {
		p.candidates = append(p.candidates, resolutionCandidate{
			from: c.From, specifier: c.Specifier, path: c.Path, file: c.File, metadata: c.Metadata,
			kind: c.Kind, block: c.Block, resolved: c.Resolved, skipped: c.Skipped,
		})
	}
	return p
}

// reachable is the listing's files as its roots, type entries and edges reach them.
func reachable(p *program) map[string]bool {
	seen := map[string]bool{}
	var queue []string
	push := func(f string) {
		if f != "" && !seen[f] {
			seen[f] = true
			queue = append(queue, f)
		}
	}
	for _, f := range p.Roots {
		push(f)
	}
	for _, t := range append(slices.Clone(p.Types), p.Implicit...) {
		push(t.File)
	}
	// Default libs enter by compiler option, not by an edge or a root.
	for _, f := range p.Files {
		if strings.HasPrefix(f, embeddedLibs) || strings.HasPrefix(f, "../") {
			push(f)
		}
	}
	next := map[string][]string{}
	for _, e := range p.Edges {
		next[e.From] = append(next[e.From], e.To)
	}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		for _, to := range next[f] {
			push(to)
		}
	}
	return seen
}

// unreached is the listed files reachable does not reach, and any it reaches
// that are not listed: a listing it does not model is never patched.
func unreached(p *program, files map[string]bool) []string {
	reached := reachable(p)
	var out []string
	for f := range files {
		if !reached[f] {
			out = append(out, f)
		}
	}
	for f := range reached {
		if !files[f] {
			out = append(out, f+" (unlisted)")
		}
	}
	slices.Sort(out)
	return out
}

func writeListing(key string, entry *cachedListing) {
	var buf bytes.Buffer
	if gob.NewEncoder(&buf).Encode(entry) != nil {
		return
	}
	tmp, err := os.CreateTemp(listingCacheDir, key+".*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(buf.Bytes())
	if cerr := tmp.Close(); werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return
	}
	if os.Rename(tmp.Name(), filepath.Join(listingCacheDir, key)) != nil {
		os.Remove(tmp.Name())
	}
}
