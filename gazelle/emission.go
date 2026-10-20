package typescript

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
	"github.com/bazelbuild/rules_go/go/runfiles"
)

type emissionGraph struct {
	roots   map[string]bool
	outputs map[string]bool
	// Write through setRule and deleteRule, which version each package's rules.
	rules       map[string]*rule.Rule
	versions    map[string]int
	epoch       int
	files       map[string]*rule.File
	lock        *npmLock
	outputIndex map[string]*packageOutputs
	registered  map[string]registeredBuild
	// Bumped on entry to every callback (Gazelle merges between them) and at each rule-list edit here.
	listEpoch int
	// Bumped whenever a stored owner or BUILD file that could declare outputs changes.
	outputsGeneration int
	// Bumped at every rule store except a resolved TypeScript rule stored again unchanged.
	rulesGeneration int
	// Bumped when a key joins or leaves rules, or its rule changes kind.
	keysGeneration int
	// Each key's last write, a store or an edit in place, on one clock.
	stamps map[string]int
	clock  int
	// observeBuilds' crawl memo, kept across the walk. crawledDirs adds the read packages'
	// ancestors; crawledMisses are the labels reached without a rule, or outside this repo.
	crawled                    map[string]crawledTarget
	crawledTargets             map[string][]string
	crawledDirs, crawledMisses map[string]bool
	// readers maps a memo label to the crawled rules whose references name it.
	readers map[string][]string
	crawl   *crawlRun
	// Directories with this language disabled that Gazelle has yet to merge, which it does
	// after their subtree and before any callback outside it; no callback here sees that merge.
	pendingDisabled map[string]bool
	// The lockfile whose workspace members the memo's npm labels were mapped through.
	crawledLock *npmLock
	// Crawled directories touched since reviewCrawl last checked them.
	touchedDirs map[string]bool
	lastTouched string
}

type crawledTarget struct {
	kind       string
	references []string
	deps       int
}

// crawlRun is the running crawl's labels with the origin each was first pushed with,
// and the ones an invalidation took out of the memo, which the crawl follows again.
type crawlRun struct {
	seen  map[string]int32
	again []queuedLabel
}

type queuedLabel struct {
	key string
	// The chain of deps the label was pushed through, or -1; only the BUILD-edit facts read it.
	origin int32
}

func (g *emissionGraph) crawlMemo() {
	if g.crawled == nil {
		g.crawled, g.crawledTargets, g.readers = map[string]crawledTarget{}, map[string][]string{}, map[string][]string{}
		g.crawledDirs, g.crawledMisses, g.touchedDirs = map[string]bool{}, map[string]bool{}, map[string]bool{}
		g.crawledLock = g.lock
	}
}

// invalidate takes key and every crawled rule that reaches it out of the memo.
func (g *emissionGraph) invalidate(key string) {
	for stack := []string{key}; len(stack) > 0; {
		key := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, crawled := g.crawled[key]; !crawled && !g.crawledMisses[key] {
			continue
		}
		delete(g.crawled, key)
		delete(g.crawledMisses, key)
		if origin, seen := g.crawl.seenAt(key); seen {
			g.crawl.again = append(g.crawl.again, queuedLabel{key, origin})
		}
		stack = append(stack, g.readers[key]...)
		delete(g.readers, key)
	}
}

func (c *crawlRun) seenAt(key string) (int32, bool) {
	if c == nil {
		return 0, false
	}
	origin, seen := c.seen[key]
	return origin, seen
}

// invalidateDir takes out what a crawl loaded dir for: loading a package loads its ancestors.
func (g *emissionGraph) invalidateDir(dir string) {
	for pkg, keys := range g.crawledTargets {
		if within(pkg, dir) {
			for _, key := range keys {
				g.invalidate(key)
			}
		}
	}
	for key := range g.crawledMisses {
		if l, err := parseLabel(key); err == nil && l.Repo == "" && within(l.Pkg, dir) {
			g.invalidate(key)
		}
	}
}

func (g *emissionGraph) crawledPackage(pkg string) {
	for dir := pkg; !g.crawledDirs[dir]; dir = parentDir(dir) {
		g.crawledDirs[dir] = true
		if dir == "" {
			break
		}
	}
}

