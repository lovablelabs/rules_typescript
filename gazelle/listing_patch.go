package typescript

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/mikn/rules_typescript/gazelle/incremental"
	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/jsonc"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

var listingPatched atomic.Int64

// pendingWrites are listing entries still being written; the run waits for
// them before it ends, so a later run reads either the old entry or the new one.
var pendingWrites sync.WaitGroup

// lastPatchRefusal says why the latest patchListing listed again, for -ts_verbose.
var lastPatchRefusal atomic.Value

func refuse(reason string) *program {
	lastPatchRefusal.Store(reason)
	return nil
}

const patchStateVersion = 7

type cachedSource struct {
	Specs []string
	Rest  string
	// Sum is scanSum of the whole scan; 0 in an entry recorded before it existed.
	Sum            uint64
	ResolutionMode bool
}

var sourceSpecs sync.Map // absolute path -> scannedSource, or nil when unscannable

type scannedSource struct {
	incremental.Scanned
	sum uint64
}

func scanFile(abs string) (scannedSource, bool) {
	if v, ok := sourceSpecs.Load(abs); ok {
		src, ok := v.(scannedSource)
		return src, ok
	}
	data, err := readSourceInput(abs)
	var scanned incremental.Scanned
	if err == nil {
		scanned, err = incremental.Scan(abs, data)
	}
	if err != nil {
		sourceSpecs.Store(abs, nil)
		return scannedSource{}, false
	}
	src := scannedSource{scanned, scanSum(scanned)}
	sourceSpecs.Store(abs, src)
	return src, true
}

func scanSpecs(abs string) (cachedSource, bool) {
	src, ok := scanFile(abs)
	return cachedSource{Specs: src.Specs, Rest: src.Rest, Sum: src.sum, ResolutionMode: src.ResolutionMode}, ok
}

// scanSum covers every field of a scan, which is all a listing or its patch reads
// of a source; an edit that keeps it keeps every listing the source is in. An
// uncounted or inexact scan has none: it may miss an import tsgo reads.
func scanSum(src incremental.Scanned) uint64 {
	if !src.Counted || !src.Exact {
		return 0
	}
	data, err := json.Marshal(src)
	if err != nil {
		return 0
	}
	if sum := lineSum(fmt.Sprintf("%d %s", patchStateVersion, data)); sum > jsonScan {
		return sum
	}
	return jsonScan + 1
}

// In a run record's scans, wholeScan stands for a source the extension read whole, and
// jsonScan for a JSON module a listing holds, whose content moves only tsgo's diagnostics.
const (
	wholeScan uint64 = 1
	jsonScan  uint64 = 2
)

func jsonModule(file string) bool {
	return path.Ext(file) == ".json" && !readByGazelle(path.Base(file)) && !strings.HasPrefix(file, "../") && !strings.Contains(file, "node_modules/")
}

// jsonModuleRead says whether the listing holds JSON file rel as a module and takes
// no compiler options from it; an entry of an older patch state keeps no Configs.
func (e *cachedListing) jsonModuleRead(rel string) bool {
	return e.PatchVersion == patchStateVersion && !e.PackageExtends && jsonModule(rel) && slices.Contains(e.Listing.Files, rel) &&
		!slices.Contains(e.Configs, filepath.Join(e.Root, filepath.FromSlash(rel)))
}

func patchableSource(rel string) bool {
	return !strings.HasPrefix(rel, "../") && !strings.HasPrefix(rel, embeddedLibs) &&
		!strings.Contains(rel, "node_modules/") && incremental.SignatureSource(rel)
}

// patchConfig is what patchListing needs from the program's tsconfig chain to
// decide which files its include globs match.
type patchConfig struct {
	Include, Exclude       []string
	IncludeDir, ExcludeDir string
	ModuleSuffixes         bool
	// Why no root may be added or removed without tsgo; empty when one may.
	Refused  string
	Paths    []tsconfig.Path
	PathsDir string
}

// recordPatchState keeps each read's stat line, each include root's name digest,
// every listed source's specifiers, the include globs and the files they skip.
func recordPatchState(entry *cachedListing, repoRoot string, project *tsconfig.Resolved, projectDir string) {
	entry.Root = repoRoot
	entry.PatchVersion = patchStateVersion
	entry.ReadLines = make([]string, len(entry.Reads))
	parallel(len(entry.Reads), func(i int) { entry.ReadLines[i] = listingStats.Line(entry.Reads[i]) })
	entry.DirDigests = make([]string, len(entry.Dirs))
	parallel(len(entry.Dirs), func(i int) { entry.DirDigests[i] = listingNames.Digest(entry.Dirs[i]) })
	patchable := slices.DeleteFunc(slices.Clone(entry.Listing.Files), func(file string) bool { return !patchableSource(file) })
	scans := make([]struct {
		src cachedSource
		ok  bool
	}, len(patchable))
	parallel(len(patchable), func(i int) {
		scans[i].src, scans[i].ok = scanSpecs(filepath.Join(repoRoot, filepath.FromSlash(patchable[i])))
	})
	imported := map[string]map[string]bool{}
	note := func(kind explainfiles.EdgeKind, from, specifier string) {
		if kind == explainfiles.Import {
			if imported[from] == nil {
				imported[from] = map[string]bool{}
			}
			imported[from][specifier] = true
		}
	}
	for _, e := range entry.Listing.Edges {
		note(e.Kind, e.From, e.Specifier)
	}
	for _, u := range entry.Listing.Unresolved {
		note(u.Kind, u.From, u.Specifier)
	}
	entry.Sources = map[string]cachedSource{}
	for i, file := range patchable {
		// A scan that misses an import tsgo resolved vouches for no edit to the source.
		if scans[i].ok && scannedAsListed(scans[i].src.Specs, imported[file]) {
			entry.Sources[file] = scans[i].src
		}
	}
	entry.Config = newPatchConfig(project, projectDir)
	if entry.Config.Refused != "" {
		return
	}
	files, symlink := includedTypeScript(repoRoot, entry.Dirs)
	if symlink {
		entry.Config.Refused = "an include root holds a symlink"
		return
	}
	roots := map[string]bool{}
	for _, root := range entry.Listing.Roots {
		roots[root] = true
	}
	for _, file := range slices.Sorted(maps.Keys(files)) {
		if !roots[file] {
			entry.Unmatched = append(entry.Unmatched, file)
		}
	}
}

// scannedAsListed says whether a scan read every import tsgo resolved from the
// source, beside those tsgo adds for JSX or helpers, which the scan's rest covers.
// A scanned import tsgo lists nowhere is common: an ambient module declares it.
func scannedAsListed(specs []string, listed map[string]bool) bool {
	scanned := map[string]bool{}
	for _, spec := range specs {
		_, text, _ := strings.Cut(spec, ":")
		scanned[text] = true
	}
	for text := range listed {
		if !scanned[text] && text != "tslib" && !strings.HasSuffix(text, "/jsx-runtime") && !strings.HasSuffix(text, "/jsx-dev-runtime") {
			return false
		}
	}
	return true
}

func newPatchConfig(project *tsconfig.Resolved, projectDir string) *patchConfig {
	if project == nil {
		return &patchConfig{Refused: "its roots are named on the command line"}
	}
	options := readPatchOptions(project.Configs)
	c := &patchConfig{IncludeDir: project.IncludeDir, ExcludeDir: project.ExcludeDir, ModuleSuffixes: options.suffixes}
	if project.Paths != nil {
		c.Paths, c.PathsDir = *project.Paths, project.PathsDir
	}
	switch {
	case project.Files != nil:
		c.Refused = "a files list names roots"
	case options.allowJs:
		c.Refused = "allowJs adds JavaScript roots"
	case options.refused != "":
		c.Refused = options.refused
	}
	if project.Include == nil {
		c.Include, c.IncludeDir = []string{"**/*"}, projectDir
	} else {
		c.Include = *project.Include
	}
	if project.Exclude != nil {
		c.Exclude = *project.Exclude
	} else {
		c.Exclude = options.outputs
	}
	namesConfigDir := func(spec string) bool { return strings.Contains(spec, "${") }
	if slices.ContainsFunc(c.Include, namesConfigDir) || slices.ContainsFunc(c.Exclude, namesConfigDir) {
		c.Refused = "an include or exclude spec names ${configDir}"
	}
	for _, spec := range c.Include {
		if strings.HasSuffix(spec, ".json") {
			c.Refused = "an include spec matches JSON"
		}
	}
	return c
}

