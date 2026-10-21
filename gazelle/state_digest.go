package typescript

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"maps"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// generatedPackage is what Gazelle handed one GenerateRules call and what it
// returned; Resolve reads both.
type generatedPackage struct {
	args language.GenerateArgs
	res  language.GenerateResult
}

// importAnswer is one Imports answer, which Gazelle's rule index holds as given.
type importAnswer struct {
	label label.Label
	kind  string
	specs []resolve.ImportSpec
}

// stateHasher digests the state Resolve and AfterResolvingDeps start from.
// Shared objects are numbered on first sight, so the digest follows the
// object graph and not where it sits in memory.
type stateHasher struct {
	h           hash.Hash
	w           *bufio.Writer
	ids         map[any]int
	programSums map[*program][sha256.Size]byte
	ruleBodies  map[*rule.Rule]ruleBody
	digest      [sha256.Size]byte
	outside     map[string]dirScope
	// Why the state cannot stand for a replay, or "".
	opaque string
}

// dirScope is how much of a walked directory the state digest leaves out.
type dirScope uint8

const (
	digested dirScope = iota
	// namesOut leaves out the directory's file names, which after the walk only
	// dirNames and dirWalked read, and they note it.
	namesOut
	// whollyOut has no BUILD file either: no rule, index answer or emission file
	// names it, so the digest leaves out its configuration and generate call too.
	whollyOut
)

// outsideDirs are the walked directories with nothing Resolve reads but their
// names: no TypeScript source, tsconfig.json or package.json, no program, no
// generated rule.
func (s *programStore) outsideDirs(generated []generatedPackage) map[string]dirScope {
	built, generating := map[string]bool{}, map[string]bool{}
	for _, g := range generated {
		built[g.args.Rel] = built[g.args.Rel] || g.args.File != nil
		// Empty rules only delete from a BUILD file, which a directory left wholly out has not.
		generating[g.args.Rel] = generating[g.args.Rel] || len(g.res.Gen)+len(g.res.Imports)+len(g.res.RelsToIndex) > 0
	}
	programDirs := map[string]bool{}
	for _, m := range []map[string]*program{s.vitestPrograms, s.compilerPrograms} {
		for cfg := range m {
			programDirs[parentDir(cfg)] = true
		}
	}
	outside := map[string]dirScope{}
	for dir, names := range s.files {
		input := s.inputs[dir]
		if len(s.visited[dir]) > 0 || slices.ContainsFunc(names, func(name string) bool { return name == "tsconfig.json" || name == "package.json" }) ||
			input.program != nil || input.discoveryErr != nil || input.refresh != nil ||
			s.programs[dir] != nil || programDirs[dir] || generating[dir] {
			continue
		}
		outside[dir] = namesOut
		if !built[dir] && !s.generatedPackages[dir] && (s.emission == nil || s.emission.files[dir] == nil) {
			outside[dir] = whollyOut
		}
	}
	return outside
}

func (d *stateHasher) out(dir string) bool { return d.outside[dir] != digested }

func (d *stateHasher) whollyOut(dir string) bool { return d.outside[dir] == whollyOut }

// sortedEachExcept is sortedEach over the keys out leaves in.
func sortedEachExcept[V any](d *stateHasher, m map[string]V, out func(string) bool, digest func(V)) {
	keys := slices.DeleteFunc(slices.Sorted(maps.Keys(m)), out)
	d.int(len(keys))
	for _, k := range keys {
		d.str(k)
		digest(m[k])
	}
}

// ruleBody is a rule's digest apart from the private attributes that point
// into the shared state, which the walk digests in order.
type ruleBody struct {
	sum    [sha256.Size]byte
	shared []string
	opaque string
}

func compareImportSpec(a, b resolve.ImportSpec) int {
	return cmp.Or(cmp.Compare(a.Lang, b.Lang), cmp.Compare(a.Imp, b.Imp))
}

// partial is whether the digest leaves out what the rule points to: shared state, or a value it cannot read.
func (b ruleBody) partial() bool { return b.opaque != "" || len(b.shared) > 0 }