// touch marks the package a callback edits. Gazelle's Fix and merge rewrite only
// the BUILD file of the directory a callback ran for, after it returns.
func (g *emissionGraph) touch(pkg string) {
	g.touchCrawled(pkg)
	g.touchCrawled(g.lastTouched)
	g.lastTouched = pkg
	for dir := range g.pendingDisabled {
		if dir != "" && pkg != dir && !strings.HasPrefix(pkg, dir+"/") {
			delete(g.pendingDisabled, dir)
			g.touchCrawled(dir)
		}
	}
}

func (g *emissionGraph) disableDir(dir string) {
	if g == nil {
		return
	}
	if g.pendingDisabled == nil {
		g.pendingDisabled = map[string]bool{}
	}
	g.pendingDisabled[dir] = true
}

func (g *emissionGraph) touchCrawled(dir string) {
	if g.crawledDirs[dir] {
		g.touchedDirs[dir] = true
	}
}

func (s *programStore) touchPackage(pkg string) {
	if s != nil && s.emission != nil {
		s.emission.touch(pkg)
	}
}

func (s *programStore) ruleListsMayChange() {
	if s != nil && s.emission != nil {
		s.emission.listEpoch++
	}
}

type registeredBuild struct {
	file    *rule.File
	rules   []*rule.Rule
	version [2]int
	epoch   int
}

func (g *emissionGraph) setRule(key string, r *rule.Rule) {
	g.rulesGeneration++
	g.storeRule(key, r)
}

// Resolve edits only resolve attributes, which neither protoIndex nor holdingIndex reads.
func (g *emissionGraph) setResolvedRule(key string, r *rule.Rule) {
	if g.rules[key] != r {
		g.rulesGeneration++
	}
	g.storeRule(key, r)
}

func (g *emissionGraph) storeRule(key string, r *rule.Rule) {
	if declaresOutputs(g.rules[key]) || declaresOutputs(r) {
		g.outputsGeneration++
	}
	if old := g.rules[key]; old == nil || old.Kind() != r.Kind() {
		g.keysGeneration++
	}
	g.rules[key] = r
	g.stamp(key)
	g.bump(key)
}

func (g *emissionGraph) deleteRule(key string) {
	if declaresOutputs(g.rules[key]) {
		g.outputsGeneration++
	}
	if g.rules[key] != nil {
		g.keysGeneration++
	}
	g.rulesGeneration++
	delete(g.rules, key)
	g.stamp(key)
	g.bump(key)
}

// stamp records a write to key's rule; an edit in place must call it.
func (g *emissionGraph) stamp(key string) {
	if g == nil {
		return
	}
	g.clock++
	if g.stamps == nil {
		g.stamps = map[string]int{}
	}
	g.stamps[key] = g.clock
}

func (g *emissionGraph) setFile(pkg string, f *rule.File) {
	if g.files[pkg] != f {
		g.outputsGeneration++
	}
	g.files[pkg] = f
}

// The attributes ruleOutputs reads; a rule without them owns no generated file.
func declaresOutputs(r *rule.Rule) bool {
	return r != nil && (r.Attr("out") != nil || r.Attr("outs") != nil || r.Attr("out_dir") != nil)
}

func (g *emissionGraph) bump(key string) {
	own, err := parseLabel(key)
	if err != nil {
		// No crawl follows an unparsable label, such as the "" of a nameless exports_files or package.
		g.epoch++
		return
	}
	if g.crawledMisses[key] {
		g.invalidate(key)
	} else {
		g.touchCrawled(own.Pkg)
	}
	if g.versions == nil {
		g.versions = map[string]int{}
	}
	g.versions[own.Pkg]++
}

func (g *emissionGraph) version(pkg string) [2]int {
	return [2]int{g.epoch, g.versions[pkg]}
}

// Registering an unchanged file's rules again would store nothing.
func (g *emissionGraph) registeredRules(pkg string, f *rule.File) bool {
	r, ok := g.registered[pkg]
	if !ok || r.file != f || r.version != g.version(pkg) || r.epoch != g.listEpoch && !slices.Equal(r.rules, f.Rules) {
		return false
	}
	r.epoch = g.listEpoch
	g.registered[pkg] = r
	return true
}

func (g *emissionGraph) recordRegistered(pkg string, f *rule.File) {
	if g.registered == nil {
		g.registered = map[string]registeredBuild{}
	}
	g.registered[pkg] = registeredBuild{file: f, rules: slices.Clone(f.Rules), version: g.version(pkg), epoch: g.listEpoch}
}