type patchOptions struct {
	allowJs, suffixes bool
	outputs           []string
	refused           string
}

// readPatchOptions reads the compilerOptions tsc's include and relative
// resolution depend on from a chain, leaf last, each value as its writer wrote it.
func readPatchOptions(configs []string) patchOptions {
	var o patchOptions
	var outDir, declarationDir string
	for _, cfg := range configs {
		data, err := readInput(cfg)
		if err != nil {
			o.refused = "a config in the chain is unreadable"
			return o
		}
		var f struct {
			CompilerOptions map[string]any `json:"compilerOptions"`
		}
		if jsonc.Unmarshal(data, &f) != nil {
			o.refused = "a config in the chain is unreadable"
			return o
		}
		for key, value := range f.CompilerOptions {
			switch key {
			case "allowJs":
				o.allowJs = value == true
			case "moduleSuffixes":
				o.suffixes = value != nil
			case "outDir", "declarationDir":
				s, _ := value.(string)
				if s != "" && !filepath.IsAbs(s) {
					s = filepath.Join(filepath.Dir(cfg), s)
				}
				if key == "outDir" {
					outDir = s
				} else {
					declarationDir = s
				}
			}
		}
	}
	for _, dir := range []string{outDir, declarationDir} {
		if strings.Contains(dir, "${") {
			o.refused = "an output directory names ${configDir}"
		}
		if dir != "" {
			o.outputs = append(o.outputs, dir)
		}
	}
	return o
}

var typeScriptRootExtensions = []string{".d.mts", ".d.cts", ".d.ts", ".mts", ".cts", ".tsx", ".ts"}

// rootStem is a file without its TypeScript extension; "" when it has none.
func rootStem(rel string) string {
	for _, ext := range typeScriptRootExtensions {
		if before, ok := strings.CutSuffix(rel, ext); ok {
			return before
		}
	}
	return ""
}

// includedTypeScript is every file a TypeScript include glob could match under
// dirs, as tsgo's directory walk and the listing's name digests both see them.
func includedTypeScript(repoRoot string, dirs []string) (map[string]bool, bool) {
	files := map[string]bool{}
	symlink := false
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			switch {
			case listingNameSkipped(name):
			case entry.Type()&fs.ModeSymlink != 0:
				symlink = true
			case entry.IsDir():
				walk(filepath.Join(dir, name))
			case rootStem(name) != "":
				if rel, err := filepath.Rel(repoRoot, filepath.Join(dir, name)); err == nil {
					files[filepath.ToSlash(rel)] = true
				}
			}
		}
	}
	for _, dir := range dirs {
		walk(dir)
	}
	return files, symlink
}

// A port of tsgo's vfsmatch for one absolute path: include globs skip hidden and
// package directories under a wildcard, and an exclude matches everything below it.
type globComponent struct {
	kind     byte // 'l' literal, 'w' wildcard, 'r' **
	literal  string
	segments []string // "*", "?" or a literal run
}

type globPattern struct {
	components []globComponent
	exclude    bool
}

func compileGlob(spec, base string, exclude bool) (globPattern, bool) {
	abs := spec
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(base, spec)
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(abs)), "/")
	last := parts[len(parts)-1]
	if !exclude && last == "**" {
		return globPattern{}, false
	}
	if !strings.ContainsAny(last, ".*?") {
		parts = append(parts, "**", "*")
	}
	p := globPattern{exclude: exclude}
	for _, part := range parts {
		switch {
		case part == "**":
			p.components = append(p.components, globComponent{kind: 'r'})
		case !strings.ContainsAny(part, "*?"):
			p.components = append(p.components, globComponent{kind: 'l', literal: part})
		default:
			c := globComponent{kind: 'w'}
			start := 0
			for i := 0; i < len(part); i++ {
				if part[i] == '*' || part[i] == '?' {
					if i > start {
						c.segments = append(c.segments, part[start:i])
					}
					c.segments = append(c.segments, part[i:i+1])
					start = i + 1
				}
			}
			if start < len(part) {
				c.segments = append(c.segments, part[start:])
			}
			p.components = append(p.components, c)
		}
	}
	return p, true
}

func packageFolder(name string) bool {
	return strings.EqualFold(name, "node_modules") || strings.EqualFold(name, "bower_components") || strings.EqualFold(name, "jspm_packages")
}

func (p *globPattern) match(parts []string, i, ci int) bool {
	for ; i < len(parts); i, ci = i+1, ci+1 {
		if ci >= len(p.components) {
			return p.exclude
		}
		part, c := parts[i], p.components[ci]
		switch c.kind {
		case 'r':
			if p.match(parts, i, ci+1) {
				return true
			}
			if !p.exclude && (strings.HasPrefix(part, ".") || packageFolder(part)) {
				return false
			}
			ci--
		case 'l':
			if c.literal != part {
				return false
			}
		case 'w':
			if !p.exclude && packageFolder(part) || !p.matchWildcard(c.segments, part) {
				return false
			}
		}
	}
	for _, c := range p.components[ci:] {
		if c.kind != 'r' {
			return false
		}
	}
	return true
}

func (p *globPattern) matchWildcard(segments []string, s string) bool {
	if !p.exclude && strings.HasPrefix(s, ".") && (segments[0] == "*" || segments[0] == "?") {
		return false
	}
	if !p.exclude && strings.HasSuffix(s, ".min.js") && !slices.ContainsFunc(segments, func(seg string) bool { return strings.Contains(seg, ".min.") }) {
		return false
	}
	// tsgo's matchSegments: a star backtracks only to its latest position, so a
	// spec of many stars stays linear, and a star or question mark consumes a rune.
	si, seg, starSeg, starS := 0, 0, -1, 0
	for si < len(s) {
		if seg < len(segments) {
			switch lit := segments[seg]; lit {
			case "*":
				starSeg, starS = seg, si
				seg++
				continue
			case "?":
				_, size := utf8.DecodeRuneInString(s[si:])
				si += size
				seg++
				continue
			default:
				if strings.HasPrefix(s[si:], lit) {
					si += len(lit)
					seg++
					continue
				}
			}
		}
		if starSeg < 0 {
			return false
		}
		_, size := utf8.DecodeRuneInString(s[starS:])
		starS += size
		si, seg = starS, starSeg+1
	}
	for seg < len(segments) && segments[seg] == "*" {
		seg++
	}
	return seg == len(segments)
}

type rootMatcher struct{ includes, excludes []globPattern }

func newRootMatcher(c *patchConfig) *rootMatcher {
	m := &rootMatcher{}
	for _, spec := range c.Include {
		trimmed := strings.TrimSuffix(spec, "/")
		if trimmed == "**" || strings.HasSuffix(trimmed, "/**") || dotDotAfterRecursion(spec) {
			continue
		}
		if p, ok := compileGlob(spec, c.IncludeDir, false); ok {
			m.includes = append(m.includes, p)
		}
	}
	for _, spec := range c.Exclude {
		if dotDotAfterRecursion(spec) {
			continue
		}
		if p, ok := compileGlob(spec, c.ExcludeDir, true); ok {
			m.excludes = append(m.excludes, p)
		}
	}
	return m
}

func dotDotAfterRecursion(spec string) bool {
	_, after, found := strings.Cut(filepath.ToSlash(spec), "**")
	return found && slices.Contains(strings.Split(after, "/"), "..")
}

func (m *rootMatcher) matches(abs string) bool {
	parts := strings.Split(filepath.ToSlash(abs), "/")
	for i := range m.excludes {
		if m.excludes[i].match(parts, 0, 0) {
			return false
		}
	}
	for i := range m.includes {
		if m.includes[i].match(parts, 0, 0) {
			return true
		}
	}
	return false
}

