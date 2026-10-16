package typescript

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
)

// sourceDelta is the TypeScript sources the walked directories gained and lost
// since the record: each directory's change is exactly these entries.
type sourceDelta struct {
	gained, lost map[string]bool
	// names are each changed directory's entry names now; before are those it had
	// before the edit, beside the names of entries nothing read, as they are now.
	names, before map[string][]string
	// newDirs are the gained directories nothing reads, with their dirLines.
	newDirs map[string]string
}

func (d *sourceDelta) has(rel string) bool {
	return d != nil && (d.gained[rel] || d.lost[rel])
}

func (d *sourceDelta) empty() bool {
	return d == nil || len(d.gained)+len(d.lost) == 0
}

func (d *sourceDelta) isGained(rel string) bool { return d != nil && d.gained[rel] }

func (d *sourceDelta) isLost(rel string) bool { return d != nil && d.lost[rel] }

// repoDir is a directory relative to the repository, "" for its root.
func repoDir(repoRoot, dir string) (string, bool) {
	rel, ok := repoRelative(repoRoot, dir)
	if rel == "." {
		rel = ""
	}
	return rel, ok
}

// pkgRelative is a repository path as package pkg names it.
func pkgRelative(pkg, f string) string {
	if pkg == "" {
		return f
	}
	return strings.TrimPrefix(f, pkg+"/")
}

// shapeEntry is one entry of a dirShape and the name it is sorted by.
type shapeEntry struct{ name, line string }

// sourceChanges reads the directories whose shape moved as gained and lost
// TypeScript sources and gained directories nothing reads: a source entry no
// listing holds is gained, a listed source no longer there is lost, and the
// directory as it was must hash to the record's sum.
func sourceChanges(rec *runRecord, facts *importFacts, repoRoot string, dirs []string, moved []int, buildNames []string, skip string, stamps *newest) (*sourceDelta, *decline) {
	listed := map[string]map[string]bool{}
	for _, l := range facts.Listings {
		for _, id := range l.Listed {
			f := facts.Files[id]
			if listed[parentDir(f)] == nil {
				listed[parentDir(f)] = map[string]bool{}
			}
			listed[parentDir(f)][path.Base(f)] = true
		}
	}
	delta := &sourceDelta{gained: map[string]bool{}, lost: map[string]bool{}, names: map[string][]string{}, before: map[string][]string{}, newDirs: map[string]string{}}
	walked := setOf(rec.dirs)
	for _, i := range moved {
		rel, ok := repoDir(repoRoot, rec.dirs[i])
		if !ok {
			return nil, declined(declineInputs, "%s is outside the repository", rec.dirs[i])
		}
		present := map[string]bool{}
		var was []shapeEntry
		// An entry nothing read stands among the names a lost source's stem is checked against.
		var unread, unwalked []string
		for e, ok := range dirEntries(rel, dirs[i], &facts.names) {
			if !ok {
				return nil, declined(declineInputs, "%s holds an entry whose name the record cannot read back", orRepoRoot(rel))
			}
			present[e.name] = true
			delta.names[rel] = append(delta.names[rel], e.name)
			switch {
			case e.source && patchableSource(path.Join(rel, e.name)) && !listed[rel][e.name]:
				delta.gained[path.Join(rel, e.name)] = true
			case e.shape == "":
				unread = append(unread, e.name)
			default:
				was = append(was, shapeEntry{e.name, e.shape})
				if e.shape == "d "+e.name && !walked[filepath.Join(rec.dirs[i], e.name)] {
					unwalked = append(unwalked, e.name)
				}
			}
		}
		for name := range listed[rel] {
			if !present[name] {
				delta.lost[path.Join(rel, name)] = true
				was = append(was, shapeEntry{name, "f " + name})
			}
		}
		slices.SortFunc(was, func(a, b shapeEntry) int { return strings.Compare(a.name, b.name) })
		var added []string
		was, added = newSubdirs(was, unwalked, rec.sums[i])
		for _, name := range added {
			tree, why := outsideTree(filepath.Join(rec.dirs[i], name), path.Join(rel, name), buildNames, skip, stamps)
			if why != nil {
				return nil, why
			}
			maps.Copy(delta.newDirs, tree)
		}
		for _, e := range was {
			delta.before[rel] = append(delta.before[rel], e.name)
		}
		delta.before[rel] = append(delta.before[rel], unread...)
		if shapeSum(was) != rec.sums[i] {
			var names []string
			for f := range mapsKeys(delta.gained, delta.lost) {
				if parentDir(f) == rel {
					names = append(names, path.Base(f))
				}
			}
			slices.Sort(names)
			return nil, declined(declineInputs, "%s gained, lost or changed an entry other than a TypeScript source a listing reads (sources read as gained or lost: %s)",
				orRepoRoot(rel), strings.Join(names, ", "))
		}
	}
	return delta, nil
}