var emissionLabels = struct {
	sync.Mutex
	m map[[3]string]string
}{m: map[[3]string]string{}}

func emissionLabel(repo, pkg, name string) string {
	if name == "" {
		return ""
	}
	key := [3]string{repo, pkg, name}
	emissionLabels.Lock()
	defer emissionLabels.Unlock()
	text, ok := emissionLabels.m[key]
	if !ok {
		text = resolveEmissionLabel(repo, pkg, name)
		emissionLabels.m[key] = text
	}
	return text
}

func resolveEmissionLabel(repo, pkg, name string) string {
	l, err := parseLabel(name)
	if err != nil {
		return ""
	}
	l = labelIdentity(l, repo, pkg)
	if !l.Canonical && l.Repo == repo {
		l.Repo = ""
	}
	return l.String()
}

func (s *programStore) semanticRule(key string) *rule.Rule {
	if s.emission == nil {
		return nil
	}
	r := s.storedRule(key)
	if r == nil {
		return nil
	}
	own, err := parseLabel(key)
	if err != nil {
		return r
	}
	return canonicalRule(s.inputs[own.Pkg].config, r)
}

// storedRule is the rule stored at key, which a speculation records reading.
func (s *programStore) storedRule(key string) *rule.Rule {
	r := s.emission.rules[key]
	if run := activeRun; run != nil && run.dry {
		return run.read(s.emission, key, r)
	}
	return r
}

func (s *programStore) ruleTarget(repo, key string) (string, *rule.Rule) {
	if key == "" {
		return "", nil
	}
	if r := s.semanticRule(key); r == nil || r.Kind() != "alias" {
		return key, r
	}
	seen := map[string]bool{}
	for key != "" && !seen[key] {
		seen[key] = true
		r := s.semanticRule(key)
		if r == nil || r.Kind() != "alias" {
			return key, r
		}
		own, err := parseLabel(key)
		if err != nil {
			break
		}
		key = emissionLabel(repo, own.Pkg, r.AttrString("actual"))
	}
	return "", nil
}

func (s *programStore) runtimeConsumerTarget(repo, key string) string {
	r := s.semanticRule(key)
	own, err := parseLabel(key)
	if r == nil || err != nil || r.ShouldKeep() {
		return ""
	}
	if r.Kind() == "ts_binary" {
		return key
	}
	if r.Kind() == "ts_test" {
		runner, _ := s.ruleTarget(repo, emissionLabel(repo, own.Pkg, r.AttrString("runner")))
		rulesRepo := "rules_typescript"
		if s.repoConfig.ModuleToApparentName != nil {
			if apparent := s.repoConfig.ModuleToApparentName("rules_typescript"); apparent != "" {
				rulesRepo = apparent
			}
		}
		canonicalRepo := runfiles.CurrentRepository()
		if runner == emissionLabel(repo, "", "@"+rulesRepo+"//ts/runners:node_test") ||
			repo == "rules_typescript" && runner == "//ts/runners:node_test" ||
			canonicalRepo != "" && canonicalRepo != "_main" && runner == "@@"+canonicalRepo+"//ts/runners:node_test" {
			return key
		}
	}
	return ""
}

type emissionMode uint8

const (
	unknownEmission emissionMode = iota
	opaqueEmission
	sourceEmission
	emittedEmission
)

func ruleEmission(r *rule.Rule) emissionMode {
	if r.Attr("emit") == nil {
		return sourceEmission
	}
	if value, literal := r.Attr("emit").(*bzl.Ident); literal {
		switch value.Name {
		case "False":
			return sourceEmission
		case "True":
			return emittedEmission
		}
	}
	return unknownEmission
}

func runtimeLabels(r *rule.Rule, attr string, repeated bool) ([]string, string) {
	expr := r.Attr(attr)
	if expr == nil {
		return nil, ""
	}
	items := []bzl.Expr{expr}
	if repeated {
		list, literal := expr.(*bzl.ListExpr)
		if !literal {
			return nil, attr
		}
		items = list.List
	}
	var labels []string
	incomplete := ""
	for _, item := range items {
		value, literal := item.(*bzl.StringExpr)
		if !literal {
			incomplete = attr
			continue
		}
		labels = append(labels, value.Value)
	}
	return labels, incomplete
}