// existence is what a stat line says a resolver probe would find.
func existence(line string) string {
	if line == "absent" {
		return line
	}
	field, _, _ := strings.Cut(line, " ")
	mode, err := strconv.ParseUint(field, 8, 64)
	switch {
	case err != nil:
		return line
	case fs.FileMode(mode)&fs.ModeSymlink != 0:
		return line
	case fs.FileMode(mode).IsDir():
		return "dir"
	}
	return "file"
}

// regularLine is whether a stat line is a regular file's.
func regularLine(line string) bool {
	field, _, _ := strings.Cut(line, " ")
	mode, err := strconv.ParseUint(field, 8, 32)
	return err == nil && fs.FileMode(mode).IsRegular()
}

type importKey struct {
	from, specifier string
}

// A patcher updates one stale listing; anything it does not model refuses, and
// the listing is listed again by tsgo.
type patcher struct {
	entry       *cachedListing
	root        string
	p           *program
	files       map[string]bool
	blocks      map[int][]resolutionCandidate
	blocksOf    map[importKey][]int
	edgesOf     map[importKey][]int
	nextBlock   int
	dirty       map[int]bool
	sources     map[string]cachedSource
	scopes, nms map[string]string
	specOwners  map[string][]string
	implicit    map[string]map[string]int
	entering    []string
	// The files list a files-list projection now names.
	filesList []string
	// Reads the patch adds to the listing's, and files it drops from it.
	newReads, dropped []string
	refusal           string
	jsonEdited        bool
}

func (t *patcher) fail(reason string) bool {
	if t.refusal == "" {
		t.refusal = reason
	}
	return false
}

func (t *patcher) abs(rel string) string {
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "../") {
		return filepath.Join(t.root, filepath.FromSlash(rel))
	}
	return t.root + string(filepath.Separator) + filepath.FromSlash(rel)
}