// stateDigest is the state's digest, or why it cannot stand for a replay. It
// leaves out the directories in outside, which a replay checks by what the phase read of them.
func (s *programStore) stateDigest(generated []generatedPackage, indexed []importAnswer, outside map[string]dirScope) (digest, refused string) {
	d := newHasher()
	d.outside = outside
	d.programSums, d.ruleBodies = s.prehash(generated)
	d.str("store")
	d.store(s)
	d.str("generated")
	each(d, slices.DeleteFunc(slices.Clone(generated), func(g generatedPackage) bool { return outside[g.args.Rel] == whollyOut }), d.generatedPackage)
	d.str("index")
	each(d, indexed, func(a importAnswer) {
		d.label(a.label)
		d.str(a.kind)
		each(d, a.specs, d.importSpec)
	})
	d.str("listings")
	d.loadedListings()
	if d.opaque != "" {
		return "", d.opaque
	}
	sum := d.sum()
	return hex.EncodeToString(sum[:]), ""
}

// The buffer only batches small writes into SHA-256; a replay makes one hasher
// per rule, program and listing, so a larger one costs more to allocate than it saves.
func newHasher() *stateHasher {
	h := sha256.New()
	return &stateHasher{h: h, w: bufio.NewWriterSize(h, 512)}
}

func (d *stateHasher) sum() [sha256.Size]byte {
	d.w.Flush()
	d.h.Sum(d.digest[:0])
	return d.digest
}

func (d *stateHasher) refuse(why string) {
	if d.opaque == "" {
		d.opaque = why
	}
}

func (d *stateHasher) int(n int) {
	if d.w.Available() < binary.MaxVarintLen64 {
		d.w.Flush()
	}
	d.w.Write(binary.AppendVarint(d.w.AvailableBuffer(), int64(n)))
}

func (d *stateHasher) bool(v bool) {
	if v {
		d.int(1)
	} else {
		d.int(0)
	}
}

// str is length-prefixed, so no two sequences of strings hash alike.
func (d *stateHasher) str(s string) {
	d.int(len(s))
	d.w.WriteString(s)
}

func (d *stateHasher) strs(list []string) { each(d, list, d.str) }

func each[V any](d *stateHasher, list []V, digest func(V)) {
	d.int(len(list))
	for _, v := range list {
		digest(v)
	}
}

func sortedEach[V any](d *stateHasher, m map[string]V, digest func(V)) {
	d.int(len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		d.str(k)
		digest(m[k])
	}
}

// seen numbers p on first sight; the caller digests it only then.
func (d *stateHasher) seen(p any) bool {
	if id, ok := d.ids[p]; ok {
		d.int(id)
		return true
	}
	if d.ids == nil {
		d.ids = map[any]int{}
	}
	d.ids[p] = len(d.ids) + 1
	d.int(-len(d.ids))
	return false
}

func (d *stateHasher) label(l label.Label) {
	d.str(l.Repo)
	d.str(l.Pkg)
	d.str(l.Name)
	d.bool(l.Relative)
	d.bool(l.Canonical)
}

func (d *stateHasher) importSpec(spec resolve.ImportSpec) {
	d.str(spec.Lang)
	d.str(spec.Imp)
}

func (d *stateHasher) expr(e bzl.Expr) { d.str(bzl.FormatString(e)) }

func (d *stateHasher) comments(c *bzl.Comments) {
	if c == nil {
		d.int(-1)
		return
	}
	for _, group := range [][]bzl.Comment{c.Before, c.Suffix, c.After} {
		each(d, group, func(comment bzl.Comment) { d.str(comment.Token) })
	}
}

// rule digests a rule through its attributes, which hold edits a BUILD
// file's syntax tree only shows after Sync.
func (d *stateHasher) rule(r *rule.Rule) {
	if r == nil {
		d.int(0)
		return
	}
	if d.seen(r) {
		return
	}
	body, ok := d.ruleBodies[r]
	if !ok {
		body = digestRuleBody(r)
	}
	d.w.Write(body.sum[:])
	if body.opaque != "" {
		d.refuse(body.opaque)
	}
	each(d, body.shared, func(key string) {
		d.str(key)
		d.imports(r.PrivateAttr(key))
	})
}