func runtimeDependencies(r *rule.Rule) ([]string, string) {
	var attributes []string
	switch r.Kind() {
	case "ts_compile", "ts_test", "ts_proto_library", "ts_npm_package", "node_modules":
		attributes = []string{"deps"}
	case "node_modules_member":
		attributes = []string{"member"}
	case "npm_workspace_package":
		attributes = []string{"target"}
	case "alias":
		attributes = []string{"actual"}
	case "ts_binary":
		attributes = []string{"entry_point"}
	}
	var dependencies []string
	for _, attr := range attributes {
		labels, incomplete := runtimeLabels(r, attr, attr == "deps")
		dependencies = append(dependencies, labels...)
		if incomplete != "" {
			return dependencies, incomplete
		}
	}
	return dependencies, ""
}

func (l *npmLock) memberCompilerTarget(repo, key string) string {
	if l == nil || !strings.HasPrefix(key, "@npm//:") {
		return ""
	}
	if l.memberLabels == nil {
		l.memberLabels = map[string]string{}
		for _, name := range slices.Sorted(maps.Keys(l.members)) {
			label := "@npm//:" + npmPackageToLabelName(name)
			if _, taken := l.memberLabels[label]; !taken {
				l.memberLabels[label] = l.members[name]
			}
		}
	}
	if dir, ok := l.memberLabels[key]; ok {
		return emissionLabel(repo, dir, ":"+packageName(dir))
	}
	return ""
}

func (s *programStore) recordEmissionConsumers(c *config.Config, pkg string) {
	g := s.emission
	if s.metadataIdentity(c, path.Join(pkg, "package.json")) != authoredInput {
		return
	}
	raw, err := readInput(filepath.Join(c.RepoRoot, filepath.FromSlash(pkg), "package.json"))
	if err != nil {
		return
	}
	var manifest map[string]any
	if json.Unmarshal(raw, &manifest) != nil {
		return
	}
	var recordEntry func(any)
	recordEntry = func(v any) {
		switch x := v.(type) {
		case string:
			for _, suffix := range []string{".js", ".jsx", ".mjs", ".cjs", ".d.ts", ".d.mts", ".d.cts"} {
				if strings.HasSuffix(x, suffix) {
					g.outputs[path.Join(pkg, x)] = true
					break
				}
			}
		case map[string]any:
			for _, entry := range x {
				recordEntry(entry)
			}
		case []any:
			for _, entry := range x {
				recordEntry(entry)
			}
		}
	}
	for _, key := range []string{"main", "module", "types", "typings", "exports", "bin"} {
		recordEntry(manifest[key])
	}
}

func (l *tsLang) AfterResolvingDeps(_ context.Context) {
	defer func() {
		logListingCache()
		if in := runInputs; in != nil && l.programs != nil {
			facts := l.programs.importFacts.seal()
			in.mu.Lock()
			in.facts = facts
			in.mu.Unlock()
		}
		writeRunCandidate()
	}()
	exitOnDeferredFatal()
	if l.programs == nil || l.programs.emission == nil {
		return
	}
	if e := l.programs.early; e != nil {
		if !e.done {
			written := l.programs.captureTracked()
			l.finishRules()
			l.programs.requireWritten(written)
		}
		return
	}
	if rec := l.programs.resolution; rec != nil && rec.started {
		if rec.replay != nil {
			rec.applyReplay(l.programs)
			return
		}
		defer rec.finish(l.programs)
	}
	l.finishRules()
}