func shapeSum(entries []shapeEntry) uint64 {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.line)
		b.WriteByte('\n')
	}
	return lineSum(b.String())
}

// maxNewSubdirs bounds the subsets of unwalked subdirectories newSubdirs hashes.
const maxNewSubdirs = 8

// newSubdirs are the subdirectories the record does not walk whose entries,
// left out, make the directory hash to sum as it was, and the entries kept;
// was itself and none when no subset does.
func newSubdirs(was []shapeEntry, unwalked []string, sum uint64) ([]shapeEntry, []string) {
	if len(unwalked) == 0 || shapeSum(was) == sum {
		return was, nil
	}
	without := func(subset []string) ([]shapeEntry, bool) {
		kept := slices.DeleteFunc(slices.Clone(was), func(e shapeEntry) bool { return slices.Contains(subset, e.name) })
		return kept, shapeSum(kept) == sum
	}
	if len(unwalked) > maxNewSubdirs {
		for _, name := range unwalked {
			if kept, ok := without([]string{name}); ok {
				return kept, []string{name}
			}
		}
		return was, nil
	}
	for mask := 1; mask < 1<<len(unwalked); mask++ {
		var subset []string
		for j, name := range unwalked {
			if mask&(1<<j) != 0 {
				subset = append(subset, name)
			}
		}
		if kept, ok := without(subset); ok {
			return kept, subset
		}
	}
	return was, nil
}

// outsideTree is every directory under gained directory dir with its dirLine,
// as long as none holds a BUILD file, a link, a program source or a file the
// extension or Gazelle reads by its name: a walk of it then generates nothing.
func outsideTree(dir, rel string, buildNames []string, skip string, stamps *newest) (map[string]string, *decline) {
	tree := map[string]string{}
	for queue := []string{dir}; len(queue) > 0; queue = queue[1:] {
		d := queue[0]
		if _, seen := tree[d]; seen {
			return nil, declined(declineInputs, "%s, a gained directory, lists %s twice", rel, d)
		}
		if filepath.Base(d) == ".git" {
			return nil, declined(declineExcluded, "the walk skips .git, which %s holds", rel)
		}
		line, stamp, err := dirLine(d, buildNames, skip)
		stamps.see(stamp)
		if err != nil || line == "absent" {
			return nil, declined(declineInputs, "%s, a gained directory, does not read back", rel)
		}
		tree[d] = line
		for entry := range strings.SplitSeq(line, "\n") {
			kind, rest, _ := strings.Cut(entry, " ")
			name, _, _ := cutFields(rest, 5)
			switch {
			case entry == "":
			case kind == "d" && (rest == "." || rest == ".." || strings.ContainsRune(rest, '/')):
				return nil, declined(declineInputs, "%s, a gained directory, holds an entry whose name the record cannot read back", rel)
			case kind == "d":
				queue = append(queue, filepath.Join(d, rest))
			case kind != "f" || programCandidate(name) || readByGazelle(name):
				return nil, declined(declineInputs, "%s, a gained directory, holds %s", rel, entry)
			}
		}
	}
	return tree, nil
}