func (d *stateHasher) rules(list []*rule.Rule) { each(d, list, d.rule) }

// digestRuleBody reads the rule and nothing it points to, so rules digest
// in parallel.
func digestRuleBody(r *rule.Rule) ruleBody {
	d := newHasher()
	var body ruleBody
	d.str(r.Kind())
	d.str(r.Name())
	d.strs(r.Comments())
	d.bool(r.ShouldKeep())
	each(d, r.Args(), d.expr)
	each(d, r.AttrKeys(), func(key string) {
		d.str(key)
		d.expr(r.Attr(key))
		d.comments(r.AttrComments(key))
	})
	for _, key := range r.PrivateAttrKeys() {
		switch v := r.PrivateAttr(key).(type) {
		case *ruleImports, *protoRuleImports:
			body.shared = append(body.shared, key)
		case nil:
			d.str(key)
			d.str("nil")
		case bool:
			d.str(key)
			d.str("bool")
			d.bool(v)
		case int:
			d.str(key)
			d.str("int")
			d.int(v)
		case string:
			d.str(key)
			d.str("string")
			d.str(v)
		case map[resolve.ImportSpec]bool:
			d.str(key)
			d.str("specs")
			specs := slices.SortedFunc(maps.Keys(v), compareImportSpec)
			each(d, specs, func(spec resolve.ImportSpec) {
				d.importSpec(spec)
				d.bool(v[spec])
			})
		case rule.KindInfo:
			// Static per binary; the record key digests the executable.
			if key != "_gazelle_kind" {
				d.refuse(fmt.Sprintf("private attribute %s holds a %T", key, v))
				break
			}
			d.str(key)
			d.str("kind")
		case interface{ Name() string }:
			// Gazelle 2 tags each generated rule with the extension that made it.
			if key != "_gazelle_lang" {
				d.refuse(fmt.Sprintf("private attribute %s holds a %T", key, v))
				break
			}
			d.str(key)
			d.str("lang")
			d.str(v.Name())
		default:
			d.refuse(fmt.Sprintf("private attribute %s holds a %T", key, v))
		}
	}
	body.sum, body.opaque = d.sum(), d.opaque
	return body
}

// file digests a BUILD file's rules through their attributes and its other
// statements as formatted. Gazelle's pre-resolve merge edits rules without
// syncing the syntax tree, so a rule's call there may be stale.
func (d *stateHasher) file(f *rule.File) {
	if f == nil {
		d.int(0)
		return
	}
	if d.seen(f) {
		return
	}
	d.str(f.Pkg)
	d.str(f.Path)
	d.str(f.DefName)
	d.bool(f.Content == nil)
	if f.File == nil {
		d.int(-1)
	} else {
		each(d, f.File.Stmt, func(stmt bzl.Expr) {
			if call, ok := stmt.(*bzl.CallExpr); ok {
				d.str("call")
				d.comments(&call.Comments)
			} else {
				d.expr(stmt)
			}
		})
	}
	each(d, f.Directives, func(directive rule.Directive) {
		d.str(directive.Key)
		d.str(directive.Value)
	})
	each(d, f.Loads, func(load *rule.Load) {
		d.str(load.Name())
		each(d, load.SymbolPairs(), func(pair struct{ From, To string }) {
			d.str(pair.From)
			d.str(pair.To)
		})
	})
	d.rules(f.Rules)
}