// finishRules is what AfterResolvingDeps does to every resolved rule: the
// emission graph's runtime closure, emit, package scopes and source exports.
func (l *tsLang) finishRules() {
	exitOnDeferredFatal()
	l.programs.ruleListsMayChange()
	g := l.programs.emission
	repo := l.programs.repoConfig.RepoName
	for pkg, f := range g.files {
		if !l.programs.generatedPackages[pkg] {
			continue
		}
		if f.File != nil {
			f.Sync()
			l.programs.ruleListsMayChange()
		}
		for _, r := range f.Rules {
			g.setRule(emissionLabel(repo, pkg, ":"+r.Name()), r)
		}
	}
	outputOwners := map[string]string{}
	for pkg, files := range l.programs.packages {
		for source := range files {
			owner, found := l.programs.owner(source)
			if isDeclarationFile(source) || !found || owner != pkg {
				continue
			}
			ext := path.Ext(source)
			var outputs []string
			switch ext {
			case ".ts":
				outputs = []string{".js", ".d.ts"}
			case ".tsx":
				outputs = []string{".js", ".jsx", ".d.ts"}
			case ".mts":
				outputs = []string{".mjs", ".d.mts"}
			case ".cts":
				outputs = []string{".cjs", ".d.cts"}
			case ".js":
				outputs = []string{".d.ts"}
			case ".mjs":
				outputs = []string{".d.mts"}
			case ".cjs":
				outputs = []string{".d.cts"}
			}
			for _, suffix := range outputs {
				outputOwners[strings.TrimSuffix(source, ext)+suffix] = emissionLabel(repo, pkg, ":"+packageName(pkg))
			}
		}
	}
	for output := range g.outputs {
		if owner := outputOwners[output]; owner != "" {
			g.roots[owner] = true
		}
		if strings.Contains(output, "*") {
			for file, owner := range outputOwners {
				if matchesExportOutput(output, file) {
					g.roots[owner] = true
				}
			}
		}
	}
	// A program the pass visits after completing it still owes the visit its pre-completion dependencies.
	before := map[string][]string{}
	for key := range l.programs.completePrograms {
		if r := l.programs.semanticRule(key); r != nil {
			before[key], _ = runtimeDependencies(r)
		}
	}
	seen := map[string]bool{}
	var visit func(string, string)
	visit = func(key, consumer string) {
		if key == "" {
			return
		}
		if required, visited := seen[key]; visited && (required || consumer == "") {
			return
		}
		seen[key] = consumer != ""
		if member := g.lock.memberCompilerTarget(repo, key); member != "" {
			visit(member, consumer)
			return
		}
		r := g.rules[key]
		if r == nil {
			return
		}
		own, err := parseLabel(key)
		if err != nil {
			return
		}
		semantic := l.programs.semanticRule(key)
		if kind := semantic.Kind(); kind == "ts_compile" || kind == "ts_test" {
			if consumer == "" && l.programs.generatedPackages[own.Pkg] && !r.ShouldKeep() {
				consumer = key
				seen[key] = true
			}
			if l.programs.generatedPackages[own.Pkg] && !r.ShouldKeep() && !attrKept(r, "emit") {
				r.SetAttr("emit", true)
				g.stamp(key)
				semantic = l.programs.semanticRule(key)
			}
			if consumer != "" && ruleEmission(semantic) != emittedEmission {
				declared := declaredSourceImports(l.programs.inputs[own.Pkg].config, semantic, g.files[own.Pkg])
				if declared.incomplete != "" {
					fatalf("typescript: %s requires a runtime boundary from %s, but %s; source emission cannot establish whether unpublished TypeScript needs transformation. Did you mean to use explicit source-file labels or declare emit = True on the owner?", consumer, key, declared.incomplete)
				}
				for _, input := range declared.imports {
					if rawTypeScript(input.Imp) {
						fatalf("typescript: %s requires emission from %s for runtime TypeScript input %s, but its effective emit value does not establish emitted JavaScript. Did you mean to include that package in this update, remove its keep marker, or declare emit = True on the owner?", consumer, key, input.Imp)
					}
				}
			}
		} else if kind == "ts_proto_library" && consumer != "" {
			fatalf("typescript: %s requires emitted JavaScript from %s, but ts_proto_library publishes TypeScript with fixed emit = False. Did you mean to use a source-capable consumer or a dependency that publishes emitted JavaScript?", consumer, key)
		}
		dependencies, incomplete := runtimeDependencies(semantic)
		if consumer != "" && incomplete != "" {
			fatalf("typescript: %s requires the runtime closure of %s, but its %s is an expression; automatic publication cannot establish its runtime dependencies. Did you mean to use literal dependency and forwarding labels, or keep the whole consuming rule and maintain its runtime closure manually?", consumer, key, incomplete)
		}
		for _, dep := range slices.Concat(before[key], dependencies) {
			visit(emissionLabel(repo, own.Pkg, dep), consumer)
		}
	}
	for key := range g.rules {
		if target := l.programs.runtimeConsumerTarget(repo, key); target != "" {
			consumer := ""
			own, err := parseLabel(key)
			if err == nil && l.programs.generatedPackages[own.Pkg] && !g.rules[key].ShouldKeep() {
				consumer = key
			}
			visit(target, consumer)
		}
	}
	for root := range g.roots {
		visit(root, "")
	}
	// Rooted programs can promote owners that unrooted programs would otherwise read as sources.
	for _, rooted := range []bool{true, false} {
		for {
			completed := false
			l.programs.speculatePending(repo, rooted, seen, true)
			for _, key := range slices.Sorted(maps.Keys(l.programs.completePrograms)) {
				complete := l.programs.completePrograms[key]
				if complete == nil || rooted && !seen[key] {
					continue
				}
				delete(l.programs.completePrograms, key)
				cx := &programCompletion{s: l.programs, repo: repo, rooted: rooted}
				l.programs.complete(key, complete, cx)
				for _, dep := range cx.reached {
					visit(dep, key)
				}
				l.programs.speculatePending(repo, rooted, seen, false)
				completed = true
			}
			if !completed {
				break
			}
		}
	}
	if l.programs.verify != nil {
		l.programs.verify.report()
	}
	exitOnDeferredFatal()
	l.programs.exportPackageScopes()
	for key, r := range g.rules {
		if r.PrivateAttr("_ts_scope_candidate") == true && len(r.AttrStrings("package_scopes")) == 0 {
			r.Delete()
			g.deleteRule(key)
		}
	}
	for pkg, file := range g.files {
		if l.programs.generatedPackages[pkg] && file.File != nil {
			file.Sync()
			l.programs.ruleListsMayChange()
		}
	}
	for _, pkg := range slices.Sorted(maps.Keys(g.files)) {
		l.programs.relocateSourceExports(l.programs.inputs[pkg].config, pkg)
	}
	for _, pkg := range slices.Sorted(maps.Keys(g.files)) {
		file := g.files[pkg]
		if !l.programs.generatedPackages[pkg] {
			continue
		}
		if file.File != nil {
			file.Sync()
			l.programs.ruleListsMayChange()
		}
		if file.Content == nil && l.programs.bazelPackage(pkg) {
			if err := l.programs.newPackageTargetConflict(pkg); err != nil {
				fatal(err)
			}
		}
	}
}