// packageEdit is one package's ts_compile whose srcs gain and lose the delta's
// sources, and the BUILD file the full pass would write for it.
type packageEdit struct {
	pkg, file    string
	dirIndex     int
	f            *rule.File
	r            *rule.Rule
	srcs         map[string]bool
	gained, lost []string
	original     []byte
	content      []byte
	mode         fs.FileMode
}

// deltaProof is what the delta's sources must meet in the patched listings, and
// the sources of the same rule and directory that stand for each one.
type deltaProof struct {
	edits []*packageEdit
	// mates are, for each gained or lost source, the other sources its rule held
	// in its directory before the edit; a gained one is resolved as they were.
	mates map[string][]string
	// owners are each delta source's package edit.
	owners map[string]*packageEdit
}

// generatedExtensions are what compiling a source writes beside it, which a
// package manifest may name.
var generatedExtensions = map[string][]string{
	".ts": {".js", ".d.ts"}, ".tsx": {".js", ".jsx", ".d.ts"},
	".mts": {".mjs", ".d.mts"}, ".cts": {".cjs", ".d.cts"},
}

// walkExcludes are the walk's exclude patterns and the directories .bazelignore names.
func walkExcludes(facts *importFacts, repoRoot, cliExcludes string) (excludes, ignored []string, why *decline) {
	excludes = slices.Clone(facts.Excludes)
	if cliExcludes != "" {
		excludes = append(excludes, strings.Split(cliExcludes, ",")...)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, ".bazelignore"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, declined(declineExcluded, ".bazelignore: %v", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			if line = strings.Trim(path.Clean(line), "/"); line != "" {
				ignored = append(ignored, line)
			}
		}
	}
	return excludes, ignored, nil
}

// excludedNewDir declines a gained directory the walk may skip, which a record
// made by the full pass would not hold as walked.
func excludedNewDir(facts *importFacts, repoRoot, cliExcludes string, newDirs map[string]string) *decline {
	if len(newDirs) == 0 {
		return nil
	}
	excludes, ignored, why := walkExcludes(facts, repoRoot, cliExcludes)
	if why != nil {
		return why
	}
	repoIgnores, why := repoIgnoredDirectories(repoRoot)
	if why != nil {
		return why
	}
	for dir := range newDirs {
		rel, _ := repoDir(repoRoot, dir)
		if slices.ContainsFunc(ignored, func(entry string) bool { return within(rel, entry) }) {
			return declined(declineExcluded, ".bazelignore names %s or a directory above it", rel)
		}
		// The walk skips a directory a pattern matches and everything under it.
		for at := rel; at != ""; at = parentDir(at) {
			for _, pattern := range excludes {
				if matched, known := excludeMatch(pattern, at); matched || !known {
					return declined(declineExcluded, "the exclude pattern %s could match %s", pattern, at)
				}
			}
			for _, pattern := range repoIgnores {
				if matched, known := excludeMatch(pattern, at); matched || !known {
					return declined(declineExcluded, "REPO.bazel's ignore_directories pattern %s could match %s", pattern, at)
				}
			}
		}
	}
	return nil
}

