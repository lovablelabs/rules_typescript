package typescript

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
)

const (
	buildSnapshotName  = "build-snapshot"
	buildSnapshotMagic = "rules_typescript gazelle build snapshot v1"
)

// buildSnapshot is what a recorded run generated for each BUILD file it read;
// the files' bytes follow it in the snapshot file, so a check decodes only this.
type buildSnapshot struct {
	Magic string
	// Walk ties the snapshot to its run record's walked directories.
	Walk     string
	Packages []buildPackage
	// Loads are the directories the crawl loaded only through deps, each with
	// the chains of Edges that reached an entry at or below it in that crawl.
	Edges   []crawlEdge
	Loads   []crawlLoad
	content []byte
}

type buildPackage struct {
	Pkg, File string
	Sum       string
	// Offset and Size place the file as the run left it in the snapshot's content.
	Offset, Size int
	Rules        []snapshotRule
}

// snapshotRule is one rule GenerateRules returned, generated or empty, with
// the attributes a BUILD edit may change as it set them.
type snapshotRule struct {
	Kind, Name string
	Empty      bool
	Attrs      []snapshotAttr
}

// snapshotAttr is a string list as GenerateRules built it; Plain is false
// for anything else, which the proof does not rebuild.
type snapshotAttr struct {
	Key       string
	Plain     bool
	Values    []string
	MultiLine bool
}

type crawlEdge struct{ Rule, Target string }

type crawlLoad struct {
	Key    string
	Chains [][]int32
}

// maxLoadChains bounds the chains kept per load; keeping fewer only declines more.
const maxLoadChains = 8

func buildSnapshotFile(recordFile string) string {
	return filepath.Join(filepath.Dir(recordFile), buildSnapshotName+strings.TrimPrefix(filepath.Base(recordFile), runRecordName))
}

func loadBuildSnapshot(file string) *buildSnapshot {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	r := bytes.NewReader(data)
	var snap buildSnapshot
	if gob.NewDecoder(r).Decode(&snap) != nil || snap.Magic != buildSnapshotMagic {
		return nil
	}
	snap.content = data[len(data)-r.Len():]
	for _, p := range snap.Packages {
		if p.Offset < 0 || p.Size < 0 || p.Offset > len(snap.content) || p.Size > len(snap.content)-p.Offset {
			return nil
		}
	}
	return &snap
}

func (snap *buildSnapshot) recorded(p *buildPackage) []byte {
	return snap.content[p.Offset : p.Offset+p.Size : p.Offset+p.Size]
}

var tsKinds = sync.OnceValue((&tsLang{}).Kinds)

// editableAttr is whether a BUILD edit may change key of kind: the pre-resolve
// merge recomputes it, and before that the walk reads it only to crawl deps.
func editableAttr(kind, key string) bool {
	if key != "deps" || (kind != "ts_compile" && kind != "ts_test") {
		return false
	}
	info := tsKinds()[kind]
	return info.MergeableAttrs[key] && info.ResolveAttrs[key] && !info.NonEmptyAttrs[key]
}

// buildFactsRecorder collects a recording run's build snapshot; seal runs from
// writeRunCandidate, apart from the walk's goroutine.
type buildFactsRecorder struct {
	mu   sync.Mutex
	pkgs map[string]*buildPackage
	// hops, pushes and loaded are one crawl's: the deps it followed, the
	// entries it queued with their chain of hops, and the directories it loaded.
	hops   []crawlHop
	pushes []crawlPush
	loaded []string
	edges  map[crawlEdge]int32
	// crawling is whether a crawl's queue runs, whose loads the deps it followed reach.
	crawling bool
	// crawlEdges and crawlLoads are the snapshot's Edges and Loads.
	crawlEdges []crawlEdge
	crawlLoads []crawlLoad
}

type crawlHop struct {
	edge   crawlEdge
	parent int32
}

type crawlPush struct {
	key   string
	chain int32
}

func newBuildFactsRecorder() *buildFactsRecorder {
	return &buildFactsRecorder{pkgs: map[string]*buildPackage{}, edges: map[crawlEdge]int32{}}
}