// programCompletion is what one program's completion asks of the emission pass.
//
// It answers each owner's emission mode from the plan, completes an owner whose
// program is still pending before the answer, and collects the labels the
// completion reaches; finishRules visits them once the completion returns.
type programCompletion struct {
	s    *programStore
	repo string
	// Whether the emission pass reached the completing program.
	rooted  bool
	reached []string
}

// reach notes that the completion reached key.
func (cx *programCompletion) reach(key string) {
	if cx.rooted {
		cx.reached = append(cx.reached, key)
	}
}

// ownerMode is the emission mode of the owner key names.
func (cx *programCompletion) ownerMode(key string) emissionMode {
	cx.reach(key)
	target, owner := cx.s.ruleTarget(cx.repo, key)
	if owner == nil || owner.Kind() != "ts_compile" && owner.Kind() != "ts_test" {
		return opaqueEmission
	}
	var promoted *rule.Rule
	if cx.rooted {
		promoted = cx.promotion(key, target)
	}
	if complete := cx.s.completePrograms[target]; complete != nil {
		if dryRun() {
			activeRun.waiting = target
			panic(frozenBail{"pending owner"})
		}
		delete(cx.s.completePrograms, target)
		if promoted != nil {
			promoted.SetAttr("emit", true)
			cx.s.emission.stamp(target)
		}
		cx.s.complete(target, complete, cx)
	}
	mode := ruleEmission(owner)
	if promoted != nil {
		mode = emittedEmission
	}
	noteOutput(func() []string { return []string{fmt.Sprintf("owner %s %d", key, mode)} })
	return mode
}

// complete runs a pending program's completion, verified against its speculation.
func (s *programStore) complete(key string, complete func(*programCompletion), cx *programCompletion) {
	s.verified("completion", key, s.verify.take(key), func() { complete(cx) })
}