// config digests what the extension reads of a directory's configuration.
// Gazelle's own extensions, named with a leading underscore, hold walk and
// resolve settings from directives, which the BUILD files carry here, and
// from .bazelignore and REPO.bazel, which count as reads.
func (d *stateHasher) config(c *config.Config) {
	if c == nil {
		d.int(0)
		return
	}
	if d.seen(c) {
		return
	}
	d.str(c.WorkDir)
	d.str(c.RepoRoot)
	d.str(c.RepoName)
	d.str(c.ReadBuildFilesDir)
	d.str(c.WriteBuildFilesDir)
	d.strs(c.ValidBuildFileNames)
	d.bool(c.ShouldFix)
	d.bool(c.Strict)
	d.bool(c.IndexLibraries)
	d.bool(c.IndexLazy)
	sortedEach(d, c.KindMap, func(m config.MappedKind) {
		d.str(m.FromKind)
		d.str(m.KindName)
		d.str(m.KindLoad)
	})
	sortedEach(d, c.AliasMap, d.str)
	d.repos(c.Repos)
	d.strs(c.Langs)
	d.bool(c.Bzlmod)
	if c.ModuleToApparentName == nil {
		d.str("")
	} else {
		d.str(c.ModuleToApparentName("rules_typescript"))
	}
	for _, name := range slices.Sorted(maps.Keys(c.Exts)) {
		switch ext := c.Exts[name].(type) {
		case *tsConfig:
			d.str(name)
			d.tsConfig(ext)
		default:
			if gazelleDefaults(name, ext) {
				d.str(name)
				d.str(fmt.Sprintf("%#v", ext))
			} else if !strings.HasPrefix(name, "_") {
				d.refuse("another language configures " + name)
			}
		}
	}
}

type reposKey struct {
	first *rule.Rule
	n     int
}

// repos digests Config.Repos, which every directory's configuration shares
// with the root's, once per list.
func (d *stateHasher) repos(list []*rule.Rule) {
	if len(list) == 0 {
		d.int(0)
		return
	}
	if d.seen(reposKey{list[0], len(list)}) {
		return
	}
	d.rules(list)
}

func (d *stateHasher) tsConfig(tc *tsConfig) {
	d.protoStore(tc.protos)
	sortedEach(d, tc.protoIdentities, d.protoIdentity)
	d.strs(tc.protoEnabled)
	d.str(tc.foreignManifest)
	d.bool(tc.updateOnly)
	d.bool(tc.programs != nil)
	d.npmLock(tc.lock)
	d.bool(tc.lockLoaded)
}

func (d *stateHasher) protoIdentity(id *protoIdentity) {
	if id == nil {
		d.int(0)
		return
	}
	if d.seen(id) {
		return
	}
	d.str(id.Name)
	d.str(id.OutDir)
	d.str(id.Tsconfig)
	d.str(id.NodeModules)
	d.strs(id.Deps)
	d.strs(id.Options)
	d.str(id.owner)
}

func (d *stateHasher) protoStore(p *protoStore) {
	if p == nil {
		d.int(0)
		return
	}
	if d.seen(p) {
		return
	}
	d.str(p.graphPath)
	if g := p.graph; g == nil {
		d.int(0)
	} else {
		each(d, g.Nodes, func(node protoGraphNode) {
			d.str(node.Label)
			d.strs(node.Sources)
			d.strs(node.Deps)
		})
		sortedEach(d, g.Roots, d.str)
	}
	d.strs(p.roots)
	d.bool(p.recursive)
	sortedEach(d, p.identities, d.protoIdentity)
	sortedEach(d, p.observations, func(list []protoObservation) {
		each(d, list, func(obs protoObservation) {
			each(d, obs.nativeDeps, d.label)
			d.config(obs.config)
			d.label(obs.native)
			d.protoIdentity(obs.identity)
			d.strs(obs.paths)
			d.strs(obs.imports)
		})
	})
	sortedEach(d, p.runtimeImports, d.str)
}

func (d *stateHasher) npmLock(l *npmLock) {
	if l == nil {
		d.int(0)
		return
	}
	if d.seen(l) {
		return
	}
	sortedEach(d, l.names, d.bool)
	sortedEach(d, l.importers, func(importer *pnpmImporter) {
		if importer == nil {
			d.int(0)
			return
		}
		d.int(1)
		sortedEach(d, importer.deps, d.str)
		sortedEach(d, importer.links, d.str)
	})
	sortedEach(d, l.members, d.str)
}