// repoIgnoredDirectories are the patterns of REPO.bazel's first top-level
// ignore_directories call, read as Gazelle's walk reads them (walk/config.go,
// loadRepoDirectoryIgnore); an argument other than a list of string literals declines.
func repoIgnoredDirectories(repoRoot string) ([]string, *decline) {
	data, err := os.ReadFile(filepath.Join(repoRoot, "REPO.bazel"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, declined(declineExcluded, "REPO.bazel: %v", err)
	case !bytes.Contains(data, []byte("ignore_directories")):
		return nil, nil
	}
	f, err := bzl.Parse(repoRoot, data)
	if err != nil {
		return nil, declined(declineExcluded, "REPO.bazel: %v", err)
	}
	for _, stmt := range f.Stmt {
		call, ok := stmt.(*bzl.CallExpr)
		if !ok {
			continue
		}
		if ident, ok := call.X.(*bzl.Ident); !ok || ident.Name != "ignore_directories" {
			continue
		}
		var list *bzl.ListExpr
		if len(call.List) == 1 {
			list, _ = call.List[0].(*bzl.ListExpr)
		}
		if list == nil {
			return nil, declined(declineExcluded, "REPO.bazel's ignore_directories takes no list literal")
		}
		patterns := make([]string, 0, len(list.List))
		for _, item := range list.List {
			s, ok := item.(*bzl.StringExpr)
			if !ok {
				return nil, declined(declineExcluded, "REPO.bazel's ignore_directories holds %s, no string literal", bzl.FormatString(item))
			}
			patterns = append(patterns, s.Value)
		}
		return patterns, nil
	}
	return nil, nil
}

// proveDelta places every gained and lost source in its directory's package's
// ts_compile, next to another source of that rule in the same directory, so
// that only the rule's srcs list tells the two apart.
func proveDelta(rec *runRecord, facts *importFacts, repoRoot string, buildNames []string, dirs []string, delta *sourceDelta, cliExcludes string) (*deltaProof, *decline) {
	if len(facts.Overrides) > 0 {
		return nil, declined(declineOverride, "a TypeScript resolve directive could name a source the record never saw: %s", facts.Overrides[0])
	}
	excludes, ignored, why := walkExcludes(facts, repoRoot, cliExcludes)
	if why != nil {
		return nil, why
	}
	walked := map[string]int{}
	for i, dir := range rec.dirs {
		if rel, ok := repoDir(repoRoot, dir); ok {
			walked[rel] = i
		}
	}
	listed := map[string]bool{}
	for _, l := range facts.Listings {
		for _, id := range l.Listed {
			listed[facts.Files[id]] = true
		}
	}
	generated := setOf(facts.Generated)
	programs := map[string]bool{}
	for _, p := range facts.Programs {
		programs[p.Dir] = generated[p.Dir]
	}
	proof := &deltaProof{mates: map[string][]string{}, owners: map[string]*packageEdit{}}
	edits := map[string]*packageEdit{}
	for _, f := range slices.Sorted(mapsKeys(delta.gained, delta.lost)) {
		gained := delta.gained[f]
		dir := parentDir(f)
		names := delta.names[dir]
		if !gained {
			names = delta.before[dir]
		}
		if why := sourceNameDecline(f, names); why != nil {
			return nil, why
		}
		if gained {
			if info, err := os.Lstat(filepath.Join(repoRoot, filepath.FromSlash(f))); err != nil || !info.Mode().IsRegular() {
				return nil, declined(declineNotSource, "%s is not a regular file", f)
			}
			if slices.ContainsFunc(ignored, func(entry string) bool { return within(f, entry) }) {
				return nil, declined(declineExcluded, ".bazelignore names %s or a directory above it", f)
			}
			for _, pattern := range excludes {
				if matched, known := excludeMatch(pattern, f); matched || !known {
					return nil, declined(declineExcluded, "the exclude pattern %s could match %s", pattern, f)
				}
			}
		}
		if slices.Contains(facts.Produced, f) || slices.ContainsFunc(facts.Trees, func(tree string) bool { return within(f, tree) }) {
			return nil, declined(declineGenerated, "%s is a declared output or lies in a declared output directory", f)
		}
		stem := strings.TrimSuffix(f, path.Ext(f))
		for _, ext := range generatedExtensions[path.Ext(f)] {
			if slices.ContainsFunc(facts.Outputs, func(o string) bool { return matchesExportOutput(o, stem+ext) }) {
				return nil, declined(declineEmission, "a package manifest names %s, which %s compiles to", stem+ext, f)
			}
		}
		edit, why := packageFor(dir, walked, dirs, buildNames, programs, repoRoot, edits)
		if why != nil {
			return nil, why
		}
		label := pkgRelative(edit.pkg, f)
		if lbl, ok := srcLabel(label); !ok || lbl != label {
			return nil, declined(declineNotSource, "%s is spelled otherwise as a label", f)
		}
		if edit.srcs[label] == gained {
			return nil, declined(declineOwner, "%s's srcs already say what the edit did to %s", ruleLabel(edit), f)
		}
		for src := range edit.srcs {
			mate := path.Join(edit.pkg, src)
			if parentDir(mate) == dir && mate != f && !delta.gained[mate] && listed[mate] && classify(mate) == libraryFile && rawTypeScript(mate) {
				proof.mates[f] = append(proof.mates[f], mate)
			}
		}
		// A lost source's directory keeps a source of the rule, which keeps its package scope.
		stays := slices.ContainsFunc(proof.mates[f], func(m string) bool { return !delta.lost[m] })
		for g := range delta.gained {
			stays = stays || parentDir(g) == dir
		}
		if len(proof.mates[f]) == 0 || !gained && !stays {
			return nil, declined(declineOwner, "no other source of %s in %s stands for %s", ruleLabel(edit), dir, f)
		}
		slices.Sort(proof.mates[f])
		if gained {
			edit.gained = append(edit.gained, label)
		} else {
			edit.lost = append(edit.lost, label)
		}
		proof.owners[f] = edit
	}
	for _, pkg := range slices.Sorted(mapsKeys(edits)) {
		proof.edits = append(proof.edits, edits[pkg])
	}
	return proof, nil
}

// sourceNameDecline refuses a name anything but its rule's srcs reads: a test
// or declaration, a JavaScript file or twin, a name the dev server or vitest
// looks for, or one sharing its stem with another file in its directory.
func sourceNameDecline(f string, names []string) *decline {
	base := path.Base(f)
	switch {
	case !rawTypeScript(base) || classify(base) != libraryFile:
		return declined(declineNotSource, "%s is no library TypeScript source", f)
	case slices.Contains(vitestConfigNames, base):
		return declined(declineNotSource, "%s is a vitest config name", f)
	}
	switch strings.ToLower(base) {
	case "main.ts", "main.tsx", "app.ts", "app.tsx":
		return declined(declineNotSource, "%s makes its package an app", f)
	}
	stem := sourceStem(base)
	for _, name := range names {
		if name != base && sourceStem(name) == stem {
			return declined(declineNotSource, "%s shares its stem with %s", f, name)
		}
	}
	return nil
}

// sourceStem is a file name without any extension a program or its outputs give it, lower-cased.
func sourceStem(name string) string {
	name = strings.ToLower(name)
	for _, ext := range []string{".d.ts", ".d.mts", ".d.cts"} {
		if stem, ok := strings.CutSuffix(name, ext); ok {
			return stem
		}
	}
	if slices.Contains(programExtensions, path.Ext(name)) {
		return strings.TrimSuffix(name, path.Ext(name))
	}
	return name
}

// excludeMatch is whether Gazelle's walk excludes the file f by pattern, for
// the patterns this can read the way doublestar does; known is false for any other.
func excludeMatch(pattern, f string) (matched, known bool) {
	switch {
	case strings.Contains(f, ","):
		return false, false
	case !strings.ContainsAny(pattern, "*?[{\\"):
		return pattern == f, true
	case strings.ContainsAny(pattern, "{[\\"):
		// doublestar reads [!...] as negation and path.Match does not.
		return false, false
	}
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok {
		if strings.Contains(rest, "/") || strings.Contains(rest, "**") {
			return false, false
		}
		matched, err := path.Match(rest, path.Base(f))
		return matched, err == nil
	}
	if strings.Contains(pattern, "**") {
		return false, false
	}
	matched, err := path.Match(pattern, f)
	return matched, err == nil
}

func mapsKeys[V any](ms ...map[string]V) iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, m := range ms {
			for k := range m {
				if !yield(k) {
					return
				}
			}
		}
	}
}