// speculatePending speculates the completions the round may run: all of them, or those not yet
// speculated and those whose speculation waited on an owner since completed.
func (s *programStore) speculatePending(repo string, rooted bool, seen map[string]bool, all bool) {
	if s.verify == nil {
		return
	}
	for _, key := range slices.Sorted(maps.Keys(s.completePrograms)) {
		spec := s.verify.specs[key]
		again := all || spec == nil || spec.waiting != "" && s.completePrograms[spec.waiting] == nil
		if complete := s.completePrograms[key]; complete != nil && (!rooted || seen[key]) && again {
			// The serial pass takes a completion out of the pending set before running it.
			delete(s.completePrograms, key)
			cx := &programCompletion{s: s, repo: repo, rooted: rooted}
			s.verify.specs[key] = s.speculateTask(func() { complete(cx) })
			s.completePrograms[key] = complete
		}
	}
}

// promotion is the rule visiting key sets emit = True on, when that rule is the owner target.
func (cx *programCompletion) promotion(key, target string) *rule.Rule {
	g := cx.s.emission
	if g == nil || g.lock.memberCompilerTarget(cx.repo, key) != "" || g.lock.memberCompilerTarget(cx.repo, target) != "" {
		return nil
	}
	r := cx.s.storedRule(target)
	own, err := parseLabel(target)
	semantic := cx.s.semanticRule(target)
	if r == nil || err != nil || semantic == nil {
		return nil
	}
	if kind := semantic.Kind(); kind != "ts_compile" && kind != "ts_test" ||
		!cx.s.generatedPackages[own.Pkg] || r.ShouldKeep() || attrKept(r, "emit") {
		return nil
	}
	return r
}