func (b *buildFactsRecorder) generated(args language.GenerateArgs, res language.GenerateResult) {
	if b == nil || args.File == nil || args.File.Path == "" || len(res.Gen)+len(res.Empty) == 0 {
		return
	}
	pkg := &buildPackage{Pkg: args.Rel, File: args.File.Path}
	for _, list := range []struct {
		rules []*rule.Rule
		empty bool
	}{{res.Empty, true}, {res.Gen, false}} {
		for _, r := range list.rules {
			g := snapshotRule{Kind: r.Kind(), Name: r.Name(), Empty: list.empty}
			for _, key := range r.AttrKeys() {
				if editableAttr(r.Kind(), key) {
					g.Attrs = append(g.Attrs, recordAttr(key, r.Attr(key)))
				}
			}
			pkg.Rules = append(pkg.Rules, g)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pkgs[args.Rel] = pkg
}

// recordAttr reads a generated string list; anything with a position or a
// comment did not come from a value, and the proof does not rebuild it.
func recordAttr(key string, expr bzl.Expr) snapshotAttr {
	a := snapshotAttr{Key: key}
	list, ok := expr.(*bzl.ListExpr)
	if !ok || list.Start.Line != 0 || list.End.Pos.Line != 0 || hasComments(list) {
		return a
	}
	for _, item := range list.List {
		str, ok := item.(*bzl.StringExpr)
		if !ok || str.Start.Line != 0 || str.TripleQuote || hasComments(str) {
			return a
		}
		a.Values = append(a.Values, str.Value)
	}
	a.Plain, a.MultiLine = true, list.ForceMultiLine
	return a
}

func (a snapshotAttr) expr() bzl.Expr {
	list := &bzl.ListExpr{List: make([]bzl.Expr, 0, len(a.Values)), ForceMultiLine: a.MultiLine}
	for _, v := range a.Values {
		list.List = append(list.List, &bzl.StringExpr{Value: v})
	}
	return list
}

func hasComments(e bzl.Expr) bool {
	c := e.Comment()
	if len(c.Before)+len(c.Suffix)+len(c.After) > 0 {
		return true
	}
	list, ok := e.(*bzl.ListExpr)
	return ok && len(list.End.Before) > 0
}

func (b *buildFactsRecorder) startCrawl() {
	if b == nil {
		return
	}
	b.hops, b.pushes, b.loaded, b.crawling = b.hops[:0], b.pushes[:0], b.loaded[:0], true
}

// hop is the id of the crawl following ruleKey's dep on target, reached
// through hop parent (-1 for none).
func (b *buildFactsRecorder) hop(ruleKey, target string, parent int32) int32 {
	b.hops = append(b.hops, crawlHop{crawlEdge{ruleKey, target}, parent})
	return int32(len(b.hops) - 1)
}

func (b *buildFactsRecorder) pushed(key string, chain int32) {
	b.pushes = append(b.pushes, crawlPush{key, chain})
}

func (b *buildFactsRecorder) loadedDir(dir string) {
	if b != nil && b.crawling {
		b.loaded = append(b.loaded, dir)
	}
}

// endCrawl keeps, per directory the crawl loaded, the dep chains that queued an entry at or
// below it; a directory an entry queued by no dep reaches loads whatever a BUILD edit does.
func (b *buildFactsRecorder) endCrawl() {
	if b == nil {
		return
	}
	b.crawling = false
	if len(b.loaded) == 0 {
		return
	}
	type reached struct {
		heads  []int32
		freely bool
	}
	loaded := map[string]*reached{}
	for _, dir := range b.loaded {
		loaded[dir] = &reached{}
	}
	for _, p := range b.pushes {
		target, err := parseLabel(p.key)
		if err != nil || target.Repo != "" {
			continue
		}
		for dir := target.Pkg; ; dir = parentDir(dir) {
			if r := loaded[dir]; r != nil && !r.freely {
				switch {
				case p.chain < 0:
					r.freely = true
				case len(r.heads) < maxLoadChains && !slices.Contains(r.heads, p.chain):
					r.heads = append(r.heads, p.chain)
				}
			}
			if dir == "" {
				break
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, dir := range slices.Sorted(maps.Keys(loaded)) {
		r := loaded[dir]
		if r.freely {
			continue
		}
		load := crawlLoad{Key: orRepoRoot(dir)}
		for _, head := range r.heads {
			var ids []int32
			for h := head; h >= 0; h = b.hops[h].parent {
				id, ok := b.edges[b.hops[h].edge]
				if !ok {
					id = int32(len(b.crawlEdges))
					b.edges[b.hops[h].edge] = id
					b.crawlEdges = append(b.crawlEdges, b.hops[h].edge)
				}
				ids = append(ids, id)
			}
			load.Chains = append(load.Chains, ids)
		}
		b.crawlLoads = append(b.crawlLoads, load)
	}
}

// seal is the snapshot of the run whose walk digest is walk, every BUILD file
// as it reads now; the record is kept only if none moved during the run.
func (b *buildFactsRecorder) seal(walk string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	snap := buildSnapshot{Magic: buildSnapshotMagic, Walk: walk, Edges: b.crawlEdges, Loads: b.crawlLoads}
	var content bytes.Buffer
	for _, pkg := range slices.Sorted(maps.Keys(b.pkgs)) {
		p := *b.pkgs[pkg]
		data, err := os.ReadFile(p.File)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		p.Sum, p.Offset, p.Size = hex.EncodeToString(sum[:]), content.Len(), len(data)
		content.Write(data)
		snap.Packages = append(snap.Packages, p)
	}
	snap.content = content.Bytes()
	return snap.encode()
}

func (snap *buildSnapshot) encode() ([]byte, error) {
	var out bytes.Buffer
	if err := gob.NewEncoder(&out).Encode(snap); err != nil {
		return nil, fmt.Errorf("encoding the build snapshot: %w", err)
	}
	out.Write(snap.content)
	return out.Bytes(), nil
}

const (
	declineBuildParse     = "build-parse"
	declineBuildDirective = "build-directive"
	declineBuildLoads     = "build-loads"
	declineBuildRules     = "build-rules"
	declineBuildRename    = "build-rename"
	declineBuildKeep      = "build-keep"
	declineBuildAttr      = "build-attr"
	declineBuildCrawl     = "build-crawl"
	declineBuildMerge     = "build-merge"
	declineBuildStatement = "build-statement"
)

// buildEditFiles are the BUILD files whose content alone moved in the walked
// directories; ok is false when anything else moved.
func buildEditFiles(rec *runRecord, packages map[string]*buildPackage, repoRoot string, buildNames []string, dirs []string, moved []int, names *nameReads) (files []string, ok bool) {
	for _, i := range moved {
		var b strings.Builder
		changed := false
		for entry := range strings.SplitSeq(dirs[i], "\n") {
			if entry == "" {
				continue
			}
			if fields := strings.Fields(entry); len(fields) == 4 && fields[0] == "b" && slices.Contains(buildNames, fields[1]) {
				file := filepath.Join(rec.dirs[i], fields[1])
				if p := packages[file]; p != nil && fields[3] != p.Sum {
					entry = "b " + fields[1] + " " + fields[2] + " " + p.Sum
					files = append(files, file)
					changed = true
				}
			}
			b.WriteString(entry)
			b.WriteByte('\n')
		}
		if !changed || dirSum(repoRoot, rec.dirs[i], b.String(), names) != rec.sums[i] {
			return nil, false
		}
	}
	return files, true
}

// decideBuildEdit proves that Gazelle's pre-resolve merge takes each edited
// BUILD file to the state it took the recorded one to, after which the run is
// the recorded one; handled is false when anything but BUILD content moved.
func decideBuildEdit(rec *runRecord, snapFile, repo, repoRoot string, buildNames []string, lines inputLines, moved []int, stamps *newest, names *nameReads) (*fastPathDecision, *decline, bool) {
	dirs := lines.dirs
	snap := loadBuildSnapshot(snapFile)
	if snap == nil || snap.Walk != rec.walkDigest {
		return nil, nil, false
	}
	packages := map[string]*buildPackage{}
	for i := range snap.Packages {
		packages[snap.Packages[i].File] = &snap.Packages[i]
	}
	files, ok := buildEditFiles(rec, packages, repoRoot, buildNames, dirs, moved, names)
	if !ok {
		return nil, nil, false
	}
	sums := lineSums(lines)
	walked := setOf(rec.dirs)
	at := 0
	for _, list := range [][]string{rec.files, rec.reads, rec.roots} {
		for i, input := range list {
			if sums[at+i] != rec.sums[len(rec.dirs)+at+i] && !(slices.Contains(buildNames, filepath.Base(input)) && walked[filepath.Dir(input)]) {
				return nil, declined(declineInputs, "%s changed beside the BUILD edit", input), true
			}
		}
		at += len(list)
	}
	var dropped []crawlEdge
	dirIndex := map[string]int{}
	for i, dir := range rec.dirs {
		dirIndex[dir] = i
	}
	decision := &fastPathDecision{lines: lines, files: map[string]string{}, stamps: stamps, restores: true, repoRoot: repoRoot, names: names}
	for _, file := range files {
		p := packages[file]
		info, err := os.Stat(file)
		if err != nil {
			return nil, declined(declineInputs, "%v", err), true
		}
		edited, err := os.ReadFile(file)
		if err != nil {
			return nil, declined(declineInputs, "%v", err), true
		}
		at := dirIndex[filepath.Dir(file)]
		if !strings.Contains("\n"+dirs[at], "\n"+buildEntry(filepath.Base(file), info.Mode(), edited)+"\n") {
			return nil, declined(declineInputs, "%s moved during the run", file), true
		}
		if why := proveBuildEdit(p, snap.recorded(p), edited, repo, &dropped); why != nil {
			return nil, why, true
		}
		decision.edits = append(decision.edits, &packageEdit{pkg: p.Pkg, file: file, dirIndex: at, original: edited, content: snap.recorded(p), mode: info.Mode()})
	}
	if why := snap.cutLoad(dropped); why != nil {
		return nil, why, true
	}
	return decision, nil, true
}

// cutLoad says why dropping these deps may change what the walk's crawl loads:
// every chain that reached one of its loads runs through one of them.
func (snap *buildSnapshot) cutLoad(dropped []crawlEdge) *decline {
	if len(dropped) == 0 {
		return nil
	}
	drops := map[crawlEdge]bool{}
	for _, e := range dropped {
		drops[e] = true
	}
	cut := map[int32]bool{}
	for id, e := range snap.Edges {
		if drops[e] {
			cut[int32(id)] = true
		}
	}
	for _, load := range snap.Loads {
		if !slices.ContainsFunc(load.Chains, func(chain []int32) bool {
			return !slices.ContainsFunc(chain, func(id int32) bool { return cut[id] })
		}) {
			return declined(declineBuildCrawl, "the walk's crawl loaded %s only through a dep the edit drops", load.Key)
		}
	}
	return nil
}

// proveBuildEdit says why the edited and recorded files may reach different
// states; they may differ only in deps the merge recomputes.
func proveBuildEdit(p *buildPackage, recorded, edited []byte, repo string, dropped *[]crawlEdge) *decline {
	now, err := rule.LoadData(p.File, p.Pkg, edited)
	if err != nil {
		return declined(declineBuildParse, "%v", err)
	}
	was, err := rule.LoadData(p.File, p.Pkg, recorded)
	if err != nil {
		return declined(declineBuildParse, "the recorded %s: %v", p.File, err)
	}
	if !slices.Equal(now.Directives, was.Directives) {
		return declined(declineBuildDirective, "%s changes a directive, which Configure reads", p.File)
	}
	if !slices.EqualFunc(now.Loads, was.Loads, func(a, b *rule.Load) bool {
		return a.Name() == b.Name() && slices.Equal(a.SymbolPairs(), b.SymbolPairs())
	}) {
		return declined(declineBuildLoads, "%s changes a load statement", p.File)
	}
	if len(now.Rules) != len(was.Rules) {
		return declined(declineBuildRules, "%s holds %d rules where the record holds %d", p.File, len(now.Rules), len(was.Rules))
	}
	strip := map[*rule.Rule][]string{}
	for i, r := range now.Rules {
		w := was.Rules[i]
		switch {
		case r.Kind() != w.Kind():
			return declined(declineBuildRules, "%s's rule %d is a %s where the record has a %s", p.File, i, r.Kind(), w.Kind())
		case r.Name() != w.Name():
			return declined(declineBuildRename, "%s names %s(%s) where the record has %s", p.File, r.Kind(), r.Name(), w.Name())
		case r.ShouldKeep() != w.ShouldKeep():
			return declined(declineBuildKeep, "%s adds or removes a keep comment on %s(%s)", p.File, r.Kind(), r.Name())
		}
		var gens []snapshotRule
		for _, g := range p.Rules {
			if g.Kind == r.Kind() && g.Name == r.Name() {
				gens = append(gens, g)
			}
		}
		managed := len(gens) > 0 && !r.ShouldKeep()
		keys := slices.Concat(r.AttrKeys(), w.AttrKeys())
		slices.Sort(keys)
		for _, key := range slices.Compact(keys) {
			if managed && tsKinds()[r.Kind()].MergeableAttrs[key] && r.Attr(key) != nil && !isLiteralAttrValue(r.Attr(key)) {
				return declined(declineBuildAttr, "%s(%s)'s %s is an expression Gazelle logs by its line", r.Kind(), r.Name(), key)
			}
			if sameAttr(r, w, key) {
				continue
			}
			if why := proveAttrEdit(p, r, w, key, gens, managed, repo, dropped); why != nil {
				return why
			}
			strip[r] = append(strip[r], key)
			strip[w] = append(strip[w], key)
		}
	}
	if !bytes.Equal(withoutAttrs(now, strip), withoutAttrs(was, strip)) {
		return declined(declineBuildStatement, "%s differs from the record outside the deps Gazelle recomputes", p.File)
	}
	return nil
}

// proveAttrEdit says why the merge may recompute an edited attribute otherwise than the recorded one.
func proveAttrEdit(p *buildPackage, r, w *rule.Rule, key string, gens []snapshotRule, managed bool, repo string, dropped *[]crawlEdge) *decline {
	label := fmt.Sprintf("%s(%s)'s %s", r.Kind(), r.Name(), key)
	now, was := r.Attr(key), w.Attr(key)
	switch {
	case attrKept(r, key) || attrKept(w, key):
		return declined(declineBuildKeep, "%s in %s carries a keep comment", label, p.File)
	case !slices.Equal(keptItems(now), keptItems(was)):
		return declined(declineBuildKeep, "%s in %s adds, removes or edits an item a keep comment holds", label, p.File)
	case !managed || !editableAttr(r.Kind(), key):
		return declined(declineBuildAttr, "%s in %s is not an attribute Gazelle's merge recomputes", label, p.File)
	case !stringList(now) || !stringList(was):
		return declined(declineBuildAttr, "%s in %s is no list of strings", label, p.File)
	case !slices.Equal(attrComments(r, key), attrComments(w, key)):
		return declined(declineBuildAttr, "%s in %s changes the comments on the attribute", label, p.File)
	}
	recorded := setOf(stringValues(was))
	for _, v := range stringValues(now) {
		if !recorded[v] {
			return declined(declineBuildAttr, "%s in %s adds %q, which the walk's crawl would follow", label, p.File, v)
		}
	}
	kept := setOf(stringValues(now))
	ruleKey := emissionLabel(repo, p.Pkg, ":"+r.Name())
	for _, v := range stringValues(was) {
		if !kept[v] {
			*dropped = append(*dropped, crawlEdge{ruleKey, emissionLabel(repo, p.Pkg, v)})
		}
	}
	for _, g := range gens {
		for _, a := range g.Attrs {
			if a.Key == key && !a.Plain {
				return declined(declineBuildMerge, "GenerateRules sets %s to an expression the proof does not rebuild", label)
			}
		}
	}
	if !sameExpr(premerged(r.Kind(), r.Name(), key, now, gens), premerged(r.Kind(), r.Name(), key, was, gens)) {
		return declined(declineBuildMerge, "Gazelle's merge does not take %s in %s back to what it recomputed for the record", label, p.File)
	}
	return nil
}

// premerged is key after Gazelle's pre-resolve merge, which merges empty rules before generated ones.
func premerged(kind, name, key string, have bzl.Expr, gens []snapshotRule) bzl.Expr {
	dst := rule.NewRule(kind, name)
	if have != nil {
		dst.SetAttr(key, have)
	}
	mergeable := tsKinds()[kind].MergeableAttrs
	for _, empty := range []bool{true, false} {
		for _, g := range gens {
			if g.Empty != empty {
				continue
			}
			src := rule.NewRule(kind, name)
			for _, a := range g.Attrs {
				if a.Key == key {
					src.SetAttr(key, a.expr())
				}
			}
			rule.MergeRules(src, dst, mergeable, "")
		}
	}
	return dst.Attr(key)
}

func sameAttr(r, w *rule.Rule, key string) bool {
	return sameExpr(r.Attr(key), w.Attr(key)) && slices.Equal(attrComments(r, key), attrComments(w, key))
}

func attrComments(r *rule.Rule, key string) []string {
	c := r.AttrComments(key)
	if c == nil {
		return nil
	}
	var out []string
	for _, group := range [][]bzl.Comment{c.Before, c.Suffix, c.After} {
		for _, comment := range group {
			out = append(out, comment.Token)
		}
	}
	return out
}

// sameExpr is whether a and b print alike wherever Gazelle prints them: in a
// syntax tree's own layout, which the state digest reads, and in a BUILD file's.
func sameExpr(a, b bzl.Expr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if la, ok := a.(*bzl.ListExpr); ok {
		lb, ok := b.(*bzl.ListExpr)
		if !ok || la.ForceMultiLine != lb.ForceMultiLine {
			return false
		}
	}
	return bzl.FormatString(a) == bzl.FormatString(b) && bytes.Equal(buildPrinted(a), buildPrinted(b))
}

func buildPrinted(e bzl.Expr) []byte {
	call := &bzl.CallExpr{X: &bzl.Ident{Name: "r"}, List: []bzl.Expr{&bzl.AssignExpr{LHS: &bzl.Ident{Name: "a"}, Op: "=", RHS: e}}}
	return bzl.FormatWithoutRewriting(&bzl.File{Type: bzl.TypeBuild, Stmt: []bzl.Expr{call}})
}

// The merge carries a list item's comments with it, but not the list's.
func stringList(e bzl.Expr) bool {
	if e == nil {
		return true
	}
	list, ok := e.(*bzl.ListExpr)
	if !ok || hasComments(list) {
		return false
	}
	return !slices.ContainsFunc(list.List, func(item bzl.Expr) bool {
		_, ok := item.(*bzl.StringExpr)
		return !ok
	})
}

func keptItems(e bzl.Expr) []string {
	var out []string
	for _, item := range listElements(e) {
		if rule.ShouldKeep(item) {
			text := bzl.FormatString(item)
			c := item.Comment()
			for _, comment := range slices.Concat(c.Before, c.Suffix, c.After) {
				text += "\n" + comment.Token
			}
			out = append(out, text)
		}
	}
	return out
}

func stringValues(e bzl.Expr) []string {
	var out []string
	for _, item := range listElements(e) {
		if str, ok := item.(*bzl.StringExpr); ok {
			out = append(out, str.Value)
		}
	}
	return out
}

func withoutAttrs(f *rule.File, strip map[*rule.Rule][]string) []byte {
	// A parsed rule's attribute comments are its assignment's own.
	removed := map[*bzl.Comments]bool{}
	for r, keys := range strip {
		for _, key := range keys {
			if c := r.AttrComments(key); c != nil {
				removed[c] = true
			}
		}
	}
	isRemoved := func(e bzl.Expr) bool {
		as, ok := e.(*bzl.AssignExpr)
		return ok && removed[as.Comment()]
	}
	stmts := make([]bzl.Expr, 0, len(f.File.Stmt))
	for _, stmt := range f.File.Stmt {
		if call, ok := stmt.(*bzl.CallExpr); ok && slices.ContainsFunc(call.List, isRemoved) {
			kept := *call
			kept.List = slices.DeleteFunc(slices.Clone(call.List), isRemoved)
			stmt = &kept
		}
		stmts = append(stmts, stmt)
	}
	file := *f.File
	file.Stmt = stmts
	return bzl.FormatWithoutRewriting(&file)
}

// verifyBuildEdit runs the full pass on the edited tree: writing the recorded
// file first would make that pass change nothing whatever the proof.
func verifyBuildEdit(file string, rec *runRecord, decision *fastPathDecision, args []string) int {
	code, _, stderr := runSelf(args)
	fail := func(why string) int {
		os.Remove(file)
		os.Remove(buildSnapshotFile(file))
		log.Printf("typescript: fast path: the full pass %s; the record is removed and the BUILD files are what it wrote", why)
		return 1
	}
	if code != rec.exit {
		return fail(fmt.Sprintf("exits %d where the record exits %d", code, rec.exit))
	}
	if !slices.Equal(logLines(stderr), logLines(rec.stderr)) {
		return fail(fmt.Sprintf("logs otherwise than the record:\n--- record\n%s--- full pass\n%s", rec.stderr, stderr))
	}
	dirs, err := dirLines(rec.dirs, decision.buildNames, decision.skipDir, nil)
	if err != nil {
		return fail(err.Error())
	}
	for i, line := range dirs {
		if dirSum(decision.repoRoot, rec.dirs[i], line, decision.names) != rec.sums[i] {
			return fail(fmt.Sprintf("leaves %s otherwise than the recorded run", rec.dirs[i]))
		}
	}
	decision.carryForward(file, rec, false)
	log.Print("typescript: fast path: verified; the full pass writes the recorded BUILD files back and logs as the record does")
	return rec.exit
}