func ruleLabel(e *packageEdit) string {
	return "//" + e.pkg + ":" + e.r.Name()
}

// packageFor is the package of the walked directory dir: the nearest walked
// directory at or above it with a BUILD file, which must be a program package
// and hold its ts_compile; nothing marks the rule or its srcs as kept.
func packageFor(dir string, walked map[string]int, dirs, buildNames []string, programs map[string]bool, repoRoot string, edits map[string]*packageEdit) (*packageEdit, *decline) {
	for pkg := dir; ; pkg = parentDir(pkg) {
		if e := edits[pkg]; e != nil {
			return e, nil
		}
		i, seen := walked[pkg]
		if !seen {
			return nil, declined(declineOwner, "%s, above %s, is not walked", orRepoRoot(pkg), dir)
		}
		build := ""
		for _, name := range buildNames {
			if strings.Contains("\n"+dirs[i], "\nl "+name+" -> ") {
				return nil, declined(declineOwner, "%s's %s is a link", orRepoRoot(pkg), name)
			}
			if build == "" && strings.Contains("\n"+dirs[i], "\nb "+name+" ") {
				build = name
			}
		}
		if build == "" {
			if pkg == "" {
				return nil, declined(declineOwner, "no BUILD file at or above %s", dir)
			}
			continue
		}
		if !programs[pkg] {
			return nil, declined(declineOwner, "%s, the package of %s, lists no program the run generates rules for", orRepoRoot(pkg), dir)
		}
		file := filepath.Join(repoRoot, filepath.FromSlash(pkg), build)
		info, err := os.Stat(file)
		if err != nil {
			return nil, declined(declineOwner, "%v", err)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, declined(declineOwner, "%v", err)
		}
		f, err := rule.LoadData(file, pkg, data)
		if err != nil {
			return nil, declined(declineOwner, "%v", err)
		}
		e := &packageEdit{pkg: pkg, file: file, dirIndex: i, f: f, original: data, mode: info.Mode(), srcs: map[string]bool{}}
		for _, r := range f.Rules {
			if r.Kind() == "ts_compile" && r.Name() == packageName(pkg) {
				e.r = r
			}
		}
		switch {
		case slices.ContainsFunc(f.Directives, func(d rule.Directive) bool { return d.Key == "ignore" }):
			return nil, declined(declineKept, "%s tells Gazelle to leave it alone", file)
		case e.r == nil:
			return nil, declined(declineOwner, "%s holds no ts_compile named %s", file, packageName(pkg))
		case e.r.ShouldKeep() || attrKept(e.r, "srcs"):
			return nil, declined(declineKept, "%s keeps %s or its srcs", file, ruleLabel(e))
		}
		list, ok := e.r.Attr("srcs").(*bzl.ListExpr)
		if !ok {
			return nil, declined(declineKept, "%s's srcs is no literal list", ruleLabel(e))
		}
		for _, item := range list.List {
			str, ok := item.(*bzl.StringExpr)
			if !ok || len(str.Comments.Before)+len(str.Comments.Suffix)+len(str.Comments.After) > 0 {
				return nil, declined(declineKept, "%s's srcs holds an expression or a comment", ruleLabel(e))
			}
			e.srcs[str.Value] = true
		}
		if len(list.Comments.Before)+len(list.Comments.Suffix)+len(list.Comments.After) > 0 {
			return nil, declined(declineKept, "%s's srcs carries a comment", ruleLabel(e))
		}
		edits[pkg] = e
		return e, nil
	}
}

// render is the BUILD file Gazelle's merge writes once the rule's srcs gain and
// lose the edit's sources: the same merge and format over the same file.
func (e *packageEdit) render() *decline {
	srcs := maps.Clone(e.srcs)
	for _, src := range e.lost {
		delete(srcs, src)
	}
	for _, src := range e.gained {
		srcs[src] = true
	}
	generated := rule.NewRule(e.r.Kind(), e.r.Name())
	generated.SetAttr("srcs", slices.Sorted(maps.Keys(srcs)))
	rule.MergeRules(generated, e.r, map[string]bool{"srcs": true}, e.file)
	e.content = e.f.Format()
	if bytes.Equal(e.content, e.original) {
		return declined(declineOwner, "%s would not change", e.file)
	}
	return nil
}

// write writes the edit in place, as Gazelle does, or the original back.
func (e *packageEdit) write(restore bool) error {
	content := e.content
	if restore {
		content = e.original
	}
	if err := os.WriteFile(e.file, content, e.mode.Perm()); err != nil {
		return fmt.Errorf("writing %s: %w", e.file, err)
	}
	return nil
}