func (d *stateHasher) program(p *program) {
	if p == nil {
		d.int(0)
		return
	}
	if d.seen(p) {
		return
	}
	digest, ok := d.programSums[p]
	if !ok {
		digest = programDigest(p)
	}
	d.w.Write(digest[:])
}

// prehash digests apart and in parallel the programs and rules the walk below
// can name, most of the state's bytes; the walk digests any other on sight.
func (s *programStore) prehash(generated []generatedPackage) (map[*program][sha256.Size]byte, map[*rule.Rule]ruleBody) {
	programSet := map[*program]bool{}
	ruleSet := map[*rule.Rule]bool{}
	addProgram := func(p *program) {
		if p != nil {
			programSet[p] = true
		}
	}
	addRule := func(r *rule.Rule) {
		if r != nil {
			ruleSet[r] = true
		}
	}
	addRules := func(list []*rule.Rule) {
		for _, r := range list {
			addRule(r)
		}
	}
	for _, input := range s.inputs {
		addProgram(input.program)
	}
	for _, m := range []map[string]*program{s.programs, s.vitestPrograms, s.compilerPrograms} {
		for _, p := range m {
			addProgram(p)
		}
	}
	if g := s.emission; g != nil {
		for _, r := range g.rules {
			addRule(r)
		}
		for _, f := range g.files {
			if f != nil {
				addRules(f.Rules)
			}
		}
	}
	for _, g := range generated {
		if g.args.File != nil {
			addRules(g.args.File.Rules)
		}
		addRules(g.args.OtherEmpty)
		addRules(g.args.OtherGen)
		addRules(g.res.Gen)
		addRules(g.res.Empty)
		for _, imports := range g.res.Imports {
			if imps, ok := imports.(*ruleImports); ok && imps != nil {
				addProgram(imps.program)
			}
		}
	}
	programList := slices.Collect(maps.Keys(programSet))
	ruleList := slices.Collect(maps.Keys(ruleSet))
	programSums := make([][sha256.Size]byte, len(programList))
	ruleBodies := make([]ruleBody, len(ruleList))
	parallel(len(programList)+len(ruleList), func(i int) {
		if i < len(programList) {
			programSums[i] = programDigest(programList[i])
		} else {
			ruleBodies[i-len(programList)] = digestRuleBody(ruleList[i-len(programList)])
		}
	})
	programs := make(map[*program][sha256.Size]byte, len(programList))
	for i, p := range programList {
		programs[p] = programSums[i]
	}
	rules := make(map[*rule.Rule]ruleBody, len(ruleList))
	for i, r := range ruleList {
		rules[r] = ruleBodies[i]
	}
	return programs, rules
}

// programDigest leaves out bySource, byImporter, moduleEdges, byDir and sourceDirIx, indexes built on first use.
func programDigest(p *program) [sha256.Size]byte {
	d := newHasher()
	d.listing(&p.Listing)
	each(d, p.candidates, d.candidate)
	d.resolutionFacts(p.resolution)
	d.str(p.config)
	d.str(p.dir)
	d.str(p.refused)
	d.bool(p.manifest)
	// The listing key names the cache entry a replayed record's import facts refer to.
	d.str(p.key)
	return d.sum()
}

func (d *stateHasher) edge(e explainfiles.Edge) {
	d.int(int(e.Kind))
	d.str(e.From)
	d.str(e.To)
	d.str(e.Specifier)
}

func (d *stateHasher) failedLookup(f explainfiles.FailedLookup) {
	d.str(f.File)
	d.str(f.Candidate)
}

func (d *stateHasher) typeEntry(t explainfiles.TypeEntry) {
	d.str(t.Entry)
	d.str(t.File)
}