func (s *programStore) exportPackageScopes() {
	tc := &tsConfig{programs: s}
	pending := map[string]*rule.Rule{}
	for pkg, file := range s.emission.files {
		for _, r := range file.Rules {
			if r.PrivateAttr("_ts_source_export") != "pending" {
				continue
			}
			list, err := exportSourceMembership(r)
			if err != nil {
				fatal(err)
			}
			for _, item := range list.List {
				pending[emissionLabel(s.repoConfig.RepoName, pkg, ":"+item.(*bzl.StringExpr).Value)] = r
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(s.emission.rules)) {
		r := s.emission.rules[key]
		if r.Kind() == "exports_files" {
			continue
		}
		consumer, err := parseLabel(key)
		if err != nil {
			continue
		}
		for _, attr := range []string{"srcs", "package_scopes", "type_inputs", "data", "config_srcs"} {
			for _, text := range r.AttrStrings(attr) {
				source, err := parseLabel(text)
				if err != nil {
					continue
				}
				source = labelIdentity(source, s.repoConfig.RepoName, consumer.Pkg)
				scope := path.Join(source.Pkg, source.Name)
				if source.Canonical || source.Repo != s.repoConfig.RepoName || path.Base(source.Name) != "package.json" ||
					s.inputIdentity(s.repoConfig, s.index, scope) != authoredInput {
					continue
				}
				pkg := configSrcPackage(tc, scope, "")
				if pkg == consumer.Pkg {
					continue
				}
				file := s.emission.files[pkg]
				name := strings.TrimPrefix(scope, pkg+"/")
				s.relocateSourceExports(s.repoConfig, pkg)
				visibility, exported, err := s.sourceVisibility(s.repoConfig, file, name)
				if err != nil {
					fatalf("typescript: %s cannot retain package scope %s: %v. Did you mean to use literal source exports with visibility for the consuming package?", key, scope, err)
				}
				if scopeVisibilityDenies(visibility, label.New(s.repoConfig.RepoName, pkg, name), consumer) {
					fatalf("typescript: %s requires package scope %s, but source visibility %v denies access. Did you mean to export that scope to the consuming package, or supply it through a compiler dependency?", key, scope, visibility)
				}
				if exported {
					continue
				}
				sourceKey := emissionLabel(s.repoConfig.RepoName, pkg, ":"+name)
				exports := pending[sourceKey]
				if !s.generatedPackages[pkg] || file == nil || exports == nil && file.File == nil {
					fatalf("typescript: %s requires package scope %s without a source export in %s, which is not selected for publication. Did you mean to include %s in the Gazelle update?", key, scope, orRepoRoot(pkg), orRepoRoot(pkg))
				}
				if exports == nil {
					exports = rule.NewRule("exports_files", "")
					exports.SetAttr("srcs", []string{name})
					exports.Insert(file)
					s.ruleListsMayChange()
				}
				exports.SetAttr("visibility", visibility)
				exports.SetPrivateAttr("_ts_source_export", nil)
				delete(pending, sourceKey)
			}
		}
	}
	for _, r := range pending {
		r.Delete()
	}
}

func scopeVisibilityDenies(visibility []string, owner, consumer label.Label) bool {
	consumer = labelIdentity(consumer, owner.Repo, owner.Pkg)
	if owner.Repo == consumer.Repo && owner.Canonical == consumer.Canonical && owner.Pkg == consumer.Pkg {
		return false
	}
	for _, text := range visibility {
		grant, err := parseLabel(text)
		if err != nil {
			return false
		}
		grant = labelIdentity(grant, owner.Repo, owner.Pkg)
		switch {
		case grant.Pkg == "visibility" && grant.Name == "public":
			return false
		case grant.Pkg == "visibility" && grant.Name == "private":
		case grant.Name == "__pkg__":
			if grant.Repo == consumer.Repo && grant.Canonical == consumer.Canonical && grant.Pkg == consumer.Pkg {
				return false
			}
		case grant.Name == "__subpackages__":
			if grant.Repo == consumer.Repo && grant.Canonical == consumer.Canonical && within(consumer.Pkg, grant.Pkg) {
				return false
			}
		default:
			// Bazel evaluates package_group membership.
			return false
		}
	}
	return true
}

func (s *programStore) publishPackageScope(c *config.Config, scope string, consumer label.Label) {
	pkg := configSrcPackage(getConfig(c), scope, "")
	key := emissionLabel(c.RepoName, pkg, ":"+packageName(pkg))
	r := s.storedRule(key)
	if !s.generatedPackages[pkg] || r == nil || r.PrivateAttr("_ts_scope_candidate") != true {
		return
	}
	if dryRun() {
		panic(frozenBail{"package scope"})
	}
	defer s.emission.stamp(key)
	scopes := r.AttrStrings("package_scopes")
	source := strings.TrimPrefix(scope, pkg+"/")
	visibility, _, err := s.sourceVisibility(c, s.emission.files[pkg], source)
	if err != nil {
		fatalf("typescript: %s cannot publish package scope %s: %v. Did you mean to use literal source exports, or keep an explicit compiler owner and maintain its visibility manually?", key, scope, err)
	}
	if scopeVisibilityDenies(visibility, label.New(c.RepoName, pkg, r.Name()), consumer) {
		fatalf("typescript: %s requires runtime package scope %s, but source visibility %v denies access to its automatic publisher %s. Did you mean to export that scope to the consuming package, or provide an explicit compiler owner with the required visibility?", consumer, scope, visibility, key)
	}
	for _, previous := range scopes {
		policy, _, err := s.sourceVisibility(c, s.emission.files[pkg], previous)
		if err != nil || !slices.Equal(policy, visibility) {
			fatalf("typescript: %s cannot publish package scopes %s and %s with different or unknown source visibility. Did you mean to provide explicit compiler owners with the required visibility?", key, previous, source)
		}
	}
	if attrKept(r, "visibility") {
		policy, err := exportVisibility(c, r, pkg)
		if err != nil || !slices.Equal(policy, visibility) {
			fatalf("typescript: %s cannot publish package scope %s through kept visibility that differs from its source export. Did you mean to remove that keep marker, or keep the whole compiler owner and maintain its complete inputs and visibility manually?", key, scope)
		}
	}
	r.SetAttr("visibility", visibility)
	if !slices.Contains(scopes, source) {
		scopes = append(scopes, source)
		slices.Sort(scopes)
		r.SetAttr("package_scopes", scopes)
	}
}

// Node replaces every RHS star with the same subpath, including slashes: https://nodejs.org/api/packages.html#subpath-patterns.
func matchesExportOutput(pattern, file string) bool {
	stars := strings.Count(pattern, "*")
	if stars == 0 {
		return pattern == file
	}
	fixed := len(pattern) - stars
	width := len(file) - fixed
	if width < 0 || width%stars != 0 {
		return false
	}
	start := strings.IndexByte(pattern, '*')
	capture := file[start : start+width/stars]
	return strings.ReplaceAll(pattern, "*", capture) == file
}