func (t *patcher) rel(abs string) (string, bool) {
	rel, err := filepath.Rel(t.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// patchListing updates a listing staled by import edits, added or deleted
// files and a changed files list, keeping every resolution whose probes still
// find what they found.
func patchListing(entry *cachedListing, subject string, filesList []string) *program {
	p, why := tryPatchListing(entry, subject, filesList)
	if p == nil {
		return refuse(why)
	}
	return p
}

// tryPatchListing is patchListing returning why it refused, for callers that patch concurrently.
func tryPatchListing(entry *cachedListing, subject string, filesList []string) (*program, string) {
	if entry.PatchVersion != patchStateVersion || entry.Sources == nil || entry.Config == nil ||
		len(entry.ReadLines) != len(entry.Reads) || len(entry.DirDigests) != len(entry.Dirs) {
		return nil, "no patch state"
	}
	if listingVerbose && entry.StaleDiagnostics {
		return nil, "a patch kept diagnostics -ts_verbose logs"
	}
	t := &patcher{entry: entry, root: entry.Root, filesList: filesList, dirty: map[int]bool{}, scopes: map[string]string{}, nms: map[string]string{}}
	p := t.patch(subject)
	if p == nil {
		return nil, t.refusal
	}
	listingPatched.Add(1)
	return p, ""
}

func (t *patcher) patch(subject string) *program {
	entry := t.entry
	t.p = programFromEntry(entry, subject)
	p := t.p
	t.sources = entry.Sources
	t.files = map[string]bool{}
	for _, f := range p.Files {
		t.files[f] = true
	}
	if missing := unreached(p, t.files); len(missing) > 0 {
		t.fail("the listing has files its edges do not reach, such as " + strings.Join(missing[:min(3, len(missing))], ", "))
		return nil
	}
	t.index()

	changedLines := map[string][2]string{}
	for i, read := range entry.Reads {
		if now := listingStats.Line(read); now != entry.ReadLines[i] {
			rel, ok := t.rel(read)
			if !ok {
				t.fail("a read outside the repository changed")
				return nil
			}
			changedLines[rel] = [2]string{entry.ReadLines[i], now}
		}
	}
	// A probe matters by what it finds, but a package.json tsgo found is read too.
	probed := map[string]bool{}
	for _, c := range p.candidates {
		for _, path := range []string{c.path, c.resolved, c.skipped} {
			lines, changed := changedLines[path]
			if !changed {
				continue
			}
			probed[path] = true
			if existence(lines[0]) != existence(lines[1]) || c.metadata && path == c.path {
				t.dirty[c.block] = true
			}
		}
	}
	deleted, edited := map[string]bool{}, map[string]cachedSource{}
	for _, rel := range slices.Sorted(maps.Keys(changedLines)) {
		now := changedLines[rel][1]
		if !t.files[rel] {
			if !probed[rel] {
				t.fail("a config or scope probe the listing read changed")
				return nil
			}
			continue
		}
		switch {
		case existence(now) != "file":
			deleted[rel] = true
		case patchableSource(rel):
			src, ok := scanSpecs(t.abs(rel))
			old, known := t.sources[rel]
			if !ok || !known {
				t.fail("a changed source is unscannable")
				return nil
			}
			if src.Rest != old.Rest {
				t.fail("a changed source changed more than its imports")
				return nil
			}
			if src.ResolutionMode || old.ResolutionMode {
				t.fail("a changed source names a resolution-mode")
				return nil
			}
			if !t.resolvedOnce(rel, old, src) {
				return nil
			}
			if !slices.Equal(src.Specs, old.Specs) {
				edited[rel] = src
			} else {
				t.sources[rel] = src
			}
		case entry.jsonModuleRead(rel) && !listingVerbose:
			t.jsonEdited = true
		default:
			t.fail("a changed read is not a listed source")
			return nil
		}
	}
	changed := len(changedLines) > 0

	var addedRoots, removedRoots, unmatched []string
	dirsChanged := false
	for i, dir := range entry.Dirs {
		if listingNames.Digest(dir) != entry.DirDigests[i] {
			dirsChanged = true
		}
	}
	if dirsChanged {
		changed = true
		var ok bool
		if addedRoots, removedRoots, unmatched, ok = t.rootChanges(); !ok {
			return nil
		}
	}
	if !slices.Equal(t.filesList, entry.FilesList) {
		changed = true
		added, removed, ok := t.filesListChanges()
		if !ok {
			return nil
		}
		addedRoots, removedRoots = append(addedRoots, added...), append(removedRoots, removed...)
	}
	if !changed {
		t.fail("nothing changed")
		return nil
	}
	roots := setOf(p.Roots)
	for f := range deleted {
		if roots[f] && !slices.Contains(removedRoots, f) {
			t.fail("a deleted root is not one the include globs or files list dropped")
			return nil
		}
	}

	for _, f := range slices.Sorted(maps.Keys(deleted)) {
		t.dropFile(f)
	}
	p.Roots = slices.DeleteFunc(p.Roots, func(r string) bool { return slices.Contains(removedRoots, r) })
	for _, rel := range slices.Sorted(maps.Keys(edited)) {
		if !t.editSource(rel, edited[rel]) {
			return nil
		}
	}
	for _, id := range slices.Sorted(maps.Keys(t.dirty)) {
		if !t.reresolve(id) {
			return nil
		}
	}
	for _, r := range addedRoots {
		p.Roots = append(p.Roots, r)
		t.enter(r)
	}
	for len(t.entering) > 0 {
		f := t.entering[0]
		t.entering = t.entering[1:]
		if !t.resolveEntered(f) {
			return nil
		}
	}
	if t.refusal != "" {
		return nil
	}

	reached := reachable(p)
	for f := range reached {
		if !t.files[f] {
			t.fail("an edge reaches a file the listing does not hold")
			return nil
		}
	}
	for f := range t.files {
		if !reached[f] {
			t.dropFile(f)
		}
	}
	p.Files = slices.DeleteFunc(p.Files, func(f string) bool { return !t.files[f] })
	p.candidates = p.candidates[:0]
	for _, id := range slices.Sorted(maps.Keys(t.blocks)) {
		p.candidates = append(p.candidates, t.blocks[id]...)
	}
	t.commit(dirsChanged, unmatched)
	return p
}

// index groups the candidates by block, and blocks and edges by importer and specifier.
func (t *patcher) index() {
	t.blocks = map[int][]resolutionCandidate{}
	t.blocksOf = map[importKey][]int{}
	for _, c := range t.p.candidates {
		if _, seen := t.blocks[c.block]; !seen {
			k := importKey{c.from, c.specifier}
			t.blocksOf[k] = append(t.blocksOf[k], c.block)
		}
		t.blocks[c.block] = append(t.blocks[c.block], c)
		t.nextBlock = max(t.nextBlock, c.block+1)
	}
	t.reindexEdges()
}

func (t *patcher) reindexEdges() {
	t.edgesOf = map[importKey][]int{}
	for i, e := range t.p.Edges {
		if e.Kind.ModuleSpecifier() {
			k := importKey{e.From, e.Specifier}
			t.edgesOf[k] = append(t.edgesOf[k], i)
		}
	}
}

func (t *patcher) removeEdges(drop func(i int, e explainfiles.Edge) bool) {
	kept := t.p.Edges[:0]
	for i, e := range t.p.Edges {
		if !drop(i, e) {
			kept = append(kept, e)
		}
	}
	t.p.Edges = kept
	t.reindexEdges()
}

// dropFile takes a file out of the program with every resolution and edge from it.
func (t *patcher) dropFile(f string) {
	t.dropped = append(t.dropped, t.abs(f))
	delete(t.files, f)
	delete(t.sources, f)
	for k, ids := range t.blocksOf {
		if k.from == f {
			for _, id := range ids {
				delete(t.blocks, id)
				delete(t.dirty, id)
			}
			delete(t.blocksOf, k)
		}
	}
	t.removeEdges(func(_ int, e explainfiles.Edge) bool { return e.From == f })
}

func specCounts(specs []string) map[string]int {
	counts := map[string]int{}
	for _, s := range specs {
		counts[s]++
	}
	return counts
}

// editSource drops the resolutions of imports a source lost and adds those it gained.
func (t *patcher) editSource(rel string, now cachedSource) bool {
	old := t.sources[rel]
	before, after := specCounts(old.Specs), specCounts(now.Specs)
	texts := map[string][]string{}
	for spec := range after {
		mode, text, _ := strings.Cut(spec, ":")
		texts[text] = append(texts[text], mode)
	}
	for spec := range before {
		mode, text, _ := strings.Cut(spec, ":")
		if !slices.Contains(texts[text], mode) {
			texts[text] = append(texts[text], mode)
		}
	}
	for _, text := range slices.Sorted(maps.Keys(texts)) {
		if len(texts[text]) > 1 {
			return t.fail("an import text in two modes changed")
		}
		spec := texts[text][0] + ":" + text
		k := importKey{rel, text}
		for n := before[spec]; n > after[spec]; n-- {
			if ids := t.blocksOf[k]; len(ids) > 0 {
				id := ids[len(ids)-1]
				t.blocksOf[k] = ids[:len(ids)-1]
				delete(t.blocks, id)
				delete(t.dirty, id)
			}
			if edges := t.edgesOf[k]; len(edges) > 0 {
				last := edges[len(edges)-1]
				t.removeEdges(func(i int, _ explainfiles.Edge) bool { return i == last })
			}
		}
	}
	t.sources[rel] = now
	for _, text := range slices.Sorted(maps.Keys(texts)) {
		spec := texts[text][0] + ":" + text
		for n := before[spec]; n < after[spec]; n++ {
			if !t.resolveNew(rel, text, false) {
				return false
			}
		}
	}
	return true
}

func writtenCounts(specs []string) map[string]int {
	counts := map[string]int{}
	for _, spec := range specs {
		_, text, _ := strings.Cut(spec, ":")
		counts[text]++
	}
	return counts
}

func (t *patcher) resolutions(f string) (map[string]int, bool) {
	src, ok := scanFile(t.abs(f))
	return src.Resolutions, ok && src.Counted
}

// resolvedOnce is whether the patch can follow a changed source: each import it
// wrote and writes is resolved once, or the change keeps the imports and their counts.
func (t *patcher) resolvedOnce(rel string, old, now cachedSource) bool {
	counts, counted := t.resolutions(rel)
	if !counted {
		return t.fail("a changed source has an import whose resolutions are not counted")
	}
	listed := func(text string) int { return len(t.blocksOf[importKey{rel, text}]) }
	onceBefore := func() bool {
		for text, n := range writtenCounts(old.Specs) {
			if listed(text) > n {
				return false
			}
		}
		return true
	}
	countsKept := func() bool {
		for text, n := range counts {
			if listed(text) != n {
				return false
			}
		}
		return true
	}
	if maps.Equal(counts, writtenCounts(now.Specs)) && onceBefore() {
		return true
	}
	if slices.Equal(old.Specs, now.Specs) && countsKept() {
		return true
	}
	return t.fail("a changed source resolves an import more than once")
}

// enter adds a file the program now holds; its imports resolve once every edit is applied.
func (t *patcher) enter(f string) {
	if t.files[f] {
		return
	}
	t.files[f] = true
	t.p.Files = append(t.p.Files, f)
	t.entering = append(t.entering, f)
	t.newReads = append(t.newReads, t.abs(f))
}

func (t *patcher) resolveEntered(f string) bool {
	if !firstParty(f) || !patchableSource(f) {
		return t.fail("a file entering the program is not a first-party source")
	}
	src, ok := scanSpecs(t.abs(f))
	if !ok {
		return t.fail("a file entering the program is unscannable")
	}
	if !slices.ContainsFunc([][2]bool{{false, false}, {false, true}, {true, false}, {true, true}}, func(c [2]bool) bool {
		return src.Rest == incremental.PlainRest(c[0], c[1])
	}) {
		return t.fail("a file entering the program has references, augmentations or pragmas")
	}
	if src.ResolutionMode {
		return t.fail("a file entering the program names a resolution-mode")
	}
	switch counts, counted := t.resolutions(f); {
	case !counted:
		return t.fail("a file entering the program has an import whose resolutions are not counted")
	case !maps.Equal(counts, writtenCounts(src.Specs)):
		return t.fail("a file entering the program resolves an import more than once")
	}
	implicit, ok := t.implicitFor(f, src.Rest)
	if !ok {
		return false
	}
	t.sources[f] = src
	for _, text := range slices.Sorted(maps.Keys(implicit)) {
		for range implicit[text] {
			if !t.resolveNew(f, text, true) {
				return false
			}
		}
	}
	for _, spec := range src.Specs {
		_, text, _ := strings.Cut(spec, ":")
		if !t.resolveNew(f, text, false) {
			return false
		}
	}
	return true
}

// implicitFor is what tsgo resolves for a file beyond what it writes (a JSX
// runtime), as every listed source with the same extension and facts has it.
func (t *patcher) implicitFor(f, rest string) (map[string]int, bool) {
	if t.implicit == nil {
		t.implicit = map[string]map[string]int{}
		resolved := map[string]map[string]int{}
		for k, ids := range t.blocksOf {
			if resolved[k.from] == nil {
				resolved[k.from] = map[string]int{}
			}
			resolved[k.from][k.specifier] += len(ids)
		}
		for file, src := range t.sources {
			if !t.files[file] || slices.Contains(t.entering, file) {
				continue
			}
			extra := map[string]int{}
			written := writtenCounts(src.Specs)
			// A source resolving a written specifier twice may hold a dynamic import's repeat, so it is no sample.
			repeats := false
			for text, n := range resolved[file] {
				switch {
				case n > 0 && written[text] == 0:
					extra[text] = n
				case n > written[text]:
					repeats = true
				}
			}
			if repeats {
				continue
			}
			class := path.Ext(file) + " " + src.Rest
			if prior, ok := t.implicit[class]; ok && !maps.Equal(prior, extra) {
				t.implicit[class] = nil
				continue
			}
			if _, ok := t.implicit[class]; !ok {
				t.implicit[class] = extra
			}
		}
	}
	extra, ok := t.implicit[path.Ext(f)+" "+rest]
	if !ok {
		return nil, t.fail("no listed source has the facts of a file entering the program")
	}
	if extra == nil {
		return nil, t.fail("listed sources with the same facts resolve different implicit imports")
	}
	return extra, true
}

// reresolve resolves again a specifier one of whose probes now finds something else.
func (t *patcher) reresolve(id int) bool {
	cands, ok := t.blocks[id]
	if !ok {
		return true
	}
	c := cands[0]
	if c.kind == explainfiles.TypeReference {
		return t.fail("a probe of a type reference changed")
	}
	k := importKey{c.from, c.specifier}
	if old := cands[len(cands)-1].resolved; old != "" {
		for _, i := range t.edgesOf[k] {
			if t.p.Edges[i].To == old {
				t.removeEdges(func(j int, _ explainfiles.Edge) bool { return j == i })
				break
			}
		}
	}
	delete(t.blocks, id)
	t.blocksOf[k] = slices.DeleteFunc(t.blocksOf[k], func(b int) bool { return b == id })
	if relativeSpecifier(c.specifier) {
		return t.resolveRelative(c.from, c.specifier)
	}
	if done, why := t.resolveAlias(c.from, c.specifier); !done {
		return t.fail("a probe of a bare specifier changed and " + cmp.Or(why, "no paths pattern maps it"))
	}
	return true
}

func (t *patcher) addBlock(from, text string, cands []resolutionCandidate, edge *explainfiles.Edge) {
	id := t.nextBlock
	t.nextBlock++
	if len(cands) > 0 {
		for i := range cands {
			cands[i].from, cands[i].block = from, id
			for _, probed := range []string{cands[i].path, cands[i].resolved, cands[i].skipped} {
				if probed != "" {
					t.newReads = append(t.newReads, t.abs(probed))
				}
			}
		}
		t.blocks[id] = cands
		k := importKey{from, text}
		t.blocksOf[k] = append(t.blocksOf[k], id)
	}
	if edge != nil {
		t.p.Edges = append(t.p.Edges, *edge)
		k := importKey{edge.From, edge.Specifier}
		t.edgesOf[k] = append(t.edgesOf[k], len(t.p.Edges)-1)
		t.enter(edge.To)
	}
}

func (t *patcher) resolveNew(from, text string, implicit bool) bool {
	if relativeSpecifier(text) {
		return t.resolveRelative(from, text)
	}
	why := ""
	if !implicit {
		var done bool
		if done, why = t.resolveAlias(from, text); done {
			return true
		}
	}
	return t.reuseBare(from, text, implicit, why)
}

// resolveRelative resolves a relative specifier as tsgo's bundler resolution
// does, its trace written out and read back by the listing's own parser.
func (t *patcher) resolveRelative(from, text string) bool {
	facts, cfg := t.entry.Resolution, t.entry.Config
	switch {
	case len(facts.Kinds) != 1 || !strings.HasSuffix(facts.Kinds[0], "'Bundler'."):
		return t.fail("the program does not resolve with bundler alone")
	case facts.RootDirs || cfg.ModuleSuffixes:
		return t.fail("rootDirs or moduleSuffixes redirect relative specifiers")
	case len(facts.RelativeTargets) != 1:
		return t.fail("no single relative resolution to follow")
	case strings.HasPrefix(text, "/"):
		return t.fail("a rooted specifier")
	}
	switch path.Ext(text) {
	case ".json", ".ts", ".tsx", ".mts", ".cts":
		return t.fail("a relative specifier with a JSON or TypeScript extension")
	}
	r := relativeResolver{t: t, targets: facts.RelativeTargets[0]}
	trace, resolved, ok := r.resolve(from, text)
	if !ok {
		return false
	}
	cands, edge, why := t.traced(from, text, trace, resolved)
	if why != "" {
		return t.fail(why)
	}
	t.addBlock(from, text, cands, edge)
	return true
}

// traced is the candidates a written trace records and the edge to what it resolved.
func (t *patcher) traced(from, text, trace, resolved string) ([]resolutionCandidate, *explainfiles.Edge, string) {
	_, cands, err := splitResolutionTrace(t.root, trace)
	if err != nil {
		return nil, nil, "a written resolution trace did not parse: " + err.Error()
	}
	if resolved == "" {
		return cands, nil, ""
	}
	switch {
	case !firstParty(resolved):
		return nil, nil, "a specifier resolved outside the first-party tree"
	case rootStem(resolved) == "":
		return nil, nil, "a specifier resolved to a file that is not TypeScript"
	}
	return cands, &explainfiles.Edge{Kind: explainfiles.Import, From: from, To: resolved, Specifier: text}, ""
}

// resolveAlias is tsgo's tryLoadModuleUsingPaths when one pattern with one
// extensionless substitution finds a file; why is "" when no pattern matches.
func (t *patcher) resolveAlias(from, text string) (done bool, why string) {
	facts, cfg := t.entry.Resolution, t.entry.Config
	if len(cfg.Paths) == 0 {
		return false, ""
	}
	var matched []tsconfig.Path
	for _, p := range cfg.Paths {
		if p.Key == text && !strings.Contains(p.Key, "*") {
			return false, "an exact paths key names the specifier"
		}
		prefix, suffix, star := strings.Cut(p.Key, "*")
		if star && !strings.Contains(suffix, "*") && len(text) >= len(prefix)+len(suffix) &&
			strings.HasPrefix(text, prefix) && strings.HasSuffix(text, suffix) {
			matched = append(matched, p)
		}
	}
	switch {
	case len(matched) == 0:
		return false, ""
	case len(matched) > 1:
		return false, "more than one paths pattern matches the specifier"
	case len(matched[0].Values) != 1:
		return false, "the paths pattern has a fallback substitution"
	case len(facts.Kinds) != 1 || !strings.HasSuffix(facts.Kinds[0], "'Bundler'."):
		return false, "the program does not resolve with bundler alone"
	case cfg.ModuleSuffixes:
		return false, "moduleSuffixes redirect a substituted candidate"
	case len(facts.SubstitutionTargets) != 1:
		return false, "no single substituted resolution to follow"
	}
	pattern, substitution := matched[0].Key, matched[0].Values[0]
	prefix, suffix, _ := strings.Cut(pattern, "*")
	substituted := strings.Replace(substitution, "*", text[len(prefix):len(text)-len(suffix)], 1)
	tail := strings.TrimPrefix(substituted, "./")
	switch {
	case strings.Contains(substitution, "${") || strings.Contains(substituted, "\\") || path.IsAbs(substituted):
		return false, "a paths substitution is rooted or names ${configDir}"
	case tail == "" || path.Clean(tail) != tail || tail == ".." || strings.HasPrefix(tail, "../"):
		return false, "a substituted candidate is not a plain path below the paths directory"
	}
	if slices.ContainsFunc(moduleExtensions, func(ext string) bool { return strings.HasSuffix(tail, ext) }) {
		return false, "a substituted candidate has a JavaScript, JSON or TypeScript extension"
	}
	base, ok := t.rel(cfg.PathsDir)
	if !ok {
		return false, "the paths directory is outside the repository"
	}
	if base == "." {
		base = ""
	}
	if !slices.Contains(facts.Substitutions, pattern+"\x00"+substitution+"\x00"+base) {
		return false, "no traced resolution substituted the paths pattern as its config says: " + pattern
	}
	candidate := filepath.Join(cfg.PathsDir, filepath.FromSlash(tail))
	r := relativeResolver{t: t, targets: facts.SubstitutionTargets[0]}
	r.line("======== Resolving module '%s' from '%s'. ========", text, t.abs(from))
	r.line("'paths' option is specified, looking for a pattern to match module name '%s'.", text)
	r.line("Module name '%s', matched pattern '%s'.", text, pattern)
	r.line("Trying substitution '%s', candidate module location: '%s'.", substitution, substituted)
	trace, resolved, why := r.load(text, candidate, false)
	switch {
	case why != "":
		return false, why
	case resolved == "":
		return false, "a substituted candidate finds nothing, so tsgo goes on to other lookups"
	}
	cands, edge, why := t.traced(from, text, trace, resolved)
	if why != "" {
		return false, why
	}
	t.addBlock(from, text, cands, edge)
	return true, ""
}

// moduleExtensions are the endings tsgo's RemoveFileExtension strips, longest first.
var moduleExtensions = []string{".d.mts", ".d.cts", ".d.ts", ".mjs", ".cjs", ".mts", ".cts", ".json", ".tsx", ".ts", ".jsx", ".js"}

type relativeResolver struct {
	t       *patcher
	targets string
	trace   strings.Builder
}

func (r *relativeResolver) line(format string, a ...any) {
	fmt.Fprintf(&r.trace, format+"\n", a...)
}

func (r *relativeResolver) isFile(abs string) bool {
	info, err := statSourceInput(abs)
	return err == nil && !info.IsDir()
}

func (r *relativeResolver) isDir(abs string) bool {
	info, err := statSourceInput(abs)
	return err == nil && info.IsDir()
}

// resolve follows tsgo's nodeLoadModuleByRelativeName outside ESM mode, the only
// mode bundler resolution has; it refuses a directory holding a package.json.
func (r *relativeResolver) resolve(from, text string) (string, string, bool) {
	t := r.t
	fromAbs := t.abs(from)
	trailing := text == "." || text == ".." || strings.HasSuffix(text, "/") || strings.HasSuffix(text, "/.") || strings.HasSuffix(text, "/..")
	candidate := filepath.Join(filepath.Dir(fromAbs), filepath.FromSlash(text))
	if _, ok := t.rel(candidate); !ok {
		return "", "", t.fail("a relative specifier leaves the repository")
	}
	r.line("======== Resolving module '%s' from '%s'. ========", text, fromAbs)
	trace, resolved, why := r.load(text, candidate, trailing)
	if why != "" {
		return "", "", t.fail(why)
	}
	return trace, resolved, true
}

// load is tsgo's nodeLoadModuleByRelativeName for one candidate: the trace so
// far with its lines, what it resolved, and what it does not model.
func (r *relativeResolver) load(text, candidate string, trailing bool) (string, string, string) {
	t := r.t
	shown := candidate
	if trailing {
		shown += "/"
	}
	r.line("Loading module as file / folder, candidate module location '%s', target file types: %s.", shown, r.targets)
	extensions := map[string]bool{}
	for name := range strings.SplitSeq(r.targets, ", ") {
		extensions[name] = true
	}
	resolved := ""
	finish := func() (string, string, string) {
		if resolved == "" {
			r.line("======== Module name '%s' was not resolved. ========", text)
			return r.trace.String(), "", ""
		}
		r.line("======== Module name '%s' was successfully resolved to '%s'. ========", text, resolved)
		rel, _ := t.rel(resolved)
		return r.trace.String(), rel, ""
	}
	if !trailing {
		if parent := filepath.Dir(candidate); !r.isDir(parent) {
			r.line("Directory '%s' does not exist, skipping all lookups in it.", parent)
			return finish()
		}
		resolved = r.fromFile(extensions, candidate)
		if resolved != "" {
			return finish()
		}
	}
	if !r.isDir(candidate) {
		r.line("Directory '%s' does not exist, skipping all lookups in it.", candidate)
		return finish()
	}
	manifest := filepath.Join(candidate, "package.json")
	if r.isFile(manifest) {
		return "", "", "a specifier names a directory with a package.json"
	}
	r.line("File '%s' does not exist.", manifest)
	resolved = r.fromFile(extensions, filepath.Join(candidate, "index"))
	return finish()
}

func (r *relativeResolver) fromFile(extensions map[string]bool, candidate string) string {
	if base := filepath.Base(candidate); strings.Contains(base, ".") {
		extensionless := candidate
		for _, ext := range moduleExtensions {
			if before, ok := strings.CutSuffix(candidate, ext); ok {
				extensionless = before
				break
			}
		}
		if extensionless == candidate {
			extensionless = candidate[:strings.LastIndex(candidate, ".")]
		}
		extension := candidate[len(extensionless):]
		r.line("File name '%s' has a '%s' extension - stripping it.", candidate, extension)
		if found := r.addingExtensions(extensions, extensionless, extension); found != "" {
			return found
		}
	}
	return r.addingExtensions(extensions, candidate, "")
}

func (r *relativeResolver) addingExtensions(extensions map[string]bool, extensionless, original string) string {
	if dir := filepath.Dir(extensionless); !r.isDir(dir) {
		return ""
	}
	var tries [][2]string // target file type, extension
	switch original {
	case ".mjs", ".mts", ".d.mts":
		tries = [][2]string{{"TypeScript", ".mts"}, {"Declaration", ".d.mts"}, {"JavaScript", ".mjs"}}
	case ".cjs", ".cts", ".d.cts":
		tries = [][2]string{{"TypeScript", ".cts"}, {"Declaration", ".d.cts"}, {"JavaScript", ".cjs"}}
	case ".json":
		tries = [][2]string{{"Declaration", ".d.json.ts"}, {"JSON", ".json"}}
	case ".tsx", ".jsx":
		tries = [][2]string{{"TypeScript", ".tsx"}, {"TypeScript", ".ts"}, {"Declaration", ".d.ts"}, {"JavaScript", ".jsx"}, {"JavaScript", ".js"}}
	case ".ts", ".d.ts", ".js", "":
		tries = [][2]string{{"TypeScript", ".ts"}, {"TypeScript", ".tsx"}, {"Declaration", ".d.ts"}, {"JavaScript", ".js"}, {"JavaScript", ".jsx"}}
	default:
		if !strings.HasSuffix(extensionless+original, ".d.ts") {
			tries = [][2]string{{"Declaration", ".d" + original + ".ts"}}
		}
	}
	for _, try := range tries {
		if !extensions[try[0]] {
			continue
		}
		file := extensionless + try[1]
		if r.isFile(file) {
			r.line("File '%s' exists - use it as a name resolution result.", file)
			return file
		}
		r.line("File '%s' does not exist.", file)
	}
	return ""
}

// scope is the nearest directory at or above dir holding name, "" for none
// in the repository; every stat is recorded as a read of the listing.
func (t *patcher) nearest(memo map[string]string, dir, name string, want func(fs.FileInfo) bool) string {
	if found, ok := memo[dir]; ok {
		return found
	}
	probe := t.abs(path.Join(dir, name))
	t.newReads = append(t.newReads, probe)
	found := ""
	if info, err := statInput(probe); err == nil && want(info) {
		found = dir
	} else if parent, ok := upDir(dir); ok {
		found = t.nearest(memo, parent, name, want)
	} else {
		found = "\x00"
	}
	memo[dir] = found
	return found
}

// bareScope is what decides a bare specifier's resolution besides its text: the
// importer's module format, its package scope and its nearest node_modules.
func (t *patcher) bareScope(f string) string {
	scope := t.nearest(t.scopes, parentDir(f), "package.json", func(i fs.FileInfo) bool { return !i.IsDir() })
	modules := t.nearest(t.nms, parentDir(f), "node_modules", func(i fs.FileInfo) bool { return i.IsDir() })
	return extensionClass(f) + "\x00" + scope + "\x00" + modules
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

// scopeWalk is the package.json files tsgo looks for from f's directory up to
// its package scope, as the listing's candidates record them.
func (t *patcher) scopeWalk(f string) []string {
	scope := t.scopes[parentDir(f)]
	var walk []string
	for dir := parentDir(f); ; {
		walk = append(walk, path.Join(dir, "package.json"))
		parent, ok := upDir(dir)
		if dir == scope || !ok {
			return walk
		}
		dir = parent
	}
}

// upDir is dir's parent, and false at the top: "", "/", "." or a dirname that
// does not shrink, so a walk up from a rooted or dotted path ends.
func upDir(dir string) (string, bool) {
	if dir == "" || dir == "/" || dir == "." {
		return "", false
	}
	parent := parentDir(dir)
	return parent, len(parent) < len(dir)
}

// reuseBare copies another importer's resolution of a bare specifier: with the same
// format, package scope and node_modules, only its package.json walk differs.
func (t *patcher) reuseBare(from, text string, implicit bool, aliasRefusal string) bool {
	if t.specOwners == nil {
		t.specOwners = map[string][]string{}
		for file, src := range t.sources {
			for _, spec := range src.Specs {
				_, txt, _ := strings.Cut(spec, ":")
				if !slices.Contains(t.specOwners[txt], file) {
					t.specOwners[txt] = append(t.specOwners[txt], file)
				}
			}
		}
		for k := range t.blocksOf {
			if !slices.Contains(t.specOwners[k.specifier], k.from) {
				t.specOwners[k.specifier] = append(t.specOwners[k.specifier], k.from)
			}
		}
		for txt := range t.specOwners {
			slices.Sort(t.specOwners[txt])
		}
	}
	mode := t.mode(from, text, implicit)
	if mode == "" {
		return t.fail("a bare specifier is written in two modes")
	}
	want := t.bareScope(from)
	for _, owner := range t.specOwners[text] {
		if owner == from || !t.files[owner] || slices.Contains(t.entering, owner) || t.sources[owner].ResolutionMode || t.mode(owner, text, implicit) != mode ||
			mode == "implicit" && len(t.blocksOf[importKey{owner, text}]) == 0 || t.bareScope(owner) != want {
			continue
		}
		return t.copyResolution(owner, from, text)
	}
	if aliasRefusal != "" {
		return t.fail("a bare specifier no importer in the same scope resolves: " + text + " (" + aliasRefusal + ")")
	}
	return t.fail("a bare specifier no importer in the same scope resolves: " + text)
}

// mode is how f imports text: "import" or "require" as written, "implicit"
// for a specifier it does not write, "" when it writes both.
func (t *patcher) mode(f, text string, implicit bool) string {
	modes := map[string]bool{}
	for _, spec := range t.sources[f].Specs {
		if m, txt, _ := strings.Cut(spec, ":"); txt == text {
			modes[m] = true
		}
	}
	switch {
	case implicit && len(modes) == 0:
		return "implicit"
	case implicit || len(modes) > 1:
		return ""
	case len(modes) == 0:
		return "implicit"
	}
	for m := range modes {
		return m
	}
	return ""
}

func (t *patcher) copyResolution(owner, from, text string) bool {
	k := importKey{owner, text}
	var template []resolutionCandidate
	for i, id := range t.blocksOf[k] {
		if t.dirty[id] {
			return t.fail("the resolution to reuse has a changed probe")
		}
		if i == 0 {
			template = t.blocks[id]
			continue
		}
		if !slices.EqualFunc(template, t.blocks[id], func(a, b resolutionCandidate) bool {
			a.block, b.block = 0, 0
			return a == b
		}) {
			return t.fail("an importer resolves one specifier two ways")
		}
	}
	var edge *explainfiles.Edge
	for _, i := range t.edgesOf[k] {
		e := t.p.Edges[i]
		if edge != nil && (e.To != edge.To || e.Kind != edge.Kind) {
			return t.fail("an importer resolves one specifier two ways")
		}
		edge = &e
	}
	resolved := ""
	if len(template) > 0 {
		resolved = template[len(template)-1].resolved
	}
	if edge != nil && (edge.To != resolved || !t.files[edge.To]) {
		return t.fail("the resolution to reuse is not a listed edge")
	}
	cands := slices.Clone(template)
	if parentDir(owner) != parentDir(from) {
		var ok bool
		if cands, ok = t.moveScopeWalk(cands, owner, from); !ok {
			return false
		}
	}
	if edge != nil {
		moved := *edge
		moved.From = from
		edge = &moved
	}
	t.addBlock(from, text, cands, edge)
	return true
}

func (t *patcher) moveScopeWalk(cands []resolutionCandidate, owner, from string) ([]resolutionCandidate, bool) {
	ownerWalk, fromWalk := t.scopeWalk(owner), t.scopeWalk(from)
	var out []resolutionCandidate
	for i := 0; i < len(cands); {
		run := 0
		for run < len(ownerWalk) && i+run < len(cands) && cands[i+run].metadata && cands[i+run].path == ownerWalk[run] {
			run++
		}
		if run == len(ownerWalk) {
			for _, p := range fromWalk {
				if firstParty(p) {
					out = append(out, resolutionCandidate{specifier: cands[i].specifier, path: p, metadata: true, kind: cands[i].kind, resolved: cands[i].resolved})
				}
			}
			i += run
			continue
		}
		out = append(out, cands[i])
		i++
	}
	own := map[string]bool{}
	for _, p := range ownerWalk {
		own[parentDir(p)] = true
	}
	for _, p := range fromWalk {
		delete(own, parentDir(p))
	}
	for _, c := range out {
		if c.path != c.resolved && (own[parentDir(c.path)] || c.skipped != "" && own[c.skipped]) {
			return nil, t.fail("a reused resolution probes the other importer's directories")
		}
	}
	return out, true
}

// commit writes the patch's reads and sources back, remember its listing; a read
// it no longer needs stays in the read set, which can only list the program again sooner.
func (t *patcher) commit(dirsChanged bool, unmatched []string) {
	entry, p := t.entry, t.p
	dropped := map[string]bool{}
	for _, f := range t.dropped {
		dropped[f] = true
	}
	for _, f := range p.Files {
		delete(dropped, t.abs(f))
	}
	reads := slices.DeleteFunc(slices.Clone(entry.Reads), func(r string) bool { return dropped[r] })
	reads = append(reads, t.newReads...)
	slices.Sort(reads)
	entry.Reads = slices.Compact(reads)
	entry.ReadLines = make([]string, len(entry.Reads))
	for i, read := range entry.Reads {
		entry.ReadLines[i] = listingStats.Line(read)
	}
	for i, dir := range entry.Dirs {
		entry.DirDigests[i] = listingNames.Digest(dir)
	}
	entry.Sources = map[string]cachedSource{}
	for f, src := range t.sources {
		if t.files[f] {
			entry.Sources[f] = src
		}
	}
	if dirsChanged {
		entry.Unmatched = unmatched
	}
	entry.FilesList = t.filesList
	entry.StaleDiagnostics = entry.StaleDiagnostics || t.jsonEdited
	entry.Fingerprint = listingFingerprint(entry.Reads, entry.Dirs)
}

// rootChanges is the roots the include globs gained and lost; the matcher must
// agree with the listing on every file already there, or tsgo lists it again.
func (t *patcher) rootChanges() (added, removed, unmatched []string, ok bool) {
	cfg := t.entry.Config
	if cfg.Refused != "" {
		return nil, nil, nil, t.fail("an include root changed and " + cfg.Refused)
	}
	now, symlink := includedTypeScript(t.root, t.entry.Dirs)
	if symlink {
		return nil, nil, nil, t.fail("an include root now holds a symlink")
	}
	roots, known := map[string]bool{}, map[string]bool{}
	for _, f := range t.entry.Unmatched {
		known[f] = true
	}
	for _, r := range t.p.Roots {
		roots[r] = true
		if t.underDirs(r) {
			known[r] = true
		}
	}
	stems := map[string]int{}
	for f := range now {
		stems[rootStem(f)]++
	}
	for f := range known {
		if !now[f] {
			stems[rootStem(f)]++
		}
	}
	m := newRootMatcher(cfg)
	for _, f := range slices.Sorted(maps.Keys(now)) {
		matched := m.matches(t.abs(f))
		switch {
		case known[f] && stems[rootStem(f)] == 1 && matched != roots[f]:
			return nil, nil, nil, t.fail("the include matcher disagrees with the listing on " + f)
		case known[f]:
			if !roots[f] {
				unmatched = append(unmatched, f)
			}
		case stems[rootStem(f)] > 1:
			return nil, nil, nil, t.fail("an added file shares its name with another TypeScript file")
		case matched:
			added = append(added, f)
		default:
			unmatched = append(unmatched, f)
		}
	}
	for _, f := range slices.Sorted(maps.Keys(known)) {
		if now[f] {
			continue
		}
		if stems[rootStem(f)] > 1 {
			return nil, nil, nil, t.fail("a deleted file shared its name with another TypeScript file")
		}
		if roots[f] {
			removed = append(removed, f)
		}
	}
	return added, removed, unmatched, true
}

// filesListChanges is the roots a files-list projection gained and lost; tsgo
// roots a files list's files and nothing else, so the listing's roots must be its old list.
func (t *patcher) filesListChanges() (added, removed []string, ok bool) {
	was, now, roots := setOf(t.entry.FilesList), setOf(t.filesList), setOf(t.p.Roots)
	// tsgo roots only the TypeScript files of a list; the rest, a script without
	// allowJs, stays out of the program with a diagnostic of its own.
	rooted := maps.Clone(was)
	maps.DeleteFunc(rooted, func(f string, _ bool) bool { return rootStem(f) == "" })
	switch {
	case t.entry.FilesList == nil || t.filesList == nil:
		return nil, nil, t.fail("only a files list's listing patches to another files list")
	case len(t.entry.Dirs) > 0:
		return nil, nil, t.fail("a files-list projection has include roots")
	case !maps.Equal(rooted, roots):
		var differ []string
		for f := range rooted {
			if !roots[f] {
				differ = append(differ, f+" (listed, not a root)")
			}
		}
		for f := range roots {
			if !rooted[f] {
				differ = append(differ, f+" (a root, not listed)")
			}
		}
		slices.Sort(differ)
		return nil, nil, t.fail("the listing's roots are not its files list, such as " + strings.Join(differ[:min(3, len(differ))], ", "))
	}
	for _, f := range slices.Sorted(maps.Keys(now)) {
		if was[f] {
			continue
		}
		if !patchableSource(f) || rootStem(f) == "" {
			return nil, nil, t.fail("a root the files list gained is not a TypeScript source: " + f)
		}
		if info, err := lstatSourceInput(t.abs(f)); err != nil || !info.Mode().IsRegular() {
			return nil, nil, t.fail("a root the files list gained is not a regular file")
		}
		added = append(added, f)
	}
	for _, f := range slices.Sorted(maps.Keys(was)) {
		switch {
		case now[f]:
		case !rooted[f]:
			return nil, nil, t.fail("a file the files list lost was not a root, and its diagnostic goes with it")
		default:
			removed = append(removed, f)
		}
	}
	return added, removed, true
}

func setOf(list []string) map[string]bool {
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	return set
}

func (t *patcher) underDirs(rel string) bool {
	abs := t.abs(rel)
	for _, dir := range t.entry.Dirs {
		if strings.HasPrefix(abs, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func programFromEntry(entry *cachedListing, subject string) *program {
	p := &program{Listing: entry.Listing, config: subject, resolution: entry.Resolution}
	p.Files = slices.Clone(p.Files)
	p.Roots = slices.Clone(p.Roots)
	p.Edges = slices.Clone(p.Edges)
	p.candidates = slices.Grow(p.candidates, len(entry.Candidates))
	for _, c := range entry.Candidates {
		p.candidates = append(p.candidates, c.resolutionCandidate())
	}
	return p
}

func (c cachedCandidate) resolutionCandidate() resolutionCandidate {
	return resolutionCandidate{
		from: c.From, specifier: c.Specifier, path: c.Path, file: c.File, metadata: c.Metadata,
		kind: c.Kind, block: c.Block, resolved: c.Resolved, skipped: c.Skipped,
	}
}

func (c resolutionCandidate) cached() cachedCandidate {
	return cachedCandidate{
		From: c.from, Specifier: c.specifier, Path: c.path, File: c.file, Metadata: c.metadata,
		Kind: c.kind, Block: c.block, Resolved: c.resolved, Skipped: c.skipped,
	}
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

// listingDifference is how two listings of one program differ, ignoring the
// order of files and edges, the numbering of resolutions and resolution facts
// got keeps that want lacks; "" when they do not.
func listingDifference(got, want *program) string {
	canon := func(p *program) map[string][]string {
		out := map[string][]string{
			"files": slices.Clone(p.Files), "roots": slices.Clone(p.Roots), "diagnostics": slices.Clone(p.Diagnostics),
		}
		edge := func(e explainfiles.Edge) string {
			return fmt.Sprintf("%d %s -> %s %q", e.Kind, e.From, e.To, e.Specifier)
		}
		for _, e := range p.Edges {
			out["edges"] = append(out["edges"], edge(e))
		}
		for _, te := range p.Types {
			out["types"] = append(out["types"], te.Entry+" "+te.File)
		}
		for _, te := range p.Implicit {
			out["implicit types"] = append(out["implicit types"], te.Entry+" "+te.File)
		}
		for _, u := range p.Unresolved {
			out["unresolved"] = append(out["unresolved"], fmt.Sprintf("%d %s %q probes=%q", u.Kind, u.From, u.Specifier, u.Candidates))
		}
		if p.refused != "" {
			out["refusal"] = []string{p.refused}
		}
		blocks := map[int][]string{}
		var order []int
		for _, c := range p.candidates {
			if _, ok := blocks[c.block]; !ok {
				order = append(order, c.block)
			}
			blocks[c.block] = append(blocks[c.block], fmt.Sprintf("%s %q %d %s file=%t meta=%t -> %s skipped=%s",
				c.from, c.specifier, c.kind, c.path, c.file, c.metadata, c.resolved, c.skipped))
		}
		for _, id := range order {
			out["resolutions"] = append(out["resolutions"], strings.Join(blocks[id], "\n  "))
		}
		for k := range out {
			slices.Sort(out[k])
		}
		return out
	}
	a, b := canon(got), canon(want)
	var diff []string
	report := func(k string, extra, missing []string) {
		diff = append(diff, fmt.Sprintf("%s: %d not in tsgo's, %d missing\n  + %s\n  - %s", k, len(extra), len(missing),
			strings.Join(extra[:min(5, len(extra))], "\n  + "), strings.Join(missing[:min(5, len(missing))], "\n  - ")))
	}
	keys := maps.Clone(a)
	maps.Copy(keys, b)
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if !slices.Equal(a[k], b[k]) {
			report(k, multisetMinus(a[k], b[k]), multisetMinus(b[k], a[k]))
		}
	}
	if missing := multisetMinus(resolutionFactList(want.resolution), resolutionFactList(got.resolution)); len(missing) > 0 {
		report("resolution facts", nil, missing)
	}
	return strings.Join(diff, "\n")
}

func resolutionFactList(f resolutionFacts) []string {
	var out []string
	for _, list := range []struct {
		name   string
		values []string
	}{
		{"kind", f.Kinds}, {"relative target", f.RelativeTargets},
		{"substitution", f.Substitutions}, {"substitution target", f.SubstitutionTargets},
	} {
		for _, v := range list.values {
			out = append(out, list.name+" "+strings.ReplaceAll(v, "\x00", " "))
		}
	}
	if f.RootDirs {
		out = append(out, "rootDirs")
	}
	return out
}

func multisetMinus(a, b []string) []string {
	counts := specCounts(b)
	var out []string
	for _, s := range a {
		if counts[s] > 0 {
			counts[s]--
			continue
		}
		out = append(out, s)
	}
	return out
}

// verifyListing is -ts_listing_patch_verify: a listing tsgo did not print for
// args must be the one it prints for the tree as it is.
func verifyListing(ctx context.Context, repoRoot, tsgo, subject string, args []string, got *program, how string) {
	fresh, err := uncachedListing(ctx, repoRoot, tsgo, subject, args)
	if err != nil {
		log.Fatalf("typescript: -ts_listing_patch_verify: %v", err)
	}
	if diff := listingDifference(got, fresh); diff != "" {
		log.Fatalf("typescript: -ts_listing_patch_verify: the %s listing of %s differs from tsgo's:\n%s", how, subject, diff)
	}
}

func writeListing(key string, entry *cachedListing) {
	writeListingFile(filepath.Join(listingCacheDir, key), entry)
}

func writeListingFile(file string, entry *cachedListing) {
	writeAtomicallyWith(file, func(w io.Writer) error { return encodeEntry(w, entry) })
}