func (d *stateHasher) listing(l *explainfiles.Listing) {
	d.strs(l.Files)
	d.strs(l.Roots)
	each(d, l.Edges, d.edge)
	each(d, l.Types, d.typeEntry)
	each(d, l.Implicit, d.typeEntry)
	d.strs(l.Diagnostics)
	each(d, l.Unresolved, func(u explainfiles.Unresolved) {
		d.int(int(u.Kind))
		d.str(u.From)
		d.str(u.Specifier)
		each(d, u.Candidates, d.failedLookup)
	})
	each(d, l.Resolutions, func(r explainfiles.Resolution) {
		d.edge(r.Edge)
		each(d, r.Candidates, d.failedLookup)
		d.strs(r.Substitutions)
		d.bool(r.FromPackage)
	})
	each(d, l.Candidates, func(c explainfiles.ResolutionCandidate) {
		d.int(int(c.Kind))
		d.str(c.From)
		d.str(c.Specifier)
		d.str(c.Path)
	})
	each(d, l.PackageTargets, d.edge)
}

func (d *stateHasher) resolutionFacts(f resolutionFacts) {
	d.strs(f.Kinds)
	d.strs(f.RelativeTargets)
	d.bool(f.RootDirs)
	// Only when present: a program without paths keeps the bytes its records hold.
	if len(f.Substitutions) > 0 || len(f.SubstitutionTargets) > 0 {
		d.strs(f.Substitutions)
		d.strs(f.SubstitutionTargets)
	}
}

func (d *stateHasher) candidate(c resolutionCandidate) {
	d.str(c.from)
	d.str(c.specifier)
	d.str(c.path)
	d.bool(c.file)
	d.bool(c.metadata)
	d.int(int(c.kind))
	d.int(c.block)
	d.str(c.resolved)
	d.str(c.skipped)
}

// imports digests what Gazelle hands Resolve beside a rule.
func (d *stateHasher) imports(v any) {
	switch imps := v.(type) {
	case nil:
		d.str("nil")
	case *ruleImports:
		if imps == nil {
			d.str("nil")
			return
		}
		d.str("rule")
		d.program(imps.program)
		each(d, imps.candidates, d.candidate)
		d.strs(imps.sources)
		each(d, imps.edges, d.edge)
		d.str(imps.config)
		d.strs(imps.deps)
		if drops := imps.srcDrops; drops == nil {
			d.int(0)
		} else {
			d.int(1)
			d.config(drops.args.Config)
			d.str(drops.args.Rel)
			d.file(drops.args.File)
			d.rule(drops.have)
			d.strs(drops.dropped)
		}
	case *protoRuleImports:
		if imps == nil {
			d.str("nil")
			return
		}
		d.str("proto")
		each(d, imps.nativeDeps, d.label)
		d.config(imps.config)
		d.protoIdentity(imps.identity)
		d.strs(imps.imports)
	default:
		d.refuse(fmt.Sprintf("Gazelle passes Resolve a %T", v))
	}
}

func (d *stateHasher) generatedPackage(g generatedPackage) {
	a := g.args
	d.config(a.Config)
	d.str(a.Dir)
	d.str(a.Rel)
	d.file(a.File)
	d.strs(slices.DeleteFunc(slices.Clone(a.Subdirs), func(sub string) bool { return d.out(path.Join(a.Rel, sub)) }))
	if d.out(a.Rel) {
		d.int(-1)
	} else {
		d.strs(a.RegularFiles)
	}
	d.strs(a.GenFiles)
	d.rules(a.OtherEmpty)
	d.rules(a.OtherGen)
	d.rules(g.res.Gen)
	d.rules(g.res.Empty)
	each(d, g.res.Imports, d.imports)
	d.strs(g.res.RelsToIndex)
}

// store digests every field Resolve and AfterResolvingDeps may read. Left
// out: memo is rebuilt once the first Resolve sets index, prefetch is stopped
// and empty, protoIndex and holdingIndex are memos of the emission rules,
// frozen is set only on a worker's view, prerefreshed only within selectInputs,
// verify only by -ts_readsets=verify, which turns the record off, and recorder
// only observes the tasks.
func (d *stateHasher) store(s *programStore) {
	d.config(s.repoConfig)
	d.emission(s.emission)
	sortedEachExcept(d, s.inputs, d.whollyOut, func(input programInput) {
		d.config(input.config)
		d.program(input.program)
		if input.discoveryErr != nil {
			d.str(input.discoveryErr.Error())
		} else {
			d.str("")
		}
		d.bool(input.refresh != nil)
	})
	d.strs(slices.Sorted(maps.Keys(s.completePrograms)))
	d.bool(s.index != nil)
	d.str(s.tsgoFlag)
	d.bool(s.verbose)
	d.str(s.tsgo)
	d.bool(s.skipped)
	for _, m := range []map[string]*program{s.programs, s.vitestPrograms, s.compilerPrograms} {
		sortedEach(d, m, d.program)
	}
	sortedEach(d, s.configs, d.str)
	sortedEach(d, s.packages, func(set map[string]bool) { sortedEach(d, set, d.bool) })
	sortedEach(d, s.generatedPackages, d.bool)
	sortedEachExcept(d, s.visited, d.out, d.strs)
	sortedEachExcept(d, s.files, d.out, d.strs)
	sortedEachExcept(d, s.walked, d.whollyOut, d.bool)
	sortedEach(d, s.bases, d.strs)
	sortedEach(d, s.extended, d.bool)
	sortedEach(d, s.foreign, d.str)
	sortedEach(d, s.foreignSaid, d.bool)
	sortedEach(d, s.wranglerConfigs, d.str)
	d.bool(s.installChecked)
	// A regular-file answer is a noted stat, which the record's reads check.
	sortedEachExcept(d, s.regularFiles, func(file string) bool {
		rel, ok := repoRelative(s.repoConfig.RepoRoot, file)
		return ok && d.out(parentDir(rel))
	}, d.bool)
	d.bool(s.fullWalk)
	sortedEach(d, s.manifests, func(m *manifest) {
		if m == nil {
			d.int(0)
			return
		}
		d.int(1)
		d.str(m.dir)
		d.str(m.name)
		d.strs(m.deps)
	})
	d.bool(s.noLockSaid)
}

// emission leaves out its memos and the counters that stamp them: versions,
// epoch, listEpoch, outputsGeneration, rulesGeneration, keysGeneration, stamps,
// clock, outputIndex, registered and the crawl memo.
func (d *stateHasher) emission(g *emissionGraph) {
	if g == nil {
		d.int(0)
		return
	}
	d.int(1)
	sortedEach(d, g.roots, d.bool)
	sortedEach(d, g.outputs, d.bool)
	sortedEach(d, g.rules, d.rule)
	sortedEach(d, g.files, d.file)
	d.npmLock(g.lock)
}

// loadedListings digests every listing the run decoded, which a later
// request for the same listing is rebuilt from without reading it again.
func (d *stateHasher) loadedListings() {
	type keyed struct {
		key   string
		entry *cachedListing
		sum   [sha256.Size]byte
	}
	var list []keyed
	loadedListings.Range(func(k, v any) bool {
		list = append(list, keyed{key: k.(string), entry: v.(*cachedListing)})
		return true
	})
	slices.SortFunc(list, func(a, b keyed) int { return cmp.Compare(a.key, b.key) })
	parallel(len(list), func(i int) { list[i].sum = listingEntryDigest(list[i].entry) })
	each(d, list, func(l keyed) {
		d.str(l.key)
		d.w.Write(l.sum[:])
	})
}

// listingEntryDigest covers what programFromEntry builds a program from, not
// the read and patch state that decides whether the entry holds or patches.
func listingEntryDigest(entry *cachedListing) [sha256.Size]byte {
	d := newHasher()
	d.listing(&entry.Listing)
	each(d, entry.Candidates, func(c cachedCandidate) { d.candidate(c.resolutionCandidate()) })
	d.resolutionFacts(entry.Resolution)
	d.str(entry.Refused)
	return d.sum()
}

// Gazelle 2 builds language/defaults into every binary; its config is strings only.
func gazelleDefaults(name string, ext any) bool {
	t := reflect.TypeOf(ext)
	return name == "defaults" && t != nil && strings.HasSuffix(t.PkgPath(), "/language/defaults")
}
